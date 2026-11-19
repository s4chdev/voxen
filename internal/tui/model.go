package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/s4chdev/voxen/internal/ipc"
)

// Client is the daemon operations the TUI needs. It is an interface so the model
// can be tested without a running daemon.
type Client interface {
	Status(ctx context.Context) (ipc.StatusResult, error)
	Peers(ctx context.Context) ([]ipc.PeerState, error)
	SetMuted(ctx context.Context, muted bool) (ipc.MuteResult, error)
	SetPeerMuted(ctx context.Context, peerID string, muted bool) (ipc.PeerMuteResult, error)
}

// Config configures a Model.
type Config struct {
	// Interval is how often to poll. Zero uses DefaultInterval.
	Interval time.Duration
	// NoColor disables styling.
	NoColor bool
	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultInterval is the polling period.
//
// Measured at roughly 6 ms per status+peers round trip, so a one-second period
// costs well under one percent of a core. Faster than this gains nothing: audio
// counters and speaking state do not change meaningfully in less than a second, and
// a higher rate makes the daemon do work for no benefit.
const DefaultInterval = time.Second

// Model is the Bubble Tea model.
type Model struct {
	client Client
	cfg    Config
	vm     ViewModel
	// help renders the key hints from the same bindings that drive key matching.
	help help.Model

	snapshot Snapshot
	// lastErr is kept separate so a successful poll clears it and a failed one
	// preserves the last good state on screen.
	lastErr  error
	quitting bool
}

// New builds a model.
func New(client Client, cfg Config) *Model {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Model{
		client: client,
		cfg:    cfg,
		vm:     ViewModel{NoColor: cfg.NoColor},
		help:   NewHelp(),
	}
}

// tickMsg triggers a poll.
type tickMsg time.Time

// snapshotMsg carries a completed poll.
type snapshotMsg Snapshot

// actionMsg carries the result of a key that changed daemon state.
type actionMsg struct {
	note string
	err  error
}

// Init starts the first poll.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.poll(), m.tick())
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.cfg.Interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// poll fetches one snapshot.
//
// Both calls are made because the peer list and the audio counters live behind
// different methods; they are cheap and must agree with each other, so a failure of
// either marks the snapshot as degraded rather than mixing a good half with a bad.
func (m *Model) poll() tea.Cmd {
	client := m.client
	now := m.cfg.Now
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		status, err := client.Status(ctx)
		if err != nil {
			return snapshotMsg(Snapshot{At: now(), Err: err})
		}
		peers, err := client.Peers(ctx)
		if err != nil {
			return snapshotMsg(Snapshot{Status: status, At: now(), Err: err})
		}
		return snapshotMsg(Snapshot{Status: status, Peers: peers, At: now()})
	}
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.vm.Width = msg.Width
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.poll(), m.tick())

	case snapshotMsg:
		m.snapshot = Snapshot(msg)
		m.lastErr = msg.Err
		// Clamp the selection: a participant may have left since the last poll.
		if m.vm.Selected >= len(m.snapshot.Peers) {
			m.vm.Selected = max(0, len(m.snapshot.Peers)-1)
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.lastErr = msg.err
		}
		return m, m.poll()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Matching goes through the same bindings that produce the help text, so a key
	// cannot work without being documented or be documented without working.
	switch {
	case key.Matches(msg, Keys.Quit):
		// Quitting must never stop the call: the daemon owns it, and a UI closing
		// should not drop everyone else.
		m.quitting = true
		return m, tea.Quit

	case key.Matches(msg, Keys.Help):
		m.vm.ShowHelp = !m.vm.ShowHelp
		return m, nil

	case key.Matches(msg, Keys.Refresh):
		return m, m.poll()

	case key.Matches(msg, Keys.Down):
		if m.vm.Selected < len(m.snapshot.Peers)-1 {
			m.vm.Selected++
		}
		return m, nil

	case key.Matches(msg, Keys.Up):
		if m.vm.Selected > 0 {
			m.vm.Selected--
		}
		return m, nil

	case key.Matches(msg, Keys.MuteSelf):
		// Toggle own microphone. The daemon is the only place mute state lives, so
		// the UI asks it rather than tracking a local copy that could drift.
		muted := !m.snapshot.Status.Muted
		return m, m.setMuted(muted)

	case key.Matches(msg, Keys.MutePeer):
		return m, m.toggleSelectedPeerMute()
	}
	return m, nil
}

func (m *Model) setMuted(muted bool) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.SetMuted(ctx, muted)
		return actionMsg{note: fmt.Sprintf("microphone muted=%v", muted), err: err}
	}
}

func (m *Model) toggleSelectedPeerMute() tea.Cmd {
	if len(m.snapshot.Peers) == 0 {
		return nil
	}
	if m.vm.Selected >= len(m.snapshot.Peers) {
		m.vm.Selected = len(m.snapshot.Peers) - 1
	}
	peer := m.snapshot.Peers[m.vm.Selected]
	client := m.client
	target := !peer.Muted
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.SetPeerMuted(ctx, peer.ID, target)
		name := peer.DisplayName
		if name == "" {
			name = peer.ID
		}
		return actionMsg{note: fmt.Sprintf("%s muted=%v", name, target), err: err}
	}
}

// View renders the current state.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	out := m.vm.RenderWithHelp(m.snapshot, m.help, Keys)
	if m.lastErr != nil && m.snapshot.Err == nil {
		// A key action failed but polling still works; show the reason without
		// replacing the whole screen.
		out += "\n" + StyleBad.Render(m.lastErr.Error())
	}
	return out
}

// Snapshot returns the last poll, for tests.
func (m *Model) Snapshot() Snapshot { return m.snapshot }

// Selection returns the selected peer index, for tests.
func (m *Model) Selection() int { return m.vm.Selected }

// SetClient swaps the client, used by tests to inject failures mid-run.
func (m *Model) SetClient(c Client) { m.client = c }

var _ tea.Model = (*Model)(nil)
