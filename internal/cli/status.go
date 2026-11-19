package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/ipc"
)

func (a *App) newStatusCommand() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon, room and audio state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch {
				return a.watchStatus(cmd.Context())
			}
			var res ipc.StatusResult
			if err := a.call(ipc.MethodStatus, nil, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			printStatus(a, res)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh every second until interrupted")
	return cmd
}

func printStatus(a *App, s ipc.StatusResult) {
	state := "stopped"
	if s.PID > 0 {
		state = fmt.Sprintf("running (pid %d)", s.PID)
	}
	room := "not in a room"
	if s.Room.InRoom {
		room = s.Room.RoomID
		if s.Room.Role != "" {
			room += " (" + s.Room.Role + ")"
		}
	}
	muted := "off"
	if s.Muted {
		muted = "on"
	}

	t := &table{}
	t.add("daemon", state)
	t.add("version", s.Version)
	t.add("platform", s.Platform)
	t.add("uptime since", s.StartedAt)
	t.add("endpoint", s.SocketPath)
	t.add("config", s.ConfigPath)
	if s.DisplayName != "" {
		t.add("identity", fmt.Sprintf("%s  (%s)", s.DisplayName, s.Fingerprint))
	} else {
		t.add("identity", s.Fingerprint)
	}
	t.add("signaling", fmt.Sprintf("%s [%s]", s.Signaling, s.SignalingState))
	t.add("room", room)
	t.add("peers", fmt.Sprint(s.Peers))
	if s.Room.InRoom {
		t.add("room role", s.Room.Role)
	}
	t.add("microphone", "muted"+" ("+muted+")")
	t.add("audio", s.AudioState)
	// Live call counters: the honest answer to "is sound actually moving?".
	if s.Audio.Running {
		mic := "live"
		if s.Audio.Muted {
			mic = "muted"
		}
		t.add("call audio", fmt.Sprintf("%s, sent %d frames, received %d", mic, s.Audio.FramesSent, s.Audio.FramesReceived))
		if s.Audio.InputDropped > 0 || s.Audio.OutputXRuns > 0 {
			t.add("audio health", fmt.Sprintf("input dropped %d, output xruns %d", s.Audio.InputDropped, s.Audio.OutputXRuns))
		}
	}
	// Capability matrix: never leave the user guessing what this build can do.
	caps := []string{}
	if s.Capabilities.Signaling {
		caps = append(caps, "signaling")
	}
	if s.Capabilities.Media {
		caps = append(caps, "media")
	}
	if s.Capabilities.Audio {
		caps = append(caps, "audio")
	}
	if s.Capabilities.Group {
		caps = append(caps, "group calls")
	}
	capStr := "none"
	if len(caps) > 0 {
		capStr = strings.Join(caps, ", ")
	}
	t.add("available", capStr)
	t.add("build phase", fmt.Sprintf("%d (%s)", s.Phase, phaseName(s.Phase)))
	a.render(t)
}

func phaseName(p int) string {
	switch p {
	case 1:
		return "foundation"
	case 2:
		return "audio"
	case 3:
		return "p2p media"
	case 4:
		return "group calls"
	case 5:
		return "security hardening"
	case 6:
		return "group calls"
	case 7:
		return "daemon and CLI"
	default:
		return "production"
	}
}

func (a *App) watchStatus(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var res ipc.StatusResult
		if err := a.call(ipc.MethodStatus, nil, &res); err != nil {
			fmt.Fprintln(a.stderr, "voxen:", err)
		} else if a.jsonOut {
			_ = a.printJSON(res)
		} else {
			fmt.Fprint(a.stdout, "\033[H\033[2J")
			printStatus(a, res)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *App) newPeersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List room participants and their trust state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.PeersResult
			if err := a.call(ipc.MethodPeersList, nil, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			if len(res.Peers) == 0 {
				fmt.Fprintln(a.stdout, "No peers. voxen is not in a room, or nobody else has joined yet.")
				return nil
			}
			t := &table{headers: []string{"PEER", "NAME", "FINGERPRINT", "TRUST", "CONNECTION", "PATH", "RTT", "SPEAKING", "MUTED"}}
			for _, p := range res.Peers {
				trust := "unverified"
				if p.Verified {
					trust = "verified"
				}
				path := "-"
				switch {
				case p.Connection == "failed":
					path = "failed"
				case p.Direct:
					path = "direct"
				case p.Connection == "connected":
					// Connected without a reported candidate pair is unexpected:
					// voxen never relays, so this must resolve to direct or fail.
					path = "unknown"
				}
				rtt := "-"
				if p.RTTMillis > 0 {
					rtt = fmt.Sprintf("%dms", p.RTTMillis)
				}
				name := p.DisplayName
				if name == "" {
					name = "-"
				}
				// "speaking" is observed locally from audio levels, and only once
				// the connection is authenticated: a peer with no media path cannot
				// be shown as talking.
				speaking := ""
				if p.Speaking && p.Connection == "connected" {
					speaking = "yes"
				}
				// "muted" here means muted by you, locally.
				muted := ""
				if p.Muted {
					muted = "local"
				}
				t.add(p.ID, name, p.Fingerprint, trust, p.Connection, path, rtt, speaking, muted)
			}
			a.render(t)

			// Explain rejections rather than leaving a bare "failed".
			for _, p := range res.Peers {
				if p.LastError != "" {
					fmt.Fprintf(a.stdout, "\n%s: %s\n", p.ID, p.LastError)
				}
			}
			return nil
		},
	}
	return cmd
}

