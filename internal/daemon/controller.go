package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/media"
	"github.com/s4chdev/voxen/internal/room"
	"github.com/s4chdev/voxen/internal/security"
	"github.com/s4chdev/voxen/internal/signaling"
	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// Controller errors surfaced to the CLI.
var (
	ErrNoRoom        = errors.New("controller: not in a room")
	ErrRoomBusy      = errors.New("controller: already in a room")
	ErrSignalingDown = errors.New("controller: signaling server is unreachable")
)

// Controller joins the control plane (signaling + room state) to the media plane
// (authenticated peer sessions).
//
// The mesh rule: for every remote peer this process maintains one independently
// authenticated session. The peer with the lexicographically smaller public key
// sends the offer, which makes offer direction deterministic so two peers cannot
// both offer at once.
type Controller struct {
	media media.Manager
	trust PeerResolver
	log   Logger

	mu       sync.Mutex
	client   *signaling.Client
	room     *room.Room
	roomID   string
	sessions map[string]*peerSession // keyed by signaling peer ID
	identity *identity.Identity
	cfg      ControllerConfig

	// events is a bounded ring of recent lifecycle events for "voxen status".
	events []ipc.RoomEvent
	closed bool
	wg     sync.WaitGroup
}

// PeerResolver is the identity-plane lookup the controller uses. It is injected
// so the controller cannot read key files directly.
type PeerResolver interface {
	// VerifiedKey returns the stored key for a verified fingerprint, if one is
	// bound.
	VerifiedKey(fingerprint identity.Fingerprint) (ed25519.PublicKey, bool)
	// IsPinned reports a verified fingerprint with no key bound yet.
	IsPinned(fingerprint identity.Fingerprint) bool
	// BindKey completes a pin with the key that was just observed. It fails if
	// the key does not hash to the pinned fingerprint, so a mismatched key can
	// never silently take over a pin.
	BindKey(pub ed25519.PublicKey) error
}

// Logger is the small logging surface the controller needs.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

// ControllerConfig is the runtime configuration for a room join.
type ControllerConfig struct {
	SignalingURL string
	DisplayName  string
	MaxMembers   int
	// JoinTimeout bounds the room handshake.
	JoinTimeout time.Duration
}

// peerSession is the local state for one remote participant.
type peerSession struct {
	peerID      string
	fingerprint string
	displayName string
	session     media.Session
	state       room.ConnectionState
	lastError   string
	// offerer records whether this side initiated negotiation, for diagnostics.
	offerer bool
}

// NewController builds a controller bound to a media manager and trust lookup.
func NewController(mediaMgr media.Manager, trust PeerResolver, id *identity.Identity, log Logger, cfg ControllerConfig) *Controller {
	if cfg.JoinTimeout <= 0 {
		cfg.JoinTimeout = 20 * time.Second
	}
	if cfg.MaxMembers <= 0 {
		cfg.MaxMembers = 5
	}
	return &Controller{
		media:    mediaMgr,
		trust:    trust,
		identity: id,
		log:      log,
		cfg:      cfg,
		sessions: make(map[string]*peerSession),
	}
}

// CreateRoom connects to signaling, creates a room and starts serving the mesh.
func (c *Controller) CreateRoom(ctx context.Context) (ipc.RoomResult, error) {
	c.mu.Lock()
	if c.roomID != "" {
		c.mu.Unlock()
		return ipc.RoomResult{}, ErrRoomBusy
	}
	c.mu.Unlock()

	client, err := c.connect(ctx)
	if err != nil {
		return ipc.RoomResult{}, err
	}

	joinCtx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
	defer cancel()
	roomID, members, err := client.CreateRoom(joinCtx)
	if err != nil {
		client.Close()
		return ipc.RoomResult{}, fmt.Errorf("controller: create room: %w", err)
	}

	if err := c.activate(ctx, client, roomID, members); err != nil {
		client.Close()
		return ipc.RoomResult{}, err
	}
	c.log.Info("room created", "room", roomID, "members", len(members))
	return ipc.RoomResult{
		Room: c.roomState(),
	}, nil
}

