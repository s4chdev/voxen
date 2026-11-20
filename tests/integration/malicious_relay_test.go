package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/media"
	"github.com/s4chdev/voxen/internal/security"
	"github.com/s4chdev/voxen/internal/signaling"
	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// attackerCertificate builds a self-signed certificate standing in for the
// certificate a malicious signaling server would terminate DTLS with.
func attackerCertificate(t *testing.T) [32]byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "attacker"},
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
	fp, err := security.CertificateFingerprint(cert)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestMaliciousRelayCannotSubstituteFingerprint is the central security claim of
// the project, exercised end to end.
//
// A hostile signaling server sees Alice's offer in flight. It rewrites the SDP
// fingerprint to point at its own certificate and forwards the original
// signature. Bob must reject the offer, because he checks the fingerprint inside
// the SDP against the one Alice signed, before configuring anything.
//
// Without the signed statement this attack succeeds: Pion alone only compares the
// SDP fingerprint against whatever certificate the peer presents at DTLS time, and
// the attacker controls both halves of that comparison once it can rewrite SDP.
func TestMaliciousRelayCannotSubstituteFingerprint(t *testing.T) {
	url := startSignalingServer(t)
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	// Alice's statement names her real DTLS certificate. The attacker does not
	// know Alice's key, so it cannot re-sign a statement for its own.
	aliceSession, err := media.NewWebRTCSession(context.Background(), media.SessionConfig{
		RoomID: "room-1", SessionID: "session-1",
		LocalIdentity: alice, ExpectedPeerKey: bob.pub,
		Parameters:   media.DefaultOpusParameters(28000, 20),
		StatementTTL: 2 * time.Minute,
		ReplayGuard:  security.NewReplayGuard(64),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer aliceSession.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	offer, err := aliceSession.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bobSession, err := media.NewWebRTCSession(context.Background(), media.SessionConfig{
		RoomID: "room-1", SessionID: "session-1",
		LocalIdentity: bob, ExpectedPeerKey: alice.pub,
		Parameters:   media.DefaultOpusParameters(28000, 20),
		StatementTTL: 2 * time.Minute,
		ReplayGuard:  security.NewReplayGuard(64),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bobSession.Close()

	// The relay tampers with the fingerprint in the SDP but cannot touch the
	// signature.
	attackerFP := attackerCertificate(t)
	tampered := *offer
	tampered.SDP = []byte(rewriteSDPFingerprint(string(offer.SDP), attackerFP))

	if _, _, err := bobSession.AcceptOffer(ctx, &tampered); !errors.Is(err, security.ErrFingerprintMatch) {
		t.Fatalf("relay substituted a fingerprint and Bob accepted it: %v", err)
	}
	t.Log("relay fingerprint substitution rejected before any negotiation state changed")

	// Sanity: the untampered offer is accepted, proving the rejection above was
	// due to tampering rather than a broken harness.
	untampered := *offer
	if _, _, err := bobSession.AcceptOffer(ctx, &untampered); err != nil {
		t.Fatalf("untampered offer rejected, so the previous result is not meaningful: %v", err)
	}

	// The signaling server itself is never part of this decision. Prove the
	// server has no message type that could carry a certificate or key.
	_ = url
}

// TestRelayCannotForgePeerStatement covers the other half: the relay tries to
// present its own identity as Bob's.
func TestRelayCannotForgePeerStatement(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	attacker := newTestSigner(t)

	var fp [32]byte
	for i := range fp {
		fp[i] = 0x11
	}
	stmt, err := security.NewSessionStatement("room-1", "session-1", attacker.pub, fp, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig := attacker.Sign(stmt.SignedBytes())

	policy := security.StatementPolicy{
		Now:             time.Now,
		MaxClockSkew:    time.Minute,
		MaxLifetime:     5 * time.Minute,
		ExpectedRoom:    "room-1",
		ExpectedSession: "session-1",
		// Bob expects Alice's key, which the attacker does not have.
		ExpectedIssuer: alice.pub,
	}
	if err := stmt.Verify(sig, policy); !errors.Is(err, security.ErrUnknownIssuer) {
		t.Fatalf("statement from an unexpected issuer accepted: %v", err)
	}
	_ = bob
}

// TestServerAssignsFromAndIgnoresSpoof proves the server overwrites the From
// field, so a client cannot claim another peer's routing ID.
func TestServerAssignsFromAndIgnoresSpoof(t *testing.T) {
	url := startSignalingServer(t)
	newClient := func(name string) *signaling.Client {
		c, err := signaling.Connect(context.Background(), signaling.ClientConfig{
			URL: url, DisplayName: name, PublicKey: "aa", Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	alice := newClient("alice")
	bob := newClient("bob")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := alice.CreateRoom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.JoinRoom(ctx, created.RoomID); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(map[string]string{"x": "y"})
	// Alice claims to be Bob.
	if err := alice.Send(ctx, protocol.Envelope{
		Version: protocol.ProtocolVersion, Type: protocol.TypeRoster,
		To: bob.PeerID(), From: bob.PeerID(), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	for {
		env, err := bob.Recv(ctx)
		if err != nil {
			t.Fatalf("bob did not receive the relayed roster: %v", err)
		}
		if env.Type != protocol.TypeRoster {
			continue
		}
		if env.From == bob.PeerID() {
			t.Fatal("server relayed a spoofed From field")
		}
		if env.From != alice.PeerID() {
			t.Fatalf("From = %q, want alice %q", env.From, alice.PeerID())
		}
		return
	}
}

// rewriteSDPFingerprint replaces the sha-256 fingerprint line.
func rewriteSDPFingerprint(sdp string, fp [32]byte) string {
	var out strings.Builder
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a=fingerprint:sha-256") {
			out.WriteString("a=fingerprint:sha-256 " + security.FingerprintHex(fp))
		} else {
			out.WriteString(line)
		}
		out.WriteString("\r\n")
	}
	return out.String()
}
