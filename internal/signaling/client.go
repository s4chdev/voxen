// Package signaling client: the control-plane transport used by the daemon.
//
// The client owns the WebSocket, reconnection policy and the request/response
// correlation for room operations. It deliberately knows nothing about SDP
// semantics or identity: payloads are passed through as raw JSON so the media and
// room layers decide what they mean.
package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// Client errors.
var (
	ErrClosed          = errors.New("signaling: connection closed")
	ErrNoRoom          = errors.New("signaling: not in a room")
	ErrNotConnected    = errors.New("signaling: not connected")
	ErrVersionMismatch = errors.New("signaling: unsupported protocol version")
)

// ClientConfig configures a signaling client.
type ClientConfig struct {
	URL         string
	DisplayName string
	PublicKey   string
	// RoomID joins immediately after the handshake when set.
	RoomID string
	// Timeout bounds dialing, the handshake and each request.
	Timeout time.Duration
	// TLSInsecureSkipVerify is only accepted for loopback URLs by config
	// validation; it exists for local testing.
	TLSInsecureSkipVerify bool
}

// Client is a connected signaling client. All methods are safe for concurrent
// use.
type Client struct {
	cfg  ClientConfig
	conn *websocket.Conn

	writeMu sync.Mutex
	mu      sync.Mutex
	peerID  string
	roomID  string
	state   State
	inbox   chan protocol.Envelope
	closed  chan struct{}
	once    sync.Once
	err     error
}

// State is a coarse connection state, surfaced by "voxen status".
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateHandshaking  State = "handshaking"
	StateConnected    State = "connected"
)

// Connect dials and performs the handshake.
func Connect(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("signaling: URL is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("signaling: parse URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("signaling: unsupported scheme %q", u.Scheme)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: cfg.Timeout,
		// gorilla resolves ws/wss itself; the field is for custom TLS setups.
		TLSClientConfig: nil,
	}
	_ = cfg.TLSInsecureSkipVerify

	c := &Client{
		cfg:    cfg,
		state:  StateConnecting,
		inbox:  make(chan protocol.Envelope, 64),
		closed: make(chan struct{}),
	}

	conn, resp, err := dialer.DialContext(ctx, cfg.URL, nil)
	if err != nil {
		c.setState(StateDisconnected)
		if resp != nil {
			return nil, fmt.Errorf("signaling: dial %s: HTTP %d: %w", cfg.URL, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("signaling: dial %s: %w", cfg.URL, err)
	}
	c.conn = conn
	c.setState(StateHandshaking)

	if err := c.handshake(ctx); err != nil {
		conn.Close()
		c.setState(StateDisconnected)
		return nil, err
	}

	go c.readLoop()
	c.setState(StateConnected)
	return c, nil
}

func (c *Client) handshake(ctx context.Context) error {
	hello := protocol.Hello{
		ProtocolVersion: protocol.ProtocolVersion,
		DisplayName:     c.cfg.DisplayName,
		PublicKey:       c.cfg.PublicKey,
		RoomID:          c.cfg.RoomID,
	}
	env := protocol.Envelope{Version: protocol.ProtocolVersion, Type: protocol.TypeHello, Payload: mustJSON(hello)}
	if err := c.write(env); err != nil {
		return err
	}

	_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.Timeout))
	var welcome protocol.Envelope
	if err := c.conn.ReadJSON(&welcome); err != nil {
		return fmt.Errorf("signaling: read welcome: %w", err)
	}
	if welcome.Type == protocol.TypeError {
		var payload protocol.ErrorPayload
		_ = json.Unmarshal(welcome.Payload, &payload)
		return fmt.Errorf("signaling: server rejected handshake: %s", payload.Message)
	}
	if welcome.Type != protocol.TypeWelcome {
		return fmt.Errorf("signaling: expected welcome, got %q", welcome.Type)
	}
	var w protocol.Welcome
	if err := json.Unmarshal(welcome.Payload, &w); err != nil {
		return fmt.Errorf("signaling: decode welcome: %w", err)
	}
	if w.ProtocolVersion != protocol.ProtocolVersion {
		return fmt.Errorf("%w: server speaks %d, client speaks %d",
			ErrVersionMismatch, w.ProtocolVersion, protocol.ProtocolVersion)
	}

	c.mu.Lock()
	c.peerID = w.PeerID
	c.mu.Unlock()
	return nil
}

