package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/ipc"
)

// newDaemonCommand is the hidden entry point used by "voxen start" to re-exec
// this binary as the daemon. Keeping it in the same executable means there is
// exactly one binary to install on every platform and no PATH assumptions.
func (a *App) newDaemonCommand() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Run the voxen daemon in the foreground (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.loadLocalConfig(); err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			ctx, cancel := contextWithSignals()
			defer cancel()
			a.foreground = foreground
			return a.runDaemonInProcess(ctx)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "log to stderr")
	return cmd
}

func (a *App) newStartCommand() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the background daemon",
		Long: `Start voxend in the background.

The daemon owns audio devices, WebRTC sessions and your identity. With
--foreground it stays attached to this terminal and logs to stderr, which is the
easiest way to debug connection problems.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.loadLocalConfig(); err != nil {
				if isNotExist(err) {
					return exitWith(ExitUsage, errors.New("no configuration found; run 'voxen init' first"))
				}
				return err
			}
			if err := a.ensureInitialised(); err != nil {
				return err
			}

			ctx := context.Background()
			if a.daemonReady(ctx) {
				fmt.Fprintln(a.stdout, "voxend is already running.")
				return nil
			}

			if foreground {
				fmt.Fprintln(a.stdout, "Starting voxend in the foreground. Press Ctrl-C to stop.")
				runCtx, cancel := contextWithSignals()
				defer cancel()
				a.foreground = true
				return a.runDaemonInProcess(runCtx)
			}

			if err := a.spawnDaemon(); err != nil {
				return err
			}
			wait := time.Duration(a.cfg.Daemon.StartupTimeoutMS) * time.Millisecond
			deadline := time.Now().Add(wait)
			for time.Now().Before(deadline) {
				if a.daemonReady(ctx) {
					var ping ipc.PingResult
					_ = a.call(ipc.MethodPing, nil, &ping)
					if a.jsonOut {
						return a.printJSON(map[string]any{"state": "running", "pid": ping.PID, "endpoint": a.endpoint()})
					}
					fmt.Fprintf(a.stdout, "voxend started (pid %d, endpoint %s)\n", ping.PID, a.endpoint())
					return nil
				}
				time.Sleep(50 * time.Millisecond)
			}
			return exitWith(ExitError, fmt.Errorf(
				"voxend did not become ready within %s; check logs with 'voxen logs'", wait))
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run attached to this terminal instead of detaching")
	return cmd
}

// spawnDaemon re-executes the current binary as "voxen daemon", detached.
func (a *App) spawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	args := []string{"daemon"}
	if a.configPath != "" {
		args = append(args, "--config", a.configPath)
	}

	logPath := a.paths.LogFile
	if a.cfg.Logging.File != "" {
		logPath = a.cfg.Logging.File
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	detach(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Release the child: the CLI must not become its reaper.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("detach daemon: %w", err)
	}
	return nil
}

func (a *App) newStopCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.loadLocalConfig(); err != nil {
				// Stop must work even with a broken config: the endpoint may come
				// from the environment or the default layout.
				a.cfg = defaultConfigForStop()
			}
			ctx := context.Background()
			if !a.daemonReady(ctx) {
				fmt.Fprintln(a.stdout, "voxend is not running.")
				return nil
			}

			if force {
				pid, err := a.readPID()
				if err != nil {
					return err
				}
				if err := killProcess(pid); err != nil {
					return fmt.Errorf("stop daemon (pid %d): %w", pid, err)
				}
			} else {
				var res map[string]string
				if err := a.call(ipc.MethodShutdown, ipc.ShutdownParams{Graceful: true}, &res); err != nil {
					return err
				}
			}

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if !a.daemonReady(ctx) {
					fmt.Fprintln(a.stdout, "voxend stopped.")
					return nil
				}
				time.Sleep(50 * time.Millisecond)
			}
			return exitWith(ExitError, errors.New("voxend did not stop; try 'voxen stop --force'"))
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "terminate the daemon process instead of asking it to stop")
	return cmd
}

func (a *App) newRestartCommand() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.loadLocalConfig(); err != nil {
				return err
			}
			if a.daemonReady(context.Background()) {
				var res map[string]string
				if err := a.call(ipc.MethodShutdown, ipc.ShutdownParams{Graceful: true}, &res); err != nil {
					return err
				}
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if !a.daemonReady(context.Background()) {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			startCmd := a.newStartCommand()
			startCmd.Flags().Lookup("foreground")
			if foreground {
				_ = startCmd.Flags().Set("foreground", "true")
			}
			return startCmd.RunE(cmd, nil)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run attached to this terminal")
	return cmd
}

func (a *App) readPID() (int, error) {
	raw, err := os.ReadFile(a.paths.PIDFile)
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 0 {
		return 0, fmt.Errorf("pid file %s is corrupt", a.paths.PIDFile)
	}
	return pid, nil
}

// defaultConfigForStop builds a minimal config when the file is unreadable.
func defaultConfigForStop() config.Config { return config.Default() }
