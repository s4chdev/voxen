package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/s4chdev/voxen/internal/fsutil"
	"github.com/s4chdev/voxen/internal/paths"
)

// TrustFileVersion is the schema version of the trust store.
const TrustFileVersion = 1

const trustFileName = "trust.json"

// TrustLevel describes how much voxen knows about a peer key.
type TrustLevel string

const (
	// TrustUnverified means the key was seen but never confirmed out of band.
	// Unverified peers are never sent media.
	TrustUnverified TrustLevel = "unverified"
	// TrustVerified means the user compared the fingerprint with the peer over
	// a separate, already-authenticated channel.
	TrustVerified TrustLevel = "verified"
	// TrustRevoked means the key was explicitly revoked. A revoked peer is
	// rejected even if a stale verified record exists elsewhere.
	TrustRevoked TrustLevel = "revoked"
)

// TrustEntry is one peer record. Trust is anchored on the fingerprint: an entry
// may be created by verifying a fingerprint out of band, before any key has been
// observed. When a key is later observed its fingerprint must match the verified
// one, which is what makes a pre-shared fingerprint usable at all.
type TrustEntry struct {
	Fingerprint Fingerprint `json:"-"`
	// PublicKey is filled in once a key has actually been observed. It is empty
	// for a fingerprint-only pin.
	PublicKey   string     `json:"public_key,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
	Level       TrustLevel `json:"level"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Note        string     `json:"note,omitempty"`
	// Pinned marks an entry that was created from a fingerprint alone. Until a
	// key is observed there is nothing to compare against, so the pin proves
	// intent, not knowledge of the key.
	Pinned bool `json:"pinned,omitempty"`
}

// PublicKeyBytes decodes the stored public key, reporting whether one is present.
func (e TrustEntry) PublicKeyBytes() (ed25519.PublicKey, error) {
	if e.PublicKey == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(e.PublicKey)
	if err != nil || len(raw) != PublicKeySize {
		return nil, errors.New("identity: trust entry has an invalid public key")
	}
	pub := ed25519.PublicKey(raw)
	// The stored key must agree with the anchor fingerprint.
	if !FingerprintFromPublic(pub).Equal(e.Fingerprint) {
		return nil, errors.New("identity: trust entry public key does not match its fingerprint")
	}
	return pub, nil
}

// MatchesKey reports whether pub is the key this entry trusts.
func (e TrustEntry) MatchesKey(pub ed25519.PublicKey) bool {
	return len(pub) == PublicKeySize && FingerprintFromPublic(pub).Equal(e.Fingerprint)
}

type trustFileEntry struct {
	Fingerprint string     `json:"fingerprint"`
	PublicKey   string     `json:"public_key,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
	Level       TrustLevel `json:"level"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Note        string     `json:"note,omitempty"`
	Pinned      bool       `json:"pinned,omitempty"`
}

type trustFile struct {
	Version int              `json:"version"`
	Peers   []trustFileEntry `json:"peers"`
}

// TrustStore persists explicit trust decisions for peer fingerprints.
//
// Files are written atomically with 0600 permissions. A trust store that cannot
// be read is an error rather than an empty store: silently forgetting trust
// would turn a full disk into an authentication bypass.
type TrustStore struct {
	path    string
	entries map[Fingerprint]TrustEntry
}

