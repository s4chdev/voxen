// Package dsp provides the optional audio preprocessing stage: echo cancellation
// and noise suppression applied to microphone audio before it is encoded.
//
// The stage is optional in two senses:
//
//   - It is compiled only with the "speex" build tag, because it needs cgo and the
//     libspeexdsp development headers. Without the tag the package still builds and
//     reports the features as unavailable, which is what keeps the default build
//     dependency-free and cross-compilable.
//   - At runtime it can be disabled per call, and the daemon reports exactly which
//     processing is active rather than implying it.
//
// Echo cancellation needs a far-end reference signal: the audio currently being
// played through the speaker. This package takes that reference explicitly rather
// than guessing, because a canceller fed the wrong reference is worse than none.
package dsp

import "sync"

// FrameSize is the processing frame length in samples: 20 ms at 48 kHz, matching
// the Opus frame size so no additional buffering is required.
const FrameSize = 960

// SampleRate is the processing rate, fixed to the media profile.
const SampleRate = 48000

// Config selects which stages run.
type Config struct {
	// EchoCancellation removes the speaker signal from the microphone.
	EchoCancellation bool
	// NoiseSuppression attenuates stationary background noise.
	NoiseSuppression bool
	// FilterLengthMS is the echo-canceller tail length in milliseconds. It must
	// cover the acoustic path plus the playback buffering; the default is generous
	// because too short a tail cancels nothing while still costing CPU.
	FilterLengthMS int
}

// DefaultFilterLengthMS is the tail length used when none is configured.
//
// 200 ms covers a typical laptop speaker-to-microphone path with room reverb. WebRTC
// implementations commonly use 100-500 ms; below 100 ms the canceller fails to model
// the delay and cancellation collapses.
const DefaultFilterLengthMS = 200

// Processor is a stateful preprocessing stage for one microphone stream.
//
// It is not safe for concurrent use: one instance belongs to one capture loop.
type Processor interface {
	// PushReference supplies the far-end signal (what is being played) so the
	// canceller can model the echo path. Call it with the same cadence as Process.
	PushReference(frames []int16)
	// Process filters one frame in place. Frames shorter than FrameSize are
	// processed as far as they go and the remainder is left untouched.
	Process(frames []int16)
	// Active reports which stages are actually running, for diagnostics.
	Active() ActiveStages
	// Close releases backend resources.
	Close() error
}

// ActiveStages describes what a processor is really doing.
//
// It exists so "voxen status" and "voxen doctor" can report the truth. A user who
// enables echo cancellation on a build without it must be told, not left to wonder
// why their voice still echoes.
type ActiveStages struct {
	EchoCancellation bool
	NoiseSuppression bool
	// Backend names the implementation, for example "speexdsp" or "none".
	Backend string
}

// None is the processor used when no backend is compiled in, or when the caller
// disabled everything. It passes audio through untouched and reports no stages.
type None struct{}

// PushReference implements Processor.
func (None) PushReference([]int16) {}

// Process implements Processor.
func (None) Process([]int16) {}

// Active implements Processor.
func (None) Active() ActiveStages { return ActiveStages{Backend: "none"} }

// Close implements Processor.
func (None) Close() error { return nil }

// factory is installed by the cgo build.
var (
	factoryMu sync.Mutex
	factory   func(Config) (Processor, error)
)

// Register installs the backend constructor. It is called from an init function in
// the cgo-tagged file, so the default build has no implementation.
func Register(fn func(Config) (Processor, error)) {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factory = fn
}

// New builds a processor for cfg.
//
// When no backend is compiled in this returns None and no error: asking for echo
// cancellation on a build that cannot provide it is a configuration fact, not a
// failure, and the caller reports it through Active rather than refusing to start.
func New(cfg Config) (Processor, error) {
	factoryMu.Lock()
	fn := factory
	factoryMu.Unlock()

	if fn == nil {
		// No backend: report honestly that nothing is running.
		return None{}, nil
	}
	if !cfg.EchoCancellation && !cfg.NoiseSuppression {
		return None{}, nil
	}
	return fn(cfg)
}

// Available reports whether a processing backend was compiled in.
func Available() bool {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	return factory != nil
}

// BackendName identifies the compiled backend, or "none".
func BackendName() string {
	if Available() {
		return "speexdsp"
	}
	return "none"
}
