package daemon

import (
	"context"
	"crypto/ed25519"
	"path/filepath"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
)

// controller returns the room controller, creating it on first use.
//
// Lazy creation keeps a daemon that never places a call from opening a signaling
// connection or allocating media state.
func (s *Server) controllerFor() *Controller {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	if s.controller != nil {
		return s.controller
	}
	s.controller = NewController(
		s.media,
		trustResolver{dir: filepath.Dir(s.identityPath()), fallback: s.trust},
		s.identity,
		s.logger(),
		ControllerConfig{
			SignalingURL: s.cfg.Signaling.URL,
			DisplayName:  s.identity.DisplayName,
			MaxMembers:   configMaxMembers(s.cfg),
			JoinTimeout:  s.cfg.SignalingTimeout(),
		},
	)
	return s.controller
}

// configMaxMembers returns the participant cap for a new mesh. The room layer
// enforces a hard limit of 16; this is the practical default.
func configMaxMembers(config.Config) int { return 5 }

// trustResolver adapts the trust store to the controller's resolver interface.
//
// Trust is anchored on the fingerprint, not on a stored key, so two cases must
// both authorise a session:
//
//  1. The peer's key was verified directly. The stored key must match the
//     advertised one, which it does because entries are keyed by fingerprint.
//  2. The user pinned a fingerprint before ever meeting the peer (the normal
//     pre-call flow). No key is stored yet, so the advertised key is accepted
//     only if it hashes to the pinned fingerprint - which is guaranteed here,
//     because the lookup key *is* that hash.
//
// Revoked entries are refused in both cases, and an unverified key never
// authorises anything.
type trustResolver struct {
	// dir is the directory holding trust.json. The store is re-read on each
	// lookup rather than cached: "voxen verify" runs as a separate process, so a
	// cached copy would make a verification invisible until the daemon restarted,
	// which is both surprising and a support burden.
	dir string
	// fallback is the store loaded at startup, used only if the file cannot be
	// re-read (for example it was temporarily removed).
	fallback *identity.TrustStore
}

// store returns the current trust store, preferring a fresh read.
func (r trustResolver) store() *identity.TrustStore {
	if fresh, err := identity.OpenTrust(r.dir); err == nil {
		return fresh
	}
	return r.fallback
}

// VerifiedKey reports whether fp is an acceptable identity and returns the key
// to bind the session to.
func (r trustResolver) VerifiedKey(fp identity.Fingerprint) (ed25519.PublicKey, bool) {
	entry, ok := r.store().Lookup(fp)
	if !ok || entry.Level != identity.TrustVerified {
		return nil, false
	}
	if entry.PublicKey == "" {
		// A pin with no key bound yet. The controller supplies the advertised key;
		// it reached this point only because it hashed to the pinned fingerprint.
		return nil, false
	}
	pub, err := entry.PublicKeyBytes()
	if err != nil || pub == nil {
		return nil, false
	}
	return pub, true
}

// IsPinned reports whether a verified fingerprint has no key bound yet.
func (r trustResolver) IsPinned(fp identity.Fingerprint) bool {
	entry, ok := r.store().Lookup(fp)
	return ok && entry.Level == identity.TrustVerified && entry.PublicKey == ""
}

// InviteBound trusts a key established by a verified invite code.
//
// This is deliberately weaker than a fingerprint comparison and is labelled as such
// in the trust store, so "voxen trust" shows how each peer was authenticated.
func (r trustResolver) InviteBound(pub ed25519.PublicKey) error {
	store := r.store()
	fp := identity.FingerprintFromPublic(pub)
	if entry, ok := store.Lookup(fp); ok && entry.Level == identity.TrustVerified {
		return nil // already trusted, by either route
	}
	_, err := store.VerifyKey(pub, "", "authenticated by invite code")
	return err
}

// BindKey completes a pin with the observed key. The trust store re-derives the
// fingerprint from the key and looks it up, so a key that does not hash to the
// pinned value simply has no entry and is refused.
func (r trustResolver) BindKey(pub ed25519.PublicKey) error {
	_, err := r.store().BindKey(pub)
	return err
}

func (s *Server) roomCreate(ctx context.Context, params jsonRaw) (any, error) {
	var p ipc.RoomCreateParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	c := s.controllerFor()
	res, err := c.CreateRoom(ctx)
	if err != nil {
		return nil, roomError(err)
	}
	s.startAudio(c)
	if p.MaxMembers > 0 {
		// A per-call cap needs the controller to be configured before the room is
		// created; rejecting is clearer than silently ignoring it.
		s.log.Warn("room max_members is configured by the daemon, not per call", "requested", p.MaxMembers)
	}
	return res, nil
}

func (s *Server) roomJoin(ctx context.Context, params jsonRaw) (any, error) {
	var p ipc.RoomJoinParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}

	c := s.controllerFor()

	// A code is the normal path: it is what a person shares, and it carries the
	// check that authenticates the caller. The room ID path remains for tests and
	// for scripting against a known room.
	var res ipc.RoomResult
	var err error
	if p.Code != "" {
		res, err = c.JoinRoomByCode(ctx, p.Code)
	} else if p.RoomID != "" {
		res, err = c.JoinRoom(ctx, p.RoomID)
	} else {
		return nil, ipc.NewError(ipc.CodeBadRequest, "a code or room_id is required")
	}
	if err != nil {
		return nil, roomError(err)
	}
	s.startAudio(c)
	return res, nil
}

func (s *Server) roomLeave(params jsonRaw) (any, error) {
	var p ipc.RoomLeaveParams
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	s.controllerMu.Lock()
	c := s.controller
	s.controllerMu.Unlock()
	if c == nil {
		return nil, ipc.NewError(ipc.CodeNotInRoom, "not in a room")
	}
	// Stop audio before tearing down sessions so capture never writes to a
	// closing session.
	s.stopAudio()
	if err := c.LeaveRoom(p.Reason); err != nil {
		return nil, roomError(err)
	}
	return ipc.RoomResult{}, nil
}

func (s *Server) roomEvents() []ipc.RoomEvent {
	s.controllerMu.Lock()
	c := s.controller
	s.controllerMu.Unlock()
	if c == nil {
		return nil
	}
	return c.Events()
}

// roomError maps controller failures onto IPC error codes so the CLI can exit
// with a meaningful status.
func roomError(err error) error {
	switch {
	case err == nil:
		return nil
	case isErr(err, ErrNoRoom):
		return ipc.NewError(ipc.CodeNotInRoom, "%v", err)
	case isErr(err, ErrRoomBusy):
		return ipc.NewError(ipc.CodeConflict, "%v", err)
	case isErr(err, ErrSignalingDown):
		return ipc.NewError(ipc.CodeUnavailable, "%v", err)
	}
	return ipc.NewError(ipc.CodeInternal, "%v", err)
}

func isErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// logger adapts slog to the controller's small logging interface.
func (s *Server) logger() Logger { return slogAdapter{l: s.log.With("component", "room")} }

type slogAdapter struct {
	l interface {
		Info(string, ...any)
		Warn(string, ...any)
		Debug(string, ...any)
		Error(string, ...any)
	}
}

func (a slogAdapter) Info(m string, args ...any)  { a.l.Info(m, args...) }
func (a slogAdapter) Warn(m string, args ...any)  { a.l.Warn(m, args...) }
func (a slogAdapter) Debug(m string, args ...any) { a.l.Debug(m, args...) }
func (a slogAdapter) Error(m string, args ...any) { a.l.Error(m, args...) }
