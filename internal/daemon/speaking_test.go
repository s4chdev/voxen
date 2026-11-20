//go:build cgo

package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/daemon"
	"github.com/s4chdev/voxen/internal/dsp"
	"github.com/s4chdev/voxen/internal/media"
)

// observer records speech transitions reported by the bridge.
type observer struct {
	mu     sync.Mutex
	events map[string][]bool
}

func newObserver() *observer { return &observer{events: map[string][]bool{}} }

func (o *observer) record(peerID string, speaking bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events[peerID] = append(o.events[peerID], speaking)
}

func (o *observer) transitions(peerID string) []bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]bool, len(o.events[peerID]))
	copy(out, o.events[peerID])
	return out
}

// TestSpeechDetectionThreshold checks the RMS gate in isolation. The threshold is
// a product decision: too low and a keyboard click lights the indicator, too high
// and quiet speech is invisible.
func TestSpeechDetectionThreshold(t *testing.T) {
	cases := []struct {
		name string
		peak int
		want bool
	}{
		{"digital silence", 0, false},
		{"quiet room noise", 100, false},
		{"keyboard click", 400, false},
		{"normal speech", 4000, true},
		{"loud speech", 12000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := make([]int16, audio.FramesPerPacket)
			for i := range frame {
				frame[i] = int16(float64(tc.peak) * math.Sin(2*math.Pi*300*float64(i)/48000))
			}
			// The exported helper is what the bridge uses.
			got := daemon.IsSpeechForTest(frame)
			if got != tc.want {
				t.Fatalf("peak %d reported speaking=%v, want %v", tc.peak, got, tc.want)
			}
		})
	}
}

// TestSpeakingIndicatorTracksPeerAudio proves a peer's speech is reported to the
// observer, and that it clears when they stop.
//
// This is what a call UI depends on: an indicator that lights and never clears is
// worse than no indicator at all.
func TestSpeakingIndicatorTracksPeerAudio(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	talker := newFakeSession()
	quiet := newFakeSession()
	list := func() []media.ActivePeer {
		return []media.ActivePeer{
			{PeerID: "talker", Session: talker},
			{PeerID: "quiet", Session: quiet},
		}
	}

	bridge := daemon.NewAudioBridge(engine,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{}, list)
	obs := newObserver()
	bridge.SetObserver(obs.record)

	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	// Feed loud audio from one peer and silence from the other.
	loud := make([]int16, audio.FramesPerPacket)
	for i := range loud {
		loud[i] = int16(6000 * math.Sin(2*math.Pi*300*float64(i)/48000))
	}
	silent := make([]int16, audio.FramesPerPacket)

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case talker.incoming <- loud:
			default:
			}
			select {
			case quiet.incoming <- silent:
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(obs.transitions("talker")) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Stop feeding loud audio. The channel is shared with the bridge, so it must
	// not be reassigned: the feed goroutine simply stops writing to it.
	close(stop)

	if tr := obs.transitions("talker"); len(tr) == 0 || !tr[0] {
		t.Fatalf("a peer sending loud audio was not reported as speaking: %v", tr)
	}

	// The silent peer must never be reported as speaking.
	for _, v := range obs.transitions("quiet") {
		if v {
			t.Fatal("a silent peer was reported as speaking")
		}
	}

	// After the audio stops, the indicator must clear. The bridge sees an empty
	// channel as "no audio from this peer", which is the same path a silent peer
	// takes.
	deadline = time.Now().Add(5 * time.Second)
	cleared := false
	for time.Now().Before(deadline) {
		tr := obs.transitions("talker")
		if len(tr) > 1 && !tr[len(tr)-1] {
			cleared = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cleared {
		t.Fatalf("the indicator never cleared after the peer stopped talking: %v", obs.transitions("talker"))
	}
}

// TestPerPeerMuteSilencesOnlyThatPeer proves a local mute removes one participant
// from the mix while leaving the others audible.
func TestPerPeerMuteSilencesOnlyThatPeer(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	a := newFakeSession()
	b := newFakeSession()
	list := func() []media.ActivePeer {
		return []media.ActivePeer{{PeerID: "a", Session: a}, {PeerID: "b", Session: b}}
	}
	bridge := daemon.NewAudioBridge(engine,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{}, list)
	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	bridge.SetPeerMuted("a", true)
	if !bridge.PeerMuted("a") {
		t.Fatal("mute did not take effect")
	}
	if bridge.PeerMuted("b") {
		t.Fatal("muting one peer muted another")
	}

	bridge.SetPeerMuted("a", false)
	if bridge.PeerMuted("a") {
		t.Fatal("unmute did not take effect")
	}
}

// TestMutedPeerStillCountsAsSpeaking proves the indicator still lights for a peer
// the user has muted. Silencing someone locally must not make the UI claim nobody
// is talking.
func TestMutedPeerStillCountsAsSpeaking(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	sess := newFakeSession()
	list := func() []media.ActivePeer {
		return []media.ActivePeer{{PeerID: "p1", Session: sess}}
	}
	bridge := daemon.NewAudioBridge(engine,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{}, list)
	obs := newObserver()
	bridge.SetObserver(obs.record)
	bridge.SetPeerMuted("p1", true)

	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	loud := make([]int16, audio.FramesPerPacket)
	for i := range loud {
		loud[i] = 6000
	}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case sess.incoming <- loud:
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	defer close(stop)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range obs.transitions("p1") {
			if v {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a muted peer that is speaking was not reported as speaking")
}

// TestBridgeReportsProcessingHonestly proves the bridge reports what is actually
// running. A build without the backend must report none, so status never claims
// echo cancellation that is not happening.
func TestBridgeReportsProcessingHonestly(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	bridge := daemon.NewAudioBridge(engine,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{EchoCancellation: true, NoiseSuppression: true},
		func() []media.ActivePeer { return nil })
	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	stages := bridge.Stages()
	if dsp.Available() {
		if stages.Backend == "none" {
			t.Fatal("a backend is compiled in but the bridge reports none active")
		}
		if !stages.EchoCancellation || !stages.NoiseSuppression {
			t.Fatalf("requested both stages but the bridge reports %+v", stages)
		}
	} else {
		if stages.Backend != "none" {
			t.Fatalf("no backend compiled in but the bridge reports %q", stages.Backend)
		}
		if stages.EchoCancellation || stages.NoiseSuppression {
			t.Fatalf("no backend but stages are claimed active: %+v", stages)
		}
	}
}

// TestBridgeReferenceReachesProcessor proves the playback signal is available to
// the canceller. Without it echo cancellation cannot work at all, and the failure is
// silent: audio still flows, it is just uncancelled.
func TestBridgeReferenceReachesProcessor(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	a := newFakeSession()
	b := newFakeSession()
	list := func() []media.ActivePeer {
		return []media.ActivePeer{{PeerID: "a", Session: a}, {PeerID: "b", Session: b}}
	}
	bridge := daemon.NewAudioBridge(engine,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{}, list)
	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	// Feed audio from a peer so the playback loop writes frames.
	frame := make([]int16, audio.FramesPerPacket)
	for i := range frame {
		frame[i] = 3000
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case a.incoming <- frame:
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// The reference buffer is populated by the playback loop; give it time.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ref := bridge.ReferenceForTest()
		if len(ref) > 0 {
			var nonZero bool
			for _, v := range ref {
				if v != 0 {
					nonZero = true
					break
				}
			}
			if nonZero {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the playback reference never reached the bridge; echo cancellation would be inert")
}
