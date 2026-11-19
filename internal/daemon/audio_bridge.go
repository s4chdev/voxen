package daemon

import (
	"context"
	"errors"
	"log/slog"
	"math"
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
// AudioBridge owns the capture and playback streams for one call.
type AudioBridge struct {
	engine audio.Engine
	log    *slog.Logger
	cfg    AudioBridgeConfig
	// peers reports the current participants. It is a function because people
	// join and leave mid-call and capture must react without a restart.
	peers func() []media.ActivePeer
	// observe receives speech transitions. A nil observer disables the
	// indicator without disabling audio.
	observe func(peerID string, speaking bool)

	mu       sync.Mutex
	capture  audio.CaptureStream
	playback audio.PlaybackStream
	muted    bool

	// activePeers sizes the silence path: with nobody connected, playback idles
	// instead of burning CPU writing silence.
	activePeers atomic.Int32

	// speaking holds the last reported state per peer, so an indicator is only
	// notified on a transition rather than on every 20 ms frame.
	speakingMu sync.Mutex
	speaking   map[string]bool
	// mutedPeers is the local per-peer mute set.
	mutedPeers map[string]bool

	framesSent     atomic.Uint64
	framesReceived atomic.Uint64
	framesMixed    atomic.Uint64

	cancel context.CancelFunc
	// lifeCancel aborts an in-flight device open, so Stop is not held hostage by a
	// driver that never returns.
	lifeCancel context.CancelFunc
	wg         sync.WaitGroup
	closed     atomic.Bool
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
// peers is a function rather than a slice because participants join and leave
// during a call; capture must react to the current set without restarting.
func NewAudioBridge(engine audio.Engine, log *slog.Logger, cfg AudioBridgeConfig, peers func() []media.ActivePeer) *AudioBridge {
	if log == nil {
		log = slog.Default()
	}
	return &AudioBridge{
		engine:     engine,
		log:        log,
		cfg:        cfg,
		peers:      peers,
		speaking:   make(map[string]bool),
		mutedPeers: make(map[string]bool),
		muted:      cfg.Muted,
	}
}

// Start opens the devices and begins pumping audio.
func (b *AudioBridge) Start(ctx context.Context) error {
	if b.engine == nil {
		return audio.ErrUnavailable
	}
	b.mu.Lock()
	if b.cancel != nil {
		b.mu.Unlock()
		return errors.New("audio bridge already started")
	}
	// A context owned by the bridge, so Stop can abort a device open that is in
	// flight. Without this, a wedged driver keeps Start blocked until its own
	// timeout even though the caller has already given up.
	lifeCtx, lifeCancel := context.WithCancel(ctx)
	// started flips to true only on the success path, where ownership of the
	// lifecycle context passes to Stop.
	started := false
	defer func() {
		if started {
			return
		}
		// Start is returning without a live bridge: release the context and clear
		// the field so a later Start is not blocked by a stale cancel func.
		lifeCancel()
		b.mu.Lock()
		b.lifeCancel = nil
		b.mu.Unlock()
	}()
	b.lifeCancel = lifeCancel
	b.mu.Unlock()

	openCtx, cancel := context.WithTimeout(lifeCtx, 10*time.Second)
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
	// Publish the streams and cancel func together under the lock. Stop may have
	// been called while the devices were opening (a failed call, or daemon
	// shutdown racing the first join); if so, close what was just opened instead
	// of starting loops nobody will stop.
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		_ = capture.Close()
		_ = playback.Close()
		return audio.ErrStreamGone
	}
	b.capture = capture
	b.playback = playback
	runCtx, runCancel := context.WithCancel(context.Background())
	b.cancel = runCancel
	// Ownership of the lifecycle context passes to Stop from here on.
	started = true
	b.wg.Add(2)
	b.mu.Unlock()

	// The loops receive their streams explicitly. Reading b.capture/b.playback from
	// inside them would race Stop, which clears those fields to release the devices.
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

		peers := b.peers()
		if muted {
			// Transmit silence rather than nothing: a continuously muted sender
			// keeps the RTP timeline alive for the receiver's jitter buffer.
			silence := make([]int16, len(pcm))
			for _, p := range peers {
				_ = p.Session.SendPCM(ctx, silence)
			}
			b.framesSent.Add(1)
			continue
		}

		for _, p := range peers {
			if err := p.Session.SendPCM(ctx, pcm); err != nil && !errors.Is(err, media.ErrUnauthenticated) {
				b.log.Debug("send to peer failed", "peer", p.PeerID, "error", err)
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

		peers := b.peers()
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
		for _, p := range peers {
			readCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
			n, err := p.Session.ReceivePCM(readCtx, frame)
			cancel()
			if err != nil || n == 0 {
				// No audio means this peer is not speaking; report that so an
				// indicator does not stay lit after someone stops talking.
				b.reportSpeaking(p.PeerID, false)
				continue
			}
			got++
			b.framesReceived.Add(1)

			// Speech detection is measured before the local mute is applied: the
			// indicator should show that the peer is talking even when the local
			// user has silenced them, otherwise the UI would claim silence from a
			// peer who is plainly speaking.
			b.reportSpeaking(p.PeerID, isSpeech(frame[:n]))

			// A locally muted peer still gets a frame read (above) so its stream
			// does not back up, but nothing of it reaches the speaker.
			if b.peerMuted(p.PeerID) {
				continue
			}
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

// Stop closes the devices and waits for the loops to exit. Safe to call more than
// once and safe to call while Start is still opening devices.
func (b *AudioBridge) Stop() {
	if b.closed.Swap(true) {
		return
	}
	b.mu.Lock()
	cancel := b.cancel
	// Cancel the lifecycle context first: this aborts a device open that Start is
	// still waiting on, which is what lets Stop return promptly on a wedged driver.
	lifeCancel := b.lifeCancel
	capture := b.capture
	playback := b.playback
	b.cancel = nil
	b.lifeCancel = nil
	b.capture = nil
	b.playback = nil
	b.mu.Unlock()

	if lifeCancel != nil {
		lifeCancel()
	}
	if cancel != nil {
		cancel()
	}
	// Close the streams before waiting: a loop blocked in ReadFrames may be waiting
	// on a device that only unblocks when the device is closed. This is also what
	// frees the backend resources, since streams own their device.
	if capture != nil {
		_ = capture.Close()
	}
	if playback != nil {
		_ = playback.Close()
	}

	// Wait for the loops, but never forever. A wedged driver must not prevent the
	// process from exiting; the OS reclaims the device when the process dies.
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopWaitTimeout):
		b.log.Warn("audio loops did not stop in time; continuing teardown")
	}
}

// stopWaitTimeout bounds how long Stop waits for its loops to exit.
const stopWaitTimeout = 2 * time.Second

// SetObserver installs the speech-state callback. Safe to call before or during a
// call; a nil observer disables reporting.
func (b *AudioBridge) SetObserver(fn func(peerID string, speaking bool)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observe = fn
}

// SetPeerMuted silences one participant locally.
//
// This is a playback control, not a transmission one: the peer keeps sending
// audio and voxen keeps reading it (so their stream does not back up), but none of
// it reaches the speaker.
func (b *AudioBridge) SetPeerMuted(peerID string, muted bool) {
	b.speakingMu.Lock()
	defer b.speakingMu.Unlock()
	if muted {
		b.mutedPeers[peerID] = true
		return
	}
	delete(b.mutedPeers, peerID)
}

// peerMuted reports the local mute state for a participant.
func (b *AudioBridge) peerMuted(peerID string) bool {
	b.speakingMu.Lock()
	defer b.speakingMu.Unlock()
	return b.mutedPeers[peerID]
}

// PeerMuted reports the local mute state, for the IPC peer listing.
func (b *AudioBridge) PeerMuted(peerID string) bool { return b.peerMuted(peerID) }

// reportSpeaking notifies the observer only when a peer's state changes, so a UI
// is not woken every 20 ms to be told the same thing.
func (b *AudioBridge) reportSpeaking(peerID string, speaking bool) {
	b.mu.Lock()
	observe := b.observe
	b.mu.Unlock()
	if observe == nil {
		return
	}

	b.speakingMu.Lock()
	was, known := b.speaking[peerID]
	if !known {
		// Record the initial state but do not emit for it: a peer that has never
		// spoken does not need a "stopped speaking" event on the first frame.
		b.speaking[peerID] = speaking
		b.speakingMu.Unlock()
		if speaking {
			observe(peerID, true)
		}
		return
	}
	if was == speaking {
		b.speakingMu.Unlock()
		return
	}
	b.speaking[peerID] = speaking
	b.speakingMu.Unlock()
	observe(peerID, speaking)
}

// forgetPeer drops per-peer state when someone leaves, so a returning peer starts
// from a clean indicator rather than inheriting stale state.
func (b *AudioBridge) forgetPeer(peerID string) {
	b.speakingMu.Lock()
	delete(b.speaking, peerID)
	delete(b.mutedPeers, peerID)
	b.speakingMu.Unlock()
}

// SpeechThreshold is the RMS level above which a peer is considered to be
// talking. 16-bit full scale is 32767, so this is roughly -40 dBFS: below normal
// speech but above room noise, and deliberately conservative so an indicator does
// not flicker on breath or keyboard noise.
const SpeechThreshold = 600

// isSpeech reports whether a frame contains speech-level energy.
func isSpeech(frame []int16) bool {
	if len(frame) == 0 {
		return false
	}
	var sum float64
	for _, v := range frame {
		f := float64(v)
		sum += f * f
	}
	rms := math.Sqrt(sum / float64(len(frame)))
	return rms >= SpeechThreshold
}

// IsSpeechForTest exposes the speech gate to tests in the external test package.
// It carries no logic of its own; the production path calls isSpeech directly.
func IsSpeechForTest(frame []int16) bool { return isSpeech(frame) }