// JoinRoom joins an existing room and starts serving the mesh.
func (c *Controller) JoinRoom(ctx context.Context, roomID string) (ipc.RoomResult, error) {
	if roomID == "" {
		return ipc.RoomResult{}, errors.New("controller: room id is required")
	}
	c.mu.Lock()
	if c.roomID != "" {
		c.mu.Unlock()
		return ipc.RoomResult{}, ErrRoomBusy
	}
	c.mu.Unlock()

	client, err := c.connect(ctx)
	if err != nil {
		return ipc.RoomResult{}, err
	}

	joinCtx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
	defer cancel()
	members, err := client.JoinRoom(joinCtx, roomID)
	if err != nil {
		client.Close()
		return ipc.RoomResult{}, fmt.Errorf("controller: join room: %w", err)
	}

	if err := c.activate(ctx, client, roomID, members); err != nil {
		client.Close()
		return ipc.RoomResult{}, err
	}
	c.log.Info("joined room", "room", roomID, "members", len(members))
	return ipc.RoomResult{Room: c.roomState()}, nil
}

func (c *Controller) connect(ctx context.Context) (*signaling.Client, error) {
	if c.cfg.SignalingURL == "" {
		return nil, errors.New("controller: no signaling URL configured")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := signaling.Connect(dialCtx, signaling.ClientConfig{
		URL:         c.cfg.SignalingURL,
		DisplayName: c.cfg.DisplayName,
		PublicKey:   hex.EncodeToString(c.identity.PublicKey),
		Timeout:     10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignalingDown, err)
	}
	return client, nil
}

// activate builds the room state machine and starts the relay loop.
//
// initialMembers is the roster returned by the join/create handshake. Those
// messages were consumed before the relay loop started, so existing members must
// be dialled explicitly here; otherwise a peer joining a room that already has
// participants would wait forever for a peer.join event that already happened.
func (c *Controller) activate(ctx context.Context, client *signaling.Client, roomID string, initialMembers []protocol.PeerInfo) error {
	trustAdapter := &trustChecker{resolver: c.trust}
	rm, err := room.New(room.Options{
		RoomID:          roomID,
		MaxParticipants: c.cfg.MaxMembers,
		Self:            c.identity,
		Trust:           trustAdapter,
		RosterPolicy: security.MembershipPolicy{
			ExpectedRoom: roomID,
			MaxClockSkew: time.Minute,
		},
	})
	if err != nil {
		return err
	}
	// A peer-created room has no separate administrator in this build: the local
	// identity acts as the roster authority, which is what a private room means.
	if err := rm.BeginJoin(room.RoleCreator); err != nil {
		return err
	}

	roster := security.NewRoomMembership(roomID, 1, c.identity.PublicKey, []security.Member{
		{PublicKey: c.identity.PublicKey, DisplayName: c.identity.DisplayName},
	}, 24*time.Hour)
	if err := rm.Activate(roster, 1); err != nil {
		return err
	}

	c.mu.Lock()
	c.client = client
	c.room = rm
	c.roomID = roomID
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.relayLoop(ctx, client)
	}()

	// Dial peers that were already in the room when we joined.
	for _, m := range initialMembers {
		if m.PeerID == client.PeerID() {
			continue
		}
		c.onPeerJoined(ctx, client, m)
	}
	return nil
}

// relayLoop is the single reader for the signaling connection. It must never
// block: authentication and other long operations run in their own goroutines so
// ICE candidates keep flowing.
func (c *Controller) relayLoop(ctx context.Context, client *signaling.Client) {
	for {
		env, err := client.Recv(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				c.recordEvent(ipc.RoomEvent{Kind: "signaling_closed", Message: err.Error()})
				c.log.Warn("signaling connection ended", "error", err)
			}
			return
		}
		c.handleEnvelope(ctx, client, env)
	}
}

