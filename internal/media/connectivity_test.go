package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/security"
)

// unreachableSession builds a session whose peer never answers: the SDP is
// accepted, DTLS is never completed because the remote candidates go nowhere.
//
// This is the shape of a real connectivity failure, and the test asserts that it
// is reported as a direct-path problem rather than as a generic timeout.
func TestNoDirectPathIsReportedSpecifically(t *testing.T) {
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)

	sess, err := NewWebRTCSession(context.Background(), SessionConfig{
		RoomID: "room-1", SessionID: "no-path-1",
		LocalIdentity: alice, ExpectedPeerKey: bob.pub,
		Parameters: params, StatementTTL: 2 * time.Minute,
		ReplayGuard: guard,
		// A short timeout so the test is fast; the production default is 15s.
		ConnectTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create an offer so the connection is in the negotiating state, but never
	// deliver it anywhere: no answer, no candidates, no DTLS.
	if _, err := sess.CreateOffer(ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = sess.Authenticate(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("authenticate succeeded with no peer present")
	}
	if !errors.Is(err, ErrNoDirectRoute) {
		t.Fatalf("failure was reported as %v, want ErrNoDirectRoute", err)
	}
	// The configured timeout must be honoured, not a hardcoded one. Allow slack
	// for scheduling but fail if it clearly used the 15s default or the old 30s.
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s; the configured ConnectTimeout was not applied", elapsed)
	}
	t.Logf("reported %v after %s", err, elapsed)
}

// TestConnectTimeoutDefaults guards the zero-value behaviour: the media plane and
// the configuration must agree on the default.
func TestConnectTimeoutDefaultMatchesConfig(t *testing.T) {
	if DefaultConnectTimeout != 15*time.Second {
		t.Fatalf("DefaultConnectTimeout = %s, but network.ice_timeout_ms defaults to 15000ms", DefaultConnectTimeout)
	}
}

// TestFailedConnectionReasonMentionsRelay proves the message explains *why* the
// call cannot be rescued, rather than just saying "failed".
func TestFailedConnectionReasonMentionsRelay(t *testing.T) {
	err := fmtNoDirectPath(750 * time.Millisecond)
	if !errors.Is(err, ErrNoDirectRoute) {
		t.Fatalf("wrapped error does not match ErrNoDirectRoute: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"no direct peer-to-peer path", "does not relay", "750ms"} {
		if !contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
