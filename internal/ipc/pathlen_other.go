//go:build !windows && !linux && !darwin

package ipc

// maxUnixPathLen is a conservative bound for platforms without an explicit
// limit defined here.
const maxUnixPathLen = 103
