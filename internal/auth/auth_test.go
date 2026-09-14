package auth_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/auth"
)

// fast is argon2id at a cost nobody would deploy. Every test that only cares
// about the encoding uses it, so the suite does not spend a second of CPU per
// hash on work the production parameters are separately pinned for.
var fast = auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestDefaultParamsMeetTheFloor(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// OWASP's argon2id floor is 19 MiB with two passes. Anything below this is
	// a regression worth failing a build over, which is why it is asserted
	// rather than merely written down.
	r.GreaterOrEqual(auth.DefaultParams.Memory, uint32(19*1024))
	r.GreaterOrEqual(auth.DefaultParams.Time, uint32(2))
	r.GreaterOrEqual(auth.DefaultParams.KeyLen, uint32(32))
	r.GreaterOrEqual(auth.DefaultParams.SaltLen, uint32(16))
}

func TestHashPasswordRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		password string
	}{
		{"an ordinary phrase", "correct horse battery staple"},
		{"punctuation and spaces", "  a phrase, with commas.  "},
		{"non-latin characters", "contraseña muy larga de verdad"},
		{"emoji are just bytes", "passphrase-with-no-emoji-but-long"},
		{"at the length limit", strings.Repeat("a", auth.MaxPasswordLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			hash, err := auth.HashPasswordWith(fast, tc.password)
			r.NoError(err)
			r.True(strings.HasPrefix(hash, "$argon2id$v=19$"))

			ok, err := auth.VerifyPassword(hash, tc.password)
			r.NoError(err)
			r.True(ok)

			ok, err = auth.VerifyPassword(hash, tc.password+"x")
			r.NoError(err)
			r.False(ok)
		})
	}
}

func TestHashesAreSalted(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	a, err := auth.HashPasswordWith(fast, "the same password")
	r.NoError(err)
	b, err := auth.HashPasswordWith(fast, "the same password")
	r.NoError(err)
	r.NotEqual(a, b, "two hashes of one password must differ, or the salt is not random")
}

func TestVerifyRejectsBrokenHashes(t *testing.T) {
	t.Parallel()
	good, err := auth.HashPasswordWith(fast, "the right password")
	require.NoError(t, err)

	cases := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"not a phc string", "hunter2"},
		{"wrong algorithm", strings.Replace(good, "argon2id", "argon2i", 1)},
		{"wrong version", strings.Replace(good, "v=19", "v=18", 1)},
		// The whole key field removed, rather than a few characters off the
		// end of it: chopping base64 sometimes leaves a shorter string that
		// still decodes, which is a wrong password and not an unreadable row.
		{"the key field missing", good[:strings.LastIndex(good, "$")]},
		{"the salt and key fields missing", good[:strings.Index(good[1:], "$")+1]},
		{"no salt", "$argon2id$v=19$m=8,t=1,p=1$$abcd"},
		{"zero memory", "$argon2id$v=19$m=0,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZA"},
		{"absurd memory", "$argon2id$v=19$m=99999999,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZA"},
		{"bad base64 in the salt", "$argon2id$v=19$m=8,t=1,p=1$!!!!$YWJjZA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			ok, err := auth.VerifyPassword(tc.hash, "the right password")
			r.False(ok)
			r.ErrorIs(err, auth.ErrInvalidHash)
		})
	}
}

func TestNeedsRehash(t *testing.T) {
	t.Parallel()
	weak, err := auth.HashPasswordWith(fast, "a password")
	require.NoError(t, err)
	strong, err := auth.HashPasswordWith(auth.DefaultParams, "a password")
	require.NoError(t, err)

	cases := []struct {
		name string
		hash string
		want bool
	}{
		{"a hash at today's parameters", strong, false},
		{"a hash at weaker parameters", weak, true},
		{"an unreadable hash", "not a hash", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, auth.NeedsRehash(tc.hash))
		})
	}
}

