package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"crypto/ed25519"
	"github.com/s4chdev/voxen/internal/audio"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/media"

	"github.com/s4chdev/voxen/internal/ipc"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func asIPCError(err error, target **ipc.Error) bool { return errors.As(err, target) }

// hostAudioUsable reports whether this machine can open a microphone and a
// speaker. It intentionally mirrors the daemon's startup probe so tests assert
// against the same notion of "usable" the product uses.
func hostAudioUsable(t *testing.T) bool {
	t.Helper()
	engine, err := audio.NewMiniAudio(nil)
	if err != nil {
		return false
	}
	defer engine.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return audio.Probe(ctx, engine).Usable()
}

// testSigner adapts an in-memory identity to the media plane's LocalSigner.
func testSigner(t *testing.T, name string) media.LocalSigner {
	t.Helper()
	id, err := identity.New(name)
	if err != nil {
		t.Fatal(err)
	}
	return signerForTest{id: id}
}

type signerForTest struct{ id *identity.Identity }

func (s signerForTest) Sign(msg []byte) []byte { return s.id.Sign(msg) }

func (s signerForTest) PublicKey() ed25519.PublicKey { return s.id.PublicKey }

// discardLogger drops all output; tests exercise state, not log format.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
