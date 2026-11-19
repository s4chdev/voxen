# voxen — Dependencies and Licences

Every runtime dependency is listed with its licence and the reason it is used.
The rule is: actively maintained, permissively licensed, no unnecessary surface.

Verified against the module graph in `go.sum` at the time of writing.

## Direct dependencies

| Module | Version | Licence | Why |
|---|---|---|---|
| `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 | CLI command tree, required by the brief |
| `gopkg.in/yaml.v3` | v3.0.1 | Apache-2.0 / MIT | YAML configuration (fork of libyaml, permissive) |
| `golang.org/x/crypto` | v0.59.0 | BSD-3-Clause | scrypt KDF and XChaCha20-Poly1305 for optional key-file encryption. The Go team's repository; audited primitives, not a hobby implementation |
| `golang.org/x/sys` | v0.50.0 | BSD-3-Clause | `unix.GetsockoptXucred` on macOS for the peer-UID check |
| `github.com/Microsoft/go-winio` | v0.6.4 | MIT | Windows named pipes with an explicit security descriptor |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | MIT | TUI event loop and terminal control |
| `github.com/charmbracelet/lipgloss` | v1.1.1 | MIT | TUI styling and layout |
| `github.com/charmbracelet/bubbles` | v1.0.0 | MIT | Key bindings and the help component |
| `github.com/charmbracelet/glamour` | v1.0.0 | MIT | Markdown rendering for the long-form help page |

`go mod tidy` drops anything not imported, so this table cannot drift into
listing modules the binary does not actually use. `github.com/gorilla/websocket`
was evaluated and is committed to for phase 4, but is intentionally **not** a
dependency yet: it will be added in the change that first imports it.

### Why four TUI libraries rather than one

They are separate concerns maintained by the same project: Bubble Tea owns the event
loop, Lip Gloss owns styling, Bubbles provides components (the key-binding and help
packages are used here), and Glamour renders markdown. Using Bubbles' key bindings
rather than raw string switches means the help text and the input handling are
generated from one definition, so help cannot describe a key that does not work.
Glamour is confined to the help page: a markdown engine has no business in a view
that redraws every second.

## Planned for later phases (not yet linked into the binary)

| Module | Phase | Licence | Role |
|---|---|---|---|
| `github.com/pion/webrtc/v4` | 3 | MIT | WebRTC peer connections, DTLS-SRTP, ICE, RTP |
| `github.com/gorilla/websocket` | 4 | BSD-2-Clause | WebSocket transport for the signaling client and server |
| `github.com/gen2brain/malgo` | 2 | Unlicense (public domain) | miniaudio bindings: WASAPI, CoreAudio, PulseAudio/ALSA behind one API |
| `github.com/thesyncim/gopus` | 2 | BSD-3-Clause | Pure-Go Opus encoder/decoder, avoids a cgo dependency on libopus |

**Why `thesyncim/gopus` and not `hraban/opus` (cgo libopus)?** A pure-Go codec
keeps `CGO_ENABLED=0` cross-compilation intact for all six targets and removes a
native runtime dependency from the release artifacts. `gopus` is the actively
maintained pure-Go implementation, is differential-tested against libopus, and
is BSD-3-Clause. The trade-off — raw encode throughput versus the C
implementation — is acceptable for mono 20 ms voice frames at 28 kbps. Phase 2
benchmarks both before committing.

**Why `gen2brain/malgo` and not PortAudio?** The brief allows either. `malgo`
exposes WASAPI, CoreAudio and PulseAudio through miniaudio with one dependency
and no separate PortAudio install on Windows/macOS, which materially improves the
"native utility" experience. It is public domain, actively maintained, and its
device callbacks map cleanly onto `audio.CaptureStream`/`PlaybackStream`. PortAudio
remains a valid swap-in behind the same interface.

## Standard library usage for security-critical work

All cryptographic primitives come from the Go standard library:
`crypto/ed25519` (identity and signatures), `crypto/ecdsa` +
`crypto/x509` (DTLS certificate handling), `crypto/sha256` (fingerprints),
`crypto/rand` (all key and nonce generation). Only the KDF (scrypt) and the AEAD
used for optional key-file encryption (XChaCha20-Poly1305) come from
`golang.org/x/crypto`, because the standard library provides neither. **No
primitive is implemented by voxen.**

## Transitive dependencies

The full graph is recorded in `go.sum`; `make license` writes the resolved module
list to `dist/dependencies.txt` for audits. The only transitive modules today are
`spf13/pflag` and `inconshreveable/mousetrap` (Apache-2.0 and Apache-2.0/MIT)
pulled in by Cobra.

## Licence policy

- Apache-2.0, MIT, BSD-2/3-Clause, ISC and public-domain/Unlicense are accepted.
- GPL, AGPL, LGPL and SSPL are rejected for linked code because they impose
  obligations on downstream users of a distributed binary.
- A dependency that is unmaintained for more than 24 months or unreachable is
  replaced rather than vendored and patched indefinitely.

## Reproducible builds

`make build` uses `-trimpath` and injects `version.Version`, `version.Commit` and
`version.BuildDate`. With a fixed toolchain (recorded in `go.mod`) and the module
graph pinned in `go.sum`, two builds of the same commit produce identical
binaries apart from the build date, which can be pinned with
`BUILD_DATE=<fixed value>`.
