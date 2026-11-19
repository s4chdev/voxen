// Package service installs and removes the user-level supervision unit that keeps
// voxend running.
//
// The unit is generated rather than shipped as a template because the correct
// content depends on the installing user's paths and, on macOS, their username.
// The generated files are the ones tested in packaging/ for reference, but the
// installed copies are always derived from the live environment.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Manager supervises the daemon on this platform.
type Manager interface {
	// Name identifies the supervisor, for diagnostics.
	Name() string
	// Install writes the unit and enables it.
	Install(opts Options) error
	// Uninstall stops and removes the unit.
	Uninstall(opts Options) error
	// Status reports whether the unit is installed and whether it is running.
	Status(opts Options) (Status, error)
}

// Options describes the installation being managed.
type Options struct {
	// Binary is the absolute path to voxend.
	Binary string
	// Config is the absolute path to config.yaml.
	Config string
	// Env is the profile environment the daemon must run with.
	//
	// The supervisor does not inherit the shell, so a profile selected through
	// VOXEN_CONFIG_DIR/VOXEN_STATE_DIR/VOXEN_RUNTIME_DIR would otherwise resolve to
	// the default layout. The daemon would then listen on a different socket from
	// the one the CLI dials, which looks like "the service is running but voxen
	// says it is not". These are baked into the unit so both always agree.
	Env []string
	// LogPath is used by supervisors that capture output to a file.
	LogPath string
}

// envLines renders environment assignments, dropping anything malformed.
func envLines(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if kv == "" || !strings.Contains(kv, "=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Status is the supervisor's view.
type Status struct {
	Supervisor string
	Installed  bool
	Enabled    bool
	Running    bool
	Detail     string
}

// ErrUnsupported is returned on platforms with no implemented supervisor.
var ErrUnsupported = errors.New("service: no supervisor implementation for this platform")

// ForPlatform returns the manager for the current OS.
func ForPlatform() (Manager, error) {
	switch runtime.GOOS {
	case "linux":
		return &systemdManager{}, nil
	case "darwin":
		return &launchdManager{}, nil
	case "windows":
		return &windowsManager{}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)
	}
}

// runCommand executes a supervisor command and returns its combined output. A
// non-zero exit is reported with the output, because supervisor errors are almost
// always explained in the text rather than the exit code.
func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

// writeFileAtomic writes a unit file with private permissions.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".unit-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// homeDir returns the current user's home directory.
func homeDir() (string, error) {
	return os.UserHomeDir()
}
