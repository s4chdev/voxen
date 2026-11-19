package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/service"
)

// newServiceCommand groups the supervision commands.
//
// These exist because "voxen start" detaches a process that dies with the login
// session and does not come back after a crash. A user who wants calls to work
// after closing the terminal needs a supervisor, and doing that by hand means
// writing a unit file whose content depends on their paths.
func (a *App) newServiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the background supervisor (systemd, launchd, Task Scheduler)",
		Long: `Install voxend under the platform's user-level supervisor.

  Linux    systemd --user unit (~/.config/systemd/user/voxend.service)
  macOS    LaunchAgent (~/Library/LaunchAgents/sh.voxen.voxend.plist)
  Windows  per-user logon task (schtasks)

A *user* supervisor is required rather than a system service: the daemon needs
your audio session, which a system service started at boot does not have. The unit
restarts voxend if it crashes and starts it when you log in.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		a.newServiceInstallCommand(),
		a.newServiceUninstallCommand(),
		a.newServiceStatusCommand(),
	)
	return cmd
}

func (a *App) serviceOptions() (service.Options, error) {
	exe, err := os.Executable()
	if err != nil {
		return service.Options{}, fmt.Errorf("locate executable: %w", err)
	}
	dir := filepath.Dir(exe)
	name := "voxend"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	if _, err := os.Stat(binary); err != nil {
		// Installed layouts often keep both binaries together; if not, the daemon
		// may be on PATH.
		if found, lookErr := exec.LookPath(name); lookErr == nil {
			binary = found
		} else {
			return service.Options{}, fmt.Errorf(
				"cannot find %s next to this binary (%s) or on PATH; install both binaries in the same directory",
				name, dir)
		}
	}
	// Carry the profile selection into the unit. Without this, a profile chosen
	// through VOXEN_* environment variables would be invisible to the supervisor,
	// and the daemon would listen on a different socket from the CLI.
	env := []string{
		"VOXEN_CONFIG_DIR=" + a.paths.ConfigDir,
		"VOXEN_STATE_DIR=" + a.paths.StateDir,
		"VOXEN_RUNTIME_DIR=" + a.paths.RuntimeDir,
	}
	return service.Options{Binary: binary, Config: a.paths.ConfigFile, Env: env}, nil
}

func (a *App) newServiceInstallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the background supervisor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.loadLocalConfig(); err != nil {
				if isNotExist(err) {
					return exitWith(ExitUsage, errors.New("no configuration found; run 'voxen init' first"))
				}
				return err
			}
			manager, err := service.ForPlatform()
			if err != nil {
				return exitWith(ExitUnsupported, err)
			}
			opts, err := a.serviceOptions()
			if err != nil {
				return err
			}

			// A daemon started by "voxen start" holds the endpoint; the supervisor
			// cannot take over until it exits.
			if a.daemonReady(cmd.Context()) {
				if _, _, code := a.runStop(cmd); code != 0 {
					return exitWith(ExitError, errors.New("a daemon is running and could not be stopped; run 'voxen stop' first"))
				}
			}

			if err := manager.Install(opts); err != nil {
				return err
			}

			if a.jsonOut {
				st, _ := manager.Status(opts)
				return a.printJSON(st)
			}
			fmt.Fprintf(a.stdout, "Installed %s.\n", manager.Name())
			fmt.Fprintf(a.stdout, "  unit:   %s\n", opts.Binary)
			fmt.Fprintf(a.stdout, "  config: %s\n", opts.Config)
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "voxend now starts at login and restarts if it crashes.")
			fmt.Fprintln(a.stdout, "Check it with:  voxen service status")
			return nil
		},
	}
	return cmd
}

func (a *App) newServiceUninstallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background supervisor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := service.ForPlatform()
			if err != nil {
				return exitWith(ExitUnsupported, err)
			}
			opts, err := a.serviceOptions()
			if err != nil {
				return err
			}
			if err := manager.Uninstall(opts); err != nil {
				return err
			}
			if a.jsonOut {
				st, _ := manager.Status(opts)
				return a.printJSON(st)
			}
			fmt.Fprintf(a.stdout, "Removed the %s unit.\n", manager.Name())
			fmt.Fprintln(a.stdout, "voxend will no longer start at login.")
			return nil
		},
	}
	return cmd
}

func (a *App) newServiceStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the supervisor is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := service.ForPlatform()
			if err != nil {
				return exitWith(ExitUnsupported, err)
			}
			opts, err := a.serviceOptions()
			if err != nil {
				// Status must work even if the daemon binary is missing, so a broken
				// installation can be diagnosed.
				opts = service.Options{Config: a.paths.ConfigFile}
			}
			st, err := manager.Status(opts)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(st)
			}
			installed := "no"
			if st.Installed {
				installed = "yes"
			}
			enabled := "no"
			if st.Enabled {
				enabled = "yes"
			}
			running := "no"
			if st.Running {
				running = "yes"
			}
			t := &table{}
			t.add("supervisor", st.Supervisor)
			t.add("installed", installed)
			t.add("starts at login", enabled)
			t.add("running", running)
			if st.Detail != "" {
				t.add("unit", st.Detail)
			}
			a.render(t)
			if !st.Installed {
				fmt.Fprintln(a.stdout)
				fmt.Fprintln(a.stdout, "Install it with:  voxen service install")
			}
			return nil
		},
	}
	return cmd
}

// runStop stops a running daemon. It returns the process exit code so callers can
// distinguish a clean stop from a refusal.
func (a *App) runStop(cmd *cobra.Command) (string, string, int) {
	stop := a.newStopCommand()
	stop.SetOut(cmd.OutOrStdout())
	stop.SetErr(cmd.ErrOrStderr())
	err := stop.RunE(stop, nil)
	if err != nil {
		return "", err.Error(), 1
	}
	return "", "", 0
}

var _ = ipc.MethodPing
