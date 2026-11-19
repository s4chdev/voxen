// Package signaling implements the self-hosted voxen signaling server.
//
// The server coordinates rooms and relays opaquely-encrypted control messages.
// It is deliberately incapable of authenticating peers on their behalf: SDP and
// ICE travel with signed statements it cannot forge, and it never sees audio.
//
// Security-relevant properties this implementation must preserve:
//
//   - Audio never traverses this server. It only relays JSON control messages.
//   - The server cannot read or alter the signed session statements without
//     detection; it relays them as opaque base64 payloads.
//   - A peer's `from` field is assigned by the server and is a routing hint,
//     never an authentication claim.
package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// Config configures a Server.
type Config struct {
	// Addr is the listen address, for example "127.0.0.1:8080".
	Addr string
	// MaxRooms bounds total rooms to keep memory use predictable.
	MaxRooms int
	// MaxMembers bounds the participants in a single room. The mesh protocol has
	// a hard cap of 16 in the room layer; the server enforces a lower ceiling so
	// a malicious client cannot build a huge room before the clients object.
	MaxMembers int
	// Logger receives structured logs.
	Logger *slog.Logger
}

// Server is a running signaling server.
type Server struct {
	cfg      Config
	log      *slog.Logger
	mu       sync.Mutex
	rooms    map[string]*roomState
	peers    map[string]*peerConn
	httpSrv  *http.Server
	listener net.Listener
	seq      uint64
	closed   bool
}

// roomState is the server's view of one room. It stores only routing metadata.
type roomState struct {
	id        string
	createdAt time.Time
	creator   string
	members   map[string]*peerConn
}

// New builds a server. It does not listen until Run is called.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("signaling: listen address is required")
	}
	if cfg.MaxRooms <= 0 {
		cfg.MaxRooms = 256
	}
	if cfg.MaxMembers <= 0 || cfg.MaxMembers > 16 {
		cfg.MaxMembers = 8
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:   cfg,
		log:   logger,
		rooms: make(map[string]*roomState),
		peers: make(map[string]*peerConn),
	}, nil
}

// Addr returns the bound address after Listen, useful when Addr used port 0.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Run listens and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("signaling: listen %s: %w", s.cfg.Addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	s.log.Info("signaling server listening", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
		s.shutdown()
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) shutdown() {
	s.mu.Lock()
	s.closed = true
	srv := s.httpSrv
	peers := make([]*peerConn, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()

	for _, p := range peers {
		p.close()
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// roomSnapshot returns a copy of member IDs for a room.
func (s *Server) roomSnapshot(roomID string) []protocol.PeerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[roomID]
	if !ok {
		return nil
	}
	out := make([]protocol.PeerInfo, 0, len(r.members))
	for id, p := range r.members {
		out = append(out, protocol.PeerInfo{
			PeerID:      id,
			DisplayName: p.displayName,
			PublicKey:   p.publicKey,
			JoinedAt:    p.joinedAt,
		})
	}
	return out
}

// nextID returns a monotonic server-assigned identifier.
func (s *Server) nextID(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextIDLocked(prefix)
}

// nextIDLocked assigns an identifier. The caller must hold s.mu.
func (s *Server) nextIDLocked(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

var _ = json.Marshal
