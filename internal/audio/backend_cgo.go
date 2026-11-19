//go:build cgo

package audio

// This file exists only to make the cgo requirement explicit: the miniaudio
// backend is compiled when cgo is available.

import "github.com/gen2brain/malgo"

// checkBackend is a compile-time assertion that the malgo API used by
// miniaudio.go exists. If a future malgo release changes these symbols the build
// fails here with a clear message rather than deep inside a callback.
var _ = []malgo.DeviceType{malgo.Capture, malgo.Playback}
