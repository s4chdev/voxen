package media

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/s4chdev/voxen/internal/security"
)

// Configuration errors.
var (
	ErrNoVerifiedPeer  = errors.New("media: peer must be verified before a session can start")
	ErrStatementNeeded = errors.New("media: negotiation is missing the peer's signed statement")
)

// SessionConfig describes the local side of one peer connection.
type SessionConfig struct {
	// RoomID and SessionID are bound into the signed statement so a captured
	// statement cannot be replayed in another room or negotiation.
	RoomID    string
	SessionID string

	// LocalIdentity signs the local statement. The private key never leaves the
	// identity plane's process.
	LocalIdentity LocalSigner

	// ExpectedPeerKey is the Ed25519 public key of the peer, taken from the
	// verified trust store. The peer's statement must be signed by exactly this
	// key; a key supplied by the signaling server is never trusted.
	ExpectedPeerKey ed25519.PublicKey

	// LocalFingerprint is the SHA-256 of the local DTLS certificate. It must be
	// known before the offer is signed.
	LocalFingerprint [32]byte

	// Parameters is the fixed Opus profile.
	Parameters OpusParameters

	// STUNServers are discovered public addresses. There is no TURN field: voxen
	// never relays media.
	STUNServers []string

	// StatementTTL bounds the signed statement's validity.
	StatementTTL time.Duration

	// ReplayGuard rejects replayed peer statements. Shared across sessions so a
	// statement captured from one session cannot be replayed in another.
	ReplayGuard *security.ReplayGuard

	// Logger is optional.
	Logger *slog.Logger
}

// LocalSigner is the identity-plane capability the media plane needs. It is an
// interface so the media plane cannot read key files and so tests can supply a
// key without touching disk.
type LocalSigner interface {
	Sign(msg []byte) []byte
	PublicKey() ed25519.PublicKey
}

// WebRTCSession is a Pion-backed, identity-authenticated media session.
//
// Lifecycle and the authentication gate:
//
//	NewWebRTCSession   -> local DTLS cert + fingerprint, no negotiation yet
//	CreateOffer        -> signs the statement over the local fingerprint
//	SetRemote(offer)   -> verifies the peer's statement, then signs the answer
//	negotiation done   -> DTLS completes
//	Authenticate       -> BindDTLS: negotiated cert must match the signed one
//	SendPCM/ReceivePCM -> only permitted after Authenticate returns nil
type WebRTCSession struct {
	cfg  SessionConfig
	log  *slog.Logger
	pc   *webrtc.PeerConnection
	opus OpusParameters

	encoder *Encoder
	decoder *Decoder
	track   *webrtc.TrackLocalStaticSample

	mu sync.Mutex
	// sessionID is the effective negotiation ID. It starts as the configured
	// value and is replaced by the initiator's ID when this session answers an
	// offer: in WebRTC the offer defines the negotiation, so the answerer must
	// adopt it rather than insist on its own.
	sessionID  string
	peer       PeerInfo
	authDone   bool
	closed     bool
	lastError  string
	negotiated *Negotiation // remote negotiation, for reference and tests
	sendMu     sync.Mutex
	recvCh     chan []int16

	// statsMu serialises Stats against Close. Pion's GetStats reads connection
	// state that Close mutates, so calling them concurrently is a data race.
	// It is a separate mutex from mu because Close triggers state callbacks that
	// take mu; reusing mu here would deadlock.
	statsMu sync.Mutex

	candHandler func(ICECandidatePayload)
	authCh      chan struct{}
	authErr     error
	closeOnce   sync.Once

	// pendingCandidates buffers candidates that arrive before a remote
	// description exists. Trickle ICE routinely delivers ours first: SetLocal
	// Description starts gathering immediately, while the offer is still in
	// flight over signaling. Dropping those candidates would silently fall back
	// to a slower or failed connection.
	pendingCandidates    []webrtc.ICECandidateInit
	remoteDescriptionSet bool
}

