package media

import (
	"errors"
	"fmt"
	"sync"

	"github.com/thesyncim/gopus"

	"github.com/s4chdev/voxen/internal/audio"
)

// Codec errors.
var (
	ErrEncode = errors.New("media: opus encode failed")
	ErrDecode = errors.New("media: opus decode failed")
)

// Encoder encodes 48 kHz mono PCM into Opus packets using voxen's fixed profile.
//
// The profile is a constant rather than a negotiation result: every voxen
// endpoint speaks exactly one configuration, so a mismatch is a bug, not a codec
// downgrade. See ADR-008.
type Encoder struct {
	mu       sync.Mutex
	enc      *gopus.Encoder
	bitrate  int
	frameMS  int
	frameLen int
	buf      []byte
}

// NewEncoder builds an encoder for the given Opus parameters.
func NewEncoder(p OpusParameters) (*Encoder, error) {
	enc, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  int(p.ClockRate),
		Channels:    int(p.Channels),
		Application: gopus.ApplicationVoIP,
	})
	if err != nil {
		return nil, fmt.Errorf("media: create opus encoder: %w", err)
	}
	if err := enc.SetBitrate(p.Bitrate); err != nil {
		return nil, fmt.Errorf("media: set opus bitrate: %w", err)
	}
	// gopus models the libopus control as an int-valued mode: 0 disabled,
	// 1 enabled, 2 enabled for speech only. voip uses 1 (always on).
	if err := enc.SetInBandFEC(gopus.InBandFECEnabled); err != nil {
		return nil, fmt.Errorf("media: enable opus FEC: %w", err)
	}
	enc.SetDTX(p.UseDTX)
	// Tell the encoder to expect loss so FEC is actually allocated. 10% is the
	// conventional value; it does not change the instantaneous bitrate much.
	if err := enc.SetPacketLoss(10); err != nil {
		return nil, fmt.Errorf("media: set opus packet loss hint: %w", err)
	}

	frameLen := int(p.ClockRate) * p.FrameMS / 1000
	// 1275 is the maximum Opus packet size per RFC 6716.
	return &Encoder{
		enc:      enc,
		bitrate:  p.Bitrate,
		frameMS:  p.FrameMS,
		frameLen: frameLen,
		buf:      make([]byte, 1275),
	}, nil
}

// FrameLength is the number of samples per encoded frame.
func (e *Encoder) FrameLength() int { return e.frameLen }

// Encode converts one frame of mono PCM into an Opus packet.
//
// A zero-length packet means the encoder suppressed the frame (DTX); callers
// must not transmit it. That is not an error.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) < e.frameLen {
		return nil, fmt.Errorf("%w: need %d samples, got %d", ErrEncode, e.frameLen, len(pcm))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	n, err := e.enc.EncodeInt16(pcm[:e.frameLen], e.buf)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncode, err)
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]byte, n)
	copy(out, e.buf[:n])
	return out, nil
}

// SetBitrate adjusts the encoder target bitrate, used when the number of mesh
// peers changes and the local upload budget must be divided.
func (e *Encoder) SetBitrate(bps int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.enc.SetBitrate(bps); err != nil {
		return fmt.Errorf("media: set opus bitrate: %w", err)
	}
	e.bitrate = bps
	return nil
}

// Decoder decodes Opus packets into 48 kHz mono PCM.
type Decoder struct {
	mu  sync.Mutex
	dec *gopus.Decoder
}

// NewDecoder builds a decoder for the given Opus parameters.
func NewDecoder(p OpusParameters) (*Decoder, error) {
	dec, err := gopus.NewDecoder(gopus.DecoderConfig{
		SampleRate: int(p.ClockRate),
		Channels:   int(p.Channels),
	})
	if err != nil {
		return nil, fmt.Errorf("media: create opus decoder: %w", err)
	}
	return &Decoder{dec: dec}, nil
}

// Decode converts an Opus packet into PCM.
//
// An empty packet is a packet-loss indication and is decoded with packet-loss
// concealment, which is what a jitter buffer should do rather than inserting
// silence.
func (d *Decoder) Decode(packet []byte, pcm []float32) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, err := d.dec.Decode(packet, pcm)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return n, nil
}

// DecodeInt16 decodes into 16-bit PCM, the format the playback path uses.
func (d *Decoder) DecodeInt16(packet []byte, pcm []int16) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// gopus exposes Decode (float32). Convert with the standard rounding used
	// by the codec's own int16 path to avoid a second dependency surface.
	n, err := d.dec.DecodeInt16(packet, pcm)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return n, nil
}

var _ = audio.DefaultFormat
