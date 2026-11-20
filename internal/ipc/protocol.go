// Package ipc defines the local control protocol between the voxen CLI and the
// voxend daemon.
//
// Transport: a Unix domain socket (Linux, macOS) or a named pipe (Windows). The
// endpoint is private to the current user; on Unix the daemon additionally
// verifies the peer's UID before serving a request.
//
// Framing: one JSON object per line, UTF-8. Requests and responses share the
// same envelope shape and are correlated by ID. The protocol is versioned and a
// daemon rejects unknown major versions instead of guessing.
package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the control protocol version.
const ProtocolVersion = 1

// MaxFrameBytes bounds a single request or response. The control plane carries
// metadata only: audio never crosses this socket.
const MaxFrameBytes = 1 << 20 // 1 MiB

// Method names. They are stable API: the CLI and integrations may rely on them.
const (
	MethodPing           = "ping"
	MethodShutdown       = "shutdown"
	MethodStatus         = "status"
	MethodDoctor         = "doctor"
	MethodConfigGet      = "config.get"
	MethodConfigReload   = "config.reload"
	MethodIdentityShow   = "identity.show"
	MethodIdentityVerify = "identity.verify"
	MethodIdentityRevoke = "identity.revoke"
	MethodTrustList      = "trust.list"
	MethodPeersList      = "peers.list"
	MethodDevicesList    = "devices.list"
	MethodLogsTail       = "logs.tail"
	MethodMuteSet        = "mute.set"
	MethodPeerMuteSet    = "peer.mute"
	MethodRoomCreate     = "room.create"
	MethodRoomJoin       = "room.join"
	MethodRoomLeave      = "room.leave"
	MethodRoomAdmit      = "room.admit"
	MethodRoomRemove     = "room.remove"
	MethodRoomEvents     = "room.events"
	MethodSessionICESet  = "session.ice-restart"
)

// Error codes returned by the daemon. Codes are stable; messages are not.
const (
	CodeBadRequest     = "bad_request"
	CodeUnavailable    = "unavailable"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeUnauthorized   = "unauthorized"
	CodeInternal       = "internal"
	CodeUnsupported    = "unsupported"
	CodeNotInRoom      = "not_in_room"
	CodeIdentityLocked = "identity_locked"
)

// Error is the structured failure returned to the CLI.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details is optional operator-facing context. It must never contain key
	// material, SDP bodies or audio.
	Details string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Details != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Details)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError builds a structured error.
func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Request is one CLI-to-daemon call.
type Request struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one daemon-to-CLI reply.
type Response struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Validate checks the envelope invariants a daemon enforces before dispatch.
func (r *Request) Validate() error {
	if r.Version != ProtocolVersion {
		return NewError(CodeUnsupported, "unsupported IPC protocol version %d (daemon speaks %d)", r.Version, ProtocolVersion)
	}
	if r.ID == "" {
		return NewError(CodeBadRequest, "missing request id")
	}
	if len(r.ID) > 128 {
		return NewError(CodeBadRequest, "request id too long")
	}
	if r.Method == "" {
		return NewError(CodeBadRequest, "missing method")
	}
	if len(r.Method) > 64 {
		return NewError(CodeBadRequest, "method name too long")
	}
	if len(r.Params) > MaxFrameBytes/2 {
		return NewError(CodeBadRequest, "request payload too large")
	}
	return nil
}

// Validate checks the response envelope.
func (r *Response) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("ipc: response protocol version %d != %d", r.Version, ProtocolVersion)
	}
	if !r.OK && r.Error == nil {
		return errors.New("ipc: failed response has no error")
	}
	if r.OK && r.Error != nil {
		return errors.New("ipc: successful response carries an error")
	}
	return nil
}

// -----------------------------------------------------------------------------
// Method payloads
// -----------------------------------------------------------------------------

// PingResult proves the daemon is alive and reports the process identity.
type PingResult struct {
	PID       int    `json:"pid"`
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
}

