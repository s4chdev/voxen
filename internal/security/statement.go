// Package security implements the authentication layer that binds peer
// identities to their actual media transports.
//
// Threat model (see docs/security.md for the full document):
//
//   - The signaling server is honest-but-curious for availability, and may be
//     actively malicious about *metadata*. It can drop, reorder or forge relayed
//     messages. It must never be able to make voxen send media to a party the
//     local user did not authenticate.
//   - The network is untrusted: DTLS-SRTP protects media in transit, but the SDP
//     fingerprint is relayed by the signaling server and therefore untrusted on
//     its own.
//
// The binding works as follows. Each peer signs a SessionStatement with its
// Ed25519 identity key. The statement contains the SHA-256 fingerprint of the
// peer's DTLS certificate. After the DTLS handshake completes, the receiver
// recomputes the fingerprint from the *actual* certificate presented during the
// handshake and requires an exact match with the signed statement. A malicious
// signaling server can substitute an SDP fingerprint, but it cannot produce a
// statement signed by the peer, nor a DTLS certificate whose hash matches a
// statement it did not sign. Media is only started after this check succeeds.
package security

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProtocolVersion is the wire version of signed statements.
const ProtocolVersion = 1

// Domain separation prefixes. Every signed structure has its own prefix so a
// signature over one structure can never be replayed as another.
const (
	domainSessionStatement = "voxen/v1/session-statement"
	domainRoomMembership   = "voxen/v1/room-membership"
	domainDTLSBinding      = "voxen/v1/dtls-binding"
)

// Size constants.
const (
	NonceSize        = 16
	MaxStatementSize = 4096
)

// Errors returned by this package.
var (
	ErrSignatureInvalid = errors.New("security: signature verification failed")
	ErrExpired          = errors.New("security: statement has expired")
	ErrNotYetValid      = errors.New("security: statement is not valid yet")
	ErrNonceReused      = errors.New("security: nonce has already been used (possible replay)")
	ErrStaleStatement   = errors.New("security: statement is older than a previously accepted one")
	ErrFingerprintMatch = errors.New("security: DTLS certificate fingerprint does not match the signed statement")
	ErrRoomMismatch     = errors.New("security: statement is for a different room or session")
	ErrUnknownIssuer    = errors.New("security: statement issuer is not the expected identity")
	ErrMemberNotListed  = errors.New("security: peer is not a member of the signed roster")
	ErrRosterRollback   = errors.New("security: roster sequence went backwards")
	ErrMalformed        = errors.New("security: malformed statement")
)

// -----------------------------------------------------------------------------
// Canonical encoding
// -----------------------------------------------------------------------------

// encoder produces a deterministic, length-prefixed byte encoding suitable for
// signing. It avoids JSON entirely: JSON implementations differ in key order,
// duplicate-key handling and number formatting, any of which can turn a signed
// payload into an ambiguous one.
type encoder struct {
	buf []byte
}

func (e *encoder) bytes(b []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	e.buf = append(e.buf, l[:]...)
	e.buf = append(e.buf, b...)
}

func (e *encoder) str(s string) { e.bytes([]byte(s)) }

func (e *encoder) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

func (e *encoder) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// -----------------------------------------------------------------------------
// Session statements
// -----------------------------------------------------------------------------

// SessionStatement is a peer's signed claim: "this Ed25519 identity owns the
// DTLS certificate with this fingerprint for this room and session until this
// time". It is exchanged during session setup, before media flows.
type SessionStatement struct {
	Version int
	RoomID  string
	// SessionID identifies one WebRTC negotiation (and changes on ICE restart
	// with a new DTLS handshake). It prevents cross-session replay.
	SessionID string
	// IssuerPublicKey is the signer. The verifier must already expect this key
	// (for example from the room roster); it is included so the statement is
	// self-describing for audit logs.
	IssuerPublicKey ed25519.PublicKey
	// DTLSFingerprint is the SHA-256 of the DER-encoded DTLS certificate.
	DTLSFingerprint [32]byte
	Nonce           [NonceSize]byte
	IssuedAt        time.Time
	ExpiresAt       time.Time
}

// Sign produces the detached signature over the canonical encoding.
func (s *SessionStatement) Sign(priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("security: invalid signing key")
	}
	return ed25519.Sign(priv, s.canonical()), nil
}

