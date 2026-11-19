# voxen — Architecture Decision Records

Each record states the decision, the context, and the consequences. They are
append-only: a superseded decision gets a new record that references the old one.

---

## ADR-001: One shared core, thin per-platform adapters

**Status:** accepted (phase 1)

**Context.** voxen must interoperate between Linux, Windows and macOS, and the
signaling, identity and room protocols must be identical everywhere. Audio
device access is the only genuinely platform-specific concern.

**Decision.** All protocol logic, media negotiation, identity, CLI and daemon
behaviour live in `internal/` with no platform build tags. Platform code is
limited to `platform/` adapters (audio backends, autostart) and to small
build-tagged files for OS primitives (IPC transport, process detachment,
credential checks). There is no `//go:build` in the control, identity, room or
security planes.

**Consequences.** A protocol change cannot drift between platforms. Audio
backends can be developed and tested independently. The shared core must be
portable: it may only use OS-specific behaviour through the interfaces in
`internal/audio` and `internal/ipc`.

---

## ADR-002: Daemon owns all privileged and stateful resources

**Status:** accepted (phase 1)

**Context.** A CLI that owns the microphone, WebRTC sessions and keys is hard to
keep alive across terminal sessions and encourages copy-pasted session setup. A
CLI that shells out to the platform audio stack violates the requirement to
avoid shell commands as a production engine.

**Decision.** `voxend` is the single process that opens audio devices, holds the
private key, maintains WebRTC peer connections and speaks the signaling
protocol. `voxen` is a thin IPC client. `voxen init` is the only CLI command that
writes key material, and it does so before a daemon exists.

**Consequences.** Device and session lifetime is decoupled from the terminal.
The attack surface of the CLI is tiny. A compromised daemon implies compromised
audio and keys, so the daemon's own hardening (socket ACL, no network listener
beyond signaling) matters and is documented in `docs/security.md`.

---

## ADR-003: Private local IPC with peer-credential checks

**Status:** accepted (phase 1)

**Alternative considered.** Localhost TCP with a bearer token. Rejected: any
local process can connect, tokens leak into process listings and logs, and port
collisions complicate multi-profile use.

**Decision.** Unix domain socket (`0600`) inside a `0700` directory on Linux and
macOS; a named pipe with a security descriptor granting only the current user,
SYSTEM and Administrators on Windows. The daemon additionally verifies
`SO_PEERCRED` (Linux) / `LOCAL_PEERCRED` (macOS) on every accepted connection.
The framing is newline-delimited JSON with a 1 MiB cap and a versioned envelope.

**Consequences.** Local privilege boundaries are enforced twice. Audio never
crosses this socket, so the frame cap stays small. The protocol is testable
without networking.

---

## ADR-004: Ed25519 identity, signed canonical encodings, no JSON for signatures

**Status:** accepted (phase 1)

**Context.** Signatures must be stable across languages and implementations. JSON
canonicalisation (key order, number formats, duplicate keys) is a classic source
of verification bypasses.

**Decision.** Every signed structure has a deterministic, length-prefixed binary
encoding built by a single `encoder` type, with a per-structure domain-separation
prefix (`voxen/v1/session-statement`, `voxen/v1/room-membership`). Ed25519 is used
for identity, statements and roster signatures. No cryptographic primitive is
implemented locally: `crypto/ed25519`, `crypto/ecdsa` and `crypto/sha256` are
used directly.

**Consequences.** Signatures are unambiguous and domain-separated. The encoding
is versioned (`ProtocolVersion`) so it can evolve without ambiguity.

---

## ADR-005: Identity-to-DTLS binding is the basis of "E2EE"

**Status:** accepted (phase 1, enforcement implemented; runtime binding in phase 3)

**Context.** DTLS-SRTP already encrypts media between two DTLS endpoints. But the
SDP fingerprint that identifies the endpoint is relayed by the signaling server.
Trusting it blindly means the server (or anyone who can rewrite its messages)
can substitute its own certificate and sit in the middle.

