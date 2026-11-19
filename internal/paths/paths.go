// Package paths resolves the on-disk locations used by voxen.
//
// The layout follows each platform's conventions:
//
//	Linux   $XDG_CONFIG_HOME/voxen, $XDG_STATE_HOME/voxen, $XDG_RUNTIME_DIR/voxen
//	macOS   ~/Library/Application Support/voxen, ~/Library/Logs/voxen
//	Windows %AppData%\voxen, %LocalAppData%\voxen
//
// Every directory created by Ensure is private to the current user (0700 on
// Unix). The daemon socket is private as well: 0600 on Unix, and an ACL
// restricted to the current user, SYSTEM and Administrators on Windows.
package paths

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
)

// AppName is the directory / pipe namespace used on disk.
const AppName = "voxen"

// File names inside the resolved directories.
const (
	ConfigFileName   = "config.yaml"
	IdentityFileName = "identity.json"
	LogFileName      = "voxend.log"
	PIDFileName      = "voxend.pid"
	SocketFileName   = "voxend.sock"
)

// Paths is the fully resolved, absolute location set for one voxen profile.
// A profile is selected purely through environment overrides, which allows
// running several isolated daemons on one machine.
type Paths struct {
	ConfigDir    string
	StateDir     string
	RuntimeDir   string
	LogDir       string
	ConfigFile   string
	IdentityFile string
	SocketPath   string
	LogFile      string
	PIDFile      string
}

// Resolve computes the effective paths for the current user and platform.
//
// Recognised environment overrides (all optional):
//
//	VOXEN_CONFIG       absolute path to config.yaml
//	VOXEN_CONFIG_DIR   base config directory
//	VOXEN_STATE_DIR    base state directory (logs live below it)
//	VOXEN_RUNTIME_DIR  directory for the Unix socket
//	VOXEN_SOCKET       explicit IPC endpoint (socket path or Windows pipe name)
func Resolve() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("paths: cannot determine home directory: %w", err)
	}

	var p Paths
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		p.ConfigDir = filepath.Join(appData, AppName)
		p.StateDir = filepath.Join(localAppData, AppName)
		p.LogDir = filepath.Join(p.StateDir, "logs")
		p.RuntimeDir = p.StateDir

	case "darwin":
		// macOS keeps per-user application data in Application Support.
		// The Unix socket cannot live there reliably in every sandbox, so the
		// runtime directory defaults to the per-user temp directory.
		p.ConfigDir = filepath.Join(home, "Library", "Application Support", AppName)
		p.StateDir = filepath.Join(p.ConfigDir, "state")
		p.LogDir = filepath.Join(home, "Library", "Logs", AppName)
		p.RuntimeDir = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", AppName, userTag()))

	default:
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			stateHome = filepath.Join(home, ".local", "state")
		}
		runtimeHome := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeHome == "" {
			runtimeHome = filepath.Join(stateHome, AppName, "run")
		}
		p.ConfigDir = filepath.Join(configHome, AppName)
		p.StateDir = filepath.Join(stateHome, AppName)
		p.LogDir = filepath.Join(p.StateDir, "logs")
		p.RuntimeDir = filepath.Join(runtimeHome, AppName)
	}

	if v := os.Getenv("VOXEN_CONFIG_DIR"); v != "" {
		p.ConfigDir = v
		p.StateDir = filepath.Join(v, "state")
		p.LogDir = filepath.Join(p.StateDir, "logs")
	}
	if v := os.Getenv("VOXEN_STATE_DIR"); v != "" {
		p.StateDir = v
		p.LogDir = filepath.Join(v, "logs")
	}
	if v := os.Getenv("VOXEN_RUNTIME_DIR"); v != "" {
		p.RuntimeDir = v
	}

	// Windows has no filesystem sockets; the runtime directory is unused.
	if runtime.GOOS == "windows" {
		p.RuntimeDir = p.StateDir
	}

	p.ConfigFile = filepath.Join(p.ConfigDir, ConfigFileName)
	p.IdentityFile = filepath.Join(p.ConfigDir, IdentityFileName)
	p.LogFile = filepath.Join(p.LogDir, LogFileName)
	p.PIDFile = filepath.Join(p.StateDir, PIDFileName)

	if v := os.Getenv("VOXEN_CONFIG"); v != "" {
		p.ConfigFile = v
	}
	if v := os.Getenv("VOXEN_SOCKET"); v != "" {
		p.SocketPath = v
	} else {
		p.SocketPath = defaultEndpoint(p)
	}

	for name, v := range map[string]*string{
		"VOXEN_CONFIG":      &p.ConfigFile,
		"VOXEN_CONFIG_DIR":  &p.ConfigDir,
		"VOXEN_STATE_DIR":   &p.StateDir,
		"VOXEN_RUNTIME_DIR": &p.RuntimeDir,
		"VOXEN_SOCKET":      &p.SocketPath,
	} {
		abs, err := filepath.Abs(*v)
		if err != nil {
			return Paths{}, fmt.Errorf("paths: resolving %s: %w", name, err)
		}
		*v = filepath.Clean(abs)
	}

	// Recompute derived files after the absolute-path normalisation so a
	// relative VOXEN_CONFIG_DIR cannot leave derived paths relative as well.
	p.IdentityFile = filepath.Join(p.ConfigDir, IdentityFileName)
	p.LogFile = filepath.Join(p.LogDir, LogFileName)
	p.PIDFile = filepath.Join(p.StateDir, PIDFileName)
	return p, nil
}

func defaultEndpoint(p Paths) string {
	if runtime.GOOS == "windows" {
		return PipeName(p.ConfigDir)
	}
	endpoint := filepath.Join(p.RuntimeDir, SocketFileName)
	if len(endpoint) <= maxUnixSocketPath {
		return endpoint
	}
	// Unix domain sockets are limited to ~104 bytes on macOS and 108 on Linux.
	// Fall back to a short per-user path rather than failing at listen time.
	shortDir := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", AppName, userTag()))
	return filepath.Join(shortDir, SocketFileName)
}

// PipeName derives a stable named-pipe name for a profile. Hashing the config
// directory keeps concurrently installed profiles from colliding.
func PipeName(configDir string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(configDir)))
	return fmt.Sprintf(`\\.\pipe\%s-%08x`, AppName, h.Sum32())
}

const maxUnixSocketPath = 100

// Dir returns the directory that must contain the IPC endpoint.
func (p Paths) Dir() string {
	if runtime.GOOS == "windows" {
		return p.StateDir
	}
	return filepath.Dir(p.SocketPath)
}

// Ensure creates every directory in the layout with private permissions and
// verifies they are usable. It is safe to call repeatedly.
func (p Paths) Ensure() error {
	dirs := []string{p.ConfigDir, p.StateDir, p.LogDir}
	if runtime.GOOS != "windows" {
		dirs = append(dirs, p.Dir())
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("paths: creating %s: %w", dir, err)
		}
	}
	return nil
}

// CheckPermissions reports unsafe permissions on a private file or directory.
// It is a no-op on Windows, where ACLs carry the security decision instead of
// POSIX mode bits.
func CheckPermissions(path string, want os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 && want&0o077 == 0 {
		return fmt.Errorf("%s has permissions %04o, expected %04o or stricter", path, info.Mode().Perm(), want)
	}
	return nil
}
