package security

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCert builds a self-signed ECDSA certificate, mirroring what Pion's DTLS
// transport generates for a WebRTC peer connection.
func testCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "voxen-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func certFingerprint(t *testing.T, cert *x509.Certificate) [32]byte {
	t.Helper()
	fp, err := CertificateFingerprint(cert)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func newIssuer(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func policy(now time.Time, pub ed25519.PublicKey) StatementPolicy {
	return StatementPolicy{
		Now:             func() time.Time { return now },
		MaxClockSkew:    60 * time.Second,
		MaxLifetime:     5 * time.Minute,
		ExpectedRoom:    "room-1",
		ExpectedSession: "session-1",
		ExpectedIssuer:  pub,
	}
}

func TestSessionStatementHappyPath(t *testing.T) {
	pub, priv := newIssuer(t)
	cert := testCert(t)
	fp := certFingerprint(t, cert)

	stmt, err := NewSessionStatement("room-1", "session-1", pub, fp, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := stmt.Sign(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := stmt.Verify(sig, policy(time.Now(), pub)); err != nil {
		t.Fatalf("valid statement rejected: %v", err)
	}
	// The signed fingerprint must be the one actually negotiated.
	if err := BindDTLS(stmt, cert); err != nil {
		t.Fatalf("valid DTLS binding rejected: %v", err)
	}
}

// Requirement: "Modified SDP fingerprint rejection". A signaling server that
// rewrites the fingerprint in the SDP cannot produce a matching signed
// statement, and a statement signed for another certificate fails the DTLS bind.
func TestModifiedSDPFingerprintRejected(t *testing.T) {
	pub, priv := newIssuer(t)
	legit := testCert(t)
	attacker := testCert(t)

	stmt, err := NewSessionStatement("room-1", "session-1", pub, certFingerprint(t, legit), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := stmt.Sign(priv)

	// The signature is valid for the honest statement...
	if err := stmt.Verify(sig, policy(time.Now(), pub)); err != nil {
		t.Fatalf("baseline statement should verify: %v", err)
	}

	// ...but substituting the fingerprint invalidates it.
	tampered := *stmt
	tampered.DTLSFingerprint = certFingerprint(t, attacker)
	if err := tampered.Verify(sig, policy(time.Now(), pub)); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("tampered statement accepted or wrong error: %v", err)
	}

	// And if the attacker negotiates its own certificate while the honest
	// statement is relayed, the DTLS bind fails.
	if err := BindDTLS(stmt, attacker); !errors.Is(err, ErrFingerprintMatch) {
		t.Fatalf("attacker certificate accepted: %v", err)
	}
}

// Requirement: "Invalid identity rejection". A statement signed by a different
// key than the expected roster identity must be refused.
func TestStatementFromUnexpectedIssuerRejected(t *testing.T) {
	expectedPub, _ := newIssuer(t)
	otherPub, otherPriv := newIssuer(t)
	cert := testCert(t)

	stmt, _ := NewSessionStatement("room-1", "session-1", otherPub, certFingerprint(t, cert), time.Minute)
	sig, _ := stmt.Sign(otherPriv)

	if err := stmt.Verify(sig, policy(time.Now(), expectedPub)); !errors.Is(err, ErrUnknownIssuer) {
		t.Fatalf("unexpected issuer accepted: %v", err)
	}
}

func TestStatementExpiryAndFuture(t *testing.T) {
	pub, priv := newIssuer(t)
	cert := testCert(t)
	stmt, _ := NewSessionStatement("room-1", "session-1", pub, certFingerprint(t, cert), time.Minute)
	sig, _ := stmt.Sign(priv)

	// Far in the future: not yet valid.
	if err := stmt.Verify(sig, policy(time.Now().Add(-time.Hour), pub)); !errors.Is(err, ErrNotYetValid) &&
		!errors.Is(err, ErrExpired) {
		t.Fatalf("future clock should reject: %v", err)
	}
	// Well after expiry.
	if err := stmt.Verify(sig, policy(stmt.ExpiresAt.Add(time.Minute), pub)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired statement accepted: %v", err)
	}
}

func TestStatementRejectsRoomAndSessionMismatch(t *testing.T) {
	pub, priv := newIssuer(t)
	cert := testCert(t)
	stmt, _ := NewSessionStatement("room-1", "session-1", pub, certFingerprint(t, cert), time.Minute)
	sig, _ := stmt.Sign(priv)

	p := policy(time.Now(), pub)
	p.ExpectedRoom = "room-2"
	if err := stmt.Verify(sig, p); !errors.Is(err, ErrRoomMismatch) {
		t.Fatalf("cross-room statement accepted: %v", err)
	}

	p = policy(time.Now(), pub)
	p.ExpectedSession = "session-2"
	if err := stmt.Verify(sig, p); !errors.Is(err, ErrRoomMismatch) {
		t.Fatalf("cross-session statement accepted: %v", err)
	}
}

func TestStatementRejectsOverlongLifetime(t *testing.T) {
	pub, priv := newIssuer(t)
	cert := testCert(t)
	// A peer tries to mint a statement valid for a year.
	stmt, _ := NewSessionStatement("room-1", "session-1", pub, certFingerprint(t, cert), 365*24*time.Hour)
	sig, _ := stmt.Sign(priv)
	if err := stmt.Verify(sig, policy(time.Now(), pub)); err == nil {
		t.Fatal("overlong validity window accepted")
	}
}

func TestCanonicalEncodingIsStableAndDomainSeparated(t *testing.T) {
	pub, _ := newIssuer(t)
	var fp [32]byte
	stmt := &SessionStatement{
		Version: ProtocolVersion, RoomID: "r", SessionID: "s",
		IssuerPublicKey: pub, DTLSFingerprint: fp,
		IssuedAt: time.Unix(1700000000, 0), ExpiresAt: time.Unix(1700000120, 0),
	}
	first := hex.EncodeToString(stmt.SignedBytes())
	second := hex.EncodeToString(stmt.SignedBytes())
	if first != second {
		t.Fatal("canonical encoding is not deterministic")
	}
	// Changing a single field changes the transcript.
	clone := *stmt
	clone.RoomID = "r2"
	if hex.EncodeToString(clone.SignedBytes()) == first {
		t.Fatal("room id not covered by the canonical encoding")
	}
	// Domain separation: a membership document with similar fields must not
	// produce the same bytes.
	m := &RoomMembership{
		Version: ProtocolVersion, RoomID: "r", Sequence: 0, Issuer: pub,
		IssuedAt: stmt.IssuedAt, ExpiresAt: stmt.ExpiresAt,
	}
	if hex.EncodeToString(m.SignedBytes()) == first {
		t.Fatal("session statement and roster share a canonical encoding")
	}
}

func TestSDPFingerprintParsing(t *testing.T) {
	pub, _ := newIssuer(t)
	var fp [32]byte
	copy(fp[:], []byte(strings.Repeat("a", 32)))
	_ = pub
	colon := strings.TrimSuffix(strings.ReplaceAll(strings.Join(hexGroups(fp[:]), ":"), ":", ":"), ":")
	parsed, err := ParseSDPFingerprint("sha-256 " + colon)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed != fp {
		t.Fatal("parsed fingerprint differs")
	}
	if _, err := ParseSDPFingerprint("sha-1 " + colon); err == nil {
		t.Fatal("sha-1 must be rejected")
	}
	if _, err := ParseSDPFingerprint("sha-256 deadbeef"); err == nil {
		t.Fatal("short fingerprint must be rejected")
	}
	if FingerprintHex(fp) != colon {
		t.Fatalf("FingerprintHex round trip: %q != %q", FingerprintHex(fp), colon)
	}
}

func hexGroups(b []byte) []string {
	out := make([]string, len(b))
	for i, v := range b {
		out[i] = hex.EncodeToString([]byte{v})
	}
	return out
}

// Requirement: "Replay attack rejection".
func TestReplayGuardRejectsExactReplay(t *testing.T) {
	pub, priv := newIssuer(t)
	cert := testCert(t)
	stmt, _ := NewSessionStatement("room-1", "session-1", pub, certFingerprint(t, cert), time.Minute)
	sig, _ := stmt.Sign(priv)

	guard := NewReplayGuard(64)
	if err := stmt.Verify(sig, policy(time.Now(), pub)); err != nil {
		t.Fatal(err)
	}
	if err := guard.Check(stmt); err != nil {
		t.Fatalf("first use rejected: %v", err)
	}
	if err := guard.Check(stmt); !errors.Is(err, ErrNonceReused) {
		t.Fatalf("replayed nonce accepted: %v", err)
	}
}

func TestReplayGuardRejectsRollback(t *testing.T) {
	pub, priv := newIssuer(t)
	cert := testCert(t)
	fp := certFingerprint(t, cert)
	guard := NewReplayGuard(64)

	older, _ := NewSessionStatement("room-1", "s-old", pub, fp, time.Minute)
	older.IssuedAt = time.Now().Add(-30 * time.Second).UTC()
	older.ExpiresAt = older.IssuedAt.Add(time.Minute)
	if err := guard.Check(older); err != nil {
		t.Fatal(err)
	}

	newer, _ := NewSessionStatement("room-1", "s-new", pub, fp, time.Minute)
	if err := guard.Check(newer); err != nil {
		t.Fatal(err)
	}

	// A third statement issued before "newer" but with a fresh nonce is a
	// rollback attempt.
	replay, _ := NewSessionStatement("room-1", "s-mid", pub, fp, time.Minute)
	replay.IssuedAt = newer.IssuedAt.Add(-time.Second)
	replay.ExpiresAt = newer.IssuedAt.Add(time.Minute)
	if err := guard.Check(replay); !errors.Is(err, ErrStaleStatement) {
		t.Fatalf("rollback accepted: %v", err)
	}
	_ = priv
}

func TestReplayGuardIsBounded(t *testing.T) {
	pub, _ := newIssuer(t)
	var fp [32]byte
	guard := NewReplayGuard(32)
	for i := 0; i < 500; i++ {
		stmt, _ := NewSessionStatement("r", "s", pub, fp, time.Minute)
		stmt.IssuedAt = time.Now().Add(time.Duration(i) * time.Second)
		stmt.ExpiresAt = stmt.IssuedAt.Add(time.Hour)
		if err := guard.Check(stmt); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if guard.Size() > 32 {
		t.Fatalf("guard grew beyond capacity: %d", guard.Size())
	}
}

// -----------------------------------------------------------------------------
// Signed room membership
// -----------------------------------------------------------------------------

func TestRoomMembershipVerification(t *testing.T) {
	adminPub, adminPriv := newIssuer(t)
	alicePub, _ := newIssuer(t)
	bobPub, _ := newIssuer(t)

	roster := NewRoomMembership("room-1", 1, adminPub, []Member{
		{PublicKey: bobPub, DisplayName: "bob"},
		{PublicKey: alicePub, DisplayName: "alice"},
	}, time.Hour)
	sig, err := roster.Sign(adminPriv)
	if err != nil {
		t.Fatal(err)
	}

	p := MembershipPolicy{
		Now:           time.Now,
		MaxClockSkew:  60 * time.Second,
		ExpectedRoom:  "room-1",
		ExpectedAdmin: adminPub,
	}
	if err := roster.Verify(sig, p); err != nil {
		t.Fatalf("valid roster rejected: %v", err)
	}
	if !roster.Contains(alicePub) || !roster.Contains(bobPub) {
		t.Fatal("roster should contain both members")
	}
	outsidePub, _ := newIssuer(t)
	if roster.Contains(outsidePub) {
		t.Fatal("non-member reported as member")
	}
	if err := roster.RequireMember(sig, outsidePub, p); !errors.Is(err, ErrMemberNotListed) {
		t.Fatalf("non-member admitted: %v", err)
	}
}

func TestRoomMembershipRejectsForgedAdmin(t *testing.T) {
	adminPub, _ := newIssuer(t)
	_, attackerPriv := newIssuer(t)
	alicePub, _ := newIssuer(t)

	// The attacker signs a roster listing themselves.
	roster := NewRoomMembership("room-1", 1, adminPub, []Member{
		{PublicKey: alicePub, DisplayName: "alice"},
	}, time.Hour)
	// Issuer field claims admin, signature comes from the attacker.
	forgedSig, _ := roster.Sign(attackerPriv)

	p := MembershipPolicy{Now: time.Now, MaxClockSkew: time.Minute, ExpectedRoom: "room-1", ExpectedAdmin: adminPub}
	if err := roster.Verify(forgedSig, p); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("forged roster accepted: %v", err)
	}
}

func TestRoomMembershipSequenceRollback(t *testing.T) {
	adminPub, adminPriv := newIssuer(t)
	alicePub, _ := newIssuer(t)

	roster := NewRoomMembership("room-1", 1, adminPub, []Member{{PublicKey: alicePub}}, time.Hour)
	sig, _ := roster.Sign(adminPriv)

	p := MembershipPolicy{
		Now: time.Now, MaxClockSkew: time.Minute,
		ExpectedRoom: "room-1", ExpectedAdmin: adminPub, MinSequence: 2,
	}
	if err := roster.Verify(sig, p); !errors.Is(err, ErrRosterRollback) {
		t.Fatalf("rolled back roster accepted: %v", err)
	}
}

func TestRoomMembershipRejectsDuplicateMembers(t *testing.T) {
	adminPub, adminPriv := newIssuer(t)
	alicePub, _ := newIssuer(t)
	roster := NewRoomMembership("room-1", 1, adminPub, []Member{
		{PublicKey: alicePub}, {PublicKey: alicePub},
	}, time.Hour)
	sig, _ := roster.Sign(adminPriv)
	p := MembershipPolicy{Now: time.Now, MaxClockSkew: time.Minute, ExpectedRoom: "room-1", ExpectedAdmin: adminPub}
	if err := roster.Verify(sig, p); err == nil {
		t.Fatal("duplicate members accepted")
	}
}

func TestEqualFingerprint(t *testing.T) {
	a := [32]byte{1, 2, 3}
	b := [32]byte{1, 2, 3}
	c := [32]byte{1, 2, 4}
	if !EqualFingerprint(a, b) {
		t.Fatal("identical fingerprints not equal")
	}
	if EqualFingerprint(a, c) {
		t.Fatal("different fingerprints equal")
	}
}
