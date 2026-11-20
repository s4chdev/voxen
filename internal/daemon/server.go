// Package daemon implements the voxend background service.
//
// The daemon is the only process that touches audio devices, WebRTC sessions,
// signaling connections and key material. The CLI is a thin client over the
// private local socket.
package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/media"
	"github.com/s4chdev/voxen/internal/paths"
	"github.com/s4chdev/voxen/internal/room"
	"github.com/s4chdev/voxen/internal/security"
	"github.com/s4chdev/voxen/internal/version"
)

// Options configures a daemon instance.
type Options struct {
	Paths paths.Paths
	// Foreground disables the daemon's own detachment logic; the caller owns
	// the process lifecycle. Logging to stderr is enabled in this mode.
	Foreground bool
	Logger     *slog.Logger
}

// Server is the running daemon.
type Server struct {
	opts     Options
	log      *slog.Logger
	cfg      config.Config
	identity *identity.Identity
	trust    *identity.TrustStore
	audio    audio.Engine
	media    media.Manager
	replay   *security.ReplayGuard

	// controller owns room membership and the media mesh. It is created lazily on
	// the first room command so a daemon that never places a call does not open a
	// signaling connection.
	controller   *Controller
	controllerMu sync.Mutex
	// bridge pumps audio between the devices and the mesh. It lives as long as
	// the stay in a room.
	bridge *AudioBridge

	listener net.Listener
	started  time.Time
	shutdown chan struct{}
	stopOnce sync.Once

	muted    atomic.Bool
	requests atomic.Uint64
	active   atomic.Int64
	// audioUsable caches the startup probe result for the capability matrix.
	audioUsable atomic.Bool
	// mutedPeers is the local per-participant mute set, kept across calls.
	mutedPeersMu sync.Mutex
	mutedPeers   map[string]bool

	mu       sync.Mutex
	sessions map[string]context.CancelFunc
}

// New creates a daemon. It loads and validates config, identity and trust state
// before returning, so a misconfigured daemon fails at construction rather than
// half-serving clients.
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := opts.Paths.Ensure(); err != nil {
		return nil, err
	}

	cfg, err := config.Load(opts.Paths.ConfigFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("daemon: no configuration at %s; run 'voxen init'", opts.Paths.ConfigFile)
		}
		return nil, err
	}

	keyPath := cfg.Identity.File
	if keyPath == "" {
		keyPath = opts.Paths.IdentityFile
	}
	store := identity.StoreFromEnv(keyPath)
	id, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("daemon: load identity: %w", err)
	}
	trust, err := identity.OpenTrust(filepath.Dir(keyPath))
	if err != nil {
		return nil, fmt.Errorf("daemon: load trust store: %w", err)
	}

	s := &Server{
		opts:     opts,
		log:      opts.Logger,
		cfg:      cfg,
		identity: id,
		trust:    trust,
		audio:    audioEngineFromEnv(opts.Logger),
		replay:   security.NewReplayGuard(security.DefaultReplayCapacity),
		started:  time.Now().UTC(),
		shutdown: make(chan struct{}),
		sessions: make(map[string]context.CancelFunc),
	}
	s.muted.Store(false)

	// Media is wired to the trust store through a resolver rather than by
	// handing the media plane a key store. A session can therefore only be
	// created for a fingerprint the local user has verified.
	// From here on, any error path must release the audio backend. Each MiniAudio
	// engine owns a miniaudio context, and leaking one per failed construction
	// eventually makes InitContext abort inside cgo, which presents as a random
	// SIGABRT in unrelated tests.
	cleanupOnError := func() {
		if s.audio != nil {
			_ = s.audio.Close()
		}
	}

	manager, err := media.NewWebRTCManager(media.ManagerConfig{
		Identity:       signerAdapter{id: id},
		Parameters:     media.DefaultOpusParameters(cfg.Audio.Bitrate, cfg.Audio.FrameMS),
		STUNServers:    cfg.Network.STUNServers,
		ConnectTimeout: time.Duration(cfg.Network.ICETimeoutMS) * time.Millisecond,
		StatementTTL:   time.Duration(cfg.Security.SessionTTLSeconds) * time.Second,
		Logger:         opts.Logger.With("component", "media"),
	})
	if err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("daemon: create media manager: %w", err)
	}
	s.media = manager

	// Probe audio once at startup so the capability matrix reflects reality
	// without opening devices on every status request.
	if probe := audio.Probe(context.Background(), s.audio); probe.Usable() {
		s.audioUsable.Store(true)
	}

	if ctx.Err() != nil {
		cleanupOnError()
		return nil, ctx.Err()
	}
	return s, nil
}