// RoomState is the daemon's view of the current room.
type RoomState struct {
	InRoom    bool   `json:"in_room"`
	RoomID    string `json:"room_id,omitempty"`
	Role      string `json:"role,omitempty"`
	JoinState string `json:"join_state,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// PeerState is one participant as seen by the daemon.
type PeerState struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Fingerprint string  `json:"fingerprint"`
	Verified    bool    `json:"verified"`
	Connection  string  `json:"connection"` // connected, connecting, disconnected, failed
	Speaking    bool    `json:"speaking"`
	Muted       bool    `json:"muted"`
	RTTMillis   int     `json:"rtt_ms,omitempty"`
	PacketLoss  float64 `json:"packet_loss,omitempty"`
	// Direct is true when the selected ICE path is not relayed. voxen never
	// relays, so a false value means no path was established yet.
	Direct bool `json:"direct"`
	// LastError explains a failed connection.
	LastError string `json:"last_error,omitempty"`
}

// RoomEvent is one lifecycle event in the local ring buffer.
type RoomEvent struct {
	At          string `json:"at"`
	Kind        string `json:"kind"`
	PeerID      string `json:"peer_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Message     string `json:"message,omitempty"`
}

// StatusResult is the aggregate daemon status.
type StatusResult struct {
	PID            int         `json:"pid"`
	Version        string      `json:"version"`
	Phase          int         `json:"phase"`
	Platform       string      `json:"platform"`
	StartedAt      string      `json:"started_at"`
	ConfigPath     string      `json:"config_path"`
	SocketPath     string      `json:"socket"`
	Fingerprint    string      `json:"fingerprint"`
	DisplayName    string      `json:"display_name,omitempty"`
	Signaling      string      `json:"signaling"`
	SignalingState string      `json:"signaling_state"`
	Muted          bool        `json:"muted"`
	Room           RoomState   `json:"room"`
	Peers          int         `json:"peers"`
	AudioState     string      `json:"audio_state"`
	Audio          AudioStatus `json:"audio"`
	// Capabilities reports which subsystems this build actually implements, so
	// clients never have to guess whether a feature is usable.
	Capabilities Capabilities `json:"capabilities"`
}

// AudioStatus reports live audio counters.
type AudioStatus struct {
	Running        bool   `json:"running"`
	Muted          bool   `json:"muted"`
	FramesSent     uint64 `json:"frames_sent"`
	FramesReceived uint64 `json:"frames_received"`
	InputDropped   uint64 `json:"input_dropped"`
	OutputXRuns    uint64 `json:"output_xruns"`
	InputDevice    string `json:"input_device,omitempty"`
	OutputDevice   string `json:"output_device,omitempty"`
	// EchoCancellation and NoiseSuppression report what is actually running, not
	// what was requested: a build without the backend must not imply otherwise.
	EchoCancellation bool `json:"echo_cancellation"`
	NoiseSuppression bool `json:"noise_suppression"`
	// DSPBackend names the preprocessing implementation, or "none".
	DSPBackend string `json:"dsp_backend,omitempty"`
}

// Capabilities is the honest feature matrix of the running build.
type Capabilities struct {
	Signaling bool `json:"signaling"`
	Media     bool `json:"media"`
	Audio     bool `json:"audio"`
	Group     bool `json:"group_calls"`
}

// MuteParams sets the local mute state.
type MuteParams struct {
	Muted *bool `json:"muted,omitempty"`
}

// PeerMuteParams mutes or unmutes one participant locally.
type PeerMuteParams struct {
	PeerID string `json:"peer_id"`
	Muted  *bool  `json:"muted,omitempty"`
}

// PeerMuteResult reports the resulting per-peer state.
type PeerMuteResult struct {
	PeerID string `json:"peer_id"`
	Muted  bool   `json:"muted"`
}

// MuteResult reports the resulting state and whether a call was active.
type MuteResult struct {
	Muted  bool `json:"muted"`
	Active bool `json:"active"`
}

// ConfigGetResult carries the effective configuration as YAML-safe JSON.
type ConfigGetResult struct {
	Path   string `json:"path"`
	YAML   string `json:"yaml"`
	Source string `json:"source"`
}

