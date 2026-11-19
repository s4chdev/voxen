//go:build cgo

// Package audio provides the miniaudio-backed audio engine.
//
// miniaudio is compiled into the binary through github.com/gen2brain/malgo, which
// binds the platform's native API:
//
//	Linux   PipeWire / PulseAudio (or ALSA)
//	Windows WASAPI
//	macOS   CoreAudio
//
// The engine presents the single normalised format the rest of voxen assumes:
// 48 kHz mono signed 16-bit PCM. The device's native rate, channel count and
// sample format are converted here, so the media plane never needs to know which
// backend is in use.
//
// This file is build-tagged on cgo because malgo is cgo. Without cgo the package
// falls back to audio.Null (audio_nocgo.go) and "voxen doctor" reports audio as
// unavailable rather than failing to start.
package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// Engine errors.
var (
	ErrBackendInit = errors.New("audio: cannot initialise audio backend")
)

// MiniAudio is the cgo-backed engine.
type MiniAudio struct {
	ctx *malgo.AllocatedContext
	log *slog.Logger
	// closed is set by Close so no new device can start afterwards.
	closed atomic.Bool
}

// NewMiniAudio initialises the backend.
func NewMiniAudio(log *slog.Logger) (*MiniAudio, error) {
	if log == nil {
		log = slog.Default()
	}
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackendInit, err)
	}
	return &MiniAudio{ctx: ctx, log: log}, nil
}

// Version reports the backend and platform for diagnostics.
func (m *MiniAudio) Version() string {
	backend := map[string]string{
		"linux":   "pipewire/pulseaudio",
		"windows": "wasapi",
		"darwin":  "coreaudio",
	}[runtime.GOOS]
	if backend == "" {
		backend = runtime.GOOS
	}
	return "miniaudio (" + backend + ")"
}

// Devices implements Enumerator.
func (m *MiniAudio) Devices(ctx context.Context) ([]Device, error) {
	if m.closed.Load() {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []Device
	for _, kind := range []struct {
		t     malgo.DeviceType
		input bool
	}{{malgo.Capture, true}, {malgo.Playback, false}} {
		list, err := m.ctx.Devices(kind.t)
		if err != nil {
			// Playback may be unavailable on a headless machine while capture
			// works, and vice versa. Report what we have.
			m.log.Debug("device enumeration failed for one direction", "error", err)
			continue
		}
		for i := range list {
			d := &list[i]
			name := d.Name()
			out = append(out, Device{
				ID:         DeviceID(name),
				Name:       name,
				IsInput:    kind.input,
				IsOutput:   !kind.input,
				IsDefault:  d.IsDefault != 0,
				SampleRate: DefaultFormat.SampleRate,
				Channels:   DefaultFormat.Channels,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no audio devices found", ErrDeviceNotFound)
	}
	return out, nil
}

// Watch implements Enumerator by polling, because malgo's notification callback
// only signals "something changed" and the caller re-enumerates regardless.
func (m *MiniAudio) Watch(ctx context.Context) (<-chan struct{}, error) {
	if m.closed.Load() {
		return nil, ErrUnavailable
	}
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var last uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				devices, err := m.Devices(ctx)
				if err != nil {
					continue
				}
				h := hashDevices(devices)
				if last != 0 && h != last {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
				last = h
			}
		}
	}()
	return ch, nil
}

// hashDevices is a cheap fingerprint of the device list.
func hashDevices(devices []Device) uint64 {
	var h uint64 = 1469598103934665603
	for _, d := range devices {
		for i := 0; i < len(d.ID); i++ {
			h ^= uint64(d.ID[i])
			h *= 1099511628211
		}
		if d.IsDefault {
			h ^= 0xff
			h *= 1099511628211
		}
	}
	return h
}

// Close releases every device and the backend context.
func (m *MiniAudio) Close() error {
	if m.closed.Swap(true) {
		return nil
	}
	// Devices are owned by their streams, which call Uninit exactly once in
	// their own Close. Calling it again here would be a double free.
	if m.ctx != nil {
		_ = m.ctx.Uninit()
		m.ctx.Free()
	}
	return nil
}

// findDevice resolves a DeviceID to the malgo device ID pointer. An empty ID
// returns nil, which makes miniaudio choose the system default.
func (m *MiniAudio) findDevice(kind malgo.DeviceType, id DeviceID) (unsafe.Pointer, error) {
	if id == "" {
		return nil, nil
	}
	devices, err := m.ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("audio: enumerate devices: %w", err)
	}
	for i := range devices {
		if devices[i].Name() == string(id) {
			// The ID lives in the slice element, which is heap-allocated and
			// remains valid for the duration of the InitDevice call.
			return unsafe.Pointer(&devices[i].ID[0]), nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
}

// OpenCapture implements Engine.
func (m *MiniAudio) OpenCapture(ctx context.Context, device DeviceID) (CaptureStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.openCapture(device)
}

// OpenPlayback implements Engine.
func (m *MiniAudio) OpenPlayback(ctx context.Context, device DeviceID) (PlaybackStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.openPlayback(device)
}

// Compile-time interface checks.
var _ Engine = (*MiniAudio)(nil)
