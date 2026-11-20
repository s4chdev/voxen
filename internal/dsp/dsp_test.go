package dsp

import (
	"math"
	"testing"
)

// tone builds a sine at the given frequency and amplitude.
func tone(freq, amp float64, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*freq*float64(i)/SampleRate))
	}
	return out
}

// TestNonePassesAudioThrough proves the fallback never alters audio, so a build
// without the backend cannot silently degrade a call.
func TestNonePassesAudioThrough(t *testing.T) {
	p := None{}
	in := tone(440, 8000, FrameSize)
	before := append([]int16(nil), in...)

	p.PushReference(tone(440, 8000, FrameSize))
	p.Process(in)

	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("sample %d changed: %d -> %d", i, before[i], in[i])
		}
	}
	a := p.Active()
	if a.EchoCancellation || a.NoiseSuppression {
		t.Fatalf("the no-op processor claims to process: %+v", a)
	}
	if a.Backend != "none" {
		t.Fatalf("backend = %q", a.Backend)
	}
}

// TestNewWithNothingEnabledReturnsNone proves asking for no processing costs
// nothing and reports nothing.
func TestNewWithNothingEnabledReturnsNone(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if _, ok := p.(None); !ok {
		t.Fatalf("expected the no-op processor, got %T", p)
	}
	if a := p.Active(); a.EchoCancellation || a.NoiseSuppression {
		t.Fatalf("reports active stages with none enabled: %+v", a)
	}
}

// TestNoneHandlesShortFrames proves the no-op path tolerates frames of any length,
// because the audio layer must never panic on an unexpected buffer.
func TestNoneHandlesShortFrames(t *testing.T) {
	p := None{}
	for _, n := range []int{0, 1, 480, FrameSize, FrameSize * 2} {
		p.Process(make([]int16, n))
	}
}

// TestBackendNameIsHonest proves the reported backend matches what is compiled in,
// which is what "voxen doctor" prints.
func TestBackendNameIsHonest(t *testing.T) {
	name := BackendName()
	if name == "" {
		t.Fatal("backend name is empty")
	}
	if Available() && name == "none" {
		t.Fatal("a backend is available but reports as none")
	}
	if !Available() && name != "none" {
		t.Fatalf("no backend but reports %q", name)
	}
}
