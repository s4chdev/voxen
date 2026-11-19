//go:build cgo

package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/daemon"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/media"
)

// fakeSession is a media.Session that records what the bridge sends and can feed
// synthetic audio back. Using it keeps the bridge test deterministic and
// independent of a real PeerConnection.
type fakeSession struct {
	sent     chan []int16
	incoming chan []int16
	auth     bool
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		sent:     make(chan []int16, 64),
		incoming: make(chan []int16, 64),
		auth:     true,
	}
}

func (f *fakeSession) SendPCM(ctx context.Context, pcm []int16) error {
	if !f.auth {
		return media.ErrUnauthenticated
	}
	cp := make([]int16, len(pcm))
	copy(cp, pcm)
	select {
	case f.sent <- cp:
	default:
	}
	return nil
}

func (f *fakeSession) ReceivePCM(ctx context.Context, pcm []int16) (int, error) {
	select {
	case frame := <-f.incoming:
		return copy(pcm, frame), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Tests only need these; the rest of the interface is unused.

// usableEngine returns an engine only if this host can actually open devices.
func usableEngine(t *testing.T) *audio.MiniAudio {
	t.Helper()
	engine, err := audio.NewMiniAudio(nil)
	if err != nil {
		t.Skipf("no audio backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if probe := audio.Probe(ctx, engine); !probe.Usable() {
		_ = engine.Close()
		t.Skipf("host cannot open audio devices: %v", probe.Err)
	}
	return engine
}

func (f *fakeSession) CreateOffer(context.Context) (*media.Negotiation, error) {
	return nil, nil
}
func (f *fakeSession) AcceptAnswer(context.Context, *media.Negotiation) (media.PeerInfo, error) {
	return media.PeerInfo{}, nil
}
func (f *fakeSession) AcceptOffer(context.Context, *media.Negotiation) (*media.Negotiation, media.PeerInfo, error) {
	return nil, media.PeerInfo{}, nil
}
func (f *fakeSession) AddICECandidate(context.Context, media.ICECandidatePayload) error { return nil }
func (f *fakeSession) SetCandidateHandler(func(media.ICECandidatePayload))              {}
func (f *fakeSession) Authenticate(context.Context) (media.PeerInfo, error) {
	return media.PeerInfo{RemoteCertVerified: true}, nil
}
func (f *fakeSession) RestartICE(context.Context) (*media.Negotiation, error) { return nil, nil }
func (f *fakeSession) Stats() media.SessionStats                              { return media.SessionStats{} }
func (f *fakeSession) Close() error                                           { return nil }

// TestAudioBridgeForwardsMicrophoneToPeers proves the capture path: what arrives
// from the microphone is delivered to every authenticated peer.
func TestAudioBridgeForwardsMicrophoneToPeers(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	sessions := []*fakeSession{newFakeSession(), newFakeSession()}
	list := func() []media.ActivePeer {
		return []media.ActivePeer{
			{PeerID: "p1", Session: sessions[0]},
			{PeerID: "p2", Session: sessions[1]},
		}
	}

	bridge := daemon.NewAudioBridge(engine, slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{}, list)
	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	// Wait for at least one frame to reach both peers.
	deadline := time.After(5 * time.Second)
	for _, s := range sessions {
		select {
		case <-s.sent:
		case <-deadline:
			t.Fatal("microphone audio was not forwarded to a peer")
		}
	}
	stats := bridge.Stats()
	if stats.FramesSent == 0 {
		t.Fatalf("bridge sent nothing: %+v", stats)
	}
	if !stats.Running {
		t.Fatal("bridge reports not running while started")
	}
	t.Logf("forwarded %d frames to %d peers", stats.FramesSent, len(sessions))
}

// TestAudioBridgeMixiesPeersIntoPlayback proves the receive path: audio arriving
// from peers is summed and written to the speaker.
func TestAudioBridgeMixesPeersIntoPlayback(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	a := newFakeSession()
	b := newFakeSession()
	list := func() []media.ActivePeer {
		return []media.ActivePeer{{PeerID: "a", Session: a}, {PeerID: "b", Session: b}}
	}

	bridge := daemon.NewAudioBridge(engine, slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{}, list)
	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	// Feed a continuous tone from both peers for a moment.
	frame := make([]int16, audio.FramesPerPacket)
	for i := range frame {
		frame[i] = 1500
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
			case a.incoming <- frame:
			default:
			}
			select {
			case b.incoming <- frame:
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	defer close(stop)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats := bridge.Stats()
		if stats.FramesMixed > 0 && stats.FramesReceived >= 2 {
			t.Logf("mixed %d frames from %d received", stats.FramesMixed, stats.FramesReceived)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("peers' audio was never mixed into playback: %+v", bridge.Stats())
}

// TestAudioBridgeMuteSendsSilence proves muting removes the local voice without
// stopping transmission, so receivers keep their timeline.
func TestAudioBridgeMuteSendsSilence(t *testing.T) {
	engine := usableEngine(t)
	defer engine.Close()

	sess := newFakeSession()
	list := func() []media.ActivePeer {
		return []media.ActivePeer{{PeerID: "p1", Session: sess}}
	}

	bridge := daemon.NewAudioBridge(engine, slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon.AudioBridgeConfig{Muted: true}, list)
	if err := bridge.Start(context.Background()); err != nil {
		t.Skipf("cannot start audio: %v", err)
	}
	defer bridge.Stop()

	// Drain whatever the muted bridge sends and confirm every frame is silence.
	deadline := time.After(5 * time.Second)
	got := 0
	for got < 3 {
		select {
		case frame := <-sess.sent:
			for i, v := range frame {
				if v != 0 {
					t.Fatalf("muted bridge sent non-zero sample %d at index %d", v, i)
				}
			}
			got++
		case <-deadline:
			t.Fatalf("muted bridge sent only %d frames; transmission stopped entirely", got)
		}
	}
}

// TestAudioBridgeClampPreventsClipping checks the mixer saturates rather than
// wrapping, which would turn loud speech into a harsh crackle.
func TestAudioBridgeClampPreventsClipping(t *testing.T) {
	cases := map[int32]int16{
		0:      0,
		1000:   1000,
		32767:  32767,
		32768:  32767,
		100000: 32767,
		-32768: -32768,
		-32769: -32768,
	}
	for in, want := range cases {
		if got := daemon.ClampInt16(in); got != want {
			t.Fatalf("clamp(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestStatusReportsAudioCapability confirms the daemon tells the truth about
// audio: available exactly when the backend initialised.
func TestStatusReportsAudioCapability(t *testing.T) {
	prof := newProfile(t, nil)
	c := startDaemon(t, prof)()

	var status ipc.StatusResult
	if err := c.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "status", Method: ipc.MethodStatus,
	}, &status); err != nil {
		t.Fatal(err)
	}

	usable := hostAudioUsable(t)
	if usable != status.Capabilities.Audio {
		t.Fatalf("audio capability %v contradicts host usability %v (state %q)",
			status.Capabilities.Audio, usable, status.AudioState)
	}
	if status.AudioState == "" {
		t.Fatal("status has no audio state string")
	}
}
