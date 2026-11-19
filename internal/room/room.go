// Package room implements the room state machine and the mesh topology rules.
//
// A room is a set of authenticated participants. voxen uses a full mesh: every
// participant maintains an independent, separately authenticated WebRTC session
// with every other participant. There is no SFU and no shared media server.
//
// Phase 1 defines the state machine, the admission policy and the roster
// validation entry points. The mesh itself is implemented in phase 6.
package room

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/security"
)

// Role describes how the local peer relates to the room.
type Role string

const (
	RoleNone    Role = ""
	RoleCreator Role = "creator"
	RoleMember  Role = "member"
)

// State is the local participation state.
type State string

const (
	StateIdle    State = "idle"
	StateJoining State = "joining"
	StateActive  State = "active"
	StateLeaving State = "leaving"
	StateFailed  State = "failed"
)

// ConnectionState is the media connection state toward one peer.
type ConnectionState string

const (
	ConnConnecting     ConnectionState = "connecting"
	ConnAuthenticating ConnectionState = "authenticating"
	ConnConnected      ConnectionState = "connected"
	ConnDisconnected   ConnectionState = "disconnected"
	ConnFailed         ConnectionState = "failed"
)

// Errors from the room layer.
var (
	ErrNotInRoom       = errors.New("room: not in a room")
	ErrAlreadyInRoom   = errors.New("room: already in a room")
	ErrRoomFull        = errors.New("room: room has reached its participant limit")
	ErrNotCreator      = errors.New("room: only the creator may admit or remove participants")
	ErrPeerUnknown     = errors.New("room: unknown peer")
	ErrPeerNotVerified = errors.New("room: peer identity has not been verified")
	ErrRosterInvalid   = errors.New("room: roster signature is invalid")
	ErrDuplicatePeer   = errors.New("room: peer is already present")
)

// Peer is one participant.
type Peer struct {
	// ID is the signaling-assigned routing identifier. It is not an identity.
	ID string
	// Fingerprint is the verified Ed25519 identity of the participant. A peer
	// without a verified fingerprint is never connected.
	Fingerprint identity.Fingerprint
	PublicKey   ed25519.PublicKey
	DisplayName string
	// Creator marks the participant who created the room and signs rosters.
	Creator  bool
	JoinedAt time.Time
	State    ConnectionState
	Muted    bool
	Speaking bool
	// LastError explains a failed connection.
	LastError string
}

// Member is the local participant.
type Member struct {
	PublicKey   ed25519.PublicKey
	DisplayName string
	Muted       bool
}

// Event is delivered to observers (CLI status, TUI, logs).
type Event struct {
	Kind    string
	RoomID  string
	PeerID  string
	Message string
	At      time.Time
}

// Event kinds.
const (
	EventJoined      = "joined"
	EventLeft        = "left"
	EventPeerAdded   = "peer_added"
	EventPeerRemoved = "peer_removed"
	EventPeerUpdated = "peer_updated"
	EventRoster      = "roster"
	EventFailed      = "failed"
)

// Observer receives room events. Implementations must not block.
type Observer func(Event)

// Options configures a room instance.
type Options struct {
	RoomID string
	// MaxParticipants bounds the mesh size. A full mesh has O(n^2) connections,
	// and every additional participant multiplies local CPU and bandwidth. The
	// count includes the local participant.
	MaxParticipants int
	// Self is the local identity and pinned administrator key.
	Self *identity.Identity
	// Trust decides whether a peer key may receive media.
	Trust TrustChecker
	// RosterPolicy is used to validate administrator-signed rosters.
	RosterPolicy security.MembershipPolicy
	// Now is injectable for tests.
	Now func() time.Time
}

// TrustChecker is the identity-plane view the room needs. It is an interface so
// the room state machine can be tested without a key store on disk.
type TrustChecker interface {
	IsVerified(fp identity.Fingerprint) bool
	Level(fp identity.Fingerprint) string
}

// Room is the local view of one room. All methods are safe for concurrent use.
type Room struct {
	mu       sync.RWMutex
	opts     Options
	state    State
	role     Role
	peers    map[string]*Peer
	roster   *security.RoomMembership
	sequence uint64
	lastErr  string
	observer Observer
	created  time.Time
}

