package media

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/security"
)

// TestAnswererAdoptsInitiatorSessionID covers the WebRTC asymmetry: the offer
// defines the negotiation, so an answering session must adopt the initiator's ID
// even though it was constructed with a different one (or none).
func TestAnswererAdoptsInitiatorSessionID(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)

	// Alice's session already carries the ID it will use.
	aliceSession, err := NewWebRTCSession(context.Background(), SessionConfig{
		RoomID: "room-1", SessionID: "alice-offer-id",
		LocalIdentity: alice, ExpectedPeerKey: bob.pub,
		Parameters: params, StatementTTL: 2 * time.Minute, ReplayGuard: guard,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer aliceSession.Close()

	// Bob's answering session starts with a placeholder ID.
	bobSession, err := NewWebRTCSession(context.Background(), SessionConfig{
		RoomID: "room-1", SessionID: "placeholder",
		LocalIdentity: bob, ExpectedPeerKey: alice.pub,
		Parameters: params, StatementTTL: 2 * time.Minute, ReplayGuard: guard,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bobSession.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	offer, err := aliceSession.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if offer.SessionID != "alice-offer-id" {
		t.Fatalf("offer session id = %q", offer.SessionID)
	}

	answer, _, err := bobSession.AcceptOffer(ctx, offer)
	if err != nil {
		t.Fatalf("answerer did not adopt the initiator's session id: %v", err)
	}
	if answer.SessionID != "alice-offer-id" {
		t.Fatalf("answer carries session id %q, want the initiator's", answer.SessionID)
	}
	if bobSession.SessionID() != "alice-offer-id" {
		t.Fatalf("answerer session id = %q", bobSession.SessionID())
	}

	// The initiator's side must accept the answer.
	if _, err := aliceSession.AcceptAnswer(ctx, answer); err != nil {
		t.Fatalf("initiator rejected the answer: %v", err)
	}
}

// TestMalformedSessionIDRejected ensures arbitrary identifiers from the relay
// cannot corrupt logs or the wire format.
func TestMalformedSessionIDRejected(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"too long":  strings.Repeat("a", 129),
		"newline":   "abc\ndef",
		"space":     "abc def",
		"control":   "abc\x00def",
		"separator": "a/b",
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateSessionID(id); err == nil {
				t.Fatalf("accepted invalid session id %q", id)
			}
		})
	}
	// A conventional random hex identifier is accepted.
	if err := validateSessionID("a1b2c3d4e5f60718293a4b5c6d7e8f90"); err != nil {
		t.Fatalf("rejected a valid session id: %v", err)
	}
}

// TestSessionIDCannotChangeAfterNegotiation prevents a second offer from moving
// an established session onto a different identifier.
func TestSessionIDCannotChangeAfterNegotiation(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)

	aliceSession, _ := NewWebRTCSession(context.Background(), SessionConfig{
		RoomID: "room-1", SessionID: "first", LocalIdentity: alice, ExpectedPeerKey: bob.pub,
		Parameters: params, StatementTTL: 2 * time.Minute, ReplayGuard: guard,
	})
	defer aliceSession.Close()
	bobSession, _ := NewWebRTCSession(context.Background(), SessionConfig{
		RoomID: "room-1", SessionID: "placeholder", LocalIdentity: bob, ExpectedPeerKey: alice.pub,
		Parameters: params, StatementTTL: 2 * time.Minute, ReplayGuard: guard,
	})
	defer bobSession.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	offer, err := aliceSession.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bobSession.AcceptOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}

	// A second offer claiming a different ID must be refused.
	offer2 := *offer
	offer2.SessionID = "second"
	if _, _, err := bobSession.AcceptOffer(ctx, &offer2); !errors.Is(err, security.ErrRoomMismatch) {
		t.Fatalf("session id changed mid-negotiation: %v", err)
	}
}
