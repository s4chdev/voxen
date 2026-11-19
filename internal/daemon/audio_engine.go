package daemon

import (
	"log/slog"

	"github.com/s4chdev/voxen/internal/audio"
)

// audioEngineFromEnv builds the platform audio engine.
//
// The engine is constructed once per daemon. If the backend cannot initialise -
// no audio server, a container without a sound device, or a build without cgo -
// the daemon still starts and reports audio as unavailable. Refusing to start
// would make "voxen doctor" useless precisely when the user needs it.
func audioEngineFromEnv(log *slog.Logger) audio.Engine {
	if log == nil {
		log = slog.Default()
	}
	engine, err := audio.NewMiniAudio(log)
	if err != nil {
		log.Warn("audio backend unavailable; calls will connect but carry no sound", "error", err)
		return audio.Null{}
	}
	return engine
}
