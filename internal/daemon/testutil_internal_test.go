package daemon

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/paths"
)

// newInternalProfile builds an isolated profile for internal tests.
func newInternalProfile(t *testing.T) paths.Paths {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{
		ConfigDir:    filepath.Join(dir, "config"),
		StateDir:     filepath.Join(dir, "state"),
		RuntimeDir:   filepath.Join(dir, "run"),
		LogDir:       filepath.Join(dir, "logs"),
		ConfigFile:   filepath.Join(dir, "config", "config.yaml"),
		IdentityFile: filepath.Join(dir, "config", "identity.json"),
		LogFile:      filepath.Join(dir, "logs", "voxend.log"),
		PIDFile:      filepath.Join(dir, "state", "voxend.pid"),
		SocketPath:   filepath.Join(dir, "run", "voxend.sock"),
	}
	if runtime.GOOS == "windows" {
		p.SocketPath = paths.PipeName(p.ConfigDir)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Identity.File = p.IdentityFile
	cfg.Daemon.Socket = p.SocketPath
	if err := config.Save(p.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.NewStore(p.IdentityFile, nil).Create("tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.OpenTrust(filepath.Dir(p.IdentityFile)); err != nil {
		t.Fatal(err)
	}
	return p
}
