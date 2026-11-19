# Cross-platform test matrix

Phase 1 verifies the foundation across platforms. Audio and media rows are marked
as pending and must be filled in when the corresponding phase lands — this file
is a checklist, not a claim.

## What runs where

| Test | Linux | Windows | macOS | How |
|---|---|---|---|---|
| Build all targets | ✅ | ✅ | ✅ | `make cross` (6 targets) |
| Unit tests | ✅ | ✅ | ✅ | `go test -short ./internal/...` |
| IPC over Unix socket | ✅ | — | ✅ | `tests/integration` |
| IPC over named pipe | — | ✅ | — | `tests/integration` |
| Same-user peer check | ✅ | ✅ (ACL) | ✅ | unit + integration |
| Key file permissions | ✅ | ACL | ✅ | `tests/integration` |
| Daemon detach + stop | ✅ | ✅ | ✅ | `tests/integration` |
| Second daemon refused | ✅ | ✅ | ✅ | `tests/integration` |
| Security acceptance | ✅ | ✅ | ✅ | `tests/security` |

## Pending phases

| Scenario | Phase | Notes |
|---|---|---|
| Linux ↔ Linux audio | 3 | needs media plane |
| Linux ↔ Windows audio | 3 | needs both audio backends |
| Linux ↔ macOS audio | 3 | needs both audio backends |
| Windows ↔ macOS audio | 3 | needs both audio backends |
| 3-participant mesh | 6 | |
| 5-participant mesh | 6 | |
| Mute during a call | 2/3 | daemon state exists; audio path pending |
| Device hotplug mid-call | 2 | |
| Network interruption | 3 | ICE restart |
| Signaling interruption | 4 | client reconnect |
| Direct-path verification | 3 | assert `Relayed == false` from stats |

## Interoperability harness (phase 8)

The intended harness starts one daemon per platform on a shared network, creates a
room on the self-hosted signaling server, joins from every platform, and asserts
for each pair:

1. ICE selected a host or server-reflexive candidate pair (not relayed).
2. `BindDTLS` ran before the first SRTP packet.
3. A 440 Hz tone injected on one side is measured on the other.
4. Recorded metrics: connection establishment time, RTT, audio latency, packet
   loss, jitter, CPU and RSS.

Until that exists, no cross-platform audio claim is made.

## Manual verification (phase 1)

On each platform:

```sh
voxen init --name tester
voxen start
voxen status          # expect phase 1, available: none, audio: unavailable
voxen mute            # expect "no active call" note
voxen logs -n 20      # expect the startup line
voxen doctor          # expect UNAVAILABLE for audio and signaling
voxen stop
voxen status          # expect a non-zero exit: daemon unavailable
```

On Windows also confirm `%APPDATA%\voxen\identity.json` exists and that a second
`voxen start` reports the daemon already running rather than starting a second one.
