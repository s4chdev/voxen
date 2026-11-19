// Package ipc implements the platform transport for the local control socket.
//
// Linux and macOS use a Unix domain socket. Windows uses a named pipe. In both
// cases the endpoint is confined to the current user:
//
//   - Unix: the socket is created 0600 inside a 0700 directory, and the daemon
//     verifies SO_PEERCRED (Linux) / LOCAL_PEERCRED (macOS) before serving a
//     request. A same-UID check prevents another local user from driving the
//     daemon even if filesystem permissions were changed by hand.
//   - Windows: the pipe is created with a security descriptor granting access
//     only to the current user, SYSTEM and Administrators.
package ipc

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrNoPeer is returned when a connection's peer credentials cannot be read.
var ErrNoPeer = errors.New("ipc: cannot determine peer credentials")

// Client is a request/response client for the daemon.
type Client struct {
	conn *FrameConn
}

// NewClient wraps an existing connection and performs the protocol handshake
// lazily on the first call.
func NewClient(conn net.Conn, timeout time.Duration) *Client {
	return &Client{conn: NewFrameConn(conn, timeout)}
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call sends req and decodes the result into out. A structured *Error from the
// daemon is returned unchanged so callers can switch on Code.
func (c *Client) Call(req *Request, out any) error {
	if req.Version == 0 {
		req.Version = ProtocolVersion
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if err := c.conn.Write(req); err != nil {
		return fmt.Errorf("ipc: write request: %w", err)
	}
	var resp Response
	if err := c.conn.Read(&resp); err != nil {
		return fmt.Errorf("ipc: read response: %w", err)
	}
	if err := resp.Validate(); err != nil {
		return err
	}
	if resp.ID != req.ID {
		return fmt.Errorf("ipc: response id %q does not match request %q", resp.ID, req.ID)
	}
	if !resp.OK {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := jsonUnmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("ipc: decode result: %w", err)
		}
	}
	return nil
}
