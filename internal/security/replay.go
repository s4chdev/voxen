package security

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReplayGuard rejects replayed session statements.
//
// Two independent checks are applied:
//
//  1. Nonce cache: every accepted nonce is remembered until its statement
//     expires. A repeated nonce is rejected.
//  2. Monotonic issue time per issuer: a statement issued earlier than one
//     already accepted from the same key is rejected, even if its nonce is new.
//     This bounds the damage of a nonce-cache eviction under memory pressure.
//
// The guard is bounded: if the cache is full, the oldest entry is evicted and
// the monotonic check remains the backstop. This is deliberate: an unbounded
// attacker-controlled cache is its own denial of service.
type ReplayGuard struct {
	mu       sync.Mutex
	capacity int
	nonces   map[[NonceSize]byte]time.Time
	latest   map[string]time.Time // hex issuer key -> newest accepted IssuedAt
	now      func() time.Time
}

// DefaultReplayCapacity bounds memory use.
const DefaultReplayCapacity = 8192

// NewReplayGuard builds a guard. A zero capacity uses DefaultReplayCapacity.
func NewReplayGuard(capacity int) *ReplayGuard {
	if capacity <= 0 {
		capacity = DefaultReplayCapacity
	}
	return &ReplayGuard{
		capacity: capacity,
		nonces:   make(map[[NonceSize]byte]time.Time, capacity/8),
		latest:   make(map[string]time.Time),
		now:      time.Now,
	}
}

// Check atomically validates and records a statement.
//
// It must only be called after SessionStatement.Verify has succeeded: accepting
// a nonce before the signature check would let an unauthenticated attacker burn
// nonces.
func (g *ReplayGuard) Check(stmt *SessionStatement) error {
	if stmt == nil {
		return fmt.Errorf("%w: nil statement", ErrMalformed)
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()

	// Monotonic issue time per issuer. Equal timestamps are allowed for
	// statements generated within the same millisecond by distinct sessions,
	// but the nonce cache still prevents exact replays.
	key := hex.EncodeToString(stmt.IssuerPublicKey)
	if prev, ok := g.latest[key]; ok && stmt.IssuedAt.Before(prev) {
		return fmt.Errorf("%w: issued at %s, already accepted %s",
			ErrStaleStatement, stmt.IssuedAt.UTC().Format(time.RFC3339Nano), prev.UTC().Format(time.RFC3339Nano))
	}

	// Nonce replay.
	if _, seen := g.nonces[stmt.Nonce]; seen {
		return ErrNonceReused
	}

	g.gc(now)
	for len(g.nonces) >= g.capacity {
		g.evictOldest()
	}
	g.nonces[stmt.Nonce] = stmt.ExpiresAt
	if prev, ok := g.latest[key]; !ok || stmt.IssuedAt.After(prev) {
		g.latest[key] = stmt.IssuedAt
	}
	return nil
}

func (g *ReplayGuard) gc(now time.Time) {
	for nonce, expiry := range g.nonces {
		if now.After(expiry) {
			delete(g.nonces, nonce)
		}
	}
	// Bound the issuer table as well; entries are tiny but peer-supplied.
	if len(g.latest) > g.capacity {
		g.latest = make(map[string]time.Time)
	}
}

func (g *ReplayGuard) evictOldest() {
	var (
		oldest     [NonceSize]byte
		oldestTime time.Time
		haveOldest bool
	)
	for nonce, expiry := range g.nonces {
		if !haveOldest || expiry.Before(oldestTime) {
			oldest, oldestTime, haveOldest = nonce, expiry, true
		}
	}
	if haveOldest {
		delete(g.nonces, oldest)
	}
}

// Size reports the number of cached nonces. Exposed for diagnostics.
func (g *ReplayGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.nonces)
}

// SetClock overrides the clock. Tests only.
func (g *ReplayGuard) SetClock(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = now
}

// -----------------------------------------------------------------------------
// Signed room membership
// -----------------------------------------------------------------------------

// Member is one roster entry.
type Member struct {
	PublicKey   ed25519.PublicKey
	DisplayName string
}

// RoomMembership is an administrator-signed roster. A joining peer must present
// a roster in which it appears, signed by the room administrator, before any
// other participant accepts its session statement.
//
// The sequence number makes rosters monotonic: an attacker replaying an old
// roster cannot re-admit a participant who was removed.
type RoomMembership struct {
	Version   int
	RoomID    string
	Sequence  uint64
	Issuer    ed25519.PublicKey
	Members   []Member
	IssuedAt  time.Time
	ExpiresAt time.Time
}

