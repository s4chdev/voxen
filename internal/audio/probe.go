package audio

import (
	"context"
	"errors"
	"time"
)

// ProbeResult reports whether this host can actually use audio.
//
// Enumeration is not sufficient evidence: a container or CI runner often lists
// phantom devices (an ALSA "default" that maps to no card) which enumerate
// successfully and then fail to open. Only opening the device proves it works, so
// that is what this checks.
type ProbeResult struct {
	// Backend is the engine version string, or "unavailable".
	Backend string
	// CaptureOK reports that a microphone stream opened and started.
	CaptureOK bool
	// PlaybackOK reports that a speaker stream opened and started.
	PlaybackOK bool
	// Permission reports that the failure was a denied microphone rather than
	// absent hardware, which is actionable for the user.
	Permission bool
	// Devices is the number of enumerated devices, for diagnostics.
	Devices int
	// Err is the first failure encountered, if any.
	Err error
}

// Usable reports whether both directions work.
func (r ProbeResult) Usable() bool { return r.CaptureOK && r.PlaybackOK }

// Probe opens a capture and a playback stream briefly and closes them.
//
// It is deliberately time-bounded: a wedged audio server must not hang a
// diagnostic command.
func Probe(ctx context.Context, engine Engine) ProbeResult {
	res := ProbeResult{Backend: "unavailable"}
	if engine == nil {
		return res
	}
	if _, ok := engine.(Null); ok {
		return res
	}
	res.Backend = engine.Version()

	if devices, err := engine.Devices(ctx); err == nil {
		res.Devices = len(devices)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if stream, err := engine.OpenCapture(probeCtx, ""); err != nil {
		res.Err = err
		if errors.Is(err, ErrPermission) {
			res.Permission = true
		}
	} else {
		res.CaptureOK = true
		_ = stream.Close()
	}

	if stream, err := engine.OpenPlayback(probeCtx, ""); err != nil {
		if res.Err == nil {
			res.Err = err
		}
	} else {
		res.PlaybackOK = true
		_ = stream.Close()
	}
	return res
}
