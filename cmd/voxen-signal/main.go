// Command voxen-signal is the self-hosted signaling server.
//
// It coordinates rooms and relays opaque control messages between voxen clients.
// It never sees audio: media flows directly between peers over DTLS-SRTP, and the
// server cannot authenticate a peer on anyone's behalf.
//
// Deployment is deliberately simple: one binary, no database, no persistent
// state. Rooms live in memory and vanish when empty.
//
//	voxen-signal -listen 0.0.0.0:8080
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s4chdev/voxen/internal/version"
	serversig "github.com/s4chdev/voxen/server/signaling"
)

func main() {
	var (
		listen     = flag.String("listen", "127.0.0.1:8080", "address to listen on")
		logLevel   = flag.String("log-level", "info", "debug, info, warn or error")
		logFormat  = flag.String("log-format", "text", "text or json")
		maxRooms   = flag.Int("max-rooms", 256, "maximum concurrent rooms")
		maxMembers = flag.Int("max-members", 8, "maximum participants per room (hard cap 16)")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("voxen-signal", version.String())
		return
	}

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "invalid -log-level %q\n", *logLevel)
		os.Exit(2)
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if *logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	logger := slog.New(handler)

	srv, err := serversig.New(serversig.Config{
		Addr:       *listen,
		MaxRooms:   *maxRooms,
		MaxMembers: *maxMembers,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// A second signal cancels immediately rather than waiting for the graceful
	// shutdown to finish.
	go func() {
		<-ctx.Done()
		select {
		case <-time.After(10 * time.Second):
			logger.Error("shutdown is taking too long; exiting")
			os.Exit(1)
		case <-context.Background().Done():
		}
	}()

	logger.Info("starting voxen-signal", "version", version.String(), "listen", *listen)
	if err := srv.Run(ctx); err != nil {
		logger.Error("server stopped with an error", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}
