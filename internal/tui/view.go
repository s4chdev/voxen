// Package tui renders the live call view.
//
// The package separates two concerns deliberately:
//
//   - ViewModel holds the state the UI renders. It is pure data plus pure
//     functions, so it can be tested without a terminal, a daemon or a clock.
//   - Model is the Bubble Tea model, which owns the polling loop and forwards
//     messages into the ViewModel.
//
// Keeping the rendering logic out of the Bubble Tea model means a test can assert
// on what a user would see without standing up a PTY.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"

	"github.com/s4chdev/voxen/internal/ipc"
)

// Snapshot is one point-in-time view of the daemon, assembled from the two IPC
// calls the CLI already uses.
type Snapshot struct {
	Status ipc.StatusResult
	Peers  []ipc.PeerState
	// At is when the snapshot was taken, used for the "updated" line and to detect
	// a daemon that stopped answering.
	At time.Time
	// Err is set when the daemon could not be reached. The view keeps rendering the
	// last known state and shows the error, because a transient IPC failure should
	// not blank the screen.
	Err error
}

// ViewModel turns snapshots into the text a user sees.
//
// It is intentionally not a Bubble Tea type: the rendering rules are the part
// worth testing, and they do not depend on the framework.
type ViewModel struct {
	// Width is the terminal width in cells. Zero means "unbounded", which is what
	// tests use.
	Width int
	// Selected is the index into the peer list for keyboard navigation.
	Selected int
	// ShowHelp toggles the key reference.
	ShowHelp bool
	// NoColor disables styling, used by tests and by --no-color.
	NoColor bool
}

// Render produces the full screen using a fixed key hint, for tests and for callers
// without a help component.
func (v *ViewModel) Render(s Snapshot) string {
	return v.RenderWithHelp(s, NewHelp(), Keys)
}

// RenderWithHelp produces the full screen, taking the help component and key map so
// the bindings are rendered from one definition rather than duplicated as strings.
func (v *ViewModel) RenderWithHelp(s Snapshot, h help.Model, keys KeyMap) string {
	var b strings.Builder

	b.WriteString(v.header(s))
	b.WriteString("\n\n")
	b.WriteString(v.body(s))
	b.WriteString("\n")
	b.WriteString(v.footer(s))

	b.WriteString("\n")
	if v.ShowHelp {
		// FullHelp is a grid; rendering it as a block keeps the key reference
		// readable and separate from the live call data.
		b.WriteString(h.FullHelpView(keys.FullHelp()))
	} else {
		b.WriteString(h.ShortHelpView(keys.ShortHelp()))
	}
	return b.String()
}

// header renders the title line and the call's identity.
func (v *ViewModel) header(s Snapshot) string {
	title := StyleTitle.Render("voxen")
	if v.NoColor {
		title = "voxen"
	}

	// The headline state is the one thing a user looks at first: are we in a call
	// and how many people are on it.
	var state string
	switch {
	case s.Err != nil:
		state = badgeDanger.Render("daemon unreachable")
		if v.NoColor {
			state = "[daemon unreachable]"
		}
	case !s.Status.Room.InRoom:
		state = badgeIdle.Render("no call")
		if v.NoColor {
			state = "[no call]"
		}
	default:
		state = badgeLive.Render(fmt.Sprintf("in call · %d peer(s)", len(s.Peers)))
		if v.NoColor {
			state = fmt.Sprintf("[in call - %d peer(s)]", len(s.Peers))
		}
	}

	line := title + "  " + state
	if s.Status.Room.InRoom && s.Status.Room.RoomID != "" {
		line += subtleStyle(v.NoColor).Render("  " + s.Status.Room.RoomID)
	}
	return line
}

// body renders either the peer table or an explanation of why there is none.
func (v *ViewModel) body(s Snapshot) string {
	if !s.Status.Room.InRoom {
		return v.emptyState(s)
	}
	if len(s.Peers) == 0 {
		return subtleStyle(v.NoColor).Render("Waiting for participants to connect…") + "\n\n" +
			subtleStyle(v.NoColor).Render("Share the call code. Each participant must verify your fingerprint first.")
	}
	return v.peerTable(s)
}

// emptyState explains the current situation rather than showing a bare blank
// screen, and names the next action.
func (v *ViewModel) emptyState(s Snapshot) string {
	var b strings.Builder
	b.WriteString(subtleStyle(v.NoColor).Render("Not in a call."))
	b.WriteString("\n\n")
	for _, line := range []string{
		"voxen call          start a call and get a code to share",
		"voxen join <code>   join an existing call",
	} {
		b.WriteString("  " + line + "\n")
	}
	if s.Status.SignalingState != "" && s.Status.SignalingState != "connected" {
		b.WriteString("\n")
		b.WriteString(subtleStyle(v.NoColor).Render(
			"Signaling is " + s.Status.SignalingState + "; a call needs a reachable signaling server."))
	}
	return strings.TrimRight(b.String(), "\n")
}

