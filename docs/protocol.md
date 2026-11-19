# voxen — Signaling Protocol (v1)

Status: **defined in phase 1, transport implemented in phase 4.** The types in
`internal/signaling` are the normative Go representation. The server relays
payloads opaquely; it never parses SDP, never sees audio and cannot authenticate
a peer on its own.

## 1. Transport

- WebSocket, `ws://` for local development and `wss://` in production.
- One JSON object per text frame, UTF-8, no binary frames.
- `v` is the protocol version. A server rejects an unknown version with a
  close code and an `error` envelope.
- The default endpoint is `/ws`; the URL comes from `signaling.url`.

## 2. Envelope

```json
{
  "v": 1,
  "type": "sdp.offer",
  "id": "a3f1",
  "room": "room-7b2c",
  "from": "peer-12",       // server-assigned; a routing hint, never auth
  "to": "peer-08",         // empty = broadcast to the room
  "payload": { },
  "sent_at": "2026-11-19T17:00:00Z"
}
```

`from` is **not** an identity. Authentication is always cryptographic, using the
signed statements inside `payload`. Clients must never make a trust decision
based on `from`.

## 3. Handshake

1. Client connects, then sends `hello`:

   ```json
   {
     "v": 1, "type": "hello",
     "payload": {
       "protocol_version": 1,
       "peer_id": "peer-08",
       "display_name": "alice",
       "public_key": "<base64 Ed25519>",
       "proof": "<base64 signature>"
     }
   }
   ```

   `proof` signs the domain-separated transcript
   `voxen/v1/signaling-hello | nonce | peer_id | public_key`, where `nonce` is the
   challenge from the server's `welcome`. This proves key possession but grants
   **no** trust to other participants; it only deters casual impersonation of a
   peer ID on the signaling bus.

2. Server replies `welcome` containing the assigned `peer_id`, the `challenge`,
   and advisory `stun_servers`. Clients ignore server-provided STUN servers
   unless their local configuration allows it: a server must not be able to
   redirect ICE traffic.

## 4. Room operations

| Type | Direction | Purpose |
|---|---|---|
| `room.create` | client → server | Create a room; server returns `room.state` |
| `room.join` | client → server | Join by ID with a signed roster/invite |
| `room.leave` | client → server | Leave and notify peers |
| `room.state` | server → client | Authoritative roster snapshot + signed roster |
| `peer.join` / `peer.leave` | server → client | Membership delta |
| `roster` | client → client | Administrator-signed membership update |
| `admit` / `remove` | creator → peers | Explicit admission decision |
| `ping` / `pong` | both | Liveness; 20 s interval, 3 missed pings = closed |

`room.state` carries `roster` (the signed document as raw JSON) and
`roster_signature`. Clients verify both before acting; a server that forges a
roster produces a signature failure at the client.

## 5. Session negotiation

`SDP` and ICE are relayed with the peer's signed identity statement attached:

```json
{
  "v": 1, "type": "sdp.offer", "room": "room-7b2c", "to": "peer-08",
  "payload": {
    "type": "offer",
    "sdp": "v=0\r\n...",
    "session_id": "s-9d3e",
    "statement": { "...SessionStatement fields..." },
    "signature": "<base64 Ed25519 signature over canonical statement bytes>",
    "ice_ufrag": "8Fh2",
    "ice_pwd": "k2..."
  }
}
```

`ice.candidate` carries one trickled candidate; `ice.end` signals end of
candidates. The receiver applies the statement verification and DTLS binding
described in [security.md](security.md) before attaching media.

ICE restart is a fresh `sdp.offer` with a new `session_id` and a new statement.
The old session's statements expire naturally; the replay guard also refuses a
reused nonce.

## 6. Errors

```json
{ "v": 1, "type": "error", "id": "a3f1",
  "payload": { "code": "room_not_found", "message": "no such room" } }
```

Codes: `bad_request`, `unauthorized`, `room_not_found`, `room_full`,
`not_member`, `protocol_violation`, `version_mismatch`, `internal`.

## 7. Server responsibilities

The reference server (phase 4) does exactly this:

1. Accept WebSocket connections, verify `hello` proofs.
2. Track rooms and memberships in memory; persist nothing by default.
3. Relay envelopes to the addressed peer or the whole room.
4. Never buffer SDP, never store audio, never terminate DTLS.
5. Rate-limit envelopes per connection to bound resource use.
6. Expire empty rooms and idle connections.

It is intentionally simple so it can be self-hosted on a small VPS and audited
in one sitting.

## 8. Compatibility

`v` is a major version. Additive payload fields are allowed within a version;
removing or reinterpreting a field requires `v+1`. Clients must ignore unknown
payload fields but reject unknown envelope versions.
