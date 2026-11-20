package media

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/security"
)

// hasInterface reports whether a named network interface exists, so overlay tests
// skip cleanly on machines without one.
func hasInterface(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// gatherWith builds a session with the given filter and returns its candidates.
func gatherWith(t *testing.T, filter []string) []string {
	t.Helper()
	alice := newTestSigner(t)
	bob := newTestSigner(t)
	sess, err := NewWebRTCSession(context.Background(), SessionConfig{
		RoomID: "r", SessionID: "s", LocalIdentity: alice, ExpectedPeerKey: bob.pub,
		Parameters:      DefaultOpusParameters(28000, 20),
		StatementTTL:    time.Minute,
		ReplayGuard:     security.NewReplayGuard(8),
		InterfaceFilter: filter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	// The handler runs on Pion's ICE goroutine, so the slice needs its own lock: a
	// bare append here races the read below.
	var (
		mu  sync.Mutex
		out []string
	)
	sess.SetCandidateHandler(func(c ICECandidatePayload) {
		mu.Lock()
		out = append(out, c.Candidate)
		mu.Unlock()
	})
	if _, err := sess.CreateOffer(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Wait for gathering to settle rather than stopping at a count: comparing two
	// sessions is only meaningful if both were given the same time to gather.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), out...)
}

// TestInterfaceFilterRestrictsCandidates proves grading honours network.interface_filter.
//
// Without it, a private overlay would still advertise LAN and public addresses: ICE
// would attempt paths that cannot work, taking longer to settle, and the peer would
// see local addresses it has no business knowing.
func TestInterfaceFilterRestrictsCandidates(t *testing.T) {
	const overlay = "tailscale0"
	if !hasInterface(overlay) {
		t.Skipf("%s is not present on this host", overlay)
	}

	all := gatherWith(t, nil)
	if len(all) == 0 {
		t.Skip("no candidates gathered on this host")
	}

	filtered := gatherWith(t, []string{overlay})
	if len(filtered) == 0 {
		t.Fatalf("filtering to %s produced no candidates at all", overlay)
	}

	t.Logf("unfiltered: %d candidates, filtered: %d", len(all), len(filtered))
	if len(filtered) >= len(all) {
		t.Fatalf("the filter did not reduce candidates: %d → %d", len(all), len(filtered))
	}

	// Every surviving candidate must belong to the overlay, not to a LAN or container
	// interface. Checking the address is the only portable way to assert this.
	overlayAddrs := interfaceAddresses(t, overlay)
	for _, c := range filtered {
		var matched bool
		for _, addr := range overlayAddrs {
			if strings.Contains(c, addr) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("candidate %q is not on %s", c, overlay)
		}
	}
}

func interfaceAddresses(t *testing.T, name string) []string {
	t.Helper()
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			out = append(out, ipnet.IP.String())
		}
	}
	return out
}

// TestFilteredSessionStillConnects proves filtering does not break negotiation: a
// session restricted to an interface can still complete a call between two local
// peers on that interface.
func TestFilteredSessionStillConnects(t *testing.T) {
	const overlay = "tailscale0"
	if !hasInterface(overlay) {
		t.Skipf("%s is not present on this host", overlay)
	}

	alice := newTestSigner(t)
	bob := newTestSigner(t)
	guard := security.NewReplayGuard(64)
	params := DefaultOpusParameters(28000, 20)
	const sessionID = "filtered-session"

	mk := func(self, peer *testSigner) *WebRTCSession {
		s, err := NewWebRTCSession(context.Background(), SessionConfig{
			RoomID: "room-1", SessionID: sessionID,
			LocalIdentity: self, ExpectedPeerKey: peer.pub,
			Parameters: params, StatementTTL: 2 * time.Minute,
			ReplayGuard:     guard,
			InterfaceFilter: []string{overlay},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a := mk(alice, bob)
	b := mk(bob, alice)

	infoA, infoB := connect(t, a, b)
	if !infoA.RemoteCertVerified || !infoB.RemoteCertVerified {
		t.Fatal("filtered session completed without verifying identities")
	}
	stats := a.Stats()
	t.Logf("connected over %s; local=%s remote=%s", overlay, stats.LocalCandidate, stats.RemoteCandidate)
	if strings.Contains(stats.LocalCandidate, "192.168.") || strings.Contains(stats.LocalCandidate, "172.17.") {
		t.Fatalf("a filtered session used a non-overlay path: %s", stats.LocalCandidate)
	}
}

// TestEmptyFilterKeepsEverything proves the zero value means "no restriction", so an
// existing configuration does not silently change meaning.
//
// It compares host coverage rather than a candidate count: a machine may legitimately
// have one address (a CI runner, or a host with only a loopback-routable interface),
// and asserting "at least two" would fail there for no product reason.
func TestEmptyFilterKeepsEverything(t *testing.T) {
	all := gatherWith(t, nil)
	empty := gatherWith(t, []string{})
	if len(all) == 0 {
		t.Skip("no candidates gathered on this host")
	}
	if len(empty) == 0 {
		t.Fatalf("an empty filter gathered nothing; it must not restrict")
	}
	// An explicit empty slice and a nil slice must behave identically. Compare the
	// set of addresses rather than counts, since ports differ per session.
	if hostsOf(empty) != hostsOf(all) {
		t.Fatalf("empty filter changed host coverage: nil gave %v, empty gave %v",
			hostsOf(all), hostsOf(empty))
	}
}

// hostsOf extracts the address from each candidate line, so two sessions on the same
// machine compare equal despite different ports.
func hostsOf(cands []string) string {
	var hosts []string
	for _, c := range cands {
		fields := strings.Fields(c)
		// candidate:<foundation> <component> <proto> <priority> <addr> <port> ...
		if len(fields) >= 6 {
			hosts = append(hosts, fields[4])
		}
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ",")
}
