//go:build windows

package paths

import (
	"os"
	"strings"
)

// userTag returns a stable tag for the per-user runtime directory.
//
// It is only used for the macOS runtime directory default. A sanitised user name
// keeps the value deterministic per account.
func userTag() string {
	name := os.Getenv("USERNAME")
	if name == "" {
		return "user"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}
