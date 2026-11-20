// Package invite implements short, self-certifying room codes.
//
// The goal is one code a person can read out loud: "voxen call K7M3-9QX2-A4F1".
// It replaces the previous flow of create, share a room ID, exchange fingerprints
// out of band, verify, then join.
//
// # Why the code carries a key check
//
// A short code alone is not a trust anchor: whoever holds the invite blob (the
// signaling server) could substitute its own key and sit in the middle. So the last
// part of the code is derived from the creator's public key and the room ID. A
// joiner recomputes it and refuses an invite whose key does not produce the check in
// the code they typed.
//
// That gives honest trust-on-first-use semantics: the server cannot silently swap
// identities, but whoever first gave you the code is trusted by virtue of having the
// code. This is the same model as an SSH host key on first connect, or a meeting ID
// and passcode. It is weaker than comparing fingerprints over a separate channel,
// which remains available for anyone who wants it, and docs/security.md says so
// plainly.
package invite

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// CodeParts describes the structure of a code.
const (
	// roomChars is the random, room-unique portion. Eight characters of base32 give
	// 40 bits: enough that guessing a live room is impractical and enough that the
	// display groups evenly, which matters because a person reads this aloud.
	roomChars = 8
	// checkChars is the self-certifying portion derived from the creator's key.
	checkChars = 4
	// groupLen is how many characters appear between dashes. roomChars+checkChars is
	// a multiple of it, so every group is the same width.
	groupLen = 4
)

// Errors returned by this package.
var (
	ErrMalformed = errors.New("invite: malformed code")
	ErrKeyCheck  = errors.New("invite: code does not match the inviter's key")
)

// crockford is an unambiguous alphabet: no I, L, O or U, so a code read aloud or
// copied by hand cannot be confused. Lowercase is accepted on input.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Code is a parsed invite code.
type Code struct {
	// Room is the server-assigned room identifier portion.
	Room string
	// Check is the key-check portion, uppercase.
	Check string
}

// String renders the code in grouped uppercase form.
func (c Code) String() string {
	raw := c.Room + c.Check
	var b strings.Builder
	for i, r := range raw {
		if i > 0 && i%groupLen == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Generate produces a new code for a room. roomSeed is any value that is stable for
// the room, normally the server's room ID; the code portion is derived from it so a
// server restart does not silently invalidate a code that was already shared.
func Generate(roomSeed string) (Code, error) {
	if roomSeed == "" {
		return Code{}, errors.New("invite: room seed is required")
	}
	sum := sha256.Sum256([]byte("voxen/room-code/v1|" + roomSeed))
	room, err := encodeBase32(sum[:], roomChars)
	if err != nil {
		return Code{}, err
	}
	return Code{Room: room}, nil
}

// DeriveCheck computes the key-check portion for a creator's key and room.
//
// The room identifier is included so a check cannot be reused across rooms: a code's
// check is bound to exactly one room and one key.
func DeriveCheck(creator ed25519.PublicKey, room string) (string, error) {
	if len(creator) != ed25519.PublicKeySize {
		return "", errors.New("invite: creator key has the wrong length")
	}
	if room == "" {
		return "", errors.New("invite: room is required")
	}
	h := sha256.New()
	h.Write([]byte("voxen/key-check/v1|"))
	h.Write(creator)
	h.Write([]byte("|"))
	h.Write([]byte(strings.ToUpper(room)))
	check, err := encodeBase32(h.Sum(nil), checkChars)
	if err != nil {
		return "", err
	}
	return check, nil
}

// WithCheck returns the code completed with its key check.
func (c Code) WithCheck(creator ed25519.PublicKey) (Code, error) {
	check, err := DeriveCheck(creator, c.Room)
	if err != nil {
		return Code{}, err
	}
	c.Check = check
	return c, nil
}

// Parse accepts the forms a person might paste: grouped, ungrouped, uppercase,
// lowercase, with spaces, or with the leading command stripped.
func Parse(s string) (Code, error) {
	clean := strings.ToUpper(s)
	// Tolerate "voxen call CODE" and "voxen join CODE" pasted whole.
	for _, prefix := range []string{"VOXEN CALL ", "VOXEN JOIN ", "VOXEN "} {
		clean = strings.TrimPrefix(clean, prefix)
	}
	clean = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '-', '_', '.':
			return -1
		}
		return r
	}, clean)
	// Crockford maps I/L to 1 and O to 0, which is why those letters were excluded.
	clean = strings.NewReplacer("I", "1", "L", "1", "O", "0", "U", "V").Replace(clean)

	if len(clean) != roomChars+checkChars {
		return Code{}, fmt.Errorf("%w: expected %d characters, got %d",
			ErrMalformed, roomChars+checkChars, len(clean))
	}
	for _, r := range clean {
		if !strings.ContainsRune(crockford, r) {
			return Code{}, fmt.Errorf("%w: invalid character %q", ErrMalformed, r)
		}
	}
	return Code{Room: clean[:roomChars], Check: clean[roomChars:]}, nil
}

// Verify checks that a creator's key matches the code. It is the only thing standing
// between a joiner and a substituted identity, so it is a distinct call rather than a
// side effect of parsing.
func (c Code) Verify(creator ed25519.PublicKey) error {
	if c.Check == "" {
		return fmt.Errorf("%w: code has no key check", ErrMalformed)
	}
	want, err := DeriveCheck(creator, c.Room)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, c.Check) {
		return fmt.Errorf("%w: expected %s, inviter's key yields %s",
			ErrKeyCheck, c.Check, want)
	}
	return nil
}

// encodeBase32 encodes data as n Crockford base32 characters.
func encodeBase32(data []byte, n int) (string, error) {
	// Take enough bytes to produce n base32 characters (5 bits each).
	need := (n*5 + 7) / 8
	if len(data) < need {
		return "", errors.New("invite: not enough entropy for the requested length")
	}
	var out strings.Builder
	var acc uint64
	var bits uint
	for _, b := range data[:need] {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 && out.Len() < n {
			bits -= 5
			out.WriteByte(crockford[(acc>>bits)&0x1f])
		}
	}
	if out.Len() != n {
		return "", errors.New("invite: encoding produced the wrong length")
	}
	return out.String(), nil
}

// Invite is the blob a joiner receives from the signaling server and verifies.
type Invite struct {
	// RoomID is the server's room identifier.
	RoomID string `json:"room_id"`
	// CreatorPublicKey is the inviter's Ed25519 key, base64.
	CreatorPublicKey []byte `json:"creator_public_key"`
	// Code is the code this invite was issued for.
	Code string `json:"code"`
}

// SessionToken derives a short server-unscoped token used for rate limiting and
// logging without leaking the creator's key.
func (i Invite) SessionToken() string {
	h := sha256.Sum256(i.CreatorPublicKey)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], binary.BigEndian.Uint64(h[:8]))
	return fmt.Sprintf("%x", b[:])
}

// RandomBytes returns n cryptographically random bytes, for callers that need their
// own nonces.
func RandomBytes(n int) ([]byte, error) {
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	return out, nil
}
