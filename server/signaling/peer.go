package signaling

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// peerConn is one connected client.
type peerConn struct {
	id          string
	publicKey   string
	displayName string
	joinedAt    time.Time
	roomID      string

	conn *websocket.Conn
	send chan protocol.Envelope

	// closeOnce guards teardown between the reader and writer goroutines.
	closeOnce sync.Once
	server    *Server
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	// The signaling protocol authenticates at the application level with signed
	// statements, but the browser origin check is still kept strict for
	// deployments that expose this port to browsers by mistake.
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		// Non-browser clients send no Origin. Allow those; a browser Origin that
		// is present must match the Host.
		if origin == "" {
			return true
		}
		host := r.Host
		return origin == "http://"+host || origin == "https://"+host
	},
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Debug("websocket upgrade failed", "error", err)
		return
	}

	p := &peerConn{
		conn:   conn,
		send:   make(chan protocol.Envelope, 64),
		server: s,
	}

	// Bound how long a client may stay silent before its first hello.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	// The first frame must be hello. Reading it before admitting the peer keeps
	// anonymous connections out of the room table entirely.
	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		p.close()
		return
	}
	if err := s.admit(p, env); err != nil {
		_ = p.writeError("", protocol.CodeUnauthorized, err.Error())
		p.close()
		return
	}

	go p.writeLoop()
	p.readLoop()
}

// admit validates the hello frame and registers the peer.
func (s *Server) admit(p *peerConn, env protocol.Envelope) error {
	if env.Version != protocol.ProtocolVersion {
		return errors.New("unsupported protocol version")
	}
	if env.Type != protocol.TypeHello {
		return errors.New("first frame must be hello")
	}
	var hello protocol.Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		return errors.New("malformed hello payload")
	}
	if hello.PublicKey == "" {
		return errors.New("hello is missing a public key")
	}
	if len(hello.DisplayName) > 64 {
		return errors.New("display name is too long")
	}
	// Note what is NOT checked here: the server does not verify the Ed25519
	// signature over the peer's session statements. It cannot, because it never
	// sees the DTLS certificates. Authenticating a peer is entirely the clients'
	// job. This server only assigns a routing ID.
	p.id = s.nextID("peer")
	p.publicKey = hello.PublicKey
	p.displayName = hello.DisplayName
	p.joinedAt = time.Now().UTC()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("server is shutting down")
	}
	s.peers[p.id] = p
	s.mu.Unlock()

	_ = p.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	p.enqueue(protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypeWelcome,
		Payload: mustJSON(protocol.Welcome{
			ProtocolVersion: protocol.ProtocolVersion,
			PeerID:          p.id,
			ServerName:      "voxen-signaling",
		}),
		SentAt: time.Now().UTC(),
	})
	s.log.Info("peer admitted", "peer", p.id, "display_name", p.displayName)

	if hello.RoomID != "" {
		return s.joinRoom(p, hello.RoomID, true)
	}
	return nil
}

func (p *peerConn) readLoop() {
	defer p.leave()
	for {
		_ = p.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		var env protocol.Envelope
		if err := p.conn.ReadJSON(&env); err != nil {
			return
		}
		if err := p.server.dispatch(p, env); err != nil {
			_ = p.writeError(env.ID, protocol.CodeBadRequest, err.Error())
		}
	}
}

func (p *peerConn) writeLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case env, ok := <-p.send:
			if !ok {
				return
			}
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteJSON(env); err != nil {
				p.close()
				return
			}
		case <-ticker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				p.close()
				return
			}
		}
	}
}

// enqueue adds a message to the peer's send queue, dropping it if the client is
// not keeping up. A dropped control message surfaces as a failed negotiation on
// the client, which is correct: silently buffering forever would be worse.
func (p *peerConn) enqueue(env protocol.Envelope) {
	select {
	case p.send <- env:
	default:
		p.server.log.Warn("dropping control message for slow peer", "peer", p.id, "type", env.Type)
	}
}

func (p *peerConn) writeError(id, code, message string) error {
	p.enqueue(protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypeError,
		ID:      id,
		Payload: mustJSON(protocol.ErrorPayload{Code: code, Message: message}),
		SentAt:  time.Now().UTC(),
	})
	return nil
}

func (p *peerConn) close() {
	p.closeOnce.Do(func() {
		_ = p.conn.Close()
	})
}

// leave removes the peer from its room and the registry.
func (p *peerConn) leave() {
	p.close()
	s := p.server

	s.mu.Lock()
	if p.roomID != "" {
		if r, ok := s.rooms[p.roomID]; ok {
			delete(r.members, p.id)
			if len(r.members) == 0 {
				delete(s.rooms, p.roomID)
			}
		}
	}
	delete(s.peers, p.id)
	roomID := p.roomID
	s.mu.Unlock()

	if roomID != "" {
		s.broadcast(roomID, protocol.Envelope{
			Version: protocol.ProtocolVersion,
			Type:    protocol.TypePeerLeave,
			Room:    roomID,
			From:    p.id,
			SentAt:  time.Now().UTC(),
		}, p.id)
		s.log.Info("peer left room", "peer", p.id, "room", roomID)
	}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
