//go:build linux

package ipc

import (
	"fmt"
	"syscall"
)

// syscallPeerUID reads SO_PEERCRED on Linux.
func syscallPeerUID(raw syscall.RawConn) (int, error) {
	var (
		uid   int
		opErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			opErr = err
			return
		}
		uid = int(cred.Uid)
	}); err != nil {
		return -1, fmt.Errorf("%w: %v", ErrNoPeer, err)
	}
	if opErr != nil {
		return -1, fmt.Errorf("%w: SO_PEERCRED: %v", ErrNoPeer, opErr)
	}
	return uid, nil
}