func (c *Controller) handleEnvelope(ctx context.Context, client *signaling.Client, env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeRoomState:
		var payload protocol.RoomStatePayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return
		}
		for _, m := range payload.Members {
			if m.PeerID == client.PeerID() {
				continue
			}
			c.onPeerJoined(ctx, client, m)
		}
	case protocol.TypePeerJoin:
		var m protocol.PeerInfo
		if err := json.Unmarshal(env.Payload, &m); err != nil {
			return
		}
		if m.PeerID == client.PeerID() {
			return
		}
		c.onPeerJoined(ctx, client, m)
	case protocol.TypePeerLeave:
		c.onPeerLeft(ctx, env.From)
	case protocol.TypeOffer:
		c.onOffer(ctx, client, env)
	case protocol.TypeAnswer:
		c.onAnswer(ctx, client, env)
	case protocol.TypeICECandidate:
		c.onCandidate(ctx, env)
	case protocol.TypeError:
		var payload protocol.ErrorPayload
		_ = json.Unmarshal(env.Payload, &payload)
		c.recordEvent(ipc.RoomEvent{Kind: "signaling_error", Message: payload.Code + ": " + payload.Message})
		c.log.Warn("signaling error", "code", payload.Code, "message", payload.Message)
	}
}

// ensureSession returns the session for a peer, creating it if necessary.
//
// Both the offerer and the answerer call this. The answerer must be able to build
// a session on demand when an offer arrives: a peer may offer before this side has
// processed the corresponding peer.join, and dropping that offer would leave both
// sides waiting forever. Returns nil if a session already existed.
func (c *Controller) ensureSession(ctx context.Context, peerID string, pub ed25519.PublicKey, fingerprint, displayName string, weOffer bool) (*peerSession, error) {
	c.mu.Lock()
	if _, exists := c.sessions[peerID]; exists {
		c.mu.Unlock()
		return nil, nil
	}
	if len(c.sessions)+1 > c.cfg.MaxMembers {
		c.mu.Unlock()
		return nil, errors.New("room is full")
	}
	roomID := c.roomID
	c.mu.Unlock()

	// Only the offerer chooses the session ID. The answerer starts with a
	// placeholder and adopts the initiator's ID from the offer, because in WebRTC
	// the offer defines the negotiation.
	sessionID := "await-offer"
	if weOffer {
		generated, err := media.NewSessionID()
		if err != nil {
			return nil, err
		}
		sessionID = generated
	}

	sess, err := c.media.NewSession(ctx, media.SessionSpec{
		RoomID:        roomID,
		SessionID:     sessionID,
		PeerPublicKey: pub,
	})
	if err != nil {
		return nil, err
	}

	ps := &peerSession{
		peerID:      peerID,
		fingerprint: fingerprint,
		displayName: displayName,
		session:     sess,
		state:       room.ConnConnecting,
		offerer:     weOffer,
	}

	c.mu.Lock()
	if _, exists := c.sessions[peerID]; exists {
		// Lost a race with a concurrent caller; discard ours.
		c.mu.Unlock()
		_ = sess.Close()
		return nil, nil
	}
	c.sessions[peerID] = ps
	c.mu.Unlock()

	sess.SetCandidateHandler(func(cand media.ICECandidatePayload) {
		raw, err := json.Marshal(cand)
		if err != nil {
			return
		}
		c.sendToPeer(ctx, peerID, protocol.TypeICECandidate, raw)
	})
	return ps, nil
}

