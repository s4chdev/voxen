//go:build cgo

package audio

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
)

// captureStream is a live microphone source.
//
// The backend delivers whatever format the device negotiated; this type converts
// it to 48 kHz mono int16 and exposes it as fixed-size frames.
type captureStream struct {
	dev    *malgo.Device
	log    *slog.Logger
	engine *MiniAudio

	// frames receives converted, normalised frames from the audio thread.
	frames chan []int16

	// stats is written by the audio thread with atomics.
	stats StreamStats

	rate     int
	channels int

	mu     sync.Mutex
	closed bool
	// downmix holds per-frame scratch used on the audio thread.
	scratch []int16
}

// ReadFrames fills pcm with exactly len(pcm) samples.
//
// If the requested length is not a multiple of the 20 ms frame size the caller
// gets whatever the backend produced; the media plane always requests 960.
func (s *captureStream) ReadFrames(ctx context.Context, pcm []int16) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-s.frames:
			if frame == nil {
				return ErrStreamGone
			}
			n := copy(pcm, frame)
			if n < len(pcm) {
				// Pad with silence so the caller always gets a full frame: a short
				// frame would produce a malformed Opus packet.
				for i := n; i < len(pcm); i++ {
					pcm[i] = 0
				}
			}
			return nil
		}
	}
}

// Stats implements CaptureStream.
func (s *captureStream) Stats() StreamStats {
	return StreamStats{
		FramesCaptured: atomic.LoadUint64(&s.stats.FramesCaptured),
		Dropped:        atomic.LoadUint64(&s.stats.Dropped),
		XRuns:          atomic.LoadUint64(&s.stats.XRuns),
		DeviceID:       s.stats.DeviceID,
	}
}

// Close stops the device.
func (s *captureStream) Close() error {
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

// openCapture builds and starts a capture device.
func (m *MiniAudio) openCapture(deviceID DeviceID) (*captureStream, error) {
	if m.closed.Load() {
		return nil, ErrUnavailable
	}
	devID, err := m.findDevice(malgo.Capture, deviceID)
	if err != nil {
		return nil, err
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = uint32(DefaultFormat.Channels)
	cfg.Capture.DeviceID = devID
	cfg.SampleRate = uint32(DefaultFormat.SampleRate)
	cfg.PeriodSizeInMilliseconds = uint32(DefaultFrameDurationMS)
	cfg.Periods = 3

	s := &captureStream{
		log:    m.log,
		engine: m,
		frames: make(chan []int16, 32),
		stats:  StreamStats{DeviceID: deviceID},
	}

	callbacks := malgo.DeviceCallbacks{
		Data: func(_, in []byte, frameCount uint32) {
			if len(in) < 2 {
				return
			}
			// The callback runs on a real-time audio thread: no logging, no
			// blocking, minimal allocation.
			samples := bytesToInt16(in)
			mono := s.toMono(samples)
			if len(mono) == 0 {
				return
			}
			frame := make([]int16, len(mono))
			copy(frame, mono)
			select {
			case s.frames <- frame:
				atomic.AddUint64(&s.stats.FramesCaptured, 1)
			default:
				// The consumer is behind. Dropping a frame is correct for live
				// audio: buffering indefinitely would add latency without bound.
				atomic.AddUint64(&s.stats.Dropped, 1)
			}
		},
	}

	dev, err := malgo.InitDevice(m.ctx.Context, cfg, callbacks)
	if err != nil {
		return nil, errDeviceOpen("capture", err)
	}

	s.dev = dev
	s.rate = int(dev.CaptureInternalSampleRate())
	s.channels = int(dev.CaptureInternalChannels())
	m.log.Debug("capture device opened",
		"requested_rate", cfg.SampleRate, "device_rate", s.rate,
		"device_channels", s.channels, "device_id", string(deviceID))

	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, errDeviceStart("capture", err)
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

// toMono converts interleaved device samples to mono at 48 kHz.
//
// The audio thread calls this, so it reuses the stream's scratch buffer rather
// than allocating per callback.
func (s *captureStream) toMono(interleaved []int16) []int16 {
	channels := s.channels
	if channels <= 0 {
		channels = 1
	}
	if channels == 1 {
		return interleaved
	}
	frames := len(interleaved) / channels
	if cap(s.scratch) < frames {
		s.scratch = make([]int16, frames)
	}
	out := s.scratch[:frames]
	for i := 0; i < frames; i++ {
		var sum int32
		for c := 0; c < channels; c++ {
			sum += int32(interleaved[i*channels+c])
		}
		out[i] = int16(sum / int32(channels))
	}
	return out
}

// bytesToInt16 interprets little-endian signed 16-bit samples.
//
// WebRTC and Opus are both little-endian on every platform voxen targets, and
// miniaudio hands back host-order S16 data.
func bytesToInt16(b []byte) []int16 {
	n := len(b) / 2
	if n == 0 {
		return nil
	}
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(uint16(b[i*2]) | uint16(b[i*2+1])<<8)
	}
	return out
}

// int16ToBytes writes little-endian signed 16-bit samples into dst.
func int16ToBytes(dst []byte, src []int16) []byte {
	need := len(src) * 2
	if cap(dst) < need {
		dst = make([]byte, need)
	}
	dst = dst[:need]
	for i, v := range src {
		u := uint16(v)
		dst[i*2] = byte(u)
		dst[i*2+1] = byte(u >> 8)
	}
	return dst
}
