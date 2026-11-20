package daemon

import (
	"context"
	"net"
	"time"

	"github.com/s4chdev/voxen/internal/ipc"
)

// dialForTest opens a raw connection to a daemon endpoint.
func dialForTest(endpoint string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return ipc.Dial(ctx, endpoint, 500*time.Millisecond)
}
