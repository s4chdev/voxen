# voxen — Architecture

voxen is a CLI-controlled, peer-to-peer, end-to-end encrypted voice system for
Linux, Windows and macOS. It is one Go codebase with a shared core and thin
platform adapters.

```
                    ┌──────────────────────────────┐
   terminal ───────▶│  voxen (CLI)                  │
                    │  Cobra, thin IPC client      │
                    └──────────────┬───────────────┘
                                   │ private local socket
                                   │ Unix socket / named pipe
                    ┌──────────────▼───────────────┐
                    │  voxend (daemon)              │
                    │                              │
   ┌────────────────┴───────┬──────────────┬───────┴───────────────┐
   │ CONTROL PLANE          │ MEDIA PLANE  │ IDENTITY PLANE        │
   │ signaling client       │ WebRTC (Pion)│ Ed25519 keys          │
   │ room state machine     │ Opus codec   │ trust store           │
   │ roster validation      │ RTP/SRTP     │ signed statements     │
   │ ICE/SDP exchange       │ jitter buffer│ replay guard          │
   └───────────┬────────────┴──────┬───────┴───────────┬───────────┘
               │                   │                   │
      ┌────────▼────────┐ ┌────────▼────────┐ ┌────────▼─────────┐
      │ ws:// signaling │ │ audio backends  │ │ OS key storage   │
      │ server (self-   │ │ PipeWire/Pulse  │ │ 0600 files with  │
      │ hosted)         │ │ WASAPI/CoreAudio│ │ optional scrypt+ │
      └─────────────────┘ └─────────────────┘ │ XChaCha20 sealing│
                                              └──────────────────┘

   media path: peer ◀──── DTLS-SRTP over UDP (direct) ────▶ peer
```

## Package boundaries

| Package | Responsibility | Must not know about |
|---|---|---|
| `internal/ipc` | Local control protocol, transport, peer credential checks | Audio, WebRTC |
| `internal/cli` | Cobra commands, rendering, process lifecycle | Key internals, audio devices |
| `internal/daemon` | Request routing, capability reporting, diagnostics | Cobra, terminal |
| `internal/config` | YAML schema, validation, defaults | Runtime state |
| `internal/identity` | Ed25519 generation, key storage, trust store | Networking |
| `internal/security` | Signed statements, DTLS binding, replay guard, rosters | Sockets, audio |
| `internal/signaling` | Wire protocol types and client contract | Media internals |
| `internal/room` | Admission policy, roster state, peer lifecycle | SDP parsing |
| `internal/media` | WebRTC/Opus session contract and stats | Signaling transport |
| `internal/audio` | Device enumeration and stream contract | WebRTC, identity |
| `platform/*` | Concrete audio backends, autostart units | Protocol logic |

The three planes communicate through interfaces defined by the consumer:
`room.TrustChecker` (identity → room), `media.Session` (room → media),
`audio.Engine` (media → audio), `signaling.Connection` (room → control). None of
them returns a concrete struct owned by another plane.

## Control plane

The signaling server coordinates rooms and relays envelopes. It never participates
in media and is never trusted: SDP and ICE payloads are opaque to it, and the
signed statements that authenticate them travel as payload. See
[protocol.md](protocol.md).

State machine for a room (implemented in `internal/room`):

```
idle ──BeginJoin──▶ joining ──Activate(roster)──▶ active ──Leave──▶ idle
                       │                              │
                       └──────Fail────────▶ failed ◀──┘
```

`Activate` requires a roster signed by the pinned administrator that lists the
local identity, at a sequence number not lower than the last one applied.
Roster updates are monotonic; a rolled-back roster is rejected.

## Media plane

The media plane is implemented in `internal/media`. A session cannot transmit or
receive audio until it has completed the four-step authentication below; this is
enforced in code, not by convention (`SendPCM` and `ReceivePCM` both return
`ErrUnauthenticated` before the binding commits).

Each peer connection is negotiated independently:

1. The initiator creates an offer after attaching an Opus track.
2. Both sides trickle ICE candidates, exchanging them through signaling.
3. DTLS completes; the negotiated certificate is captured.
4. `security.BindDTLS` compares that certificate with the fingerprint in the
   peer's signed statement.
5. Only then is the capture stream attached. Audio flows over SRTP.

No TURN server exists in the configuration or the code. If ICE cannot find a
direct path within `network.ice_timeout_ms`, the session fails with an explicit
error.

## Identity plane

The local key is generated once and written atomically with `0600` permissions.
Optionally the 32-byte Ed25519 seed is sealed with XChaCha20-Poly1305 under a
scrypt-derived key; the public key, fingerprint and creation time are bound into
the AEAD associated data so keys cannot be swapped between files.

Verification is explicit and out of band. `voxen verify <fingerprint>` promotes a
peer key to `verified` only if that key was already observed through a session
attempt; voxen never fabricates a trust record from a string. Revocation is
sticky: a revoked key stays revoked and is never silently downgraded to
unverified.

## Cross-platform notes

| Concern | Linux | Windows | macOS |
|---|---|---|---|
| Config | `$XDG_CONFIG_HOME/voxen` | `%APPDATA%\voxen` | `~/Library/Application Support/voxen` |
| State / logs | `$XDG_STATE_HOME/voxen` | `%LOCALAPPDATA%\voxen` | `~/Library/Logs/voxen` |
| IPC | Unix socket `0600` | named pipe + security descriptor | Unix socket `0600` |
| Peer check | `SO_PEERCRED` | pipe ACL | `LOCAL_PEERCRED` |
| Detach | `setsid` | `DETACHED_PROCESS` | `setsid` (launchd in phase 7) |
| Audio | PipeWire/PulseAudio (phase 2) | WASAPI via miniaudio (phase 2) | CoreAudio via miniaudio (phase 2) |

All platforms share the same config schema, IPC protocol, signaling protocol,
identity format and media profile.

## Build phases

| Phase | Scope | State in this build |
|---|---|---|
| 1 | foundation: modules, interfaces, config, logging, CLI, daemon, IPC, identity, security primitives | **complete** |
| 2 | audio backends, device hotplug | **partially** — Opus pipeline done; PipeWire/WASAPI/CoreAudio capture pending |
| 3 | Pion peer connections, two-peer encrypted audio, DTLS binding at runtime | **complete and tested** |
| 4 | WebSocket signaling client and self-hosted server, rooms, STUN | **complete and tested** |
| 5 | full security test matrix, verifier tooling, rotation procedures | planned |
| 6 | mesh group calls, speaking indicators, per-peer mute | planned |
| 7 | daemon supervision (systemd/launchd/Windows), reconnection, TUI hooks | planned |
| 8 | packaging, cross-platform CI matrix, audits | planned |

`voxen status` and `voxen doctor` report the build phase and each subsystem's real
state, so an operator is never told a phase-2 feature works in a phase-1 binary.