// SignedBytes returns the exact bytes that are signed. Exposed for tests and for
// independent verification tooling.
func (s *SessionStatement) SignedBytes() []byte { return s.canonical() }

func (s *SessionStatement) canonical() []byte {
	e := &encoder{}
	e.str(domainSessionStatement)
	e.u32(uint32(s.Version))
	e.str(s.RoomID)
	e.str(s.SessionID)
	e.bytes(s.IssuerPublicKey)
	e.bytes(s.DTLSFingerprint[:])
	e.bytes(s.Nonce[:])
	e.u64(uint64(s.IssuedAt.UnixMilli()))
	e.u64(uint64(s.ExpiresAt.UnixMilli()))
	out := make([]byte, len(e.buf))
	copy(out, e.buf)
	return out
}

// StatementPolicy is the verification policy. It is constructed from validated
// configuration, never from peer-supplied data.
type StatementPolicy struct {
	// Now is the verification clock. Tests inject a fixed clock.
	Now func() time.Time
	// MaxClockSkew tolerates small clock differences between peers.
	MaxClockSkew time.Duration
	// MaxLifetime bounds how far in the future ExpiresAt may be, so a peer
	// cannot mint a statement valid for a year.
	MaxLifetime time.Duration
	// ExpectedRoom and ExpectedSession pin the statement to the current
	// negotiation.
	ExpectedRoom    string
	ExpectedSession string
	// ExpectedIssuer, when non-nil, requires the statement to be signed by this
	// exact key. It must come from the verified roster, never from the statement
	// itself.
	ExpectedIssuer ed25519.PublicKey
}

