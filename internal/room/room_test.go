package room

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/security"
)

type memTrust struct {
	mu       sync.RWMutex
	verified map[identity.Fingerprint]string
}

func newMemTrust(verified ...ed25519.PublicKey) *memTrust {
	m := &memTrust{verified: map[identity.Fingerprint]string{}}
	for _, pub := range verified {
		m.verified[identity.FingerprintFromPublic(pub)] = "verified"
	}
	return m
}

func (m *memTrust) IsVerified(fp identity.Fingerprint) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.verified[fp] == "verified"
}

func (m *memTrust) Level(fp identity.Fingerprint) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if v, ok := m.verified[fp]; ok {
		return v
	}
	return "unverified"
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func newSelf(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.New("self")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newActiveRoom(t *testing.T, self *identity.Identity, trust TrustChecker) *Room {
	t.Helper()
	adminPub, adminPriv := newKey(t)
	roster := security.NewRoomMembership("room-1", 1, adminPub, []security.Member{
		{PublicKey: self.PublicKey, DisplayName: self.DisplayName},
	}, time.Hour)
	sig, err := roster.Sign(adminPriv)
	if err != nil {
		t.Fatal(err)
	}

	r, err := New(Options{
		RoomID:          "room-1",
		MaxParticipants: 5,
		Self:            self,
		Trust:           trust,
		RosterPolicy: security.MembershipPolicy{
			ExpectedAdmin: adminPub,
			MaxClockSkew:  time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BeginJoin(RoleCreator); err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(roster, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyRoster(roster, sig, security.MembershipPolicy{ExpectedAdmin: adminPub, MaxClockSkew: time.Minute}); err != nil {
		t.Fatal(err)
	}
	// ApplyRoster requires sequence >= current; re-applying the same roster is a
	// no-op but must not error, which the call above proves.
	return r
}

func TestNewValidatesOptions(t *testing.T) {
	self := newSelf(t)
	trust := newMemTrust()
	if _, err := New(Options{Self: self, Trust: trust}); err == nil {
		t.Fatal("missing room id accepted")
	}
	if _, err := New(Options{RoomID: "r", Trust: trust}); err == nil {
		t.Fatal("missing identity accepted")
	}
	if _, err := New(Options{RoomID: "r", Self: self}); err == nil {
		t.Fatal("missing trust checker accepted")
	}
	if _, err := New(Options{RoomID: "r", Self: self, Trust: trust, MaxParticipants: 64}); err == nil {
		t.Fatal("mesh limit not enforced")
	}
}

func TestJoinLifecycle(t *testing.T) {
	self := newSelf(t)
	r, err := New(Options{RoomID: "room-1", Self: self, Trust: newMemTrust()})
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, _ := r.Snapshot()
	if state != StateIdle {
		t.Fatalf("initial state = %q", state)
	}
	if _, err := r.AddPeer("p1", self.PublicKey, "x"); !errors.Is(err, ErrNotInRoom) {
		t.Fatalf("peer added before join: %v", err)
	}
	if err := r.BeginJoin(RoleCreator); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginJoin(RoleCreator); !errors.Is(err, ErrAlreadyInRoom) {
		t.Fatalf("double join accepted: %v", err)
	}
	r.Leave()
	state, _, _, _ = r.Snapshot()
	if state != StateIdle {
		t.Fatalf("state after leave = %q", state)
	}
}

func TestActivateRejectsRosterWithoutSelf(t *testing.T) {
	self := newSelf(t)
	r, _ := New(Options{RoomID: "room-1", Self: self, Trust: newMemTrust()})
	_ = r.BeginJoin(RoleCreator)

	adminPub, adminPriv := newKey(t)
	stranger, _ := newKey(t)
	roster := security.NewRoomMembership("room-1", 1, adminPub, []security.Member{{PublicKey: stranger}}, time.Hour)
	_ = adminPriv
	if err := r.Activate(roster, 1); !errors.Is(err, ErrRosterInvalid) {
		t.Fatalf("roster without self accepted: %v", err)
	}
}

func TestActivateRejectsWrongRoom(t *testing.T) {
	self := newSelf(t)
	r, _ := New(Options{RoomID: "room-1", Self: self, Trust: newMemTrust()})
	_ = r.BeginJoin(RoleCreator)
	adminPub, _ := newKey(t)
	roster := security.NewRoomMembership("room-2", 1, adminPub, []security.Member{{PublicKey: self.PublicKey}}, time.Hour)
	if err := r.Activate(roster, 1); !errors.Is(err, ErrRosterInvalid) {
		t.Fatalf("cross-room roster accepted: %v", err)
	}
}

// Requirement: "Unauthorized participant rejection". A peer introduced by
// signaling whose key the user has not verified must never be connected.
func TestUnverifiedPeerIsNotConnected(t *testing.T) {
	self := newSelf(t)
	r := newActiveRoom(t, self, newMemTrust())

	strangerPub, _ := newKey(t)
	peer, err := r.AddPeer("p1", strangerPub, "stranger")
	if !errors.Is(err, ErrPeerNotVerified) {
		t.Fatalf("unverified peer accepted: %v", err)
	}
	if peer != nil {
		if peer.State != ConnFailed {
			t.Fatalf("peer state = %q, want failed", peer.State)
		}
		if peer.LastError == "" {
			t.Fatal("failed peer must carry a reason")
		}
	}
	// The peer is still visible so "voxen peers" can explain the rejection, and
	// it must never be marked connected.
	stored, ok := r.PeerState("p1")
	if !ok {
		t.Fatal("rejected peer should remain visible with a failure reason")
	}
	if stored.State == ConnConnected {
		t.Fatal("unverified peer marked connected")
	}
}

func TestVerifiedPeerIsConnected(t *testing.T) {
	self := newSelf(t)
	peerPub, _ := newKey(t)
	r := newActiveRoom(t, self, newMemTrust(peerPub))

	peer, err := r.AddPeer("p2", peerPub, "friend")
	if err != nil {
		t.Fatal(err)
	}
	if peer.State != ConnConnecting {
		t.Fatalf("peer state = %q, want connecting", peer.State)
	}
	if err := r.UpdatePeer("p2", func(p *Peer) {
		p.State = ConnConnected
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ := r.PeerState("p2")
	if stored.State != ConnConnected {
		t.Fatalf("state = %q", stored.State)
	}
}

func TestDuplicatePeerRejected(t *testing.T) {
	self := newSelf(t)
	peerPub, _ := newKey(t)
	r := newActiveRoom(t, self, newMemTrust(peerPub))
	if _, err := r.AddPeer("p1", peerPub, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddPeer("p1", peerPub, ""); !errors.Is(err, ErrDuplicatePeer) {
		t.Fatalf("duplicate peer accepted: %v", err)
	}
}

func TestRoomCapacityEnforced(t *testing.T) {
	self := newSelf(t)
	// Two distinct verified peers are available, but the room only admits one.
	peerA, _ := newKey(t)
	peerB, _ := newKey(t)
	r := newActiveRoomWithLimit(t, self, newMemTrust(peerA, peerB), 2)

	if _, err := r.AddPeer("a", peerA, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddPeer("b", peerB, "b"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("room capacity not enforced: %v", err)
	}
}

func newActiveRoomWithLimit(t *testing.T, self *identity.Identity, trust TrustChecker, limit int) *Room {
	t.Helper()
	adminPub, adminPriv := newKey(t)
	roster := security.NewRoomMembership("room-1", 1, adminPub, []security.Member{
		{PublicKey: self.PublicKey, DisplayName: self.DisplayName},
	}, time.Hour)
	sig, err := roster.Sign(adminPriv)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{
		RoomID:          "room-1",
		MaxParticipants: limit,
		Self:            self,
		Trust:           trust,
		RosterPolicy:    security.MembershipPolicy{ExpectedAdmin: adminPub, MaxClockSkew: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BeginJoin(RoleCreator); err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(roster, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyRoster(roster, sig, security.MembershipPolicy{ExpectedAdmin: adminPub, MaxClockSkew: time.Minute}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestApplyRosterRemovesDroppedMembers(t *testing.T) {
	self := newSelf(t)
	alicePub, _ := newKey(t)
	bobPub, _ := newKey(t)
	trust := newMemTrust(alicePub, bobPub)
	r := newActiveRoom(t, self, trust)

	if _, err := r.AddPeer("alice", alicePub, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddPeer("bob", bobPub, "bob"); err != nil {
		t.Fatal(err)
	}

	// Administrator issues a new roster that removes bob.
	adminPub, adminPriv := newKey(t)
	updated := security.NewRoomMembership("room-1", 2, adminPub, []security.Member{
		{PublicKey: self.PublicKey, DisplayName: "self"},
		{PublicKey: alicePub, DisplayName: "alice"},
	}, time.Hour)
	sig, _ := updated.Sign(adminPriv)

	if err := r.ApplyRoster(updated, sig, security.MembershipPolicy{ExpectedAdmin: adminPub, MaxClockSkew: time.Minute}); err != nil {
		t.Fatalf("valid roster rejected: %v", err)
	}
	if _, ok := r.PeerState("bob"); ok {
		t.Fatal("removed member still present")
	}
	if _, ok := r.PeerState("alice"); !ok {
		t.Fatal("retained member was dropped")
	}
	if r.Sequence() != 2 {
		t.Fatalf("sequence = %d, want 2", r.Sequence())
	}
}

func TestApplyRosterRejectsRollbackAndForgery(t *testing.T) {
	self := newSelf(t)
	adminPub, adminPriv := newKey(t)
	r, _ := New(Options{RoomID: "room-1", Self: self, Trust: newMemTrust()})
	_ = r.BeginJoin(RoleCreator)

	first := security.NewRoomMembership("room-1", 2, adminPub, []security.Member{{PublicKey: self.PublicKey}}, time.Hour)
	if _, err := first.Sign(adminPriv); err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(first, 2); err != nil {
		t.Fatal(err)
	}

	older := security.NewRoomMembership("room-1", 1, adminPub, []security.Member{{PublicKey: self.PublicKey}}, time.Hour)
	olderSig, _ := older.Sign(adminPriv)
	if err := r.ApplyRoster(older, olderSig, security.MembershipPolicy{ExpectedAdmin: adminPub, MaxClockSkew: time.Minute}); !errors.Is(err, ErrRosterInvalid) {
		t.Fatalf("rolled back roster accepted: %v", err)
	}

	_, attackerPriv := newKey(t)
	forged := security.NewRoomMembership("room-1", 3, adminPub, []security.Member{{PublicKey: self.PublicKey}}, time.Hour)
	forgedSig, _ := forged.Sign(attackerPriv)
	if err := r.ApplyRoster(forged, forgedSig, security.MembershipPolicy{ExpectedAdmin: adminPub, MaxClockSkew: time.Minute}); !errors.Is(err, ErrRosterInvalid) {
		t.Fatalf("forged roster accepted: %v", err)
	}
}

func TestObserverReceivesEvents(t *testing.T) {
	self := newSelf(t)
	peerPub, _ := newKey(t)

	var mu sync.Mutex
	var kinds []string
	r := newActiveRoom(t, self, newMemTrust(peerPub))
	r.SetObserver(func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, e.Kind)
	})

	if _, err := r.AddPeer("p1", peerPub, "friend"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemovePeer("p1"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(kinds) < 2 {
		t.Fatalf("expected peer_added and peer_removed, got %v", kinds)
	}
	if kinds[0] != EventPeerAdded || kinds[1] != EventPeerRemoved {
		t.Fatalf("unexpected events: %v", kinds)
	}
}

func TestWaitReady(t *testing.T) {
	self := newSelf(t)
	r, _ := New(Options{RoomID: "room-1", Self: self, Trust: newMemTrust()})
	_ = r.BeginJoin(RoleCreator)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := contextWithTimeout(200 * time.Millisecond)
		defer cancel()
		done <- r.WaitReady(ctx)
	}()

	r.Fail(errors.New("signaling refused"))
	if err := <-done; err == nil {
		t.Fatal("WaitReady should surface a failed join")
	}
}
