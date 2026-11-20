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

---

## Progress update: real audio (phase 2)

Microphone and speaker audio are implemented and working on Linux.

### Delivered

| Item | Evidence |
|---|---|
| miniaudio backend (PipeWire/Pulse, WASAPI, CoreAudio) | `internal/audio/miniaudio.go` |
| Device enumeration with defaults | `voxen devices`, `TestDevicesEnumeration` |
| Live capture, 48 kHz mono normalisation | `TestCaptureProducesFramesFromRealDevice` |
| Live playback with channel expansion | `TestPlaybackAcceptsFrames` |
| cgo-free builds still work, reporting audio unavailable | `internal/audio/backend_nocgo.go` |
| Capture fanned out to every peer, peers mixed to speaker | `internal/daemon/audio_bridge.go` |
| Mute sends silence so the timeline survives | `TestAudioBridgeMuteSendsSilence` |
| Clipping protection on the mix | `TestAudioBridgeClampPreventsClipping` |
| End-to-end call with real devices | `TestRealCallCarriesAudio` |

### Bugs found and fixed

1. **Double free on device teardown.** Both the stream and the engine called
   `ma_device_uninit`. Streams now own their device lifetime exclusively; the
   engine owns only the context.
2. **Playback loop read a field `Stop` clears.** The loops now receive their
   streams as parameters instead of re-reading mutable fields.
3. **Pion race on Stats.** `ICETransport.GetSelectedCandidatePair` reads the
   gatherer that `StartContext` writes. Candidate selection is now read from
   `GetStats`, which is internally synchronised, and that also lets relayed
   candidates be detected by URL.
4. **Send on a closed channel.** `Close` closed the receive channel while the RTP
   reader could be sending on it. The channel is now never closed; a separate
   `done` channel signals teardown.

### Not verified

Windows (WASAPI) and macOS (CoreAudio) audio compile in CI but have not been run
on real hardware. The backend is the same library with the same code path, which
is good evidence but not proof. Verify with `voxen devices` and a loopback call
on each platform before claiming support.

Echo cancellation and noise suppression are still unimplemented; the config keys
exist and `voxen doctor` will report them as unavailable rather than pretending.

---

## Progress update: the connectivity contract

Three defects found by auditing what the docs promised against what the code did.
All were in the same area and all were user-visible.

### 1. The documented failure never happened

`docs/security.md` promised that a failed direct path produces
`media: no direct peer-to-peer path`. `ErrNoDirectRoute` existed but was **never
returned**: a failed connection reported a generic "not connected" after a
hardcoded 30 seconds, and `network.ice_timeout_ms` was validated and then ignored.

Now the timeout is plumbed from configuration through the manager into the
session, and both the deadline path and Pion's `Failed` state report
`ErrNoDirectRoute` with the elapsed budget. The controller turns that into a
per-peer message explaining that voxen does not relay and the call cannot be
rescued by waiting.

Tests: `TestNoDirectPathIsReportedSpecifically` (asserts the error identity *and*
that the configured 300 ms timeout was honoured rather than a 30 s default),
`TestConnectTimeoutDefaultMatchesConfig` (media default agrees with the config
default), `TestNoDirectPathSurfacesToTheUser` (daemon boundary).

### 2. Silent echo-cancellation and noise-suppression

`audio.echo_cancellation` and `audio.noise_suppression` were accepted, defaulted
to false, and did nothing. A user enabling them would have believed their echo
was handled. Both now fail validation with an explanation. This is deliberate:
a rejected config is honest, a silently ignored one is not.

### 3. Documentation drift

`docs/security.md` and the README now name both of the above, including that a
speaker-based user will hear echo until AEC exists.

### Next

Service supervision (phase 7) is the highest-value remaining item: calls still
end when the terminal closes. Then speaking indicators and per-peer mute, then
AEC/NS.

---

## Progress update: service supervision (phase 7, Linux verified)

`voxen start` detaches a daemon that dies with the login session and never comes
back after a crash. `voxen service install` now puts voxend under the platform's
*per-user* supervisor, which is the difference between a tool you run and a tool
you have.

### Delivered

