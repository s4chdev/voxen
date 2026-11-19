package cli

import (
	"context"

	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/tui"
)

// ipcTUI adapts the CLI's IPC client to the TUI's interface.
//
// Every call dials the daemon, which is correct rather than wasteful: the TUI polls
// once a second and a dial costs well under a millisecond on a Unix socket. Reusing
// a connection would mean handling reconnects on daemon restart, which the dial-per-
// call model gets for free.
type ipcTUI struct {
	app *App
}

func (i ipcTUI) Status(ctx context.Context) (ipc.StatusResult, error) {
	var out ipc.StatusResult
	if err := i.app.callContext(ctx, ipc.MethodStatus, nil, &out); err != nil {
		return ipc.StatusResult{}, err
	}
	return out, nil
}

func (i ipcTUI) Peers(ctx context.Context) ([]ipc.PeerState, error) {
	var out ipc.PeersResult
	if err := i.app.callContext(ctx, ipc.MethodPeersList, nil, &out); err != nil {
		return nil, err
	}
	return out.Peers, nil
}

func (i ipcTUI) SetMuted(ctx context.Context, muted bool) (ipc.MuteResult, error) {
	var out ipc.MuteResult
	if err := i.app.callContext(ctx, ipc.MethodMuteSet, ipc.MuteParams{Muted: &muted}, &out); err != nil {
		return ipc.MuteResult{}, err
	}
	return out, nil
}

func (i ipcTUI) SetPeerMuted(ctx context.Context, peerID string, muted bool) (ipc.PeerMuteResult, error) {
	var out ipc.PeerMuteResult
	err := i.app.callContext(ctx, ipc.MethodPeerMuteSet, ipc.PeerMuteParams{PeerID: peerID, Muted: &muted}, &out)
	if err != nil {
		return ipc.PeerMuteResult{}, err
	}
	return out, nil
}

var _ tui.Client = ipcTUI{}
