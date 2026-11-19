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
	// Phase 3 is the current milestone: two-peer encrypted media is implemented
	// and tested. Some phase-2 (microphone/speaker) and phase-6 (group call)
	// work is still outstanding; see docs/plan.md for the exact gaps.
	Phase     = 3
	PhaseName = "p2p media"
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
