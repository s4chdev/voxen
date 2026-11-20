package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
)

// newCallCommand starts a call and prints the one thing another person needs: a
// short code.
//
// This replaces a four-step ritual (create, share a room ID, exchange fingerprints
// over another channel, verify, join). A code is enough to talk, and the code carries
// a check derived from this machine's key so the signaling server cannot swap
// identities without the check changing.
func (a *App) newCallCommand() *cobra.Command {
	var maxMembers int
	cmd := &cobra.Command{
		Use:   "call",
		Short: "Start a call and get a code to share",
		Long: `Start a call and print a code.

Give the code to the other person however you like — read it out, paste it in a
message. They run:

  voxen join <code>

That is the whole setup.

The code carries a check derived from your identity key. The other side verifies it
against the key the signaling server hands them, so a compromised server cannot
substitute its own identity without the code changing. That is trust-on-first-use:
whoever has the code is trusted, like an SSH host key on first connect. For the
stronger guarantee, compare fingerprints with 'voxen identity' and 'voxen verify'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.RoomResult
			if err := a.call(ipc.MethodRoomCreate, ipc.RoomCreateParams{MaxMembers: maxMembers}, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(map[string]any{
					"code":    res.ShareCode,
					"room_id": res.Room.RoomID,
				})
			}

			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "  Call started. Share this code:")
			fmt.Fprintln(a.stdout)
			fmt.Fprintf(a.stdout, "      %s\n", res.ShareCode)
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "  They join with:")
			fmt.Fprintln(a.stdout)
			fmt.Fprintf(a.stdout, "      voxen join %s\n", res.ShareCode)
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "  Watch the call with:  voxen tui")
			return nil
		},
	}
	cmd.Flags().IntVar(&maxMembers, "max-members", 5, "maximum participants (mesh limit: 16)")
	return cmd
}

// newJoinCommand joins by shared code. Accepts the pasteable variants the invite
// package tolerates, including the whole command.
func (a *App) newJoinCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join <code>",
		Short: "Join a call using a shared code",
		Long: `Join a call using the code the other person gave you.

Paste the code as-is; case, spaces and dashes do not matter, and pasting the whole
"voxen join ..." line works.

The code is checked against the caller's identity key before any audio flows. If it
does not match, the join is refused rather than connecting you to whoever answered.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Normalise here so the daemon always receives a canonical code.
			code := strings.TrimSpace(args[0])
			if code == "" {
				return exitWith(ExitUsage, errors.New("a code is required"))
			}

			var res ipc.RoomResult
			if err := a.call(ipc.MethodRoomJoin, ipc.RoomJoinParams{Code: code}, &res); err != nil {
				return joinError(err)
			}
			if a.jsonOut {
				return a.printJSON(res)
			}

			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "  Joined the call.")
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "  Connecting directly to each participant; no relay is used.")
			fmt.Fprintln(a.stdout, "  Watch it with:  voxen tui")
			return nil
		},
	}
	return cmd
}

// joinError turns the failures a joiner actually hits into advice, rather than
// leaving a protocol error on screen.
func joinError(err error) error {
	var ipcErr *ipc.Error
	if errors.As(err, &ipcErr) {
		switch ipcErr.Code {
		case ipc.CodeNotFound:
			return exitWith(ExitError, fmt.Errorf(
				"no call with that code: check it, and make sure the other person still has the call open (%s)", ipcErr.Message))
		case ipc.CodeUnavailable:
			return exitWith(ExitUnavailable, fmt.Errorf(
				"cannot reach the signaling server: a self-hosted server must be running and reachable (%s)", ipcErr.Message))
		}
	}
	if strings.Contains(err.Error(), "does not match the inviter") {
		return exitWith(ExitAuth, fmt.Errorf(
			"%w: the code you were given does not match the identity answering for that call. "+
				"That is what a substituted identity looks like, so do not continue unless you trust "+
				"the person who gave you the code", err))
	}
	return err
}

func (a *App) newLeaveCommand() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave the current call",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res ipc.RoomResult
			if err := a.call(ipc.MethodRoomLeave, ipc.RoomLeaveParams{Reason: reason}, &res); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(res)
			}
			fmt.Fprintln(a.stdout, "Left the call.")
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
		Short: "Verify a peer's fingerprint (stronger than a code)",
		Long: `Compare a fingerprint with a peer over a separate channel and record it.

Joining by code is trust-on-first-use. This is the stronger option: compare the full
fingerprint in person, over a signed message, or on a previous authenticated call, then
record it here. A verified fingerprint cannot be satisfied by a different key, so a
compromised signaling server has nothing to gain.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fp, err := identity.ParseFingerprint(args[0])
			if err != nil {
				return exitWith(ExitUsage, err)
			}
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
		Short: "List peers this machine has authenticated",
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
				fmt.Fprintln(a.stdout, "No peers yet.")
				fmt.Fprintln(a.stdout, "Joining a call by code records the caller automatically.")
				return nil
			}
			t := &table{headers: []string{"FINGERPRINT", "NAME", "LEVEL", "HOW", "LAST SEEN"}}
			for _, e := range res.Entries {
				name := e.DisplayName
				if name == "" {
					name = "-"
				}
				// How a peer was authenticated matters: a fingerprint comparison is
				// a stronger statement than a code, and the listing should say which.
				how := "unverified"
				switch {
				case e.Pinned:
					how = "fingerprint pinned"
				case e.Note != "":
					how = e.Note
				case e.Level == string(identity.TrustVerified):
					how = "fingerprint compared"
				}
				t.add(e.Fingerprint, name, e.Level, how, e.LastSeen)
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
