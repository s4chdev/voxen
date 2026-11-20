# voxen

Peer-to-peer, end-to-end encrypted voice calls from the terminal, for Linux,
Windows and macOS. One Go core, a background daemon, and a CLI you can script.

```
voxen init --name alice
voxen start
voxen call            # prints: 265P-DKJV-2AM9
```

On the other machine:

```
voxen join 265P-DKJV-2AM9
```

That is the whole setup. One code, read out or pasted. No fingerprint comparison,
no room IDs, no accounts.

### Calls between different networks

Same LAN works directly. Across networks it depends on NAT: voxen uses STUN to find a
public address and connects directly, with no relay. Two ordinary home routers usually
work; symmetric NAT on both sides does not, and the call says so rather than failing
silently.

The reliable answer is a private overlay. If both machines are on Tailscale or
WireGuard, restrict ICE to that interface and a cross-network call becomes a LAN call:

```yaml
network:
  interface_filter:
    - tailscale0
```

`voxen doctor` detects the interface and tells you this. Verified: filtering to
`tailscale0` reduces gathered candidates from 6 to 2, both on the overlay, and a
filtered session connects over it.

> **Build status: calls carry real microphone audio.**
> Identity, the daemon, the CLI, the security state machines, a self-hosted
> signaling server, and **real WebRTC sessions with the Ed25519-to-DTLS binding
> enforced at runtime** are implemented and tested. A two-peer call carries Opus
> audio over a direct, authenticated SRTP path, and hostile-relay attacks are
> rejected by tests.
>
> Microphone capture and speaker playback work through miniaudio (PipeWire /
> PulseAudio on Linux, WASAPI on Windows, CoreAudio on macOS). `voxen status`
> reports live frame counters, so "is sound actually moving?" has a factual
> answer. Linux is verified end to end here; the Windows and macOS audio paths
> compile in CI but have not been run on real hardware yet, and the docs say so.

---

## What works today

- `voxen init` — Ed25519 identity, `0600` key file with optional
  passphrase-based encryption, explicit trust store, validated YAML config.
- `voxen start` / `stop` / `restart` — background daemon or foreground mode,
  private Unix socket (`0600`, same-UID peer check) or Windows named pipe (ACL
  restricted to your account).
- `voxen status` / `peers` / `devices` / `config` / `logs` / `doctor` — honest
  state, including which subsystems are actually available.
- `voxen identity` / `verify` / `revoke` / `trust` — fingerprint display and
  out-of-band trust management.
- `voxen mute` / `unmute` — state tracked by the daemon.
- `voxen tui` — live call view: participants, connection state, whether the path is
  direct, RTT, packet loss, who is speaking, and live audio counters. Keys mute you
  or a selected participant. Closing it does not end the call.
- `voxen service install` / `status` / `uninstall` — supervision under systemd
  (user unit), launchd (LaunchAgent) or Task Scheduler, so calls survive a crash
  and start at login. Verified end to end on Linux.
- Security primitives: signed session statements with DTLS-fingerprint binding,
  deterministic canonical encodings, replay protection, administrator-signed
  room rosters with rollback rejection.
- **`voxen-signal`** — a self-hosted WebSocket signaling server (single binary,
  no database, never sees audio).
- **Two-peer encrypted media** — Pion WebRTC, Opus 48 kHz mono, direct SRTP, with
  the peer's Ed25519 identity bound to the DTLS certificate actually negotiated.
  Media is refused until that binding succeeds.
- **Room orchestration** — `voxen call`, `join`, `leave`, `peers`. The daemon
  maintains a full mesh: every participant has an independently authenticated
  session with every other one. A verified three-peer mesh is covered by a test
  that drives the real binaries.
- **Real audio** — 48 kHz mono Opus over the authenticated session, with capture
  fanned out to every peer and peer audio mixed into the speaker. Mute keeps the
  RTP timeline alive by sending silence rather than stopping transmission.
- Tests: unit, IPC, daemon, CLI, process-level integration, security acceptance,
  and live two-peer negotiation, all clean under `-race`.

