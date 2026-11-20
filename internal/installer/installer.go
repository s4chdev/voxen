// Package installer places the voxen binaries on a machine without a package manager.
//
// It exists because a release archive is the lowest common denominator across three
// platforms: no distro packaging, no Homebrew formula, no winget manifest. The
// installer copies binaries next to each other, because the daemon and the CLI must be
// able to find one another, and it never edits shell profiles or paths the user cannot
// see and undo.
package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Binary names.
const (
	cliName    = "voxen"
	daemonName = "voxend"
	signalName = "voxen-signal"
)

// Options configures an install.
type Options struct {
	// Source is the directory containing the binaries to install.
	Source string
	// BinDir is the destination directory.
	BinDir string
	// Prefix is the installation prefix, when BinDir is not given.
	Prefix string
	// Force overwrites existing files.
	Force bool
	// Purge removes user data as well as binaries (uninstall only).
	Purge bool
	// DataDirs are the user data directories an uninstall may keep or purge.
	DataDirs []string
}

// Result describes what an install changed.
type Result struct {
	BinDir           string   `json:"bin_dir"`
	Installed        []string `json:"installed"`
	Version          string   `json:"version,omitempty"`
	NeedsPathUpdate  bool     `json:"needs_path_update"`
	PathHint         string   `json:"path_hint,omitempty"`
	ReplacedPrevious bool     `json:"replaced_previous"`
}

// UninstallResult describes what an uninstall removed.
type UninstallResult struct {
	Removed []string `json:"removed"`
	Kept    []string `json:"kept"`
}

// AlreadyInstalledError reports that a binary is already present and Force was not set.
//
// It is a distinct type so a caller can choose to treat it as a no-op rather than a
// failure, which is what "install --service" on an already-installed machine needs.
type AlreadyInstalledError struct {
	Path string
	Dir  string
}

func (e *AlreadyInstalledError) Error() string {
	return fmt.Sprintf("installer: %s already exists; re-run with --force to replace it (this is an upgrade)", e.Path)
}

// NeedsPrivilegeError reports that the destination requires higher privileges, with a
// suggestion the caller can print.
type NeedsPrivilegeError struct {
	Path string
	Err  error
}

func (e *NeedsPrivilegeError) Error() string {
	return fmt.Sprintf("cannot write to %s: %v", e.Path, e.Err)
}

func (e *NeedsPrivilegeError) Unwrap() error { return e.Err }

// BinaryName returns the CLI binary name for this platform.
func BinaryName() string { return exeName(cliName) }

// DaemonBinaryName returns the daemon binary name for this platform.
func DaemonBinaryName() string { return exeName(daemonName) }

// SignalBinaryName returns the signaling server binary name for this platform.
func SignalBinaryName() string { return exeName(signalName) }

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// UserPrefix is the per-user install location, which never needs root.
func UserPrefix() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "voxen")
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, "Programs", "voxen")
		}
		return filepath.Join(home, "AppData", "Local", "Programs", "voxen")
	}
	return filepath.Join(home, ".local")
}

// SystemPrefix is the machine-wide install location.
func SystemPrefix() string {
	if runtime.GOOS == "windows" {
		return UserPrefix()
	}
	return "/usr/local"
}

// DefaultBinDir resolves the destination for a prefix.
//
// An empty prefix means: the system location when this process can write to it,
// otherwise the per-user location. That is what a user expects from "install" without
// arguments, and it avoids the common failure of a bare install requiring sudo.
func DefaultBinDir(prefix string) string {
	if prefix != "" {
		if isBinDir(prefix) {
			return prefix
		}
		return filepath.Join(prefix, "bin")
	}
	system := filepath.Join(SystemPrefix(), "bin")
	if writable(system) {
		return system
	}
	return filepath.Join(UserPrefix(), "bin")
}

// isBinDir reports whether a path already names a bin directory, so
// "--prefix /usr/local/bin" behaves as the user meant.
func isBinDir(p string) bool {
	return filepath.Base(filepath.Clean(p)) == "bin"
}

// writable reports whether this process can create files in dir, creating it if
// needed. It answers by trying, because directory permissions can come from ACLs,
// groups or a read-only mount that os.Stat cannot summarise.
func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe, err := os.CreateTemp(dir, ".voxen-write-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)
	return true
}

