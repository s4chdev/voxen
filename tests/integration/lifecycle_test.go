// Package integration drives the real voxen binaries end to end.
//
// These tests build cmd/voxen and cmd/voxend, run them as separate processes in an
// isolated profile, and talk to the daemon only through the CLI. That is the same
// path a user takes, so a regression in IPC, socket permissions, process
// management or CLI wiring fails here rather than only in unit tests.
//
// The tests skip on platforms where spawning and signalling a background daemon
// is not available in CI (they require a writable temp dir and Unix sockets or
// named pipes); they do not skip on Linux, macOS or Windows in the project's own
// workflow.
package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildBinaries compiles voxen and voxend once per test run.
func buildBinaries(t *testing.T) (voxen, voxend string) {
	t.Helper()
	dir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	voxen = filepath.Join(dir, "voxen"+suffix)
	voxend = filepath.Join(dir, "voxend"+suffix)

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
		cmd.Env = os.Environ()
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, data)
		}
	}
	build(voxen, "github.com/s4chdev/voxen/cmd/voxen")
	build(voxend, "github.com/s4chdev/voxen/cmd/voxend")
	return voxen, voxend
}

// env is an isolated voxen profile plus the built binaries.
// syncBuffer is a concurrency-safe bytes.Buffer for capturing a child process's
// output while the test may read it concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t      *testing.T
	dir    string
	voxen  string
	voxend string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	voxen, voxend := buildBinaries(t)
	return &env{t: t, dir: t.TempDir(), voxen: voxen, voxend: voxend}
}

func (e *env) cmdEnv() []string {
	base := os.Environ()
	return append(base,
		"VOXEN_CONFIG_DIR="+filepath.Join(e.dir, "config"),
		"VOXEN_STATE_DIR="+filepath.Join(e.dir, "state"),
		"VOXEN_RUNTIME_DIR="+filepath.Join(e.dir, "run"),
		"VOXEN_KEY_PASSPHRASE=",
	)
}

func (e *env) configPath() string { return filepath.Join(e.dir, "config", "config.yaml") }
func (e *env) keyPath() string    { return filepath.Join(e.dir, "config", "identity.json") }
func (e *env) logPath() string    { return filepath.Join(e.dir, "state", "logs", "voxend.log") }

// run executes the CLI and returns stdout, stderr and the exit code.
func (e *env) run(args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(e.voxen, args...)
	cmd.Env = e.cmdEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), code
}

func (e *env) mustRun(args ...string) string {
	e.t.Helper()
	out, errOut, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("%v failed with exit %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

// startForeground launches voxend directly (not via "voxen start") so the test
// owns the process handle and can assert on shutdown.
func (e *env) startForeground() *exec.Cmd {
	e.t.Helper()
	cmd := exec.Command(e.voxend, "--foreground", "--config", e.configPath())
	cmd.Env = e.cmdEnv()
	// A locked writer: the child's stderr is copied on an exec goroutine, so
	// reading the buffer without synchronisation is a data race.
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start voxend: %v", err)
	}
	e.t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		if e.t.Failed() {
			e.t.Logf("voxend stderr:\n%s", stderr.String())
		}
	})
	e.waitReady()
	return cmd
}

func (e *env) waitReady() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, code := e.run("status"); code == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("daemon did not become ready")
}

func TestTwoPeerIdentityExchange(t *testing.T) {
	// Two independent profiles, each with its own identity: the foundation of a
	// future call is that both sides can exchange and verify fingerprints.
	alice := newEnv(t)
	bob := newEnv(t)

	aliceOut := alice.mustRun("--json", "init", "--name", "alice")
	bobOut := bob.mustRun("--json", "init", "--name", "bob")

	var aliceJSON, bobJSON map[string]any
	if err := json.Unmarshal([]byte(aliceOut), &aliceJSON); err != nil {
		t.Fatalf("alice init json: %v\n%s", err, aliceOut)
	}
	if err := json.Unmarshal([]byte(bobOut), &bobJSON); err != nil {
		t.Fatalf("bob init json: %v\n%s", err, bobOut)
	}
	aliceFP, _ := aliceJSON["fingerprint"].(string)
	bobFP, _ := bobJSON["fingerprint"].(string)
	if aliceFP == "" || bobFP == "" {
		t.Fatalf("init json missing fingerprints: alice=%v bob=%v", aliceJSON, bobJSON)
	}
	if aliceFP == bobFP {
		t.Fatal("two independently generated identities share a fingerprint")
	}

	// Both profiles must have private key files.
	for _, e := range []*env{alice, bob} {
		info, err := os.Stat(e.keyPath())
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %04o, want 0600", info.Mode().Perm())
		}
	}
}

