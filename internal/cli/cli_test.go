package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s4chdev/voxen/internal/cli"
)

// sandbox creates an isolated VOXEN_CONFIG_DIR per test and returns an executor.
type sandbox struct {
	dir     string
	execute func(args ...string) (string, string, int)
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	runtimeDir := filepath.Join(dir, "run")
	stateDir := filepath.Join(dir, "state")
	t.Setenv("VOXEN_CONFIG_DIR", configDir)
	t.Setenv("VOXEN_STATE_DIR", stateDir)
	t.Setenv("VOXEN_RUNTIME_DIR", runtimeDir)
	t.Setenv("VOXEN_KEY_PASSPHRASE", "")

	s := &sandbox{dir: dir}
	s.execute = func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := cli.Execute(args, &stdout, &stderr, strings.NewReader(""))
		return stdout.String(), stderr.String(), code
	}
	return s
}

func (s *sandbox) configPath() string { return filepath.Join(s.dir, "config", "config.yaml") }
func (s *sandbox) keyPath() string    { return filepath.Join(s.dir, "config", "identity.json") }

func TestInitCreatesIdentityAndConfig(t *testing.T) {
	sb := newSandbox(t)
	out, errOut, code := sb.execute("init", "--name", "alice")
	if code != cli.ExitOK {
		t.Fatalf("init failed (%d): %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "fingerprint") {
		t.Fatalf("init output should print the fingerprint:\n%s", out)
	}
	if _, err := os.Stat(sb.keyPath()); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}
	if _, err := os.Stat(sb.configPath()); err != nil {
		t.Fatalf("config file missing: %v", err)
	}

	// Config must be private.
	info, err := os.Stat(sb.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %04o", info.Mode().Perm())
	}
}

func TestInitRefusesSecondRun(t *testing.T) {
	sb := newSandbox(t)
	if _, errOut, code := sb.execute("init", "--name", "alice"); code != cli.ExitOK {
		t.Fatalf("first init failed: %s", errOut)
	}
	_, errOut, code := sb.execute("init", "--name", "bob")
	if code != cli.ExitUsage {
		t.Fatalf("second init exit code = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(errOut, "already exists") {
		t.Fatalf("second init should explain the conflict: %s", errOut)
	}
}

func TestInitJSONOutput(t *testing.T) {
	sb := newSandbox(t)
	out, _, code := sb.execute("--json", "init", "--name", "alice")
	if code != cli.ExitOK {
		t.Fatalf("init --json failed: %s", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("init --json is not JSON: %v\n%s", err, out)
	}
	fp, _ := payload["fingerprint"].(string)
	if len(strings.ReplaceAll(fp, " ", "")) != 64 {
		t.Fatalf("fingerprint in JSON looks wrong: %q", fp)
	}
}

func TestInitWithPassphrase(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("VOXEN_KEY_PASSPHRASE", "a sufficiently long passphrase")
	out, errOut, code := sb.execute("init", "--name", "alice")
	if code != cli.ExitOK {
		t.Fatalf("init failed: %s%s", out, errOut)
	}
	if !strings.Contains(out, "encrypted") {
		t.Fatalf("output should report encrypted key storage:\n%s", out)
	}
	raw, err := os.ReadFile(sb.keyPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"encrypted"`)) {
		t.Fatal("key file is not marked encrypted")
	}
}

func TestIdentityCommandWithoutDaemon(t *testing.T) {
	sb := newSandbox(t)
	if _, errOut, code := sb.execute("init", "--name", "alice"); code != cli.ExitOK {
		t.Fatalf("init failed: %s", errOut)
	}
	out, _, code := sb.execute("identity")
	if code != cli.ExitOK {
		t.Fatalf("identity failed: %s", out)
	}
	if !strings.Contains(out, "fingerprint") {
		t.Fatalf("identity should print a fingerprint: %s", out)
	}
}

func TestCommandsFailCleanlyWithoutDaemon(t *testing.T) {
	sb := newSandbox(t)
	if _, errOut, code := sb.execute("init", "--name", "alice"); code != cli.ExitOK {
		t.Fatalf("init failed: %s", errOut)
	}
	for _, args := range [][]string{
		{"status"}, {"peers"}, {"mute"}, {"unmute"}, {"devices"}, {"doctor"},
		{"logs"}, {"config"}, {"identity"},
	} {
		out, errOut, code := sb.execute(args...)
		if code == cli.ExitOK && args[0] != "identity" && args[0] != "config" {
			t.Fatalf("%v unexpectedly succeeded: %s", args, out)
		}
		if code != cli.ExitOK && !strings.Contains(errOut, "voxend is not running") {
			t.Fatalf("%v: unhelpful error: %s", args, errOut)
		}
	}
}

func TestUnimplementedRoomCommandsSaySo(t *testing.T) {
	sb := newSandbox(t)
	if _, errOut, code := sb.execute("init", "--name", "alice"); code != cli.ExitOK {
		t.Fatalf("init failed: %s", errOut)
	}
	for _, args := range [][]string{{"create"}, {"join", "room-1"}} {
		_, errOut, code := sb.execute(args...)
		if code != cli.ExitUnsupported {
			t.Fatalf("%v exit code = %d, want %d (%s)", args, code, cli.ExitUnsupported, errOut)
		}
		if !strings.Contains(errOut, "phase") {
			t.Fatalf("%v should explain the phase gate: %s", args, errOut)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	sb := newSandbox(t)
	out, _, code := sb.execute("version")
	if code != cli.ExitOK || !strings.Contains(out, "voxen") {
		t.Fatalf("version failed: %s", out)
	}
	out, _, code = sb.execute("--help")
	if code != cli.ExitOK {
		t.Fatalf("help failed: %s", out)
	}
	for _, want := range []string{"init", "start", "stop", "restart", "create", "join", "leave", "mute", "unmute", "peers", "status", "devices", "config", "logs", "doctor"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help is missing the %q command:\n%s", want, out)
		}
	}
}

func TestVerifyRequiresDaemon(t *testing.T) {
	sb := newSandbox(t)
	if _, errOut, code := sb.execute("init", "--name", "alice"); code != cli.ExitOK {
		t.Fatalf("init failed: %s", errOut)
	}
	// Verification must be recorded by the daemon, which owns the trust store.
	_, errOut, code := sb.execute("verify", strings.Repeat("ab", 32))
	if code == cli.ExitOK {
		t.Fatalf("verify succeeded without a daemon: %s", errOut)
	}
	if !strings.Contains(errOut, "voxend is not running") {
		t.Fatalf("unhelpful verify error: %s", errOut)
	}
	// A malformed fingerprint is a usage error, caught before dialing.
	_, errOut, code = sb.execute("verify", "not-a-fingerprint")
	if code != cli.ExitUsage {
		t.Fatalf("bad fingerprint exit code = %d (%s)", code, errOut)
	}
}

func TestConfigCommandPrintsYAML(t *testing.T) {
	sb := newSandbox(t)
	if _, errOut, code := sb.execute("init", "--name", "alice"); code != cli.ExitOK {
		t.Fatalf("init failed: %s", errOut)
	}
	out, _, code := sb.execute("config")
	if code != cli.ExitOK {
		t.Fatalf("config failed: %s", out)
	}
	if !strings.Contains(out, "signaling:") || !strings.Contains(out, "require_verified_peers: true") {
		t.Fatalf("config output missing expected keys:\n%s", out)
	}
	pathOut, _, code := sb.execute("config", "--path")
	if code != cli.ExitOK || strings.TrimSpace(pathOut) != sb.configPath() {
		t.Fatalf("config --path = %q", pathOut)
	}
}
