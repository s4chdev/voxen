//go:build !windows

package paths

import (
	"fmt"
	"os"
)

func userTag() string {
	return fmt.Sprintf("%d", os.Getuid())
}
