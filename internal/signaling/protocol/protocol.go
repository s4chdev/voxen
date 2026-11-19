// Package protocol defines the control-plane wire protocol shared by the voxen
// client and the self-hosted signaling server.
//
// The types here are the normative Go representation of docs/protocol.md. Both
// processes import this package, so the wire format cannot drift between them.
// The server relays SDP and ICE verbatim and never sees audio.
package protocol

import (
	"encoding/json"
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

// Error codes returned in an error envelope.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeRoomNotFound    = "room_not_found"
	CodeRoomFull        = "room_full"
	CodeNotMember       = "not_member"
	CodeProtocol        = "protocol_violation"
	CodeVersionMismatch = "version_mismatch"
	CodeInternal        = "internal"
)

// Envelope is the single frame type.
type Envelope struct {
	Version int             `json:"v"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Room    string          `json:"room,omitempty"`
	From    string          `json:"from,omitempty"`
	To      string          `json:"to,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	SentAt  time.Time       `json:"sent_at,omitempty"`
}

// Hello is the first client message.
//
// Proof is a signature over the domain-separated handshake transcript. It proves
// key possession to the server but grants no trust to peers: peer trust comes
// only from the signed session statements exchanged later.
type Hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	DisplayName     string `json:"display_name"`
	PublicKey       string `json:"public_key"`
	Proof           string `json:"proof,omitempty"`
	RoomID          string `json:"room_id,omitempty"`
}

// Welcome is the server's hello response.
type Welcome struct {
	ProtocolVersion int    `json:"protocol_version"`
	PeerID          string `json:"peer_id"`
	ServerName      string `json:"server_name"`
	Challenge       string `json:"challenge,omitempty"`
}

// RoomStatePayload is the roster as known by the server.
type RoomStatePayload struct {
	RoomID    string     `json:"room_id"`
	CreatedAt time.Time  `json:"created_at,omitempty"`
	Members   []PeerInfo `json:"members"`
}

// PeerInfo is one participant as advertised by the server.
type PeerInfo struct {
	PeerID      string    `json:"peer_id"`
	DisplayName string    `json:"display_name"`
	PublicKey   string    `json:"public_key"`
	JoinedAt    time.Time `json:"joined_at"`
}

// ErrorPayload is a server-side error response.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