// signerAdapter exposes only the signing capability of an identity to the media
// plane. The media plane cannot read the key file, export the private key or
// learn the seed through this interface.
type signerAdapter struct {
	id *identity.Identity
}

func (a signerAdapter) Sign(msg []byte) []byte { return a.id.Sign(msg) }

func (a signerAdapter) PublicKey() ed25519.PublicKey { return a.id.PublicKey }

// Identity exposes the loaded identity (read-only) for tests and diagnostics.
func (s *Server) Identity() *identity.Identity { return s.identity }

// Run serves IPC requests until ctx is cancelled or a shutdown request arrives.
func (s *Server) Run(ctx context.Context) error {
	endpoint := s.cfg.Endpoint(s.opts.Paths)
	ln, err := ipc.Listen(endpoint)
	if err != nil {
		return err
	}
	s.listener = ln

	if err := s.writePIDFile(); err != nil {
		ln.Close()
		return err
	}
	defer s.cleanup()

	s.log.Info("voxend started",
		"pid", os.Getpid(),
		"version", version.String(),
		"endpoint", endpoint,
		"fingerprint", s.identity.Fingerprint().String(),
		"phase", version.Phase,
	)

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.serve(ctx) }()

	select {
	case <-ctx.Done():
		s.log.Info("shutdown requested by signal")
	case <-s.shutdown:
		s.log.Info("shutdown requested by client")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			s.log.Error("ipc server failed", "error", err)
			s.closeOnce()
			return err
		}
	}

	s.closeOnce()
	// Give in-flight handlers a short window to finish.
	graceful, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for s.active.Load() > 0 && graceful.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	s.log.Info("voxend stopped")
	return nil
}

func (s *Server) closeOnce() {
	s.stopOnce.Do(func() { close(s.shutdown) })
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

func (s *Server) cleanup() {
	s.controllerMu.Lock()
	bridge := s.bridge
	s.bridge = nil
	s.controllerMu.Unlock()
	if bridge != nil {
		bridge.Stop()
	}
	// The engine is closed only after the bridge has been asked to stop, and the
	// bridge closes its device streams before returning. Closing the engine while a
	// device still exists frees the backend context out from under it, which
	// underflows PulseAudio's client refcount and aborts the process from inside C.
	// If the bridge teardown timed out, the engine is deliberately leaked rather
	// than freed under a live device: leaking is recoverable, aborting is not.
	if bridge == nil || bridge.FullyStopped() {
		_ = s.audio.Close()
	}
	s.controllerMu.Lock()
	if s.controller != nil {
		s.controller.Close()
	}
	s.controllerMu.Unlock()
	_ = s.media.Close()
	endpoint := s.cfg.Endpoint(s.opts.Paths)
	if err := ipc.RemoveEndpoint(endpoint); err != nil {
		s.log.Warn("could not remove IPC endpoint", "endpoint", endpoint, "error", err)
	}
	_ = os.Remove(s.opts.Paths.PIDFile)
}

func (s *Server) writePIDFile() error {
	content := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.MkdirAll(filepath.Dir(s.opts.Paths.PIDFile), 0o700); err != nil {
		return fmt.Errorf("daemon: create pid directory: %w", err)
	}
	if err := os.WriteFile(s.opts.Paths.PIDFile, []byte(content), 0o600); err != nil {
		return fmt.Errorf("daemon: write pid file: %w", err)
	}
	return nil
}

func (s *Server) serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.active.Add(1)
		go func() {
			defer s.active.Add(-1)
			s.handleConn(ctx, conn)
		}()
	}
}

