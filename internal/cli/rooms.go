package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
)

func (a *App) newCreateCommand() *cobra.Command {
	var (
		name       string
		private    bool
		maxMembers int
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a room and invite peers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.RoomResult
			err := a.call(ipc.MethodRoomCreate, ipc.RoomCreateParams{
				Name: name, Private: private, MaxMembers: maxMembers,
			}, &res)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintf(a.stdout, "Room %s created.\n", res.Room.RoomID)
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Share this room ID with each participant. They must verify your")
			fmt.Fprintln(a.stdout, "fingerprint out of band, then run:")
			fmt.Fprintf(a.stdout, "\n  voxen join %s\n", res.Room.RoomID)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human readable room name")
	cmd.Flags().BoolVar(&private, "private", true, "only admit participants you explicitly approve")
	cmd.Flags().IntVar(&maxMembers, "max-members", 5, "maximum participants (mesh limit: 16)")
	return cmd
}

func (a *App) newJoinCommand() *cobra.Command {
	var invite string
	cmd := &cobra.Command{
		Use:   "join <room-id>",
		Short: "Join an existing room",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res ipc.RoomResult
			if err := a.call(ipc.MethodRoomJoin, ipc.RoomJoinParams{RoomID: args[0], Invite: invite}, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintf(a.stdout, "Joined room %s.\n", res.Room.RoomID)
			fmt.Fprintln(a.stdout, "Connecting directly to each participant; no relay server is used.")
			fmt.Fprintln(a.stdout, "Run 'voxen peers' to watch connections progress.")
			return nil
		},
	}
	cmd.Flags().StringVar(&invite, "invite", "", "signed invitation token from the room creator")
	return cmd
}

func (a *App) newLeaveCommand() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave the current room",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.RoomResult
			if err := a.call(ipc.MethodRoomLeave, ipc.RoomLeaveParams{Reason: reason}, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintln(a.stdout, "Left the room.")
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional reason sent to participants")
	return cmd
}

func (a *App) newVerifyCommand() *cobra.Command {
	var (
		name string
		note string
	)
	cmd := &cobra.Command{
		Use:   "verify <fingerprint>",
		Short: "Mark a peer's fingerprint as verified",
		Long: `Mark a peer's identity as verified after comparing fingerprints out of band.

Only verified peers are sent audio. Compare the full fingerprint over a separate
channel (in person, a signed message, a previous authenticated call): a peer that
the signaling server introduced is never trusted automatically.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fp, err := identity.ParseFingerprint(args[0])
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			// The daemon owns the trust store; require it to be running so a
			// verification is recorded once, in one place.
			var res ipc.TrustRecord
			params := ipc.VerifyPeerParams{Fingerprint: fp.String(), DisplayName: name, Note: note}
			if err := a.call(ipc.MethodIdentityVerify, params, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintf(a.stdout, "Verified %s\n", res.Fingerprint)
			if res.DisplayName != "" {
				fmt.Fprintf(a.stdout, "  name: %s\n", res.DisplayName)
			}
			if res.Pinned {
				fmt.Fprintln(a.stdout)
				fmt.Fprintln(a.stdout, "This is a fingerprint pin. Until this peer is seen, voxen knows the")
				fmt.Fprintln(a.stdout, "fingerprint you trust but not the key behind it. The key presented")
				fmt.Fprintln(a.stdout, "in the first session must hash to this value, or the call is refused.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "display name for this peer")
	cmd.Flags().StringVar(&note, "note", "", "why and where you verified this fingerprint")
	return cmd
}

func (a *App) newTrustCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "List peer trust records",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.TrustListResult
			if err := a.call(ipc.MethodTrustList, nil, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			if len(res.Entries) == 0 {
				fmt.Fprintln(a.stdout, "No peers in the trust store.")
				fmt.Fprintln(a.stdout, "Compare a peer's fingerprint out of band, then record it with:")
				fmt.Fprintln(a.stdout, "  voxen verify <fingerprint> --name <peer>")
				return nil
			}
			t := &table{headers: []string{"FINGERPRINT", "NAME", "LEVEL", "BOUND", "LAST SEEN", "NOTE"}}
			for _, e := range res.Entries {
				name := e.DisplayName
				if name == "" {
					name = "-"
				}
				bound := "key"
				switch {
				case e.Pinned:
					bound = "fingerprint only"
				case e.PublicKey == "":
					bound = "-"
				}
				t.add(e.Fingerprint, name, e.Level, bound, e.LastSeen, e.Note)
			}
			a.render(t)
			return nil
		},
	}
	return cmd
}

func (a *App) newRevokeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <fingerprint>",
		Short: "Revoke trust for a peer fingerprint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fp, err := identity.ParseFingerprint(args[0])
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			var res map[string]string
			if err := a.call(ipc.MethodIdentityRevoke, ipc.RevokePeerParams{Fingerprint: fp.String()}, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintf(a.stdout, "Revoked %s. This peer can no longer be connected.\n", fp.String())
			return nil
		},
	}
	return cmd
}

// newPeerMuteCommand mutes or unmutes one participant locally.
func (a *App) newPeerMuteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer-mute <peer-id>",
		Short: "Silence one participant locally",
		Long: `Silence one participant on this machine.

This is a local playback control: the participant keeps sending audio, and voxen
keeps receiving it, but none of it reaches your speaker. Use "voxen mute" to stop
your own microphone instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			muted := true
			return a.setPeerMute(args[0], &muted)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "off <peer-id>",
		Short: "Restore a locally muted participant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			muted := false
			return a.setPeerMute(args[0], &muted)
		},
	})
	return cmd
}

func (a *App) setPeerMute(peerID string, muted *bool) error {
	var res ipc.PeerMuteResult
	if err := a.call(ipc.MethodPeerMuteSet, ipc.PeerMuteParams{PeerID: peerID, Muted: muted}, &res); err != nil {
		return err
	}
	if a.jsonOut {
		return a.printJSON(res)
	}
	if res.Muted {
		fmt.Fprintf(a.stdout, "Silenced %s locally.\n", res.PeerID)
	} else {
		fmt.Fprintf(a.stdout, "Restored %s.\n", res.PeerID)
	}
	return nil
}
