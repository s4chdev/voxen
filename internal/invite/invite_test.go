package invite

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func newKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// TestCodeRoundTrip covers the shape of a code: six room characters, four check
// characters, grouped in fours.
func TestCodeRoundTrip(t *testing.T) {
	code, err := Generate("room-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(code.Room) != roomChars {
		t.Fatalf("room portion = %q, want %d chars", code.Room, roomChars)
	}

	pub := newKey(t)
	full, err := code.WithCheck(pub)
	if err != nil {
		t.Fatal(err)
	}

	// A person reads this aloud, so the grouped form matters.
	display := full.String()
	groups := strings.Split(display, "-")
	if len(groups) != 3 {
		t.Fatalf("display form %q has %d groups, want 3", display, len(groups))
	}
	for _, g := range groups {
		if len(g) != groupLen {
			t.Fatalf("group %q has %d chars, want %d", g, len(g), groupLen)
		}
	}

	parsed, err := Parse(display)
	if err != nil {
		t.Fatalf("parse %q: %v", display, err)
	}
	if parsed.Room != full.Room || parsed.Check != full.Check {
		t.Fatalf("round trip changed the code: %+v -> %+v", full, parsed)
	}
}

// TestParseAcceptsWhatPeoplePaste covers the realistic input variants: lowercase,
// no dashes, spaces, and the whole command copied from the terminal.
func TestParseAcceptsWhatPeoplePaste(t *testing.T) {
	code, _ := Generate("room-7")
	pub := newKey(t)
	full, _ := code.WithCheck(pub)
	display := full.String()

	variants := []string{
		display,
		strings.ToLower(display),
		strings.ReplaceAll(display, "-", ""),
		strings.ReplaceAll(display, "-", " "),
		"  " + display + "  ",
		"voxen join " + display,
		"voxen call " + display,
		"VOXEN JOIN " + strings.ToLower(display),
	}
	for _, v := range variants {
		got, err := Parse(v)
		if err != nil {
			t.Fatalf("parse %q: %v", v, err)
		}
		if got.Room != full.Room || got.Check != full.Check {
			t.Fatalf("parse %q produced %+v, want %+v", v, got, full)
		}
	}
}

// TestParseMapsConfusableCharacters proves the alphabet does its job. Crockford base32
// exists so a code read over a phone call survives: I/L become 1, O becomes 0.
func TestParseMapsConfusableCharacters(t *testing.T) {
	code, _ := Generate("room-x")
	pub := newKey(t)
	full, _ := code.WithCheck(pub)

	// Substitute every 0 with O and every 1 with I; parsing must still work.
	mangled := strings.NewReplacer("0", "O", "1", "I").Replace(full.String())
	got, err := Parse(mangled)
	if err != nil {
		t.Fatalf("parse of confusable characters failed: %v", err)
	}
	if got.Room != full.Room {
		t.Fatalf("room portion changed: %q -> %q", full.Room, got.Room)
	}
}

// TestParseRejectsGarbage proves bad input fails clearly rather than producing a
// code that silently points at the wrong room.
func TestParseRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"too short": "ABC",
		"too long":  "ABCDEFGHIJKLM",
		"bad char":  "ABC!EFGHIJK",
		"letters":   "ABCDEFGHIJKLMNOP", // P and M are valid but the length is wrong
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("accepted %q", in)
			}
		})
	}
}

// TestKeyCheckRejectsSubstitutedIdentity is the security property the code exists for.
//
// This is the attack the check prevents: the invite blob travels through the
// signaling server, so a hostile or compromised server could replace the creator's
// public key with its own. The joiner recomputes the check from the key it was handed
// and compares it with the check in the code they typed. A substituted key produces a
// different check, so the join is refused.
func TestKeyCheckRejectsSubstitutedIdentity(t *testing.T) {
	genuine := newKey(t)
	attacker := newKey(t)

	code, _ := Generate("room-z")
	full, err := code.WithCheck(genuine)
	if err != nil {
		t.Fatal(err)
	}

	if err := full.Verify(genuine); err != nil {
		t.Fatalf("genuine key rejected: %v", err)
	}

	// The server swaps in its own key while the joiner still has the real code.
	if err := full.Verify(attacker); err == nil {
		t.Fatal("a substituted identity passed verification")
	} else if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("unhelpful rejection: %v", err)
	}
}

// TestCheckIsBoundToRoom proves a check cannot be lifted from one room into another.
func TestCheckIsBoundToRoom(t *testing.T) {
	pub := newKey(t)
	a, _ := Generate("room-a")
	b, _ := Generate("room-b")

	checkA, err := DeriveCheck(pub, a.Room)
	if err != nil {
		t.Fatal(err)
	}
	checkB, err := DeriveCheck(pub, b.Room)
	if err != nil {
		t.Fatal(err)
	}
	if checkA == checkB {
		// Not impossible, but with six random room characters it should be
		// vanishingly rare; a collision here would mean the room is not mixed in.
		t.Fatalf("check %q is the same for two different rooms; the room is not bound in", checkA)
	}

	// A code assembled from room A's identifier and room B's check must fail.
	hybrid := Code{Room: a.Room, Check: checkB}
	if err := hybrid.Verify(pub); err == nil {
		t.Fatal("a check from another room was accepted")
	}
}

// TestGenerateIsDeterministic proves the code survives a server restart. A code that
// was already shared must not silently stop working.
func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate("room-stable")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate("room-stable")
	if err != nil {
		t.Fatal(err)
	}
	if a.Room != b.Room {
		t.Fatalf("the same room produced different codes: %q vs %q", a.Room, b.Room)
	}

	c, _ := Generate("room-other")
	if c.Room == a.Room {
		t.Fatal("different rooms produced the same code")
	}
}

// TestVerifyRequiresCheck proves an incomplete code cannot be verified, so a caller
// cannot accidentally skip the step that authenticates the inviter.
func TestVerifyRequiresCheck(t *testing.T) {
	code, _ := Generate("room-q")
	if err := code.Verify(newKey(t)); err == nil {
		t.Fatal("a code with no check verified")
	}
}

// TestCodesAreUnambiguous proves the alphabet excludes characters that look alike.
func TestCodesAreUnambiguous(t *testing.T) {
	for _, bad := range []rune{'I', 'L', 'O', 'U'} {
		if strings.ContainsRune(crockford, bad) {
			t.Fatalf("alphabet contains the ambiguous character %q", bad)
		}
	}
}

// TestSessionTokenDoesNotLeakKey proves the logging token is derived, not the key.
func TestSessionTokenDoesNotLeakKey(t *testing.T) {
	pub := newKey(t)
	inv := Invite{CreatorPublicKey: pub}
	tok := inv.SessionToken()
	if tok == "" {
		t.Fatal("empty token")
	}
	if strings.Contains(tok, string(pub)) {
		t.Fatal("token contains the raw public key")
	}
	if len(tok) != 16 {
		t.Fatalf("token = %q, want 16 hex chars", tok)
	}
}