// handleConn serves requests on one CLI connection. The connection stays open so
// a client can issue several calls (for example "status" then "peers") without a
// new dial.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	fc := ipc.NewFrameConn(conn, 30*time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			return
		default:
		}
		var req ipc.Request
		if err := fc.Read(&req); err != nil {
			return
		}
		s.requests.Add(1)
		resp := s.dispatch(ctx, &req)
		if err := fc.Write(resp); err != nil {
			return
		}
		if req.Method == ipc.MethodShutdown {
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req *ipc.Request) *ipc.Response {
	resp := &ipc.Response{Version: ipc.ProtocolVersion, ID: req.ID, OK: true}
	if err := req.Validate(); err != nil {
		return failure(req, err)
	}

	result, err := s.route(ctx, req)
	if err != nil {
		return failure(req, err)
	}
	if result != nil {
		raw, encErr := jsonMarshal(result)
		if encErr != nil {
			return failure(req, ipc.NewError(ipc.CodeInternal, "encode result: %v", encErr))
		}
		resp.Result = raw
	}
	return resp
}

func failure(req *ipc.Request, err error) *ipc.Response {
	resp := &ipc.Response{Version: ipc.ProtocolVersion, ID: req.ID, OK: false}
	var ipcErr *ipc.Error
	if errors.As(err, &ipcErr) {
		resp.Error = ipcErr
		return resp
	}
	code := ipc.CodeInternal
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = ipc.CodeUnavailable
	case errors.Is(err, room.ErrNotInRoom):
		code = ipc.CodeNotInRoom
	case errors.Is(err, identity.ErrUnknownPeer), errors.Is(err, identity.ErrNotVerified):
		code = ipc.CodeNotFound
	}
	resp.Error = &ipc.Error{Code: code, Message: err.Error()}
	return resp
}

func (s *Server) route(ctx context.Context, req *ipc.Request) (any, error) {
	switch req.Method {
	case ipc.MethodPing:
		return ipc.PingResult{PID: os.Getpid(), Version: version.String(), StartedAt: s.started.Format(time.RFC3339)}, nil
	case ipc.MethodStatus:
		return s.status(), nil
	case ipc.MethodShutdown:
		resp := map[string]string{"state": "stopping"}
		s.closeOnce()
		return resp, nil
	case ipc.MethodConfigGet:
		raw, err := s.cfg.Marshal()
		if err != nil {
			return nil, err
		}
		return ipc.ConfigGetResult{Path: s.opts.Paths.ConfigFile, YAML: string(raw), Source: "file"}, nil
	case ipc.MethodConfigReload:
		return s.reloadConfig()
	case ipc.MethodIdentityShow:
		return s.identityResult(), nil
	case ipc.MethodTrustList:
		return s.trustList(), nil
	case ipc.MethodIdentityVerify:
		return s.verifyPeer(req.Params)
	case ipc.MethodIdentityRevoke:
		return s.revokePeer(req.Params)
	case ipc.MethodPeersList:
		return s.peersList(), nil
	case ipc.MethodDevicesList:
		return s.devicesList(ctx), nil
	case ipc.MethodLogsTail:
		return s.logsTail(req.Params)
	case ipc.MethodMuteSet:
		return s.setMute(req.Params)
	case ipc.MethodPeerMuteSet:
		return s.setPeerMute(req.Params)
	case ipc.MethodDoctor:
		return s.doctor(ctx), nil
	case ipc.MethodRoomCreate:
		return s.roomCreate(ctx, req.Params)
	case ipc.MethodRoomJoin:
		return s.roomJoin(ctx, req.Params)
	case ipc.MethodRoomLeave:
		return s.roomLeave(req.Params)
	case ipc.MethodRoomEvents:
		return ipc.RoomEventsResult{Events: s.roomEvents()}, nil
	case ipc.MethodRoomAdmit, ipc.MethodRoomRemove, ipc.MethodSessionICESet:
		return nil, ipc.NewError(ipc.CodeUnsupported,
			"%s is not available in this build (phase %d: %s)", req.Method, version.Phase, version.PhaseName)
	default:
		return nil, ipc.NewError(ipc.CodeBadRequest, "unknown method %q", req.Method)
	}
}

func (s *Server) status() ipc.StatusResult {
	signalingState := "disconnected"
	s.controllerMu.Lock()
	if s.controller != nil {
		signalingState = s.controller.SignalingState()
	}
	s.controllerMu.Unlock()
	peers := len(s.peerSnapshot())
	return ipc.StatusResult{
		PID:            os.Getpid(),
		Version:        version.String(),
		Phase:          version.Phase,
		Platform:       version.Get().Platform,
		StartedAt:      s.started.Format(time.RFC3339),
		ConfigPath:     s.opts.Paths.ConfigFile,
		SocketPath:     s.cfg.Endpoint(s.opts.Paths),
		Fingerprint:    s.identity.Fingerprint().String(),
		DisplayName:    s.identity.DisplayName,
		Signaling:      s.cfg.Signaling.URL,
		SignalingState: signalingState,
		Muted:          s.muted.Load(),
		Room:           s.roomState(),
		Peers:          peers,
		AudioState:     s.audioState(),
		Audio:          s.audioStats(),
		Capabilities:   s.capabilities(),
	}
}

