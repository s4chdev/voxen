# voxen — Security Properties and Limitations

This document states exactly what voxen protects, what it does not, and what has
actually been implemented and tested in the current build. It is normative: if a
claim here is not backed by code and a test, the claim is wrong and must be
removed.

**Current build: phase 1.** Identity, signed statements, DTLS binding logic,
replay protection and roster authentication are implemented and unit-tested.
Audio transport and the runtime DTLS binding are **not** implemented yet, so
voxen does **not** currently carry voice calls. It does not claim to.

---

## 1. Threat model

### In scope

| Adversary | Capability | What voxen must prevent |
|---|---|---|
| Malicious signaling server | Read, drop, reorder, forge, replay all control messages | Inserting itself into the media path; admitting an unverified peer; replaying old control state |
| Network attacker | Observe, inject, replay UDP (including DTLS handshake) | Reading or injecting media; substituting a DTLS endpoint |
| Malicious room participant | Hold a valid identity, send arbitrary control messages | Impersonating another participant; injecting audio into another peer's session |
| Other local users | Run processes on the same machine | Reading keys, driving the daemon, reading the trust store |
| Curious peer | Legitimate participant | Reading media not addressed to it |

### Out of scope

- A compromised local account or a keylogger. Identity protection ends at the
  filesystem and the local OS.
- A compromised audio device or kernel driver.
- Traffic analysis of call metadata (who called whom, when, packet timing) by
  an observer on the signaling path or the network. Voice activity can be
  inferred from packet timing **even with DTX disabled**.
- Availability. A signaling server can always deny service.
- Hardware attestation. A software identity says nothing about the machine.

---

## 2. Identity

- **Algorithm:** Ed25519 (`crypto/ed25519`). No custom cryptography anywhere in
  the codebase. Fingerprints are SHA-256 over the 32-byte public key.
- **Display form:** 64 hex characters in 16 groups of four, uppercase. Parsing
  accepts spaces, colons, hyphens and mixed case so operators can copy the form
  their peer read out.
- **Storage:** `<config dir>/identity.json`, `0600`, written through a temporary
  file, `fsync`, then rename. The daemon refuses to load a key file with looser
  permissions on Unix.
- **Optional encryption at rest:** when `VOXEN_KEY_PASSPHRASE` is set, the 32-byte
  seed is sealed with XChaCha20-Poly1305 under a 32-byte key derived with
  scrypt (N=2^15, r=8, p=1, 16-byte salt, 24-byte nonce). Public key,
  fingerprint and creation timestamp are authenticated as associated data, so an
  attacker with write access cannot swap key material between files.
- **Passphrase handling:** read from the environment only. There is no
  `--passphrase` flag, because command line arguments are visible in process
  listings, and no hidden prompt, because a daemon has no terminal and a hidden
  prompt is not auditable. The trade-off — environment variables can be visible
  to the same user's other processes on some platforms — is reported by
  `voxen doctor`.
- **Creation is not silent rotation:** `voxen init` refuses to overwrite an
  existing identity. Rotation requires `--force` and prints the retired
  fingerprint.

### Rotation and revocation

- **Identity rotation** (`voxen init --force`) generates a new key pair. Every peer
  must re-verify the new fingerprint; the old fingerprint is reported so it can
  be recorded. There is no automatic trust propagation — that is intentional.
- **Peer revocation** (`voxen revoke <fingerprint>`) sets a sticky `revoked`
  level. A revoked peer is refused even if it would otherwise be unverified, and
  its record is retained rather than deleted so audit history survives and the
  key cannot silently reappear as merely unverified.
- **Session statements** are short-lived (2 minutes by default) and bound to one
  room, one session and one DTLS certificate, so a leaked statement has a small
  window and no cross-session use.

---

## 3. Trust and admission

voxen's central rule: **a peer may only receive media after its fingerprint has
been verified out of band and its negotiated DTLS certificate has been bound to
that fingerprint.**

- The trust store (`trust.json`, `0600`) is the only source of admission. A peer
  introduced by the signaling server is recorded as `unverified` at most.
