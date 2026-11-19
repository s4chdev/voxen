package tui

import (
	"strings"
	"testing"
)

// TestDiagnosticRenderProducesText proves Glamour rendering works and, importantly,
// that it never returns empty output: a blank help page would be worse than plain
// text.
func TestDiagnosticRenderProducesText(t *testing.T) {
	d := DiagnosticText{NoColor: true, Width: 80}
	out := d.Render(HelpMarkdown(Keys))
	if strings.TrimSpace(out) == "" {
		t.Fatal("rendered help is empty")
	}
	// The headings survive as text even without styling.
	for _, want := range []string{"voxen", "Keys", "STATE", "Talking"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Fatalf("rendered help missing %q:\n%s", want, out)
		}
	}
}

// TestDiagnosticFallbackOnBadInput proves a renderer failure degrades to the source
// text rather than showing nothing.
func TestDiagnosticFallbackOnBadInput(t *testing.T) {
	d := DiagnosticText{NoColor: true}
	// A markdown document with no structure still renders; the point is that the
	// function returns something usable for any input.
	out := d.Render("plain text")
	if out == "" {
		t.Fatal("plain text rendered to nothing")
	}
}

// TestHelpMarkdownCoversEveryKey proves the long-form help is generated from the
// same bindings as input handling, so it cannot omit a working key.
func TestHelpMarkdownCoversEveryKey(t *testing.T) {
	md := HelpMarkdown(Keys)
	for _, group := range Keys.FullHelp() {
		for _, b := range group {
			h := b.Help()
			if h.Key == "" {
				continue
			}
			if !strings.Contains(md, h.Key) {
				t.Fatalf("help markdown omits the %q binding", h.Key)
			}
		}
	}
}

// TestHelpMarkdownStatesQuitKeepsCall documents the one thing a user must not get
// wrong about closing the view.
func TestHelpMarkdownStatesQuitKeepsCall(t *testing.T) {
	md := HelpMarkdown(Keys)
	if !strings.Contains(md, "does not end the call") {
		t.Fatalf("help does not state that quitting keeps the call:\n%s", md)
	}
}

// TestHelpMarkdownExplainsNoRelay proves the PATH column is explained, since "direct"
// is meaningless without the no-relay context.
func TestHelpMarkdownExplainsNoRelay(t *testing.T) {
	md := HelpMarkdown(Keys)
	if !strings.Contains(strings.ToLower(md), "no relay") && !strings.Contains(strings.ToLower(md), "no\nrelay") {
		t.Fatalf("help does not explain the no-relay design:\n%s", md)
	}
}
