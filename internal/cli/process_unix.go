//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func osFindProcess(pid int) (*os.Process, error) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil, err
	}
	// FindProcess always succeeds on Unix; probe liveness explicitly.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return nil, err
	}
	return proc, nil
}
