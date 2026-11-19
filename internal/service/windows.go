package service

import (
	"errors"
	"fmt"
	"strings"
)

// windowsManager registers voxend as a per-user logon task.
//
// A Windows *service* would run in session 0 with no audio devices, which is the
// same problem as a systemd system unit. The supported mechanism for a per-user
// background app that needs the user's audio is a logon task created with
// schtasks, which runs in the interactive session.
type windowsManager struct{}

const windowsTaskName = "voxen-daemon"

func (m *windowsManager) Name() string { return "Task Scheduler (per-user logon task)" }

func (m *windowsManager) Install(opts Options) error {
	if opts.Binary == "" || opts.Config == "" {
		return errors.New("service: binary and config paths are required")
	}
	// schtasks cannot set environment variables for a task, so a profile selected
	// through VOXEN_* is not supported here. The task runs the default layout,
	// which is what a normal installation uses. This is reported by
	// "voxen service status" rather than silently ignored.
	// /RL LIMITED keeps the task at the user's privileges; it needs none higher.
	args := []string{
		"/Create", "/F",
		"/SC", "ONLOGON",
		"/TN", windowsTaskName,
		"/RL", "LIMITED",
		"/TR", fmt.Sprintf(`"%s" --foreground --config "%s"`, opts.Binary, opts.Config),
	}
	if _, err := runCommand("schtasks", args...); err != nil {
		return fmt.Errorf("service: create logon task: %w", err)
	}
	// Starting immediately makes `voxen service install` behave like the other
	// platforms, where installation also brings the daemon up.
	_, _ = runCommand("schtasks", "/Run", "/TN", windowsTaskName)
	return nil
}

func (m *windowsManager) Uninstall(opts Options) error {
	_, _ = runCommand("schtasks", "/End", "/TN", windowsTaskName)
	if _, err := runCommand("schtasks", "/Delete", "/F", "/TN", windowsTaskName); err != nil {
		if strings.Contains(err.Error(), "cannot find") {
			return nil
		}
		return err
	}
	return nil
}

func (m *windowsManager) Status(opts Options) (Status, error) {
	st := Status{Supervisor: m.Name()}
	out, err := runCommand("schtasks", "/Query", "/TN", windowsTaskName, "/FO", "LIST")
	if err != nil {
		return st, nil
	}
	st.Installed = true
	st.Enabled = true
	st.Running = strings.Contains(out, "Running")
	st.Detail = windowsTaskName
	return st, nil
}

var _ Manager = (*windowsManager)(nil)
