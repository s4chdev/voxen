//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts the child in its own session so it survives the CLI exiting and
// terminal hangup. The child is not a job-control member of the shell.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killProcess terminates a process by PID.
func killProcess(pid int) error {
	proc, err := osFindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