// capabilities reports what this build can actually do. Media and signaling are
// real; audio capture and group calling are not yet, and saying so is the whole
// point of this method.
func (s *Server) capabilities() ipc.Capabilities {
	// Audio availability is probed once at startup and cached. Probing on every
	// status call would open the microphone repeatedly, which is wasteful and on
	// some systems audible. doctor re-probes live, so a user diagnosing a problem
	// still gets an up-to-date answer.
	audioOK := s.audioUsable.Load()
	return ipc.Capabilities{
		Signaling: true,
		Media:     true,
		Audio:     audioOK,
		Group:     true,
	}
}

func (s *Server) roomState() ipc.RoomState {
	s.controllerMu.Lock()
	c := s.controller
	s.controllerMu.Unlock()
	if c == nil {
		return ipc.RoomState{}
	}
	return c.roomState()
}

// peerSnapshot returns the current peer list.
func (s *Server) peerSnapshot() []ipc.PeerState {
	s.controllerMu.Lock()
	c := s.controller
	s.controllerMu.Unlock()
	if c == nil {
		return nil
	}
	return c.Peers()
}

func (s *Server) identityResult() ipc.IdentityResult {
	encrypted := os.Getenv(identity.PassphraseEnv) != ""
	return ipc.IdentityResult{
		Fingerprint: s.identity.Fingerprint().String(),
		PublicKey:   s.identity.PublicKeyEncoded(),
		DisplayName: s.identity.DisplayName,
		CreatedAt:   s.identity.CreatedAt(),
		Encrypted:   encrypted,
	}
}

func (s *Server) trustList() ipc.TrustListResult {
	entries := s.trust.List()
	out := make([]ipc.TrustRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, trustRecord(e))
	}
	return ipc.TrustListResult{Entries: out}
}