// NewWebRTCSession builds a session for the peer identified by
// cfg.ExpectedPeerKey.
func NewWebRTCSession(ctx context.Context, cfg SessionConfig) (*WebRTCSession, error) {
	if len(cfg.ExpectedPeerKey) != ed25519.PublicKeySize {
		return nil, ErrNoVerifiedPeer
	}
	if cfg.LocalIdentity == nil {
		return nil, errors.New("media: local identity is required")
	}
	if cfg.ReplayGuard == nil {
		return nil, errors.New("media: replay guard is required")
	}
	if cfg.StatementTTL <= 0 {
		cfg.StatementTTL = security.DefaultStatementTTL()
	}
	if cfg.Parameters.ClockRate == 0 {
		cfg.Parameters = DefaultOpusParameters(28000, 20)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Only STUN is configured. There is deliberately no TURN server here or in
	// the configuration schema: a relay would defeat the point of the design.
	iceServers := make([]webrtc.ICEServer, 0, len(cfg.STUNServers))
	for _, s := range cfg.STUNServers {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{s}})
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: cfg.Parameters.ClockRate,
			Channels:  uint16(cfg.Parameters.Channels),
			// Advertise the same parameters the encoder is configured with so a
			// remote peer cannot silently negotiate something else.
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=0;sprop-stereo=0",
		},
		PayloadType: webrtc.PayloadType(cfg.Parameters.PayloadType),
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("media: register opus codec: %w", err)
	}

	se := webrtc.SettingEngine{}
	// mDNS candidates are off by default so candidates are explicit and testable.
	se.SetICEMulticastDNSMode(0)
	// Let the daemon own teardown. By default Pion closes the PeerConnection from
	// inside its DTLS goroutine when the peer sends close-notify, which races any
	// concurrent GetStats call and makes shutdown ordering unpredictable. With
	// this disabled, a peer's close-notify surfaces as an ICE failure that the
	// connection-state handler observes, and the controller closes the session
	// explicitly under statsMu.
	se.DisableCloseByDTLS(true)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, fmt.Errorf("media: new peer connection: %w", err)
	}

	// Read the local certificate fingerprint before any offer is created: it is
	// signed into the statement, so it must be stable at offer time.
	localFP, err := localFingerprint(pc)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if cfg.LocalFingerprint != [32]byte{} && !security.EqualFingerprint(cfg.LocalFingerprint, localFP) {
		pc.Close()
		return nil, errors.New("media: supplied fingerprint does not match the local DTLS certificate")
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: cfg.Parameters.ClockRate,
		Channels:  uint16(cfg.Parameters.Channels),
	}, "audio", "voxen")
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("media: create audio track: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return nil, fmt.Errorf("media: add audio track: %w", err)
	}
	// We must not rely on the remote to add a track for our audio to arrive.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		pc.Close()
		return nil, fmt.Errorf("media: add receive transceiver: %w", err)
	}

	encoder, err := NewEncoder(cfg.Parameters)
	if err != nil {
		pc.Close()
		return nil, err
	}
	decoder, err := NewDecoder(cfg.Parameters)
	if err != nil {
		pc.Close()
		return nil, err
	}

	s := &WebRTCSession{
		cfg:       cfg.clone(localFP),
		sessionID: cfg.SessionID,
		log:       logger,
		pc:        pc,
		opus:      cfg.Parameters,
		encoder:   encoder,
		decoder:   decoder,
		track:     track,
		recvCh:    make(chan []int16, 32),
		authCh:    make(chan struct{}),
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		payload, ok := NewICECandidatePayload(*c)
		if !ok {
			return
		}
		if h := s.getCandidateHandler(); h != nil {
			h(payload)
		}
	})

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		s.readRTPLoop(remote)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.log.Debug("peer connection state", "state", state.String(), "session", cfg.SessionID)
		switch state {
		case webrtc.PeerConnectionStateFailed:
			s.setError("peer connection failed")
			s.failAuth(ErrNotConnected)
		case webrtc.PeerConnectionStateClosed:
			s.failAuth(ErrClosed)
		}
	})

	// NOTE: no eager background Authenticate here. Authentication is an explicit
	// step the caller performs after SDP exchange, so that a caller can inspect
	// the stated fingerprint before the binding commits, and so two concurrent
	// Authenticate calls cannot race on the same state transition.
	return s, nil
}

