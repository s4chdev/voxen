// Package identity implements the Ed25519 identity plane.
//
// Responsibilities:
//
//   - generation and secure storage of the local Ed25519 key pair
//   - stable, human-verifiable fingerprints of public keys
//   - a trust store of peer fingerprints that the user has explicitly verified
//
// The identity plane is deliberately independent of the media plane. Nothing in
// this package knows about WebRTC, Opus or audio devices, and the media plane
// never reads key files directly.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Size constants of the Ed25519 construction.
const (
	PublicKeySize  = ed25519.PublicKeySize  // 32
	PrivateKeySize = ed25519.PrivateKeySize // 64 (seed || public)
	SeedSize       = ed25519.SeedSize       // 32
)

// Errors returned by the identity plane. Callers should use errors.Is.
var (
	ErrNoIdentity      = errors.New("identity: no local identity; run 'voxen init'")
	ErrCorruptIdentity = errors.New("identity: stored identity is corrupt")
	ErrUnknownPeer     = errors.New("identity: peer is not in the trust store")
	ErrNotVerified     = errors.New("identity: peer identity has not been verified")
)

// Fingerprint is the SHA-256 digest of an Ed25519 public key.
//
// A fingerprint is a *local* trust anchor: it is not a secret, but a peer may
// only send media after its fingerprint has been verified out of band and added
// to the trust store.
type Fingerprint [32]byte

// FingerprintFromPublic derives the fingerprint of a public key.
func FingerprintFromPublic(pub ed25519.PublicKey) Fingerprint {
	return sha256.Sum256(pub)
}

// ParseFingerprint parses the display form produced by String. Whitespace,
// separators and case are ignored; the input must decode to exactly 32 bytes.
func ParseFingerprint(s string) (Fingerprint, error) {
	var fp Fingerprint
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', ':', '-', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	clean = strings.ToLower(clean)
	if len(clean) != 64 {
		return fp, fmt.Errorf("identity: fingerprint must be 64 hex characters, got %d", len(clean))
	}
	raw, err := decodeHex(clean)
	if err != nil {
		return fp, fmt.Errorf("identity: fingerprint is not valid hex: %w", err)
	}
	copy(fp[:], raw)
	return fp, nil
}

// String renders the fingerprint in groups of four uppercase hex characters,
// which is easier to compare over a phone call than a raw digest.
func (f Fingerprint) String() string {
	hex := strings.ToUpper(fmt.Sprintf("%x", f[:]))
	var b strings.Builder
	for i := 0; i < len(hex); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(hex[i : i+4])
	}
	return b.String()
}

// Short returns the first and last group, suitable for compact log lines.
func (f Fingerprint) Short() string {
	parts := strings.Split(f.String(), " ")
	if len(parts) < 2 {
		return f.String()
	}
	return parts[0] + "…" + parts[len(parts)-1]
}

// Bytes returns a copy of the raw digest.
func (f Fingerprint) Bytes() []byte {
	out := make([]byte, len(f))
	copy(out, f[:])
	return out
}

// Equal reports whether two fingerprints are identical using a constant-time
// comparison, so verification code cannot leak matching prefixes via timing.
func (f Fingerprint) Equal(other Fingerprint) bool {
	return subtle.ConstantTimeCompare(f[:], other[:]) == 1
}

// IsZero reports whether f is the unset fingerprint.
func (f Fingerprint) IsZero() bool {
	return f == Fingerprint{}
}

// Identity is a loaded local identity. It never serialises the private key as a
// slog value: there is intentionally no LogValue method.
type Identity struct {
	DisplayName string
	PublicKey   ed25519.PublicKey
	privateKey  ed25519.PrivateKey
	createdAt   string
}

// New generates a fresh identity using crypto/rand.
func New(displayName string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate Ed25519 key: %w", err)
	}
	return &Identity{
		DisplayName: displayName,
		PublicKey:   pub,
		privateKey:  priv,
	}, nil
}

// Fingerprint returns the local fingerprint.
func (i *Identity) Fingerprint() Fingerprint {
	return FingerprintFromPublic(i.PublicKey)
}

// Sign returns an Ed25519 signature over msg using the local private key.
func (i *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(i.privateKey, msg)
}

// PrivateKey returns a copy of the private key. Only the signing helpers in the
// security package should need it; callers must never log or persist the result
// outside of Store.
func (i *Identity) PrivateKey() ed25519.PrivateKey {
	out := make(ed25519.PrivateKey, len(i.privateKey))
	copy(out, i.privateKey)
	return out
}

// Seed returns the 32-byte Ed25519 seed (the canonical private representation).
func (i *Identity) Seed() []byte {
	seed := i.privateKey.Seed()
	out := make([]byte, len(seed))
	copy(out, seed)
	return out
}

// PublicKeyEncoded returns the base64 (std) encoding used on the wire and in
// stored records.
func (i *Identity) PublicKeyEncoded() string {
	return base64.StdEncoding.EncodeToString(i.PublicKey)
}

// CreatedAt returns the RFC3339 creation timestamp recorded in the key file.
func (i *Identity) CreatedAt() string { return i.createdAt }

// FromSeed reconstructs an identity from a stored seed. It is used by Store when
// loading an unencrypted key file and never accepts a public key on its own.
func FromSeed(displayName string, seed []byte, createdAt string) (*Identity, error) {
	if len(seed) != SeedSize {
		return nil, fmt.Errorf("%w: seed must be %d bytes, got %d", ErrCorruptIdentity, SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Identity{
		DisplayName: displayName,
		PublicKey:   priv.Public().(ed25519.PublicKey),
		privateKey:  priv,
		createdAt:   createdAt,
	}, nil
}

// Verify checks a signature made by the given public key.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

func decodeHex(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, err := hexVal(s[i*2])
		if err != nil {
			return nil, err
		}
		lo, err := hexVal(s[i*2+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex character %q", c)
	}
}
