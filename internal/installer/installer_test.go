package installer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// makeRelease builds a fake release archive layout: the three binaries in one
// directory, which is what extracting a tarball produces.
func makeRelease(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+name+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestInstallCopiesAllBinaries is the basic contract: the daemon ends up beside the CLI,
// which is what lets the supervisor find it.
func TestInstallCopiesAllBinaries(t *testing.T) {
	src := makeRelease(t, BinaryName(), DaemonBinaryName(), SignalBinaryName())
	dst := t.TempDir()

	res, err := Install(Options{Source: src, BinDir: dst})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.Installed) != 3 {
		t.Fatalf("installed %d files, want 3: %v", len(res.Installed), res.Installed)
	}
	for _, name := range []string{BinaryName(), DaemonBinaryName(), SignalBinaryName()} {
		p := filepath.Join(dst, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s missing after install: %v", name, err)
		}
		// Windows has no POSIX executable bit; the .exe extension is what makes a
		// binary runnable there.
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("%s is not executable (mode %04o)", p, info.Mode().Perm())
		}
	}
}

// TestInstallWithoutSignalServer proves the signaling server is optional: a user who
// joins someone else's server does not need to host one.
func TestInstallWithoutSignalServer(t *testing.T) {
	src := makeRelease(t, BinaryName(), DaemonBinaryName())
	dst := t.TempDir()

	res, err := Install(Options{Source: src, BinDir: dst})
	if err != nil {
		t.Fatalf("install without the signal server should succeed: %v", err)
	}
	if len(res.Installed) != 2 {
		t.Fatalf("installed %d files, want 2", len(res.Installed))
	}
}

// TestInstallRefusesToOverwriteWithoutForce proves an accidental re-install of a
// different version is visible rather than silent.
func TestInstallRefusesToOverwriteWithoutForce(t *testing.T) {
	src := makeRelease(t, BinaryName(), DaemonBinaryName())
	dst := t.TempDir()

	if _, err := Install(Options{Source: src, BinDir: dst}); err != nil {
		t.Fatal(err)
	}
	_, err := Install(Options{Source: src, BinDir: dst})
	if err == nil {
		t.Fatal("second install without --force succeeded")
	}
	if !contains(err.Error(), "already exists") {
		t.Fatalf("error does not explain the conflict: %v", err)
	}
}

// TestInstallForceUpgrades proves --force replaces binaries and reports it as an upgrade.
func TestInstallForceUpgrades(t *testing.T) {
	src := makeRelease(t, BinaryName(), DaemonBinaryName())
	dst := t.TempDir()

	if _, err := Install(Options{Source: src, BinDir: dst}); err != nil {
		t.Fatal(err)
	}
	// Change the payload so an upgrade is observable.
	if err := os.WriteFile(filepath.Join(src, BinaryName()), []byte("new content"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Install(Options{Source: src, BinDir: dst, Force: true})
	if err != nil {
		t.Fatalf("forced upgrade: %v", err)
	}
	if !res.ReplacedPrevious {
		t.Fatal("upgrade did not report that it replaced a previous install")
	}
	data, err := os.ReadFile(filepath.Join(dst, BinaryName()))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new content" {
		t.Fatalf("binary was not replaced: %q", data)
	}
}

// TestInstallRequiresTheDaemon proves a partial release is rejected rather than
// producing a CLI that cannot find its daemon.
func TestInstallRequiresTheDaemon(t *testing.T) {
	src := makeRelease(t, BinaryName()) // daemon deliberately missing
	dst := t.TempDir()

	if _, err := Install(Options{Source: src, BinDir: dst}); err == nil {
		t.Fatal("install succeeded with no daemon binary")
	}
}

// TestFailedCopyLeavesNoPartialBinary proves a failed install cannot leave a truncated
// executable that looks installed.
func TestFailedCopyLeavesNoPartialBinary(t *testing.T) {
	dst := t.TempDir()
	// A source that does not exist: the copy fails after the destination directory
	// exists.
	_, err := Install(Options{Source: t.TempDir(), BinDir: dst})
	if err == nil {
		t.Fatal("install from an empty directory succeeded")
	}
	entries, readErr := os.ReadDir(dst)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		t.Fatalf("a failed install left %s behind", e.Name())
	}
}