// peerKeyFromStatement derives a peer's key from its signed statement and
// authorises it against the trust store.
//
// The statement is attacker-supplied, so its key is used only as a candidate: it
// must hash to a fingerprint the local user verified. That is the same rule
// applied to keys advertised over signaling, just reached through a different
// message, so a peer cannot bypass verification by offering first.
func (c *Controller) peerKeyFromStatement(stmt *security.SessionStatement, peerID string) (ed25519.PublicKey, string, string, error) {
	if stmt == nil || len(stmt.IssuerPublicKey) != ed25519.PublicKeySize {
		return nil, "", "", errors.New("offer has no usable identity statement")
	}
	candidate := ed25519.PublicKey(stmt.IssuerPublicKey)
	fp := identity.FingerprintFromPublic(candidate)

	if verified, ok := c.trust.VerifiedKey(fp); ok {
		return verified, fp.String(), "", nil
	}
	if c.trust.IsPinned(fp) {
		if err := c.trust.BindKey(candidate); err != nil {
			return nil, "", "", fmt.Errorf("pinned identity could not be bound: %w", err)
		}
		c.log.Info("completed a fingerprint pin from an offer", "fingerprint", fp.String(), "peer", peerID)
		return candidate, fp.String(), "", nil
	}
	return nil, "", "", fmt.Errorf("identity %s is not verified; run 'voxen verify %s' first", fp.Short(), fp)
}

// sendToPeer relays one envelope to a peer over the current signaling client.
func (c *Controller) sendToPeer(ctx context.Context, peerID, typ string, payload json.RawMessage) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return
	}
	_ = client.Send(ctx, protocol.Envelope{
		Version: protocol.ProtocolVersion, Type: typ, To: peerID, Payload: payload,
	})
}

// onPeerJoined decides whether to serve or initiate a session with a new peer.
func (c *Controller) onPeerJoined(ctx context.Context, client *signaling.Client, peer protocol.PeerInfo) {
	pub, fingerprint, err := c.peerKey(peer)
	if err != nil {
		c.recordEvent(ipc.RoomEvent{
			Kind: "peer_rejected", PeerID: peer.PeerID, DisplayName: peer.DisplayName,
			Message: err.Error(),
		})
		c.log.Warn("refusing a peer whose identity is not verified",
			"peer", peer.PeerID, "display_name", peer.DisplayName, "reason", err.Error())
		return
	}

	weOffer := compareKeys(c.identity.PublicKey, pub) < 0
	ps, err := c.ensureSession(ctx, peer.PeerID, pub, fingerprint, peer.DisplayName, weOffer)
	if err != nil {
		c.recordEvent(ipc.RoomEvent{
			Kind: "peer_failed", PeerID: peer.PeerID, Message: err.Error(),
		})
		c.log.Error("cannot create media session", "peer", peer.PeerID, "error", err)
		return
	}
	if ps == nil {
		// Already had a session; nothing more to do.
		return
	}
	sess := ps.session

	if weOffer {
		// Offer from a goroutine: CreateOffer is quick, but authentication must
		// never run on the relay reader.
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.startOffer(ctx, client, peer.PeerID, sess)
		}()
	}
	c.recordEvent(ipc.RoomEvent{
		Kind: "peer_connected", PeerID: peer.PeerID, DisplayName: peer.DisplayName,
		Fingerprint: fingerprint,
	})
}

// startOffer negotiates as the initiating side and waits for authentication.
func (c *Controller) startOffer(ctx context.Context, client *signaling.Client, peerID string, sess media.Session) {
	offer, err := sess.CreateOffer(ctx)
	if err != nil {
		c.failSession(peerID, fmt.Errorf("create offer: %w", err))
		return
	}
	raw, err := json.Marshal(offer)
	if err != nil {
		c.failSession(peerID, err)
		return
	}
	if err := client.Send(ctx, protocol.Envelope{
		Version: protocol.ProtocolVersion, Type: protocol.TypeOffer,
		To: peerID, Payload: raw,
	}); err != nil {
		c.failSession(peerID, fmt.Errorf("send offer: %w", err))
	}
}

