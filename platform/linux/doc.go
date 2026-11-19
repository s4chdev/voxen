// Package linux will hold the Linux audio backend.
//
// Phase 2 implements it against PipeWire through the PulseAudio compatibility
// layer, with a plain PulseAudio fallback. The backend is selected at runtime;
// voxen never shells out to pactl, pw-cli or any other command-line tool as a
// production audio engine.
//
// The package is intentionally empty in phase 1. It exists so the module layout
// is stable and so the build tags for later phases have a home. A file with only
// a build constraint and no declarations is legal Go and keeps `go build ./...`
// honest for every target.
package linux

// Backend returns the audio backend identifier implemented by this package.
// It is a compile-time constant so a phase-1 build cannot claim audio support.
const Backend = "pipewire-pulseaudio"

// Implemented reports whether this package contains a working backend.
const Implemented = false
