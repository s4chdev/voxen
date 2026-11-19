package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/ipc"
)

func noColorVM() *ViewModel {
	return &ViewModel{NoColor: true}
}

func liveStatus(peers int) ipc.StatusResult {
	return ipc.StatusResult{
		PID:            4242,
		SignalingState: "connected",
		AudioState:     "miniaudio (pipewire/pulseaudio)",
		Room:           ipc.RoomState{InRoom: true, RoomID: "room-7", Role: "creator"},
		Peers:          peers,
		Audio:          ipc.AudioStatus{Running: true, FramesSent: 120, FramesReceived: 118},
	}
}

// TestRenderShowsCallStateWhenInCall checks the headline a user reads first.
func TestRenderShowsCallStateWhenInCall(t *testing.T) {
	out := noColorVM().Render(Snapshot{
		Status: liveStatus(2),
		Peers: []ipc.PeerState{
			{ID: "p1", DisplayName: "alice", Connection: "connected", Direct: true, RTTMillis: 12},
			{ID: "p2", DisplayName: "bob", Connection: "connected", Direct: true},
		},
		At: time.Unix(0, 0),
	})

	for _, want := range []string{"voxen", "in call", "2 peer", "room-7", "alice", "bob"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderExplainsNoCall checks the empty state names the next action rather than
// showing a blank screen.
func TestRenderExplainsNoCall(t *testing.T) {
	out := noColorVM().Render(Snapshot{
		Status: ipc.StatusResult{
			PID: 1, Room: ipc.RoomState{InRoom: false},
			SignalingState: "connected", AudioState: "miniaudio",
		},
		At: time.Unix(0, 0),
	})
	if !strings.Contains(out, "no call") {
		t.Fatalf("missing no-call state:\n%s", out)
	}
	for _, want := range []string{"voxen create", "voxen join"} {
		if !strings.Contains(out, want) {
			t.Fatalf("empty state does not suggest %q:\n%s", want, out)
		}
	}
}

// TestRenderWarnsWhenSignalingIsDown proves a call that cannot connect has a
// visible cause instead of looking merely empty.
func TestRenderWarnsWhenSignalingIsDown(t *testing.T) {
	out := noColorVM().Render(Snapshot{
		Status: ipc.StatusResult{Room: ipc.RoomState{InRoom: false}, SignalingState: "disconnected"},
		At:     time.Unix(0, 0),
	})
	if !strings.Contains(out, "disconnected") {
		t.Fatalf("signaling state not surfaced:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "signaling") {
		t.Fatalf("no explanation of the signaling problem:\n%s", out)
	}
}

// TestRenderMarksDaemonUnreachable proves a dead daemon is stated plainly rather
// than shown as an idle call.
func TestRenderMarksDaemonUnreachable(t *testing.T) {
	out := noColorVM().Render(Snapshot{Err: errTest, At: time.Unix(0, 0)})
	if !strings.Contains(out, "unreachable") {
		t.Fatalf("daemon failure not shown:\n%s", out)
	}
}

// TestSpeakingOnlyShownWhenConnected guards against an indicator lighting for a
// peer with no media path, which would be a claim the daemon cannot support.
func TestSpeakingOnlyShownWhenConnected(t *testing.T) {
	vm := noColorVM()
	connected := vm.Render(Snapshot{
		Status: liveStatus(1),
		Peers:  []ipc.PeerState{{ID: "p1", DisplayName: "a", Connection: "connected", Direct: true, Speaking: true}},
		At:     time.Unix(0, 0),
	})
	if !strings.Contains(connected, "speaking") {
		t.Fatalf("a speaking peer was not shown as speaking:\n%s", connected)
	}

	pending := vm.Render(Snapshot{
		Status: liveStatus(1),
		Peers:  []ipc.PeerState{{ID: "p1", DisplayName: "a", Connection: "connecting", Speaking: true}},
		At:     time.Unix(0, 0),
	})
	if strings.Contains(pending, "speaking") {
		t.Fatalf("an unconnected peer was shown as speaking:\n%s", pending)
	}
}

// TestPathIsNotClaimedBeforeConnection proves the PATH column does not guess.
func TestPathIsNotClaimedBeforeConnection(t *testing.T) {
	out := noColorVM().Render(Snapshot{
		Status: liveStatus(1),
		Peers:  []ipc.PeerState{{ID: "p1", DisplayName: "a", Connection: "connecting"}},
		At:     time.Unix(0, 0),
	})
	if strings.Contains(out, "direct") {
		t.Fatalf("path was claimed before a connection existed:\n%s", out)
	}
}

// TestFailedPeerShowsReason proves a broken connection is explained.
func TestFailedPeerShowsReason(t *testing.T) {
	out := noColorVM().Render(Snapshot{
		Status: liveStatus(1),
		Peers: []ipc.PeerState{{
			ID: "p1", DisplayName: "a", Connection: "connected",
			LastError: "no direct path to peer",
		}},
		At: time.Unix(0, 0),
	})
	if !strings.Contains(out, "failed") {
		t.Fatalf("a peer with an error was not shown as failed:\n%s", out)
	}
}

// TestAudioCountersAreShown proves the "is audio moving" question has an answer.
func TestAudioCountersAreShown(t *testing.T) {
	out := noColorVM().Render(Snapshot{Status: liveStatus(0), At: time.Unix(0, 0)})
	for _, want := range []string{"sent 120", "received 118"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audio counters missing %q:\n%s", want, out)
		}
	}
}

// TestMutedMicrophoneIsVisible checks the local mute state is never ambiguous.
func TestMutedMicrophoneIsVisible(t *testing.T) {
	st := liveStatus(0)
	st.Audio.Muted = true
	out := noColorVM().Render(Snapshot{Status: st, At: time.Unix(0, 0)})
	if !strings.Contains(out, "muted") {
		t.Fatalf("muted microphone not shown:\n%s", out)
	}
}

// TestHelpListsWorkingKeys proves the help text only advertises keys that exist.
func TestHelpListsWorkingKeys(t *testing.T) {
	vm := noColorVM()
	vm.ShowHelp = true
	out := vm.Render(Snapshot{Status: liveStatus(0), At: time.Unix(0, 0)})
	// The help is rendered by Bubbles from the key bindings, so it shows the
	// binding descriptions rather than raw key glyphs. Match the descriptions to
	// avoid coupling the test to Bubbles' layout choices.
	for _, want := range []string{"mute self", "mute peer", "select peer", "refresh", "help", "quit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help does not describe %q:\n%s", want, out)
		}
	}
}