- Trust is anchored on the **fingerprint**, the same way SSH anchors on a host
  key. `voxen verify <fingerprint>` can therefore be run before the peer has ever
  been seen: it records a *pin*. The key presented during the first session must
  hash to that fingerprint (`TrustStore.BindKey`), or the call is refused. A
  different key never takes over a pinned fingerprint, because it hashes to a
  different value and gets its own unverified record.
- A pin is displayed as `fingerprint only` by `voxen trust` until a key is bound;
  after binding the entry carries the key and the column shows `key`.
- If the key was already observed, `voxen verify` verifies the key directly and
  the stored key must still match the anchor fingerprint (checked on load, so a
  hand-edited trust store is rejected).
- The room state machine marks an unverified peer as `failed` with an explicit
  reason and never transitions it to `connected`.
- `security.require_verified_peers: false` is rejected at configuration load.
  There is no supported way to disable identity verification.
- Room rosters are signed by the room administrator's Ed25519 key. A joining peer
  must present a roster that (a) verifies under the pinned administrator key,
  (b) is current and within its validity window, (c) has a sequence number not
  lower than the last applied, (d) lists the peer itself, and (e) contains no
  duplicate keys. A removed participant is torn down when the new roster arrives.

---

## 4. The identity-to-DTLS binding

This is the mechanism that makes "end-to-end encrypted" meaningful despite an
untrusted signaling server.

1. Each peer generates a DTLS certificate (Pion) and computes its SHA-256.
2. It builds a `SessionStatement`:

   ```
   version | room_id | session_id | ed25519_pubkey | sha256(dtls_cert)
          | 16-byte nonce | issued_at | expires_at
   ```

   encoded deterministically with a domain-separation prefix
   (`voxen/v1/session-statement`) and signed with its Ed25519 key.
3. The statement and signature travel with the SDP offer/answer. The signaling
   server relays them opaquely.
4. The receiver verifies: signature → issuer == the roster/trust identity for
   this peer → room and session match → within lifetime and clock skew →
   nonce not seen → issue time not older than the last accepted statement from
   this key.
5. After the DTLS handshake, the receiver hashes the **negotiated** certificate
   and requires an exact match with the signed fingerprint. Only then is audio
   attached.

A signaling server can rewrite the SDP fingerprint, but then the signature check
fails. It can relay the correct statement and terminate DTLS itself with a
different certificate, but then the DTLS bind fails. It cannot produce a
statement signed by a peer key it does not hold.

**Tested in this build** (`internal/security`, `tests/security`):

| Scenario | Test |
|---|---|
| Valid statement and binding accepted | `TestSessionStatementHappyPath` |
| Modified SDP fingerprint rejected | `TestModifiedSDPFingerprintRejected`, `TestModifiedFingerprintIsRejected` |
| Statement from an unexpected identity rejected | `TestStatementFromUnexpectedIssuerRejected`, `TestInvalidIdentityIsRejected` |
| Expired / future statement rejected | `TestStatementExpiryAndFuture` |
| Cross-room / cross-session replay rejected | `TestStatementRejectsRoomAndSessionMismatch` |
| Overlong validity window rejected | `TestStatementRejectsOverlongLifetime` |
| Exact replay rejected | `TestReplayGuardRejectsExactReplay`, `TestReplayIsRejected` |
| Older statement (rollback) rejected | `TestReplayGuardRejectsRollback` |
| Replay guard bounded under load | `TestReplayGuardIsBounded` |
| Canonical encoding deterministic and domain-separated | `TestCanonicalEncodingIsStableAndDomainSeparated` |
| Forged administrator roster rejected | `TestRoomMembershipRejectsForgedAdmin` |
| Roster rollback rejected | `TestRoomMembershipSequenceRollback`, `TestApplyRosterRejectsRollbackAndForgery` |
| Non-member admission rejected | `TestRoomMembershipVerification` |
| Unverified peer never connected | `TestUnverifiedPeerIsNotConnected` |
| Fingerprint pin binds only the matching key | `TestFingerprintPinThenBind` |
| Revoked pin cannot be completed | `TestRevokedPinCannotBeBound` |
| Observation never downgrades trust | `TestObserveDoesNotDowngradeOrRebind` |
| Hand-edited trust store key/fingerprint mismatch rejected | `TestTrustStoreRejectsFingerprintKeyMismatch` |

