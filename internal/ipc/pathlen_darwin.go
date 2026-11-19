//go:build darwin

package ipc

// maxUnixPathLen is the usable length of sun_path on macOS (104 bytes including
// the NUL terminator).
const maxUnixPathLen = 103
