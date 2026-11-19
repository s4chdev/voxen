package daemon_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/daemon"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/paths"
)

// profile builds an isolated voxen home directory for one test.
type profile struct {
	dir  string
	p    paths.Paths
	cfg  config.Config
	key  string
	pass []byte
}

func newProfile(t *testing.T, passphrase []byte) *profile {
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
		// The endpoint must use the platform's transport: a Unix socket path on
		// Windows is rejected by the named-pipe listener, and vice versa.
		SocketPath: paths.PipeName(filepath.Join(dir, "config")),
	}
	if runtime.GOOS != "windows" {
		p.SocketPath = filepath.Join(dir, "run", "voxend.sock")
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
	store := identity.NewStore(p.IdentityFile, passphrase)
	if _, err := store.Create("tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.OpenTrust(filepath.Dir(p.IdentityFile)); err != nil {
		t.Fatal(err)
	}
	return &profile{dir: dir, p: p, cfg: cfg, key: p.IdentityFile, pass: passphrase}
}

// startDaemon runs a daemon in the background and returns a client factory.
func startDaemon(t *testing.T, prof *profile) func() *ipc.Client {
	t.Helper()
	if prof.pass != nil {
		t.Setenv(identity.PassphraseEnv, string(prof.pass))
	}
	logger := slog.New(slog.NewTextHandler(&safeWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := daemon.New(ctx, daemon.Options{Paths: prof.p, Foreground: true, Logger: logger})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Wait for the socket to accept connections. Report why the daemon exited
	// instead of only timing out: a bind failure is a different bug from a slow
	// start, and the two used to look identical.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon did not start listening on %s within 5s", prof.p.SocketPath)
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("daemon exited before it was ready: %v", err)
		default:
		}
		conn, err := ipc.Dial(context.Background(), prof.p.SocketPath, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down")
		}
	})

	return func() *ipc.Client {
		conn, err := ipc.Dial(context.Background(), prof.p.SocketPath, time.Second)
		if err != nil {
			t.Fatalf("dial daemon: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return ipc.NewClient(conn, 5*time.Second)
	}
}

type safeWriter struct{ t *testing.T }

func (w *safeWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func call[T any](t *testing.T, c *ipc.Client, method string, params any) T {
	t.Helper()
	var out T
	var raw []byte
	if params != nil {
		raw = mustJSON(t, params)
	}
	req := &ipc.Request{Version: ipc.ProtocolVersion, ID: "test-" + method, Method: method, Params: raw}
	if err := c.Call(req, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := jsonMarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDaemonReportsHonestCapabilities(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	status := call[ipc.StatusResult](t, c, ipc.MethodStatus, nil)
	// The capability matrix must match the code. Media and signaling are now
	// implemented and tested; audio capture and group calls are not, and the
	// daemon must not claim them.
	if !status.Capabilities.Media {
		t.Fatal("media is implemented but not reported as available")
	}
	if !status.Capabilities.Signaling {
		t.Fatal("signaling is implemented but not reported as available")
	}
	if status.Capabilities.Audio {
		t.Fatal("audio capture is not implemented, so it must not be reported available")
	}
	if status.Capabilities.Group {
		t.Fatal("group calling is not implemented, so it must not be reported available")
	}
	if status.Fingerprint == "" {
		t.Fatal("status must include the identity fingerprint")
	}
	if status.Room.InRoom {
		t.Fatal("fresh daemon must not be in a room")
	}
}

func TestDaemonIdentityAndTrust(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	id := call[ipc.IdentityResult](t, c, ipc.MethodIdentityShow, nil)
	if id.Fingerprint == "" || id.PublicKey == "" {
		t.Fatalf("identity incomplete: %+v", id)
	}
	if id.Encrypted {
		t.Fatal("unencrypted key reported as encrypted")
	}

	// Status and identity must agree on the fingerprint.
	status := call[ipc.StatusResult](t, c, ipc.MethodStatus, nil)
	if status.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch between status (%s) and identity (%s)", status.Fingerprint, id.Fingerprint)
	}

	// Verifying a fingerprint that has never been observed pins it: the key
	// presented by that peer later must hash to exactly this value.
	peerFP := strings.Repeat("ab", 32)
	rec := call[ipc.TrustRecord](t, c, ipc.MethodIdentityVerify,
		ipc.VerifyPeerParams{Fingerprint: peerFP, DisplayName: "bob", Note: "read over the phone"})
	if rec.Level != string(identity.TrustVerified) {
		t.Fatalf("pinned entry level = %q", rec.Level)
	}
	if !rec.Pinned {
		t.Fatal("pre-observation verification should be marked as a pin")
	}
	if rec.PublicKey != "" {
		t.Fatal("a pin must not invent a public key")
	}

	// It shows up in the trust list and is usable as an anchor.
	list := call[ipc.TrustListResult](t, c, ipc.MethodTrustList, nil)
	if len(list.Entries) != 1 || list.Entries[0].Fingerprint != rec.Fingerprint {
		t.Fatalf("trust list = %+v", list.Entries)
	}

	// Revocation is sticky and recorded.
	var revoked map[string]string
	if err := c.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "revoke", Method: ipc.MethodIdentityRevoke,
		Params: mustJSON(t, ipc.RevokePeerParams{Fingerprint: peerFP}),
	}, &revoked); err != nil {
		t.Fatal(err)
	}
	list = call[ipc.TrustListResult](t, c, ipc.MethodTrustList, nil)
	if len(list.Entries) != 1 || list.Entries[0].Level != string(identity.TrustRevoked) {
		t.Fatalf("revocation not recorded: %+v", list.Entries)
	}

	// Revoking an unknown fingerprint is a not-found error, not a silent success.
	err := c.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "revoke2", Method: ipc.MethodIdentityRevoke,
		Params: mustJSON(t, ipc.RevokePeerParams{Fingerprint: strings.Repeat("cd", 32)}),
	}, nil)
	var ipcErr *ipc.Error
	if err == nil || !asIPCError(err, &ipcErr) || ipcErr.Code != ipc.CodeNotFound {
		t.Fatalf("expected not_found, got %v", err)
	}
}

