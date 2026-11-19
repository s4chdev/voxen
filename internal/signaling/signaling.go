// Package signaling defines the control-plane contract and the JSON wire
// protocol spoken between voxen clients and a voxen signaling server.
//
// The signaling server is deliberately dumb: it manages rooms, membership
// envelopes and opaque SDP/ICE payloads. It never sees audio, never sees private
// keys, and is never trusted to authenticate a peer. See docs/protocol.md for
// the normative description and docs/security.md for the trust model.
//
// Phase 1 defines the protocol types and the client interface; the transport is
// implemented in phase 4.
package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ProtocolVersion is the signaling wire version.
const ProtocolVersion = 1

// Message types exchanged over the WebSocket.
const (
	TypeHello        = "hello"
	TypeWelcome      = "welcome"
	TypeRoomCreate   = "room.create"
	TypeRoomJoin     = "room.join"
	TypeRoomLeave    = "room.leave"
	TypeRoomState    = "room.state"
	TypePeerJoin     = "peer.join"
	TypePeerLeave    = "peer.leave"
	TypeOffer        = "sdp.offer"
	TypeAnswer       = "sdp.answer"
	TypeICECandidate = "ice.candidate"
	TypeICEEnd       = "ice.end"
	TypeRoster       = "roster"
	TypeAdmit        = "admit"
	TypeRemove       = "remove"
	TypeError        = "error"
	TypePing         = "ping"
	TypePong         = "pong"
)

// Errors from the signaling layer.
var (
	ErrClosed          = errors.New("signaling: connection closed")
	ErrRoomNotFound    = errors.New("signaling: room not found")
	ErrRoomFull        = errors.New("signaling: room is full")
	ErrNotMember       = errors.New("signaling: not a member of this room")
	ErrUnauthorized    = errors.New("signaling: unauthorized")
	ErrProtocol        = errors.New("signaling: protocol violation")
	ErrVersionMismatch = errors.New("signaling: unsupported protocol version")
)

// Envelope is the single frame type. Payload is kept raw so the server never
// needs to understand SDP or signed statements; it relays them verbatim and
// clients validate them cryptographically.
type Envelope struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	// ID correlates a request with its response for request/response pairs.
	ID string `json:"id,omitempty"`
	// Room is the room the message applies to.
	Room string `json:"room,omitempty"`
	// From is filled in by the server with the sender's peer ID. Clients must
	// treat it as a routing hint, never as authentication.
	From string `json:"from,omitempty"`
	// To addresses a single peer for SDP/ICE. Empty means broadcast to the room.
	To      string          `json:"to,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// SentAt is server-assigned and used for diagnostics only.
	SentAt time.Time `json:"sent_at,omitempty"`
}

// Hello is the first client message. It proves key possession by signing a
// server nonce; it does not grant trust to other peers.
type Hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	PeerID          string `json:"peer_id"`
	DisplayName     string `json:"display_name"`
	PublicKey       string `json:"public_key"` // base64 Ed25519
	// Proof is an Ed25519 signature over the server challenge and the handshake
	// transcript (see docs/protocol.md).
	Proof string `json:"proof"`
	// RoomID asks to join immediately after the welcome.
	RoomID string `json:"room_id,omitempty"`
}

// Welcome is the server's hello response.
type Welcome struct {
	ProtocolVersion int    `json:"protocol_version"`
	PeerID          string `json:"peer_id"`
	Challenge       string `json:"challenge"`
	ServerName      string `json:"server_name"`
	// STUNServers is advisory only. Clients use their local configuration for
	// ICE; a server cannot force a relay.
	STUNServers []string `json:"stun_servers,omitempty"`
}

// RoomStatePayload is the authoritative roster as known by the server.
type RoomStatePayload struct {
	RoomID    string     `json:"room_id"`
	CreatedAt time.Time  `json:"created_at"`
	Members   []PeerInfo `json:"members"`
	// Roster is the administrator-signed membership document. Clients verify it;
	// the server cannot admit a peer on its own.
	Roster          json.RawMessage `json:"roster,omitempty"`
	RosterSignature string          `json:"roster_signature,omitempty"`
	Sequence        uint64          `json:"sequence"`
}

// PeerInfo is one room participant as advertised by the server.
type PeerInfo struct {
	PeerID      string    `json:"peer_id"`
	DisplayName string    `json:"display_name"`
	PublicKey   string    `json:"public_key"`
	JoinedAt    time.Time `json:"joined_at"`
}

// SessionDescription carries SDP with its signed identity binding.
type SessionDescription struct {
	Type string `json:"type"` // "offer" or "answer"
	SDP  string `json:"sdp"`
	// SessionID must match the statement; it scopes the statement to this
	// negotiation.
	SessionID string `json:"session_id"`
	// Statement is the peer's signed SessionStatement (JSON encoding).
	Statement json.RawMessage `json:"statement"`
	// Signature is the Ed25519 signature over Statement.SignedBytes().
	Signature string `json:"signature"`
	// ICEUfrag / ICEPwd are duplicated for observability; SDP is authoritative.
	ICEUfrag string `json:"ice_ufrag,omitempty"`
	ICEPwd   string `json:"ice_pwd,omitempty"`
}

// ICECandidate is a trickled candidate.
type ICECandidate struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdp_mid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdp_mline_index,omitempty"`
	UsernameFragment *string `json:"username_fragment,omitempty"`
}

// ErrorPayload is a server-side error response.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Connection is the client-side signaling transport.
//
// Implementations own reconnection and MUST NOT silently drop messages: a
// dropped SDP/ICE message is surfaced to the room layer as an error so the
// session can be renegotiated or torn down.
type Connection interface {
	// Send transmits one envelope. It returns an error if the transport is
	// closed or the send buffer is full.
	Send(ctx context.Context, env Envelope) error
	// Recv returns the next envelope, blocking until one arrives or ctx ends.
	Recv(ctx context.Context) (Envelope, error)
	// Close closes the transport. Safe to call multiple times.
	Close() error
	// State reports the connection state for "voxen status".
	State() State
}

// State is a coarse signaling connection state.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateHandshaking  State = "handshaking"
	StateConnected    State = "connected"
)
