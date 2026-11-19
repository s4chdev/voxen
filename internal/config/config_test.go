package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("version: 1\nfrobnicate: true\n"), t.TempDir())
	if err == nil {
		t.Fatal("expected unknown key to be rejected")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("error should name the offending key: %v", err)
	}
}

func TestParseRejectsWeakSecurity(t *testing.T) {
	_, err := Parse([]byte("version: 1\nsecurity:\n  require_verified_peers: false\n"), t.TempDir())
	if err == nil {
		t.Fatal("expected security.require_verified_peers=false to be rejected")
	}
}

func TestParseRejectsTurnAndRelayConfig(t *testing.T) {
	// A typo that would silently disable direct-only behaviour must fail.
	_, err := Parse([]byte("version: 1\nnetwork:\n  turn_servers:\n    - turn:example.org\n"), t.TempDir())
	if err == nil {
		t.Fatal("expected network.turn_servers to be rejected as an unknown field")
	}
}

func TestValidationBounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"sample rate", func(c *Config) { c.Audio.SampleRate = 44100 }, "sample_rate"},
		{"frame too short", func(c *Config) { c.Audio.FrameMS = 5 }, "frame_ms"},
		{"bitrate too high", func(c *Config) { c.Audio.Bitrate = 600000 }, "bitrate_bps"},
		{"ice timeout", func(c *Config) { c.Network.ICETimeoutMS = 100 }, "ice_timeout_ms"},
		{"bad signaling scheme", func(c *Config) { c.Signaling.URL = "http://example.org" }, "ws or wss"},
		{"credentials in url", func(c *Config) { c.Signaling.URL = "wss://u:p@example.org/ws" }, "credentials"},
		{"tls skip on remote", func(c *Config) {
			c.Signaling.URL = "wss://example.org/ws"
			c.Signaling.TLSSkipVerify = true
		}, "non-loopback"},
		{"bad log level", func(c *Config) { c.Logging.Level = "trace" }, "logging.level"},
		{"session ttl", func(c *Config) { c.Security.SessionTTLSeconds = 5 }, "session_ttl_seconds"},
		{"relative socket", func(c *Config) { c.Daemon.Socket = "voxen.sock" }, "daemon.socket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTLSVerifySkipAllowedOnLoopback(t *testing.T) {
	cfg := Default()
	cfg.Signaling.URL = "wss://127.0.0.1:8443/ws"
	cfg.Signaling.TLSSkipVerify = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback testing override should be allowed: %v", err)
	}
}

// TestEndpointMustMatchPlatformTransport guards the error a Windows user would
// otherwise only see as an opaque "not a named pipe path" at listen time.
func TestEndpointMustMatchPlatformTransport(t *testing.T) {
	cfg := Default()
	cfg.Daemon.Socket = `\\.\pipe\voxen-test`
	err := cfg.Validate()
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("a pipe path should be valid on Windows: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "Windows named pipe") {
		t.Fatalf("a pipe path should be rejected off Windows, got %v", err)
	}

	cfg = Default()
	cfg.Daemon.Socket = filepath.Join(t.TempDir(), "voxend.sock")
	err = cfg.Validate()
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "named pipe") {
			t.Fatalf("a Unix socket path should be rejected on Windows, got %v", err)
		}
	} else if err != nil {
		t.Fatalf("an absolute Unix socket path should be valid: %v", err)
	}

	cfg = Default()
	cfg.Daemon.Socket = "relative.sock"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a relative socket path should be rejected")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Default()
	cfg.Identity.DisplayName = "alice"
	cfg.Audio.Bitrate = 32000
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// POSIX permission bits are not meaningful on Windows, where the per-user
	// %APPDATA% ACL is the security boundary instead.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %04o, want 0600", info.Mode().Perm())
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Identity.DisplayName != "alice" || loaded.Audio.Bitrate != 32000 {
		t.Fatalf("round trip lost values: %+v", loaded)
	}
	if loaded.Identity.File != filepath.Join(dir, "identity.json") {
		t.Fatalf("relative identity path not resolved: %s", loaded.Identity.File)
	}
}

func TestParseEmptyAndMultiDoc(t *testing.T) {
	if _, err := Parse(nil, t.TempDir()); err == nil {
		t.Fatal("empty file must be rejected")
	}
	if _, err := Parse([]byte("version: 1\n---\nversion: 1\n"), t.TempDir()); err == nil {
		t.Fatal("multiple documents must be rejected")
	}
}

func TestFrameSamples(t *testing.T) {
	cfg := Default()
	if got := cfg.FrameSamples(); got != 960 {
		t.Fatalf("FrameSamples() = %d, want 960", got)
	}
}
