package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/invite"
	"github.com/s4chdev/voxen/internal/signaling/protocol"
)

// TestJoinerIsHeldUntilItProvesTheCode covers the ordering bug that made every
// one-code mesh one-sided.
//
// The server delivers peer.join before the joiner's invite acceptance, so refusing an
// unauthenticated peer on peer.join rejects every legitimate joiner. The peer must be
// held pending, then started when its proof arrives.
//
// Before the fix, the creator logged "refusing a peer whose identity is not verified"
// and then "participant proved the invite code" in the same second: it granted trust
// and never retried the session, so only the joiner ever saw a connection.
func TestJoinerIsHeldUntilItProvesTheCode(t *testing.T) {
	srv := newServerForPeers(t)
	c := srv.controllerFor()

	// The creator issues a code, so it will validate acceptances.
	code, err := invite.Generate("room-order")
	if err != nil {
		t.Fatal(err)
	}
	full, err := code.WithCheck(c.identity.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.codeCheck = full.Check
	c.mu.Unlock()

	// A joiner arrives with a key the creator does not know yet.
	joiner, joinerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peer := protocol.PeerInfo{
		PeerID:      "peer-joiner",
		DisplayName: "bob",
		PublicKey:   hex.EncodeToString(joiner),
	}
	// First: peer.join with no proof. It must be held, not rejected.
	c.onPeerJoined(context.Background(), nil, peer)

	c.mu.Lock()
	_, held := c.pendingPeers["peer-joiner"]
	_, hasSession := c.sessions["peer-joiner"]
	c.mu.Unlock()
	if !held {
		t.Fatal("an unauthenticated joiner was not held pending; it would be refused before its proof arrives")
	}
	if hasSession {
		t.Fatal("a session was started before the joiner proved the code")
	}

	// Now the proof arrives: the joiner signs the check.
	acceptance := acceptancePayload{
		CodeCheck:    full.Check,
		PublicKey:    hex.EncodeToString(joiner),
		DisplayName:  "bob",
		SignedAtUnix: time.Now().Unix(),
	}
	payload, err := json.Marshal(acceptance)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(joinerPriv, acceptanceSigningBytes(full.Check, joiner))
	wrapper, err := json.Marshal(acceptanceEnvelope{
		Acceptance: payload,
		Signature:  base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.onInviteAcceptance(context.Background(), protocol.Envelope{
		Type: protocol.TypeAdmit, From: "peer-joiner", Payload: wrapper,
	})

	c.mu.Lock()
	_, stillHeld := c.pendingPeers["peer-joiner"]
	c.mu.Unlock()
	if stillHeld {
		t.Fatal("the joiner is still pending after proving the code")
	}

	// And it is now trusted, which is what lets the session start.
	if _, ok := c.trust.VerifiedKey(identity.FingerprintFromPublic(joiner)); !ok {
		t.Fatal("the joiner's key was not trusted after a valid proof")
	}
}

// TestAcceptanceWithWrongCheckIsRefused proves a proof for a different code does not
// authenticate anyone.
func TestAcceptanceWithWrongCheckIsRefused(t *testing.T) {
	srv := newServerForPeers(t)
	c := srv.controllerFor()

	code, _ := invite.Generate("room-real")
	full, _ := code.WithCheck(c.identity.PublicKey)
	c.mu.Lock()
	c.codeCheck = full.Check
	c.mu.Unlock()

	joiner, joinerPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrong := acceptancePayload{
		CodeCheck:   "ZZZZ",
		PublicKey:   hex.EncodeToString(joiner),
		DisplayName: "mallory",
	}
	payload, _ := json.Marshal(wrong)
	sig := ed25519.Sign(joinerPriv, acceptanceSigningBytes(wrong.CodeCheck, joiner))
	wrapper, _ := json.Marshal(acceptanceEnvelope{
		Acceptance: payload, Signature: base64.StdEncoding.EncodeToString(sig),
	})
	c.onInviteAcceptance(context.Background(), protocol.Envelope{
		Type: protocol.TypeAdmit, From: "peer-x", Payload: wrapper,
	})

	if _, ok := c.trust.VerifiedKey(identity.FingerprintFromPublic(joiner)); ok {
		t.Fatal("a proof quoting the wrong code authenticated a peer")
	}
}