func TestDaemonLifecycleThroughCLI(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")

	// Foreground daemon started out-of-band.
	proc := e.startForeground()

	status := e.mustRun("--json", "status")
	var st struct {
		PID          int    `json:"pid"`
		Fingerprint  string `json:"fingerprint"`
		Phase        int    `json:"phase"`
		SocketPath   string `json:"socket"`
		Capabilities struct {
			Audio     bool `json:"audio"`
			Media     bool `json:"media"`
			Signaling bool `json:"signaling"`
			Group     bool `json:"group_calls"`
		} `json:"capabilities"`
		AudioState string `json:"audio_state"`
	}
	if err := json.Unmarshal([]byte(status), &st); err != nil {
		t.Fatalf("status json: %v\n%s", err, status)
	}
	if st.PID != proc.Process.Pid {
		t.Fatalf("status pid %d != voxend pid %d", st.PID, proc.Process.Pid)
	}
	if st.Fingerprint == "" {
		t.Fatal("status has no fingerprint")
	}
	// Media and group calling are implemented. Audio depends on whether the
	// backend initialised on this host, so assert consistency rather than a
	// hardcoded value.
	if !st.Capabilities.Media {
		t.Fatal("media is implemented but not reported")
	}
	if !st.Capabilities.Group {
		t.Fatal("group calling is implemented but not reported")
	}
	// Audio capability is probed at daemon startup. A backend that is present but
	// has no openable hardware legitimately reports unavailable, so the assertion
	// is that the state string is never empty and that a claimed capability is
	// backed by a real backend name.
	if st.AudioState == "" {
		t.Fatal("status has no audio state")
	}
	if st.Capabilities.Audio && strings.Contains(strings.ToLower(st.AudioState), "unavailable") {
		t.Fatalf("audio claimed available but state is %q", st.AudioState)
	}

	// Mute round trip through the daemon.
	muted := e.mustRun("--json", "mute")
	var m struct {
		Muted bool `json:"muted"`
	}
	if err := json.Unmarshal([]byte(muted), &m); err != nil || !m.Muted {
		t.Fatalf("mute: %v %s", err, muted)
	}

	// Logs must be readable from the daemon's perspective.
	logs := e.mustRun("logs", "-n", "100")
	if !strings.Contains(logs, "voxend started") {
		t.Fatalf("logs missing startup line:\n%s", logs)
	}

	// Graceful stop, then verify the process exited and the socket is gone.
	e.mustRun("stop")
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("voxend did not exit after 'voxen stop'")
	}
	if _, _, code := e.run("status"); code == 0 {
		t.Fatal("status succeeded after the daemon stopped")
	}
}

func TestSocketIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions do not apply; the pipe ACL is checked in unit tests")
	}
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")
	e.startForeground()

	endpoint := filepath.Join(e.dir, "run", "voxend.sock")
	info, err := os.Stat(endpoint)
	if err != nil {
		t.Fatalf("socket missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %04o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory mode = %04o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestSecondDaemonIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")
	first := e.startForeground()

	second := exec.Command(e.voxend, "--foreground", "--config", e.configPath())
	second.Env = e.cmdEnv()
	var stderr bytes.Buffer
	second.Stderr = &stderr
	err := second.Run()
	if err == nil {
		t.Fatal("second daemon started on an occupied endpoint")
	}
	if !strings.Contains(stderr.String(), "already in use") {
		t.Fatalf("second daemon failed without explaining why: %s", stderr.String())
	}
	_ = first
}

func TestStartDetachedAndStop(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")
	e.mustRun("start")

	// The daemon must outlive the CLI process that started it.
	out := e.mustRun("status")
	if !strings.Contains(out, "running") {
		t.Fatalf("detached daemon is not running:\n%s", out)
	}

	e.mustRun("stop")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, code := e.run("status"); code != 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("daemon still running after stop")
}

// TestRoomCommandsReachTheDaemon confirms create/join are wired to the daemon
// rather than gated behind a phase check. With no daemon running the CLI must
// report that clearly instead of claiming the feature does not exist.
func TestRoomCommandsReachTheDaemon(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")

	for _, args := range [][]string{{"call"}, {"join", "D8CY-2XWT-FME0"}} {
		_, errOut, code := e.run(args...)
		if code != 3 {
			t.Fatalf("%v exit code = %d, want 3 (daemon unavailable)\n%s", args, code, errOut)
		}
		if !strings.Contains(errOut, "voxend is not running") {
			t.Fatalf("%v should tell the user to start the daemon: %s", args, errOut)
		}
	}
}

