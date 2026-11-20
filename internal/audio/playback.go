//go:build cgo

package audio

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
)

// playbackStream is a live speaker sink.
//
// It owns a small queue of frames. If the writer falls behind, WriteFrames drops
// the oldest frame rather than blocking: a live call must not accumulate latency
// when the network stalls.
type playbackStream struct {
	dev    *malgo.Device
	log    *slog.Logger
	engine *MiniAudio

	frames chan []int16
	stats  StreamStats

	rate     int
	channels int

	mu     sync.Mutex
	closed bool
}

// WriteFrames queues one frame for playback.
func (s *playbackStream) WriteFrames(ctx context.Context, pcm []int16) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrStreamGone
	}

	frame := make([]int16, len(pcm))
	copy(frame, pcm)

	select {
	case s.frames <- frame:
	default:
		// Drop the oldest frame to keep latency bounded.
		select {
		case <-s.frames:
			atomic.AddUint64(&s.stats.Dropped, 1)
		default:
		}
		select {
		case s.frames <- frame:
		default:
			atomic.AddUint64(&s.stats.Dropped, 1)
		}
	}
	return nil
}

// Stats implements PlaybackStream.
func (s *playbackStream) Stats() StreamStats {
	return StreamStats{
		FramesPlayed: atomic.LoadUint64(&s.stats.FramesPlayed),
		Dropped:      atomic.LoadUint64(&s.stats.Dropped),
		XRuns:        atomic.LoadUint64(&s.stats.XRuns),
		DeviceID:     s.stats.DeviceID,
	}
}

// Close stops the device.
func (s *playbackStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.dev != nil {
		_ = s.dev.Stop()
		s.dev.Uninit()
	}
	if s.engine != nil {
		s.engine.forget(s)
	}
	return nil
}

// openPlayback builds and starts a playback device.
func (m *MiniAudio) openPlayback(deviceID DeviceID) (*playbackStream, error) {
	if m.closed.Load() {
		return nil, ErrUnavailable
	}
	devID, err := m.findDevice(malgo.Playback, deviceID)
	if err != nil {
		return nil, err
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = uint32(DefaultFormat.Channels)
	cfg.Playback.DeviceID = devID
	cfg.SampleRate = uint32(DefaultFormat.SampleRate)
	cfg.PeriodSizeInMilliseconds = uint32(DefaultFrameDurationMS)
	cfg.Periods = 3

	s := &playbackStream{
		log:    m.log,
		engine: m,
		frames: make(chan []int16, 32),
		stats:  StreamStats{DeviceID: deviceID},
	}

	callbacks := malgo.DeviceCallbacks{
		Data: func(out, _ []byte, frameCount uint32) {
			if len(out) == 0 {
				return
			}
			// Silence first: an underrun must never replay stale audio, which
			// would sound like a stutter locked in a loop.
			for i := range out {
				out[i] = 0
			}
			select {
			case frame := <-s.frames:
				n := s.toDevice(frame, out)
				_ = n
				atomic.AddUint64(&s.stats.FramesPlayed, 1)
			default:
				atomic.AddUint64(&s.stats.XRuns, 1)
			}
		},
	}

	dev, err := malgo.InitDevice(m.ctx.Context, cfg, callbacks)
	if err != nil {
		return nil, errDeviceOpen("playback", err)
	}
	s.dev = dev
	s.rate = int(dev.PlaybackInternalSampleRate())
	s.channels = int(dev.PlaybackInternalChannels())
	m.log.Debug("playback device opened",
		"requested_rate", cfg.SampleRate, "device_rate", s.rate,
		"device_channels", s.channels, "device_id", string(deviceID))

	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, errDeviceStart("playback", err)
	}
	// Register before returning, so Engine.Close can tear this stream down. An
	// engine closed while a device is still open frees the backend context out from
	// under that device, which underflows PulseAudio's client refcount and aborts
	// the process from inside C.
	if !m.watch(s) {
		_ = dev.Stop()
		dev.Uninit()
		return nil, ErrUnavailable
	}
	return s, nil
}

// toDevice expands mono audio to the device's channel count and writes it into
// the output buffer as little-endian S16. It runs on the audio thread.
func (s *playbackStream) toDevice(mono []int16, out []byte) int {
	channels := s.channels
	if channels <= 0 {
		channels = 1
	}
	frames := len(out) / (2 * channels)
	if frames > len(mono) {
		frames = len(mono)
	}
	for i := 0; i < frames; i++ {
		v := mono[i]
		for c := 0; c < channels; c++ {
			off := (i*channels + c) * 2
			out[off] = byte(uint16(v))
			out[off+1] = byte(uint16(v) >> 8)
		}
	}
	return frames
}

// errDeviceOpen wraps a device-open failure with actionable context, mapping the
// most common permission problem onto ErrPermission.
func errDeviceOpen(kind string, err error) error {
	if isPermissionError(err) {
		return ErrPermission
	}
	return &deviceError{kind: kind, op: "open", err: err}
}

func errDeviceStart(kind string, err error) error {
	if isPermissionError(err) {
		return ErrPermission
	}
	return &deviceError{kind: kind, op: "start", err: err}
}

type deviceError struct {
	kind string
	op   string
	err  error
}

func (e *deviceError) Error() string {
	return "audio: " + e.op + " " + e.kind + " device: " + e.err.Error()
}

func (e *deviceError) Unwrap() error { return e.err }

// isPermissionError recognises the backend codes for a denied microphone.
//
// miniaudio surfaces these as MA_ERROR_ACCESS_DENIED or, on macOS, as a failed
// device start when the TCC prompt has not been granted.
func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if err == malgo.ErrDeviceNotInitialized {
		return false
	}
	msg := err.Error()
	for _, needle := range []string{"access denied", "permission", "not permitted", "ACCESS_DENIED"} {
		if containsFold(msg, needle) {
			return true
		}
	}
	return false
}

func containsFold(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a, b := s[i+j], substr[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
