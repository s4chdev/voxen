package daemon

import (
	"context"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/ipc"
)

// audioState describes the backend for "voxen status".
func (s *Server) audioState() string {
	if s.audio == nil {
		return "unavailable"
	}
	if _, ok := s.audio.(audio.Null); ok {
		return "unavailable"
	}
	return s.audio.Version()
}

// audioStats returns live counters for the current call.
func (s *Server) audioStats() ipc.AudioStatus {
	s.controllerMu.Lock()
	bridge := s.bridge
	s.controllerMu.Unlock()
	if bridge == nil {
		return ipc.AudioStatus{Muted: s.muted.Load()}
	}
	st := bridge.Stats()
	return ipc.AudioStatus{
		Running:        st.Running,
		Muted:          st.Muted,
		FramesSent:     st.FramesSent,
		FramesReceived: st.FramesReceived,
		InputDropped:   st.InputDropped,
		OutputXRuns:    st.OutputXRuns,
		InputDevice:    st.InputDevice,
		OutputDevice:   st.OutputDevice,
	}
}

// startAudio opens the devices for a call. A failure is reported but does not
// abort the room join: the mesh and the security handshake are still useful, and
// "voxen status" will show audio as not running with the reason.
func (s *Server) startAudio(controller *Controller) {
	if s.audio == nil {
		return
	}
	if _, ok := s.audio.(audio.Null); ok {
		return
	}
	s.controllerMu.Lock()
	if s.bridge != nil {
		s.controllerMu.Unlock()
		return
	}
	bridge := NewAudioBridge(s.audio, s.log.With("component", "audio"), AudioBridgeConfig{
		InputDevice:  s.cfg.Audio.InputDevice,
		OutputDevice: s.cfg.Audio.OutputDevice,
		Muted:        s.muted.Load(),
	}, controller.ActivePeers)

	// Speech transitions update the controller, which is where "voxen peers"
	// reads them from.
	bridge.SetObserver(func(peerID string, speaking bool) {
		controller.setSpeaking(peerID, speaking)
	})
	// Clear per-peer bridge state when someone leaves, so a peer who rejoins does
	// not inherit a stale speaking indicator.
	controller.SetPeerGoneHook(func(peerID string) {
		s.controllerMu.Lock()
		b := s.bridge
		s.controllerMu.Unlock()
		if b != nil {
			b.forgetPeer(peerID)
		}
	})
	// Carry over any per-peer mute the user set before the call started.
	for _, p := range controller.ActivePeers() {
		if muted := s.peerMutedLocally(p.PeerID); muted {
			bridge.SetPeerMuted(p.PeerID, true)
		}
	}
	s.controllerMu.Unlock()

	// Opening audio devices is a blocking OS call that can take seconds, or hang
	// outright when a platform has no audio service. It must not run on the IPC
	// handler that is answering "voxen join", and it must not be able to delay
	// daemon shutdown, so it happens in its own goroutine with a deadline.
	//
	// The call is fully usable meanwhile: the mesh and the authenticated media
	// sessions are already up, and "voxen status" reports audio as not running
	// until this finishes.
	go s.openAudioAsync(bridge, controller)
}

// stopAudio closes the devices when leaving a room.
func (s *Server) stopAudio() {
	s.controllerMu.Lock()
	bridge := s.bridge
	s.bridge = nil
	s.controllerMu.Unlock()
	if bridge != nil {
		s.stopAudioAsync(bridge)
		s.log.Info("call audio stopped")
	}
}

// peerMutedLocally reports whether the user has muted a participant.
//
// The set lives on the server rather than the bridge because the bridge is
// recreated per call, and a mute should survive a rejoin within the same daemon
// session.
func (s *Server) peerMutedLocally(peerID string) bool {
	s.mutedPeersMu.Lock()
	defer s.mutedPeersMu.Unlock()
	return s.mutedPeers[peerID]
}

// setPeerMuted records a mute decision and applies it to the live bridge.
func (s *Server) setPeerMuted(peerID string, muted bool) {
	s.mutedPeersMu.Lock()
	if s.mutedPeers == nil {
		s.mutedPeers = make(map[string]bool)
	}
	if muted {
		s.mutedPeers[peerID] = true
	} else {
		delete(s.mutedPeers, peerID)
	}
	s.mutedPeersMu.Unlock()

	s.controllerMu.Lock()
	bridge := s.bridge
	s.controllerMu.Unlock()
	if bridge != nil {
		bridge.SetPeerMuted(peerID, muted)
	}
}

// setPeerMute handles the IPC call that mutes one participant locally.
func (s *Server) setPeerMute(params jsonRaw) (any, error) {
	var p ipc.PeerMuteParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.PeerID == "" {
		return nil, ipc.NewError(ipc.CodeBadRequest, "peer_id is required")
	}

	s.controllerMu.Lock()
	c := s.controller
	s.controllerMu.Unlock()
	if c == nil {
		return nil, ipc.NewError(ipc.CodeNotInRoom, "not in a room")
	}

	// A participant the controller has never heard of is a caller mistake, not a
	// silent no-op: a TUI keyed by peer ID should not be able to mute a ghost.
	next := true
	if p.Muted != nil {
		next = *p.Muted
	}
	if err := c.SetPeerMuted(p.PeerID, next); err != nil {
		return nil, ipc.NewError(ipc.CodeNotFound, "%v", err)
	}
	s.setPeerMuted(p.PeerID, next)
	s.log.Info("participant mute changed", "peer", p.PeerID, "muted", next)
	return ipc.PeerMuteResult{PeerID: p.PeerID, Muted: next}, nil
}

// openAudioAsync opens the audio devices for a call without blocking the caller.
func (s *Server) openAudioAsync(bridge *AudioBridge, controller *Controller) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := bridge.Start(ctx); err != nil {
		s.log.Warn("audio did not start; the call will be silent", "error", err)
		bridge.Stop()
		return
	}

	// Publish only on success, and only if this bridge is still the current one: the
	// room may have been left while the devices were opening.
	s.controllerMu.Lock()
	current := s.controller
	already := s.bridge != nil
	if current == controller && !already {
		s.bridge = bridge
		s.controllerMu.Unlock()
		s.log.Info("call audio is live")
		return
	}
	s.controllerMu.Unlock()
	bridge.Stop()
}

// audioStopTimeout bounds how long shutdown waits for audio teardown.
//
// A wedged driver must not prevent the process from exiting; the OS reclaims the
// device when the process dies.
const audioStopTimeout = 3 * time.Second

// stopAudioAsync closes the devices without blocking the caller.
func (s *Server) stopAudioAsync(bridge *AudioBridge) {
	done := make(chan struct{})
	go func() {
		bridge.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(audioStopTimeout):
		s.log.Warn("audio teardown timed out; continuing shutdown")
	}
}
