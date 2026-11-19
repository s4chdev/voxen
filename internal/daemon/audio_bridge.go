package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/media"
)

// AudioBridge moves PCM between the local audio devices and the mesh.
//
// Topology for a mesh call:
//
//	           ┌────────────┐
//	microphone │ capture    │──► one copy per peer ──► media.Session.SendPCM
//	           └────────────┘
//
//	           ┌────────────┐
//	peer 1 ───►│ mix        │──► playback ──► speakers
//	peer 2 ───►│            │
//	           └────────────┘
//
// Capture is fanned out: each peer receives the same encoded audio, which is what
// a mesh does (there is no server to fan out for us). Playback is mixed: summing
// every peer's decoded audio into one speaker stream is the only option when the
// device has a single output, and it is what makes a conversation intelligible.
const (
	// mixQueueDepth bounds how far playback can run ahead of the network.
	mixQueueDepth = 8
)

// AudioBridge owns the capture and playback streams for one call.
type AudioBridge struct {
	engine  audio.Engine
	log     *slog.Logger
	cfg     AudioBridgeConfig
	session func() []media.Session

	mu       sync.Mutex
	capture  audio.CaptureStream
	playback audio.PlaybackStream
	muted    bool

	// mixQueue carries one decoded frame per peer per tick.
	mixQueue chan []int16
	// activePeers is used to size the silence path: with no peers, playback is
	// idle rather than consuming CPU.
	activePeers atomic.Int32

	framesSent     atomic.Uint64
	framesReceived atomic.Uint64
	framesMixed    atomic.Uint64

	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed atomic.Bool
}

// AudioBridgeConfig configures the bridge.
type AudioBridgeConfig struct {
	InputDevice  string
	OutputDevice string
	// Muted starts the microphone disabled.
	Muted bool
}

// NewAudioBridge builds a bridge over the given engine.
//
// sessions is a function rather than a slice because peers join and leave during
// a call; capture must react to the current set without being restarted.
func NewAudioBridge(engine audio.Engine, log *slog.Logger, cfg AudioBridgeConfig, sessions func() []media.Session) *AudioBridge {
	if log == nil {
		log = slog.Default()
	}
	return &AudioBridge{
		engine:   engine,
		log:      log,
		cfg:      cfg,
		session:  sessions,
		mixQueue: make(chan []int16, mixQueueDepth*4),
		muted:    cfg.Muted,
	}
}

// Start opens the devices and begins pumping audio.
func (b *AudioBridge) Start(ctx context.Context) error {
	if b.engine == nil {
		return audio.ErrUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return errors.New("audio bridge already started")
	}

	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	capture, err := b.engine.OpenCapture(openCtx, audio.DeviceID(b.cfg.InputDevice))
	if err != nil {
		return err
	}
	playback, err := b.engine.OpenPlayback(openCtx, audio.DeviceID(b.cfg.OutputDevice))
	if err != nil {
		_ = capture.Close()
		return err
	}
	b.capture = capture
	b.playback = playback

	runCtx, runCancel := context.WithCancel(context.Background())
	b.cancel = runCancel

	// The loops receive their streams explicitly. Reading b.capture/b.playback from
	// inside them would race Stop, which clears those fields to release the devices.
	b.wg.Add(2)
	go func() { defer b.wg.Done(); b.captureLoop(runCtx, capture) }()
	go func() { defer b.wg.Done(); b.playbackLoop(runCtx, playback) }()

	b.log.Info("audio started", "input", string(capture.Stats().DeviceID), "output", string(playback.Stats().DeviceID))
	return nil
}

// SetMuted enables or disables microphone transmission.
//
// Muting stops transmission but keeps the capture stream open: reopening a device
// takes hundreds of milliseconds and can produce an audible click, and a muted
// participant must still be able to unmute instantly.
func (b *AudioBridge) SetMuted(muted bool) {
	b.mu.Lock()
	b.muted = muted
	b.mu.Unlock()
}

// Muted reports the current mute state.
func (b *AudioBridge) Muted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.muted
}

