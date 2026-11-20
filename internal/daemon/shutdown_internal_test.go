package daemon

import (
	"context"
	"testing"
	"time"
)

// TestShutdownIsBounded proves a stop request returns promptly even with a client
// connection held open and audio that never started.
//
// The bound matters because "voxen stop" is what a user reaches for when something is
// wrong; a stop that takes seconds on a broken machine is its own bug. The components
// are individually bounded, and this asserts their sum stays reasonable.
func TestShutdownIsBounded(t *testing.T) {
	prof := newInternalProfile(t)
	srv, err := New(context.Background(), Options{Paths: prof, Foreground: true, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Wait for the endpoint, then hold a client connection open: an idle connection is
	// exactly what used to extend shutdown.
	deadline := time.Now().Add(10 * time.Second)
	var conn interface{ Close() error }
	for time.Now().Before(deadline) {
		c, err := dialForTest(srv.cfg.Endpoint(srv.opts.Paths))
		if err == nil {
			conn = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("daemon did not start listening")
	}
	defer conn.Close()

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	elapsed := time.Since(start)
	t.Logf("shutdown took %s with a client held open", elapsed)
	if elapsed > 6*time.Second {
		t.Fatalf("shutdown took %s; the components are not bounded", elapsed)
	}
}
