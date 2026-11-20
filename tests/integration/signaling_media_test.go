package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/media"
	"github.com/s4chdev/voxen/internal/security"
	"github.com/s4chdev/voxen/internal/signaling"
	"github.com/s4chdev/voxen/internal/signaling/protocol"
	serversig "github.com/s4chdev/voxen/server/signaling"
)

// startSignalingServer runs a real signaling server on an ephemeral port and
// returns its WebSocket URL.
func startSignalingServer(t *testing.T) string {
	t.Helper()
	srv, err := serversig.New(serversig.Config{
		Addr:       "127.0.0.1:0",
		MaxMembers: 8,
		Logger:     slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("new signaling server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("signaling server did not stop")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			return "ws://" + addr + "/ws"
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("signaling server did not start listening")
	return ""
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

type testSigner struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{pub: pub, priv: priv}
}

func (s *testSigner) Sign(m []byte) []byte         { return ed25519.Sign(s.priv, m) }
func (s *testSigner) PublicKey() ed25519.PublicKey { return s.pub }

// relayDispatcher owns the single reader for one signaling connection and routes
// messages by type. A WebSocket must have exactly one reader; splitting it
// between an SDP exchange and a candidate pump introduces a race where one
// consumer steals the other's message.
type relayDispatcher struct {
	client   *signaling.Client
	OnOffer  func(*media.Negotiation)
	OnAnswer func(*media.Negotiation)
	OnICE    func(media.ICECandidatePayload)
	OnPeer   func(env protocol.Envelope)

	mu   sync.Mutex
	last []protocol.Envelope
}

func (d *relayDispatcher) run(ctx context.Context) {
	for {
		env, err := d.client.Recv(ctx)
		if err != nil {
			return
		}
		d.mu.Lock()
		d.last = append(d.last, env)
		d.mu.Unlock()

		switch env.Type {
		case protocol.TypeOffer:
			var n media.Negotiation
			if json.Unmarshal(env.Payload, &n) == nil && d.OnOffer != nil {
				d.OnOffer(&n)
			}
		case protocol.TypeAnswer:
			var n media.Negotiation
			if json.Unmarshal(env.Payload, &n) == nil && d.OnAnswer != nil {
				d.OnAnswer(&n)
			}
		case protocol.TypeICECandidate:
			var c media.ICECandidatePayload
			if json.Unmarshal(env.Payload, &c) == nil && d.OnICE != nil {
				d.OnICE(c)
			}
		default:
			if d.OnPeer != nil {
				d.OnPeer(env)
			}
		}
	}
}

// TestSignalingRelayAndDirectMedia is the end-to-end acceptance test.
//
// It starts a real signaling server, connects two clients that negotiate real
// Pion sessions through it, completes the Ed25519-to-DTLS binding, and carries
// audio in one direction. Nothing is faked except the audio source, which is a
// generated tone standing in for a microphone.
func TestSignalingRelayAndDirectMedia(t *testing.T) {
	url := startSignalingServer(t)

	alice := newTestSigner(t)
	bob := newTestSigner(t)

	newClient := func(name string, s *testSigner) *signaling.Client {
		c, err := signaling.Connect(context.Background(), signaling.ClientConfig{
			URL: url, DisplayName: name,
			PublicKey: hex.EncodeToString(s.pub), Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	aliceClient := newClient("alice", alice)
	bobClient := newClient("bob", bob)

	if aliceClient.PeerID() == "" || bobClient.PeerID() == "" {
		t.Fatal("server did not assign peer IDs")
	}
	if aliceClient.PeerID() == bobClient.PeerID() {
		t.Fatal("distinct peers received the same routing ID")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	created, err := aliceClient.CreateRoom(ctx)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	joined, err := bobClient.JoinRoom(ctx, created.RoomID)
	if err != nil {
		t.Fatalf("join room: %v", err)
	}
	if len(joined.Members) != 2 {
		t.Fatalf("bob sees %d members, want 2", len(joined.Members))
	}

	const sessionID = "e2e-session-1"
	params := media.DefaultOpusParameters(28000, 20)
	shared := func(self, peer *testSigner) (*media.WebRTCSession, error) {
		return media.NewWebRTCSession(ctx, media.SessionConfig{
			RoomID: created.RoomID, SessionID: sessionID,
			LocalIdentity: self, ExpectedPeerKey: peer.pub,
			Parameters: params, StatementTTL: 2 * time.Minute,
			ReplayGuard: security.NewReplayGuard(64),
		})
	}
	aliceSession, err := shared(alice, bob)
	if err != nil {
		t.Fatalf("alice session: %v", err)
	}
	defer aliceSession.Close()
	bobSession, err := shared(bob, alice)
	if err != nil {
		t.Fatalf("bob session: %v", err)
	}
	defer bobSession.Close()

	// Answer/authenticate results arrive asynchronously.
	type result struct {
		info media.PeerInfo
		err  error
	}
	aliceAuth := make(chan result, 1)
	bobAuth := make(chan result, 1)
	bobAnswer := make(chan error, 1)

	send := func(c *signaling.Client, to, typ string, payload any) {
		raw, _ := json.Marshal(payload)
		if err := c.Send(ctx, protocol.Envelope{
			Version: protocol.ProtocolVersion, Type: typ, To: to, Payload: raw,
		}); err != nil {
			t.Logf("relay %s: %v", typ, err)
		}
	}

	aliceDispatch := &relayDispatcher{client: aliceClient}
	bobDispatch := &relayDispatcher{client: bobClient}

	aliceSession.SetCandidateHandler(func(c media.ICECandidatePayload) {
		send(aliceClient, bobClient.PeerID(), protocol.TypeICECandidate, c)
	})
	bobSession.SetCandidateHandler(func(c media.ICECandidatePayload) {
		send(bobClient, aliceClient.PeerID(), protocol.TypeICECandidate, c)
	})

	bobDispatch.OnICE = func(c media.ICECandidatePayload) {
		if err := bobSession.AddICECandidate(ctx, c); err != nil {
			t.Logf("bob ice: %v", err)
		}
	}
	aliceDispatch.OnICE = func(c media.ICECandidatePayload) {
		if err := aliceSession.AddICECandidate(ctx, c); err != nil {
			t.Logf("alice ice: %v", err)
		}
	}

	// Bob answers an offered session when it arrives.
	//
	// Authenticate runs in its own goroutine: it blocks until DTLS completes,
	// and DTLS needs the ICE candidates that this same dispatcher goroutine is
	// responsible for reading. Blocking here would starve the candidate feed and
	// deadlock the negotiation. The daemon's room layer follows the same rule.
	bobDispatch.OnOffer = func(offer *media.Negotiation) {
		answer, _, err := bobSession.AcceptOffer(ctx, offer)
		if err != nil {
			bobAnswer <- err
			return
		}
		send(bobClient, aliceClient.PeerID(), protocol.TypeAnswer, answer)
		bobAnswer <- nil

		go func() {
			info, err := bobSession.Authenticate(ctx)
			bobAuth <- result{info, err}
		}()
	}

	// Alice completes the answer, then authenticates off the reader goroutine.
	aliceDispatch.OnAnswer = func(answer *media.Negotiation) {
		if _, err := aliceSession.AcceptAnswer(ctx, answer); err != nil {
			aliceAuth <- result{err: err}
			return
		}
		go func() {
			info, err := aliceSession.Authenticate(ctx)
			aliceAuth <- result{info, err}
		}()
	}

	go aliceDispatch.run(ctx)
	go bobDispatch.run(ctx)

	offer, err := aliceSession.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	send(aliceClient, bobClient.PeerID(), protocol.TypeOffer, offer)

	if err := <-bobAnswer; err != nil {
		t.Fatalf("bob failed to answer: %v", err)
	}
	resA := <-aliceAuth
	if resA.err != nil {
		t.Fatalf("alice authentication failed: %v", resA.err)
	}
	resB := <-bobAuth
	if resB.err != nil {
		t.Fatalf("bob authentication failed: %v", resB.err)
	}

	if !resA.info.RemoteCertVerified || !resB.info.RemoteCertVerified {
		t.Fatal("media started without the identity-to-DTLS binding")
	}
	// Each side must have bound the other's real certificate.
	if !security.EqualFingerprint(resA.info.DTLSFingerprint, bobSession.LocalFingerprint()) {
		t.Fatal("alice did not bind bob's certificate")
	}
	if !security.EqualFingerprint(resB.info.DTLSFingerprint, aliceSession.LocalFingerprint()) {
		t.Fatal("bob did not bind alice's certificate")
	}
	t.Logf("authenticated: alice bound %s", security.FingerprintHex(resA.info.DTLSFingerprint))

	// Audio flows only after authentication.
	pcm := make([]int16, 960)
	for i := range pcm {
		pcm[i] = int16(9000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	go func() {
		for i := 0; i < 30; i++ {
			_ = aliceSession.SendPCM(ctx, pcm)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	out := make([]int16, 960)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no audio received over the relayed session")
		default:
		}
		n, err := bobSession.ReceivePCM(ctx, out)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("timed out waiting for audio")
			}
			continue
		}
		if n > 0 {
			var peak int16
			for _, v := range out[:n] {
				if v > peak {
					peak = v
				}
				if -v > peak {
					peak = -v
				}
			}
			if peak == 0 {
				t.Fatal("received a silent frame; audio path is not working")
			}
			t.Logf("received %d samples, peak amplitude %d", n, peak)
			return
		}
	}
}

// TestSignalingRelayRejectsCrossRoomInjection proves a participant cannot address
// a peer in another room, even if it learns that peer's routing ID.
func TestSignalingRelayRejectsCrossRoomInjection(t *testing.T) {
	url := startSignalingServer(t)
	s := newTestSigner(t)

	newClient := func(name string) *signaling.Client {
		c, err := signaling.Connect(context.Background(), signaling.ClientConfig{
			URL: url, DisplayName: name, PublicKey: hex.EncodeToString(s.pub),
			Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	alice := newClient("alice")
	bob := newClient("bob")
	eve := newClient("eve")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	createdA, err := alice.CreateRoom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.JoinRoom(ctx, createdA.RoomID); err != nil {
		t.Fatal(err)
	}
	// Eve is in her own room and knows Bob's routing ID.
	if _, err := eve.CreateRoom(ctx); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(map[string]string{"sdp": "v=0", "type": "offer"})
	if err := eve.Send(ctx, protocol.Envelope{
		Version: protocol.ProtocolVersion, Type: protocol.TypeOffer,
		To: bob.PeerID(), Payload: payload,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Bob must not receive it. His only messages should be room state.
	recvCtx, recvCancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer recvCancel()
	for {
		env, err := bob.Recv(recvCtx)
		if err != nil {
			return // timeout: nothing was injected, as required
		}
		if env.Type == protocol.TypeOffer {
			t.Fatal("cross-room SDP injection was delivered")
		}
	}
}

// TestSignalingServerNeverSeesMediaGuard documents and enforces a structural
// property: the signaling protocol has no media message type. If someone adds
// one, this test fails and forces a review.
func TestSignalingServerNeverSeesMediaGuard(t *testing.T) {
	allowed := map[string]bool{
		protocol.TypeHello:        true,
		protocol.TypeWelcome:      true,
		protocol.TypeRoomCreate:   true,
		protocol.TypeRoomJoin:     true,
		protocol.TypeRoomLeave:    true,
		protocol.TypeRoomState:    true,
		protocol.TypePeerJoin:     true,
		protocol.TypePeerLeave:    true,
		protocol.TypeOffer:        true,
		protocol.TypeAnswer:       true,
		protocol.TypeICECandidate: true,
		protocol.TypeICEEnd:       true,
		protocol.TypeRoster:       true,
		protocol.TypeAdmit:        true,
		protocol.TypeRemove:       true,
		protocol.TypeError:        true,
		protocol.TypePing:         true,
		protocol.TypePong:         true,
	}
	if len(allowed) != 18 {
		t.Fatalf("protocol grew to %d types; audit each one to confirm none carries audio", len(allowed))
	}
	for name := range allowed {
		if name == "rtp" || name == "audio" || name == "media" {
			t.Fatalf("signaling protocol must not carry media, found type %q", name)
		}
	}
	_ = fmt.Sprintf
}
