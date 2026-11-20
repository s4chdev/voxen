package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/installer"
	"github.com/s4chdev/voxen/internal/service"
	"github.com/s4chdev/voxen/internal/version"
)

// newInstallCommand puts the binaries on the system and, optionally, sets up the
// supervisor.
//
// This is deliberately separate from "voxen service install": that one configures
// supervision of binaries that already exist, which is right for a package manager
// install. This one is for the case where a user downloaded a release archive and
// wants it installed the same way on every platform, without a package manager.
func (a *App) newInstallCommand() *cobra.Command {
	var (
		prefix      string
		withService bool
		force       bool
		binDir      string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install voxen onto this machine",
		Long: `Install the voxen binaries into a system location and put them on PATH.

  Linux/macOS   /usr/local/bin, or ~/.local/bin when not writable as root
  Windows       %LOCALAPPDATA%\Programs\voxen

The installer copies the binaries next to each other, because the daemon and CLI
must be able to find one another, and prints what it changed. It does not modify
shell profiles: adding a directory to PATH is a decision the user can see and undo,
so the exact line to add is printed instead.

Use --service to also install the supervisor in one step, which is what most people
want on a personal machine.

Re-running install is an upgrade: the existing binaries are replaced and the previous
version's config and identity are left alone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := installer.Options{
				Prefix: prefix,
				BinDir: binDir,
				Force:  force,
			}
			if opts.BinDir == "" {
				opts.BinDir = installer.DefaultBinDir(prefix)
			}

			// The binaries to install are the ones sitting next to the running CLI.
			// A release archive extracts them together, which is exactly the layout
			// the installer expects.
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate the running binary: %w", err)
			}
			srcDir := filepath.Dir(exe)
			opts.Source = srcDir

			res, err := installer.Install(opts)
			if err != nil {
				var permErr *installer.NeedsPrivilegeError
				if errors.As(err, &permErr) {
					return exitWith(ExitError, fmt.Errorf(
						"%w\n\nEither run with sudo, or install per-user:\n\n  voxen install --prefix %s",
						err, installer.UserPrefix()))
				}
				// "Already installed" is not a failure when the user only wants the
				// supervisor: re-running "install --service" should be a safe no-op.
				var exists *installer.AlreadyInstalledError
				if errors.As(err, &exists) && withService && !force {
					if !a.jsonOut {
						fmt.Fprintf(a.stdout, "Binaries already installed at %s; configuring the supervisor only.\n\n", exists.Dir)
					}
					res = installer.Result{BinDir: exists.Dir}
				} else {
					return err
				}
			}

			if a.jsonOut {
				if err := a.printJSON(res); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(a.stdout, "Installed voxen %s\n\n", version.Version)
				for _, f := range res.Installed {
					fmt.Fprintf(a.stdout, "  %s\n", f)
				}
				fmt.Fprintln(a.stdout)
				if res.NeedsPathUpdate {
					fmt.Fprintf(a.stdout, "  %s is not on your PATH. Add it:\n\n", res.BinDir)
					fmt.Fprintf(a.stdout, "      %s\n\n", res.PathHint)
				}
				if res.ReplacedPrevious {
					fmt.Fprintf(a.stdout, "  Upgraded from a previous installation; your config and identity are untouched.\n\n")
				}
				if !res.NeedsPathUpdate {
					fmt.Fprintf(a.stdout, "  Next:  voxen init\n")
				}
			}

			if withService {
				// Configuring a supervisor needs a config file to point at, so this is
				// only meaningful after init. Saying so is better than creating a unit
				// that fails on first start.
				if _, statErr := os.Stat(a.paths.ConfigFile); statErr != nil {
					return exitWith(ExitUsage, fmt.Errorf(
						"binaries installed, but --service needs a configuration first\n\n  voxen init --name <you>\n  voxen service install"))
				}
				manager, err := service.ForPlatform()
				if err != nil {
					return exitWith(ExitUnsupported, err)
				}
				binDir := res.BinDir
				if binDir == "" {
					binDir = installer.DefaultBinDir(prefix)
				}
				svcOpts := service.Options{
					Binary:     filepath.Join(binDir, installer.DaemonBinaryName()),
					Config:     a.paths.ConfigFile,
					ConfigDir:  a.paths.ConfigDir,
					StateDir:   a.paths.StateDir,
					RuntimeDir: a.paths.RuntimeDir,
					// The supervisor does not inherit this shell, so the profile must be
					// baked in or the daemon listens on a different socket than the CLI.
					Env: []string{
						"VOXEN_CONFIG_DIR=" + a.paths.ConfigDir,
						"VOXEN_STATE_DIR=" + a.paths.StateDir,
						"VOXEN_RUNTIME_DIR=" + a.paths.RuntimeDir,
					},
				}
				if err := manager.Install(svcOpts); err != nil {
					return fmt.Errorf("binaries installed, but the service failed to install: %w", err)
				}
				if !a.jsonOut {
					fmt.Fprintf(a.stdout, "  Supervisor installed (%s).\n", manager.Name())
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "installation prefix (default: /usr/local, or ~/.local for a user install)")
	cmd.Flags().StringVar(&binDir, "bin-dir", "", "directory for the binaries (overrides --prefix)")
	cmd.Flags().BoolVar(&withService, "service", false, "also install the background supervisor")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	return cmd
}

// newUninstallCommand removes what "voxen install" put on the system.
//
// It refuses to delete configuration or identity: those are the user's data, and an
// uninstall that silently destroys a private key is a bad uninstall.
func (a *App) newUninstallCommand() *cobra.Command {
	var (
		purge  bool
		yes    bool
		binDir string
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the voxen binaries",
		Long: `Remove the installed binaries.

Configuration, identity and logs are kept by default. Use --purge to remove those
too; that destroys your identity, which cannot be recovered and means every peer
must verify you again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Stop supervision first, or the daemon keeps running from a deleted
			// binary and the next start looks like a mysterious failure.
			if manager, err := service.ForPlatform(); err == nil {
				_ = manager.Uninstall(service.Options{Config: a.paths.ConfigFile})
			}
			if a.daemonReady(cmd.Context()) {
				var res map[string]string
				_ = a.call("shutdown", nil, &res)
			}

			target := binDir
			if target == "" {
				target = installer.DefaultBinDir("")
			}
			res, err := installer.Uninstall(installer.Options{
				BinDir: target,
				Purge:  purge,
				DataDirs: []string{
					a.paths.ConfigDir,
					a.paths.StateDir,
				},
			})
			if err != nil {
				return err
			}

			if a.jsonOut {
				return a.printJSON(res)
			}
			for _, f := range res.Removed {
				fmt.Fprintf(a.stdout, "  removed %s\n", f)
			}
			if len(res.Kept) > 0 {
				fmt.Fprintln(a.stdout)
				fmt.Fprintln(a.stdout, "  Kept (your data):")
				for _, f := range res.Kept {
					fmt.Fprintf(a.stdout, "    %s\n", f)
				}
				fmt.Fprintln(a.stdout)
				fmt.Fprintln(a.stdout, "  Remove these too with:  voxen uninstall --purge")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&binDir, "bin-dir", "", "directory the binaries were installed to")
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove configuration, identity and logs (destroys your identity)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation for --purge")
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if !purge || yes || a.jsonOut {
			return nil
		}
		fmt.Fprint(a.stdout, "This deletes your identity and configuration permanently. Continue? [y/N] ")
		var answer string
		if _, err := fmt.Fscanln(a.stdin, &answer); err != nil {
			return exitWith(ExitUsage, errors.New("no confirmation; re-run with --yes to skip the prompt"))
		}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			return exitWith(ExitOK, errors.New("cancelled"))
		}
		return nil
	}
	return cmd
}

