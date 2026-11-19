//go:build linux

package ipc

// maxUnixPathLen is the usable length of sun_path on Linux (108 bytes including
// the NUL terminator).
const maxUnixPathLen = 107