| Item | Evidence |
|---|---|
| `voxen service install/status/uninstall` | `internal/cli/service.go` |
| systemd user unit generation | `internal/service/systemd.go` |
| launchd LaunchAgent generation | `internal/service/launchd.go` |
| Windows logon task generation | `internal/service/windows.go` |
| Real systemd lifecycle test | `TestSystemdServiceLifecycle` (opt-in) |
| Unit content regression tests | `internal/service/service_test.go` |

### Recommendations that were wrong, and why

The packaging templates existed before this phase and had never been run. Both
planted assumptions failed on first use:

1. **`ProtectSystem=strict`** makes `/run/user/$UID` read-only inside the unit's
   mount namespace. The daemon could not create its IPC socket:
   `bind: read-only file system`. Removed, with an explanatory comment so it is
   not re-added.
2. **`ReadWritePaths` without the runtime directory** produced
   `status=226/NAMESPACE` and a restart loop, because the directory did not exist
   at start. The unit now pre-creates it via `ExecStartPre` and lists it.

A third issue was found by the integration test rather than the unit: the daemon
started with the environment, then read config by path, but the *socket* came from
`VOXEN_RUNTIME_DIR`. The supervisor does not inherit the shell, so it and the CLI
disagreed about the endpoint — the service was running while `voxen status` said it
was not. The install command now bakes the resolved profile variables into the
unit.

### Verified on Linux, unverified elsewhere

`VOXEN_TEST_SYSTEMD=1 go test ./tests/integration/ -run TestSystemdServiceLifecycle`
installs a real unit, checks it is enabled and running, kills the daemon and
confirms systemd restarts it with a new PID, then uninstalls and confirms nothing
is left. It passes here.

macOS and Windows generate correct-looking units covered by content tests, but
neither has been run under a real supervisor. Treat them as unproven until someone
runs `voxen service install` on those machines.

---

## Progress update: speaking indicators and per-peer mute (phase 6)

The two fields a call UI needs were previously fake: `PeerState.Speaking` was
defined and never set, and `PeerState.Muted` had no per-peer meaning. Both are now
real, so a UI built on top of the IPC peer listing tells the truth.

### Delivered

| Item | Evidence |
|---|---|
| Speech detection from decoded peer audio | `isSpeech`, `TestSpeechDetectionThreshold` |
| Indicator lights and clears | `TestSpeakingIndicatorTracksPeerAudio` |
| Local per-peer mute in the mix | `TestPerPeerMuteSilencesOnlyThatPeer` |
| Muted peer still shown as speaking | `TestMutedPeerStillCountsAsSpeaking` |
| Flags reach the IPC peer listing | `TestPeerFlagsReachIPC` |
| `voxen peer-mute <id>` / `peer-mute off <id>` | `internal/cli/rooms.go` |
| Per-peer state cleared on leave | `SetPeerGoneHook` + `forgetPeer` |

### Decisions worth recording

- **Speech detection is an RMS gate at roughly -40 dBFS.** Below normal speech and
  above room noise, tested against silence, quiet noise, keyboard clicks and two
  speech levels. It is deliberately conservative: a flickering indicator is worse
  than none.
- **A muted peer still shows as speaking.** The indicator reflects whether they are
  talking, not whether you can hear them. Measuring after the local mute would make
  the UI claim nobody is speaking while someone plainly is.
- **Per-peer mute is a playback control, not a transmission one.** The peer keeps
  sending, and voxen keeps reading their stream so it does not back up; the audio
  simply never reaches the speaker.
- **Peer state is cleared when someone leaves.** Without the hook, a returning peer
  inherited a stale speaking indicator from their previous session.

## Progress update: the TUI

`voxen tui` renders the live call. Built with the four requested libraries, each used
where it genuinely fits rather than bolted on.

| Library | Use |
|---|---|
| Bubble Tea | event loop, alternate screen, key dispatch |
| Lip Gloss | styles and alignment |
| Bubbles | key bindings (`key`) and the help component (`help`) |
| Glamour | the long-form help page (`voxen tui --help-page`) |

### Structure

The package separates rendering from the framework. `ViewModel` holds a snapshot and
turns it into text; `Model` is the Bubble Tea model that polls and forwards keys.
That split is why the view has 12 tests that assert on what a user would read without
standing up a terminal, a daemon or a PTY.

