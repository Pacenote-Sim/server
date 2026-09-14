package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters. They are encoded into every hash, so
// raising them later does not invalidate the passwords already stored: an old
// hash still verifies against the parameters it was made with, and is rewritten
// at the next successful sign-in.
type Params struct {
	// Memory is the memory cost in KiB.
	Memory uint32
	// Time is the number of passes.
	Time uint32
	// Threads is the parallelism.
	Threads uint8
	// SaltLen and KeyLen are in bytes.
	SaltLen, KeyLen uint32
}

// DefaultParams is 64 MiB over three passes, comfortably above the OWASP
// minimum for argon2id. It costs roughly a tenth of a second on a laptop, which
// is what makes an offline attack on a stolen database expensive.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// hashers bounds how many argon2 hashes run at the same time.
//
// This is a memory limit, not a nicety. Each hash holds [Params.Memory] — 64
// MiB at the default — for as long as it runs, and the sign-in form is
// unauthenticated. Without a bound, a handful of concurrent attempts is a
// gigabyte of allocation that nobody had to log in to cause. Rate limiting
// slows that down; this puts a ceiling on it.
//
// The work is memory-bound rather than processor-bound, so the ceiling is small
// and the ones that do not fit wait rather than fail.
var hashers = make(chan struct{}, maxConcurrentHashes())

// MaxConcurrentHashes is how many password hashes may run at once.
func MaxConcurrentHashes() int { return cap(hashers) }

func maxConcurrentHashes() int {
	return min(max(runtime.NumCPU()/2, 2), 8)
}

// idKey is argon2id under the concurrency bound. Every call in this package
// goes through it, so the bound cannot be forgotten at a call site.
func idKey(password string, salt []byte, p Params, keyLen uint32) []byte {
	hashers <- struct{}{}
	defer func() { <-hashers }()
	return argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, keyLen)
}

// MinPasswordLen is the shortest password the server accepts. Length is the
// only rule: composition rules push people towards "Passw0rd!" and buy nothing.
const MinPasswordLen = 12

// maxHashPart bounds the salt and the key read out of a stored hash, so a
// hostile row cannot ask for an enormous allocation and so the conversions to
// uint32 are provably safe.
const maxHashPart = 1024

// MaxPasswordLen caps what is hashed, because argon2id will happily chew
// through a megabyte of input and an unauthenticated form must not be able to
// ask it to.
const MaxPasswordLen = 1024

// ErrInvalidHash is returned when a stored hash cannot be parsed. It means the
// row was corrupted or written by something else, not that the password was
// wrong, and it must never be reported to the person signing in as either.
var ErrInvalidHash = errors.New("auth: stored password hash is not readable")

// CheckPasswordStrength reports whether a password may be used, phrased for the
// person who has to choose another one.
func CheckPasswordStrength(password string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n == 0:
		return errors.New("auth: the password is empty")
	case n < MinPasswordLen:
		return fmt.Errorf("auth: that password is %d characters — use at least %d, and prefer a phrase you can remember over something short and clever", n, MinPasswordLen)
	case len(password) > MaxPasswordLen:
		return fmt.Errorf("auth: that password is longer than %d bytes", MaxPasswordLen)
	case strings.TrimSpace(password) == "":
		return errors.New("auth: the password is only spaces")
	}
	return nil
}

// HashPassword hashes a password with [DefaultParams] and returns the PHC
// string that is stored in the database.
func HashPassword(password string) (string, error) {
	return HashPasswordWith(DefaultParams, password)
}

// HashPasswordWith hashes a password with explicit parameters. Tests use it to
// stay fast; the server uses [HashPassword].
func HashPasswordWith(p Params, password string) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", fmt.Errorf("auth: that password is longer than %d bytes", MaxPasswordLen)
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: no randomness available: %w", err)
	}
	key := idKey(password, salt, p, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the stored hash. The
// comparison is constant time, and a hash that cannot be parsed is
// [ErrInvalidHash] rather than a quiet false, so a corrupted row shows up as a
// fault instead of as everyone's password being wrong.
func VerifyPassword(encoded, password string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordLen {
		return false, nil
	}
	got := idKey(password, salt, p, uint32(len(want))) //nolint:gosec // G115: decodeHash bounds the key length at maxHashPart.
	// Zero the derived key: it is as good as the password for as long as it
	// sits in memory, and this costs nothing on a path that runs once per
	// sign-in.
	defer clear(got)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether a stored hash was made with weaker parameters
// than [DefaultParams] and should be rewritten at the next successful sign-in.
func NeedsRehash(encoded string) bool {
	p, _, want, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.Memory < DefaultParams.Memory ||
		p.Time < DefaultParams.Time ||
		uint32(len(want)) < DefaultParams.KeyLen //nolint:gosec // G115: decodeHash bounds the key length at maxHashPart.
}

func decodeHash(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}
	var version int
	if _, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil || version != argon2.Version {
		return p, nil, nil, ErrInvalidHash
	}
	if _, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); scanErr != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	if salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.Strict().DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	// The upper bound makes the conversions below provably safe and rejects a
	// row that was not written by this package.
	if len(salt) == 0 || len(key) == 0 || len(salt) > maxHashPart || len(key) > maxHashPart {
		return p, nil, nil, ErrInvalidHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key)) //nolint:gosec // G115: both lengths are bounded by maxHashPart on the line above.
	// A hostile row could name a gigabyte of memory and hang the process. The
	// ceiling is a multiple of what DefaultParams asks for, so raising the real
	// parameters stays possible without touching this.
	if p.Memory > 1<<21 || p.Time > 16 || int(p.Threads) > 4*runtime.NumCPU()+8 {
		return p, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
