// Package media defines and implements the media plane.
//
// The media plane owns WebRTC peer connections, Opus encoding/decoding, RTP
// statistics and jitter buffering. It knows nothing about signaling or CLI
// plumbing; the room layer wires peers to sessions.
//
// Security invariant: a Session will not transmit or accept audio until it has
// verified the peer's signed session statement and bound it to the DTLS
// certificate actually negotiated. See webrtc_session.go.
package media

import (
	"context"
	"errors"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/security"
)

// Errors from the media plane.
var (
	ErrNotConnected    = errors.New("media: peer connection is not established")
	ErrNoDirectRoute   = errors.New("media: no direct peer-to-peer path; voxen does not relay media through TURN")
	ErrUnauthenticated = errors.New("media: peer identity has not been authenticated")
	ErrClosed          = errors.New("media: session closed")
)

// OpusParameters is the negotiated codec configuration. These values are fixed
// by voxen rather than negotiated per call, because every implementation speaks
// exactly one profile: 48 kHz mono, 20 ms frames, VoIP mode, FEC and DTX on.
type OpusParameters struct {
	PayloadType uint8
	ClockRate   uint32
	Channels    uint8
	FrameMS     int
	Bitrate     int
	Application string
	UseFEC      bool
	UseDTX      bool
}

// DefaultOpusParameters returns voxen's canonical codec profile. The payload type
// 111 is the conventional dynamic value used by Pion's Opus registration.
func DefaultOpusParameters(bitrate int, frameMS int) OpusParameters {
	return OpusParameters{
		PayloadType: 111,
		ClockRate:   uint32(audio.DefaultFormat.SampleRate),
		Channels:    uint8(audio.DefaultFormat.Channels),
		FrameMS:     frameMS,
		Bitrate:     bitrate,
		Application: "voip",
		UseFEC:      true,
		UseDTX:      true,
	}
}

// SessionStats is the live quality snapshot shown by "voxen status" and used for
// the measurements required by the test plan.
type SessionStats struct {
	RTT             time.Duration `json:"rtt"`
	PacketsSent     uint64        `json:"packets_sent"`
	PacketsReceived uint64        `json:"packets_received"`
	PacketsLost     uint64        `json:"packets_lost"`
	PacketLoss      float64       `json:"packet_loss"`
	Jitter          time.Duration `json:"jitter"`
	BytesSent       uint64        `json:"bytes_sent"`
	BytesReceived   uint64        `json:"bytes_received"`
	// LocalCandidate and RemoteCandidate are the selected ICE pair, used to
	// prove that a call is actually direct rather than relayed.
	LocalCandidate  string `json:"local_candidate"`
	RemoteCandidate string `json:"remote_candidate"`
	// Relayed is always false: voxen refuses TURN and never configures a relay.
	Relayed bool `json:"relayed"`
}

// ICECandidatePayload is a trickled candidate in transport-neutral form.
//
// sdpMid and sdpMLineIndex are not optional decoration: Pion (and every WebRTC
// implementation) needs at least one of them to know which media section the
// candidate belongs to. Relaying only the candidate string silently drops
// candidates, which is exactly the bug this type prevents.
type ICECandidatePayload struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdp_mid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdp_mline_index,omitempty"`
}

// ToInit converts the payload into a Pion candidate initialiser.
func (c ICECandidatePayload) ToInit() (webrtc.ICECandidateInit, error) {
	if c.Candidate == "" {
		return webrtc.ICECandidateInit{}, errors.New("media: empty ICE candidate")
	}
	if c.SDPMid == nil && c.SDPMLineIndex == nil {
		return webrtc.ICECandidateInit{}, errors.New("media: ICE candidate has neither sdp_mid nor sdp_mline_index")
	}
	return webrtc.ICECandidateInit{
		Candidate:     c.Candidate,
		SDPMid:        c.SDPMid,
		SDPMLineIndex: c.SDPMLineIndex,
	}, nil
}

// NewICECandidatePayload converts a Pion candidate into the wire form.
func NewICECandidatePayload(c webrtc.ICECandidate) (ICECandidatePayload, bool) {
	init := c.ToJSON()
	if init.Candidate == "" {
		return ICECandidatePayload{}, false
	}
	return ICECandidatePayload{
		Candidate:     init.Candidate,
		SDPMid:        init.SDPMid,
		SDPMLineIndex: init.SDPMLineIndex,
	}, true
}

// PeerInfo describes the authenticated remote party.
type PeerInfo struct {
	Fingerprint string
	DisplayName string
	// DTLSFingerprint is the SHA-256 of the certificate negotiated in the DTLS
	// handshake. It has been checked against the peer's signed statement.
	DTLSFingerprint [32]byte
	// RemoteCertVerified is true only after BindDTLS succeeded.
	RemoteCertVerified bool
}

// Negotiation is one side of an SDP exchange, together with the cryptographic
// proof that binds the enclosed fingerprint to a verified identity.
//
// The statement is what makes the SDP trustworthy: the fingerprint inside the
// SDP is relayed by the signaling server, which may rewrite it, but the
// statement is signed by the peer's Ed25519 key over the fingerprint the peer
// actually uses.
type Negotiation struct {
	// Type is "offer" or "answer".
	Type string
	// SDP is the session description.
	SDP []byte
	// SessionID scopes the statement to this negotiation.
	SessionID string
	// Statement is the peer's signed claim about its DTLS fingerprint.
	Statement *security.SessionStatement
	// Signature is the Ed25519 signature over Statement.SignedBytes().
	Signature []byte
}

// Session is one authenticated peer-to-peer media session.
//
// Implementations must not begin sending audio until Authenticate has returned
// nil. Authenticate verifies the signed session statement against the DTLS
// certificate that was actually negotiated.
type Session interface {
	// CreateOffer starts negotiation as the initiating side.
	CreateOffer(ctx context.Context) (*Negotiation, error)
	// AcceptAnswer completes negotiation on the initiating side. It verifies the
	// answer's statement and, once DTLS completes, the negotiated certificate.
	AcceptAnswer(ctx context.Context, answer *Negotiation) (PeerInfo, error)
	// AcceptOffer answers an incoming offer, verifying its statement first.
	AcceptOffer(ctx context.Context, offer *Negotiation) (*Negotiation, PeerInfo, error)
	// AddICECandidate applies a trickled candidate.
	AddICECandidate(ctx context.Context, candidate ICECandidatePayload) error
	// SetCandidateHandler installs a callback for locally gathered candidates.
	SetCandidateHandler(func(candidate ICECandidatePayload))
	// Authenticate waits for DTLS and binds the peer identity to it.
	Authenticate(ctx context.Context) (PeerInfo, error)
	// SendPCM encodes and transmits one frame of 48 kHz mono PCM.
	SendPCM(ctx context.Context, pcm []int16) error
	// ReceivePCM reads decoded audio into pcm, blocking until a frame is ready.
	ReceivePCM(ctx context.Context, pcm []int16) (int, error)
	// RestartICE performs an ICE restart after a network change.
	RestartICE(ctx context.Context) (*Negotiation, error)
	// Stats returns the current quality snapshot.
	Stats() SessionStats
	// Close tears the session down.
	Close() error
}

// Manager owns all media sessions for the daemon.
type Manager interface {
	// NewSession creates an unnegotiated session bound to an expected peer
	// identity (the fingerprint verified in the trust store).
	NewSession(ctx context.Context, expectedFingerprint string) (Session, error)
	Close() error
}