Bubbles' key bindings are the single source of truth for input matching, the short
help line and the full help panel. `TestAllDocumentedKeysAreHandled` walks the same
`KeyMap` the UI uses, so a key cannot work without being documented or be documented
without working.

### Decisions worth recording

- **The TUI is a viewer and remote control, never an owner.** `q` exits the view; the
  daemon keeps the call running for everyone else. The help text says so explicitly,
  because a user who believes otherwise will not close it.
- **A failed poll keeps the last good state on screen** with an "unreachable" badge
  rather than blanking it, so a momentary IPC hiccup does not look like the call
  ended.
- **The PATH column claims nothing before connection.** `direct` is only shown once a
  connection exists; before that it is `-`, because voxen has no relay and implying a
  path would be a guess.
- **Speaking is only shown for connected peers**, for the same reason: an indicator
  on a peer with no media path is a claim the daemon cannot support.
- **Polling is one second.** Measured at ~6 ms per status+peers round trip, so the
  cost is negligible, and audio counters do not change meaningfully faster.

### Verified

Rendered against a real two-daemon call: participants, `direct` path, and audio
counters climbing live (`sent 602 · received 603`). All 27 TUI tests pass under
`-race`; the view, the key handling, and the Glamour help page are covered.

### Caveat

Interactive key delivery could not be exercised through `script(1)` in this
environment because piped stdin is not delivered as Bubble Tea key events. Key
handling is covered by unit tests driving `tea.KeyMsg` directly, which tests the same
code path Bubble Tea calls, but a human has not yet pressed the keys in a real
terminal.

---

## Progress update: echo cancellation and noise suppression (phase 8, opt-in build)

Implemented as an **opt-in build variant**, because it is the one dependency that does
not belong in the default build.

### Why opt-in

Echo cancellation needs SpeexDSP, which is cgo plus a system header. Making it a
default dependency would break the dependency-free `CGO_ENABLED=0` cross-build that
`make cross` and CI rely on for six targets. So:

- Default build: no preprocessing, and `audio.echo_cancellation: true` is a
  configuration error that explains how to get it.
- `-tags speex` (`make build-speex`): real echo cancellation and noise suppression.

Reporting is honest in both cases: `voxen status` shows a `processing` line naming
what is actually running, and `voxen doctor` has an `audio-processing` check that
distinguishes "not compiled in", "compiled but disabled", and "active".

### Measured, not assumed

The significant finding is that SpeexDSP offers two ways to cancel echo and they
differ by an order of magnitude:

| Route | Cancellation on a simulated echo path |
|---|---|
| `speex_echo_cancellation` directly | **68 dB** |
| canceller via `speex_preprocess`'s echo state | 15 dB |

Both run without error and both "work" by any smoke test. The weaker path would have
shipped unnoticed, and the symptom - "echo cancellation is on but I can still hear
myself" - is exactly the kind of thing a user cannot debug. The direct path is used,
and `TestEchoCancellationRemovesSimulatedEcho` asserts at least 20 dB so a regression
to the weaker behaviour fails the build.

A second silent trap is recorded in the code: calling `ec.Playback` for the reference
*and* letting the preprocessor feed it corrupts the adaptive filter, dropping
cancellation to 15 dB with no error. The reference is now supplied exactly once, per
call, to `Cancellation`.

### Integration

The playback loop records the frame it is sending to the speaker as the far-end
reference; the capture loop supplies that reference to the canceller and filters the
microphone frame before encoding. Without this wiring the processor would run and do
nothing, which `TestBridgeReferenceReachesProcessor` guards against.

### Bugs found while wiring it

- A re-entrant lock in `Start`: the processor was built while the bridge mutex was
  already held, deadlocking the first `Start`. Second re-entrant deadlock in this
  file, which is a sign the locking deserves a cleanup pass.
- `PushReference` early-returned when only echo cancellation was enabled, so the
  reference never arrived and cancellation was inert.

### Not verified on real hardware

The 68 dB figure is from a simulated acoustic path: a tone played, attenuated and
delayed one frame. Real echo paths have delay, reverb and nonlinearity, and a real
speaker-to-microphone loop will cancel less. **Nobody has yet made a speaker-based
call with a microphone open and confirmed their own voice is inaudible.** That is the
test that matters, and it needs a person and a room.