// onOffer answers an incoming offer.
func (c *Controller) onOffer(ctx context.Context, client *signaling.Client, env protocol.Envelope) {
	var offer media.Negotiation
	if err := json.Unmarshal(env.Payload, &offer); err != nil {
		c.recordEvent(ipc.RoomEvent{Kind: "protocol_error", Message: "malformed offer"})
		return
	}

	c.mu.Lock()
	ps, ok := c.sessions[env.From]
	sess := media.Session(nil)
	if ok {
		sess = ps.session
	}
	c.mu.Unlock()

	if !ok {
		// The offer may arrive before this side processed the peer.join for the
		// sender. Build the session now instead of dropping the offer: dropping it
		// leaves both peers waiting until the negotiation times out.
		pub, fingerprint, displayName, err := c.peerKeyFromStatement(offer.Statement, env.From)
		if err != nil {
			c.recordEvent(ipc.RoomEvent{
				Kind: "peer_rejected", PeerID: env.From,
				Message: "offer from a peer that cannot be authenticated: " + err.Error(),
			})
			return
		}
		created, err := c.ensureSession(ctx, env.From, pub, fingerprint, displayName, false)
		if err != nil {
			c.recordEvent(ipc.RoomEvent{
				Kind: "peer_failed", PeerID: env.From, Message: "cannot answer offer: " + err.Error(),
			})
			return
		}
		if created == nil {
			// A concurrent path created it; retry the lookup.
			c.mu.Lock()
			ps, ok = c.sessions[env.From]
			if ok {
				sess = ps.session
			}
			c.mu.Unlock()
			if !ok {
				return
			}
		} else {
			sess = created.session
		}
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		answer, _, err := sess.AcceptOffer(ctx, &offer)
		if err != nil {
			c.failSession(env.From, err)
			return
		}
		raw, err := json.Marshal(answer)
		if err != nil {
			c.failSession(env.From, err)
			return
		}
		if err := client.Send(ctx, protocol.Envelope{
			Version: protocol.ProtocolVersion, Type: protocol.TypeAnswer,
			To: env.From, Payload: raw,
		}); err != nil {
			c.failSession(env.From, fmt.Errorf("send answer: %w", err))
			return
		}
		c.awaitAuth(ctx, env.From, sess)
	}()
}

// onAnswer completes negotiation on the initiating side.
func (c *Controller) onAnswer(ctx context.Context, client *signaling.Client, env protocol.Envelope) {
	var answer media.Negotiation
	if err := json.Unmarshal(env.Payload, &answer); err != nil {
		c.recordEvent(ipc.RoomEvent{Kind: "protocol_error", Message: "malformed answer"})
		return
	}

	c.mu.Lock()
	ps, ok := c.sessions[env.From]
	sess := media.Session(nil)
	if ok {
		sess = ps.session
	}
	c.mu.Unlock()
	if !ok {
		return
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if _, err := sess.AcceptAnswer(ctx, &answer); err != nil {
			c.failSession(env.From, err)
			return
		}
		c.awaitAuth(ctx, env.From, sess)
	}()
}

// awaitAuth blocks until the identity binding completes, then marks the peer
// connected. Only after this may audio flow.
func (c *Controller) awaitAuth(ctx context.Context, peerID string, sess media.Session) {
	authCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := sess.Authenticate(authCtx)
	if err != nil {
		c.failSession(peerID, err)
		return
	}
	if !info.RemoteCertVerified {
		c.failSession(peerID, errors.New("peer certificate was not verified"))
		return
	}

	c.mu.Lock()
	if ps, ok := c.sessions[peerID]; ok {
		ps.state = room.ConnConnected
		ps.lastError = ""
	}
	c.mu.Unlock()

	c.log.Info("peer session authenticated",
		"peer", peerID, "dtls_fingerprint", security.FingerprintHex(info.DTLSFingerprint))
	c.recordEvent(ipc.RoomEvent{
		Kind: "peer_authenticated", PeerID: peerID,
		Message: "direct encrypted path established",
	})
}