## What does not work yet

| Feature | Phase |
|---|---|
| Speaking indicators, per-peer mute | 6 |
| Explicit admission and removal (`admit` / `remove`) | 6 |
| launchd and Windows supervision verified on real hardware | needs a run on those hosts |
| Echo cancellation and noise suppression verified on real hardware | needs a run with speakers and a mic |
| Windows/macOS audio verified on real hardware | needs a run on those hosts |

---

## Requirements

- Go 1.25 or newer (the repo builds with 1.27).
- A C toolchain, because the audio backend is cgo: `gcc` on Linux (plus
  `libasound2-dev`), the Xcode command line tools on macOS, and MinGW-w64 or
  LLVM/clang on Windows.
- Opus is pure Go, so no `libopus` install is needed.
- Echo cancellation and noise suppression need `libspeexdsp-dev` **and** a build with
  `-tags speex` (`make build-speex`). The default build reports them as unavailable
  rather than pretending.
- Building with `CGO_ENABLED=0` still works; audio is then reported as
  unavailable instead of silently producing no sound.

## Build

```sh
make build          # bin/voxen and bin/voxend
make test           # unit + integration + security suites
make test-race      # everything under the race detector
make cross          # verify all six platform targets compile
make dist           # release archives in dist/
```

Without `make`:

```sh
go build -trimpath -o bin/voxen ./cmd/voxen
go build -trimpath -o bin/voxend ./cmd/voxend
go test ./...
```

### Platform build notes

**Linux**

```sh
make build
./bin/voxen init --name "$USER"
```

Configuration lives in `~/.config/voxen`, state and logs in
`~/.local/state/voxen`, and the socket in `$XDG_RUNTIME_DIR/voxen`.

**macOS**

```sh
make build
./bin/voxen init --name "$USER"
```

Configuration lives in `~/Library/Application Support/voxen` and logs in
`~/Library/Logs/voxen`. Fingerprint and IPC behaviour is identical to Linux. The
microphone permission prompt arrives with the phase 2 audio backend and the
bundle `Info.plist` (`NSMicrophoneUsageDescription`).

**Windows (PowerShell)**

```powershell
go build -trimpath -o bin\voxen.exe .\cmd\voxen
go build -trimpath -o bin\voxend.exe .\cmd\voxend
.\bin\voxen.exe init --name $env:USERNAME
.\bin\voxen.exe start
```

Configuration lives in `%APPDATA%\voxen`; logs in `%LOCALAPPDATA%\voxen\logs`.
The daemon endpoint is a named pipe named after the profile, restricted by a
security descriptor to your account, SYSTEM and Administrators. `voxen start`
detaches the daemon into its own process group with no console window.

## Quick start

```sh
voxen init --name alice          # create identity + config
voxen start                      # start the daemon in the background
voxen status                     # see what is running and what is available
voxen service install            # optional: start at login, restart on crash
voxen identity                   # your fingerprint
voxen doctor                     # diagnostics, including honest gaps
voxen stop
```

With a passphrase-protected key:

```sh
export VOXEN_KEY_PASSPHRASE='a long passphrase'
voxen init --name alice --force   # rotates the key and encrypts it
voxen start                       # the daemon needs the same variable
```

Verifying a peer (this gates media once calls exist):

```sh
# Read the peer's fingerprint over a channel you already trust (in person,
# a signed message, a previous authenticated call), then record it:
voxen verify "C697 1DF6 ABDD 7317 B891 78DE FBB2 BF88 E21A 2469 9C7E 27F4 8EB1 090B 51E8 C1D9" \
     --name bob --note "read over the phone 2026-11-19"
voxen trust      # shows the pin, and later whether a key is bound to it
voxen revoke "C697 ..."
```

Trust is anchored on the fingerprint, like SSH host keys. You can pin a
fingerprint before ever connecting; the key that peer presents in the first
session must hash to it, or the call is refused.

## Commands

