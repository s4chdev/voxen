// Package audio defines the platform-independent audio abstraction.
//
// Everything above this package works with 48 kHz mono signed 16-bit PCM. The
// platform adapters (Linux PipeWire/PulseAudio, Windows WASAPI, macOS CoreAudio)
// are responsible for resampling, channel conversion and device hotplug. No
// WebRTC, signaling or identity code may import a platform audio backend
// directly.
//
// Phase 1 ships the interfaces only: the daemon reports audio as unavailable
// until the phase 2 adapters exist. voxen never claims working audio it has not
// tested.
package audio

import (
	"context"
	"errors"
)

// Format is the single internal audio format. It is not negotiable: Opus is
// configured for 48 kHz mono and the media plane assumes it.
type Format struct {
	SampleRate int // always 48000
	Channels   int // always 1
}

// DefaultFormat is the normalised internal format.
var DefaultFormat = Format{SampleRate: 48000, Channels: 1}

// FramesPerPacket is 20 ms at 48 kHz.
const FramesPerPacket = 960

// DefaultFrameDurationMS is the frame duration the media profile uses. It is
// repeated here because the audio backends need it without importing media,
// which would invert the dependency direction.
const DefaultFrameDurationMS = 20

// DeviceID is a stable, backend-specific identifier. Names are for humans;
// IDs survive device reordering.
type DeviceID string

// Device describes one capture or playback endpoint.
type Device struct {
	ID         DeviceID
	Name       string
	IsInput    bool
	IsOutput   bool
	IsDefault  bool
	SampleRate int
	Channels   int
}

// Errors returned by audio backends.
var (
	ErrUnavailable     = errors.New("audio: backend unavailable")
	ErrDeviceNotFound  = errors.New("audio: device not found")
	ErrPermission      = errors.New("audio: permission denied (grant microphone access)")
	ErrDisconnected    = errors.New("audio: device disconnected")
	ErrUnsupportedRate = errors.New("audio: unsupported sample rate")
	// ErrStreamGone is returned when a stream was closed underneath its user.
	ErrStreamGone = errors.New("audio: stream is closed")
)

// Enumerator lists and watches audio devices.
type Enumerator interface {
	// Devices returns the current device list. It must not block on a hotplug
	// notification.
	Devices(ctx context.Context) ([]Device, error)
	// Watch delivers an empty struct on every device-list change. The channel
	// closes when ctx is cancelled or the backend shuts down. Backends without
	// hotplug support return nil and no error.
	Watch(ctx context.Context) (<-chan struct{}, error)
}

// CaptureStream is a live microphone source.
//
// ReadFrames returns exactly len(pcm) int16 samples, or an error. Implementations
// block until enough samples are available and must respect ctx cancellation.
// Dropped input is reported through Stats rather than as an error, because
// audio is a real-time stream: a late buffer is skipped, not fatal.
type CaptureStream interface {
	ReadFrames(ctx context.Context, pcm []int16) error
	Stats() StreamStats
	Close() error
}

// PlaybackStream is a live speaker sink.
//
// WriteFrames blocks until the buffer is accepted. Implementations hold a small
// jitter buffer and must drop the oldest audio if the writer falls behind; the
// call must never block the RTP receive loop indefinitely.
type PlaybackStream interface {
	WriteFrames(ctx context.Context, pcm []int16) error
	Stats() StreamStats
	Close() error
}

// StreamStats exposes counters used by "voxen status" and diagnostics.
type StreamStats struct {
	// FramesCaptured / FramesPlayed count 20 ms frames.
	FramesCaptured uint64 `json:"frames_captured"`
	FramesPlayed   uint64 `json:"frames_played"`
	// Dropped counts frames discarded because of overruns or underruns.
	Dropped uint64 `json:"dropped"`
	// XRuns counts backend-level buffer underruns / overruns.
	XRuns uint64 `json:"xruns"`
	// DeviceID is the device currently in use.
	DeviceID DeviceID `json:"device_id"`
}

// Engine owns device lifecycle for the daemon. A single Engine exists per
// daemon and is safe for concurrent use.
type Engine interface {
	Enumerator
	// OpenCapture starts microphone capture on device (empty selects the
	// configured or system default).
	OpenCapture(ctx context.Context, device DeviceID) (CaptureStream, error)
	// OpenPlayback starts speaker playback on device.
	OpenPlayback(ctx context.Context, device DeviceID) (PlaybackStream, error)
	// Version reports the backend and its version, for "voxen doctor".
	Version() string
	Close() error
}

// Null is an Enumerator used when no backend is compiled in. It reports a single
// explicit error instead of pretending devices exist.
type Null struct{}

// Devices implements Enumerator.
func (Null) Devices(context.Context) ([]Device, error) { return nil, ErrUnavailable }

// Watch implements Enumerator.
func (Null) Watch(context.Context) (<-chan struct{}, error) { return nil, ErrUnavailable }

// Version implements Engine.
func (Null) Version() string { return "unavailable" }

// OpenCapture implements Engine.
func (Null) OpenCapture(context.Context, DeviceID) (CaptureStream, error) {
	return nil, ErrUnavailable
}

// OpenPlayback implements Engine.
func (Null) OpenPlayback(context.Context, DeviceID) (PlaybackStream, error) {
	return nil, ErrUnavailable
}

// Close implements Engine.
func (Null) Close() error { return nil }
