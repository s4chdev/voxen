package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/media"
	"github.com/s4chdev/voxen/internal/security"
	serversig "github.com/s4chdev/voxen/server/signaling"
)

// startSignaling runs a real signaling server for the daemon tests.
func startSignaling(t *testing.T) string {
	t.Helper()
	srv, err := serversig.New(serversig.Config{
		Addr:       "127.0.0.1:0",
		MaxMembers: 8,
		Logger:     slog.New(slog.NewTextHandler(quietWriter{}, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("signaling server did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			return "ws://" + addr + "/ws"
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("signaling server did not start")
	return ""
}

type quietWriter struct{}

func (quietWriter) Write(p []byte) (int, error) { return len(p), nil }

// twoProfiles builds two isolated daemons pointed at one signaling server, with
// each peer's key verified in the other's trust store.
func twoProfiles(t *testing.T, signalingURL string) ([]*profile, []func() *ipc.Client) {
	t.Helper()
	profiles := make([]*profile, 2)
	clients := make([]func() *ipc.Client, 2)

	for i := range profiles {
		prof := newProfile(t, nil)
		cfg, err := config.Load(prof.p.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Signaling.URL = signalingURL
		if err := config.Save(prof.p.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}
		profiles[i] = prof
	}

	// Each side must have verified the other's fingerprint before media flows.
	for i, prof := range profiles {
		other := profiles[1-i]
		store := identity.StoreFromEnv(other.p.IdentityFile)
		// The key is already created by newProfile; load it to learn the public key.
		id, err := store.Load()
		if err != nil {
			t.Fatalf("load peer identity: %v", err)
		}
		// Simulate the out-of-band verification the user performs.
		trust, err := identity.OpenTrust(filepath.Dir(prof.p.IdentityFile))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := trust.VerifyKey(id.PublicKey, id.DisplayName, "test fixture"); err != nil {
			t.Fatal(err)
		}
	}

	for i, prof := range profiles {
		clients[i] = startDaemon(t, prof)
	}
	return profiles, clients
}

// TestTwoDaemonsFormMeshJoinAndLeave is the daemon-orchestration acceptance test.
//
// Two independent daemons, each with its own identity and IPC socket, join one
// room through a real signaling server. The controller must discover each other,
// negotiate a Pion session, complete the identity binding, and report a direct
// connection - then tear it all down on leave.
func TestTwoDaemonsFormMeshJoinAndLeave(t *testing.T) {
	url := startSignaling(t)
	_, clients := twoProfiles(t, url)
	alice := clients[0]()
	bob := clients[1]()

	var created ipc.RoomResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "create", Method: ipc.MethodRoomCreate,
		Params: mustJSON(t, ipc.RoomCreateParams{}),
	}, &created); err != nil {
		t.Fatalf("create room: %v", err)
	}
	if !created.Room.InRoom || created.Room.RoomID == "" {
		t.Fatalf("create returned no room: %+v", created.Room)
	}
	roomID := created.Room.RoomID

	// Bob joins it.
	var joined ipc.RoomResult
	if err := bob.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "join", Method: ipc.MethodRoomJoin,
		Params: mustJSON(t, ipc.RoomJoinParams{RoomID: roomID}),
	}, &joined); err != nil {
		t.Fatalf("join room: %v", err)
	}
	if joined.Room.RoomID != roomID {
		t.Fatalf("joined room %q, want %q", joined.Room.RoomID, roomID)
	}

	// Both sides must converge on a connected, direct, verified peer.
	waitForPeerState(t, alice, roomID, "connected")
	waitForPeerState(t, bob, roomID, "connected")

	for name, c := range map[string]*ipc.Client{"alice": alice, "bob": bob} {
		var peers ipc.PeersResult
		if err := c.Call(&ipc.Request{
			Version: ipc.ProtocolVersion, ID: "peers-" + name, Method: ipc.MethodPeersList,
		}, &peers); err != nil {
			t.Fatalf("%s peers: %v", name, err)
		}
		if len(peers.Peers) != 1 {
			t.Fatalf("%s sees %d peers, want 1: %+v", name, len(peers.Peers), peers.Peers)
		}
		peer := peers.Peers[0]
		if peer.Connection != "connected" {
			t.Fatalf("%s peer state = %q, want connected", name, peer.Connection)
		}
		if !peer.Verified {
			t.Fatalf("%s reports the peer as unverified", name)
		}
		if !peer.Direct {
			t.Fatalf("%s peer is connected without a direct path: %+v", name, peer)
		}
		if peer.Fingerprint == "" {
			t.Fatalf("%s peer has no fingerprint", name)
		}
	}

	// Status must agree.
	var status ipc.StatusResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "status", Method: ipc.MethodStatus,
	}, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Room.InRoom || status.Room.RoomID != roomID {
		t.Fatalf("status room = %+v", status.Room)
	}
	if status.Peers != 1 {
		t.Fatalf("status peers = %d, want 1", status.Peers)
	}
	if status.SignalingState != "connected" {
		t.Fatalf("signaling state = %q", status.SignalingState)
	}

	// Leaving tears the peer down on the other side.
	if err := bob.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "leave", Method: ipc.MethodRoomLeave,
		Params: mustJSON(t, ipc.RoomLeaveParams{Reason: "test finished"}),
	}, nil); err != nil {
		t.Fatalf("leave: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var peers ipc.PeersResult
		if err := alice.Call(&ipc.Request{
			Version: ipc.ProtocolVersion, ID: "peers-after", Method: ipc.MethodPeersList,
		}, &peers); err != nil {
			t.Fatal(err)
		}
		if len(peers.Peers) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("alice still sees a peer after bob left")
}

// waitForPeerState polls until the peer reaches the wanted connection state.
func waitForPeerState(t *testing.T, c *ipc.Client, roomID, want string) {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	var last []ipc.PeerState
	for time.Now().Before(deadline) {
		var peers ipc.PeersResult
		if err := c.Call(&ipc.Request{
			Version: ipc.ProtocolVersion, ID: "poll", Method: ipc.MethodPeersList,
		}, &peers); err != nil {
			t.Fatalf("poll peers: %v", err)
		}
		last = peers.Peers
		if len(peers.Peers) == 1 && peers.Peers[0].Connection == want {
			return
		}
		// A failed peer will never reach the wanted state; fail fast with detail.
		for _, p := range peers.Peers {
			if p.Connection == "failed" {
				var events ipc.RoomEventsResult
				_ = c.Call(&ipc.Request{
					Version: ipc.ProtocolVersion, ID: "events", Method: ipc.MethodRoomEvents,
				}, &events)
				raw, _ := json.Marshal(events.Events)
				t.Fatalf("peer failed in room %s: %s\nevents: %s", roomID, p.LastError, raw)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peer did not reach %q; last state: %+v", want, last)
}

// TestUnverifiedPeerRefused proves the orchestration honours the trust model: a
// peer whose fingerprint the user has not verified never gets a session.
func TestUnverifiedPeerRefused(t *testing.T) {
	url := startSignaling(t)

	// Build two profiles but deliberately do NOT verify each other.
	a := newProfile(t, nil)
	b := newProfile(t, nil)
	for _, prof := range []*profile{a, b} {
		cfg, err := config.Load(prof.p.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Signaling.URL = url
		if err := config.Save(prof.p.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}
	}
	alice := startDaemon(t, a)()
	bob := startDaemon(t, b)()

	var created ipc.RoomResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "create", Method: ipc.MethodRoomCreate,
		Params: mustJSON(t, ipc.RoomCreateParams{}),
	}, &created); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := bob.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "join", Method: ipc.MethodRoomJoin,
		Params: mustJSON(t, ipc.RoomJoinParams{RoomID: created.Room.RoomID}),
	}, nil); err != nil {
		t.Fatalf("join: %v", err)
	}

	// Give the controller time to notice and refuse the peer.
	time.Sleep(2 * time.Second)

	for name, c := range map[string]*ipc.Client{"alice": alice, "bob": bob} {
		var peers ipc.PeersResult
		if err := c.Call(&ipc.Request{
			Version: ipc.ProtocolVersion, ID: "peers-" + name, Method: ipc.MethodPeersList,
		}, &peers); err != nil {
			t.Fatal(err)
		}
		for _, p := range peers.Peers {
			if p.Connection == "connected" {
				t.Fatalf("%s connected to an unverified peer: %+v", name, p)
			}
		}
	}

	// The rejection must be explained, not silent.
	var events ipc.RoomEventsResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "events", Method: ipc.MethodRoomEvents,
	}, &events); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events.Events {
		if e.Kind == "peer_rejected" && strings.Contains(e.Message, "not verified") {
			found = true
		}
	}
	if !found {
		raw, _ := json.Marshal(events.Events)
		t.Fatalf("no rejection event explaining why the peer was refused: %s", raw)
	}
}