// onCandidate applies a trickled candidate.
func (c *Controller) onCandidate(ctx context.Context, env protocol.Envelope) {
	var cand media.ICECandidatePayload
	if err := json.Unmarshal(env.Payload, &cand); err != nil {
		return
	}
	c.mu.Lock()
	ps, ok := c.sessions[env.From]
	var sess media.Session
	if ok {
		sess = ps.session
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if err := sess.AddICECandidate(ctx, cand); err != nil {
		c.log.Debug("candidate rejected", "peer", env.From, "error", err)
	}
}

func (c *Controller) failSession(peerID string, err error) {
	// Give the user the actual reason. "no direct path" and "authentication
	// failed" call for completely different responses, and a generic message
	// would hide which one happened.
	reason := err.Error()
	if errors.Is(err, media.ErrNoDirectRoute) {
		reason = "no direct path to peer. voxen does not relay media, so this call " +
			"cannot connect; both sides need a network that allows direct UDP or a STUN server that does"
	}
	c.mu.Lock()
	if ps, ok := c.sessions[peerID]; ok {
		ps.state = room.ConnFailed
		ps.lastError = reason
		if ps.session != nil {
			_ = ps.session.Close()
		}
		delete(c.sessions, peerID)
	}
	c.mu.Unlock()
	c.log.Warn("peer session failed", "peer", peerID, "error", err)
	c.recordEvent(ipc.RoomEvent{Kind: "peer_failed", PeerID: peerID, Message: reason})
}

// LeaveRoom tears down every session and disconnects from signaling.
func (c *Controller) LeaveRoom(reason string) error {
	c.mu.Lock()
	client := c.client
	rm := c.room
	hadRoom := c.roomID != ""
	roomID := c.roomID
	sessions := make([]*peerSession, 0, len(c.sessions))
	for _, ps := range c.sessions {
		sessions = append(sessions, ps)
	}
	c.sessions = make(map[string]*peerSession)
	c.client = nil
	c.room = nil
	c.roomID = ""
	c.mu.Unlock()

	if !hadRoom {
		return ErrNoRoom
	}
	if rm != nil {
		rm.Leave()
	}
	for _, ps := range sessions {
		if ps.session != nil {
			_ = ps.session.Close()
		}
	}
	if client != nil {
		_ = client.Send(context.Background(), protocol.Envelope{
			Version: protocol.ProtocolVersion, Type: protocol.TypeRoomLeave,
		})
		_ = client.Close()
	}
	c.log.Info("left room", "room", roomID, "reason", reason)
	c.recordEvent(ipc.RoomEvent{Kind: "left", Message: reason})
	return nil
}

func (c *Controller) onPeerLeft(_ context.Context, peerID string) {
	c.mu.Lock()
	ps, ok := c.sessions[peerID]
	delete(c.sessions, peerID)
	c.mu.Unlock()
	if !ok {
		return
	}
	if ps.session != nil {
		_ = ps.session.Close()
	}
	c.log.Info("peer left", "peer", peerID)
	c.recordEvent(ipc.RoomEvent{Kind: "peer_left", PeerID: peerID})
}

// peerKey validates that a peer's advertised key is one the local user verified.
func (c *Controller) peerKey(peer protocol.PeerInfo) (ed25519.PublicKey, string, error) {
	raw, err := hex.DecodeString(peer.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, "", errors.New("peer advertised an invalid public key")
	}
	pub := ed25519.PublicKey(raw)
	fp := identity.FingerprintFromPublic(pub)

	// Case 1: the key is stored and verified.
	if verified, ok := c.trust.VerifiedKey(fp); ok {
		return verified, fp.String(), nil
	}

	// Case 2: the user pinned this fingerprint before meeting the peer. The
	// advertised key hashes to the pin by construction (fp is its hash), so
	// accepting it is exactly what the pin authorised. Bind it so the store
	// records which key satisfied the pin.
	if c.trust.IsPinned(fp) {
		if err := c.trust.BindKey(pub); err != nil {
			return nil, "", fmt.Errorf("identity %s is pinned but could not be bound: %w", fp.Short(), err)
		}
		c.log.Info("completed a fingerprint pin", "fingerprint", fp.String(), "display_name", peer.DisplayName)
		return pub, fp.String(), nil
	}

	return nil, "", fmt.Errorf(
		"identity %s is not verified; run 'voxen verify %s' first", fp.Short(), fp)
}

// RoomState reports the local view for IPC.
func (c *Controller) roomState() ipc.RoomState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roomID == "" {
		return ipc.RoomState{}
	}
	state := "active"
	if c.room == nil {
		state = "inactive"
	}
	return ipc.RoomState{
		InRoom:    true,
		RoomID:    c.roomID,
		Role:      string(room.RoleCreator),
		JoinState: state,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// Sessions returns the live media sessions, for the audio bridge to fan out to.
//
// Only authenticated sessions are returned: sending to a session before the
// identity binding completes is refused by the session itself, but excluding it
// here avoids pointless work and keeps the capture loop cheap.
func (c *Controller) Sessions() []media.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]media.Session, 0, len(c.sessions))
	for _, ps := range c.sessions {
		if ps.session != nil && ps.state == room.ConnConnected {
			out = append(out, ps.session)
		}
	}
	return out
}

