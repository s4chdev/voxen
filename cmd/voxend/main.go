// Command voxend is the voxen background daemon.
//
// It can be started directly (useful under systemd, launchd or a Windows
// service wrapper) or spawned by "voxen start". It owns audio devices, WebRTC
// sessions and the local Ed25519 identity, and serves the CLI over a private
// local socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/daemon"
	"github.com/s4chdev/voxen/internal/logging"
	"github.com/s4chdev/voxen/internal/paths"
	"github.com/s4chdev/voxen/internal/version"
)

func main() {
	var (
		foreground = flag.Bool("foreground", false, "log to stderr in addition to the log file")
		configPath = flag.String("config", "", "path to config.yaml")
		printVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *printVer {
		fmt.Println("voxend", version.String())
		return
	}

	if err := run(*foreground, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "voxend:", err)
		os.Exit(1)
	}
}

func run(foreground bool, configPath string) error {
	p, err := paths.Resolve()
	if err != nil {
		return err
	}
	if configPath != "" {
		p.ConfigFile = configPath
	}
	if err := p.Ensure(); err != nil {
		return err
	}

	// Read logging settings from the configuration. A broken config must still
	// produce readable output, so fall back to the defaults for logging only and
	// let daemon.New report the real configuration error.
	logCfg := config.Default().Logging
	if cfg, err := config.Load(p.ConfigFile); err == nil {
		logCfg = cfg.Logging
	}

	logPath := p.LogFile
	if logCfg.File != "" {
		logPath = logCfg.File
	}
	sink, sinkErr := logging.OpenSink(logPath, foreground)
	if sinkErr != nil {
		return sinkErr
	}
	defer sink.Close()

	logger, err := logging.New(logCfg, sink.Writer)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv, err := daemon.New(ctx, daemon.Options{
		Paths:      p,
		Foreground: foreground,
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
