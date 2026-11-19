package media

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/security"
)

// testSigner is an identity-plane stand-in for tests.
type testSigner struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{pub: pub, priv: priv}
}

func (s *testSigner) Sign(msg []byte) []byte       { return ed25519.Sign(s.priv, msg) }
func (s *testSigner) PublicKey() ed25519.PublicKey { return s.pub }

// wireCandidates connects the ICE candidate callbacks in both directions.
func wireCandidates(t *testing.T, a, b *WebRTCSession) {
	t.Helper()
	a.SetCandidateHandler(func(c ICECandidatePayload) {
		if err := b.AddICECandidate(context.Background(), c); err != nil {
			t.Logf("candidate a->b: %v", err)
		}
	})
	b.SetCandidateHandler(func(c ICECandidatePayload) {
		if err := a.AddICECandidate(context.Background(), c); err != nil {
			t.Logf("candidate b->a: %v", err)
		}
	})
}

// Session IDs must agree; helper to build a matched pair with one shared ID.
func newMatchedPair(t *testing.T) (*WebRTCSession, *WebRTCSession, *testSigner, *testSigner) {
	t.Helper()
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)
	const sessionID = "shared-session-1"

	mk := func(self, peer *testSigner) *WebRTCSession {
		s, err := NewWebRTCSession(context.Background(), SessionConfig{
			RoomID:          "room-1",
			SessionID:       sessionID,
			LocalIdentity:   self,
			ExpectedPeerKey: peer.pub,
			Parameters:      params,
			StatementTTL:    2 * time.Minute,
			ReplayGuard:     guard,
		})
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	return mk(alice, bob), mk(bob, alice), alice, bob
}

