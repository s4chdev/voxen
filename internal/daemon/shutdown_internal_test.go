package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/media"
)

// TestShutdownIsBounded proves a stop request returns promptly even with a client
// connection held open and audio that never started.
//
// The bound matters because "voxen stop" is what a user reaches for when something is
// wrong; a stop that takes seconds on a broken machine is its own bug. The components
// are individually bounded, and this asserts their sum stays reasonable.
func TestShutdownIsBounded(t *testing.T) {
	prof := newInternalProfile(t)
	srv, err := New(context.Background(), Options{Paths: prof, Foreground: true, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Wait for the endpoint, then hold a client connection open: an idle connection is
	// exactly what used to extend shutdown.
	deadline := time.Now().Add(10 * time.Second)
	var conn interface{ Close() error }
	for time.Now().Before(deadline) {
		c, err := dialForTest(srv.cfg.Endpoint(srv.opts.Paths))
		if err == nil {
			conn = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("daemon did not start listening")
	}
	defer conn.Close()

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	elapsed := time.Since(start)
	t.Logf("shutdown took %s with a client held open", elapsed)
	if elapsed > 6*time.Second {
		t.Fatalf("shutdown took %s; the components are not bounded", elapsed)
	}
}

// deafEngine ignores its context entirely, which is what a wedged audio driver does.
// It is defined here rather than in a throwaway test so the bound below stays covered.
type deafEngine struct{ audio.Null }

func (deafEngine) Version() string { return "miniaudio (deaf)" }
func (deafEngine) Devices(context.Context) ([]audio.Device, error) {
	return []audio.Device{{ID: "x", Name: "x", IsInput: true, IsOutput: true}}, nil
}

func (deafEngine) OpenCapture(context.Context, audio.DeviceID) (audio.CaptureStream, error) {
	// Deliberately ignores cancellation and then returns, so the test does not leak a
	// goroutine for the rest of the run.
	time.Sleep(30 * time.Second)
	return nil, audio.ErrUnavailable
}

// TestShutdownSurvivesAnEngineThatIgnoresCancellation proves a driver that does not
// honour cancellation cannot hold shutdown open.
//
// Leaking the bridge is acceptable; hanging the process is not. This is the shape of
// the intermittent Windows failure: the open could not be interrupted, and shutdown
// waited for it unconditionally.
func TestShutdownSurvivesAnEngineThatIgnoresCancellation(t *testing.T) {
	prof := newInternalProfile(t)
	srv, err := New(context.Background(), Options{Paths: prof, Foreground: true, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	srv.audio = deafEngine{}

	controller := &Controller{log: quietLogger()}
	bridge := NewAudioBridge(srv.audio, quietLogger(), AudioBridgeConfig{},
		func() []media.ActivePeer { return nil })
	go srv.openAudioAsync(bridge, controller)

	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	srv.cleanup()
	elapsed := time.Since(start)
	t.Logf("cleanup took %s", elapsed)
	if elapsed > 4*time.Second {
		t.Fatalf("cleanup waited %s on a driver that ignores cancellation", elapsed)
	}
}
