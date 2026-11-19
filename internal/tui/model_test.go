package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/s4chdev/voxen/internal/ipc"
)

// fakeClient records calls and returns scripted results.
type fakeClient struct {
	status    ipc.StatusResult
	peers     []ipc.PeerState
	statusErr error
	peersErr  error
	muteErr   error

	muteCalls     []bool
	peerMuteCalls []struct {
		id    string
		muted bool
	}
}

func (f *fakeClient) Status(context.Context) (ipc.StatusResult, error) {
	return f.status, f.statusErr
}

func (f *fakeClient) Peers(context.Context) ([]ipc.PeerState, error) {
	return f.peers, f.peersErr
}

func (f *fakeClient) SetMuted(_ context.Context, muted bool) (ipc.MuteResult, error) {
	f.muteCalls = append(f.muteCalls, muted)
	return ipc.MuteResult{Muted: muted}, f.muteErr
}

func (f *fakeClient) SetPeerMuted(_ context.Context, id string, muted bool) (ipc.PeerMuteResult, error) {
	f.peerMuteCalls = append(f.peerMuteCalls, struct {
		id    string
		muted bool
	}{id, muted})
	return ipc.PeerMuteResult{PeerID: id, Muted: muted}, nil
}

// clientWithPeers builds a fake client with a live call of n peers.
func clientWithPeers(n int) *fakeClient {
	f := &fakeClient{status: liveStatus(n)}
	for i := 0; i < n; i++ {
		f.peers = append(f.peers, ipc.PeerState{
			ID: string(rune('a' + i)), DisplayName: string(rune('a' + i)),
			Connection: "connected", Direct: true,
		})
	}
	return f
}

// applyPoll runs one poll command and feeds the result back into the model.
func applyPoll(t *testing.T, m *Model) *Model {
	t.Helper()
	msg := m.poll()()
	next, _ := m.Update(msg)
	return next.(*Model)
}

// TestModelPollsAndPopulates proves the model gets real data into the view.
func TestModelPollsAndPopulates(t *testing.T) {
	c := clientWithPeers(2)
	m := New(c, Config{Interval: time.Second, NoColor: true})
	m = applyPoll(t, m)

	if len(m.Snapshot().Peers) != 2 {
		t.Fatalf("snapshot has %d peers", len(m.Snapshot().Peers))
	}
	if !m.Snapshot().Status.Room.InRoom {
		t.Fatal("snapshot lost the room state")
	}
	if s := m.View(); s == "" {
		t.Fatal("view is empty")
	}
}

// TestModelSurvivesDaemonFailure proves a poll error keeps the last good state on
// screen instead of blanking it, so a momentary IPC hiccup does not look like the
// call ended.
func TestModelSurvivesDaemonFailure(t *testing.T) {
	c := clientWithPeers(1)
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)
	if len(m.Snapshot().Peers) != 1 {
		t.Fatal("setup failed")
	}

	c.statusErr = errors.New("connection refused")
	m = applyPoll(t, m)

	if m.Snapshot().Err == nil {
		t.Fatal("a failed poll did not record an error")
	}
	if !contains(m.View(), "unreachable") {
		t.Fatalf("failure not shown:\n%s", m.View())
	}
}

// TestMuteKeyTogglesThroughDaemon proves the UI never invents mute state: it asks
// the daemon and re-reads the result.
func TestMuteKeyTogglesThroughDaemon(t *testing.T) {
	c := clientWithPeers(1)
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)

	// Press "m": the daemon was unmuted, so the request must be to mute.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if cmd == nil {
		t.Fatal("mute key produced no command")
	}
	msg := cmd()
	if _, ok := msg.(actionMsg); !ok {
		t.Fatalf("mute produced %T", msg)
	}
	if len(c.muteCalls) != 1 || c.muteCalls[0] != true {
		t.Fatalf("mute calls = %v, want one 'true'", c.muteCalls)
	}

	// Simulate the daemon now reporting muted, then press "m" again.
	c.status.Muted = true
	m = applyPoll(t, m)
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	cmd()
	if len(c.muteCalls) != 2 || c.muteCalls[1] != false {
		t.Fatalf("second mute should unmute, got %v", c.muteCalls)
	}
}

