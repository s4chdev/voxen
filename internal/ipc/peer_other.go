//go:build !windows && !linux && !darwin

package ipc

import (
	"fmt"
	"syscall"
)

// syscallPeerUID is a safe fallback for Unix platforms without a portability
// wrapper here. voxen refuses to serve a local control socket when it cannot
// prove the peer's UID; the socket's 0600 mode stays the only defence.
func syscallPeerUID(raw syscall.RawConn) (int, error) {
	return -1, fmt.Errorf("%w: peer credential check is not implemented on this platform", ErrNoPeer)
}
