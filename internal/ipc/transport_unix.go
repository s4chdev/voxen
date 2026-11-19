//go:build !windows

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

func jsonUnmarshal(raw []byte, out any) error { return json.Unmarshal(raw, out) }

// Listen opens the local control endpoint. The returned listener verifies the
// peer's UID on every accepted connection.
func Listen(endpoint string) (net.Listener, error) {
	ln, err := listen(endpoint)
	if err != nil {
		return nil, err
	}
	return &verifiedListener{Listener: ln}, nil
}

// Dial connects to a running daemon and verifies the server's UID.
func Dial(ctx context.Context, endpoint string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := dial(ctx, d, endpoint)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// verifiedListener wraps the platform listener and enforces the same-user rule
// per accepted connection.
type verifiedListener struct {
	net.Listener
}

func listen(endpoint string) (net.Listener, error) {
	if err := os.MkdirAll(dirOf(endpoint), 0o700); err != nil {
		return nil, fmt.Errorf("ipc: create socket directory: %w", err)
	}
	// A stale socket from a crashed daemon must not block startup, but only
	// remove it when nobody is listening: otherwise a second daemon could
	// hijack a live endpoint.
	if _, err := os.Stat(endpoint); err == nil {
		conn, dialErr := net.DialTimeout("unix", endpoint, 250*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("ipc: %s is already in use by a running daemon", endpoint)
		}
		if err := os.Remove(endpoint); err != nil {
			return nil, fmt.Errorf("ipc: remove stale socket %s: %w", endpoint, err)
		}
	}

	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", endpoint, err)
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		ln.Close()
		os.Remove(endpoint)
		return nil, fmt.Errorf("ipc: secure socket %s: %w", endpoint, err)
	}
	return ln, nil
}

func dial(ctx context.Context, d *net.Dialer, endpoint string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := verifyPeer(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}

// verifyPeer checks that the process on the other end of a Unix socket runs as
// the same user. This is belt-and-braces: the socket mode already restricts
// access, but an operator could chmod it back to 0666.
func verifyPeer(conn net.Conn) error {
	uid, err := peerUID(conn)
	if err != nil {
		return err
	}
	if uid != os.Getuid() {
		return fmt.Errorf("%w: peer uid %d is not the current user %d", ErrNoPeer, uid, os.Getuid())
	}
	return nil
}

// Accept implements net.Listener. Each accepted connection is checked against
// the daemon's own UID before it is handed to the server.
func (l *verifiedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := verifyPeer(conn); err != nil {
			conn.Close()
			// A rejected peer must not terminate the accept loop.
			continue
		}
		return conn, nil
	}
}

// peerUID returns the UID of the process on the other end.
func peerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, fmt.Errorf("%w: not a Unix socket", ErrNoPeer)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("%w: %v", ErrNoPeer, err)
	}
	uid, credErr := syscallPeerUID(raw)
	if credErr != nil {
		return -1, credErr
	}
	return uid, nil
}

// RemoveEndpoint deletes a Unix socket path, ignoring absence.
func RemoveEndpoint(endpoint string) error {
	err := os.Remove(endpoint)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// EndpointExists reports whether the endpoint file exists.
func EndpointExists(endpoint string) bool {
	info, err := os.Stat(endpoint)
	return err == nil && info.Mode()&os.ModeSocket != 0
}