func (p StatementPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Verify checks the signature and all validity windows. It does not consult the
// replay guard; call ReplayGuard.Check separately so that the two concerns can
// be tested independently.
func (s *SessionStatement) Verify(sig []byte, policy StatementPolicy) error {
	if s.Version != ProtocolVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrMalformed, s.Version)
	}
	if s.RoomID == "" || s.SessionID == "" {
		return fmt.Errorf("%w: empty room or session", ErrMalformed)
	}
	if len(s.IssuerPublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: invalid issuer key length", ErrMalformed)
	}
	if policy.ExpectedIssuer != nil {
		if len(policy.ExpectedIssuer) != ed25519.PublicKeySize || !equalBytes(policy.ExpectedIssuer, s.IssuerPublicKey) {
			return ErrUnknownIssuer
		}
	}
	if policy.ExpectedRoom != "" && s.RoomID != policy.ExpectedRoom {
		return fmt.Errorf("%w: room %q != %q", ErrRoomMismatch, s.RoomID, policy.ExpectedRoom)
	}
	if policy.ExpectedSession != "" && s.SessionID != policy.ExpectedSession {
		return fmt.Errorf("%w: session %q != %q", ErrRoomMismatch, s.SessionID, policy.ExpectedSession)
	}

	now := policy.now()
	if now.Before(s.IssuedAt.Add(-policy.MaxClockSkew)) {
		return fmt.Errorf("%w: issued in the future", ErrNotYetValid)
	}
	if !now.Before(s.ExpiresAt) {
		return ErrExpired
	}
	if s.ExpiresAt.Before(s.IssuedAt) {
		return fmt.Errorf("%w: expiry precedes issue time", ErrMalformed)
	}
	if policy.MaxLifetime > 0 && s.ExpiresAt.Sub(s.IssuedAt) > policy.MaxLifetime {
		return fmt.Errorf("%w: validity window exceeds %s", ErrMalformed, policy.MaxLifetime)
	}
	if s.ExpiresAt.Sub(now) > policy.MaxLifetime && policy.MaxLifetime > 0 {
		return fmt.Errorf("%w: statement extends too far into the future", ErrMalformed)
	}

	if !ed25519.Verify(s.IssuerPublicKey, s.canonical(), sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// DefaultStatementTTL is the validity window voxen requests for its own
// statements. It is short because statements are reissued per session.
func DefaultStatementTTL() time.Duration { return 2 * time.Minute }

// NewSessionStatement builds a statement for the local identity.
func NewSessionStatement(roomID, sessionID string, issuer ed25519.PublicKey, dtlsFingerprint [32]byte, ttl time.Duration) (*SessionStatement, error) {
	if roomID == "" || sessionID == "" {
		return nil, fmt.Errorf("%w: room and session are required", ErrMalformed)
	}
	if ttl <= 0 {
		ttl = DefaultStatementTTL()
	}
	stmt := &SessionStatement{
		Version:         ProtocolVersion,
		RoomID:          roomID,
		SessionID:       sessionID,
		IssuerPublicKey: issuer,
		DTLSFingerprint: dtlsFingerprint,
		IssuedAt:        time.Now().UTC(),
		ExpiresAt:       time.Now().UTC().Add(ttl),
	}
	if _, err := rand.Read(stmt.Nonce[:]); err != nil {
		return nil, fmt.Errorf("security: generate nonce: %w", err)
	}
	return stmt, nil
}

// -----------------------------------------------------------------------------
// DTLS certificate binding
// -----------------------------------------------------------------------------

// CertificateFingerprint computes the SHA-256 digest of the DER encoding of the
// certificate presented during the DTLS handshake. The instance tag is ignored
// because voxen always uses a single certificate on both ends.
func CertificateFingerprint(cert *x509.Certificate) ([32]byte, error) {
	var out [32]byte
	if cert == nil {
		return out, errors.New("security: nil DTLS certificate")
	}
	sum := sha256.Sum256(cert.Raw)
	copy(out[:], sum[:])
	return out, nil
}

// FingerprintHex renders a certificate fingerprint in the form used by SDP
// ("aa:bb:..." lowercase, colon separated).
func FingerprintHex(fp [32]byte) string {
	parts := make([]string, len(fp))
	for i, b := range fp {
		parts[i] = hex.EncodeToString([]byte{b})
	}
	return strings.Join(parts, ":")
}

// ParseSDPFingerprint parses an SDP "a=fingerprint:sha-256 AA:BB:.." value.
// Only sha-256 is accepted; voxen never negotiates weaker digest algorithms.
func ParseSDPFingerprint(s string) ([32]byte, error) {
	var out [32]byte
	value := strings.TrimSpace(s)
	if i := strings.IndexByte(value, ' '); i >= 0 {
		alg := strings.ToLower(value[:i])
		value = strings.TrimSpace(value[i+1:])
		if alg != "sha-256" {
			return out, fmt.Errorf("security: unsupported fingerprint algorithm %q", alg)
		}
	}
	value = strings.ReplaceAll(value, ":", "")
	if len(value) != 64 {
		return out, fmt.Errorf("security: SDP fingerprint must be 32 bytes, got %d", len(value)/2)
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return out, fmt.Errorf("security: SDP fingerprint is not valid hex: %w", err)
	}
	copy(out[:], raw)
	return out, nil
}

// BindDTLS checks that the fingerprint in a verified statement matches the
// certificate actually negotiated. This is the step that converts a relayed,
// untrusted SDP fingerprint into an authenticated one. It must be called after
// the DTLS handshake and before any audio is attached.
func BindDTLS(stmt *SessionStatement, negotiated *x509.Certificate) error {
	actual, err := CertificateFingerprint(negotiated)
	if err != nil {
		return err
	}
	if !EqualFingerprint(actual, stmt.DTLSFingerprint) {
		return fmt.Errorf("%w: signed %s, negotiated %s",
			ErrFingerprintMatch, FingerprintHex(stmt.DTLSFingerprint), FingerprintHex(actual))
	}
	return nil
}

// EqualFingerprint compares fingerprints in constant time.
func EqualFingerprint(a, b [32]byte) bool {
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// -----------------------------------------------------------------------------
// Issuer key extraction helpers
// -----------------------------------------------------------------------------

// PublicKeyFromECDSACert is a helper for tests that need to confirm a Pion DTLS
// certificate is a self-signed ECDSA certificate (Pion's default). voxen never
// uses the certificate's own key for authentication; only the Ed25519 statement
// matters.
func PublicKeyFromECDSACert(cert *x509.Certificate) (*ecdsa.PublicKey, bool) {
	if cert == nil {
		return nil, false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return pub, ok
}

// SortMembers returns a stable ordering for member lists so canonical encodings
// of rosters do not depend on input order.
func SortMembers(keys []ed25519.PublicKey) {
	sort.Slice(keys, func(i, j int) bool {
		return strings.Compare(hex.EncodeToString(keys[i]), hex.EncodeToString(keys[j])) < 0
	})
}
