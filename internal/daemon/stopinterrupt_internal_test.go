package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/media"
)

// blockEngine blocks a device open until its context is cancelled, which is what a
// platform with no audio service does.
type blockEngine struct{ audio.Null }

func (blockEngine) Version() string { return "miniaudio (blocked)" }
func (blockEngine) Devices(ctx context.Context) ([]audio.Device, error) {
	return []audio.Device{{ID: "x", Name: "x", IsInput: true, IsOutput: true}}, nil
}
func (blockEngine) OpenCapture(ctx context.Context, _ audio.DeviceID) (audio.CaptureStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestShutdownInterruptsBlockedAudioOpen proves a stop request cancels a wedged
// device open instead of waiting for its timeout. On Windows CI this is the
// difference between "voxen stop" returning and the daemon hanging for seconds.
func TestShutdownInterruptsBlockedAudioOpen(t *testing.T) {
	srv := &Server{log: quietLogger()}
	srv.audio = blockEngine{}
	srv.stopCtx, srv.stopCancel = context.WithCancel(context.Background())

	controller := &Controller{log: quietLogger()}
	bridge := NewAudioBridge(srv.audio, quietLogger(), AudioBridgeConfig{}, func() []media.ActivePeer { return nil })

	done := make(chan struct{})
	go func() {
		srv.openAudioAsync(bridge, controller)
		close(done)
	}()

	// Let the open reach the blocked call.
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	srv.stopCancel()
	srv.audioPending.Wait()
	elapsed := time.Since(start)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not interrupt the blocked audio open")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("shutdown waited %s for a blocked open; it should interrupt it", elapsed)
	}
}
