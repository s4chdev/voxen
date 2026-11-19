package tui

import "github.com/charmbracelet/lipgloss"

// Styles are defined once so the view code reads as layout rather than as colour
// constants.
//
// Colour is never the only signal: every state that has a colour also has distinct
// text ("connected", "failed"), so the view is readable with colour disabled, on a
// monochrome terminal, or by a screen reader.
var (
	StyleTitle  = lipgloss.NewStyle().Bold(true)
	StyleHeader = lipgloss.NewStyle().Bold(true).Faint(true)
	StyleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	StyleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	StyleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	StyleSpeech = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	StyleSubtle = lipgloss.NewStyle().Faint(true)

	badgeLive   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	badgeIdle   = lipgloss.NewStyle().Faint(true)
	badgeDanger = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
)

// subtleStyle returns the faint style, or a no-op when colour is disabled.
//
// lipgloss.Render on an empty style still emits escape sequences in some
// terminals, so the no-colour path returns the text unchanged instead.
func subtleStyle(noColor bool) interface{ Render(...string) string } {
	if noColor {
		return plainRenderer{}
	}
	return StyleSubtle
}

// plainRenderer passes text through unchanged.
type plainRenderer struct{}

func (plainRenderer) Render(s ...string) string {
	out := ""
	for _, v := range s {
		out += v
	}
	return out
}
