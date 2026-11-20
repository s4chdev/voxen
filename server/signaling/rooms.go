package signaling

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/s4chdev/voxen/internal/invite"
	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// dispatch routes one authenticated peer's message.
//
// Relay envelopes (SDP, ICE) are forwarded verbatim to the addressed peer or the
// room. The server does not inspect their payload beyond reading `to` and
// `room` for routing.
func (s *Server) dispatch(p *peerConn, env protocol.Envelope) error {
	if env.Version != protocol.ProtocolVersion {
		return errors.New("unsupported protocol version")
	}
	switch env.Type {
	case protocol.TypeRoomCreate:
		return s.createRoom(p, env)
	case protocol.TypeRoomJoin:
		var payload struct {
			RoomID string `json:"room_id"`
			Code   string `json:"code"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return errors.New("room.join requires room_id or code")
		}
		roomID := payload.RoomID
		if payload.Code != "" {
			// A code is what a person shares; the server maps it to a room.
			r, ok := s.roomByCode(payload.Code)
			if !ok {
				return errors.New("room not found for that code")
			}
			roomID = r.id
		}
		if roomID == "" {
			return errors.New("room.join requires room_id or code")
		}
		return s.joinRoom(p, roomID, false)
	case protocol.TypeRoomLeave:
		p.leave()
		return nil
	case protocol.TypeOffer, protocol.TypeAnswer, protocol.TypeICECandidate, protocol.TypeICEEnd, protocol.TypeRoster, protocol.TypeAdmit, protocol.TypeRemove:
		return s.relay(p, env)
	case protocol.TypePing:
		p.enqueue(protocol.Envelope{Version: protocol.ProtocolVersion, Type: protocol.TypePong, SentAt: time.Now().UTC()})
		return nil
	case protocol.TypePong:
		return nil
	case protocol.TypePeerJoin, protocol.TypePeerLeave, protocol.TypeWelcome, protocol.TypeRoomState, protocol.TypeError:
		return errors.New("client may not send server-originated message types")
	default:
		return fmt.Errorf("unknown message type %q", env.Type)
	}
}

func (s *Server) createRoom(p *peerConn, env protocol.Envelope) error {
	s.mu.Lock()
	if len(s.rooms) >= s.cfg.MaxRooms {
		s.mu.Unlock()
		return errors.New("server is at room capacity")
	}
	// nextIDLocked is used rather than nextID: createRoom already holds s.mu, and
	// nextID would deadlock trying to take it again.
	roomID := s.nextIDLocked("room")
	code, codeErr := invite.Generate(roomID)
	if codeErr != nil {
		s.mu.Unlock()
		return fmt.Errorf("cannot generate room code: %w", codeErr)
	}
	s.rooms[roomID] = &roomState{
		id:         roomID,
		createdAt:  time.Now().UTC(),
		creator:    p.id,
		members:    make(map[string]*peerConn),
		code:       code.Room,
		creatorKey: p.publicKey,
	}
	s.mu.Unlock()

	if err := s.joinRoom(p, roomID, false); err != nil {
		return err
	}
	// Tell the creator the room ID before the roster, so a client can render it
	// even if the roster is still empty.
	// Include the code and creator key so the creator can display the code and a
	// joiner can verify it against the key it was handed.
	p.enqueue(protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypeRoomState,
		Room:    roomID,
		Payload: mustJSON(protocol.RoomStatePayload{
			RoomID:     roomID,
			CreatedAt:  time.Now().UTC(),
			Members:    s.roomSnapshot(roomID),
			Code:       code.Room,
			CreatorKey: p.publicKey,
		}),
		SentAt: time.Now().UTC(),
	})
	return nil
}

func (s *Server) joinRoom(p *peerConn, roomID string, fromHello bool) error {
	s.mu.Lock()
	r, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return errors.New("room not found")
	}
	if _, already := r.members[p.id]; already {
		s.mu.Unlock()
		return nil
	}
	if len(r.members) >= s.cfg.MaxMembers {
		s.mu.Unlock()
		return errors.New("room is full")
	}
	r.members[p.id] = p
	p.roomID = roomID
	members := make([]protocol.PeerInfo, 0, len(r.members))
	for id, m := range r.members {
		members = append(members, protocol.PeerInfo{
			PeerID:      id,
			DisplayName: m.displayName,
			PublicKey:   m.publicKey,
			JoinedAt:    m.joinedAt,
		})
	}
	s.mu.Unlock()

	// The joining peer learns the full roster; existing members learn only the
	// delta. Both are routing information, not trust decisions.
	// Send the code and creator key with the roster: this is the blob a joiner
	// verifies against the code they typed.
	s.mu.Lock()
	roomCode := ""
	creatorKey := ""
	if r, ok := s.rooms[roomID]; ok {
		roomCode = r.code
		creatorKey = r.creatorKey
	}
	s.mu.Unlock()

	p.enqueue(protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypeRoomState,
		Room:    roomID,
		Payload: mustJSON(protocol.RoomStatePayload{
			RoomID:     roomID,
			Members:    members,
			Code:       roomCode,
			CreatorKey: creatorKey,
		}),
		SentAt: time.Now().UTC(),
	})
	s.broadcast(roomID, protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypePeerJoin,
		Room:    roomID,
		From:    p.id,
		Payload: mustJSON(protocol.PeerInfo{
			PeerID:      p.id,
			DisplayName: p.displayName,
			PublicKey:   p.publicKey,
			JoinedAt:    p.joinedAt,
		}),
		SentAt: time.Now().UTC(),
	}, p.id)

	s.log.Info("peer joined room", "peer", p.id, "room", roomID, "members", len(members), "from_hello", fromHello)
	return nil
}

// relay forwards a control message. It validates routing only.
func (s *Server) relay(p *peerConn, env protocol.Envelope) error {
	s.mu.Lock()
	if p.roomID == "" {
		s.mu.Unlock()
		return errors.New("not in a room")
	}
	if env.Room != "" && env.Room != p.roomID {
		s.mu.Unlock()
		return errors.New("room does not match the sender's room")
	}
	r, ok := s.rooms[p.roomID]
	if !ok {
		s.mu.Unlock()
		return errors.New("room no longer exists")
	}
	// A peer may only address another member of the same room. This prevents a
	// participant from injecting control messages into an unrelated call.
	var target *peerConn
	if env.To != "" {
		target, ok = r.members[env.To]
		if !ok {
			s.mu.Unlock()
			return errors.New("target peer is not in this room")
		}
	}
	roomID := p.roomID
	s.mu.Unlock()

	out := protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    env.Type,
		ID:      env.ID,
		Room:    roomID,
		// `from` is assigned by the server, never copied from the client, so a
		// peer cannot spoof another peer's routing ID.
		From:    p.id,
		To:      env.To,
		Payload: env.Payload,
		SentAt:  time.Now().UTC(),
	}
	if target != nil {
		target.enqueue(out)
		return nil
	}
	s.broadcast(roomID, out, p.id)
	return nil
}

// broadcast sends env to every member of roomID except skip.
func (s *Server) broadcast(roomID string, env protocol.Envelope, skip string) {
	s.mu.Lock()
	r, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return
	}
	targets := make([]*peerConn, 0, len(r.members))
	for id, m := range r.members {
		if id == skip {
			continue
		}
		targets = append(targets, m)
	}
	s.mu.Unlock()

	for _, t := range targets {
		t.enqueue(env)
	}
}

// roomByCode resolves a short code to a room. It is how a joiner turns the code a
// person read out into something the server can route to.
func (s *Server) roomByCode(code string) (*roomState, bool) {
	// Normalise the way a person might type it: case, spaces and dashes are not
	// meaningful, and the check portion is not part of the server-side lookup.
	parsed, err := invite.Parse(code + "0000")
	if err == nil {
		code = parsed.Room
	}
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rooms {
		if r.code == code {
			return r, true
		}
	}
	return nil, false
}