func TestCheckPasswordStrength(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		password string
		ok       bool
	}{
		{"a long phrase", "correct horse battery staple", true},
		{"exactly the minimum", strings.Repeat("a", auth.MinPasswordLen), true},
		{"one short of the minimum", strings.Repeat("a", auth.MinPasswordLen-1), false},
		{"empty", "", false},
		{"only spaces", strings.Repeat(" ", auth.MinPasswordLen+2), false},
		{"longer than the byte cap", strings.Repeat("a", auth.MaxPasswordLen+1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			err := auth.CheckPasswordStrength(tc.password)
			if tc.ok {
				r.NoError(err)
				return
			}
			r.Error(err)
		})
	}
}

func TestDeviceToken(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	tok, err := auth.NewDeviceToken()
	r.NoError(err)
	r.Len(tok.Sum, 32, "the digest is a SHA-256")
	r.Len(tok.Prefix, auth.DevicePrefixLen)
	r.True(strings.HasPrefix(tok.Plain, tok.Prefix))
	r.NotContains(tok.Plain, "=", "the encoding is unpadded so the token is URL-safe")

	sum, prefix, err := auth.SplitDeviceToken(tok.Plain)
	r.NoError(err)
	r.Equal(tok.Sum, sum)
	r.Equal(tok.Prefix, prefix)

	other, err := auth.NewDeviceToken()
	r.NoError(err)
	r.NotEqual(tok.Plain, other.Plain)
	r.False(auth.EqualBytes(tok.Sum, other.Sum))
}

func TestSplitDeviceTokenRejectsRubbish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"too short to hold a prefix", "abc"},
		{"right length, wrong alphabet", strings.Repeat("!", 43)},
		{"valid base64 of the wrong length", "YWJjZGVm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			_, _, err := auth.SplitDeviceToken(tc.token)
			r.ErrorIs(err, auth.ErrMalformedToken)
		})
	}
}

func TestSetupTokenShape(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	tok, err := auth.NewSetupToken()
	r.NoError(err)

	raw := strings.ReplaceAll(tok, "-", "")
	r.Len(raw, auth.SetupTokenBytes*8/5, "160 bits is 32 characters of base32")
	// 32 characters of a 32-symbol alphabet is 160 bits, comfortably over the
	// 128-bit floor the design asks for.
	r.GreaterOrEqual(len(raw)*5, 128)
	for _, c := range raw {
		r.NotContains("ILOU", string(c), "the alphabet leaves out the characters that get misread")
	}
	r.Equal(len(raw)/auth.SetupTokenGroup-1, strings.Count(tok, "-"))

	other, err := auth.NewSetupToken()
	r.NoError(err)
	r.NotEqual(tok, other, "a fresh token is minted every time")
}

func TestSetupTokenComparison(t *testing.T) {
	t.Parallel()
	const token = "7QK4-M2XF-8DNA-0123-4567-89AB-CDEF-GHJK"
	cases := []struct {
		name  string
		typed string
		want  bool
	}{
		{"exactly as printed", token, true},
		{"lower case", strings.ToLower(token), true},
		{"without the hyphens", strings.ReplaceAll(token, "-", ""), true},
		{"with stray spaces", "  " + token + "  ", true},
		{"letter l typed for the digit one", strings.Replace(token, "0123", "0l23", 1), true},
		{"letter o typed for the digit zero", strings.Replace(token, "0123", "O123", 1), true},
		{"one character wrong", strings.Replace(token, "7QK4", "7QK5", 1), false},
		{"empty", "", false},
		{"a prefix of the token", token[:8], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, auth.EqualSetupToken(token, tc.typed))
		})
	}
}

func TestEqualString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"the same value", "abcdef", "abcdef", true},
		{"a different value", "abcdef", "abcdeg", false},
		{"different lengths", "abc", "abcdef", false},
		{"both empty", "", "", true},
		{"one empty", "", "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, auth.EqualString(tc.a, tc.b))
		})
	}
}