**Not yet tested** because the runtime path does not exist yet: the binding
against a live Pion handshake. That is the phase 3 gate and voxen will not call a
call encrypted until it passes.

---

## 5. Media encryption

Once negotiated, audio is carried in SRTP with keys derived from the DTLS
handshake (DTLS-SRTP), which is what WebRTC mandates. voxen never implements a
cipher itself.

- The required path is `AES-128-CM` / AEAD SRTP profiles negotiated by Pion.
- cleartext SDP carries no media and no key material beyond the DTLS
  fingerprint, which is authenticated separately as described above.
- RTCP feedback (NACK, PLI, REMB) is multiplexed and protected by SRTCP.
- There is no fallback to plain RTP and no code path that disables DTLS.

---

## 6. Direct connectivity, no relay

- ICE uses host candidates and the configured STUN servers (maximum four) to
  discover public addresses.
- **TURN is not implemented.** The configuration schema has no TURN field, and an
  unknown `turn_servers` key fails validation (`TestParseRejectsTurnAndRelayConfig`).
- If no direct path succeeds within the ICE timeout, the session fails with
  `media: no direct peer-to-peer path; voxen does not relay media through TURN`.
  It does not degrade silently.
- Consequence: peers behind symmetric NAT on both sides may not be able to call
  each other. This is a deliberate property, not a bug.

The selected ICE pair is reported through session stats, and the `Relayed` field
asserts that no relay was used, so a future regression that reintroduces TURN is
detectable.

---

## 7. Local attack surface

| Surface | Protection |
|---|---|
| IPC socket | `0600` in a `0700` directory; same-UID peer credential check per connection |
| Windows pipe | security descriptor: current user, SYSTEM, Administrators; network access denied |
| Key files | `0600`, atomic write, permission check on load |
| Trust store | `0600`, atomic write, permission check on load |
| Config | `0600`; rejected invalid/unsafe values |
| IPC framing | 1 MiB cap, JSON only, version checked, request IDs correlated |
| Process arguments | no secrets are accepted on the command line |
| Logs | structured; never include key material, session keys or full SDP |

A second daemon cannot hijack a live endpoint: starting a daemon while another
holds the socket fails with "already in use", and a stale socket is removed only
after confirming nobody is listening (`TestSecondDaemonIsRefused`).

---

## 8. Known limitations (explicit)

1. **No audio or media in this build.** Phase 2 and 3 deliver them. Until a live
   two-peer call with the DTLS binding has been tested, voxen makes no E2EE claim
   about calls.
2. **Metadata is public to the signaling server:** room IDs, participant
   identities, IP addresses observed by the server, and call timing. voxen does not
   attempt metadata privacy.
3. **Packet timing reveals speech activity** even with Opus DTX enabled.
4. **Passphrase via environment** can be visible to same-user processes on some
   platforms. `voxen doctor` warns.
5. **No forward secrecy beyond the DTLS session.** Each call has fresh DTLS keys,
   but a compromise of the long-term Ed25519 key allows impersonation in future
   sessions (not decryption of past media). There is no post-compromise
   security signal.
6. **Group calls are a full mesh** capped at 16 (default 5). A participant can
   observe that a call exists and who is in it.
7. **macOS microphone permission** is not yet requested by a headless daemon;
   the TCC prompt belongs to the phase 2 audio work, and the `Info.plist`
   usage description ships with the bundle packaging in phase 8.
8. **Windows service management** and launchd/systemd units are phase 7.
9. **No independent audit.** The protocol and implementation have not been
   reviewed by a third party. The test matrix above is the current evidence.

---

## 9. Reporting a vulnerability

Do not open a public issue for a security defect. Send a description and, where
possible, a minimal reproduction to the maintainers through the project's private
security contact. Include the build phase reported by `voxen version`.
