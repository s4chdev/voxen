package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/security"
)

// TestRejectedTamperDoesNotBurnNonce guards against a subtle denial of service.
//
// If the replay guard consumed a nonce before the SDP fingerprint was checked, a
// relay could rewrite the SDP of an honest offer, have it rejected, and then let
// the genuine offer through - which would now fail as a replay. The legitimate
// session would be permanently unusable on that path.
func TestRejectedTamperDoesNotBurnNonce(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)

	mk := func(self, peer *testSigner) *WebRTCSession {
		s, err := NewWebRTCSession(context.Background(), SessionConfig{
			RoomID: "room-1", SessionID: "session-1",
			LocalIdentity: self, ExpectedPeerKey: peer.pub,
			Parameters:   DefaultOpusParameters(28000, 20),
			StatementTTL: 2 * time.Minute,
			ReplayGuard:  guard,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	aliceSession := mk(alice, bob)
	bobSession := mk(bob, alice)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	offer, err := aliceSession.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A relay rewrites the SDP fingerprint.
	var wrong [32]byte
	for i := range wrong {
		wrong[i] = 0xAB
	}
	tampered := *offer
	tampered.SDP = []byte(rewriteTestFingerprint(string(offer.SDP), wrong))

	if _, _, err := bobSession.AcceptOffer(ctx, &tampered); !errors.Is(err, security.ErrFingerprintMatch) {
		t.Fatalf("tampered offer accepted: %v", err)
	}
	if got := guard.Size(); got != 0 {
		t.Fatalf("rejected negotiation recorded %d nonce(s); the honest retry will be treated as a replay", got)
	}

	// The genuine offer must still work.
	if _, _, err := bobSession.AcceptOffer(ctx, offer); err != nil {
		t.Fatalf("genuine offer rejected after a tampering attempt: %v", err)
	}
	if got := guard.Size(); got != 1 {
		t.Fatalf("accepted negotiation recorded %d nonces, want 1", got)
	}
}

// TestSecondUseOfSameStatementIsReplay confirms the guard still works for the
// case it exists to stop.
func TestSecondUseOfSameStatementIsReplay(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)

	mk := func(self, peer *testSigner) *WebRTCSession {
		s, err := NewWebRTCSession(context.Background(), SessionConfig{
			RoomID: "room-1", SessionID: "session-1",
			LocalIdentity: self, ExpectedPeerKey: peer.pub,
			Parameters: params, StatementTTL: 2 * time.Minute, ReplayGuard: guard,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	aliceSession := mk(alice, bob)
	first := mk(bob, alice)
	second := mk(bob, alice)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	offer, err := aliceSession.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AcceptOffer(ctx, offer); err != nil {
		t.Fatalf("first use rejected: %v", err)
	}
	if _, _, err := second.AcceptOffer(ctx, offer); !errors.Is(err, security.ErrNonceReused) {
		t.Fatalf("replay accepted: %v", err)
	}
}

func rewriteTestFingerprint(sdp string, fp [32]byte) string {
	out := ""
	for _, line := range splitLines(sdp) {
		if _, ok := cutPrefix(line, "a=fingerprint:"); ok {
			line = "a=fingerprint:sha-256 " + security.FingerprintHex(fp)
		}
		out += line + "\r\n"
	}
	return out
}
