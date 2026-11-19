package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/paths"
	"github.com/s4chdev/voxen/internal/room"
)

// This test is internal because the link it checks - the controller's per-peer
// flags reaching the IPC peer listing - is unexported on purpose. Everything the
// flags depend on is covered from outside: speech detection by
// TestSpeakingIndicatorTracksPeerAudio, and mute by TestPerPeerMuteSilencesOnlyThatPeer.
//
// The value here is proving a UI reading "voxen peers" sees the same state the
// audio path wrote, without the test needing a real microphone.

func newServerForPeers(t *testing.T) *Server {
	t.Helper()

	// Build an isolated profile inline: newProfile lives in the external test
	// package and cannot be reused from an internal one.
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

	srv, err := New(context.Background(), Options{Paths: p, Foreground: true, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if srv.bridge != nil {
			srv.bridge.Stop()
		}
		srv.controllerMu.Lock()
		if srv.controller != nil {
			srv.controller.Close()
		}
		srv.controllerMu.Unlock()
		_ = srv.audio.Close()
		_ = srv.media.Close()
	})
	return srv
}

// TestPeerFlagsReachIPC checks that speaking and local mute, as recorded by the
// controller, appear in the peer listing a client reads.
func TestPeerFlagsReachIPC(t *testing.T) {
	srv := newServerForPeers(t)
	c := srv.controllerFor()

	// Register a connected participant without needing a signaling server.
	c.mu.Lock()
	c.sessions["peer-1"] = &peerSession{
		peerID:      "peer-1",
		fingerprint: "AA",
		displayName: "bob",
		state:       room.ConnConnected,
	}
	c.mu.Unlock()

	// Nothing is speaking or muted yet.
	peers := srv.peersList().Peers
	if len(peers) != 1 {
		t.Fatalf("expected one peer, got %d", len(peers))
	}
	if peers[0].Speaking {
		t.Fatal("a peer that has never sent audio is reported as speaking")
	}
	if peers[0].Muted {
		t.Fatal("a peer is reported muted before any mute was requested")
	}

	// Speech is observed.
	c.setSpeaking("peer-1", true)
	if !srv.peersList().Peers[0].Speaking {
		t.Fatal("speaking was set on the controller but not visible over IPC")
	}

	// And clears.
	c.setSpeaking("peer-1", false)
	if srv.peersList().Peers[0].Speaking {
		t.Fatal("speaking was cleared but IPC still reports it")
	}

	// Local mute reaches both the controller and the IPC view.
	if _, err := srv.setPeerMute(mustJSONForTest(t, ipc.PeerMuteParams{PeerID: "peer-1"})); err != nil {
		t.Fatalf("peer mute: %v", err)
	}
	if !srv.peersList().Peers[0].Muted {
		t.Fatal("mute did not reach the peer listing")
	}
	off := false
	if _, err := srv.setPeerMute(mustJSONForTest(t, ipc.PeerMuteParams{PeerID: "peer-1", Muted: &off})); err != nil {
		t.Fatalf("peer unmute: %v", err)
	}
	if srv.peersList().Peers[0].Muted {
		t.Fatal("unmute did not reach the peer listing")
	}
}

// TestPeerMuteUnknownPeerIsAnError proves muting a peer that is not present is a
// clear failure rather than a silent no-op, so a UI keyed by peer ID cannot act on
// a stale entry.
func TestPeerMuteUnknownPeerIsAnError(t *testing.T) {
	srv := newServerForPeers(t)
	_ = srv.controllerFor()

	_, err := srv.setPeerMute(mustJSONForTest(t, ipc.PeerMuteParams{PeerID: "ghost"}))
	if err == nil {
		t.Fatal("muting an unknown peer succeeded")
	}
	ipcErr, ok := err.(*ipc.Error)
	if !ok || ipcErr.Code != ipc.CodeNotFound {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

// TestPeerMuteWithoutRoomIsNotInRoom proves the error names the real problem when
// no call is active.
func TestPeerMuteWithoutRoomIsNotInRoom(t *testing.T) {
	srv := newServerForPeers(t)
	_, err := srv.setPeerMute(mustJSONForTest(t, ipc.PeerMuteParams{PeerID: "peer-1"}))
	if err == nil {
		t.Fatal("muting outside a room succeeded")
	}
	ipcErr, ok := err.(*ipc.Error)
	if !ok || ipcErr.Code != ipc.CodeNotInRoom {
		t.Fatalf("expected not-in-room, got %v", err)
	}
}

// quietLogger drops all output; this test exercises state, not log format.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustJSONForTest marshals a parameter struct for a direct handler call.
func mustJSONForTest(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
