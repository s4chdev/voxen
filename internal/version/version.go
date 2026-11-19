// Package version exposes build metadata for voxen binaries.
//
// Version, Commit and BuildDate are intended to be overridden at link time:
//
//	go build -ldflags "-X github.com/s4chdev/voxen/internal/version.Version=0.1.0" \
//	    -o bin/voxen ./cmd/voxen
package version

import (
	"fmt"
	"runtime"
)

// These values are replaced by the Makefile / release pipeline. They must never
// contain secrets.
var (
	Version   = "0.1.0-dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Phase is the implementation phase currently shipped by this build. Anything
// the binary cannot actually do is reported as unavailable rather than faked.
const (
	// Phase 2 + 3 + 4 are delivered: real microphone and speaker audio over
	// authenticated two-peer and mesh sessions. Remaining work is supervision and
	// packaging; see docs/plan.md.
	Phase     = 4
	PhaseName = "group calls"
)

// String renders a single-line human readable version string.
func String() string {
	return fmt.Sprintf("%s (phase %d: %s, commit %s)", Version, Phase, PhaseName, Commit)
}

// Info is a machine readable description of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Phase     int    `json:"phase"`
	PhaseName string `json:"phase_name"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the build description for the current process.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		Phase:     Phase,
		PhaseName: PhaseName,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}