func TestDaemonMuteToggles(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	res := call[ipc.MuteResult](t, c, ipc.MethodMuteSet, ipc.MuteParams{})
	if !res.Muted {
		t.Fatal("first toggle should mute")
	}
	res = call[ipc.MuteResult](t, c, ipc.MethodMuteSet, ipc.MuteParams{})
	if res.Muted {
		t.Fatal("second toggle should unmute")
	}
	status := call[ipc.StatusResult](t, c, ipc.MethodStatus, nil)
	if status.Muted {
		t.Fatal("status does not reflect unmuted state")
	}
}

func TestDaemonUnimplementedFeaturesFailExplicitly(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	for _, method := range []string{ipc.MethodRoomCreate, ipc.MethodRoomJoin, ipc.MethodSessionICESet} {
		err := c.Call(&ipc.Request{
			Version: ipc.ProtocolVersion, ID: "x-" + method, Method: method,
			Params: mustJSON(t, ipc.RoomCreateParams{Name: "x"}),
		}, nil)
		var ipcErr *ipc.Error
		if err == nil {
			t.Fatalf("%s unexpectedly succeeded", method)
		}
		if !asIPCError(err, &ipcErr) || ipcErr.Code != ipc.CodeUnsupported {
			t.Fatalf("%s: expected unsupported, got %v", method, err)
		}
	}
}

func TestDaemonDevicesReportsUnavailable(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	res := call[ipc.DevicesResult](t, c, ipc.MethodDevicesList, nil)
	if res.Error == "" {
		t.Fatal("phase 1 has no audio backend; the daemon must say so")
	}
	if len(res.Inputs) != 0 {
		t.Fatal("phase 1 must not invent devices")
	}
}

func TestDaemonDoctorDoesNotClaimPhantomFeatures(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	res := call[ipc.DoctorResult](t, c, ipc.MethodDoctor, nil)
	if len(res.Checks) == 0 {
		t.Fatal("doctor returned no checks")
	}
	statuses := map[string]string{}
	for _, chk := range res.Checks {
		statuses[chk.Name] = chk.Status
	}
	if statuses["audio"] != "unavailable" {
		t.Fatalf("audio check = %q, want unavailable", statuses["audio"])
	}
	if statuses["signaling"] != "unavailable" {
		t.Fatalf("signaling check = %q, want unavailable", statuses["signaling"])
	}
	if statuses["identity"] != "pass" {
		t.Fatalf("identity check = %q", statuses["identity"])
	}
}

func TestDaemonLogsTail(t *testing.T) {
	prof := newProfile(t, nil)
	// Write a log file as the daemon's background mode would.
	if err := os.WriteFile(prof.p.LogFile, []byte("line one\nline two\nline three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := startDaemon(t, prof)
	c := client()

	res := call[ipc.LogsResult](t, c, ipc.MethodLogsTail, ipc.LogsParams{Lines: 2})
	if len(res.Lines) != 2 || res.Lines[1] != "line three" {
		t.Fatalf("unexpected tail: %+v", res)
	}
	if !res.Truncated {
		t.Fatal("tail should report truncation")
	}
}

func TestDaemonEncryptedIdentityRequiresPassphrase(t *testing.T) {
	pass := []byte("a long enough passphrase")
	prof := newProfile(t, pass)

	// Without the passphrase the daemon must refuse to start.
	t.Setenv(identity.PassphraseEnv, "")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := daemon.New(context.Background(), daemon.Options{Paths: prof.p, Foreground: true, Logger: logger})
	if err == nil {
		t.Fatal("daemon started without unlocking an encrypted identity")
	}
	if !strings.Contains(err.Error(), "passphrase") && !strings.Contains(err.Error(), identity.PassphraseEnv) {
		t.Fatalf("error should explain the missing passphrase: %v", err)
	}

	// With the passphrase it starts and reports encryption.
	client := startDaemon(t, prof)
	c := client()
	id := call[ipc.IdentityResult](t, c, ipc.MethodIdentityShow, nil)
	if !id.Encrypted {
		t.Fatal("encrypted identity reported as plaintext")
	}
}

func TestDaemonShutdown(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	var res map[string]string
	if err := c.Call(&ipc.Request{Version: ipc.ProtocolVersion, ID: "shutdown", Method: ipc.MethodShutdown}, &res); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := ipc.Dial(context.Background(), prof.p.SocketPath, 200*time.Millisecond)
		if err != nil {
			return // socket is gone: clean shutdown
		}
		conn.Close()
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("daemon still accepting connections after shutdown")
}

func TestDaemonRejectsStaleConfigReload(t *testing.T) {
	prof := newProfile(t, nil)
	client := startDaemon(t, prof)
	c := client()

	// Change a restart-only field.
	cfg, err := config.Load(prof.p.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Daemon.Socket = filepath.Join(prof.dir, "other.sock")
	if runtime.GOOS == "windows" {
		cfg.Daemon.Socket = paths.PipeName(filepath.Join(prof.dir, "other"))
	}
	if err := config.Save(prof.p.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}

	err = c.Call(&ipc.Request{Version: ipc.ProtocolVersion, ID: "reload", Method: ipc.MethodConfigReload}, nil)
	var ipcErr *ipc.Error
	if err == nil || !asIPCError(err, &ipcErr) || ipcErr.Code != ipc.CodeConflict {
		t.Fatalf("expected conflict on socket change, got %v", err)
	}
}
