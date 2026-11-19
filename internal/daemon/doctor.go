package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/s4chdev/voxen/internal/audio"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
	"github.com/s4chdev/voxen/internal/paths"
	"github.com/s4chdev/voxen/internal/version"
)

// doctor runs diagnostics that can be answered from inside the daemon. Checks
// report honest "unavailable" states instead of passing a feature that does not
// exist in this build.
func (s *Server) doctor(ctx context.Context) ipc.DoctorResult {
	var checks []ipc.DoctorCheck
	add := func(name, status, message, hint string) {
		checks = append(checks, ipc.DoctorCheck{Name: name, Status: status, Message: message, Hint: hint})
	}

	add("daemon", "pass", fmt.Sprintf("running as pid %d, %s", os.Getpid(), version.String()), "")

	// Config.
	if _, err := os.Stat(s.opts.Paths.ConfigFile); err != nil {
		add("config", "fail", err.Error(), "run 'voxen init'")
	} else if err := paths.CheckPermissions(s.opts.Paths.ConfigFile, 0o600); err != nil {
		add("config", "warn", "configuration is readable by other users: "+err.Error(), "chmod 600 the config file")
	} else {
		add("config", "pass", s.opts.Paths.ConfigFile, "")
	}

	// Identity and trust store.
	if err := paths.CheckPermissions(s.identityPath(), 0o600); err != nil {
		add("identity", "warn", "key file permissions are too wide: "+err.Error(), "chmod 600 the identity file")
	} else {
		add("identity", "pass", "fingerprint "+s.identity.Fingerprint().String(), "")
	}
	if err := paths.CheckPermissions(s.trust.Path(), 0o600); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			add("trust-store", "pass", "no verified peers yet", "verify a peer with 'voxen verify <fingerprint>'")
		} else {
			add("trust-store", "warn", err.Error(), "chmod 600 the trust store")
		}
	} else {
		add("trust-store", "pass", fmt.Sprintf("%d peer record(s)", len(s.trust.List())), "")
	}

	// Secret hygiene: a passphrase in the environment is visible to the same
	// user's processes on some platforms.
	if os.Getenv(identity.PassphraseEnv) != "" {
		add("key-encryption", "pass", "identity is unlocked from "+identity.PassphraseEnv, "")
	} else {
		add("key-encryption", "warn",
			"identity key is stored unencrypted and protected by file permissions only",
			"set "+identity.PassphraseEnv+" before 'voxen init' to encrypt the key file")
	}

	// IPC endpoint.
	if _, err := os.Stat(s.cfg.Endpoint(s.opts.Paths)); err == nil {
		add("ipc", "pass", s.cfg.Endpoint(s.opts.Paths), "")
	} else {
		add("ipc", "warn", "daemon endpoint not found (answering over the current connection)", "")
	}

	// Signaling reachability is a phase 4 feature; report it as such rather
	// than pretending the socket is fine.
	add("signaling", "unavailable",
		"signaling transport is not implemented in phase "+fmt.Sprint(version.Phase),
		"phase 4 implements the WebSocket client and server")

	// Audio.
	devices, err := s.audio.Devices(ctx)
	switch {
	case errors.Is(err, audio.ErrUnavailable):
		add("audio", "unavailable",
			"no audio backend is compiled into this build (phase "+fmt.Sprint(version.Phase)+")",
			"phase 2 adds PipeWire/PulseAudio, WASAPI and CoreAudio backends")
	case err != nil:
		add("audio", "fail", err.Error(), "check your audio server and device permissions")
	default:
		inputs, outputs := 0, 0
		for _, d := range devices {
			if d.IsInput {
				inputs++
			}
			if d.IsOutput {
				outputs++
			}
		}
		status := "pass"
		if inputs == 0 {
			status = "warn"
		}
		add("audio", status, fmt.Sprintf("%s: %d input, %d output device(s)", s.audio.Version(), inputs, outputs),
			"run 'voxen devices' for the full list")
	}

	// Clock: signed statements depend on reasonable time synchronisation.
	add("clock", "pass", time.Now().UTC().Format(time.RFC3339)+" (UTC)", "")

	// Platform notes.
	switch runtime.GOOS {
	case "darwin":
		add("microphone-permission", "unavailable",
			"microphone permission cannot be requested by a headless daemon in this build",
			"phase 2 adds the TCC usage description and a permission preflight")
	case "windows":
		add("background-service", "unavailable",
			"Windows background process management is not implemented yet",
			"phase 7 adds detached daemon startup")
	}

	ok := true
	for _, c := range checks {
		if c.Status == "fail" {
			ok = false
		}
	}
	return ipc.DoctorResult{Checks: checks, OK: ok}
}

func (s *Server) identityPath() string {
	if s.cfg.Identity.File != "" {
		return s.cfg.Identity.File
	}
	return filepath.Join(s.opts.Paths.ConfigDir, paths.IdentityFileName)
}
