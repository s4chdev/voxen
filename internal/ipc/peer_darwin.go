//go:build darwin

package ipc

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// syscallPeerUID reads LOCAL_PEERCRED on macOS.
//
// x/sys/unix exposes the correct struct xucred layout, which avoids hardcoding
// native-endian field offsets for a security-relevant check.
func syscallPeerUID(raw syscall.RawConn) (int, error) {
	var (
		uid   int
		opErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			opErr = err
			return
		}
		uid = int(cred.Uid)
	}); err != nil {
		return -1, fmt.Errorf("%w: %v", ErrNoPeer, err)
	}
	if opErr != nil {
		return -1, fmt.Errorf("%w: LOCAL_PEERCRED: %v", ErrNoPeer, opErr)
	}
	return uid, nil
}
