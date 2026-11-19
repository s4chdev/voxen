// Package signaling will implement the self-hosted voxen signaling server.
//
// Phase 4 delivers a single-binary WebSocket server that:
//
//   - verifies the hello proof for each connection,
//   - tracks rooms and memberships in memory,
//   - relays SDP and ICE envelopes verbatim between peers,
//   - enforces per-connection rate limits and idle expiry,
//   - stores nothing and never handles audio.
//
// The wire protocol and the server's obligations are specified in
// docs/protocol.md. Phase 1 ships this package as documentation only; there is no
// listening server in this build, and `voxen status` reports signaling as
// disconnected rather than pretending otherwise.
package signaling

// ProtocolVersion re-exports the client protocol version so the server and client
// cannot drift.
const ProtocolVersion = 1

// Implemented reports whether this package contains a working server.
const Implemented = false
