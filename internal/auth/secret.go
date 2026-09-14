package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
)

// SecretKeyBytes is the length of the data key that encrypts the operator's own
// API keys at rest: AES-256.
const SecretKeyBytes = 32

// ErrNoSecretKey is returned when something sealed needs opening and no key is
// available — the usual cause being a data directory that was lost while the
// database survived. It is not corruption: the operator retypes the API key and
// the feature comes back.
var ErrNoSecretKey = errors.New("auth: no secret key, so the stored API key cannot be read")

// ErrSealed is returned when a sealed value will not open with the key given.
var ErrSealed = errors.New("auth: the stored value does not open with this key")

// SecretKey encrypts the values that must not be readable in a database dump —
// today every one of them belongs to a plugin: the core holds no vendor
// credential of its own.
//
// It lives in the configuration file beside the binary, not in the database,
// which is the whole point: a dump of the database contains the ciphertext and
// nothing that opens it. The cost is that losing the data directory loses the
// stored API key, and the server says so and asks for it again rather than
// pretending the feature is on.
type SecretKey []byte

// NewSecretKey mints a data key.
func NewSecretKey() (SecretKey, error) {
	b := make([]byte, SecretKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: no randomness available: %w", err)
	}
	return b, nil
}

// String renders the key for the configuration file.
func (k SecretKey) String() string { return base64.RawStdEncoding.EncodeToString(k) }

// ParseSecretKey reads a key back out of the configuration file. An empty
// string is not an error: it is a server that has never been given one.
func ParseSecretKey(s string) (SecretKey, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawStdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != SecretKeyBytes {
		return nil, errors.New("auth: the secret key in the configuration file is not readable")
	}
	return b, nil
}

// Seal encrypts plaintext with AES-256-GCM and returns the nonce followed by
// the ciphertext. Each call uses a fresh nonce, so sealing the same value twice
// gives two different results and a dump leaks nothing by comparison.
func (k SecretKey) Seal(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	gcm, err := k.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("auth: no randomness available: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a value produced by [SecretKey.Seal]. An empty input is an
// empty result, which is how "no key was ever stored" is spelled.
func (k SecretKey) Open(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	gcm, err := k.gcm()
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", ErrSealed
	}
	out, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", ErrSealed
	}
	return string(out), nil
}

func (k SecretKey) gcm() (cipher.AEAD, error) {
	if len(k) != SecretKeyBytes {
		return nil, ErrNoSecretKey
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, fmt.Errorf("auth: cannot build the cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: cannot build the cipher: %w", err)
	}
	return gcm, nil
}

// Keyring holds the data key in a place the whole process shares, so that an
// operator who regenerates it in the admin panel does not have to restart the
// server for the API to notice.
//
// Without it the key would be a value captured when the handlers were built,
// and a regenerated key would leave every sealed credential unreadable until a
// restart — which is the one thing the danger zone must not do quietly.
type Keyring struct{ v atomic.Pointer[SecretKey] }

// NewKeyring holds k. A nil key is valid: it is a server whose configuration
// file carries no data key, and sealed values simply do not open.
func NewKeyring(k SecretKey) *Keyring {
	r := &Keyring{}
	r.Replace(k)
	return r
}

// Key is the data key in force right now. It is safe on a nil receiver, which
// is what a caller that was given no keyring has.
func (r *Keyring) Key() SecretKey {
	if r == nil {
		return nil
	}
	if p := r.v.Load(); p != nil {
		return *p
	}
	return nil
}

// Replace swaps in a new data key. Callers must have re-sealed everything that
// was sealed with the old one first: this call makes the old ciphertext
// unreadable everywhere at once.
func (r *Keyring) Replace(k SecretKey) {
	if r == nil {
		return
	}
	r.v.Store(&k)
}