func TestRoomCommandsRequireDaemonState(t *testing.T) {
	prof := newProfile(t, nil)
	c := startDaemon(t, prof)()

	// Leaving when not in a room is a typed error, not a crash.
	err := c.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "leave", Method: ipc.MethodRoomLeave,
	}, nil)
	var ipcErr *ipc.Error
	if err == nil || !errors.As(err, &ipcErr) || ipcErr.Code != ipc.CodeNotInRoom {
		t.Fatalf("expected not_in_room, got %v", err)
	}
}

// TestTrustVerificationAfterDaemonStart proves a "voxen verify" performed while
// the daemon is already running takes effect without a restart.
//
// The daemon originally cached the trust store at startup, so verification ran
// from a separate CLI process wrote to disk but the daemon never saw it. The
// symptom was a peer that could never connect despite the user having verified
// its fingerprint, plus a log line telling them to verify what they just had.
func TestTrustVerificationAfterDaemonStart(t *testing.T) {
	url := startSignaling(t)

	a := newProfile(t, nil)
	b := newProfile(t, nil)
	for _, prof := range []*profile{a, b} {
		cfg, err := config.Load(prof.p.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Signaling.URL = url
		if err := config.Save(prof.p.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}
	}

	// Start both daemons first, with empty trust stores.
	alice := startDaemon(t, a)()
	bob := startDaemon(t, b)()

	// Learn each identity the way the CLI does.
	aID, err := identity.StoreFromEnv(a.p.IdentityFile).Load()
	if err != nil {
		t.Fatal(err)
	}
	bID, err := identity.StoreFromEnv(b.p.IdentityFile).Load()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate "voxen verify" running in a separate process while the daemons
	// are up: write the trust stores directly, exactly as the CLI would.
	aTrust, err := identity.OpenTrust(filepath.Dir(a.p.IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aTrust.VerifyFingerprint(identity.FingerprintFromPublic(bID.PublicKey), "bob", "verified after start"); err != nil {
		t.Fatal(err)
	}
	bTrust, err := identity.OpenTrust(filepath.Dir(b.p.IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bTrust.VerifyFingerprint(identity.FingerprintFromPublic(aID.PublicKey), "alice", "verified after start"); err != nil {
		t.Fatal(err)
	}

	var created ipc.RoomResult
	if err := alice.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "create", Method: ipc.MethodRoomCreate,
		Params: mustJSON(t, ipc.RoomCreateParams{}),
	}, &created); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := bob.Call(&ipc.Request{
		Version: ipc.ProtocolVersion, ID: "join", Method: ipc.MethodRoomJoin,
		Params: mustJSON(t, ipc.RoomJoinParams{RoomID: created.Room.RoomID}),
	}, nil); err != nil {
		t.Fatalf("join: %v", err)
	}

	// The peers must connect using the pins written after startup.
	waitForPeerState(t, alice, created.Room.RoomID, "connected")
	waitForPeerState(t, bob, created.Room.RoomID, "connected")
}

// TestNoDirectPathSurfacesToTheUser proves a connectivity failure reaches the
// media plane as a specific, actionable error rather than a generic timeout.
//
// This is the behaviour the documentation promises: voxen does not relay, so a
// failed direct path must be named as such instead of leaving the user to wonder
// whether they simply waited too little.
func TestNoDirectPathSurfacesToTheUser(t *testing.T) {
	aliceID := testSigner(t, "alice")
	bobID := testSigner(t, "bob")

	sess, err := media.NewWebRTCSession(context.Background(), media.SessionConfig{
		RoomID: "room-x", SessionID: "blackhole-1",
		LocalIdentity: aliceID, ExpectedPeerKey: bobID.PublicKey(),
		Parameters:     media.DefaultOpusParameters(28000, 20),
		StatementTTL:   2 * time.Minute,
		ReplayGuard:    security.NewReplayGuard(64),
		ConnectTimeout: 400 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.CreateOffer(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = sess.Authenticate(ctx)
	if !errors.Is(err, media.ErrNoDirectRoute) {
		t.Fatalf("expected ErrNoDirectRoute, got %v", err)
	}
	t.Logf("connectivity failure is specific: %v", err)
}