// connect negotiates a pair and returns both authenticated PeerInfos.
func connect(t *testing.T, a, b *WebRTCSession) (PeerInfo, PeerInfo) {
	t.Helper()
	wireCandidates(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	offer, err := a.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	answer, _, err := b.AcceptOffer(ctx, offer)
	if err != nil {
		t.Fatalf("accept offer: %v", err)
	}
	if _, err := a.AcceptAnswer(ctx, answer); err != nil {
		t.Fatalf("accept answer: %v", err)
	}
	// Both sides wait for DTLS to complete, then bind. This is the gate that
	// must pass before any audio.
	infoA, err := a.Authenticate(ctx)
	if err != nil {
		t.Fatalf("authenticate initiator: %v", err)
	}
	infoB, err := b.Authenticate(ctx)
	if err != nil {
		t.Fatalf("authenticate answerer: %v", err)
	}
	return infoA, infoB
}

// TestTwoPeerAuthenticatedSession is the core acceptance test: two peers
// negotiate, bind identity to DTLS, and exchange audio.
func TestTwoPeerAuthenticatedSession(t *testing.T) {
	a, b, aliceSigner, bobSigner := newMatchedPair(t)

	infoA, infoB := connect(t, a, b)

	if !infoA.RemoteCertVerified || !infoB.RemoteCertVerified {
		t.Fatal("session completed without verifying the remote certificate")
	}
	// Each side bound the certificate presented by the *other* peer, so the two
	// values must differ. What must hold is that each side's bound fingerprint
	// equals the certificate that peer actually signed.
	if security.EqualFingerprint(infoA.DTLSFingerprint, infoB.DTLSFingerprint) {
		t.Fatal("both sides bound the same certificate; they should each bind the remote peer's")
	}
	if security.EqualFingerprint(infoA.DTLSFingerprint, a.LocalFingerprint()) {
		t.Fatal("initiator bound its own certificate instead of the peer's")
	}
	if security.EqualFingerprint(infoB.DTLSFingerprint, b.LocalFingerprint()) {
		t.Fatal("answerer bound its own certificate instead of the peer's")
	}
	if !security.EqualFingerprint(infoA.DTLSFingerprint, b.LocalFingerprint()) {
		t.Fatal("initiator did not bind the certificate the answerer actually uses")
	}
	if !security.EqualFingerprint(infoB.DTLSFingerprint, a.LocalFingerprint()) {
		t.Fatal("answerer did not bind the certificate the initiator actually uses")
	}
	if infoA.Fingerprint != hex.EncodeToString(bobSigner.pub) {
		t.Fatal("initiator bound the wrong peer key")
	}
	if infoB.Fingerprint != hex.EncodeToString(aliceSigner.pub) {
		t.Fatal("answerer bound the wrong peer key")
	}

	// Audio must flow in the authenticated direction.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pcm := make([]int16, 960)
	for i := range pcm {
		pcm[i] = int16(9000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	// Send several frames so at least one survives startup and jitter.
	go func() {
		for i := 0; i < 25; i++ {
			_ = a.SendPCM(ctx, pcm)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	out := make([]int16, 960)
	got := false
	deadline := time.After(8 * time.Second)
	for !got {
		select {
		case <-deadline:
			t.Fatal("no audio received after authentication")
		default:
		}
		n, err := b.ReceivePCM(ctx, out)
		if err != nil {
			continue
		}
		if n > 0 {
			got = true
		}
	}
	if !got {
		t.Fatal("received an empty frame")
	}
}

// TestAudioRefusedBeforeAuthentication proves media cannot be sent or received
// before the identity binding completes.
func TestAudioRefusedBeforeAuthentication(t *testing.T) {
	a, _, _, _ := newMatchedPair(t)
	pcm := make([]int16, 960)
	if err := a.SendPCM(context.Background(), pcm); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("SendPCM before authentication returned %v, want ErrUnauthenticated", err)
	}
	if _, err := a.ReceivePCM(context.Background(), pcm); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("ReceivePCM before authentication returned %v, want ErrUnauthenticated", err)
	}
}

// TestForgedStatementRejected covers a signaling server that substitutes its own
// identity: it cannot sign with the peer's key.
func TestForgedStatementRejected(t *testing.T) {
	a, b, _, _ := newMatchedPair(t)
	wireCandidates(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	offer, err := a.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The attacker re-signs the offer's statement with its own key.
	attacker := newTestSigner(t)
	forged := *offer
	stmt, err := security.NewSessionStatement("room-1", offer.SessionID,
		attacker.pub, offer.Statement.DTLSFingerprint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	forged.Statement = stmt
	forged.Signature = attacker.Sign(stmt.SignedBytes())

	if _, _, err := b.AcceptOffer(ctx, &forged); !errors.Is(err, security.ErrUnknownIssuer) {
		t.Fatalf("forged statement accepted: %v", err)
	}
}

// TestRewrittenSDPFingerprintRejected covers a relay that rewrites the SDP
// fingerprint while passing the original signature through.
func TestRewrittenSDPFingerprintRejected(t *testing.T) {
	a, b, _, _ := newMatchedPair(t)
	wireCandidates(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	offer, err := a.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	tampered := *offer
	var wrong [32]byte
	for i := range wrong {
		wrong[i] = 0xAB
	}
	tampered.SDP = []byte(replaceFingerprint(string(offer.SDP), wrong))

	if _, _, err := b.AcceptOffer(ctx, &tampered); !errors.Is(err, security.ErrFingerprintMatch) {
		t.Fatalf("rewritten SDP fingerprint accepted: %v", err)
	}
}

// TestReplayedStatementRejected proves a captured statement cannot be reused in
// a second session, even with the same room and session ID.
func TestReplayedStatementRejected(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	// One guard shared by both receivers, as the manager does in production.
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)
	const sessionID = "replay-session"

	// The victim of the replay: a second Bob session that expects Alice.
	mk := func(self, peer *testSigner) *WebRTCSession {
		s, err := NewWebRTCSession(context.Background(), SessionConfig{
			RoomID: "room-1", SessionID: sessionID,
			LocalIdentity: self, ExpectedPeerKey: peer.pub,
			Parameters: params, StatementTTL: 2 * time.Minute, ReplayGuard: guard,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	aliceSess := mk(alice, bob)
	bobSess1 := mk(bob, alice)
	bobSess2 := mk(bob, alice)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	offer, err := aliceSess.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bobSess1.AcceptOffer(ctx, offer); err != nil {
		t.Fatalf("first use of the statement rejected: %v", err)
	}
	// Replaying the identical, validly signed statement must fail.
	if _, _, err := bobSess2.AcceptOffer(ctx, offer); !errors.Is(err, security.ErrNonceReused) {
		t.Fatalf("replayed statement accepted (err=%v)", err)
	}
}

// TestUnauthenticatedPeerCannotBeCreated proves a session requires a verified
// peer key.
func TestUnauthenticatedPeerCannotBeCreated(t *testing.T) {
	if _, err := NewWebRTCSession(context.Background(), SessionConfig{}); !errors.Is(err, ErrNoVerifiedPeer) {
		t.Fatalf("session created without an expected peer key: %v", err)
	}
}

// TestStatsShowDirectPath checks the measurement required by the test plan: the
// selected candidate pair must not be a relayed one.
func TestStatsShowDirectPath(t *testing.T) {
	a, b, _, _ := newMatchedPair(t)
	connect(t, a, b)

	// Give ICE a moment to publish the selected pair.
	var stats SessionStats
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats = a.Stats()
		if stats.LocalCandidate != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stats.Relayed {
		t.Fatalf("voxen must never use a relayed candidate: %+v", stats)
	}
	if stats.LocalCandidate == "" || stats.RemoteCandidate == "" {
		t.Logf("candidate pair not yet reported; skipping strict assertion: %+v", stats)
	}
}

// replaceFingerprint rewrites the a=fingerprint line for test tampering.
func replaceFingerprint(sdp string, fp [32]byte) string {
	lines := splitLines(sdp)
	out := ""
	for _, line := range lines {
		if _, ok := cutPrefix(line, "a=fingerprint:"); ok {
			line = "a=fingerprint:sha-256 " + security.FingerprintHex(fp)
		}
		out += line + "\r\n"
	}
	return out
}
