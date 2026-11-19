// Package cli implements the voxen command line interface.
//
// Every command is a thin client of the daemon, except "init", which creates
// local state before a daemon can start. Commands never open audio devices,
// never read private keys directly and never speak WebRTC: that is the daemon's
// job, and keeping it there means the CLI can run as an unprivileged, short
// lived process.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/paths"
	"github.com/s4chdev/voxen/internal/version"
)

// Exit codes. They are part of the CLI contract used by scripts and by CI.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitUnavailable = 3
	ExitUnsupported = 4
	ExitAuth        = 5
)

// App carries process-wide CLI state.
type App struct {
	paths      paths.Paths
	cfg        config.Config
	jsonOut    bool
	timeout    time.Duration
	configPath string

	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader

	localIdentity *identity.Identity
	foreground    bool
}

// exitError lets a command choose a specific exit code without printing twice.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitWith(code int, err error) error {
	return &exitError{code: code, err: err}
}

// Execute runs the CLI and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	app := &App{stdout: stdout, stderr: stderr, stdin: stdin}

	root := app.newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	var coded *exitError
	if errors.As(err, &coded) {
		if coded.err != nil {
			fmt.Fprintln(stderr, "voxen:", coded.err)
		}
		return coded.code
	}
	if errors.Is(err, errPrinted) {
		return ExitError
	}
	// Cobra prints usage errors itself; avoid duplicating them.
	fmt.Fprintln(stderr, "voxen:", err)
	return ExitError
}

// errPrinted signals that the command already rendered its error.
var errPrinted = errors.New("error already reported")

func (a *App) newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "voxen",
		Short: "Encrypted peer-to-peer voice calls from the terminal",
		Long: `voxen is a peer-to-peer, end-to-end encrypted voice communication tool.

voxen runs a background daemon (voxend) that owns audio devices, WebRTC sessions
and the local Ed25519 identity. This CLI talks to that daemon over a private
local socket and never handles audio or keys itself.

Getting started:
  voxen init              create your identity and configuration
  voxen verify <fp>       verify a peer's fingerprint out of band
  voxen start             start the background daemon
  voxen status            show daemon and session state

Media is only sent to peers whose fingerprint you have verified. voxen never
relays audio: if two peers cannot connect directly, the call fails and says so.`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "help" || cmd.Name() == "version" {
				return nil
			}
			p, err := paths.Resolve()
			if err != nil {
				return err
			}
			a.paths = p
			if a.configPath != "" {
				abs, err := absPath(a.configPath)
				if err != nil {
					return err
				}
				a.paths.ConfigFile = abs
			}
			if a.timeout == 0 {
				a.timeout = 10 * time.Second
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&a.configPath, "config", "", "path to config.yaml (overrides VOXEN_CONFIG)")
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "emit machine readable JSON")
	root.PersistentFlags().DurationVar(&a.timeout, "timeout", 10*time.Second, "timeout for daemon calls")

	root.AddCommand(
		a.newInitCommand(),
		a.newStartCommand(),
		a.newStopCommand(),
		a.newRestartCommand(),
		a.newStatusCommand(),
		a.newPeersCommand(),
		a.newDevicesCommand(),
		a.newConfigCommand(),
		a.newLogsCommand(),
		a.newDoctorCommand(),
		a.newMuteCommand(),
		a.newUnmuteCommand(),
		a.newCreateCommand(),
		a.newJoinCommand(),
		a.newLeaveCommand(),
		a.newPeerMuteCommand(),
		a.newIdentityCommand(),
		a.newVerifyCommand(),
		a.newTrustCommand(),
		a.newRevokeCommand(),
		a.newVersionCommand(),
		a.newDaemonCommand(),
		a.newServiceCommand(),
	)
	return root
}

func (a *App) newVersionCommand() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if short {
				fmt.Fprintln(a.stdout, version.Version)
				return nil
			}
			if a.jsonOut {
				return a.printJSON(version.Get())
			}
			info := version.Get()
			fmt.Fprintf(a.stdout, "voxen %s\n", info.Version)
			fmt.Fprintf(a.stdout, "  commit:   %s\n", info.Commit)
			fmt.Fprintf(a.stdout, "  built:    %s\n", info.BuildDate)
			fmt.Fprintf(a.stdout, "  phase:    %d (%s)\n", info.Phase, info.PhaseName)
			fmt.Fprintf(a.stdout, "  go:       %s\n", info.GoVersion)
			fmt.Fprintf(a.stdout, "  platform: %s\n", info.Platform)
			return nil
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version number")
	return cmd
}

// contextWithSignals returns a context cancelled by SIGINT/SIGTERM.
func contextWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
