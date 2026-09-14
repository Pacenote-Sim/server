package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// DeviceTokenBytes is the entropy of a device token: 256 bits. It is never
// guessed, only stolen, so the length is chosen to
// end the conversation about brute force rather than to be convenient.
const DeviceTokenBytes = 32

// DevicePrefixLen is how much of a device token is kept in clear text, purely
// so a lookup is an index seek rather than a scan of every row. Eight
// characters of base64 is 48 bits, which narrows a lookup to one row in
// practice and is far too little to reconstruct anything.
const DevicePrefixLen = 8

// SessionTokenBytes is the entropy of an admin session cookie.
const SessionTokenBytes = 32

// CSRFTokenBytes is the entropy of a cross-site request forgery token.
const CSRFTokenBytes = 32

// ErrMalformedToken is returned when a presented token is not even the right
// shape. It is worth distinguishing from "no such token" only in a log line;
// the caller is told the same thing either way.
var ErrMalformedToken = errors.New("auth: token is not the right shape")

// DeviceToken is a freshly minted device credential in its three forms.
//
// Plain is shown to the client once, at pairing, and never again — it is not
// stored anywhere. Sum is what the database holds. Prefix is the clear-text
// fragment the lookup index is built on.
type DeviceToken struct {
	Plain  string
	Sum    []byte
	Prefix string
}

// NewDeviceToken mints a device token.
func NewDeviceToken() (DeviceToken, error) {
	b := make([]byte, DeviceTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return DeviceToken{}, fmt.Errorf("auth: no randomness available: %w", err)
	}
	plain := base64Raw(b)
	sum, prefix, err := SplitDeviceToken(plain)
	if err != nil {
		return DeviceToken{}, err
	}
	return DeviceToken{Plain: plain, Sum: sum, Prefix: prefix}, nil
}

// SplitDeviceToken turns a presented token into the two things the database is
// queried with: its lookup prefix and its digest. It is the only way a token
// from a request becomes a row, so the hashing cannot be forgotten at a call
// site.
func SplitDeviceToken(plain string) (sum []byte, prefix string, err error) {
	if len(plain) < DevicePrefixLen {
		return nil, "", ErrMalformedToken
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(plain)
	if err != nil || len(raw) != DeviceTokenBytes {
		return nil, "", ErrMalformedToken
	}
	h := sha256.Sum256([]byte(plain))
	return h[:], plain[:DevicePrefixLen], nil
}

// base64Raw is the one spelling every token in this package is rendered in:
// unpadded, URL-safe base64, so a token can sit in a header, a query string or
// a JSON document without being escaped anywhere.
func base64Raw(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// NewSessionToken mints an admin session token and returns it with its digest.
// The cookie carries the plain value; the database holds only the digest, so a
// dump cannot be used to sign in as anyone.
func NewSessionToken() (plain string, sum []byte, err error) {
	b := make([]byte, SessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("auth: no randomness available: %w", err)
	}
	plain = base64Raw(b)
	return plain, HashToken(plain), nil
}

// HashToken is the digest a token is stored and looked up by.
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// NewCSRFToken mints a cross-site request forgery token.
func NewCSRFToken() (string, error) {
	b := make([]byte, CSRFTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: no randomness available: %w", err)
	}
	return base64Raw(b), nil
}

// EqualString compares two secrets in constant time. Length is not secret, so
// leaking it through an early return is fine; which byte differs is, so the
// comparison itself must not stop at the first difference.
func EqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// EqualBytes compares two digests in constant time.
func EqualBytes(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// setupAlphabet is Crockford's base32 without I, L, O and U: an operator reads
// this token off a terminal and types it into a browser, and those four are the
// characters that get read wrong.
const setupAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// SetupTokenBytes is the entropy of the one-time setup token: 160 bits, which
// encodes as exactly 32 characters of the alphabet above with nothing left
// over.
const SetupTokenBytes = 20

// SetupTokenGroup is how many characters sit between the hyphens.
const SetupTokenGroup = 4

// NewSetupToken mints the token printed on the terminal at first run,
// hyphenated into groups so it can be read aloud and typed without losing
// one's place.
func NewSetupToken() (string, error) {
	b := make([]byte, SetupTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: no randomness available: %w", err)
	}
	var raw strings.Builder
	raw.Grow(SetupTokenBytes * 8 / 5)
	var acc, bits uint32
	for _, c := range b {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			raw.WriteByte(setupAlphabet[(acc>>bits)&31])
		}
	}
	return groupSetupToken(raw.String()), nil
}

func groupSetupToken(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/SetupTokenGroup)
	for i := 0; i < len(s); i += SetupTokenGroup {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+SetupTokenGroup, len(s))])
	}
	return b.String()
}

// NormaliseSetupToken puts a token the operator typed into the one spelling the
// comparison uses: upper case, no hyphens or spaces, and with the four
// characters the alphabet leaves out folded onto the ones they are mistaken
// for. Someone who types a lower-case "l" for a "1" gets in; someone who types
// the wrong token does not.
func NormaliseSetupToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		case 'U':
			b.WriteByte('V')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EqualSetupToken compares a typed token against the real one in constant time,
// after normalising both.
func EqualSetupToken(want, got string) bool {
	return EqualString(NormaliseSetupToken(want), NormaliseSetupToken(got))
}