// peerTable renders one row per participant.
//
// Column order follows what a person scanning a call wants: who, can I hear them,
// are they talking, and is the path healthy. The fingerprint is last and
// abbreviated because it is for verification, not for reading.
func (v *ViewModel) peerTable(s Snapshot) string {
	rows := make([][]string, 0, len(s.Peers))
	peers := s.Peers
	// Stable ordering so rows do not jump between refreshes. The controller already
	// sorts by ID; sorting here keeps the view correct regardless of the source.
	sort.SliceStable(peers, func(i, j int) bool {
		if peers[i].DisplayName != peers[j].DisplayName {
			return peers[i].DisplayName < peers[j].DisplayName
		}
		return peers[i].ID < peers[j].ID
	})

	header := []string{"", "NAME", "STATE", "PATH", "RTT", "LOSS", "TALKING"}
	for i, p := range peers {
		name := p.DisplayName
		if name == "" {
			name = p.ID
		}
		state := p.Connection
		if p.LastError != "" {
			state = "failed"
		}

		// path is only meaningful once connected: before that there is no selected
		// candidate pair, and "direct" would be a guess.
		path := "-"
		if p.Connection == "connected" {
			if p.Direct {
				path = "direct"
			} else {
				// voxen never relays, so a connected peer without a direct path is
				// an inconsistency worth surfacing rather than hiding.
				path = "unknown"
			}
		}

		rtt := "-"
		if p.RTTMillis > 0 {
			rtt = fmt.Sprintf("%dms", p.RTTMillis)
		}
		loss := "-"
		if p.PacketLoss > 0 {
			loss = fmt.Sprintf("%.1f%%", p.PacketLoss*100)
		}

		talking := ""
		if p.Speaking && p.Connection == "connected" {
			talking = speechMark(v.NoColor)
		}

		marker := " "
		if i == v.Selected {
			marker = ">"
		}
		rows = append(rows, []string{marker, name, state, path, rtt, loss, talking})
	}

	return renderTable(header, rows, v.NoColor)
}

// footer renders status counters and the key hints.
//
// The counters answer "is audio actually moving", which is the question a call UI
// must be able to answer factually.
func (v *ViewModel) footer(s Snapshot) string {
	var b strings.Builder

	// Audio line.
	if s.Status.Audio.Running {
		mic := "live"
		if s.Status.Audio.Muted {
			mic = "muted"
		}
		b.WriteString(fmt.Sprintf("audio  %s · sent %d · received %d",
			mic, s.Status.Audio.FramesSent, s.Status.Audio.FramesReceived))
		if s.Status.Audio.InputDropped > 0 || s.Status.Audio.OutputXRuns > 0 {
			b.WriteString(fmt.Sprintf(" · dropped %d · xruns %d",
				s.Status.Audio.InputDropped, s.Status.Audio.OutputXRuns))
		}
	} else {
		b.WriteString("audio  " + s.Status.AudioState)
	}
	b.WriteString("\n")

	// Signaling and daemon health, so a stalled call has a visible cause.
	sig := s.Status.SignalingState
	if sig == "" {
		sig = "unknown"
	}
	b.WriteString(fmt.Sprintf("signaling %s · daemon pid %d · updated %s",
		sig, s.Status.PID, s.At.Format("15:04:05")))

	return strings.TrimRight(b.String(), "\n")
}

// renderTable aligns columns with two spaces between them.
func renderTable(header []string, rows [][]string, noColor bool) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}

	var b strings.Builder
	writeRow := func(cols []string, render func(int, string) string) {
		for i, c := range cols {
			if i == len(cols)-1 {
				b.WriteString(render(i, c))
				b.WriteString("\n")
				continue
			}
			pad := ""
			if i < len(widths) {
				pad = strings.Repeat(" ", widths[i]-len(c))
			}
			b.WriteString(render(i, c) + pad + "  ")
		}
	}

	writeRow(header, func(_ int, s string) string {
		if noColor {
			return s
		}
		return StyleHeader.Render(s)
	})
	for _, row := range rows {
		writeRow(row, func(i int, s string) string {
			if noColor {
				return s
			}
			// State gets a colour so a failed peer stands out without reading text.
			if i == 2 {
				switch s {
				case "connected":
					return StyleOK.Render(s)
				case "failed":
					return StyleBad.Render(s)
				case "connecting":
					return StyleWarn.Render(s)
				}
			}
			if i == 6 && s != "" {
				return StyleSpeech.Render(s)
			}
			return s
		})
	}
	return strings.TrimRight(b.String(), "\n")
}

// speechMark is the indicator shown beside a talking participant.
//
// It is a plain ASCII character rather than an emoji or a block glyph: those
// render at inconsistent widths across terminals, which breaks table alignment.
func speechMark(noColor bool) string {
	if noColor {
		return "speaking"
	}
	return StyleSpeech.Render("◉ speaking")
}
