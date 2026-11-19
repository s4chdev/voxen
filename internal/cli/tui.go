package cli

import (
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/tui"
)

// newTUICommand runs the live call view.
//
// The TUI is a viewer and a remote control, never an owner: it polls the daemon and
// forwards key presses. Closing it does not end the call, which is why "q" is
// documented as such in the help panel.
func (a *App) newTUICommand() *cobra.Command {
	var noColor bool
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the live call view",
		Long: `Open a full-screen view of the current call.

Shows participants, connection state, whether the path is direct, round-trip time,
packet loss, and who is speaking, plus live audio counters.

Keys:
  m        mute or unmute your microphone
  M        mute or unmute the selected participant locally
  j / k    select the next or previous participant
  r        refresh now
  ?        show or hide the key reference
  q        quit the view (the call keeps running)

This is a viewer and remote control. The daemon owns the call, so closing the view
does not disconnect anyone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The TUI needs a daemon to poll; failing fast with the usual hint is
			// better than rendering an empty screen that says "unreachable".
			if !a.daemonReady(cmd.Context()) {
				return exitWith(ExitUnavailable,
					fmt.Errorf("voxend is not running (endpoint %s); start it with 'voxen start'", a.endpoint()))
			}

			// Styling follows the environment unless explicitly disabled. Bubble Tea
			// handles the alternate screen; a non-TTY stdin (a pipe, CI) is refused
			// because a full-screen app there would just hang.
			if !isTerminal(os.Stdin) {
				return exitWith(ExitUsage, errors.New(
					"voxen tui needs an interactive terminal; use 'voxen status --watch' or 'voxen --json status' instead"))
			}

			model := tui.New(ipcTUI{app: a}, tui.Config{NoColor: noColor || a.jsonOut})
			program := tea.NewProgram(model, tea.WithAltScreen())
			if _, err := program.Run(); err != nil {
				return fmt.Errorf("tui: %w", err)
			}
			return nil
		},
	}
	var helpPage bool
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable colour")
	cmd.Flags().BoolVar(&helpPage, "help-page", false,
		"print the rendered help and exit (Markdown via Glamour)")

	// --help-page is handled before the terminal check so it works when piped.
	origRun := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if helpPage {
			d := tui.DiagnosticText{NoColor: noColor || a.jsonOut, Width: 80}
			fmt.Fprintln(a.stdout, d.Render(tui.HelpMarkdown(tui.Keys)))
			return nil
		}
		return origRun(cmd, args)
	}
	return cmd
}

// isTerminal reports whether f is a character device, which is the portable
// approximation of "interactive" without pulling in a dependency for one check.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
