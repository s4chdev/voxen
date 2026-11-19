# voxen — Implementation Plan

Phase 1 is complete and shippable as a foundation. This is the plan for the
remaining phases, with tests and exit criteria. Nothing below is claimed as
done; `voxen status` reports the real phase.

---

## Phase 2: Cross-platform audio and Opus

**Goal.** Capture and playback at 48 kHz mono on all three platforms, with a
real Opus pipeline, behind the interfaces already defined in `internal/audio`.

Work:

1. `platform/linux/audio`: PipeWire through the PulseAudio compatibility layer,
   falling back to plain PulseAudio. No shelling out: use the native client
   library via cgo or a maintained Go binding.
2. `platform/windows/audio`: WASAPI via miniaudio (`malgo`), including device
   enumeration with the WASAPI loopback filter and hotplug notifications.
3. `platform/darwin/audio`: CoreAudio via miniaudio, plus a microphone
   permission preflight (`AVCaptureDevice authorizationStatus`) and the
   `NSMicrophoneUsageDescription` in the bundle `Info.plist`.
4. Resampling: capture at the device rate, convert to 48 kHz mono `int16` with a
   windowed-sinc resampler in `internal/audio/dsp`. Test with 44.1 kHz and
   96 kHz simulated inputs.
5. `internal/media/opus`: encoder/decoder construction with the fixed profile
   (48 kHz, mono, 20 ms, VoIP mode, FEC, DTX, 28 kbps), packet-loss concealment
   on decode, and a drift-correcting jitter buffer.
6. Device hotplug: watch for removal, fall back to the default device, report the
   switch in `voxen status` and the log.

Tests:

- Loopback test: capture a generated tone, encode, decode, compare spectral
  energy (per platform, in CI where a virtual device exists).
- Resampler unit tests with known sample sequences.
- Jitter buffer tests: burst loss, late arrival, reordering.
- Hotplug simulation via the backend's device-change callback.

Exit criteria: `voxen devices` lists real devices on all three platforms; a
recorded 5-second buffer round-trips through Opus with acceptable SNR; a device
unplug mid-capture switches without crashing and is reported.

Risks: PipeWire client-library API churn; Windows cgo toolchain in CI; macOS TCC
permission needing a signed bundle rather than a bare binary.

---

## Phase 3: P2P media and the runtime DTLS binding

**Goal.** Two peers exchange encrypted audio over a direct WebRTC connection,
with the identity binding enforced at runtime.

Work:

1. `internal/media/webrtc`: Pion `PeerConnection` construction with only STUN
   servers, an Opus track, RTCP feedback (NACK, PLI), and an explicit check that
   the selected candidate pair is not relayed.
2. SDP offer/answer and trickle ICE wiring to `signaling.Connection`.
3. Capture the negotiated DTLS certificate after `ConnectionState == connected`
   and run `security.BindDTLS` **before** attaching the capture stream.
4. ICE restart on network change, with a new session ID and statement.
5. Stats collection through `GetStats`, mapped into `media.SessionStats`.

Tests:

- Two processes on one host: direct UDP path established, audio frames arrive,
  `BindDTLS` observed to run before the first SRTP packet.
- Negative test: a test harness that rewrites the SDP fingerprint must be
  rejected and must never receive media.
- Negative test: a certificate swap after the handshake must be rejected.
- Latency and RTT measurements recorded in the test log.

Exit criteria: a two-peer call works with real audio, and the logs prove the
binding ran. **Only at this point may voxen claim E2EE calls.**

---

## Phase 4: Signaling server and rooms

Work:

1. `server/signaling`: WebSocket server implementing [protocol.md](protocol.md),
   in-memory rooms, hello proof verification, rate limiting, idle expiry, and
   structured logging. Single binary, no database.
2. `internal/signaling/ws`: client with reconnect, ping/pong, and explicit
   surfacing of dropped messages.
3. Room create/join/leave, invites carrying a signed roster, and admission.
4. STUN configuration from local config only.

Tests:

- Protocol conformance tests against the server with a fake client.
- Reconnect test: kill the socket mid-negotiation; the client must report and
  retry rather than hang.
- Roster forgery test at the wire level.

---

## Phase 5: Security hardening

- End-to-end test matrix from `tests/security`, extended to the live path.
- Key rotation and revocation runbooks, with a CLI flow that prints exactly what
  peers must do.
- A `voxen verify --qr`-style out-of-band channel is considered; the default stays
  manual fingerprint comparison.
- Threat-model review against `docs/security.md`, updating that document where
  the implementation tightened or relaxed a property.
- Fuzz targets: SDP statement parsing, roster parsing, IPC frames.

---

## Phase 6: Group calls (mesh)

Work:

1. Wire `room.Room` peers to one `media.Session` each.
2. Per-peer audio mixdown for playback (sum with clipping protection, or
   per-peer streams if the backend supports it).
3. Speaking indicators from Opus VAD or energy, per-peer mute, roster-driven
   teardown.
4. Enforce the participant cap in both the room layer and the signaling server.

Tests: 2, 3 and 5 participants on one host and across the three platforms;
participant removal mid-call; per-peer connection failure with the rest of the
call surviving; bandwidth measurement at 5 participants.

---

## Phase 7: Daemon supervision and reconnection

- systemd user unit (`packaging/systemd/voxend.service`) with `Restart=on-failure`.
- launchd agent (`packaging/launchd/sh.sh.voxen.voxend.plist`) with `RunAtLoad`.
- Windows: detached start (already implemented) plus an optional service wrapper.
- CLI reconnection policy, daemon crash detection, and `voxen logs --follow`
  implemented as a streaming IPC event rather than a stub.
