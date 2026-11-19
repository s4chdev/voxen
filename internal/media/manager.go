package media

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/s4chdev/voxen/internal/security"
)

// WebRTCManager owns sessions and the shared replay guard.
//
// The replay guard is manager-wide on purpose: a statement captured in one
// session must be rejected if it is replayed in another, so the cache cannot be
// per-session.
type WebRTCManager struct {
	mu       sync.Mutex
	cfg      ManagerConfig
	log      *slog.Logger
	replay   *security.ReplayGuard
	resolve  func(string) (ed25519.PublicKey, bool)
	sessions map[*WebRTCSession]struct{}
	closed   bool
}

// ManagerConfig is the daemon-wide media configuration.
type ManagerConfig struct {
	Identity LocalSigner
	// Parameters is the Opus profile from configuration.
	Parameters OpusParameters
	// STUNServers come from local configuration only. A signaling server cannot
	// inject one.
	STUNServers []string
	// StatementTTL bounds signed statement validity.
	StatementTTL time.Duration
	Logger       *slog.Logger
}

// NewWebRTCManager builds a manager from validated configuration.
func NewWebRTCManager(cfg ManagerConfig) (*WebRTCManager, error) {
	if cfg.Identity == nil {
		return nil, errors.New("media: manager requires an identity signer")
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
	return &WebRTCManager{
		cfg:      cfg,
		log:      logger,
		replay:   security.NewReplayGuard(security.DefaultReplayCapacity),
		sessions: make(map[*WebRTCSession]struct{}),
	}, nil
}

// NewSession creates an unnegotiated session bound to a peer's verified
// fingerprint.
//
// The fingerprint must name a key the local user verified out of band: the
// expected key derived from it is what the peer's statement must be signed by.
// A caller cannot pass an arbitrary key here, which is why this takes the
// fingerprint string rather than raw key bytes.
func (m *WebRTCManager) NewSession(ctx context.Context, expectedFingerprint string) (Session, error) {
	pub, err := m.keyForFingerprint(expectedFingerprint)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("media: manager is closed")
	}
	m.mu.Unlock()

	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}
	sess, err := NewWebRTCSession(ctx, SessionConfig{
		RoomID:          "",
		SessionID:       sessionID,
		LocalIdentity:   m.cfg.Identity,
		ExpectedPeerKey: pub,
		Parameters:      m.cfg.Parameters,
		STUNServers:     m.cfg.STUNServers,
		StatementTTL:    m.cfg.StatementTTL,
		ReplayGuard:     m.replay,
		Logger:          m.log,
	})
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		sess.Close()
		return nil, errors.New("media: manager is closed")
	}
	m.sessions[sess] = struct{}{}
	m.mu.Unlock()
	return sess, nil
}

// keyForFingerprint is replaced by the daemon with a trust-store lookup. The
// manager keeps a resolver so the media plane never reads the trust store
// itself, preserving the package boundary.
var errNoResolver = errors.New("media: no peer key resolver configured")

// SetPeerKeyResolver installs the identity-plane lookup used to turn a verified
// fingerprint into a public key.
func (m *WebRTCManager) SetPeerKeyResolver(fn func(fingerprint string) (ed25519.PublicKey, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolve = fn
}

func (m *WebRTCManager) keyForFingerprint(fingerprint string) (ed25519.PublicKey, error) {
	m.mu.Lock()
	resolve := m.resolve
	m.mu.Unlock()
	if resolve == nil {
		return nil, errNoResolver
	}
	pub, ok := resolve(fingerprint)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoVerifiedPeer, shorten(fingerprint))
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: key for %s has the wrong length", ErrNoVerifiedPeer, shorten(fingerprint))
	}
	return pub, nil
}

// Sessions returns a snapshot of live sessions.
func (m *WebRTCManager) Sessions() []*WebRTCSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*WebRTCSession, 0, len(m.sessions))
	for s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// Close tears every live session down.
func (m *WebRTCManager) Close() error {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*WebRTCSession, 0, len(m.sessions))
	for s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[*WebRTCSession]struct{})
	m.mu.Unlock()

	var errs []error
	for _, s := range sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// newSessionID produces a random identifier that scopes signed statements.
func newSessionID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("media: generate session id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func shorten(fp string) string {
	if len(fp) <= 12 {
		return fp
	}
	return fp[:6] + "…" + fp[len(fp)-4:]
}
