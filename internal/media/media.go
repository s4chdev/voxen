// Package media defines the media-plane contract.
//
// The media plane owns WebRTC peer connections, Opus encoding/decoding, RTP
// statistics and jitter buffering. It knows nothing about signaling or CLI
// plumbing; the room layer wires peers to sessions.
//
// Phase 1 defines the interfaces and the negotiated codec parameters only. The
// implementation lands in phase 3 (two-peer) and phase 6 (mesh).
package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
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

// PeerInfo describes the authenticated remote party.
type PeerInfo struct {
	Fingerprint string
	DisplayName string
}

// Session is one authenticated peer-to-peer media session.
//
// Implementations must not begin sending audio until Authenticate has returned
// nil. Authenticate verifies the signed session statement against the DTLS
// certificate that was actually negotiated.
type Session interface {
	// CreateOffer starts negotiation as the initiating side.
	CreateOffer(ctx context.Context) ([]byte, error)
	// AcceptAnswer completes negotiation on the initiating side.
	AcceptAnswer(ctx context.Context, sdp []byte) error
	// AcceptOffer answers an incoming offer.
	AcceptOffer(ctx context.Context, sdp []byte) ([]byte, error)
	// AddICECandidate applies a trickled candidate.
	AddICECandidate(ctx context.Context, candidate []byte) error
	// Authenticate binds the peer identity to the DTLS certificate.
	Authenticate(ctx context.Context) (PeerInfo, error)
	// SetMicrophone attaches the capture stream. A nil stream means muted.
	SetMicrophone(ctx context.Context, stream audio.CaptureStream) error
	// SetSpeaker attaches the playback stream.
	SetSpeaker(ctx context.Context, stream audio.PlaybackStream) error
	// RestartICE performs an ICE restart after a network change.
	RestartICE(ctx context.Context) error
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

// Unavailable is the phase 1 Manager. Every call reports that media is not
// implemented; the CLI surfaces that honestly instead of failing obscurely.
type Unavailable struct{}

// NewSession implements Manager.
func (Unavailable) NewSession(context.Context, string) (Session, error) {
	return nil, fmt.Errorf("%w: media plane arrives in phase 3", errors.New("media: not implemented"))
}

// Close implements Manager.
func (Unavailable) Close() error { return nil }
