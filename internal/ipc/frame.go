package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// WriteFrame encodes v as one newline-terminated JSON object, enforcing the
// frame size limit on the encoded bytes (not on the decoded struct).
func WriteFrame(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ipc: encode frame: %w", err)
	}
	if len(raw)+1 > MaxFrameBytes {
		return fmt.Errorf("ipc: frame of %d bytes exceeds limit %d", len(raw)+1, MaxFrameBytes)
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}

// ReadFrame reads exactly one newline-terminated JSON object.
//
// It reads at most MaxFrameBytes+1 bytes and rejects frames that exceed the
// limit instead of growing memory. A missing trailing newline on EOF is
// tolerated because some platforms close a pipe mid-frame on shutdown.
func ReadFrame(r *bufio.Reader, v any) error {
	raw, err := readLimitedLine(r)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("ipc: empty frame")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("ipc: decode frame: %w", err)
	}
	return nil
}

func readLimitedLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(out)+len(chunk) > MaxFrameBytes+1 {
			return nil, fmt.Errorf("ipc: frame exceeds %d bytes", MaxFrameBytes)
		}
		out = append(out, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(out) == 0 {
				return nil, io.EOF
			}
			break
		}
		return nil, err
	}
	// Trim the newline and optional carriage return.
	for len(out) > 0 && (out[len(out)-1] == '\n' || out[len(out)-1] == '\r') {
		out = out[:len(out)-1]
	}
	return out, nil
}

// FrameConn adapts a bidirectional connection to request/response framing with
// per-call deadlines.
type FrameConn struct {
	rw      io.ReadWriteCloser
	reader  *bufio.Reader
	timeout time.Duration
}

// NewFrameConn wraps rw. A zero timeout defaults to 10 seconds for reads and
// writes; the daemon sets a longer read timeout while idle.
func NewFrameConn(rw io.ReadWriteCloser, timeout time.Duration) *FrameConn {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &FrameConn{rw: rw, reader: bufio.NewReaderSize(rw, 32*1024), timeout: timeout}
}

// Write sends one frame.
func (c *FrameConn) Write(v any) error {
	if d, ok := c.rw.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return WriteFrame(c.rw, v)
}

// Read receives one frame.
func (c *FrameConn) Read(v any) error {
	if d, ok := c.rw.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return ReadFrame(c.reader, v)
}

// Close closes the underlying connection.
func (c *FrameConn) Close() error { return c.rw.Close() }

// SetTimeout changes the per-read/write timeout.
func (c *FrameConn) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout = d
	}
}
