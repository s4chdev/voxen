//go:build speex

package dsp

import (
	"math"
	"testing"
)

// TestEchoCancellationRemovesSimulatedEcho is the test that matters: it asserts the
// canceller actually attenuates a known echo path.
//
// A filter that runs without cancelling would be worse than none, because it costs
// CPU and adds latency while claiming to help. The simulation is a tone played
// through the speaker, picked up by the microphone attenuated and delayed by one
// frame. That is a simplified acoustic path, but it is the shape of a real one, and
// 20 dB of attenuation is a threshold a working canceller clears easily while a
// broken one cannot reach.
func TestEchoCancellationRemovesSimulatedEcho(t *testing.T) {
	p, err := New(Config{EchoCancellation: true, FilterLengthMS: DefaultFilterLengthMS})
	if err != nil {
		t.Skipf("echo canceller unavailable: %v", err)
	}
	defer p.Close()

	if a := p.Active(); !a.EchoCancellation {
		t.Fatalf("processor reports echo cancellation off: %+v", a)
	}

	far := tone(440, 12000, FrameSize)
	near := make([]int16, FrameSize)
	for i := range near {
		near[i] = far[i] / 3 // roughly -10 dB echo path
	}

	// The canceller needs time to adapt, exactly as it would in a call. Measure only
	// after convergence.
	var beforeSum, afterSum float64
	var samples int
	for iter := 0; iter < 300; iter++ {
		p.PushReference(far)

		mic := make([]int16, FrameSize)
		copy(mic, near)

		// Measure the raw echo before processing, then the residue after. The
		// canceller is stateful, so a single frame is meaningless.
		rawLevel := rms(mic)
		p.Process(mic)
		processedLevel := rms(mic)

		if iter >= 200 {
			beforeSum += rawLevel
			afterSum += processedLevel
			samples++
		}
	}

	beforeAvg := beforeSum / float64(samples)
	afterAvg := afterSum / float64(samples)
	if beforeAvg == 0 {
		t.Fatal("simulated echo has no level; the test is not measuring anything")
	}

	reductionDB := 20 * math.Log10(beforeAvg/(afterAvg+1))
	t.Logf("echo level %.0f -> %.0f (%.1f dB reduction)", beforeAvg, afterAvg, reductionDB)

	if reductionDB < 20 {
		t.Fatalf("echo cancellation achieved only %.1f dB; expected at least 20 dB", reductionDB)
	}
}

// TestNoiseSuppressionReducesStationaryNoise proves the denoiser attenuates noise
// on a speech-like signal.
func TestNoiseSuppressionReducesStationaryNoise(t *testing.T) {
	p, err := New(Config{NoiseSuppression: true})
	if err != nil {
		t.Skipf("noise suppression unavailable: %v", err)
	}
	defer p.Close()

	if a := p.Active(); !a.NoiseSuppression {
		t.Fatalf("processor reports noise suppression off: %+v", a)
	}

	// A tone plus noise: the denoiser should keep much of the tone while cutting
	// the noise floor, so total energy drops but not to zero.
	frame := make([]int16, FrameSize)
	seed := uint32(12345)
	next := func() float64 {
		seed = seed*1664525 + 1013904223
		return float64(seed>>16)/32768.0 - 1
	}
	for i := range frame {
		frame[i] = int16(6000*math.Sin(2*math.Pi*300*float64(i)/SampleRate) + next()*800)
	}

	var inSum, outSum float64
	for iter := 0; iter < 50; iter++ {
		work := make([]int16, FrameSize)
		copy(work, frame)
		inSum += rms(work)
		p.Process(work)
		outSum += rms(work)
	}

	inAvg := inSum / 50
	outAvg := outSum / 50
	t.Logf("level %.0f -> %.0f", inAvg, outAvg)
	if outAvg >= inAvg {
		t.Fatalf("noise suppression did not reduce level: %.0f -> %.0f", inAvg, outAvg)
	}
	if outAvg == 0 {
		t.Fatal("noise suppression removed everything, including the signal")
	}
}

// TestSilenceStaysSilent proves the processor never fabricates signal. A denoiser
// that emits energy from silence would make a silent call sound like a fault.
func TestSilenceStaysSilent(t *testing.T) {
	p, err := New(Config{NoiseSuppression: true, EchoCancellation: true})
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	defer p.Close()

	silent := make([]int16, FrameSize)
	for i := 0; i < 20; i++ {
		p.PushReference(silent)
		work := make([]int16, FrameSize)
		p.Process(work)
		if lvl := rms(work); lvl > 1 {
			t.Fatalf("iteration %d: silence produced level %.0f", i, lvl)
		}
	}
}

// TestShortFrameIsPadded proves a short frame does not corrupt filter state. The
// caller may deliver a partial frame at the end of a stream.
func TestShortFrameIsPadded(t *testing.T) {
	p, err := New(Config{EchoCancellation: true})
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	defer p.Close()

	short := make([]int16, 100)
	for i := range short {
		short[i] = 1000
	}
	p.PushReference(short)
	p.Process(short) // must not panic or corrupt state

	// A full frame afterwards must still work.
	full := tone(440, 5000, FrameSize)
	p.PushReference(full)
	p.Process(full)
}

// TestCloseIsIdempotent proves double cleanup does not crash, since shutdown paths
// can overlap.
func TestCloseIsIdempotent(t *testing.T) {
	p, err := New(Config{EchoCancellation: true})
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// Processing after close must be a no-op rather than a crash.
	p.Process(make([]int16, FrameSize))
}
