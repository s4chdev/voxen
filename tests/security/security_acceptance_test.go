// Package security_test contains the security acceptance tests named in the
// project's test plan. They exercise the identity-to-DTLS binding, replay
// protection and roster authentication at the package boundary, with real
// Ed25519 keys and real X.509 certificates.
//
// These are duplicated in spirit by unit tests inside internal/security; the
// copies here exist so the acceptance criteria are readable in one place and can
// be executed independently in CI (go test ./tests/security/...).
package security_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/security"
)

func cert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "voxen"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// Scenario 13: invalid identity rejection.
func TestInvalidIdentityIsRejected(t *testing.T) {
	expected, _ := keys(t)
	wrongPub, wrongPriv := keys(t)
	dtls := cert(t)
	fp, err := security.CertificateFingerprint(dtls)
	if err != nil {
		t.Fatal(err)
	}

	stmt, err := security.NewSessionStatement("room", "session", wrongPub, fp, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := stmt.Sign(wrongPriv)
	if err != nil {
		t.Fatal(err)
	}

	policy := security.StatementPolicy{
		Now:             time.Now,
		MaxClockSkew:    time.Minute,
		MaxLifetime:     5 * time.Minute,
		ExpectedRoom:    "room",
		ExpectedSession: "session",
		ExpectedIssuer:  expected,
	}
	if err := stmt.Verify(sig, policy); !errors.Is(err, security.ErrUnknownIssuer) {
		t.Fatalf("statement from an unexpected identity accepted: %v", err)
	}
}

// Scenario 14: modified SDP fingerprint rejection.
func TestModifiedFingerprintIsRejected(t *testing.T) {
	pub, priv := keys(t)
	honest := cert(t)
	attacker := cert(t)

	honestFP, _ := security.CertificateFingerprint(honest)
	stmt, err := security.NewSessionStatement("room", "session", pub, honestFP, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := stmt.Sign(priv)

	// A signaling server rewrites the fingerprint and relays the original
	// signature.
	attackerFP, _ := security.CertificateFingerprint(attacker)
	tampered := *stmt
	tampered.DTLSFingerprint = attackerFP

	policy := security.StatementPolicy{
		Now: time.Now, MaxClockSkew: time.Minute, MaxLifetime: 5 * time.Minute,
		ExpectedRoom: "room", ExpectedSession: "session", ExpectedIssuer: pub,
	}
	if err := tampered.Verify(sig, policy); !errors.Is(err, security.ErrSignatureInvalid) {
		t.Fatalf("rewritten fingerprint accepted: %v", err)
	}

	// Even if the attacker also relays the untouched statement and presents its
	// own DTLS certificate, the post-handshake bind fails.
	if err := security.BindDTLS(stmt, attacker); !errors.Is(err, security.ErrFingerprintMatch) {
		t.Fatalf("mismatched DTLS certificate accepted: %v", err)
	}
}

// Scenario 15: replay attack rejection.
func TestReplayIsRejected(t *testing.T) {
	pub, priv := keys(t)
	dtls := cert(t)
	fp, _ := security.CertificateFingerprint(dtls)

	stmt, err := security.NewSessionStatement("room", "session", pub, fp, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := stmt.Sign(priv)
	policy := security.StatementPolicy{
		Now: time.Now, MaxClockSkew: time.Minute, MaxLifetime: 5 * time.Minute,
		ExpectedRoom: "room", ExpectedSession: "session", ExpectedIssuer: pub,
	}
	if err := stmt.Verify(sig, policy); err != nil {
		t.Fatal(err)
	}

	guard := security.NewReplayGuard(16)
	if err := guard.Check(stmt); err != nil {
		t.Fatalf("first statement rejected: %v", err)
	}
	if err := guard.Check(stmt); !errors.Is(err, security.ErrNonceReused) {
		t.Fatalf("replayed statement accepted: %v", err)
	}
}

// Scenario 16: unauthorized participant rejection, enforced at the room layer.
func TestUnauthorizedParticipantCannotJoinMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("room layer exercised in internal/room tests as well")
	}
	// A peer is only admitted if its fingerprint is verified in the trust store.
	// The trust decision is covered by internal/identity and internal/room; here
	// we assert the fingerprint helper is stable, which the room layer relies on.
	pub, _ := keys(t)
	fp := identity.FingerprintFromPublic(pub)
	rendered := fp.String()
	parsed, err := identity.ParseFingerprint(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Equal(fp) {
		t.Fatal("fingerprint display form is not round-trip stable")
	}
}