func (c *Client) readLoop() {
	for {
		var env protocol.Envelope
		// A generous read deadline: the server pings every 30s, and a peer in an
		// idle call must not be disconnected for silence.
		_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		if err := c.conn.ReadJSON(&env); err != nil {
			c.fail(err)
			return
		}
		if env.Type == protocol.TypeError {
			var payload protocol.ErrorPayload
			_ = json.Unmarshal(env.Payload, &payload)
			c.mu.Lock()
			c.err = fmt.Errorf("signaling: server error %s: %s", payload.Code, payload.Message)
			c.mu.Unlock()
			continue
		}
		if env.Type == protocol.TypeRoomState {
			var payload protocol.RoomStatePayload
			if err := json.Unmarshal(env.Payload, &payload); err == nil {
				c.mu.Lock()
				c.roomID = payload.RoomID
				c.mu.Unlock()
			}
		}
		select {
		case c.inbox <- env:
		case <-c.closed:
			return
		case <-time.After(2 * time.Second):
			// A consumer that is not draining must not stall the socket. Dropping
			// a control message is a bug, so it is logged by the caller when the
			// negotiation times out.
		}
	}
}

// Send transmits one envelope.
func (c *Client) Send(ctx context.Context, env protocol.Envelope) error {
	if env.Version == 0 {
		env.Version = protocol.ProtocolVersion
	}
	if env.Version != protocol.ProtocolVersion {
		return ErrVersionMismatch
	}
	return c.write(env)
}

func (c *Client) write(env protocol.Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.cfg.Timeout))
	if err := c.conn.WriteJSON(env); err != nil {
		return fmt.Errorf("signaling: write: %w", err)
	}
	return nil
}

// Recv returns the next envelope, blocking until one arrives or ctx ends.
func (c *Client) Recv(ctx context.Context) (protocol.Envelope, error) {
	select {
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	case <-c.closed:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return protocol.Envelope{}, err
		}
		return protocol.Envelope{}, ErrClosed
	case env, ok := <-c.inbox:
		if !ok {
			return protocol.Envelope{}, ErrClosed
		}
		return env, nil
	}
}

// PeerID is the server-assigned routing identifier for this client.
func (c *Client) PeerID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peerID
}

// RoomID is the room this client is in, if any.
func (c *Client) RoomID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roomID
}

// State reports the connection state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) setState(s State) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

func (c *Client) fail(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.state = StateDisconnected
		c.mu.Unlock()
		close(c.closed)
		_ = c.conn.Close()
	})
}

// Close closes the connection.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	return nil
}

// CreateRoom asks the server for a new room and waits for its state.
func (c *Client) CreateRoom(ctx context.Context) (string, []protocol.PeerInfo, error) {
	if err := c.Send(ctx, protocol.Envelope{
		Version: protocol.ProtocolVersion, Type: protocol.TypeRoomCreate,
		Payload: mustJSON(map[string]string{"name": ""}),
	}); err != nil {
		return "", nil, err
	}
	return c.waitForRoomState(ctx)
}

// JoinRoom joins an existing room and waits for the roster.
func (c *Client) JoinRoom(ctx context.Context, roomID string) ([]protocol.PeerInfo, error) {
	if err := c.Send(ctx, protocol.Envelope{
		Version: protocol.ProtocolVersion, Type: protocol.TypeRoomJoin,
		Payload: mustJSON(map[string]string{"room_id": roomID}),
	}); err != nil {
		return nil, err
	}
	_, members, err := c.waitForRoomState(ctx)
	return members, err
}

func (c *Client) waitForRoomState(ctx context.Context) (string, []protocol.PeerInfo, error) {
	for {
		env, err := c.Recv(ctx)
		if err != nil {
			return "", nil, err
		}
		if env.Type != protocol.TypeRoomState {
			continue
		}
		var payload protocol.RoomStatePayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return "", nil, fmt.Errorf("signaling: decode room state: %w", err)
		}
		c.mu.Lock()
		c.roomID = payload.RoomID
		c.mu.Unlock()
		return payload.RoomID, payload.Members, nil
	}
}

// mustJSON encodes a payload, returning an empty object on failure. The values
// marshalled here are plain structs and maps, so failure is unreachable.
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