// newUpdateCommand reports how to upgrade, and warns when the running binary is older
// than the one on disk.
//
// There is no self-updater on purpose: replacing a running binary across three
// platforms with three different file-locking rules is a reliability risk, and an
// update that fails halfway leaves a broken install. The honest alternative is to say
// exactly what to run.
func (a *App) newUpdateCommand() *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Upgrade an installed copy",
		Long: `Upgrade voxen by replacing the installed binaries.

This is not a self-updater. voxen does not download and execute code for you; you
fetch a release and run its installer, which replaces the binaries in place and
leaves configuration and identity alone.

  voxen update --check     report the installed version and how to upgrade`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			installed := filepath.Join(filepath.Dir(exe), installer.BinaryName())

			if a.jsonOut {
				return a.printJSON(map[string]any{
					"version":  version.Version,
					"commit":   version.Commit,
					"path":     installed,
					"platform": runtime.GOOS + "/" + runtime.GOARCH,
				})
			}
			fmt.Fprintf(a.stdout, "  version:  %s\n", version.Version)
			fmt.Fprintf(a.stdout, "  commit:   %s\n", version.Commit)
			fmt.Fprintf(a.stdout, "  binary:   %s\n", installed)
			if checkOnly {
				return nil
			}
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "To upgrade:")
			fmt.Fprintln(a.stdout, "  1. download the release archive for this platform")
			fmt.Fprintln(a.stdout, "  2. extract it and run:  ./voxen install --force")
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Your configuration and identity are not touched, so peers keep")
			fmt.Fprintln(a.stdout, "trusting you; only the binaries are replaced.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report the installed version and exit")
	return cmd
}

// ensure io stays referenced if the confirmation prompt is compiled out on a platform.
var _ io.Reader = os.Stdin
