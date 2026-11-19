package tui

import (
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
)

// KeyMap is the single source of truth for the TUI's keys.
//
// It is defined with Bubbles' key bindings rather than as raw string comparisons so
// the same definition drives three things at once: key matching, the short help
// line, and the full help panel. That makes the help text structurally unable to
// describe a key that does not exist, which is the drift this project cares about.
type KeyMap struct {
	MuteSelf key.Binding
	MutePeer key.Binding
	Up       key.Binding
	Down     key.Binding
	Refresh  key.Binding
	Help     key.Binding
	Quit     key.Binding
}

// Keys is the active key map.
var Keys = KeyMap{
	MuteSelf: key.NewBinding(
		key.WithKeys("m"),
		key.WithHelp("m", "mute self"),
	),
	MutePeer: key.NewBinding(
		key.WithKeys("M"),
		key.WithHelp("M", "mute peer"),
	),
	Up: key.NewBinding(
		key.WithKeys("k", "up"),
		key.WithHelp("j/k", "select peer"),
	),
	// Down carries the help text because the two directions are one concept to a
	// user. Up is disabled for help purposes so the binding is not listed twice.
	Down: key.NewBinding(
		key.WithKeys("j", "down"),
		key.WithHelp("j/k", "select peer"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "refresh"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c", "esc"),
		// The wording matters: closing the view does not end the call, and a user
		// who believes otherwise will not close it.
		key.WithHelp("q", "quit view (call keeps running)"),
	),
}

// ShortHelp is the one-line hint shown by default.
func (k KeyMap) ShortHelp() []key.Binding {
	// MutePeer and Up/Down share a slot each in the short line; Bubbles renders
	// bindings in order, so the sequence here is the sequence a user reads.
	return []key.Binding{k.MuteSelf, k.MutePeer, k.Down, k.Help, k.Quit}
}

// FullHelp is the expanded panel shown with "?".
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.MuteSelf, k.MutePeer},
		{k.Down, k.Up},
		{k.Refresh, k.Help, k.Quit},
	}
}

// NewHelp builds a help component sized for the current terminal.
func NewHelp() help.Model {
	h := help.New()
	// The full panel is a key reference, not decoration, so it is always shown in
	// full when toggled rather than collapsed into "more".
	h.ShowAll = true
	return h
}