// Install copies the binaries into BinDir.
func Install(opts Options) (Result, error) {
	var res Result
	if opts.Source == "" {
		return res, errors.New("installer: source directory is required")
	}
	binDir := opts.BinDir
	if binDir == "" {
		binDir = DefaultBinDir(opts.Prefix)
	}
	res.BinDir = binDir

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		if isPermissionErr(err) {
			return res, &NeedsPrivilegeError{Path: binDir, Err: err}
		}
		return res, fmt.Errorf("installer: create %s: %w", binDir, err)
	}

	// The daemon and the CLI must live together: serviceOptions looks for the daemon
	// beside the CLI, and a split install produces a supervisor that cannot find its
	// binary.
	for _, name := range []string{BinaryName(), DaemonBinaryName(), SignalBinaryName()} {
		src := filepath.Join(opts.Source, name)
		if _, err := os.Stat(src); err != nil {
			if name == SignalBinaryName() {
				// The signaling server is optional: a user joining someone else's
				// server does not need it.
				continue
			}
			return res, fmt.Errorf("installer: %s not found in %s (extract the whole release archive)", name, opts.Source)
		}
		dst := filepath.Join(binDir, name)

		existed := false
		if _, err := os.Stat(dst); err == nil {
			existed = true
			if !opts.Force {
				// A different version requires --force so an accidental overwrite is
				// visible, but the error is typed so a caller can ignore it.
				return res, &AlreadyInstalledError{Path: dst, Dir: binDir}
			}
		}

		if err := copyExecutable(src, dst); err != nil {
			if isPermissionErr(err) {
				return res, &NeedsPrivilegeError{Path: dst, Err: err}
			}
			return res, fmt.Errorf("installer: install %s: %w", dst, err)
		}
		res.Installed = append(res.Installed, dst)
		res.ReplacedPrevious = res.ReplacedPrevious || existed
	}

	res.NeedsPathUpdate = !onPath(binDir)
	if res.NeedsPathUpdate {
		res.PathHint = pathHint(binDir)
	}
	return res, nil
}

// copyExecutable writes src to dst through a temporary file in the destination
// directory, so a failed copy cannot leave a truncated binary that looks installed.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".voxen-install-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := io.Copy(tmp, in); err != nil {
		cleanup()
		return err
	}
	// Executable for everyone: these are system-wide binaries, not secrets.
	if err := tmp.Chmod(0o755); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// onPath reports whether dir is already in PATH, so the installer only mentions PATH
// when it genuinely needs to.
func onPath(dir string) bool {
	clean := filepath.Clean(dir)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		if filepath.Clean(entry) == clean {
			return true
		}
	}
	return false
}

// pathHint returns the exact line a user should add, for their shell.
func pathHint(binDir string) string {
	switch runtime.GOOS {
	case "windows":
		return fmt.Sprintf(`setx PATH "%%PATH%%;%s"`, binDir)
	case "darwin":
		return fmt.Sprintf(`echo 'export PATH="%s:$PATH"' >> ~/.zshrc`, binDir)
	default:
		return fmt.Sprintf(`echo 'export PATH="%s:$PATH"' >> ~/.bashrc`, binDir)
	}
}

// Uninstall removes the binaries, and optionally the user data.
func Uninstall(opts Options) (UninstallResult, error) {
	var res UninstallResult
	binDir := opts.BinDir
	if binDir == "" {
		binDir = DefaultBinDir(opts.Prefix)
	}

	for _, name := range []string{BinaryName(), DaemonBinaryName(), SignalBinaryName()} {
		path := filepath.Join(binDir, name)
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if isPermissionErr(err) {
				return res, &NeedsPrivilegeError{Path: path, Err: err}
			}
			return res, fmt.Errorf("installer: remove %s: %w", path, err)
		}
		res.Removed = append(res.Removed, path)
	}

	for _, dir := range opts.DataDirs {
		if dir == "" {
			continue
		}
		if opts.Purge {
			if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
				return res, fmt.Errorf("installer: purge %s: %w", dir, err)
			}
			res.Removed = append(res.Removed, dir)
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			res.Kept = append(res.Kept, dir)
		}
	}
	return res, nil
}

// isPermissionErr recognises a privilege failure through wrapping.
//
// os.MkdirAll and os.Create return a *PathError; a caller that wraps it further would
// otherwise hide the fact that the fix is "run with sudo", which is the one hint that
// makes this failure actionable.
func isPermissionErr(err error) bool {
	for err != nil {
		if os.IsPermission(err) {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