// IdentityResult describes the local identity without exposing the key.
type IdentityResult struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	Encrypted   bool   `json:"encrypted"`
}

// VerifyPeerParams records an explicit out-of-band verification decision.
type VerifyPeerParams struct {
	Fingerprint string `json:"fingerprint"`
	DisplayName string `json:"display_name,omitempty"`
	Note        string `json:"note,omitempty"`
}

// RevokePeerParams removes trust from a peer key.
type RevokePeerParams struct {
	Fingerprint string `json:"fingerprint"`
}

// TrustListResult lists peer trust records.
type TrustListResult struct {
	Entries []TrustRecord `json:"entries"`
}

// TrustRecord is one trust store entry.
type TrustRecord struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Level       string `json:"level"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
	VerifiedAt  string `json:"verified_at,omitempty"`
	RevokedAt   string `json:"revoked_at,omitempty"`
	Note        string `json:"note,omitempty"`
	// Pinned is true when the user verified a fingerprint alone and no key has
	// been observed yet. The next session's key must hash to it.
	Pinned bool `json:"pinned,omitempty"`
}

// PeersResult lists participants.
type PeersResult struct {
	Peers []PeerState `json:"peers"`
}

// DeviceInfo is one enumerated audio device.
type DeviceInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsInput    bool   `json:"is_input"`
	IsOutput   bool   `json:"is_output"`
	IsDefault  bool   `json:"is_default"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

// DevicesResult is the enumeration reply.
type DevicesResult struct {
	Inputs  []DeviceInfo `json:"inputs"`
	Outputs []DeviceInfo `json:"outputs"`
	// Error explains why enumeration failed; the command still exits 0 with an
	// explicit "unavailable" state so scripts can distinguish "no devices" from
	// "subsystem missing".
	Error string `json:"error,omitempty"`
}

// LogsParams requests recent log lines.
type LogsParams struct {
	Lines int `json:"lines,omitempty"`
}

// LogsResult returns log lines, newest last.
type LogsResult struct {
	Lines []string `json:"lines"`
	Path  string   `json:"path"`
	// Truncated reports that older lines were dropped.
	Truncated bool `json:"truncated"`
}

// DoctorCheck is one diagnostic result.
type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // pass, warn, fail, unavailable
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// DoctorResult aggregates diagnostics.
type DoctorResult struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// RoomCreateParams requests a new room.
type RoomCreateParams struct {
	Name       string `json:"name,omitempty"`
	Private    bool   `json:"private,omitempty"`
	MaxMembers int    `json:"max_members,omitempty"`
}

// RoomJoinParams joins an existing room, by code or by room ID.
type RoomJoinParams struct {
	// Code is the short code a person shares, for example "D8CY-2XWT-FME0".
	Code string `json:"code,omitempty"`
	// RoomID is the server's identifier, used internally and by tests.
	RoomID string `json:"room_id,omitempty"`
}

// RoomResult returns the resulting room state.
type RoomResult struct {
	Room RoomState `json:"room"`
	// ShareCode is the short code to give to other people, populated on create.
	// This is the only thing a user needs to share; there is no separate room ID to
	// read out and no fingerprint exchange.
	ShareCode string `json:"share_code,omitempty"`
}

// RoomEventsResult returns the recent lifecycle events.
type RoomEventsResult struct {
	Events []RoomEvent `json:"events"`
}

// RoomLeaveParams leaves the current room.
type RoomLeaveParams struct {
	Reason string `json:"reason,omitempty"`
}

// AdmitParams admits or removes a participant by fingerprint.
type AdmitParams struct {
	Fingerprint string `json:"fingerprint"`
}

// ShutdownParams requests daemon termination.
type ShutdownParams struct {
	// Graceful waits for active calls to close (bounded). Phase 1 has no calls.
	Graceful bool `json:"graceful,omitempty"`
}

// StaticError is used when the CLI needs a typed error without a daemon.
var ErrDaemonUnavailable = NewError(CodeUnavailable, "voxend is not running")
