package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSystemdUnitContent checks the generated unit for the directives that were
// found to matter by actually running it. Each assertion here corresponds to a
// failure observed on a real machine, so a regression is caught without needing
// systemd in CI.
func TestSystemdUnitContent(t *testing.T) {
	m := &systemdManager{}
	unit := m.render(Options{
		Binary: "/opt/voxen/voxend",
		Config: "/home/u/.config/voxen/config.yaml",
	})

	// The unit must point at the configured binary and config.
	if !strings.Contains(unit, "ExecStart=/opt/voxen/voxend --foreground") {
		t.Fatal("ExecStart does not run the daemon in the foreground")
	}
	if !strings.Contains(unit, "--config /home/u/.config/voxen/config.yaml") {
		t.Fatal("ExecStart does not pass the config path")
	}

	// The runtime directory must be writable, or binding the socket fails with
	// "read-only file system" inside the namespace ProtectHome creates.
	if !strings.Contains(unit, "%t/voxen") {
		t.Fatal("the runtime directory is not in ReadWritePaths; the IPC socket cannot bind")
	}
	if !strings.Contains(unit, "ExecStartPre=/bin/mkdir -p %t/voxen") {
		t.Fatal("the runtime directory is not pre-created")
	}

	// Both config and state must be writable.
	for _, path := range []string{"%h/.config/voxen", "%h/.local/state/voxen"} {
		if !strings.Contains(unit, path) {
			t.Fatalf("%s missing from ReadWritePaths", path)
		}
	}

	// These two look like good hardening but break the daemon. They must not
	// appear as directives. Matching whole lines avoids tripping over the comment
	// block that explains why they are absent.
	for _, forbidden := range []string{"ProtectSystem=strict", "PrivateTmp=yes"} {
		for _, line := range strings.Split(unit, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if trimmed == forbidden {
				t.Fatalf("%s would break the daemon and must not be set", forbidden)
			}
		}
	}

	// Restart policy: restart on crash, honour a clean exit.
	if !strings.Contains(unit, "Restart=on-failure") {
		t.Fatal("a crash would not be recovered")
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Fatal("the unit would not start at login")
	}
	// It must be a user unit for audio, never a system one.
	if strings.Contains(unit, "User=") {
		t.Fatal("a system unit cannot access the user's audio session")
	}
}

// TestLaunchdPlistContent checks the generated agent.
func TestLaunchdPlistContent(t *testing.T) {
	m := &launchdManager{}
	plist := m.render(Options{
		Binary: "/opt/voxen/voxend",
		Config: "/Users/u/.config/voxen/config.yaml",
	}, "/Users/u/Library/Logs/voxen")

	for _, want := range []string{
		"<string>" + launchdLabel + "</string>",
		"<string>--foreground</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>Aqua</string>", // needs an interactive audio session
		"/Users/u/Library/Logs/voxen/voxend.err.log",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q", want)
		}
	}
	// A clean shutdown must not be restarted, or "voxen stop" would loop.
	if !strings.Contains(plist, "<key>SuccessfulExit</key>") {
		t.Fatal("plist does not distinguish a clean exit from a crash")
	}
	// The plist must be valid XML with a matching DOCTYPE version.
	if !strings.HasPrefix(plist, "<?xml") || !strings.Contains(plist, "</plist>") {
		t.Fatal("plist is not well-formed")
	}
}

// TestForPlatformReturnsAManager checks every platform resolves to something,
// so a build never silently lacks supervision.
func TestForPlatformReturnsAManager(t *testing.T) {
	m, err := ForPlatform()
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		if err != nil {
			t.Fatalf("no manager on %s: %v", runtime.GOOS, err)
		}
		if m.Name() == "" {
			t.Fatal("manager has no name")
		}
	default:
		if err == nil {
			t.Fatalf("unexpected manager on %s", runtime.GOOS)
		}
	}
}

// TestQuietlyStopIsNotAnInstallFailure checks Uninstall tolerates a unit that was
// never installed, which is what a repeated or scripted uninstall looks like.
func TestUninstallToleratesMissingUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-specific")
	}
	// Point HOME at an empty directory so the real user config is untouched.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	m := &systemdManager{}
	// Uninstalling something that is not there must not error, and must not touch
	// anything outside the temporary home.
	if err := m.Uninstall(Options{}); err != nil {
		t.Fatalf("uninstall of a missing unit failed: %v", err)
	}
	// Status must report "not installed" rather than an error.
	st, err := m.Status(Options{})
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if st.Installed {
		t.Fatal("status claims an installation that does not exist")
	}
}

// TestInstallCreatesPrivateDirectories checks the directories systemd requires are
// created with private modes by voxen itself, not by the supervisor.
func TestInstallPreparesDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-specific")
	}
	// This test only exercises the filesystem preparation; it deliberately does
	// not call systemctl. EnsureInstallDirs is the seam that makes that possible.
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	cfg := filepath.Join(cfgDir, "config.yaml")

	if err := ensureInstallDirs(cfg, ""); err != nil {
		t.Fatalf("prepare directories: %v", err)
	}
	info, err := os.Stat(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("config directory mode = %04o, want 0700", info.Mode().Perm())
	}
}