**Decision.** Each peer signs a `SessionStatement` containing the SHA-256 of its
DTLS certificate, the room ID, a session ID, a nonce and a short validity
window. Verification happens in four steps: (1) the issuer must be the identity
already verified for this peer, (2) the signature must be valid, (3) the
statement must be current, in-room, in-session and non-replayed, and (4) after
the DTLS handshake the certificate **actually negotiated** must hash to the
fingerprint in the signed statement. Media may not be attached before step 4.
voxen does not describe a call as end-to-end encrypted until step 4 has run.

**Consequences.** A malicious signaling server can deny service but cannot
insert itself into the media path without breaking authentication. Statements
are short-lived and nonce-protected so a captured statement cannot be replayed.
The limitation — a compromised *local* endpoint or a user who verifies the wrong
fingerprint — is documented rather than hidden.

---

## ADR-006: Direct connectivity only; no TURN, no relay fallback

**Status:** accepted (phase 1)

**Decision.** ICE uses STUN and host candidates only. TURN is neither configured
nor implemented, and there is no code path that forwards media through a server.
The configuration schema has no TURN field; an unknown `turn_servers` key is
rejected at load time. A failed direct path surfaces as
`media: no direct peer-to-peer path` rather than a silent relay.

**Consequences.** Calls fail on symmetric-NAT-only paths by design. That is a
product decision, not an oversight: the alternative is a server that sees
ciphertext but controls reachability, and the brief forbids it. Operators who
need reachability can run their own STUN server.

---

## ADR-007: Mesh topology with a hard participant cap

**Status:** accepted (phase 1, mesh implemented in phase 6)

**Context.** Group calls must not use an SFU.

**Decision.** Every participant has an independent authenticated session with
every other participant. The cap defaults to 5 and is hard-limited to 16 in the
room state machine, because upload bandwidth and CPU grow linearly per peer.

**Consequences.** Audio quality degrades predictably with group size. Each
connection is separately authenticated, so one compromised peer cannot inject
audio into another peer's session. The cap is enforced in code, not just
documented.

---

## ADR-008: Opus profile is fixed, not negotiated per call

**Status:** accepted (phase 1, implemented in phase 2)

**Decision.** 48 kHz mono, 20 ms frames, VoIP application mode, in-band FEC and
DTX enabled, 24–32 kbps target. These are constants in the media profile rather
than SDP-negotiated options, because every voxen endpoint speaks exactly one
profile and a negotiation failure should not silently produce a lower-quality
call.

**Consequences.** Interoperability is trivial and predictable; the trade-off is
no adaptation to unusual network conditions beyond FEC/DTX. Jitter buffering and
PLC handle loss at the media layer.

---

## ADR-009: Strict configuration validation, unknown keys rejected

**Status:** accepted (phase 1)

**Decision.** `yaml.Decoder.KnownFields(true)`, explicit range checks, and
refusal to start on any validation error. Security-relevant limits are constants
in code; YAML can tighten them within bounds but cannot relax them.

**Consequences.** Upgrades that rename a key produce a clear error instead of
silently reverting to a default. Operators cannot disable
`security.require_verified_peers`.

---

## ADR-010: Honest capability reporting instead of stubs

**Status:** accepted (phase 1)

**Context.** The brief explicitly forbids placeholder implementations presented
as complete features.

**Decision.** The daemon reports a `Capabilities` matrix and the build phase in
`voxen status` and over IPC. Commands that depend on unimplemented subsystems exit
with code 4 (`unsupported`) and explain which phase delivers them. The audio
engine is an `audio.Null` that returns `ErrUnavailable`, and `voxen doctor`
prints `UNAVAILABLE` for signaling and audio. The project was scaffolded as
`hush` and renamed to `voxen` before the first release; the wire format uses the
`voxen/` domain prefix from the start, so no compatibility path is needed.

**Consequences.** Nothing in the CLI can be mistaken for working audio or media.
This record must be deleted or amended in the phase where each capability turns
real.
