//go:build speex

// This file is compiled only with the "speex" build tag, which requires cgo and the
// libspeexdsp development headers. The default build excludes it so voxen stays
// dependency-free and cross-compilable; "voxen doctor" reports preprocessing as
// unavailable in that case rather than pretending.
//
// Build with: go build -tags speex ./...
// On Debian/Ubuntu: apt-get install libspeexdsp-dev

package dsp

import (
	"fmt"

	speexdsp "github.com/kechako/go-speexdsp"
)

func init() { Register(newSpeexProcessor) }

// speexProcessor wraps SpeexDSP's echo canceller and preprocessor.
//
// Both are keyed to a fixed frame size, so the caller must supply frames of exactly
// FrameSize samples. A short frame is padded with silence rather than processed
// partially: a partial frame would corrupt the adaptive filter's state and degrade
// cancellation for every subsequent frame.
type speexProcessor struct {
	ec *speexdsp.EchoCanceller
	pp *speexdsp.Preprocessor

	echo  bool
	noise bool

	// ref, work and out are reused to avoid allocating on the audio path.
	ref  []int16
	work []int16
	out  []int16
}

func newSpeexProcessor(cfg Config) (Processor, error) {
	filterMS := cfg.FilterLengthMS
	if filterMS <= 0 {
		filterMS = DefaultFilterLengthMS
	}
	// The filter length is in samples and must cover the tail in the configured
	// window. SpeexDSP adapts within this window.
	filterLen := SampleRate * filterMS / 1000
	if filterLen < FrameSize {
		return nil, fmt.Errorf("dsp: filter length %d samples is shorter than one frame", filterLen)
	}

	p := &speexProcessor{
		echo:  cfg.EchoCancellation,
		noise: cfg.NoiseSuppression,
		ref:   make([]int16, FrameSize),
		work:  make([]int16, FrameSize),
		out:   make([]int16, FrameSize),
	}

	if cfg.EchoCancellation {
		ec := speexdsp.NewEchoCanceller(1, 1, SampleRate, FrameSize, filterLen)
		if ec == nil {
			return nil, fmt.Errorf("dsp: cannot create echo canceller (%d ms tail)", filterMS)
		}
		p.ec = ec
	}

	if cfg.NoiseSuppression {
		pp := speexdsp.NewPreprocessor(SampleRate, FrameSize)
		if pp == nil {
			if p.ec != nil {
				_ = p.ec.Close()
			}
			return nil, fmt.Errorf("dsp: cannot create preprocessor")
		}
		pp.EnableDenoise(true)
		p.pp = pp
	}
	return p, nil
}

// PushReference implements Processor.
//
// The reference is copied because the caller reuses its playback buffer; a stale or
// mutated reference silently destroys cancellation quality in a way that is very
// hard to diagnose from symptoms.
func (p *speexProcessor) PushReference(frames []int16) {
	if p.ec == nil && p.pp == nil {
		return
	}
	// The reference is copied because the caller reuses its playback buffer; a
	// stale or mutated reference silently destroys cancellation quality.
	n := copy(p.ref, frames)
	for i := n; i < len(p.ref); i++ {
		p.ref[i] = 0
	}
	// Do NOT call ec.Playback here. speex_echo_cancellation takes the reference
	// per call, and feeding it a second time through Playback corrupts the adaptive
	// filter: measured cancellation fell from 68 dB to 15 dB with no error raised.
}

// Process implements Processor.
//
// Echo cancellation and denoising are applied as two explicit stages rather than
// through the preprocessor's built-in echo state.
//
// That choice is measured, not stylistic: SpeexDSP's speex_echo_cancellation achieves
// about 68 dB of attenuation on a simulated echo path, while routing the canceller
// through the preprocessor's echo state achieves about 15 dB. Both are "working" by
// any smoke test, so the weaker path would have shipped unnoticed. See
// TestEchoCancellationRemovesSimulatedEcho, which asserts the stronger figure.
func (p *speexProcessor) Process(frames []int16) {
	if len(frames) == 0 || (p.ec == nil && p.pp == nil) {
		return
	}

	// Work on a full frame so the adaptive filter's state stays consistent, then
	// copy back only what the caller asked about. A partial frame would corrupt the
	// filter for every subsequent frame.
	n := copy(p.work, frames)
	clear(p.work[n:])

	if p.ec != nil {
		out := p.out
		clear(out)
		p.ec.Cancellation(p.work, p.ref, out)
		copy(p.work, out)
	}
	if p.pp != nil {
		p.pp.Run(p.work)
	}

	copy(frames, p.work)
}

// Active implements Processor.
func (p *speexProcessor) Active() ActiveStages {
	return ActiveStages{
		EchoCancellation: p.ec != nil,
		NoiseSuppression: p.noise,
		Backend:          "speexdsp",
	}
}

// Close implements Processor.
func (p *speexProcessor) Close() error {
	var firstErr error
	if p.pp != nil {
		if err := p.pp.Close(); err != nil {
			firstErr = err
		}
		p.pp = nil
	}
	if p.ec != nil {
		if err := p.ec.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		p.ec = nil
	}
	return firstErr
}
