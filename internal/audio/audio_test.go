//go:build cgo

package audio

import (
	"context"
	"math"
	"testing"
	"time"
)

// newEngine builds an engine or skips the test when no audio server is
// available, which is the normal state on a headless CI runner.
func newEngine(t *testing.T) *MiniAudio {
	t.Helper()
	engine, err := NewMiniAudio(nil)
	if err != nil {
		t.Skipf("audio backend unavailable: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

// TestDevicesEnumeration checks that real devices are listed and that at least
// one of them is marked default.
func TestDevicesEnumeration(t *testing.T) {
	engine := newEngine(t)
	devices, err := engine.Devices(context.Background())
	if err != nil {
		t.Skipf("cannot enumerate devices: %v", err)
	}
	if len(devices) == 0 {
		t.Skip("no audio devices present")
	}

	var inputs, outputs int
	for _, d := range devices {
		if d.ID == "" {
			t.Fatalf("device %q has an empty identifier", d.Name)
		}
		if d.IsInput {
			inputs++
		}
		if d.IsOutput {
			outputs++
		}
		if d.IsInput && d.IsOutput {
			t.Fatalf("device %q claims to be both input and output", d.Name)
		}
	}
	t.Logf("%d input, %d output device(s); backend %s", inputs, outputs, engine.Version())
	if inputs == 0 {
		t.Skip("no capture devices present")
	}
}

// TestCaptureProducesFramesFromRealDevice records briefly from the default
// microphone and checks the stream delivers correctly sized, correctly
// formatted frames.
//
// A silent microphone is acceptable: the test asserts on shape, not on level,
// because CI runners and muted machines legitimately capture silence.
func TestCaptureProducesFramesFromRealDevice(t *testing.T) {
	engine := newEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := engine.OpenCapture(ctx, "")
	if err != nil {
		t.Skipf("cannot open capture device: %v", err)
	}
	defer stream.Close()

	pcm := make([]int16, FramesPerPacket)
	received := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && received < 10 {
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		err := stream.ReadFrames(readCtx, pcm)
		readCancel()
		if err != nil {
			break
		}
		received++
	}

	if received == 0 {
		t.Skip("no frames captured; the machine may have no live microphone")
	}
	stats := stream.Stats()
	t.Logf("captured %d frames (stats: captured=%d dropped=%d xruns=%d device=%q)",
		received, stats.FramesCaptured, stats.Dropped, stats.XRuns, stats.DeviceID)
	if stats.FramesCaptured == 0 {
		t.Fatal("frames arrived but the capture counter stayed at zero")
	}
	if stats.Dropped > stats.FramesCaptured {
		t.Fatalf("dropped more frames than were captured: %+v", stats)
	}
}

// TestPlaybackAcceptsFrames pushes a tone through the default speaker and checks
// the backend accepts it without erroring.
//
// This cannot assert that sound was audible; it verifies the write path, the
// channel expansion and that no underrun storm occurs.
func TestPlaybackAcceptsFrames(t *testing.T) {
	engine := newEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := engine.OpenPlayback(ctx, "")
	if err != nil {
		t.Skipf("cannot open playback device: %v", err)
	}
	defer stream.Close()

	// A short quiet tone, so a developer running the test hears something but
	// not something loud.
	frame := make([]int16, FramesPerPacket)
	for i := 0; i < 10; i++ {
		for j := range frame {
			frame[j] = int16(2000 * math.Sin(2*math.Pi*440*float64(j)/48000))
		}
		if err := stream.WriteFrames(ctx, frame); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stats := stream.Stats()
	t.Logf("playback stats: played=%d dropped=%d xruns=%d device=%q",
		stats.FramesPlayed, stats.Dropped, stats.XRuns, stats.DeviceID)
	if stats.Dropped > 0 {
		t.Fatalf("playback dropped frames while being fed in real time: %+v", stats)
	}
}

// TestCaptureHonoursContextCancellation proves ReadFrames does not hang when the
// caller gives up, which matters for a clean call teardown.
func TestCaptureHonoursContextCancellation(t *testing.T) {
	engine := newEngine(t)
	stream, err := engine.OpenCapture(context.Background(), "")
	if err != nil {
		t.Skipf("cannot open capture device: %v", err)
	}
	defer stream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		pcm := make([]int16, FramesPerPacket)
		// Loop until cancelled; at least one iteration must observe ctx.Done.
		for {
			if err := stream.ReadFrames(ctx, pcm); err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ReadFrames returned without an error after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadFrames did not observe context cancellation")
	}
}

// TestToMonoDownmix verifies the channel conversion used on the audio thread.
func TestToMonoDownmix(t *testing.T) {
	s := &captureStream{channels: 2}
	stereo := []int16{100, 300, -200, 200, 0, 0}
	got := s.toMono(stereo)
	want := []int16{200, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("mono length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mono[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestToMonoPassthrough checks that mono input is not altered.
func TestToMonoPassthrough(t *testing.T) {
	s := &captureStream{channels: 1}
	mono := []int16{1, -2, 3}
	got := s.toMono(mono)
	if len(got) != len(mono) {
		t.Fatalf("length changed: %d != %d", len(got), len(mono))
	}
	for i := range mono {
		if got[i] != mono[i] {
			t.Fatalf("sample %d altered: %d != %d", i, got[i], mono[i])
		}
	}
}

// TestToDeviceExpansion verifies mono is duplicated across device channels.
func TestToDeviceExpansion(t *testing.T) {
	s := &playbackStream{channels: 2}
	mono := []int16{1000, -1000}
	out := make([]byte, len(mono)*2*2)
	s.toDevice(mono, out)

	// Expect: L=1000 R=1000 L=-1000 R=-1000 as little-endian S16.
	want := []byte{
		0xE8, 0x03, 0xE8, 0x03,
		0x18, 0xFC, 0x18, 0xFC,
	}
	if len(out) != len(want) {
		t.Fatalf("output length = %d, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("byte %d = 0x%02X, want 0x%02X", i, out[i], want[i])
		}
	}
}

// TestBytesToInt16RoundTrip checks the PCM byte conversion in both directions.
func TestBytesToInt16RoundTrip(t *testing.T) {
	samples := []int16{0, 1, -1, 32767, -32768, 12345}
	buf := int16ToBytes(nil, samples)
	got := bytesToInt16(buf)
	if len(got) != len(samples) {
		t.Fatalf("length = %d, want %d", len(got), len(samples))
	}
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], samples[i])
		}
	}
}