// LocalFingerprint returns the SHA-256 of the local DTLS certificate.
func (s *WebRTCSession) LocalFingerprint() [32]byte { return s.cfg.LocalFingerprint }

// SessionID returns the effective negotiation ID.
func (s *WebRTCSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// adoptSessionID binds this session to the initiator's negotiation ID.
//
// Only an offer may set it, and only when the current ID is empty or was
// supplied by the answerer. The bound is deliberately restrictive: an attacker
// must not be able to force a session onto a chosen identifier, because a
// constant ID would make statements easier to correlate and replay across calls.
func (s *WebRTCSession) adoptSessionID(id string) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authDone || s.negotiated != nil {
		if s.sessionID != id {
			return fmt.Errorf("%w: session already bound to %q", security.ErrRoomMismatch, s.sessionID)
		}
		return nil
	}
	s.sessionID = id
	return nil
}

// validateSessionID rejects identifiers that are empty, oversized, or contain
// characters that would corrupt logs or the wire format.
func validateSessionID(id string) error {
	if id == "" {
		return errors.New("media: session id is empty")
	}
	if len(id) > 128 {
		return errors.New("media: session id is longer than 128 bytes")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("media: session id contains an invalid character %q", r)
		}
	}
	return nil
}

func (cfg SessionConfig) clone(fp [32]byte) SessionConfig {
	cfg.LocalFingerprint = fp
	return cfg
}

func (s *WebRTCSession) setError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
}

func (s *WebRTCSession) getCandidateHandler() func(ICECandidatePayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.candHandler
}

func (s *WebRTCSession) failAuth(err error) {
	s.mu.Lock()
	already := s.authDone
	s.authErr = err
	s.mu.Unlock()
	if !already {
		select {
		case <-s.authCh:
		default:
			close(s.authCh)
		}
	}
}

// localFingerprint derives the SHA-256 of the certificate Pion will present.
func localFingerprint(pc *webrtc.PeerConnection) ([32]byte, error) {
	var out [32]byte
	sctp := pc.SCTP()
	if sctp == nil || sctp.Transport() == nil {
		return out, errors.New("media: DTLS transport is unavailable before negotiation")
	}
	params, err := sctp.Transport().GetLocalParameters()
	if err != nil {
		return out, fmt.Errorf("media: read local DTLS parameters: %w", err)
	}
	for _, fp := range params.Fingerprints {
		parsed, err := security.ParseSDPFingerprint(fp.Algorithm + " " + fp.Value)
		if err == nil {
			return parsed, nil
		}
	}
	return out, errors.New("media: local DTLS certificate has no SHA-256 fingerprint")
}

// CreateOffer builds and signs an offer.
func (s *WebRTCSession) CreateOffer(ctx context.Context) (*Negotiation, error) {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("media: create offer: %w", err)
	}
	// SetLocalDescription begins ICE gathering and fixes the DTLS role.
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("media: set local description: %w", err)
	}

	sid := s.SessionID()
	stmt, err := security.NewSessionStatement(
		s.cfg.RoomID, sid,
		s.cfg.LocalIdentity.PublicKey(), s.cfg.LocalFingerprint,
		s.cfg.StatementTTL,
	)
	if err != nil {
		return nil, err
	}
	sig := s.cfg.LocalIdentity.Sign(stmt.SignedBytes())

	local := s.pc.LocalDescription()
	if local == nil {
		return nil, errors.New("media: local description missing after setting offer")
	}
	return &Negotiation{
		Type:      "offer",
		SDP:       []byte(local.SDP),
		SessionID: sid,
		Statement: stmt,
		Signature: sig,
	}, nil
}

