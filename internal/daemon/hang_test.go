package daemon_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/daemon"
	"github.com/s4chdev/voxen/internal/media"
)

// failingEngine opens slowly then fails, which is what a Windows runner without an
// audio service does. The daemon must still shut down promptly.
type failingEngine struct {
	audio.Null
	delay time.Duration
}

func (f failingEngine) Version() string { return "miniaudio (failing)" }
func (f failingEngine) Devices(context.Context) ([]audio.Device, error) {
	return []audio.Device{{ID: "x", Name: "x", IsInput: true, IsOutput: true}}, nil
}
func (f failingEngine) OpenCapture(ctx context.Context, _ audio.DeviceID) (audio.CaptureStream, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, errors.New("miniaudio: Unknown error")
}

// TestStopDuringSlowStartDoesNotHang covers the bug that made a Windows CI run
// hang: the bridge was published before its devices finished opening, so Stop ran
// against a half-started bridge and the daemon never shut down.
func TestStopDuringSlowStartDoesNotHang(t *testing.T) {
	engine := failingEngine{delay: 300 * time.Millisecond}
	peers := func() []media.ActivePeer { return nil }
	b := daemon.NewAudioBridge(engine, discardLogger(), daemon.AudioBridgeConfig{}, peers)

	startDone := make(chan error, 1)
	go func() { startDone <- b.Start(context.Background()) }()

	// Stop while the device is still opening.
	time.Sleep(50 * time.Millisecond)
	stopDone := make(chan struct{})
	go func() { b.Stop(); close(stopDone) }()

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung while Start was still opening a device")
	}
	select {
	case err := <-startDone:
		if err == nil {
			t.Fatal("Start reported success with a failing device")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start hung after Stop")
	}

	// A second Stop must be a no-op, not a panic or a block.
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second Stop hung")
	}
}

// TestStartAfterStopReportsClosed proves a stopped bridge cannot be restarted
// silently, which would leave two sets of loops running.
func TestStartAfterStopReportsClosed(t *testing.T) {
	b := daemon.NewAudioBridge(failingEngine{}, discardLogger(), daemon.AudioBridgeConfig{},
		func() []media.ActivePeer { return nil })
	b.Stop()
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("a stopped bridge restarted")
	}
}

var _ = sync.Mutex{}

// hungEngine blocks forever opening a device, which is what a platform with a
// wedged or absent audio service can do.
type hungEngine struct{ audio.Null }

func (hungEngine) Version() string { return "miniaudio (hung)" }
func (hungEngine) Devices(context.Context) ([]audio.Device, error) {
	return []audio.Device{{ID: "x", Name: "x", IsInput: true, IsOutput: true}}, nil
}
func (hungEngine) OpenCapture(ctx context.Context, _ audio.DeviceID) (audio.CaptureStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hungEngine) OpenPlayback(ctx context.Context, _ audio.DeviceID) (audio.PlaybackStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStopWhileDeviceOpenHangsDoesNotBlock proves shutdown survives a wedged audio
// driver. This is the failure that hung a Windows CI run: the daemon could not exit
// because audio teardown waited on a device open that never returned.
func TestStopWhileDeviceOpenHangsDoesNotBlock(t *testing.T) {
	b := daemon.NewAudioBridge(hungEngine{}, discardLogger(), daemon.AudioBridgeConfig{},
		func() []media.ActivePeer { return nil })

	startDone := make(chan error, 1)
	go func() { startDone <- b.Start(context.Background()) }()
	time.Sleep(50 * time.Millisecond)

	stopped := make(chan struct{})
	go func() { b.Stop(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on a hung device open")
	}
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start never returned after Stop")
	}
}
