//go:build !windows

package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResolveEndpointShortensLongPaths covers the macOS regression where a
// t.TempDir()-based profile exceeded the 104-byte sun_path limit, so the daemon
// could not bind and the CLI waited for a socket that was never created.
func TestResolveEndpointShortensLongPaths(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40), "voxend.sock")
	if len(long) <= maxUnixPathLen {
		t.Skipf("temporary directory is short (%d bytes); cannot build an over-long path", len(long))
	}
	got := resolveEndpoint(long)
	if len(got) > maxUnixPathLen {
		t.Fatalf("resolved endpoint is still too long: %d bytes > %d\n%s", len(got), maxUnixPathLen, got)
	}
	// Deterministic: the CLI and the daemon must derive the same path.
	if again := resolveEndpoint(long); again != got {
		t.Fatalf("resolution is not deterministic:\n%s\n%s", got, again)
	}
	if got == resolveEndpoint(long+"x") {
		t.Fatal("different endpoints collapsed to the same socket path")
	}
}

func TestResolveEndpointLeavesShortPathsAlone(t *testing.T) {
	short := "/tmp/vx.sock"
	if got := resolveEndpoint(short); got != short {
		t.Fatalf("short path was rewritten: %s -> %s", short, got)
	}
}

// TestListenDialLongPath proves the fix end to end: a listener created with an
// over-long endpoint is reachable by a client using the same original string.
func TestListenDialLongPath(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, strings.Repeat("d", 50), strings.Repeat("e", 50), "voxend.sock")
	if len(long) <= maxUnixPathLen {
		t.Skip("temporary directory is too short to exceed the socket path limit")
	}

	ln, err := Listen(long)
	if err != nil {
		t.Fatalf("listen with long endpoint: %v", err)
	}
	defer func() {
		ln.Close()
		_ = RemoveEndpoint(long)
	}()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
			close(accepted)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, long, time.Second)
	if err != nil {
		t.Fatalf("dial long endpoint: %v", err)
	}
	conn.Close()

	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("listener did not accept a connection on the shortened path")
	}

	// Cleanup must target the same resolved path, not the original.
	ln.Close()
	if err := RemoveEndpoint(long); err != nil {
		t.Fatalf("remove endpoint: %v", err)
	}
	if _, err := os.Stat(resolveEndpoint(long)); !os.IsNotExist(err) {
		t.Fatalf("resolved socket still exists after removal: %v", err)
	}
}
