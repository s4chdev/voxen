package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// DiagnosticText renders a diagnostic report as markdown for the TUI.
//
// Glamour is used here rather than for the call view because a diagnostic report is
// prose: headings, lists and code spans render meaningfully in markdown, while a
// live call table does not. Keeping the renderer to one place avoids pulling a
// markdown engine into the hot path that redraws every second.
type DiagnosticText struct {
	// Width bounds the rendered output. Zero lets Glamour choose.
	Width int
	// NoColor disables styling, for monochrome terminals and tests.
	NoColor bool
}

// Render converts markdown into styled terminal text, falling back to the raw
// markdown if rendering fails.
//
// A renderer error must never blank the screen: showing the source text is worse
// than styled text but far better than nothing.
func (d DiagnosticText) Render(markdown string) string {
	opts := []glamour.TermRendererOption{
		glamour.WithWordWrap(d.Width),
	}
	if d.NoColor {
		opts = append(opts, glamour.WithStandardStyle("notty"))
	} else {
		opts = append(opts, glamour.WithStandardStyle("dark"))
	}

	r, err := glamour.NewTermRenderer(opts...)
	if err != nil {
		return markdown
	}
	out, err := r.Render(markdown)
	if err != nil {
		return markdown
	}
	// Glamour pads the output with blank lines at both ends, which would push the
	// footer off the screen in a tight terminal.
	return strings.Trim(out, "\n")
}

// HelpMarkdown is the long-form help shown by "voxen tui --help-page".
//
// It lives in the TUI package so the rendered text and the key bindings stay next
// to each other; the CLI supplies it with actual key names interpolated from the
// same KeyMap that drives input.
func HelpMarkdown(keys KeyMap) string {
	var b strings.Builder
	b.WriteString("# voxen\n\n")
	b.WriteString("A peer-to-peer, end-to-end encrypted voice call in your terminal.\n\n")
	b.WriteString("## Keys\n\n")
	// Both j and k are bound, but they are one concept to a reader. Emitting each
	// distinct help label once keeps the reference from repeating a line.
	seen := make(map[string]bool)
	for _, group := range keys.FullHelp() {
		for _, binding := range group {
			if !binding.Enabled() {
				continue
			}
			h := binding.Help()
			label := h.Key + "|" + h.Desc
			if seen[label] {
				continue
			}
			seen[label] = true
			b.WriteString("- `" + h.Key + "` — " + h.Desc + "\n")
		}
	}
	b.WriteString("\n## What the columns mean\n\n")
	// Bold inside a list item is not rendered by Glamour's list handler, so the
	// column names are emphasised with backticks instead, which render reliably.
	b.WriteString("- `STATE` — connected means the media path is up and authenticated; ")
	b.WriteString("failed always carries a reason, shown in the peer listing.\n")
	b.WriteString("- `PATH` — direct means ICE found a peer-to-peer route. voxen has no ")
	b.WriteString("relay, so a call that cannot go direct is reported as failed rather than ")
	b.WriteString("silently tunnelled.\n")
	b.WriteString("- `RTT` — round-trip time on the selected candidate pair.\n")
	b.WriteString("- `LOSS` — packet loss on the inbound audio stream.\n")
	b.WriteString("- `TALKING` — observed locally from audio levels, not reported by the peer.\n")
	b.WriteString("\n## Closing the view\n\n")
	b.WriteString("Quitting the view does not end the call. The daemon owns it, so other ")
	b.WriteString("participants are unaffected. Use `voxen leave` to leave the room.\n")
	return b.String()
}
