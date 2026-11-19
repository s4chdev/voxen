//go:build windows

package paths

import (
	"os"
	"strings"
)

// userTag is used only for the short temp fallback path, which is not used on
// Windows. A sanitised user name keeps the value deterministic per account.
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
