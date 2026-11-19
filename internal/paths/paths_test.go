package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveUsesPlatformDirectories(t *testing.T) {
	t.Setenv("VOXEN_CONFIG", "")
	t.Setenv("VOXEN_CONFIG_DIR", "")
	t.Setenv("VOXEN_STATE_DIR", "")
	t.Setenv("VOXEN_RUNTIME_DIR", "")
	t.Setenv("VOXEN_SOCKET", "")
	t.Setenv("VOXEN_SOCKET", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.ConfigDir) {
		t.Fatalf("config dir is not absolute: %s", p.ConfigDir)
	}
	switch runtime.GOOS {
	case "linux":
		if !strings.Contains(p.ConfigDir, ".config") && !strings.Contains(p.ConfigDir, "config") {
			t.Fatalf("unexpected Linux config dir: %s", p.ConfigDir)
		}
		if !strings.HasSuffix(p.SocketPath, ".sock") && runtime.GOOS != "windows" {
			t.Fatalf("unexpected socket path: %s", p.SocketPath)
		}
	case "darwin":
		if !strings.Contains(p.ConfigDir, "Application Support") {
			t.Fatalf("macOS config dir should live in Application Support: %s", p.ConfigDir)
		}
		if !strings.Contains(p.LogDir, "Logs") {
			t.Fatalf("macOS log dir should live in Library/Logs: %s", p.LogDir)
		}
	case "windows":
		if !strings.HasPrefix(p.SocketPath, `\\.\pipe\`) {
			t.Fatalf("Windows endpoint should be a named pipe: %s", p.SocketPath)
		}
	}
}

func TestResolveHonoursOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOXEN_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("VOXEN_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("VOXEN_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("VOXEN_SOCKET", filepath.Join(dir, "custom.sock"))

	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != filepath.Join(dir, "cfg") {
		t.Fatalf("config dir = %s", p.ConfigDir)
	}
	if p.LogDir != filepath.Join(dir, "state", "logs") {
		t.Fatalf("log dir = %s", p.LogDir)
	}
	if runtime.GOOS != "windows" && p.SocketPath != filepath.Join(dir, "custom.sock") {
		t.Fatalf("socket = %s", p.SocketPath)
	}
}

func TestEnsureCreatesPrivateDirectories(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOXEN_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("VOXEN_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("VOXEN_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("VOXEN_SOCKET", "")

	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p.ConfigDir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("config dir mode = %04o, want 0700", info.Mode().Perm())
		}
		if err := CheckPermissions(p.ConfigDir, 0o700); err != nil {
			t.Fatalf("freshly created directory should pass its own check: %v", err)
		}
	}
}

func TestCheckPermissionsRejectsLooseFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes do not apply on Windows")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckPermissions(path, 0o600); err == nil {
		t.Fatal("0644 file passed a 0600 check")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPermissions(path, 0o600); err != nil {
		t.Fatalf("0600 file rejected: %v", err)
	}
}

func TestPipeNameIsStableAndScoped(t *testing.T) {
	a := PipeName(`C:\Users\alice\AppData\Roaming\voxen`)
	b := PipeName(`C:\Users\alice\AppData\Roaming\voxen`)
	c := PipeName(`C:\Users\bob\AppData\Roaming\voxen`)
	if a != b {
		t.Fatalf("pipe name is not deterministic: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("different profiles collided on %s", a)
	}
	if !strings.HasPrefix(strings.ToLower(a), `\\.\pipe\`) {
		t.Fatalf("not a pipe path: %s", a)
	}
}
