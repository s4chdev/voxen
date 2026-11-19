//go:build windows

package cli

import "os"

func osFindProcess(pid int) (*os.Process, error) { return os.FindProcess(pid) }
