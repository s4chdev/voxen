package invite_test

import (
	"crypto/ed25519"
	"fmt"

	"github.com/s4chdev/voxen/internal/invite"
)

// Example shows the whole lifecycle a user goes through: the caller generates a
// code, reads it out, and the joiner's client verifies that the key it was handed
// matches the code that was spoken.
//
// The output uses a fixed key so the example is reproducible; generate keys with
// crypto/rand in real code.
func Example() {
	// A key that always yields the same code, so the example output is stable.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	creator := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	code, _ := invite.Generate("room-42")
	full, _ := code.WithCheck(creator)
	fmt.Println("code:", full.String())

	// The joiner parses what they were given and checks it against the key that
	// arrived with the invite. A substituted key fails here.
	parsed, err := invite.Parse("d8cy 2xwt fme0")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Println("matches inviter key:", parsed.Verify(creator) == nil)
	// Output:
	// code: D8CY-2XWT-FME0
	// matches inviter key: true
}

// ExampleParse_shorthand demonstrates that the forms a person is likely to paste all
// work, including the command itself.
func ExampleParse_shorthand() {
	for _, in := range []string{"D8CY-2XWT-FME0", "d8cy2xwtfme0", "voxen join D8CY 2XWT FME0"} {
		c, err := invite.Parse(in)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println(c.Room, c.Check)
	}
	// Output:
	// D8CY2XWT FME0
	// D8CY2XWT FME0
	// D8CY2XWT FME0
}