func (m *RoomMembership) canonical() []byte {
	e := &encoder{}
	e.str(domainRoomMembership)
	e.u32(uint32(m.Version))
	e.str(m.RoomID)
	e.u64(m.Sequence)
	e.bytes(m.Issuer)
	e.u64(uint64(len(m.Members)))
	for _, member := range m.Members {
		e.bytes(member.PublicKey)
		e.str(member.DisplayName)
	}
	e.u64(uint64(m.IssuedAt.UnixMilli()))
	e.u64(uint64(m.ExpiresAt.UnixMilli()))
	out := make([]byte, len(e.buf))
	copy(out, e.buf)
	return out
}

// SignedBytes returns the exact signed encoding.
func (m *RoomMembership) SignedBytes() []byte { return m.canonical() }

// Hash returns a SHA-256 of the canonical encoding, usable for comparison.
func (m *RoomMembership) Hash() [32]byte {
	return sha256.Sum256(m.canonical())
}

// Sign signs the roster with the administrator key.
func (m *RoomMembership) Sign(priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("security: invalid administrator key")
	}
	return ed25519.Sign(priv, m.canonical()), nil
}

// MembershipPolicy is the verification policy for a roster.
type MembershipPolicy struct {
	Now          func() time.Time
	MaxClockSkew time.Duration
	ExpectedRoom string
	// ExpectedAdmin is required; rosters are only valid when signed by the
	// administrator the local user pinned for that room.
	ExpectedAdmin ed25519.PublicKey
	// MinSequence rejects rollbacks. Zero accepts the first roster.
	MinSequence uint64
}

func (p MembershipPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Verify validates the roster signature, validity window, room and sequence.
func (m *RoomMembership) Verify(sig []byte, policy MembershipPolicy) error {
	if m.Version != ProtocolVersion {
		return fmt.Errorf("%w: unsupported roster version %d", ErrMalformed, m.Version)
	}
	if m.RoomID == "" {
		return fmt.Errorf("%w: empty room id", ErrMalformed)
	}
	if len(policy.ExpectedAdmin) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: no administrator key pinned", ErrUnknownIssuer)
	}
	if !equalBytes(policy.ExpectedAdmin, m.Issuer) {
		return ErrUnknownIssuer
	}
	if policy.ExpectedRoom != "" && m.RoomID != policy.ExpectedRoom {
		return ErrRoomMismatch
	}
	now := policy.now()
	if now.Before(m.IssuedAt.Add(-policy.MaxClockSkew)) {
		return ErrNotYetValid
	}
	if !now.Before(m.ExpiresAt) {
		return ErrExpired
	}
	if m.Sequence < policy.MinSequence {
		return fmt.Errorf("%w: sequence %d < %d", ErrRosterRollback, m.Sequence, policy.MinSequence)
	}
	if len(m.Members) == 0 {
		return fmt.Errorf("%w: roster is empty", ErrMalformed)
	}
	if len(m.Members) > 64 {
		return fmt.Errorf("%w: roster has too many members", ErrMalformed)
	}
	if !ed25519.Verify(m.Issuer, m.canonical(), sig) {
		return ErrSignatureInvalid
	}
	// Reject duplicate members: a duplicated key would break the "one identity,
	// one participant" invariant.
	seen := make(map[string]struct{}, len(m.Members))
	for _, member := range m.Members {
		if len(member.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: member key has wrong length", ErrMalformed)
		}
		k := hex.EncodeToString(member.PublicKey)
		if _, dup := seen[k]; dup {
			return fmt.Errorf("%w: duplicate member", ErrMalformed)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// Contains reports whether pub is listed in the roster.
func (m *RoomMembership) Contains(pub ed25519.PublicKey) bool {
	for _, member := range m.Members {
		if equalBytes(member.PublicKey, pub) {
			return true
		}
	}
	return false
}

// RequireMember verifies the roster and that pub is listed.
func (m *RoomMembership) RequireMember(sig []byte, pub ed25519.PublicKey, policy MembershipPolicy) error {
	if err := m.Verify(sig, policy); err != nil {
		return err
	}
	if !m.Contains(pub) {
		return ErrMemberNotListed
	}
	return nil
}

// NewRoomMembership builds a roster signed by admin. Members are sorted into a
// canonical order so identical rosters encode identically.
func NewRoomMembership(roomID string, sequence uint64, admin ed25519.PublicKey, members []Member, ttl time.Duration) *RoomMembership {
	sorted := make([]Member, len(members))
	copy(sorted, members)
	sortMembersByKey(sorted)
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now().UTC()
	return &RoomMembership{
		Version:   ProtocolVersion,
		RoomID:    roomID,
		Sequence:  sequence,
		Issuer:    admin,
		Members:   sorted,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
}

func sortMembersByKey(m []Member) {
	sort.Slice(m, func(i, j int) bool {
		return strings.Compare(hex.EncodeToString(m[i].PublicKey), hex.EncodeToString(m[j].PublicKey)) < 0
	})
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