- TUI hooks: the IPC `status`/`peers` payloads are already shaped for a future
  Bubble Tea front end.

---

## Phase 8: Production readiness

- CI matrix: Linux, macOS, Windows on amd64 and arm64; `make ci` on each.
- Release archives per platform with checksums and a licence bundle.
- Cross-platform interoperability runs (Linux↔Windows, Linux↔macOS,
  Windows↔macOS) driven by a scripted harness.
- Network failure tests: packet loss injection, interface flap, NAT rebinding.
- Independent security review before any "production ready" claim.

---

## Definition of done (per phase)

A phase is complete only when: it compiles for all six targets, `go test -race`
passes, the integration suite passes, the documentation and ADRs reflect reality,
and `voxen status`/`voxen doctor` tell the truth about it.

---

## Progress update: phase 3 and 4 vertical slice

The foundation-first order was changed after phase 1, deliberately. The security
claim is the differentiator and it can be tested headlessly, so it was built
before the audio backends, which can only be verified on their own platforms.

### Delivered

| Item | Phase | Evidence |
|---|---|---|
| Opus encode/decode, fixed profile | 2 (partial) | `internal/media/codec.go`, round-trip test |
| Pion peer connections | 3 | `internal/media/webrtc_session.go` |
| Signed statement → live DTLS binding | 3 | `TestTwoPeerAuthenticatedSession` |
| Refusal to move media before auth | 3 | `TestAudioRefusedBeforeAuthentication` |
| ICE candidate buffering for trickle | 3 | `internal/media` candidate tests |
| Direct-path verification (`Relayed` false) | 3 | `TestStatsShowDirectPath` |
| Self-hosted signaling server | 4 | `server/signaling`, `cmd/voxen-signal` |
| Signaling client with relay | 4 | `internal/signaling/client.go` |
| Cross-room injection rejected | 4 | `TestSignalingRelayRejectsCrossRoomInjection` |
| Server cannot spoof `from` | 4 | `TestServerAssignsFromAndIgnoresSpoof` |
| End-to-end: server → two sessions → audio | 3+4 | `TestSignalingRelayAndDirectMedia` |

### Bugs this work exposed (all fixed, all regression-tested)

1. **Trickle ICE candidates were dropped.** Relaying only the candidate string
   loses `sdpMid`/`sdpMLineIndex`, so Pion cannot attach it to a media section.
   Candidates are now carried with their media identifiers.
2. **Candidates arriving before a remote description were lost.** They are now
   buffered (bounded) and flushed once negotiation applies the remote SDP.
3. **`Authenticate` self-deadlocked.** It waited on a channel only it closes.
4. **A tampering attempt could poison the session.** The replay guard committed
   the nonce before the SDP fingerprint was checked, so a rejected tampered offer
   made the genuine retry look like a replay. Fixed by ordering the checks;
   `TestRejectedTamperDoesNotBurnNonce` pins it.
5. **Server self-deadlock** in `createRoom` (locking `s.mu` twice).

### Remaining

Phase 2 (real audio), phase 6 (mesh + room CLI), phase 7 (service supervision),
phase 8 (multi-host interop). The audio interfaces are unchanged, so phase 2 slots
in behind `audio.Engine` without touching the security code.

---

## Progress update: room orchestration

The daemon now drives the media mesh that phase 3 built. `voxen create`, `join`,
`leave` and `peers` work end to end against real binaries.

### Delivered

| Item | Evidence |
|---|---|
| Controller joining signaling, room state and media | `internal/daemon/controller.go` |
| Deterministic offer direction (smaller key offers) | `compareKeys`, controller tests |
| Answerer adopts the initiator's session ID | `internal/media/sessionid_test.go` |
| Verified two-peer mesh via real binaries | `TestRealBinariesFormMesh` |
| Verified three-peer full mesh (2 sessions each) | `TestRealBinariesThreePeerMesh` |
| Unverified peer refused, with an explanatory event | `TestRealBinariesRefuseUnverifiedPeer` |
| Fingerprint pins completed on first contact | `peerKey` in `controller.go` |

### Bugs found and fixed

1. **Answerer insisted on its own session ID.** In WebRTC the offer defines the
   negotiation, so a peer answering an offer it did not generate could never
   authenticate. The answerer now adopts the offer's ID, with validation and a
   rule that the ID cannot change once negotiated.
2. **Existing members were never dialled.** The join handshake consumes the room
   roster before the relay loop starts, so a peer joining a non-empty room waited
   forever for a `peer.join` that had already happened. The roster is now passed
   into activation and dialled explicitly.
3. **`Stats` raced `Close`.** Pion's `GetStats` reads state that `Close` mutates,
   and Pion closes the connection internally on DTLS close-notify. Teardown is now
   daemon-owned (`DisableCloseByDTLS`) and `Stats` is serialised against `Close`.
4. **Media required a bound key, not a verified fingerprint.** The normal
   pre-call flow pins a fingerprint before ever seeing the peer, so the mesh could
   never start. The controller now accepts the advertised key when it hashes to a
   verified pin, and binds it.
5. **A fingerprint pin replacing a direct verification** was not handled; both
   paths now authorise a session, and revoked entries authorise neither.

### Remaining

Phase 2 (real audio) is next. Explicit admission/removal and speaking indicators
belong with phase 6. Service supervision is phase 7.