// OpenTrust loads (or initialises) the trust store in the identity directory.
func OpenTrust(dir string) (*TrustStore, error) {
	path := dir + string(os.PathSeparator) + trustFileName
	ts := &TrustStore{path: path, entries: map[Fingerprint]TrustEntry{}}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ts, nil
	}
	if err != nil {
		return nil, fmt.Errorf("identity: read trust store: %w", err)
	}
	if err := paths.CheckPermissions(path, 0o600); err != nil {
		return nil, fmt.Errorf("identity: refusing insecure trust store: %w", err)
	}

	var tf trustFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("identity: trust store is corrupt: %w", err)
	}
	if tf.Version != TrustFileVersion {
		return nil, fmt.Errorf("identity: unsupported trust store version %d", tf.Version)
	}
	for _, rec := range tf.Peers {
		fp, err := ParseFingerprint(rec.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("identity: trust store entry %q: %w", rec.Fingerprint, err)
		}
		if fp.IsZero() {
			return nil, errors.New("identity: trust store contains an empty fingerprint")
		}
		entry := TrustEntry{
			Fingerprint: fp,
			PublicKey:   rec.PublicKey,
			DisplayName: rec.DisplayName,
			Level:       rec.Level,
			FirstSeen:   rec.FirstSeen,
			LastSeen:    rec.LastSeen,
			VerifiedAt:  rec.VerifiedAt,
			RevokedAt:   rec.RevokedAt,
			Note:        rec.Note,
			Pinned:      rec.Pinned,
		}
		// Validate a stored key against its anchor before trusting the file.
		if _, err := entry.PublicKeyBytes(); err != nil {
			return nil, fmt.Errorf("identity: trust store entry %s: %w", fp.Short(), err)
		}
		ts.entries[fp] = entry
	}
	return ts, nil
}

// Path returns the trust store file path.
func (t *TrustStore) Path() string { return t.path }

// Lookup returns the entry for fp and whether it exists.
func (t *TrustStore) Lookup(fp Fingerprint) (TrustEntry, bool) {
	e, ok := t.entries[fp]
	return e, ok
}

// Level returns the trust level for fp. Unknown keys are TrustUnverified.
func (t *TrustStore) Level(fp Fingerprint) TrustLevel {
	if e, ok := t.entries[fp]; ok {
		return e.Level
	}
	return TrustUnverified
}

// IsVerified reports whether fp may receive media.
func (t *TrustStore) IsVerified(fp Fingerprint) bool {
	return t.Level(fp) == TrustVerified
}

// Observe records that a key was seen, without granting trust. It is called when
// a peer is introduced by the signaling server or during session setup.
//
// A pinned (verified) fingerprint is never downgraded: if the derived
// fingerprint matches a verified pin, the observed key is bound to that pin and
// becomes usable. If it does not match, it is a different key and gets its own
// unverified record, so a peer cannot replace the pinned key.
func (t *TrustStore) Observe(pub ed25519.PublicKey, displayName string) (TrustEntry, error) {
	if len(pub) != PublicKeySize {
		return TrustEntry{}, errors.New("identity: invalid public key length")
	}
	fp := FingerprintFromPublic(pub)
	now := time.Now().UTC()
	entry, ok := t.entries[fp]
	if !ok {
		entry = TrustEntry{
			Fingerprint: fp,
			Level:       TrustUnverified,
			FirstSeen:   now,
		}
	}
	if entry.Level != TrustRevoked {
		entry.PublicKey = base64.StdEncoding.EncodeToString(pub)
		entry.Pinned = false
	}
	if entry.Level == TrustUnverified && displayName != "" {
		entry.DisplayName = displayName
	}
	if entry.Level == TrustVerified && entry.DisplayName == "" && displayName != "" {
		entry.DisplayName = displayName
	}
	entry.LastSeen = now
	t.entries[fp] = entry
	return entry, t.save()
}

// VerifyKey records an explicit out-of-band verification of a key. The key is
// stored so later sessions can compare against it directly.
func (t *TrustStore) VerifyKey(pub ed25519.PublicKey, displayName, note string) (TrustEntry, error) {
	if len(pub) != PublicKeySize {
		return TrustEntry{}, errors.New("identity: invalid public key length")
	}
	fp := FingerprintFromPublic(pub)
	now := time.Now().UTC()
	entry, ok := t.entries[fp]
	if !ok {
		entry = TrustEntry{Fingerprint: fp, FirstSeen: now}
	}
	entry.PublicKey = base64.StdEncoding.EncodeToString(pub)
	entry.Pinned = false
	entry.Level = TrustVerified
	entry.VerifiedAt = &now
	entry.RevokedAt = nil
	if displayName != "" {
		entry.DisplayName = displayName
	}
	if note != "" {
		entry.Note = note
	}
	entry.LastSeen = now
	t.entries[fp] = entry
	return entry, t.save()
}