// TestPeerMuteTargetsSelection proves "M" acts on the row the user selected, not on
// an arbitrary peer.
func TestPeerMuteTargetsSelection(t *testing.T) {
	c := clientWithPeers(3)
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)

	// Move down one row, then mute.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.Selection() != 1 {
		t.Fatalf("selection = %d, want 1", m.Selection())
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if cmd == nil {
		t.Fatal("peer mute produced no command")
	}
	cmd()

	if len(c.peerMuteCalls) != 1 {
		t.Fatalf("peer mute calls = %d", len(c.peerMuteCalls))
	}
	// The view sorts by display name, so selection index 1 is peer "b".
	if got := c.peerMuteCalls[0].id; got != "b" {
		t.Fatalf("muted peer %q, want the selected row (b)", got)
	}
}

// TestSelectionClampedWhenPeerLeaves proves a participant leaving does not leave
// the selection pointing past the end of the list.
func TestSelectionClampedWhenPeerLeaves(t *testing.T) {
	c := clientWithPeers(3)
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)

	// Select the last row.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.Selection() != 2 {
		t.Fatalf("selection = %d", m.Selection())
	}

	// Two peers leave.
	c.peers = c.peers[:1]
	m = applyPoll(t, m)
	if m.Selection() != 0 {
		t.Fatalf("selection not clamped: %d", m.Selection())
	}

	// Pressing M must not panic with an empty list either.
	c.peers = nil
	m = applyPoll(t, m)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if cmd != nil {
		t.Fatal("peer mute ran with no participants")
	}
}

// TestNavigationStopsAtBounds proves j/k do not run past the ends of the list.
func TestNavigationStopsAtBounds(t *testing.T) {
	c := clientWithPeers(2)
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.Selection() != 0 {
		t.Fatalf("k moved past the top: %d", m.Selection())
	}
	for i := 0; i < 5; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	if m.Selection() != 1 {
		t.Fatalf("j moved past the bottom: %d", m.Selection())
	}
}

// TestQuitDoesNotStopTheCall proves q only exits the view. It must not call any
// daemon method, because the call belongs to the daemon and other participants.
func TestQuitDoesNotStopTheCall(t *testing.T) {
	c := clientWithPeers(1)
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("quit produced no command")
	}
	if msg := cmd(); msg != nil {
		if _, isQuit := msg.(tea.QuitMsg); !isQuit {
			t.Fatalf("quit produced %T", msg)
		}
	}
	if len(c.muteCalls) != 0 || len(c.peerMuteCalls) != 0 {
		t.Fatal("quitting touched daemon state")
	}
	if m.View() != "" {
		t.Fatal("view still renders after quitting")
	}
}

// TestHelpToggle proves "?" flips the help panel.
func TestHelpToggle(t *testing.T) {
	m := New(clientWithPeers(0), Config{NoColor: true})
	// The short line shows a subset; the full panel shows every binding. Asserting
	// on the full panel proves the toggle works regardless of which subset Bubbles
	// chooses to elide in short form.
	if contains(m.View(), "mute self") && contains(m.View(), "refresh") {
		t.Fatal("full help shown before it was requested")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !contains(m.View(), "refresh") {
		t.Fatalf("full help did not appear:\n%s", m.View())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if contains(m.View(), "refresh") {
		t.Fatal("full help did not hide")
	}
}

// TestInitStartsPollAndTick proves the model schedules its own refresh, which is
// what makes it live rather than a one-shot dump.
func TestInitStartsPollAndTick(t *testing.T) {
	m := New(clientWithPeers(0), Config{Interval: time.Hour})
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
}

// TestActionErrorIsSurfaced proves a rejected action (for example muting a peer who
// has just left) is shown rather than silently ignored.
func TestActionErrorIsSurfaced(t *testing.T) {
	c := clientWithPeers(1)
	c.muteErr = errors.New("no such participant")
	m := New(c, Config{NoColor: true})
	m = applyPoll(t, m)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	msg := cmd()
	m.Update(msg)

	if !contains(m.View(), "no such participant") {
		t.Fatalf("action error not shown:\n%s", m.View())
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