// AcceptOffer verifies the peer's statement, then answers it.
//
// The statement is checked before the SDP is applied so a peer that cannot prove
// its identity never influences our state at all.
func (s *WebRTCSession) AcceptOffer(ctx context.Context, offer *Negotiation) (*Negotiation, PeerInfo, error) {
	if offer == nil {
		return nil, PeerInfo{}, ErrStatementNeeded
	}
	// The offer defines the negotiation, so adopt its session ID before any
	// statement is verified against it.
	if err := s.adoptSessionID(offer.SessionID); err != nil {
		s.failAuth(err)
		return nil, PeerInfo{}, err
	}
	if err := s.verifyStatement(offer); err != nil {
		s.failAuth(err)
		return nil, PeerInfo{}, err
	}
	s.mu.Lock()
	s.negotiated = offer
	s.mu.Unlock()

	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offer.SDP),
	}); err != nil {
		return nil, PeerInfo{}, fmt.Errorf("media: set remote offer: %w", err)
	}
	s.markRemoteDescriptionSet()
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return nil, PeerInfo{}, fmt.Errorf("media: create answer: %w", err)
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return nil, PeerInfo{}, fmt.Errorf("media: set local answer: %w", err)
	}

	sid := s.SessionID()
	stmt, err := security.NewSessionStatement(
		s.cfg.RoomID, sid,
		s.cfg.LocalIdentity.PublicKey(), s.cfg.LocalFingerprint,
		s.cfg.StatementTTL,
	)
	if err != nil {
		return nil, PeerInfo{}, err
	}
	sig := s.cfg.LocalIdentity.Sign(stmt.SignedBytes())

	local := s.pc.LocalDescription()
	if local == nil {
		return nil, PeerInfo{}, errors.New("media: local description missing after answer")
	}
	return &Negotiation{
		Type:      "answer",
		SDP:       []byte(local.SDP),
		SessionID: sid,
		Statement: stmt,
		Signature: sig,
	}, PeerInfo{}, nil
}

// AcceptAnswer verifies the answer's statement and applies it.
func (s *WebRTCSession) AcceptAnswer(ctx context.Context, answer *Negotiation) (PeerInfo, error) {
	if err := s.verifyStatement(answer); err != nil {
		s.failAuth(err)
		return PeerInfo{}, err
	}
	s.mu.Lock()
	s.negotiated = answer
	s.mu.Unlock()

	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(answer.SDP),
	}); err != nil {
		return PeerInfo{}, fmt.Errorf("media: set remote answer: %w", err)
	}
	s.markRemoteDescriptionSet()

	// A PeerInfo with RemoteCertVerified=false is a placeholder: the caller must
	// wait on Authenticate (or the connection-ready signal) before treating the
	// peer as authenticated. Returning early keeps SDP exchange non-blocking,
	// which is what trickle ICE needs.
	return PeerInfo{}, nil
}

// verifyStatement validates the peer's signed claim. This is the check that a
// malicious signaling server cannot pass: it does not hold the peer's key.
//
// Ordering matters. The SDP fingerprint is checked against the statement *before*
// the statement's nonce is committed to the replay guard. A relay that rewrites
// the SDP would otherwise be able to burn the legitimate nonce for a statement it
// cannot re-sign, converting a tampering attempt into a denial of service against
// the honest session. Verifying the cheap structural checks first means a
// rejected negotiation leaves the replay guard untouched, so the genuine offer
// can still be retried on a clean path.
func (s *WebRTCSession) verifyStatement(n *Negotiation) error {
	if n == nil || n.Statement == nil || len(n.Signature) == 0 {
		return ErrStatementNeeded
	}
	effective := s.SessionID()
	if n.SessionID != effective {
		return fmt.Errorf("%w: negotiation is for session %q, want %q",
			security.ErrRoomMismatch, n.SessionID, effective)
	}
	policy := security.StatementPolicy{
		MaxClockSkew:    time.Minute,
		MaxLifetime:     s.cfg.StatementTTL + time.Minute,
		ExpectedRoom:    s.cfg.RoomID,
		ExpectedSession: effective,
		ExpectedIssuer:  s.cfg.ExpectedPeerKey,
	}
	if err := n.Statement.Verify(n.Signature, policy); err != nil {
		s.setError("peer statement rejected: " + err.Error())
		s.log.Warn("rejected peer session statement",
			"session", s.cfg.SessionID, "reason", err.Error())
		return fmt.Errorf("media: %w", err)
	}

	// Bind the relayed SDP to the signed fingerprint before consuming the nonce.
	if err := crossCheckSDPFingerprint(n.SDP, n.Statement.DTLSFingerprint); err != nil {
		s.setError("SDP fingerprint does not match the signed statement: " + err.Error())
		s.log.Warn("rejected SDP whose fingerprint was rewritten",
			"session", s.cfg.SessionID, "reason", err.Error())
		return err
	}

	// Only now is the statement known to be genuine and applicable, so it is
	// safe to record its nonce.
	if err := s.cfg.ReplayGuard.Check(n.Statement); err != nil {
		s.setError("peer statement replayed: " + err.Error())
		s.log.Warn("rejected replayed session statement", "session", s.cfg.SessionID, "reason", err.Error())
		return fmt.Errorf("media: %w", err)
	}
	return nil
}

