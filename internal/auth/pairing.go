package auth

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// DeviceCodeBytes is the entropy of the device code a client polls with: 256
// bits, the same as the token it is exchanged for. It is a secret, so it is
// stored the way every secret here is — as its SHA-256.
const DeviceCodeBytes = 32

// UserCodeLen is how many characters the driver reads out to the operator. Six
// characters of the 32-letter alphabet below is thirty bits, which is far more
// than a code that lives for ten minutes behind a rate limiter needs, and short
// enough to say down a voice channel without repeating it.
const UserCodeLen = 6

// UserCodeGroup is how many characters sit before the hyphen: "H4T-9KQ".
const UserCodeGroup = 3

// PairingCodes is one device-code grant in the three forms the flow needs.
//
// DeviceCode is returned to the client once and never stored; DeviceCodeSum is
// what the pairings row holds. UserCode is the short code the driver reads out
// and an operator matches in the admin panel, and it is not a secret — it is
// useless without the device code.
type PairingCodes struct {
	DeviceCode    string
	DeviceCodeSum []byte
	UserCode      string
}

// NewPairingCodes mints one pairing's codes.
func NewPairingCodes() (PairingCodes, error) {
	b := make([]byte, DeviceCodeBytes)
	if _, err := rand.Read(b); err != nil {
		return PairingCodes{}, fmt.Errorf("auth: no randomness available: %w", err)
	}
	device := base64Raw(b)
	user, err := NewUserCode()
	if err != nil {
		return PairingCodes{}, err
	}
	return PairingCodes{DeviceCode: device, DeviceCodeSum: HashToken(device), UserCode: user}, nil
}

// NewUserCode mints the short code on its own, for the case where a collision
// forces a second attempt.
func NewUserCode() (string, error) {
	b := make([]byte, UserCodeLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: no randomness available: %w", err)
	}
	var raw strings.Builder
	raw.Grow(UserCodeLen + 1)
	for i, c := range b {
		if i > 0 && i%UserCodeGroup == 0 {
			raw.WriteByte('-')
		}
		// The modulo is a negligible bias over a 32-letter alphabet drawn from
		// 256 values — 256 is exactly eight times 32, so there is none at all.
		raw.WriteByte(setupAlphabet[int(c)%len(setupAlphabet)])
	}
	return raw.String(), nil
}

// NormaliseUserCode puts a code an operator typed into the one spelling the
// comparison uses, folding the four characters the alphabet leaves out onto the
// ones they are misread as. It is [NormaliseSetupToken] under a name that says
// what it is being used for.
func NormaliseUserCode(s string) string { return NormaliseSetupToken(s) }
