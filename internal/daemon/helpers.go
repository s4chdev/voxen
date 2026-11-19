package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/s4chdev/voxen/internal/ipc"
)

// jsonRaw is an alias that keeps the server readable; json.RawMessage is the
// underlying type.
type jsonRaw = json.RawMessage

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// decodeParams decodes request parameters and converts decoding failures into a
// structured bad-request error.
func decodeParams(raw jsonRaw, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ipc.NewError(ipc.CodeBadRequest, "invalid parameters: %v", err)
	}
	return nil
}

// tailFile returns the last n lines of path. It reads the file sequentially with
// a bounded scanner: voxen log files are small and rotated, and a random-access
// seek would add complexity for no practical gain.
func tailFile(path string, n int) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	ring := make([]string, 0, n)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	truncated := false
	for scanner.Scan() {
		if len(ring) == n {
			copy(ring, ring[1:])
			ring[n-1] = scanner.Text()
			truncated = true
			continue
		}
		ring = append(ring, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("daemon: read log file: %w", err)
	}
	return ring, truncated, nil
}