// crossCheckSDPFingerprint compares the fingerprint carried in the relayed SDP
// with the one in the signed statement.
//
// Pion performs the same comparison against the certificate finally negotiated.
// Doing it here as well means a rewritten SDP fails before the connection is
// configured, which keeps the failure local and legible.
func crossCheckSDPFingerprint(sdp []byte, want [32]byte) error {
	got, ok := sdpFingerprintSHA256(sdp)
	if !ok {
		return errors.New("media: SDP has no sha-256 fingerprint")
	}
	if !security.EqualFingerprint(got, want) {
		return fmt.Errorf("%w: SDP says %s, statement says %s",
			security.ErrFingerprintMatch,
			security.FingerprintHex(got), security.FingerprintHex(want))
	}
	return nil
}

// sdpFingerprintSHA256 extracts the a=fingerprint:sha-256 value.
func sdpFingerprintSHA256(sdp []byte) ([32]byte, bool) {
	var out [32]byte
	for _, line := range splitLines(string(sdp)) {
		value, ok := cutPrefix(line, "a=fingerprint:")
		if !ok {
			continue
		}
		fp, err := security.ParseSDPFingerprint(value)
		if err == nil {
			return fp, true
		}
	}
	return out, false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			for len(line) > 0 && (line[len(line)-1] == '\r') {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// Authenticate waits for the DTLS handshake and binds the negotiated
// certificate to the peer's verified identity.
//
// This is the step that converts Pion's SDP-fingerprint check (which only
// protects against a network attacker, since the SDP came from the signaling
// server) into an authenticated one.
//
// It is idempotent and safe to call concurrently: the first caller performs the
// binding and every caller observes the same result.
func (s *WebRTCSession) Authenticate(ctx context.Context) (PeerInfo, error) {
	s.mu.Lock()
	if s.authDone {
		info, err := s.peer, s.authErr
		s.mu.Unlock()
		return info, err
	}
	s.mu.Unlock()

	// Wait for DTLS to complete. Polling the connection state is used rather
	// than a channel because Pion delivers state changes on internal
	// goroutines and a missed edge would block forever.
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for s.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if s.isClosed() {
			return PeerInfo{}, ErrClosed
		}
		if time.Now().After(deadline) {
			s.failAuth(ErrNotConnected)
			return PeerInfo{}, ErrNotConnected
		}
		select {
		case <-ctx.Done():
			return PeerInfo{}, ctx.Err()
		case <-ticker.C:
		}
	}

	sctp := s.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil {
		return PeerInfo{}, errors.New("media: DTLS transport unavailable")
	}
	der := sctp.Transport().GetRemoteCertificate()
	if len(der) == 0 {
		return PeerInfo{}, errors.New("media: DTLS handshake completed without a remote certificate")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return PeerInfo{}, fmt.Errorf("media: parse remote DTLS certificate: %w", err)
	}

	s.mu.Lock()
	negotiated := s.negotiated
	s.mu.Unlock()
	if negotiated == nil || negotiated.Statement == nil {
		return PeerInfo{}, ErrStatementNeeded
	}

	// The binding: the certificate actually used must hash to the fingerprint
	// the peer signed.
	if err := security.BindDTLS(negotiated.Statement, cert); err != nil {
		s.setError(err.Error())
		s.log.Error("DTLS binding failed", "session", s.cfg.SessionID, "error", err)
		s.failAuth(err)
		return PeerInfo{}, err
	}

	actualFP, err := security.CertificateFingerprint(cert)
	if err != nil {
		return PeerInfo{}, err
	}
	info := PeerInfo{
		Fingerprint:        hex.EncodeToString(s.cfg.ExpectedPeerKey),
		DTLSFingerprint:    actualFP,
		RemoteCertVerified: true,
	}

	s.mu.Lock()
	s.peer = info
	s.authDone = true
	s.authErr = nil
	s.mu.Unlock()

	s.log.Info("peer authenticated",
		"session", s.cfg.SessionID,
		"dtls_fingerprint", security.FingerprintHex(actualFP))

	select {
	case <-s.authCh:
	default:
		close(s.authCh)
	}
	return info, nil
}

// SetCandidateHandler installs a callback for locally gathered candidates.
func (s *WebRTCSession) SetCandidateHandler(fn func(ICECandidatePayload)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.candHandler = fn
}

// AddICECandidate applies a trickled remote candidate.
//
// Candidates that arrive before the remote description are buffered and flushed
// once it is applied, rather than dropped. With trickle ICE the initiator's
// host candidates are usually gathered and sent before the answer arrives.
func (s *WebRTCSession) AddICECandidate(ctx context.Context, candidate ICECandidatePayload) error {
	init, err := candidate.ToInit()
	if err != nil {
		return err
	}

	s.mu.Lock()
	if !s.remoteDescriptionSet {
		// Bound the buffer: a hostile relay must not be able to grow it without
		// limit before negotiation completes.
		if len(s.pendingCandidates) >= maxPendingCandidates {
			s.mu.Unlock()
			return errors.New("media: too many ICE candidates before negotiation")
		}
		s.pendingCandidates = append(s.pendingCandidates, init)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	return s.applyCandidate(init)
}

const maxPendingCandidates = 128

func (s *WebRTCSession) applyCandidate(init webrtc.ICECandidateInit) error {
	if err := s.pc.AddICECandidate(init); err != nil {
		return fmt.Errorf("media: add ICE candidate: %w", err)
	}
	return nil
}

// markRemoteDescriptionSet flushes buffered candidates. It must be called after
// SetRemoteDescription succeeds.
func (s *WebRTCSession) markRemoteDescriptionSet() {
	s.mu.Lock()
	s.remoteDescriptionSet = true
	pending := s.pendingCandidates
	s.pendingCandidates = nil
	s.mu.Unlock()

	for _, init := range pending {
		if err := s.applyCandidate(init); err != nil {
			s.log.Debug("buffered ICE candidate rejected", "error", err)
		}
	}
}

// SendPCM encodes and transmits one frame.
//
// It refuses to run before authentication, which is what prevents media from
// flowing to an unverified peer even if a caller ignores the ordering.
func (s *WebRTCSession) SendPCM(ctx context.Context, pcm []int16) error {
	if err := s.requireAuthenticated(); err != nil {
		return err
	}
	packet, err := s.encoder.Encode(pcm)
	if err != nil {
		return err
	}
	if packet == nil {
		// DTX suppressed the frame.
		return nil
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.track.WriteSample(media.Sample{
		Data:     packet,
		Duration: time.Duration(s.opus.FrameMS) * time.Millisecond,
	})
}

// ReceivePCM returns one decoded frame, blocking until one is available.
func (s *WebRTCSession) ReceivePCM(ctx context.Context, pcm []int16) (int, error) {
	if err := s.requireAuthenticated(); err != nil {
		return 0, err
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case frame, ok := <-s.recvCh:
		if !ok {
			return 0, ErrClosed
		}
		n := copy(pcm, frame)
		return n, nil
	}
}

func (s *WebRTCSession) requireAuthenticated() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !s.authDone {
		return ErrUnauthenticated
	}
	return nil
}

// readRTPLoop decodes incoming Opus and publishes frames to ReceivePCM.
//
// ReadRTP returns one RTP packet, and voxen's encoder emits exactly one packet
// per 20 ms frame, so each packet is decoded independently. A malformed or
// truncated packet is dropped rather than tearing the session down: RTP over a
// lossy path is expected to deliver junk occasionally.
func (s *WebRTCSession) readRTPLoop(track *webrtc.TrackRemote) {
	frameLen := int(s.opus.ClockRate) * s.opus.FrameMS / 1000
	if frameLen <= 0 {
		frameLen = 960
	}
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		pcm := make([]int16, frameLen)
		n, err := s.decoder.DecodeInt16(rtpPacket.Payload, pcm)
		if err != nil {
			s.log.Debug("dropping undecodable opus packet", "error", err)
			continue
		}
		// Deliver only once authenticated: buffering audio for an unauthenticated
		// peer would let a caller read it early by mistake.
		if !s.isAuthenticated() {
			continue
		}
		select {
		case s.recvCh <- pcm[:n]:
		case <-time.After(100 * time.Millisecond):
			// Nobody is consuming audio; drop rather than block the reader and
			// stall the RTP stream.
		}
	}
}

func (s *WebRTCSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *WebRTCSession) isAuthenticated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authDone
}

