// Package darwin will hold the macOS audio backend.
//
// Phase 2 implements it with CoreAudio through miniaudio
// (github.com/gen2brain/malgo), plus the microphone permission preflight and the
// bundle usage description required by TCC.
//
// The package is intentionally empty in phase 1.
package darwin

// Backend returns the audio backend identifier implemented by this package.
const Backend = "coreaudio"

// Implemented reports whether this package contains a working backend.
const Implemented = false
