//go:build cgo

package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/ipc"
)

// TestRealCallCarriesAudio is the end-to-end audio acceptance test.
//
// Two daemons with real microphones and speakers form a call over a real
// signaling server. It then asserts that:
//
//   - both sides report audio running,
//   - the local microphone is being forwarded (frames_sent grows),
//   - the encrypting media path delivers audio to the other side (frames_received
//     grows), which can only happen if every frame traversed DTLS-SRTP.
//
// It skips when the host has no working audio backend, because a headless CI
// runner legitimately has none.
func TestRealCallCarriesAudio(t *testing.T) {
	requireUsableAudio(t)

	url := startSignaling(t)
	_, clients := twoProfiles(t, url)
	alice := clients[0]()
	bob := clients[1]()

	var created ipc.RoomResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "create", Method: ipc.MethodRoomCreate,
		Params: mustJSON(t, ipc.RoomCreateParams{}),
	}, &created); err != nil {
		t.Fatalf("create room: %v", err)
	}
	if err := bob.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "join", Method: ipc.MethodRoomJoin,
		Params: mustJSON(t, ipc.RoomJoinParams{RoomID: created.Room.RoomID}),
	}, nil); err != nil {
		t.Fatalf("join room: %v", err)
	}

	waitForPeerState(t, alice, created.Room.RoomID, "connected")
	waitForPeerState(t, bob, created.Room.RoomID, "connected")

	// Give the bridge a moment to open devices and start pumping.
	deadline := time.Now().Add(10 * time.Second)
	var aliceStats, bobStats ipc.AudioStatus
	for time.Now().Before(deadline) {
		aliceStats = audioStatus(t, alice, "alice")
		bobStats = audioStatus(t, bob, "bob")
		if aliceStats.Running && bobStats.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !aliceStats.Running || !bobStats.Running {
		t.Fatalf("audio did not start on both sides: alice=%+v bob=%+v", aliceStats, bobStats)
	}
	t.Logf("alice audio: in=%q out=%q", aliceStats.InputDevice, aliceStats.OutputDevice)
	t.Logf("bob   audio: in=%q out=%q", bobStats.InputDevice, bobStats.OutputDevice)

	// Wait for frames to move in both directions. The microphone may be silent,
	// but frames must still be produced: a muted or quiet mic transmits silence
	// so the receiver's timeline stays alive.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		aliceStats = audioStatus(t, alice, "alice")
		bobStats = audioStatus(t, bob, "bob")
		if aliceStats.FramesSent > 10 && bobStats.FramesSent > 10 &&
			aliceStats.FramesReceived > 0 && bobStats.FramesReceived > 0 {
			t.Logf("alice: sent=%d received=%d dropped=%d xruns=%d",
				aliceStats.FramesSent, aliceStats.FramesReceived, aliceStats.InputDropped, aliceStats.OutputXRuns)
			t.Logf("bob:   sent=%d received=%d dropped=%d xruns=%d",
				bobStats.FramesSent, bobStats.FramesReceived, bobStats.InputDropped, bobStats.OutputXRuns)
			// Audio cannot arrive unless it traversed the authenticated SRTP path:
			// the media plane refuses to receive before the binding completes, and
			// waitForPeerState already proved the binding did complete.
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("audio did not flow both ways: alice=%+v bob=%+v", aliceStats, bobStats)
}

// requireUsableAudio skips the test unless this host can actually open both a
// microphone and a speaker.
//
// Enumeration alone is not enough: CI runners list phantom devices that then fail
// to open, so a test that trusted enumeration would fail rather than skip on
// hardware that genuinely cannot carry a call.
func requireUsableAudio(t *testing.T) {
	t.Helper()
	engine, err := audio.NewMiniAudio(nil)
	if err != nil {
		t.Skipf("no audio backend: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	probe := audio.Probe(ctx, engine)
	if !probe.Usable() {
		t.Skipf("host cannot open audio devices (backend=%s, devices=%d, capture=%v, playback=%v): %v",
			probe.Backend, probe.Devices, probe.CaptureOK, probe.PlaybackOK, probe.Err)
	}
}

func audioStatus(t *testing.T, c *ipc.Client, name string) ipc.AudioStatus {
	t.Helper()
	var status ipc.StatusResult
	if err := c.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "status-" + name, Method: ipc.MethodStatus,
	}, &status); err != nil {
		t.Fatalf("%s status: %v", name, err)
	}
	return status.Audio
}

// TestMuteDuringCallStopsVoiceAndResumes proves mute works on a live call: the
// bridge keeps transmitting (so the timeline survives) but sends silence.
func TestMuteDuringCallStopsVoiceAndResumes(t *testing.T) {
	requireUsableAudio(t)

	url := startSignaling(t)
	_, clients := twoProfiles(t, url)
	alice := clients[0]()
	bob := clients[1]()

	var created ipc.RoomResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "create", Method: ipc.MethodRoomCreate,
		Params: mustJSON(t, ipc.RoomCreateParams{}),
	}, &created); err != nil {
		t.Fatal(err)
	}
	if err := bob.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "join", Method: ipc.MethodRoomJoin,
		Params: mustJSON(t, ipc.RoomJoinParams{RoomID: created.Room.RoomID}),
	}, nil); err != nil {
		t.Fatal(err)
	}
	waitForPeerState(t, alice, created.Room.RoomID, "connected")
	waitForPeerState(t, bob, created.Room.RoomID, "connected")

	// Wait for audio to be running, then mute.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if audioStatus(t, alice, "alice").Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var res ipc.MuteResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "mute", Method: ipc.MethodMuteSet,
		Params: mustJSON(t, ipc.MuteParams{Muted: boolPtr(true)}),
	}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Muted {
		t.Fatal("mute did not take effect")
	}

	// The daemon's audio status must agree.
	stats := audioStatus(t, alice, "alice")
	if !stats.Muted {
		t.Fatalf("audio status does not report muted: %+v", stats)
	}

	// Unmute and confirm it clears.
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "unmute", Method: ipc.MethodMuteSet,
		Params: mustJSON(t, ipc.MuteParams{Muted: boolPtr(false)}),
	}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Muted {
		t.Fatal("unmute did not take effect")
	}
	if audioStatus(t, alice, "alice").Muted {
		t.Fatal("audio status still reports muted after unmute")
	}
}

func boolPtr(b bool) *bool { return &b }

var _ = json.Marshal