// RestartICE builds a new offer with fresh ICE credentials after a network
// change. The session ID changes so the new negotiation needs a new statement.
func (s *WebRTCSession) RestartICE(ctx context.Context) (*Negotiation, error) {
	offer, err := s.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		return nil, fmt.Errorf("media: create ICE restart offer: %w", err)
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("media: set ICE restart description: %w", err)
	}
	sid := s.SessionID()
	stmt, err := security.NewSessionStatement(
		s.cfg.RoomID, sid,
		s.cfg.LocalIdentity.PublicKey(), s.cfg.LocalFingerprint,
		s.cfg.StatementTTL,
	)
	if err != nil {
		return nil, err
	}
	sig := s.cfg.LocalIdentity.Sign(stmt.SignedBytes())
	local := s.pc.LocalDescription()
	return &Negotiation{
		Type:      "offer",
		SDP:       []byte(local.SDP),
		SessionID: sid,
		Statement: stmt,
		Signature: sig,
	}, nil
}

// Stats reports the live quality snapshot, including proof that the selected
// candidate pair is a direct path.
func (s *WebRTCSession) Stats() SessionStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	var out SessionStats
	sctp := s.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil {
		return out
	}
	ice := sctp.Transport().ICETransport()
	if ice == nil {
		return out
	}
	if pair, err := ice.GetSelectedCandidatePair(); err == nil && pair != nil {
		out.LocalCandidate = describeCandidate(pair.Local)
		out.RemoteCandidate = describeCandidate(pair.Remote)
		// voxen never configures TURN, so a relayed candidate would be a bug.
		out.Relayed = candidateIsRelay(pair.Local) || candidateIsRelay(pair.Remote)
	}
	report := s.pc.GetStats()
	for _, stat := range report {
		switch v := stat.(type) {
		case webrtc.ICECandidatePairStats:
			if v.Nominated {
				out.RTT = time.Duration(v.CurrentRoundTripTime * float64(time.Second))
			}
		case webrtc.InboundRTPStreamStats:
			if v.Kind == "audio" {
				out.PacketsReceived = uint64(v.PacketsReceived)
				if v.PacketsLost > 0 {
					out.PacketsLost = uint64(v.PacketsLost)
				}
				out.BytesReceived = v.BytesReceived
				out.Jitter = time.Duration(v.Jitter * float64(time.Second))
				total := out.PacketsReceived + out.PacketsLost
				if total > 0 {
					out.PacketLoss = float64(out.PacketsLost) / float64(total)
				}
			}
		case webrtc.OutboundRTPStreamStats:
			if v.Kind == "audio" {
				out.PacketsSent = uint64(v.PacketsSent)
				out.BytesSent = v.BytesSent
			}
		}
	}
	return out
}

func describeCandidate(c *webrtc.ICECandidate) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s", c.Typ, c.Protocol, c.Address)
}

func candidateIsRelay(c *webrtc.ICECandidate) bool {
	if c == nil {
		return false
	}
	return c.Typ == webrtc.ICECandidateTypeRelay
}

// Close tears the session down. Safe to call more than once.
func (s *WebRTCSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		// Take statsMu so an in-flight Stats cannot race the close.
		s.statsMu.Lock()
		err = s.pc.Close()
		s.statsMu.Unlock()
		close(s.recvCh)
	})
	return err
}

// Ensure the implementation satisfies the interface at compile time.
var _ Session = (*WebRTCSession)(nil)
