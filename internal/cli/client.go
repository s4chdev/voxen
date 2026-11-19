package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/s4chdev/voxen/internal/daemon"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/logging"
	"github.com/s4chdev/voxen/internal/version"
)

// endpoint resolves the IPC endpoint, preferring the configured value.
func (a *App) endpoint() string {
	if a.cfg.Daemon.Socket != "" {
		return a.cfg.Daemon.Socket
	}
	return a.paths.SocketPath
}

// call performs one request and decodes the result into out.
func (a *App) call(method string, params any, out any) error {
	return a.callContext(context.Background(), method, params, out)
}

func (a *App) callContext(ctx context.Context, method string, params any, out any) error {
	conn, err := ipc.Dial(ctx, a.endpoint(), 2*time.Second)
	if err != nil {
		return exitWith(ExitUnavailable,
			fmt.Errorf("voxend is not running (endpoint %s); start it with 'voxen start'", a.endpoint()))
	}
	defer conn.Close()

	cli := ipc.NewClient(conn, a.timeout)
	req := &ipc.Request{Version: ipc.ProtocolVersion, ID: newRequestID(), Method: method}
	if params != nil {
		raw, encErr := json.Marshal(params)
		if encErr != nil {
			return fmt.Errorf("encode parameters: %w", encErr)
		}
		req.Params = raw
	}
	if err := cli.Call(req, out); err != nil {
		var ipcErr *ipc.Error
		if errors.As(err, &ipcErr) {
			switch ipcErr.Code {
			case ipc.CodeUnsupported:
				return exitWith(ExitUnsupported, ipcErr)
			case ipc.CodeUnauthorized:
				return exitWith(ExitAuth, ipcErr)
			case ipc.CodeUnavailable:
				return exitWith(ExitUnavailable, ipcErr)
			}
		}
		return err
	}
	return nil
}

// daemonReady reports whether a daemon answers a ping.
func (a *App) daemonReady(ctx context.Context) bool {
	conn, err := ipc.Dial(ctx, a.endpoint(), 700*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	cli := ipc.NewClient(conn, 2*time.Second)
	var ping ipc.PingResult
	return cli.Call(&ipc.Request{Version: ipc.ProtocolVersion, ID: newRequestID(), Method: ipc.MethodPing}, &ping) == nil
}

// runDaemonInProcess runs voxend in the foreground of the current process. The
// CLI blocks until the daemon stops, which is what "voxen start --foreground"
// promises: one process, logs on the terminal, Ctrl-C to stop.
func (a *App) runDaemonInProcess(ctx context.Context) error {
	logPath := a.paths.LogFile
	if a.cfg.Logging.File != "" {
		logPath = a.cfg.Logging.File
	}
	sink, err := logging.OpenSink(logPath, true)
	if err != nil {
		return err
	}
	defer sink.Close()

	logger, err := logging.New(a.cfg.Logging, sink.Writer)
	if err != nil {
		return err
	}
	srv, err := daemon.New(ctx, daemon.Options{
		Paths:      a.paths,
		Foreground: true,
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// newRequestID returns a short unique request identifier.
func newRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), requestCounter())
}

var counter int64

func requestCounter() int64 {
	counter++
	return counter
}

// ensureInitialised reports an actionable error when "voxen init" has not run.
func (a *App) ensureInitialised() error {
	idPath := a.paths.IdentityFile
	if a.cfg.Identity.File != "" {
		idPath = a.cfg.Identity.File
	}
	store := identity.StoreFromEnv(idPath)
	exists, err := store.Exists()
	if err != nil {
		return err
	}
	if !exists {
		return exitWith(ExitUsage,
			errors.New("no identity found; run 'voxen init' first"))
	}
	return nil
}

// requirePhase gates a command behind an implemented phase. Commands that
// describe future work must say so explicitly and exit non-zero, never pretend
// to work.
func (a *App) requirePhase(p int, feature string) error {
	if version.Phase >= p {
		return nil
	}
	return exitWith(ExitUnsupported, fmt.Errorf(
		"%s is planned for phase %d; this build is phase %d (%s)", feature, p, version.Phase, version.PhaseName))
}