// VerifyFingerprint pins a fingerprint the user compared out of band, without
// having seen the key yet. This is the normal flow before the first call: the
// peer reads their fingerprint, the user records it, and the key presented
// during the first session must hash to this value.
//
// The returned entry is verified even though no key is bound yet; the
// fingerprint is the trust anchor and the later key is checked against it.
func (t *TrustStore) VerifyFingerprint(fp Fingerprint, displayName, note string) (TrustEntry, error) {
	if fp.IsZero() {
		return TrustEntry{}, errors.New("identity: empty fingerprint")
	}
	now := time.Now().UTC()
	entry, ok := t.entries[fp]
	if !ok {
		entry = TrustEntry{Fingerprint: fp, FirstSeen: now, Pinned: true}
	}
	entry.Level = TrustVerified
	entry.VerifiedAt = &now
	entry.RevokedAt = nil
	if displayName != "" {
		entry.DisplayName = displayName
	}
	if note != "" {
		entry.Note = note
	}
	entry.LastSeen = now
	t.entries[fp] = entry
	return entry, t.save()
}

// BindKey completes a fingerprint-only pin with the key observed in a session.
// It fails if the key does not hash to the pinned fingerprint, which is the
// check that makes a pre-shared fingerprint meaningful.
func (t *TrustStore) BindKey(pub ed25519.PublicKey) (TrustEntry, error) {
	if len(pub) != PublicKeySize {
		return TrustEntry{}, errors.New("identity: invalid public key length")
	}
	fp := FingerprintFromPublic(pub)
	entry, ok := t.entries[fp]
	if !ok {
		return TrustEntry{}, fmt.Errorf("%w: %s", ErrUnknownPeer, fp.Short())
	}
	if entry.Level == TrustRevoked {
		return entry, fmt.Errorf("%w: %s is revoked", ErrNotVerified, fp.Short())
	}
	if entry.Level != TrustVerified {
		return entry, fmt.Errorf("%w: %s", ErrNotVerified, fp.Short())
	}
	entry.PublicKey = base64.StdEncoding.EncodeToString(pub)
	entry.Pinned = false
	t.entries[fp] = entry
	return entry, t.save()
}

// Revoke marks a key untrusted. It is retained rather than deleted so audit
// history survives rotation and so a revoked key cannot silently reappear as
// merely unverified.
func (t *TrustStore) Revoke(fp Fingerprint) error {
	entry, ok := t.entries[fp]
	if !ok {
		return ErrUnknownPeer
	}
	now := time.Now().UTC()
	entry.Level = TrustRevoked
	entry.RevokedAt = &now
	t.entries[fp] = entry
	return t.save()
}

// Remove deletes a record entirely.
func (t *TrustStore) Remove(fp Fingerprint) error {
	if _, ok := t.entries[fp]; !ok {
		return ErrUnknownPeer
	}
	delete(t.entries, fp)
	return t.save()
}

// List returns every entry sorted by display name then fingerprint.
func (t *TrustStore) List() []TrustEntry {
	out := make([]TrustEntry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].DisplayName), strings.ToLower(out[j].DisplayName)
		if a != b {
			return a < b
		}
		return strings.Compare(out[i].Fingerprint.String(), out[j].Fingerprint.String()) < 0
	})
	return out
}

func (t *TrustStore) save() error {
	tf := trustFile{Version: TrustFileVersion}
	for _, e := range t.entries {
		tf.Peers = append(tf.Peers, trustFileEntry{
			Fingerprint: e.Fingerprint.String(),
			PublicKey:   e.PublicKey,
			DisplayName: e.DisplayName,
			Level:       e.Level,
			FirstSeen:   e.FirstSeen,
			LastSeen:    e.LastSeen,
			VerifiedAt:  e.VerifiedAt,
			RevokedAt:   e.RevokedAt,
			Note:        e.Note,
			Pinned:      e.Pinned,
		})
	}
	sort.Slice(tf.Peers, func(i, j int) bool {
		return strings.Compare(tf.Peers[i].Fingerprint, tf.Peers[j].Fingerprint) < 0
	})
	raw, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return fmt.Errorf("identity: encode trust store: %w", err)
	}
	raw = append(raw, '\n')
	return fsutil.WriteFileAtomic(t.path, raw, 0o600)
}