// New builds an idle room bound to the local identity.
func New(opts Options) (*Room, error) {
	if opts.RoomID == "" {
		return nil, errors.New("room: room id is required")
	}
	if opts.Self == nil {
		return nil, errors.New("room: local identity is required")
	}
	if opts.Trust == nil {
		return nil, errors.New("room: trust checker is required")
	}
	if opts.MaxParticipants <= 0 {
		opts.MaxParticipants = 5
	}
	if opts.MaxParticipants > 16 {
		return nil, fmt.Errorf("room: max participants %d exceeds mesh limit 16", opts.MaxParticipants)
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Room{
		opts:  opts,
		state: StateIdle,
		peers: make(map[string]*Peer, opts.MaxParticipants),
	}, nil
}

// SetObserver installs an event observer.
func (r *Room) SetObserver(o Observer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observer = o
}

func (r *Room) emit(e Event) {
	if e.At.IsZero() {
		e.At = r.opts.Now()
	}
	o := r.observer
	if o != nil {
		o(e)
	}
}

// BeginJoin transitions to StateJoining. It fails if a room is already active.
func (r *Room) BeginJoin(role Role) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateActive || r.state == StateJoining {
		return ErrAlreadyInRoom
	}
	r.state = StateJoining
	r.role = role
	r.created = r.opts.Now()
	return nil
}

// Activate marks the room as joined after the signaling server confirmed
// membership and the local roster was validated.
func (r *Room) Activate(roster *security.RoomMembership, sequence uint64) error {
	if roster == nil {
		return ErrRosterInvalid
	}
	if roster.RoomID != r.opts.RoomID {
		return fmt.Errorf("%w: roster is for room %q", ErrRosterInvalid, roster.RoomID)
	}
	if !roster.Contains(r.opts.Self.PublicKey) {
		return fmt.Errorf("%w: local identity is not listed", ErrRosterInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence < r.sequence {
		return fmt.Errorf("%w: sequence %d < %d", ErrRosterInvalid, sequence, r.sequence)
	}
	r.roster = roster
	r.sequence = sequence
	r.state = StateActive
	r.lastErr = ""
	r.emit(Event{Kind: EventRoster, RoomID: r.opts.RoomID})
	return nil
}

// Fail records a terminal failure and moves to StateFailed.
func (r *Room) Fail(err error) {
	r.mu.Lock()
	r.state = StateFailed
	if err != nil {
		r.lastErr = err.Error()
	}
	id := r.opts.RoomID
	r.mu.Unlock()
	r.emit(Event{Kind: EventFailed, RoomID: id, Message: r.lastErr})
}

// Leave clears the room state.
func (r *Room) Leave() {
	r.mu.Lock()
	r.state = StateLeaving
	r.peers = make(map[string]*Peer)
	r.roster = nil
	r.sequence = 0
	r.role = RoleNone
	id := r.opts.RoomID
	r.mu.Unlock()
	r.emit(Event{Kind: EventLeft, RoomID: id})
	r.mu.Lock()
	r.state = StateIdle
	r.mu.Unlock()
}

// AddPeer introduces a participant discovered through signaling.
//
// The peer is admitted only when the local trust store has verified its
// fingerprint. Unverified peers are recorded as failed with an explicit reason
// so "voxen peers" can show why nothing is being sent to them.
func (r *Room) AddPeer(id string, pub ed25519.PublicKey, displayName string) (*Peer, error) {
	if id == "" || len(pub) != ed25519.PublicKeySize {
		return nil, ErrPeerUnknown
	}
	fp := identity.FingerprintFromPublic(pub)

	r.mu.Lock()
	if r.state != StateActive {
		r.mu.Unlock()
		return nil, ErrNotInRoom
	}
	if _, dup := r.peers[id]; dup {
		r.mu.Unlock()
		return nil, ErrDuplicatePeer
	}
	if len(r.peers) >= r.opts.MaxParticipants-1 {
		r.mu.Unlock()
		return nil, ErrRoomFull
	}
	peer := &Peer{
		ID:          id,
		Fingerprint: fp,
		PublicKey:   append(ed25519.PublicKey(nil), pub...),
		DisplayName: displayName,
		JoinedAt:    r.opts.Now(),
		State:       ConnConnecting,
	}
	if !r.opts.Trust.IsVerified(fp) {
		peer.State = ConnFailed
		peer.LastError = "identity not verified; run 'voxen verify " + fp.String() + "'"
	}
	r.peers[id] = peer
	roomID := r.opts.RoomID
	r.mu.Unlock()

	r.emit(Event{Kind: EventPeerAdded, RoomID: roomID, PeerID: id})
	if peer.State == ConnFailed {
		return peer, ErrPeerNotVerified
	}
	return peer, nil
}

// RemovePeer deletes a participant after a leave notification or a removal.
func (r *Room) RemovePeer(id string) error {
	r.mu.Lock()
	if _, ok := r.peers[id]; !ok {
		r.mu.Unlock()
		return ErrPeerUnknown
	}
	delete(r.peers, id)
	roomID := r.opts.RoomID
	r.mu.Unlock()
	r.emit(Event{Kind: EventPeerRemoved, RoomID: roomID, PeerID: id})
	return nil
}

// UpdatePeer applies a state change from the media plane.
func (r *Room) UpdatePeer(id string, fn func(*Peer)) error {
	r.mu.Lock()
	peer, ok := r.peers[id]
	if !ok {
		r.mu.Unlock()
		return ErrPeerUnknown
	}
	fn(peer)
	snapshot := *peer
	roomID := r.opts.RoomID
	r.mu.Unlock()
	r.emit(Event{Kind: EventPeerUpdated, RoomID: roomID, PeerID: id})
	_ = snapshot
	return nil
}

// PeerState returns a copy of one peer record.
func (r *Room) PeerState(id string) (Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	peer, ok := r.peers[id]
	if !ok {
		return Peer{}, false
	}
	return *peer, true
}

// Snapshot returns the room state and a copy of the peer list.
func (r *Room) Snapshot() (State, Role, string, []Peer) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	peers := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, *p)
	}
	return r.state, r.role, r.lastErr, peers
}

