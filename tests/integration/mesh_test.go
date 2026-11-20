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
	"testing"
	"time"
)

// meshEnv is a set of independent voxen profiles plus a signaling server, driven
// entirely through the real binaries. It exists because the room orchestration
// bugs so far only appeared at this level: an in-process test shares the trust
// store and the socket, so it cannot catch a stale-config or stale-binary
// problem.
type meshEnv struct {
	t          *testing.T
	root       string
	hush       string
	hushd      string
	signalAddr string
	signalCmd  *exec.Cmd
	signalLog  *syncBuffer
}

func newMeshEnv(t *testing.T) *meshEnv {
	t.Helper()
	binDir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	build := func(name, pkg string) string {
		out := filepath.Join(binDir, name+suffix)
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, data)
		}
		return out
	}
	e := &meshEnv{
		t:     t,
		root:  t.TempDir(),
		hush:  build("voxen", "github.com/s4chdev/voxen/cmd/voxen"),
		hushd: build("voxend", "github.com/s4chdev/voxen/cmd/voxend"),
	}
	e.startSignaling()
	return e
}

func (e *meshEnv) startSignaling() {
	e.t.Helper()
	sigBin := filepath.Join(filepath.Dir(e.hush), "voxen-signal")
	if runtime.GOOS == "windows" {
		sigBin += ".exe"
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", sigBin, "github.com/s4chdev/voxen/cmd/voxen-signal")
	if data, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("build signaling: %v\n%s", err, data)
	}

	log := &syncBuffer{}
	proc := exec.Command(sigBin, "-listen", "127.0.0.1:0")
	proc.Stdout = log
	proc.Stderr = log
	if err := proc.Start(); err != nil {
		e.t.Fatalf("start signaling: %v", err)
	}
	e.signalCmd = proc
	e.signalLog = log

	// The server prints its bound address because port 0 was requested.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := log.String()
		for _, line := range strings.Split(out, "\n") {
			if i := strings.Index(line, "addr=127.0.0.1:"); i >= 0 {
				rest := line[i+len("addr="):]
				if j := strings.IndexAny(rest, " \""); j > 0 {
					rest = rest[:j]
				}
				e.signalAddr = rest
				e.t.Cleanup(func() {
					if proc.Process != nil {
						_ = proc.Process.Kill()
						_, _ = proc.Process.Wait()
					}
				})
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("signaling server did not report an address:\n%s", log.String())
}

func (e *meshEnv) wsURL() string { return "ws://" + e.signalAddr + "/ws" }

// profile is one isolated voxen home directory.
type meshProfile struct {
	name      string
	configDir string
	stateDir  string
	runDir    string
}

func (e *meshEnv) newProfile(name string) *meshProfile {
	base := filepath.Join(e.root, name)
	p := &meshProfile{
		name:      name,
		configDir: filepath.Join(base, "config"),
		stateDir:  filepath.Join(base, "state"),
		runDir:    filepath.Join(base, "run"),
	}
	for _, d := range []string{p.configDir, p.stateDir, p.runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			e.t.Fatal(err)
		}
	}
	return p
}

func (p *meshProfile) env() []string {
	return append(os.Environ(),
		"VOXEN_CONFIG_DIR="+p.configDir,
		"VOXEN_STATE_DIR="+p.stateDir,
		"VOXEN_RUNTIME_DIR="+p.runDir,
		"VOXEN_KEY_PASSPHRASE=",
	)
}

func (p *meshProfile) configPath() string { return filepath.Join(p.configDir, "config.yaml") }

// run executes the CLI for this profile.
func (e *meshEnv) run(p *meshProfile, args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(e.hush, args...)
	cmd.Env = p.env()
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

func (e *meshEnv) mustRun(p *meshProfile, args ...string) string {
	e.t.Helper()
	out, errOut, code := e.run(p, args...)
	if code != 0 {
		e.t.Fatalf("%s %v failed (%d)\nstdout: %s\nstderr: %s", p.name, args, code, out, errOut)
	}
	return out
}

// initProfile creates identity and config, then points it at the test server.
func (e *meshEnv) initProfile(p *meshProfile) {
	e.t.Helper()
	e.mustRun(p, "init", "--name", p.name)
	e.pointAtSignaling(p)
}

// pointAtSignaling rewrites the signaling URL in an existing config.
func (e *meshEnv) pointAtSignaling(p *meshProfile) {
	e.t.Helper()
	raw, err := os.ReadFile(p.configPath())
	if err != nil {
		e.t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "url:") &&
			strings.Contains(line, "ws") && !strings.Contains(lines[i], "signaling") {
			// Only rewrite within the signaling block; the audio block has no
			// url key, so the first match is correct.
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			lines[i] = indent + "url: " + e.wsURL()
			break
		}
	}
	if err := os.WriteFile(p.configPath(), []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// fingerprint returns the profile's own fingerprint via the CLI.
func (e *meshEnv) fingerprint(p *meshProfile) string {
	e.t.Helper()
	out := e.mustRun(p, "--json", "identity")
	var id struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal([]byte(out), &id); err != nil {
		e.t.Fatalf("identity json: %v\n%s", err, out)
	}
	if id.Fingerprint == "" {
		e.t.Fatalf("no fingerprint for %s", p.name)
	}
	return id.Fingerprint
}

// startDaemon launches voxend in the foreground for a profile.
func (e *meshEnv) startDaemon(p *meshProfile) {
	e.t.Helper()
	log := &syncBuffer{}
	cmd := exec.Command(e.hushd, "--foreground", "--config", p.configPath())
	cmd.Env = p.env()
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start %s daemon: %v", p.name, err)
	}
	e.t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		if e.t.Failed() {
			e.t.Logf("%s daemon log:\n%s", p.name, log.String())
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, code := e.run(p, "status"); code == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("%s daemon did not become ready:\n%s", p.name, log.String())
}

// peerList decodes "voxen --json peers".
func (e *meshEnv) peerList(p *meshProfile) []struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Fingerprint string `json:"fingerprint"`
	Verified    bool   `json:"verified"`
	Connection  string `json:"connection"`
	Direct      bool   `json:"direct"`
	LastError   string `json:"last_error"`
} {
	e.t.Helper()
	out := e.mustRun(p, "--json", "peers")
	var res struct {
		Peers []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Fingerprint string `json:"fingerprint"`
			Verified    bool   `json:"verified"`
			Connection  string `json:"connection"`
			Direct      bool   `json:"direct"`
			LastError   string `json:"last_error"`
		} `json:"peers"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		e.t.Fatalf("peers json: %v\n%s", err, out)
	}
	return res.Peers
}

// TestRealBinariesFormMesh is the acceptance test for room orchestration, run
// against the actual executables.
//
// It covers the full user journey: init, verify, start, create, join, peers,
// leave. Every step goes through the CLI the user would type.
func TestRealBinariesFormMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs real binaries")
	}
	e := newMeshEnv(t)

	alice := e.newProfile("alice")
	bob := e.newProfile("bob")
	e.initProfile(alice)
	e.initProfile(bob)

	aliceFP := e.fingerprint(alice)
	bobFP := e.fingerprint(bob)
	if aliceFP == bobFP {
		t.Fatal("distinct profiles share a fingerprint")
	}

	// Daemons start BEFORE verification, which is the situation that used to
	// fail: the daemon cached the trust store at startup.
	e.startDaemon(alice)
	e.startDaemon(bob)

	e.mustRun(alice, "verify", bobFP, "--name", "bob", "--note", "read over the phone")
	e.mustRun(bob, "verify", aliceFP, "--name", "alice", "--note", "read over the phone")

	roomOut := e.mustRun(alice, "--json", "call")
	var created struct {
		ShareCode string `json:"code"`
	}
	if err := json.Unmarshal([]byte(roomOut), &created); err != nil {
		t.Fatalf("create json: %v\n%s", err, roomOut)
	}
	if created.ShareCode == "" {
		t.Fatalf("call produced no share code: %s", roomOut)
	}
	t.Logf("share code: %s", created.ShareCode)

	// The code is the whole handshake: no fingerprint exchange, and the joiner
	// verifies it against the caller's key before any audio flows.
	e.mustRun(bob, "join", created.ShareCode)

	// Both sides must reach a connected, verified, direct peer.
	waitForConnected(t, e, alice, "alice")
	waitForConnected(t, e, bob, "bob")

	for _, p := range []*meshProfile{alice, bob} {
		peers := e.peerList(p)
		if len(peers) != 1 {
			t.Fatalf("%s sees %d peers, want 1: %+v", p.name, len(peers), peers)
		}
		peer := peers[0]
		if !peer.Verified {
			t.Fatalf("%s: peer not verified", p.name)
		}
		if !peer.Direct {
			t.Fatalf("%s: peer connected without a direct path: %+v", p.name, peer)
		}
	}

	// Bob leaving must clear alice's peer list.
	e.mustRun(bob, "leave")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.peerList(alice)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("alice still sees peers after bob left: %+v", e.peerList(alice))
}

func waitForConnected(t *testing.T, e *meshEnv, p *meshProfile, label string) {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		peers := e.peerList(p)
		if len(peers) == 1 && peers[0].Connection == "connected" {
			return
		}
		if len(peers) == 1 && peers[0].Connection == "failed" {
			t.Fatalf("%s peer failed: %s", label, peers[0].LastError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never connected; peers: %+v", label, e.peerList(p))
}

// TestRealBinariesRefuseUnverifiedPeer is the negative counterpart: without
// out-of-band verification, the mesh must not form.
func TestRealBinariesRefuseUnverifiedPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs real binaries")
	}
	e := newMeshEnv(t)

	alice := e.newProfile("alice")
	bob := e.newProfile("bob")
	e.initProfile(alice)
	e.initProfile(bob)
	e.startDaemon(alice)
	e.startDaemon(bob)

	roomOut := e.mustRun(alice, "--json", "call")
	var created struct {
		ShareCode string `json:"code"`
	}
	if err := json.Unmarshal([]byte(roomOut), &created); err != nil {
		t.Fatal(err)
	}
	e.mustRun(bob, "join", created.ShareCode)

	// Allow the controller time to see and refuse the peer.
	time.Sleep(3 * time.Second)
	for _, p := range []*meshProfile{alice, bob} {
		for _, peer := range e.peerList(p) {
			if peer.Connection == "connected" {
				t.Fatalf("%s connected to an unverified peer: %+v", p.name, peer)
			}
		}
	}
}

// TestRealBinariesThreePeerMesh proves the topology is a real full mesh: with
// three participants every pair must establish its own authenticated session,
// so each peer reports two direct connections.
//
// This is where a mesh differs from a star or an SFU: if the controller only
// wired new joiners to the creator, each peer would see one connection and this
// test would fail.
func TestRealBinariesThreePeerMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs real binaries")
	}
	e := newMeshEnv(t)

	names := []string{"alice", "bob", "carol"}
	profiles := make([]*meshProfile, len(names))
	for i, name := range names {
		p := e.newProfile(name)
		e.initProfile(p)
		profiles[i] = p
	}

	// Collect fingerprints while the profiles are offline, then start every
	// daemon, then verify pairwise. "voxen verify" writes through the daemon
	// because the daemon owns the trust store, so the order matters.
	fps := make([]string, len(profiles))
	for i, p := range profiles {
		fps[i] = e.fingerprint(p)
	}
	for _, p := range profiles {
		e.startDaemon(p)
	}
	for i, p := range profiles {
		for j := range profiles {
			if i == j {
				continue
			}
			e.mustRun(p, "verify", fps[j], "--name", names[j])
		}
	}

	var created struct {
		ShareCode string `json:"code"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(profiles[0], "--json", "call")), &created); err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles[1:] {
		e.mustRun(p, "join", created.ShareCode)
	}

	// Every peer must reach two connected, direct peers.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		allGood := true
		for _, p := range profiles {
			peers := e.peerList(p)
			if len(peers) != 2 {
				allGood = false
				continue
			}
			for _, peer := range peers {
				if peer.Connection != "connected" || !peer.Direct {
					allGood = false
				}
				if peer.Connection == "failed" {
					t.Fatalf("%s peer failed: %s", p.name, peer.LastError)
				}
			}
		}
		if allGood {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	for _, p := range profiles {
		t.Logf("%s peers: %+v", p.name, e.peerList(p))
	}
	t.Fatal("three-peer mesh did not fully connect")
}
