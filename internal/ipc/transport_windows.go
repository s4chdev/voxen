//go:build windows

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func jsonUnmarshal(raw []byte, out any) error { return json.Unmarshal(raw, out) }

// Listen opens the local control endpoint as a named pipe restricted to the
// current user.
func Listen(endpoint string) (net.Listener, error) { return listen(endpoint) }

// Dial connects to a running daemon's named pipe.
func Dial(ctx context.Context, endpoint string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return dial(ctx, d, endpoint)
}

// currentUserSID resolves the SID of the user running voxen so the pipe ACL can
// grant access to exactly that account.
func currentUserSID() (string, error) {
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("ipc: read process token: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return "", fmt.Errorf("ipc: process token has no user SID")
	}
	if !user.User.Sid.IsValid() {
		return "", fmt.Errorf("ipc: invalid user SID")
	}
	return user.User.Sid.String(), nil
}

func listen(endpoint string) (net.Listener, error) {
	// Require an explicit pipe path: a relative path here would be ambiguous.
	if !strings.HasPrefix(strings.ToLower(endpoint), `\\.\pipe\`) {
		return nil, fmt.Errorf("ipc: %q is not a named pipe path", endpoint)
	}
	sid, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("ipc: determine current user: %w", err)
	}
	// Deny network access; allow the current user, SYSTEM and Administrators.
	// The current-user ACE is the security boundary.
	sddl := fmt.Sprintf("D:P(A;;GA;;;%s)(A;;GA;;;SY)(A;;GA;;;BA)", sid)
	ln, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        false,
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		// A pipe held by a running daemon reports ERROR_ACCESS_DENIED rather
		// than "already exists", because that daemon's descriptor excluded this
		// process from the inherited ACE list. Translate it into the same
		// message the Unix path uses so "voxen start" and operator docs read
		// consistently, and so a real permission problem is still distinguishable
		// by the underlying error text.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("ipc: %s is already in use by a running daemon", endpoint)
		}
		return nil, fmt.Errorf("ipc: listen %s: %w", endpoint, err)
	}
	return ln, nil
}

func dial(ctx context.Context, d *net.Dialer, endpoint string) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// RemoveEndpoint is a no-op on Windows: pipes vanish when the last handle
// closes, so there is nothing to clean up.
func RemoveEndpoint(string) error { return nil }

// EndpointExists reports whether a pipe is currently being served.
func EndpointExists(endpoint string) bool {
	conn, err := winio.DialPipe(endpoint, nil)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
