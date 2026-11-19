package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/audio"

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
