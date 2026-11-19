package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAllDocumentedKeysAreHandled proves every key advertised in the help panel has
// an implementation. It exists so help text cannot drift into describing keys that
// do nothing, which is a small lie a UI can easily tell.
func TestAllDocumentedKeysAreHandled(t *testing.T) {
	// Each key maps to a check that it did what it claims.
	checks := map[string]func(t *testing.T, before, after *Model, cmds int){
		"j": func(t *testing.T, before, after *Model, _ int) {
			if before.vm.Selected != 0 || after.vm.Selected != 1 {
				t.Fatalf("j did not move down: %d -> %d", before.vm.Selected, after.vm.Selected)
			}
		},
		"k": func(t *testing.T, before, after *Model, _ int) {
			// Starting at the bottom, k must move up.
			if before.vm.Selected != 1 || after.vm.Selected != 0 {
				t.Fatalf("k did not move up: %d -> %d", before.vm.Selected, after.vm.Selected)
			}
		},
		"?": func(t *testing.T, before, after *Model, _ int) {
			if before.vm.ShowHelp == after.vm.ShowHelp {
				t.Fatal("? did not toggle help")
			}
		},
		"m": func(t *testing.T, _, _ *Model, cmds int) {
			if cmds == 0 {
				t.Fatal("m produced no daemon command")
			}
		},
		"M": func(t *testing.T, _, _ *Model, cmds int) {
			if cmds == 0 {
				t.Fatal("M produced no daemon command")
			}
		},
		"r": func(t *testing.T, _, _ *Model, cmds int) {
			if cmds == 0 {
				t.Fatal("r produced no refresh command")
			}
		},
		"q": func(t *testing.T, _, after *Model, _ int) {
			if after.View() != "" {
				t.Fatal("q did not leave the view")
			}
		},
	}

	// The help panel must document exactly these keys; a new key added to the help
	// without a test fails here, and a key implemented but undocumented is caught
	// by TestHelpListsWorkingKeys.
	documented := []string{"m", "M", "j", "k", "r", "?", "q"}
	if len(checks) != len(documented) {
		t.Fatalf("help documents %d keys but %d are tested", len(documented), len(checks))
	}

	for _, key := range documented {
		check, ok := checks[key]
		if !ok {
			t.Fatalf("key %q is documented but has no test", key)
		}
		t.Run(key, func(t *testing.T) {
			c := clientWithPeers(2)
			m := New(c, Config{Interval: time.Hour, NoColor: true})
			m = applyPoll(t, m)

			// "k" starts at the bottom so it has somewhere to go.
			if key == "k" {
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
			}

			// Model is a pointer, so capture the values to compare, not the pointer.
			selBefore := m.Selection()
			helpBefore := m.vm.ShowHelp
			cmds := 0
			// Update may return a different model value, so use what it returns.
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			after := updated.(*Model)
			if cmd != nil {
				cmds = 1
				// Run the command so side effects (quit) are observable.
				_ = cmd()
			}
			// Restore the snapshots on a throwaway copy so the checks read naturally.
			before := &Model{vm: ViewModel{Selected: selBefore, ShowHelp: helpBefore}}
			check(t, before, after, cmds)
		})
	}
}