```
voxen init                       Create identity and configuration
voxen start [--foreground]       Start the daemon
voxen stop [--force]             Stop the daemon
voxen restart                    Restart the daemon
voxen status [-w]                Daemon, room, audio and capability state
voxen peers                      Participants and their trust/connection state
voxen devices                    Microphones and speakers
voxen config [--path]            Effective configuration (and "config reload")
voxen logs [-n N]                Daemon log tail
voxen doctor                     Diagnostics with actionable hints
voxen mute | unmute              Microphone state
voxen call | join | leave        Rooms (phase 4)
voxen identity                   Local fingerprint
voxen verify <fp>                Verify a peer out of band
voxen trust | revoke <fp>        Inspect or revoke trust
voxen version                    Build and phase information
```

Global flags: `--json` for machine-readable output, `--config <path>`,
`--timeout <duration>`.

Exit codes: `0` ok, `1` error, `2` usage, `3` daemon unavailable,
`4` feature not in this build, `5` authentication/authorization failure.

## Configuration

Generated by `voxen init`; a commented reference is in
[`configs/config.example.yaml`](configs/config.example.yaml). Several entries are
security decisions and cannot be relaxed:

- `security.require_verified_peers` must be `true`.
- `audio.sample_rate` must be `48000` (the media profile normalises to 48 kHz mono).
- There is no TURN setting. `network.turn_servers` is rejected as an unknown key.
- `identity.file` and `daemon.socket` changes require a restart, not a reload.

Environment overrides for isolated profiles are documented in
[`internal/paths`](internal/paths/paths.go): `VOXEN_CONFIG_DIR`, `VOXEN_STATE_DIR`,
`VOXEN_RUNTIME_DIR`, `VOXEN_SOCKET`, `VOXEN_CONFIG`, `VOXEN_KEY_PASSPHRASE`.

## Security model, in one paragraph

voxen binds each peer's Ed25519 identity to the DTLS certificate that is actually
negotiated. A peer signs a short-lived, nonce-protected statement containing the
SHA-256 of its DTLS certificate; after the handshake the receiver hashes the
negotiated certificate and requires an exact match before any audio is attached.
The signaling server relays SDP and ICE but cannot forge that statement, so it
cannot insert itself into the media path. Media only goes to peers whose
fingerprint you verified out of band, and a fingerprint pin can be recorded
before the peer is ever seen. There is no TURN and no relay fallback: if a direct
path fails, the call fails loudly. Full threat model, tested scenarios and known
limitations: [`docs/security.md`](docs/security.md).

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Component boundaries, planes, data flow, cross-platform matrix |
| [docs/adr.md](docs/adr.md) | Ten architecture decision records with alternatives and consequences |
| [docs/security.md](docs/security.md) | Threat model, binding protocol, tested scenarios, limitations |
| [docs/protocol.md](docs/protocol.md) | Signaling wire protocol, handshake, rooms, errors |
| [docs/dependencies.md](docs/dependencies.md) | Every dependency, licence, and why it was chosen |
| [docs/plan.md](docs/plan.md) | Phase-by-phase plan with tests and exit criteria |

## Project layout

```
cmd/voxen/            CLI entry point
cmd/voxend/           daemon entry point
internal/
  audio/             platform-independent audio interfaces
  cli/               Cobra commands, rendering, process lifecycle
  config/            YAML schema and validation
  daemon/            request routing, diagnostics, capability reporting
  fsutil/            atomic private-file writes
  identity/          Ed25519 identity, key storage, trust store
  ipc/               local control protocol and platform transports
  logging/           slog configuration
  media/             media-plane contract and quality stats
  paths/             per-platform directory resolution
  room/              room state machine, admission, rosters
  security/          signed statements, DTLS binding, replay guard
  signaling/         control-plane wire protocol
  version/           build metadata
platform/            per-OS audio backends (phase 2)
server/signaling/    self-hosted signaling server (phase 4)
tests/integration/   process-level end-to-end tests
tests/security/      security acceptance scenarios
configs/ packaging/  example config and service units
```

## Licence

See [LICENSE](LICENSE). Dependencies and their licences are in
[docs/dependencies.md](docs/dependencies.md).