// captureLoop reads microphone frames and sends one encoded copy to every peer.
func (b *AudioBridge) captureLoop(ctx context.Context, capture audio.CaptureStream) {
	pcm := make([]int16, audio.FramesPerPacket)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := capture.ReadFrames(ctx, pcm); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Warn("capture read failed", "error", err)
			// A transient read failure (device switch, resume from suspend) should
			// not tear the call down; retry after a short pause.
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}

		b.mu.Lock()
		muted := b.muted
		b.mu.Unlock()

		peers := b.session()
		if muted {
			// Transmit silence rather than nothing: a continuously muted sender
			// keeps the RTP timeline alive for the receiver's jitter buffer.
			silence := make([]int16, len(pcm))
			for _, s := range peers {
				_ = s.SendPCM(ctx, silence)
			}
			b.framesSent.Add(1)
			continue
		}

		for _, s := range peers {
			if err := s.SendPCM(ctx, pcm); err != nil && !errors.Is(err, media.ErrUnauthenticated) {
				b.log.Debug("send to peer failed", "error", err)
			}
		}
		if len(peers) > 0 {
			b.framesSent.Add(1)
		}
	}
}

// playbackLoop pulls one frame from every peer, mixes them, and writes the sum to
// the speaker.
func (b *AudioBridge) playbackLoop(ctx context.Context, playback audio.PlaybackStream) {
	out := make([]int16, audio.FramesPerPacket)
	frame := make([]int16, audio.FramesPerPacket)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		peers := b.session()
		b.activePeers.Store(int32(len(peers)))
		if len(peers) == 0 {
			// Nobody to listen to. Do not feed the speaker (which would emit
			// silence at full rate for no reason); wait for a peer to appear.
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}

		// Collect one frame from each peer without blocking the loop on a slow
		// peer: a stalled session must not silence everyone else.
		sum := make([]int32, len(out))
		got := 0
		for _, s := range peers {
			readCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
			n, err := s.ReceivePCM(readCtx, frame)
			cancel()
			if err != nil || n == 0 {
				continue
			}
			got++
			b.framesReceived.Add(1)
			for i := 0; i < n && i < len(sum); i++ {
				sum[i] += int32(frame[i])
			}
		}

		if got == 0 {
			// No audio this tick; keep the speaker fed with silence so playback
			// does not underrun noisily.
			for i := range out {
				out[i] = 0
			}
		} else {
			for i := range out {
				out[i] = ClampInt16(sum[i])
			}
			b.framesMixed.Add(1)
		}

		if err := playback.WriteFrames(ctx, out); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Debug("playback write failed", "error", err)
		}
	}
}

// ClampInt16 saturates a summed sample, which is what prevents the clipping
// distortion that plain truncation produces when several peers talk at once.
func ClampInt16(v int32) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return int16(v)
	}
}

// Stats reports live audio counters for "voxen status".
type AudioStats struct {
	Running        bool   `json:"running"`
	Muted          bool   `json:"muted"`
	FramesSent     uint64 `json:"frames_sent"`
	FramesReceived uint64 `json:"frames_received"`
	FramesMixed    uint64 `json:"frames_mixed"`
	InputDropped   uint64 `json:"input_dropped"`
	OutputXRuns    uint64 `json:"output_xruns"`
	InputDevice    string `json:"input_device"`
	OutputDevice   string `json:"output_device"`
}

// Stats returns a snapshot.
func (b *AudioBridge) Stats() AudioStats {
	b.mu.Lock()
	capture := b.capture
	playback := b.playback
	muted := b.muted
	b.mu.Unlock()

	stats := AudioStats{
		Running:        capture != nil,
		Muted:          muted,
		FramesSent:     b.framesSent.Load(),
		FramesReceived: b.framesReceived.Load(),
		FramesMixed:    b.framesMixed.Load(),
	}
	if capture != nil {
		cs := capture.Stats()
		stats.InputDropped = cs.Dropped
		stats.InputDevice = string(cs.DeviceID)
	}
	if playback != nil {
		ps := playback.Stats()
		stats.OutputXRuns = ps.XRuns
		stats.OutputDevice = string(ps.DeviceID)
	}
	return stats
}

// Stop closes the devices and waits for the loops to exit.
func (b *AudioBridge) Stop() {
	if b.closed.Swap(true) {
		return
	}
	b.mu.Lock()
	cancel := b.cancel
	capture := b.capture
	playback := b.playback
	b.cancel = nil
	b.capture = nil
	b.playback = nil
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	b.wg.Wait()

	// Streams own their device lifetime, so closing them is what frees the
	// backend resources.
	if capture != nil {
		_ = capture.Close()
	}
	if playback != nil {
		_ = playback.Close()
	}
}