func formatOptional(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (s *Server) verifyPeer(params jsonRaw) (any, error) {
	var p ipc.VerifyPeerParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	fp, err := identity.ParseFingerprint(p.Fingerprint)
	if err != nil {
		return nil, ipc.NewError(ipc.CodeBadRequest, "%v", err)
	}
	entry, ok := s.trust.Lookup(fp)
	if !ok || entry.PublicKey == "" {
		// The fingerprint has not been observed in a session yet. That is the
		// normal pre-call flow: pin the fingerprint the peer read out loud and
		// require the key presented later to hash to it.
		pinned, err := s.trust.VerifyFingerprint(fp, p.DisplayName, p.Note)
		if err != nil {
			return nil, ipc.NewError(ipc.CodeBadRequest, "%v", err)
		}
		s.log.Info("fingerprint pinned",
			"fingerprint", pinned.Fingerprint.String(), "display_name", pinned.DisplayName)
		return trustRecord(pinned), nil
	}

	pub, err := entry.PublicKeyBytes()
	if err != nil {
		return nil, ipc.NewError(ipc.CodeInternal, "%v", err)
	}
	if pub == nil {
		return nil, ipc.NewError(ipc.CodeInternal, "trust entry has no public key")
	}
	updated, err := s.trust.VerifyKey(pub, p.DisplayName, p.Note)
	if err != nil {
		return nil, err
	}
	s.log.Info("peer identity verified", "fingerprint", updated.Fingerprint.String(), "display_name", updated.DisplayName)
	return trustRecord(updated), nil
}

func trustRecord(e identity.TrustEntry) ipc.TrustRecord {
	return ipc.TrustRecord{
		Fingerprint: e.Fingerprint.String(),
		PublicKey:   e.PublicKey,
		DisplayName: e.DisplayName,
		Level:       string(e.Level),
		FirstSeen:   e.FirstSeen.Format(time.RFC3339),
		LastSeen:    e.LastSeen.Format(time.RFC3339),
		VerifiedAt:  formatOptional(e.VerifiedAt),
		RevokedAt:   formatOptional(e.RevokedAt),
		Note:        e.Note,
		Pinned:      e.Pinned,
	}
}

func (s *Server) revokePeer(params jsonRaw) (any, error) {
	var p ipc.RevokePeerParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	fp, err := identity.ParseFingerprint(p.Fingerprint)
	if err != nil {
		return nil, ipc.NewError(ipc.CodeBadRequest, "%v", err)
	}
	if err := s.trust.Revoke(fp); err != nil {
		return nil, ipc.NewError(ipc.CodeNotFound, "%v", err)
	}
	s.log.Warn("peer identity revoked", "fingerprint", fp.String())
	return map[string]string{"fingerprint": fp.String(), "level": string(identity.TrustRevoked)}, nil
}

func (s *Server) peersList() ipc.PeersResult {
	return ipc.PeersResult{Peers: s.peerSnapshot()}
}

func (s *Server) devicesList(ctx context.Context) ipc.DevicesResult {
	devices, err := s.audio.Devices(ctx)
	if err != nil {
		return ipc.DevicesResult{Error: err.Error()}
	}
	res := ipc.DevicesResult{}
	for _, d := range devices {
		info := ipc.DeviceInfo{
			ID:         string(d.ID),
			Name:       d.Name,
			IsInput:    d.IsInput,
			IsOutput:   d.IsOutput,
			IsDefault:  d.IsDefault,
			SampleRate: d.SampleRate,
			Channels:   d.Channels,
		}
		if d.IsInput {
			res.Inputs = append(res.Inputs, info)
		}
		if d.IsOutput {
			res.Outputs = append(res.Outputs, info)
		}
	}
	return res
}

func (s *Server) logsTail(params jsonRaw) (any, error) {
	var p ipc.LogsParams
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	if p.Lines <= 0 {
		p.Lines = 50
	}
	if p.Lines > 5000 {
		return nil, ipc.NewError(ipc.CodeBadRequest, "lines must be at most 5000")
	}
	lines, truncated, err := tailFile(s.opts.Paths.LogFile, p.Lines)
	if err != nil {
		// In foreground mode the daemon logs to the terminal, so there is no log
		// file. Report that explicitly instead of an empty success.
		if errors.Is(err, os.ErrNotExist) {
			return ipc.LogsResult{Path: s.opts.Paths.LogFile, Lines: nil, Truncated: false}, nil
		}
		return nil, err
	}
	return ipc.LogsResult{Path: s.opts.Paths.LogFile, Lines: lines, Truncated: truncated}, nil
}

func (s *Server) reloadConfig() (any, error) {
	cfg, err := config.Load(s.opts.Paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	// Only reloadable fields are applied. Identity, socket path and security
	// limits require a restart; silently changing them mid-call would be a
	// security surprise.
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.Identity.File != s.cfg.Identity.File {
		return nil, ipc.NewError(ipc.CodeConflict, "identity.file cannot change without a restart")
	}
	if cfg.Daemon.Socket != s.cfg.Daemon.Socket {
		return nil, ipc.NewError(ipc.CodeConflict, "daemon.socket cannot change without a restart")
	}
	restartNeeded := cfg.Signaling != s.cfg.Signaling ||
		cfg.Audio != s.cfg.Audio ||
		cfg.Network.ICETimeoutMS != s.cfg.Network.ICETimeoutMS
	s.cfg = cfg
	resp := map[string]any{"reloaded": true, "restart_required": restartNeeded}
	return resp, nil
}

func (s *Server) setMute(params jsonRaw) (any, error) {
	var p ipc.MuteParams
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	next := true
	if p.Muted != nil {
		next = *p.Muted
	} else {
		next = !s.muted.Load()
	}
	s.muted.Store(next)
	s.controllerMu.Lock()
	bridge := s.bridge
	s.controllerMu.Unlock()
	if bridge != nil {
		bridge.SetMuted(next)
	}
	active := len(s.peerSnapshot()) > 0
	s.log.Info("microphone state changed", "muted", next)
	return ipc.MuteResult{Muted: next, Active: active}, nil
}

// Stop requests a graceful shutdown. Used by tests.
func (s *Server) Stop() { s.closeOnce() }

// StartedAt reports process start time.
func (s *Server) StartedAt() time.Time { return s.started }

// RequestsServed reports how many IPC requests have been handled.
func (s *Server) RequestsServed() uint64 { return s.requests.Load() }
