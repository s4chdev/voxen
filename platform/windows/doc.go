// Package windows will hold the Windows audio backend.
//
// Phase 2 implements it with WASAPI through miniaudio (github.com/gen2brain/malgo),
// including device enumeration, the default-device role handling and hotplug
// notifications.
//
// The package is intentionally empty in phase 1.
package windows

// Backend returns the audio backend identifier implemented by this package.
const Backend = "wasapi"

// Implemented reports whether this package contains a working backend.
const Implemented = false