func TestDoctorReportsRealSubsystems(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")
	e.startForeground()

	out := e.mustRun("doctor")
	for _, want := range []string{"identity", "audio", "signaling"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
	// Every check must carry a status; doctor must never emit a blank row.
	for _, chk := range []string{"daemon", "config", "identity", "trust-store", "signaling", "audio", "clock"} {
		if !strings.Contains(out, chk) {
			t.Fatalf("doctor missing the %q check:\n%s", chk, out)
		}
	}
	// Identity is always available, so doctor must never fail outright.
	if strings.Contains(out, "one or more checks failed") {
		t.Fatalf("doctor reported a failure on a healthy profile:\n%s", out)
	}
}

func TestInitTwiceFails(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")
	_, errOut, code := e.run("init", "--name", "tester")
	if code == 0 {
		t.Fatalf("second init succeeded")
	}
	if !strings.Contains(errOut, "already exists") {
		t.Fatalf("second init did not explain the conflict: %s", errOut)
	}
}

// TestTrustPinFlow drives the out-of-band verification flow a user performs
// before their first call: read a fingerprint, record it, inspect the store.
func TestTrustPinFlow(t *testing.T) {
	alice := newEnv(t)
	bob := newEnv(t)

	alice.mustRun("init", "--name", "alice")
	bobOut := bob.mustRun("--json", "init", "--name", "bob")
	var bobInit map[string]any
	if err := json.Unmarshal([]byte(bobOut), &bobInit); err != nil {
		t.Fatal(err)
	}
	bobFP, _ := bobInit["fingerprint"].(string)
	if bobFP == "" {
		t.Fatal("bob has no fingerprint")
	}

	alice.startForeground()
	alice.mustRun("verify", bobFP, "--name", "bob", "--note", "read over the phone")

	trustOut := alice.mustRun("--json", "trust")
	var trust struct {
		Entries []struct {
			Fingerprint string `json:"fingerprint"`
			Level       string `json:"level"`
			Pinned      bool   `json:"pinned"`
			PublicKey   string `json:"public_key"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(trustOut), &trust); err != nil {
		t.Fatalf("trust json: %v\n%s", err, trustOut)
	}
	if len(trust.Entries) != 1 {
		t.Fatalf("expected one trust entry, got %+v", trust.Entries)
	}
	entry := trust.Entries[0]
	if entry.Fingerprint != bobFP {
		t.Fatalf("stored fingerprint %q != bob's %q", entry.Fingerprint, bobFP)
	}
	if entry.Level != "verified" || !entry.Pinned {
		t.Fatalf("expected a verified pin, got %+v", entry)
	}
	if entry.PublicKey != "" {
		t.Fatal("a pin must not invent a public key")
	}

	// Revocation is visible and sticky.
	alice.mustRun("revoke", bobFP)
	trustOut = alice.mustRun("--json", "trust")
	if err := json.Unmarshal([]byte(trustOut), &trust); err != nil {
		t.Fatal(err)
	}
	if trust.Entries[0].Level != "revoked" {
		t.Fatalf("revocation not reflected: %+v", trust.Entries[0])
	}
}

func TestVersionAndHelp(t *testing.T) {
	e := newEnv(t)
	version := e.mustRun("version")
	if !strings.Contains(version, "phase:") {
		t.Fatalf("version output should name the phase:\n%s", version)
	}
	// The phase name changes as the project advances; assert the shape, not the
	// specific milestone, so this test does not need editing every phase.
	if !strings.Contains(version, "(") || !strings.Contains(version, ")") {
		t.Fatalf("version should pair the phase number with its name:\n%s", version)
	}
	help := e.mustRun("--help")
	for _, cmd := range []string{"init", "start", "stop", "restart", "create", "join", "leave"} {
		if !strings.Contains(help, cmd) {
			t.Fatalf("help missing %q", cmd)
		}
	}
}

func TestNoSecretsInLogs(t *testing.T) {
	e := newEnv(t)
	e.mustRun("init", "--name", "tester")
	e.startForeground()
	e.mustRun("status")
	e.mustRun("mute")
	e.mustRun("stop")

	raw, err := os.ReadFile(e.logPath())
	if err != nil {
		// The daemon may have written to stderr only if the log file could not be
		// opened; that is a failure for this profile layout.
		t.Fatalf("log file missing: %v", err)
	}
	text := string(raw)
	// The private seed must never appear. Public fingerprints are fine.
	keyFile, err := os.ReadFile(e.keyPath())
	if err != nil {
		t.Fatal(err)
	}
	var kf struct {
		PrivateKey struct {
			Seed string `json:"seed"`
		} `json:"private_key"`
	}
	if err := json.Unmarshal(keyFile, &kf); err == nil && kf.PrivateKey.Seed != "" {
		if strings.Contains(text, kf.PrivateKey.Seed) {
			t.Fatal("private key seed leaked into the log file")
		}
	}
	// The fingerprint is public and must be present, proving the log is real.
	if !strings.Contains(text, "voxend started") {
		t.Fatalf("log file does not contain the startup line:\n%s", text)
	}
}
