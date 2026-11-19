package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSystemdServiceLifecycle exercises the real supervisor on Linux.
//
// It is opt-in via VOXEN_TEST_SYSTEMD=1 because installing and starting a user
// unit affects the developer's own session: it would fight with a real voxen
// installation, and a CI container may have no user manager. The manual procedure
// is documented in packaging/systemd/README.md, and this test automates exactly
// that procedure for a machine where it is safe.
func TestSystemdServiceLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux-only")
	}
	if os.Getenv("VOXEN_TEST_SYSTEMD") != "1" {
		t.Skip("set VOXEN_TEST_SYSTEMD=1 to run; this installs a real user unit")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not available")
	}
	if out, err := exec.Command("systemctl", "--user", "is-system-running").CombinedOutput(); err != nil {
		t.Skipf("no usable user manager: %s", strings.TrimSpace(string(out)))
	}

	e := newEnv(t)
	e.mustRun("init", "--name", "service-test")

	// Install through the CLI, exactly as a user would.
	out := e.mustRun("service", "install")
	if !strings.Contains(out, "Installed") {
		t.Fatalf("install output unexpected:\n%s", out)
	}
	t.Cleanup(func() {
		_, _, _ = e.run("service", "uninstall")
	})

	// The unit must be enabled for login and running now. The table's column
	// widths vary with the longest value, so match on fields, not spacing.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if serviceStatusIs(e, "installed", "yes") && serviceStatusIs(e, "running", "yes") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	status := e.mustRun("service", "status")
	if !serviceStatusIs(e, "installed", "yes") {
		t.Fatalf("unit not reported installed:\n%s", status)
	}
	if !serviceStatusIs(e, "running", "yes") {
		t.Fatalf("unit not reported running:\n%s", status)
	}
	if !serviceStatusIs(e, "starts at login", "yes") {
		t.Fatalf("unit is not enabled for login:\n%s", status)
	}

	// The supervised daemon must answer over IPC.
	if _, _, code := e.run("status"); code != 0 {
		t.Fatal("the daemon supervises but does not answer 'voxen status'")
	}

	// Crash recovery: killing the daemon must bring it back with a new PID.
	pidBefore := servicePID(t)
	if pidBefore == "" {
		t.Skip("cannot determine the supervised PID on this system")
	}
	kill := exec.Command("kill", "-9", pidBefore)
	if err := kill.Run(); err != nil {
		t.Skipf("cannot signal the daemon: %v", err)
	}
	deadline = time.Now().Add(20 * time.Second)
	restarted := false
	for time.Now().Before(deadline) {
		if pid := servicePID(t); pid != "" && pid != pidBefore {
			restarted = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !restarted {
		t.Fatal("the supervisor did not restart the daemon after a crash")
	}

	// Uninstall must leave nothing behind.
	e.mustRun("service", "uninstall")
	after := e.mustRun("service", "status")
	if !serviceStatusIs(e, "installed", "no") {
		t.Fatalf("unit still installed after uninstall:\n%s", after)
	}
}

// serviceStatusIs reports whether "voxen service status" shows the given field
// with the given value. The output is a two-column table whose padding depends on
// the longest label, so fields are compared rather than substrings.
func serviceStatusIs(e *env, field, want string) bool {
	out, _, code := e.run("--json", "service", "status")
	if code != 0 {
		return false
	}
	var st struct {
		Installed bool `json:"Installed"`
		Enabled   bool `json:"Enabled"`
		Running   bool `json:"Running"`
	}
	// The JSON field names follow the Go struct; decode leniently via a map.
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return false
	}
	_ = st
	get := func(key string) (bool, bool) {
		v, ok := raw[key]
		if !ok {
			return false, false
		}
		b, ok := v.(bool)
		return b, ok
	}
	switch field {
	case "installed", "starts at login", "running":
		key := map[string]string{
			"installed":       "Installed",
			"starts at login": "Enabled",
			"running":         "Running",
		}[field]
		b, ok := get(key)
		if !ok {
			return false
		}
		return b == (want == "yes")
	}
	return false
}

// servicePID returns the supervised daemon's PID, or "" if unknown.
func servicePID(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", "voxend.service").Output()
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(out))
	if pid == "0" {
		return ""
	}
	return pid
}