// TestQuittingKeepsCallRunning documents the contract in the help text: "q" exits
// the view, not the call.
func TestQuitHelpDoesNotClaimToEndCall(t *testing.T) {
	vm := noColorVM()
	vm.ShowHelp = true
	out := vm.Render(Snapshot{Status: liveStatus(0), At: time.Unix(0, 0)})
	if strings.Contains(out, "end the call") {
		t.Fatalf("quit is described as ending the call, which it does not:\n%s", out)
	}
	if !strings.Contains(out, "call keeps running") {
		t.Fatalf("quit does not say the call survives:\n%s", out)
	}
}

// TestLongNamesDoNotBreakAlignment checks the table survives a name wider than its
// column header.
func TestLongNamesDoNotBreakAlignment(t *testing.T) {
	out := noColorVM().Render(Snapshot{
		Status: liveStatus(2),
		Peers: []ipc.PeerState{
			{ID: "p1", DisplayName: "a-very-long-display-name-indeed", Connection: "connected", Direct: true},
			{ID: "p2", DisplayName: "bob", Connection: "connected", Direct: true},
		},
		At: time.Unix(0, 0),
	})
	lines := strings.Split(out, "\n")
	var bobLine string
	for _, l := range lines {
		if strings.Contains(l, "bob") {
			bobLine = l
		}
	}
	if bobLine == "" {
		t.Fatalf("row for bob missing:\n%s", out)
	}
	// The STATE column must still line up: "connected" appears in both rows at the
	// same offset.
	var offsets []int
	for _, l := range lines {
		if idx := strings.Index(l, "connected"); idx >= 0 {
			offsets = append(offsets, idx)
		}
	}
	if len(offsets) == 2 && offsets[0] != offsets[1] {
		t.Fatalf("STATE column not aligned: %v\n%s", offsets, out)
	}
}

var errTest = testError("daemon unreachable")

type testError string

func (e testError) Error() string { return string(e) }