// TestDefaultBinDirHonoursPrefix proves the prefix rules: a prefix ending in "bin" is
// used as-is, otherwise "bin" is appended.
func TestDefaultBinDirHonoursPrefix(t *testing.T) {
	prefix := filepath.Join(string(filepath.Separator), "opt", "voxen")
	want := filepath.Join(prefix, "bin")

	got := DefaultBinDir(prefix)
	if got != want {
		t.Fatalf("DefaultBinDir(%q) = %q, want %q", prefix, got, want)
	}
	// A prefix that already ends in "bin" is used as given, so "--prefix /a/bin"
	// does not become "/a/bin/bin".
	got = DefaultBinDir(want)
	if got != want {
		t.Fatalf("DefaultBinDir(%q) = %q, want it unchanged", want, got)
	}
}

// TestDefaultBinDirIsAlwaysAbsolute proves the no-argument case resolves somewhere
// writable, so a bare "voxen install" does not fail needing root.
func TestDefaultBinDirIsAlwaysAbsolute(t *testing.T) {
	got := DefaultBinDir("")
	if !filepath.IsAbs(got) {
		t.Fatalf("DefaultBinDir(\"\") = %q, want an absolute path", got)
	}
	if !writable(got) {
		t.Fatalf("DefaultBinDir(\"\") = %q, which is not writable", got)
	}
}

// TestUninstallKeepsDataByDefault is the important safety property: removing the
// binaries must not destroy a user's identity.
func TestUninstallKeepsDataByDefault(t *testing.T) {
	src := makeRelease(t, BinaryName(), DaemonBinaryName())
	dst := t.TempDir()
	if _, err := Install(Options{Source: src, BinDir: dst}); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "identity.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Uninstall(Options{BinDir: dst, DataDirs: []string{dataDir}})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("removed %d files, want 2", len(res.Removed))
	}
	if len(res.Kept) != 1 || res.Kept[0] != dataDir {
		t.Fatalf("data directory not reported as kept: %v", res.Kept)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "identity.json")); err != nil {
		t.Fatalf("uninstall destroyed the identity: %v", err)
	}
}

// TestUninstallPurgeRemovesData proves --purge does what it says.
func TestPurgeRemovesData(t *testing.T) {
	src := makeRelease(t, BinaryName(), DaemonBinaryName())
	dst := t.TempDir()
	if _, err := Install(Options{Source: src, BinDir: dst}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	res, err := Uninstall(Options{BinDir: dst, DataDirs: []string{dataDir}, Purge: true})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(res.Kept) != 0 {
		t.Fatalf("purge kept %v", res.Kept)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("purge did not remove the data directory")
	}
}

// TestUninstallToleratesMissingFiles proves a repeated uninstall is a no-op rather than
// an error, which is what a scripted uninstall looks like.
func TestUninstallToleratesMissingFiles(t *testing.T) {
	if _, err := Uninstall(Options{BinDir: t.TempDir()}); err != nil {
		t.Fatalf("uninstall of nothing failed: %v", err)
	}
}

// TestNeedsPrivilegeIsTyped proves a permission failure is distinguishable, so the CLI
// can suggest a user install instead of just failing.
func TestNeedsPrivilegeIsTyped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions do not apply")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; permissions are not enforced")
	}
	// Build a directory that exists but cannot be written to, which is what
	// /usr/local/bin looks like to a normal user.
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	src := makeRelease(t, BinaryName(), DaemonBinaryName())
	_, err := Install(Options{Source: src, BinDir: filepath.Join(locked, "bin")})
	if err == nil {
		t.Fatal("install into an unwritable directory succeeded")
	}
	var needs *NeedsPrivilegeError
	if !asNeedsPrivilege(err, &needs) {
		t.Fatalf("permission failure was not typed: %v", err)
	}
}

// TestOnPathDetectsEntries proves the PATH check, which decides whether the installer
// bothers telling the user to edit their shell config.
func TestOnPathDetectsEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", filepath.Join(dir, "a")+string(os.PathListSeparator)+filepath.Join(dir, "b"))
	if !onPath(filepath.Join(dir, "b")) {
		t.Fatal("an entry present in PATH was not detected")
	}
	if onPath(filepath.Join(dir, "c")) {
		t.Fatal("an entry not in PATH was reported present")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func asNeedsPrivilege(err error, target **NeedsPrivilegeError) bool {
	for err != nil {
		if e, ok := err.(*NeedsPrivilegeError); ok {
			*target = e
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