// ApplyRoster validates an administrator-signed roster update before applying
// it. Members removed from the roster are torn down immediately; new members are
// added as unverified until the local user verifies them.
func (r *Room) ApplyRoster(roster *security.RoomMembership, sig []byte, policy security.MembershipPolicy) error {
	policy.ExpectedRoom = r.opts.RoomID
	policy.MinSequence = r.sequence
	if err := roster.Verify(sig, policy); err != nil {
		return fmt.Errorf("%w: %v", ErrRosterInvalid, err)
	}
	allowed := make(map[string]struct{}, len(roster.Members))
	for _, m := range roster.Members {
		fp := identity.FingerprintFromPublic(m.PublicKey)
		allowed[fp.String()] = struct{}{}
	}

	r.mu.Lock()
	r.roster = roster
	r.sequence = roster.Sequence
	removed := make([]string, 0)
	for id, peer := range r.peers {
		if _, ok := allowed[peer.Fingerprint.String()]; !ok {
			removed = append(removed, id)
			delete(r.peers, id)
		}
	}
	roomID := r.opts.RoomID
	r.mu.Unlock()

	for _, id := range removed {
		r.emit(Event{Kind: EventPeerRemoved, RoomID: roomID, PeerID: id})
	}
	r.emit(Event{Kind: EventRoster, RoomID: roomID})
	return nil
}

// IsMember reports whether a fingerprint is listed in the active roster.
func (r *Room) IsMember(fp identity.Fingerprint) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.roster == nil {
		return false
	}
	for _, m := range r.roster.Members {
		if identity.FingerprintFromPublic(m.PublicKey).Equal(fp) {
			return true
		}
	}
	return false
}

// Sequence returns the last applied roster sequence.
func (r *Room) Sequence() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sequence
}

// WaitReady blocks until the room is active or ctx ends. Phase 6 uses this to
// sequence mesh establishment after the join handshake.
func (r *Room) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, _, lastErr, _ := r.Snapshot()
		switch state {
		case StateActive:
			return nil
		case StateFailed:
			return fmt.Errorf("room: join failed: %s", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
