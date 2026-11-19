// Package logging configures structured slog logging for voxen.
//
// Log records are structured and never include private key material, session
// keys, or raw SDP. Those types deliberately have no LogValuer implementation;
// call sites log identifiers (fingerprints, peer IDs) instead.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/s4chdev/voxen/internal/config"
)

// New builds a logger according to cfg writing to w. w is typically stderr for
// the foreground daemon or a rotating file for the background daemon.
func New(cfg config.LoggingConfig, w io.Writer) (*slog.Logger, error) {
	if w == nil {
		w = os.Stderr
	}
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "", "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("logging: unsupported level %q", cfg.Level)
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Normalise time to RFC3339 with milliseconds for operator logs.
			// slog's built-in handlers already use RFC3339; no rewrite needed.
			return a
		},
	}

	var handler slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "", "text":
		handler = slog.NewTextHandler(w, opts)
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: unsupported format %q", cfg.Format)
	}
	return slog.New(handler), nil
}

// OpenFile opens (creating if needed) the log file with private permissions.
// The caller owns the returned file and must close it.
func OpenFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("logging: create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("logging: open %s: %w", path, err)
	}
	// Enforce permissions even if the file pre-existed with a wider mode.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("logging: secure %s: %w", path, err)
	}
	return f, nil
}

// Sink is a log destination. Close releases the file, and is a no-op for a
// stderr-only sink.
type Sink struct {
	Writer io.Writer
	closer io.Closer
}

// Close closes the underlying file if there is one.
func (s *Sink) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// OpenSink builds the daemon's log destination.
//
// A background daemon logs to its file. A foreground daemon logs to stderr *and*
// the file, so "voxen logs" keeps working after "voxen start --foreground" and an
// operator can still watch the terminal. If the file cannot be opened in
// foreground mode the error is not fatal: the daemon keeps logging to stderr and
// the CLI reports the missing file explicitly.
func OpenSink(logPath string, foreground bool) (*Sink, error) {
	if !foreground {
		f, err := OpenFile(logPath)
		if err != nil {
			return nil, err
		}
		return &Sink{Writer: f, closer: f}, nil
	}
	f, err := OpenFile(logPath)
	if err != nil {
		return &Sink{Writer: os.Stderr}, nil
	}
	return &Sink{Writer: io.MultiWriter(os.Stderr, f), closer: f}, nil
}

// WithComponent returns a logger annotated with a stable component name.
func WithComponent(l *slog.Logger, name string) *slog.Logger {
	return l.With("component", name)
}

type ctxKey struct{}

// IntoContext stores l in ctx so libraries can retrieve it without globals.
func IntoContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored in ctx, or slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
