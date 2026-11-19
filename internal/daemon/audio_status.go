package daemon

import (
	"context"

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
	}, controller.Sessions)
	s.bridge = bridge
	s.controllerMu.Unlock()

	if err := bridge.Start(context.Background()); err != nil {
		s.log.Warn("audio did not start; the call will be silent", "error", err)
		s.controllerMu.Lock()
		s.bridge = nil
		s.controllerMu.Unlock()
		bridge.Stop()
		return
	}
	s.log.Info("call audio is live")
}

// stopAudio closes the devices when leaving a room.
func (s *Server) stopAudio() {
	s.controllerMu.Lock()
	bridge := s.bridge
	s.bridge = nil
	s.controllerMu.Unlock()
	if bridge != nil {
		bridge.Stop()
		s.log.Info("call audio stopped")
	}
}