func (a *App) newDevicesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List microphones and speakers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.DevicesResult
			if err := a.call(ipc.MethodDevicesList, nil, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			if res.Error != "" {
				return exitWith(ExitUnsupported, fmt.Errorf("audio devices unavailable: %s", res.Error))
			}
			printDevices := func(label string, devices []ipc.DeviceInfo) {
				fmt.Fprintf(a.stdout, "%s:\n", label)
				if len(devices) == 0 {
					fmt.Fprintln(a.stdout, "  (none)")
					return
				}
				for _, d := range devices {
					marker := " "
					if d.IsDefault {
						marker = "*"
					}
					// The device name is its identifier in this backend, so print
					// it once. "voxen config" records the name to select it.
					fmt.Fprintf(a.stdout, " %s %s\n", marker, d.Name)
				}
			}
			printDevices("Input devices", res.Inputs)
			fmt.Fprintln(a.stdout)
			printDevices("Output devices", res.Outputs)
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Asterisk marks the system default. To pin a device, set")
			fmt.Fprintln(a.stdout, "audio.input_device / audio.output_device in the config.")
			return nil
		},
	}
	return cmd
}

func (a *App) newConfigCommand() *cobra.Command {
	var pathOnly bool
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show the effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.ConfigGetResult
			if err := a.call(ipc.MethodConfigGet, nil, &res); err != nil {
				// Without a daemon, render the file directly.
				if err := a.loadLocalConfig(); err != nil {
					return err
				}
				raw, encErr := a.cfg.Marshal()
				if encErr != nil {
					return encErr
				}
				res = ipc.ConfigGetResult{Path: a.paths.ConfigFile, YAML: string(raw), Source: "file"}
			}
			if pathOnly {
				fmt.Fprintln(a.stdout, res.Path)
				return nil
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprint(a.stdout, res.YAML)
			return nil
		},
	}
	cmd.Flags().BoolVar(&pathOnly, "path", false, "print only the configuration file path")
	cmd.AddCommand(&cobra.Command{
		Use:   "reload",
		Short: "Reload the configuration without restarting",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res map[string]any
			if err := a.call(ipc.MethodConfigReload, nil, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintln(a.stdout, "Configuration reloaded.")
			if v, ok := res["restart_required"].(bool); ok && v {
				fmt.Fprintln(a.stdout, "Some changes take effect only after 'voxen restart'.")
			}
			return nil
		},
	})
	return cmd
}

func (a *App) newLogsCommand() *cobra.Command {
	var lines int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show daemon logs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.LogsResult
			if err := a.call(ipc.MethodLogsTail, ipc.LogsParams{Lines: lines}, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			if len(res.Lines) == 0 {
				fmt.Fprintf(a.stdout, "No log lines at %s.\n", res.Path)
				if res.Truncated {
					fmt.Fprintln(a.stdout, "(older lines were dropped)")
				}
				return nil
			}
			for _, line := range res.Lines {
				fmt.Fprintln(a.stdout, line)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of lines to show (max 5000)")
	// Following logs is deliberately not a streaming socket yet: phase 7 adds a
	// tail event stream. Saying so is better than a flag that silently does
	// nothing.
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow new lines (not available in this build)")
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if follow {
			return exitWith(ExitUnsupported, errors.New("--follow is not available in this build"))
		}
		return nil
	}
	return cmd
}

func (a *App) newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run local diagnostics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.DoctorResult
			if err := a.call(ipc.MethodDoctor, nil, &res); err != nil {
				return err
			}
			if a.jsonOut {
				if err := a.printJSON(res); err != nil {
					return err
				}
			} else {
				t := &table{headers: []string{"CHECK", "STATUS", "DETAIL"}}
				for _, c := range res.Checks {
					t.add(c.Name, strings.ToUpper(c.Status), c.Message)
				}
				a.render(t)
				hints := []string{}
				for _, c := range res.Checks {
					if c.Hint != "" && c.Status != "pass" {
						hints = append(hints, fmt.Sprintf("  %s: %s", c.Name, c.Hint))
					}
				}
				if len(hints) > 0 {
					fmt.Fprintln(a.stdout, "\nSuggestions:")
					for _, h := range hints {
						fmt.Fprintln(a.stdout, h)
					}
				}
			}
			if !res.OK {
				return exitWith(ExitError, errors.New("one or more checks failed"))
			}
			return nil
		},
	}
	return cmd
}

func (a *App) newMuteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mute",
		Short: "Mute the microphone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			muted := true
			return a.setMute(&muted)
		},
	}
}

func (a *App) newUnmuteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unmute",
		Short: "Unmute the microphone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			muted := false
			return a.setMute(&muted)
		},
	}
}

func (a *App) setMute(muted *bool) error {
	var res ipc.MuteResult
	if err := a.call(ipc.MethodMuteSet, ipc.MuteParams{Muted: muted}, &res); err != nil {
		return err
	}
	if a.jsonOut {
		return a.printJSON(res)
	}
	if res.Muted {
		fmt.Fprintln(a.stdout, "Microphone muted.")
	} else {
		fmt.Fprintln(a.stdout, "Microphone live.")
	}
	if !res.Active {
		fmt.Fprintln(a.stdout, "Note: no active call; the setting applies to the next call.")
	}
	return nil
}