// Peers reports per-peer state for IPC.
func (c *Controller) Peers() []ipc.PeerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ipc.PeerState, 0, len(c.sessions))
	for _, ps := range c.sessions {
		stats := media.SessionStats{}
		if ps.session != nil {
			stats = ps.session.Stats()
		}
		out = append(out, ipc.PeerState{
			ID:          ps.peerID,
			DisplayName: ps.displayName,
			Fingerprint: ps.fingerprint,
			Verified:    true,
			Connection:  string(ps.state),
			RTTMillis:   int(stats.RTT / time.Millisecond),
			PacketLoss:  stats.PacketLoss,
			LastError:   ps.lastError,
			Direct:      !stats.Relayed && stats.LocalCandidate != "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SignalingState reports the connection state for "voxen status".
func (c *Controller) SignalingState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return string(signaling.StateDisconnected)
	}
	return string(c.client.State())
}

// Events returns recent lifecycle events, oldest first.
func (c *Controller) Events() []ipc.RoomEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ipc.RoomEvent, len(c.events))
	copy(out, c.events)
	return out
}

// maxEvents bounds the event ring.
const maxEvents = 64

func (c *Controller) recordEvent(e ipc.RoomEvent) {
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) >= maxEvents {
		copy(c.events, c.events[1:])
		c.events[maxEvents-1] = e
		return
	}
	c.events = append(c.events, e)
}

// Close stops the controller and every session. Safe to call more than once.
func (c *Controller) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	client := c.client
	c.mu.Unlock()

	_ = c.LeaveRoom("shutdown")
	if client != nil {
		_ = client.Close()
	}
	c.wg.Wait()
}

// trustChecker adapts the injected resolver to the room's TrustChecker interface.
type trustChecker struct {
	resolver PeerResolver
}

func (t *trustChecker) IsVerified(fp identity.Fingerprint) bool {
	if _, ok := t.resolver.VerifiedKey(fp); ok {
		return true
	}
	// A pin is a verified trust decision even before its key is bound: the user
	// compared the fingerprint. The key check happens at session setup.
	return t.resolver.IsPinned(fp)
}

func (t *trustChecker) Level(fp identity.Fingerprint) string {
	if t.IsVerified(fp) {
		return string(identity.TrustVerified)
	}
	return string(identity.TrustUnverified)
}

// compareKeys orders public keys lexicographically to pick the offerer.
func compareKeys(a, b ed25519.PublicKey) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
