package auth_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/auth"
)

// A pairing code is read off a screen and typed in by a person, so it has to
// survive a person: lowercase, spaces, the hyphens the interface prints, and
// the O-for-zero substitution every alphanumeric code attracts.
func TestNormaliseUserCodeSurvivesBeingTypedByAPerson(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	code := auth.NormaliseUserCode("K7M4-P2QX")
	r.NotEmpty(code)
	r.Equal(code, auth.NormaliseUserCode("k7m4-p2qx"), "case is not part of the code")
	r.Equal(code, auth.NormaliseUserCode(" K7M4 P2QX "), "spaces are not part of the code")
	r.Equal(code, auth.NormaliseUserCode("K7M4P2QX"), "the hyphen is for reading, not for matching")

	// It is the same normalisation the setup token uses, which is the point of
	// it being one function: a code that pairs must not depend on which screen
	// it was typed into.
	r.Equal(auth.NormaliseSetupToken("K7M4-P2QX"), code)
}

// Argon2 is deliberately expensive, which makes the length limit a denial of
// service control rather than a validation nicety: without it, one request with
// a very long password costs the server real time.
func TestHashPasswordRefusesOneLongerThanTheLimit(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	_, err := auth.HashPassword(strings.Repeat("a", auth.MaxPasswordLen+1))
	r.Error(err)
	r.Contains(err.Error(), "longer than")

	// And the limit itself is accepted, so the message is about what is too
	// long rather than what is nearly too long.
	hash, err := auth.HashPassword(strings.Repeat("a", auth.MaxPasswordLen))
	r.NoError(err)
	r.NotEmpty(hash)
}

// A nil keyring is a server whose configuration file carries no data key. Every
// method has to work on one, because the alternative is a nil check at every
// call site and a panic wherever one was forgotten.
func TestANilKeyringIsUsable(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var ring *auth.Keyring
	r.Nil(ring.Key(), "a nil keyring holds no key")
	r.NotPanics(func() { ring.Replace(nil) }, "replacing on a nil keyring is a no-op, not a panic")

	// A keyring holding no key is the same thing by another route.
	empty := auth.NewKeyring(nil)
	r.Nil(empty.Key())

	// And it can be given one later, which is what regenerating the data key
	// does while the server is running.
	key, err := auth.NewSecretKey()
	r.NoError(err)
	empty.Replace(key)
	r.Equal(key, empty.Key())
}

// Sealing with no key is refused rather than returning the plaintext, which is
// the failure mode that would matter.
func TestSealingWithoutAKeyIsRefused(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var none auth.SecretKey
	_, err := none.Seal("sk-ant-thekeynobodymayeversee")
	r.Error(err)

	_, err = none.Open([]byte("anything at all that is long enough"))
	r.Error(err)

	// An empty plaintext is not a credential and seals to nothing, which is how
	// "the operator cleared this field" is spelled.
	key, err := auth.NewSecretKey()
	r.NoError(err)
	sealed, err := key.Seal("")
	r.NoError(err)
	r.Empty(sealed)

	opened, err := key.Open(nil)
	r.NoError(err)
	r.Empty(opened)
}