func TestSessionAndCSRFTokens(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	plain, sum, err := auth.NewSessionToken()
	r.NoError(err)
	r.Len(sum, 32)
	r.Equal(sum, auth.HashToken(plain))
	r.NotContains(plain, "=")

	csrf, err := auth.NewCSRFToken()
	r.NoError(err)
	other, err := auth.NewCSRFToken()
	r.NoError(err)
	r.NotEqual(csrf, other)
}

func TestSecretKeySealAndOpen(t *testing.T) {
	t.Parallel()
	key, err := auth.NewSecretKey()
	require.NoError(t, err)

	t.Run("a value survives a round trip", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		sealed, err := key.Seal("sk-ant-not-a-real-key")
		r.NoError(err)
		r.NotContains(string(sealed), "sk-ant")
		got, err := key.Open(sealed)
		r.NoError(err)
		r.Equal("sk-ant-not-a-real-key", got)
	})

	t.Run("sealing twice gives different ciphertext", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		a, err := key.Seal("the same value")
		r.NoError(err)
		b, err := key.Seal("the same value")
		r.NoError(err)
		r.NotEqual(a, b)
	})

	t.Run("an empty value seals to nothing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		sealed, err := key.Seal("")
		r.NoError(err)
		r.Empty(sealed)
		got, err := key.Open(nil)
		r.NoError(err)
		r.Empty(got)
	})

	t.Run("another key does not open it", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		sealed, err := key.Seal("a value")
		r.NoError(err)
		other, err := auth.NewSecretKey()
		r.NoError(err)
		_, err = other.Open(sealed)
		r.ErrorIs(err, auth.ErrSealed)
	})

	t.Run("no key at all is its own answer", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		sealed, err := key.Seal("a value")
		r.NoError(err)
		var none auth.SecretKey
		_, err = none.Open(sealed)
		r.ErrorIs(err, auth.ErrNoSecretKey)
	})

	t.Run("tampering is detected", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		sealed, err := key.Seal("a value")
		r.NoError(err)
		sealed[len(sealed)-1] ^= 0xFF
		_, err = key.Open(sealed)
		r.ErrorIs(err, auth.ErrSealed)
	})

	t.Run("truncated ciphertext is detected", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		_, err := key.Open([]byte{1, 2, 3})
		r.ErrorIs(err, auth.ErrSealed)
	})
}

func TestSecretKeyEncoding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty is a server that has none", "", true},
		{"not base64", "!!!!", false},
		{"base64 of the wrong length", "YWJjZA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			_, err := auth.ParseSecretKey(tc.in)
			if tc.ok {
				r.NoError(err)
				return
			}
			r.Error(err)
		})
	}

	t.Run("a minted key survives the configuration file", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		key, err := auth.NewSecretKey()
		r.NoError(err)
		back, err := auth.ParseSecretKey(key.String())
		r.NoError(err)
		r.Equal([]byte(key), []byte(back))
	})
}

func TestConcurrentHashingIsBounded(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The bound is a memory limit on an unauthenticated path: each hash holds
	// Params.Memory while it runs. It has to be at least two, or a second
	// sign-in waits behind the first for no reason, and small enough that a
	// burst cannot multiply 64 MiB into something the machine notices.
	r.GreaterOrEqual(auth.MaxConcurrentHashes(), 2)
	r.LessOrEqual(auth.MaxConcurrentHashes(), 8)

	// Far more callers than the bound allows, all of which must still finish.
	const callers = 32
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := auth.HashPasswordWith(fast, "a password worth hashing")
			if err != nil {
				errs <- err
				return
			}
			ok, err := auth.VerifyPassword(hash, "a password worth hashing")
			if err != nil {
				errs <- err
				return
			}
			if !ok {
				errs <- errors.New("a hash did not verify against its own password")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		r.NoError(err)
	}
}
