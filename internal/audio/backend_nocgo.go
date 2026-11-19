//go:build !cgo

package audio

// Without cgo there is no miniaudio backend. Returning an explicit error keeps
// the binary buildable on a machine with no C toolchain, which is what makes
// `make cross` work for every target; the daemon then reports audio as
// unavailable rather than failing to start.

import (
	"context"
	"fmt"
	"log/slog"
)

// NewMiniAudio reports that the cgo backend is not compiled in.
func NewMiniAudio(*slog.Logger) (*MiniAudio, error) {
	return nil, fmt.Errorf("%w: this build was compiled without cgo", ErrUnavailable)
}

// MiniAudio is a stub so the daemon can refer to the type on every platform.
type MiniAudio struct{}

// Version implements Engine.
func (*MiniAudio) Version() string { return "unavailable (no cgo)" }

// Devices implements Enumerator.
func (*MiniAudio) Devices(context.Context) ([]Device, error) { return nil, ErrUnavailable }

// Watch implements Enumerator.
func (*MiniAudio) Watch(context.Context) (<-chan struct{}, error) { return nil, ErrUnavailable }

// OpenCapture implements Engine.
func (*MiniAudio) OpenCapture(context.Context, DeviceID) (CaptureStream, error) {
	return nil, ErrUnavailable
}

// OpenPlayback implements Engine.
func (*MiniAudio) OpenPlayback(context.Context, DeviceID) (PlaybackStream, error) {
	return nil, ErrUnavailable
}

// Close implements Engine.
func (*MiniAudio) Close() error { return nil }

var _ Engine = (*MiniAudio)(nil)
