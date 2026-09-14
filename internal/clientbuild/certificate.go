package clientbuild

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"software.sslmate.com/src/go-pkcs12"
)

// The operator's own code-signing certificate, and the signature it makes.
//
// Signing a Windows executable is not "encrypt the file's hash". It is a PKCS#7
// SignedData whose content is a small Microsoft-specific structure naming the
// hash algorithm and the hash, signed over a set of attributes rather than over
// the content itself. None of that shape is negotiable: Windows checks it
// field by field, and a signature that is merely cryptographically correct is
// still refused if it is laid out any other way.
//
// It is written here rather than taken from a library because the libraries
// that do it well bring a code-signing service with them — key stores, terminal
// password prompts, PGP — and this server needs one small part of one of them.
// The parts it does need are two hundred lines and every one of them is
// checked against a real signature in the tests.

// The object identifiers the format is made of. They are spelled out because
// each one is a decision somebody reading this has to be able to check against
// Microsoft's own description of the format.
var (
	// oidSignedData is PKCS#7 SignedData, the envelope.
	oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	// oidSpcIndirectData is what is signed: a hash of the program, rather than
	// the program. It is why a signature can sit inside the file it covers.
	oidSpcIndirectData = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 4}
	// oidSpcPEImageData says the thing hashed was a Windows executable.
	oidSpcPEImageData = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 15}
	// oidSpcStatementType and oidIndividualCodeSigning are the claim that this
	// is a signature over software rather than over a commercial statement.
	oidSpcStatementType       = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 11}
	oidIndividualCodeSigning  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 21}
	oidSpcSpOpusInfo          = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 12}
	oidAttributeContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidAttributeMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidSHA256                 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidRSAEncryption          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidECDSAWithSHA256        = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidExtKeyUsageCodeSigning = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 3}
	_                         = oidExtKeyUsageCodeSigning
)

// signingHash is the digest every signature this server makes uses. SHA-1 is
// the only other one Authenticode defines and Windows has refused it for code
// signing since 2016, so there is no choice to offer.
const signingHash = crypto.SHA256

// ErrCertificate reports a certificate this server cannot sign with. The
// message is the operator's, because every cause is something they fix in the
// file they uploaded.
var ErrCertificate = errors.New("clientbuild")

// Certificate is the operator's code-signing certificate and its key.
//
// The key never leaves this process. It is held as a [crypto.Signer] rather
// than as key material so that the day this grows to speak to a hardware token
// or a cloud key service, nothing above this line changes.
type Certificate struct {
	// Leaf is the certificate that signs.
	Leaf *x509.Certificate
	// Chain is everything between the leaf and a root, which travels with the
	// signature so that a machine trusting the root can build the path.
	Chain []*x509.Certificate
	// Key signs.
	Key crypto.Signer
}

// CertificateInfo is everything the build page says about a stored certificate.
// It is derived from the certificate and holds no key material, so it is safe
// to put on a page and in a log.
type CertificateInfo struct {
	// Subject and Issuer are who the certificate says signed and who vouched
	// for them, as a person reads them.
	Subject, Issuer string
	// NotBefore and NotAfter are the window the signature is made inside.
	NotBefore, NotAfter time.Time
	// Thumbprint is the SHA-256 of the certificate, which is what an operator
	// compares against what their authority issued them.
	Thumbprint string
	// Algorithm is the key, in words: "RSA 3072" or "ECDSA P-256".
	Algorithm string
	// SelfSigned is whether the operator issued it to themselves, which is the
	// normal case here and changes what their drivers will see.
	SelfSigned bool
	// CodeSigning is whether the certificate is marked for signing software.
	// A certificate without it can still make a signature and Windows will not
	// accept it, so the page says so rather than letting an operator find out
	// from a driver.
	CodeSigning bool
}

// Expired reports whether the certificate can no longer sign.
func (c CertificateInfo) Expired(now time.Time) bool { return now.After(c.NotAfter) }

// Expiring reports whether the certificate runs out within a month, which is
// the warning an operator needs before a driver meets it.
func (c CertificateInfo) Expiring(now time.Time) bool {
	return !c.Expired(now) && now.Add(30*24*time.Hour).After(c.NotAfter)
}

// Describe is what the page says about this certificate.
func (c Certificate) Describe() CertificateInfo {
	sum := sha256.Sum256(c.Leaf.Raw)
	info := CertificateInfo{
		Subject:    name(c.Leaf.Subject.String(), c.Leaf.Subject.CommonName),
		Issuer:     name(c.Leaf.Issuer.String(), c.Leaf.Issuer.CommonName),
		NotBefore:  c.Leaf.NotBefore,
		NotAfter:   c.Leaf.NotAfter,
		Thumbprint: hex.EncodeToString(sum[:]),
		Algorithm:  keyWords(c.Leaf.PublicKey),
		SelfSigned: c.Leaf.Subject.String() == c.Leaf.Issuer.String(),
	}
	for _, use := range c.Leaf.ExtKeyUsage {
		if use == x509.ExtKeyUsageCodeSigning || use == x509.ExtKeyUsageAny {
			info.CodeSigning = true
		}
	}
	return info
}

// name is the common name where there is one and the whole distinguished name
// where there is not, because a certificate with no common name still has to
// render as something a person can tell apart from another.
func name(full, common string) string {
	if strings.TrimSpace(common) != "" {
		return common
	}
	return full
}

// keyWords describes a public key the way an operator would say it.
func keyWords(pub any) string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PublicKey:
		return "ECDSA " + k.Curve.Params().Name
	default:
		return fmt.Sprintf("%T", pub)
	}
}

// ParseCertificate opens a PKCS#12 file — a .pfx or .p12, which is what every
// certificate authority hands out and what Windows exports — and checks that
// what came out of it can actually sign.
//
// Every failure here is the operator's to fix and is phrased as such. The
// commonest by a distance is the wrong password, and it is worth saying which
// of the two things went wrong: a password that is merely wrong and a file that
// is not a certificate store at all need different fixes.
func ParseCertificate(pfx []byte, password string) (Certificate, error) {
	if len(pfx) == 0 {
		return Certificate{}, fmt.Errorf("%w: there was no file to read", ErrCertificate)
	}
	key, leaf, chain, err := pkcs12.DecodeChain(pfx, password)
	if err != nil {
		if strings.Contains(err.Error(), "incorrect password") ||
			strings.Contains(err.Error(), "decryption password incorrect") {
			return Certificate{}, fmt.Errorf("%w: that password does not open the file", ErrCertificate)
		}
		return Certificate{}, fmt.Errorf("%w: that file is not a certificate store this server can read — %w",
			ErrCertificate, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return Certificate{}, fmt.Errorf("%w: the key in that file is a %T, which cannot sign", ErrCertificate, key)
	}
	switch signer.Public().(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey:
	default:
		return Certificate{}, fmt.Errorf(
			"%w: the key in that file is a %T, and Windows accepts only RSA and ECDSA signatures",
			ErrCertificate, signer.Public())
	}
	cert := Certificate{Leaf: leaf, Chain: chain, Key: signer}
	// The switch above vetted the private key. This is the certificate's own
	// key, which is a separate value in the file and need not be the same kind
	// of thing — a store holding a DSA certificate beside an RSA key is
	// malformed, not impossible. Every key type this server accepts has Equal;
	// asserting without checking would turn that malformed file into a panic
	// on an upload the operator is allowed to make.
	checkable, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok {
		return Certificate{}, fmt.Errorf(
			"%w: the certificate in that file holds a %T, and Windows accepts only RSA and ECDSA signatures",
			ErrCertificate, leaf.PublicKey)
	}
	if !checkable.Equal(signer.Public()) {
		return Certificate{}, fmt.Errorf("%w: the key in that file does not belong to the certificate in it",
			ErrCertificate)
	}
	return cert, nil
}

// Opus is what Windows shows a driver about the program itself, beside who
// signed it. Both parts are optional and both are worth filling in: the dialog
// a driver sees says "unknown" for anything left out.
type Opus struct {
	// Program is the program's name.
	Program string
	// URL is where a driver can read about it.
	URL string
}

// Sign signs a stamped client with the operator's certificate and returns the
// signed file.
//
// It must be the last thing done to the bytes. Every edit to a signed Windows
// executable breaks its signature, which is why stamping happens first and why
// nothing here writes into the program.
func Sign(body []byte, cert Certificate, opus Opus) ([]byte, error) {
	layout, err := readPE(body)
	if err != nil {
		return nil, err
	}
	prepared := prepare(body, layout)

	digest := signingHash.New()
	peDigest(prepared, layout, digest)

	indirect, err := indirectData(digest.Sum(nil))
	if err != nil {
		return nil, err
	}
	signature, err := signedData(indirect, cert, opus)
	if err != nil {
		return nil, err
	}
	return attach(prepared, layout, signature)
}

// indirectData is the structure Authenticode actually signs: the hash of the
// program, the algorithm that made it, and a note that the thing hashed was a
// Windows executable.
func indirectData(digest []byte) ([]byte, error) {
	return asn1.Marshal(struct {
		Data struct {
			Type  asn1.ObjectIdentifier
			Value asn1.RawValue
		}
		MessageDigest struct {
			Algorithm algorithmIdentifier
			Digest    []byte
		}
	}{
		Data: struct {
			Type  asn1.ObjectIdentifier
			Value asn1.RawValue
		}{Type: oidSpcPEImageData, Value: asn1.RawValue{FullBytes: peImageData()}},
		MessageDigest: struct {
			Algorithm algorithmIdentifier
			Digest    []byte
		}{Algorithm: algorithmIdentifier{Algorithm: oidSHA256, Parameters: nullValue()}, Digest: digest},
	})
}

// peImageData is the SpcPeImageData every signing tool writes, and it is
// almost entirely ceremony: an empty flags field, and a file link holding the
// string "<<<Obsolete>>>" in UTF-16.
//
// It is obsolete because it once named the file being signed, which is a fact
// nobody wanted inside a signature. Windows still requires the field, and
// still requires that exact string, so it is written out here with a comment
// rather than left looking like a mistake.
func peImageData() []byte {
	link := tagged(0, true, tagged(2, true, tagged(0, false, utf16BE("<<<Obsolete>>>"))))
	// An empty BIT STRING: no flags set.
	flags := []byte{0x03, 0x01, 0x00}
	return sequence(slices.Concat(flags, link))
}

// signedData builds the PKCS#7 envelope: the content, the certificates that
// explain who signed, and one signer.
func signedData(content []byte, cert Certificate, opus Opus) ([]byte, error) {
	// What the messageDigest attribute covers is the inside of the content,
	// not the content. PKCS#7 hashes the octets of the thing signed, and for
	// this content type the thing signed is a SEQUENCE, so its own tag and
	// length are not part of it. Hashing the whole encoding instead is the one
	// mistake in this file that produces a signature every verifier rejects
	// and no structural check catches.
	var inside asn1.RawValue
	if _, err := asn1.Unmarshal(content, &inside); err != nil {
		return nil, err
	}
	attributes, err := signedAttributes(inside.Bytes, opus)
	if err != nil {
		return nil, err
	}
	// The signature covers the attributes as a SET, which is not the tag they
	// are stored under. Signing the stored form is the classic way to get this
	// wrong, and it produces a signature that verifies nowhere.
	signable := set(attributes)
	stored := tagged(0, true, attributes)

	sum := sha256.Sum256(signable)
	signature, err := cert.Key.Sign(rand.Reader, sum[:], signingHash)
	if err != nil {
		return nil, fmt.Errorf("%w: the certificate's key would not sign — %w", ErrCertificate, err)
	}

	var certificates []byte
	for _, c := range append([]*x509.Certificate{cert.Leaf}, cert.Chain...) {
		certificates = append(certificates, c.Raw...)
	}

	signerAlgorithm := algorithmIdentifier{Algorithm: oidRSAEncryption, Parameters: nullValue()}
	if _, ok := cert.Key.Public().(*ecdsa.PublicKey); ok {
		signerAlgorithm = algorithmIdentifier{Algorithm: oidECDSAWithSHA256}
	}

	// The content and the envelope both sit under an explicit [0], and both
	// are wrapped by hand. encoding/asn1 takes a pre-encoded value as final
	// and drops any tag asked for alongside it, which produces a structure
	// that parses and is missing a layer — the kind of mistake that is
	// invisible until something else tries to read it.
	inner, err := asn1.Marshal(struct {
		Version          int
		DigestAlgorithms []algorithmIdentifier `asn1:"set"`
		ContentInfo      struct {
			ContentType asn1.ObjectIdentifier
			Content     asn1.RawValue
		}
		Certificates asn1.RawValue `asn1:"optional"`
		SignerInfos  []signerInfo  `asn1:"set"`
	}{
		Version:          1,
		DigestAlgorithms: []algorithmIdentifier{{Algorithm: oidSHA256, Parameters: nullValue()}},
		ContentInfo: struct {
			ContentType asn1.ObjectIdentifier
			Content     asn1.RawValue
		}{ContentType: oidSpcIndirectData, Content: asn1.RawValue{FullBytes: tagged(0, true, content)}},
		Certificates: asn1.RawValue{FullBytes: tagged(0, true, certificates)},
		SignerInfos: []signerInfo{{
			Version: 1,
			Signer: issuerAndSerial{
				Issuer: asn1.RawValue{FullBytes: cert.Leaf.RawIssuer},
				Serial: cert.Leaf.SerialNumber,
			},
			DigestAlgorithm:    algorithmIdentifier{Algorithm: oidSHA256, Parameters: nullValue()},
			SignedAttributes:   asn1.RawValue{FullBytes: stored},
			SignatureAlgorithm: signerAlgorithm,
			Signature:          signature,
		}},
	})
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue
	}{ContentType: oidSignedData, Content: asn1.RawValue{FullBytes: tagged(0, true, inner)}})
}

// signerInfo is one signature, as PKCS#7 lays it out. The tag on the attributes
// is [0] IMPLICIT rather than a SET, which is why they are carried raw.
type signerInfo struct {
	Version            int
	Signer             issuerAndSerial
	DigestAlgorithm    algorithmIdentifier
	SignedAttributes   asn1.RawValue
	SignatureAlgorithm algorithmIdentifier
	Signature          []byte
}

// issuerAndSerial names the certificate that made a signature without carrying
// it, which is how a verifier finds the right one among those enclosed.
type issuerAndSerial struct {
	Issuer asn1.RawValue
	Serial *big.Int
}

// algorithmIdentifier is an algorithm and whatever it is parameterised by.
type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

// signedAttributes is what the signature actually covers, concatenated in the
// order DER requires a SET to be in.
//
// Four of them. Two are PKCS#7's own and mandatory: what kind of thing was
// signed, and its hash. Two are Microsoft's: a statement that this is a
// signature over software, and the program's name as a driver will see it in
// the dialog Windows shows them.
func signedAttributes(content []byte, opus Opus) ([]byte, error) {
	digest := sha256.Sum256(content)
	statement, err := asn1.Marshal(struct{ Type asn1.ObjectIdentifier }{Type: oidIndividualCodeSigning})
	if err != nil {
		return nil, err
	}

	var encoded [][]byte
	for _, a := range []struct {
		oid   asn1.ObjectIdentifier
		value any
	}{
		{oidAttributeContentType, oidSpcIndirectData},
		{oidAttributeMessageDigest, digest[:]},
		{oidSpcStatementType, asn1.RawValue{FullBytes: statement}},
		{oidSpcSpOpusInfo, asn1.RawValue{FullBytes: opusInfo(opus)}},
	} {
		value, err := asn1.Marshal(a.value)
		if err != nil {
			return nil, err
		}
		attribute, err := asn1.Marshal(struct {
			Type  asn1.ObjectIdentifier
			Value asn1.RawValue
		}{Type: a.oid, Value: asn1.RawValue{FullBytes: set(value)}})
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, attribute)
	}
	// DER orders the members of a SET by their own encodings. Nothing in
	// Windows minds the order, but a verifier that re-encodes what it read
	// does, and the whole point of a signature is that everyone agrees on the
	// bytes.
	slices.SortFunc(encoded, slices.Compare)
	return slices.Concat(encoded...), nil
}

// opusInfo is the program's name and address, both optional.
func opusInfo(opus Opus) []byte {
	var body []byte
	if opus.Program != "" {
		body = append(body, tagged(0, true, tagged(0, false, utf16BE(opus.Program)))...)
	}
	if opus.URL != "" {
		// A URL is an IA5String under an implicit [0], inside an explicit [1].
		body = append(body, tagged(1, true, tagged(0, false, []byte(opus.URL)))...)
	}
	return sequence(body)
}

// tagged wraps a body in a context-specific tag, which is how ASN.1 spells
// "this field, in this position" — the notation that makes up most of the
// Microsoft structures above. Tags above 30 need a longer form and none of
// these structures uses one, so the tag is a single byte.
func tagged(tag int, compound bool, body []byte) []byte {
	identifier := byte(0x80) | byte(tag) //nolint:gosec // G115: every tag here is 0, 1 or 2.
	if compound {
		identifier |= 0x20
	}
	return tlv(identifier, body)
}

// sequence and set are the two universal constructed types these structures
// are built out of.
func sequence(body []byte) []byte { return tlv(0x30, body) }
func set(body []byte) []byte      { return tlv(0x31, body) }

// tlv is one DER value: an identifier, a length, and the body.
//
// It is written here rather than taken from encoding/asn1 because that package
// marshals Go values and everything above is already encoded — asking it to
// wrap pre-encoded bytes means handing it a RawValue, and a RawValue carrying
// its own encoding is returned unchanged with any tag the caller asked for
// silently dropped. That behaviour cost an afternoon: the envelope came out
// one layer flatter than the format wants, parsed cleanly, and verified
// nowhere.
//
// DER writes a length below 128 as one byte, and anything larger as a count of
// length bytes followed by the length itself, most significant first.
func tlv(identifier byte, body []byte) []byte {
	out := []byte{identifier}
	if n := len(body); n < 0x80 {
		out = append(out, byte(n))
	} else {
		var length []byte
		for v := n; v > 0; v >>= 8 {
			length = append([]byte{byte(v)}, length...)
		}
		out = append(out, byte(0x80|len(length))) //nolint:gosec // G115: a signature is kilobytes, so at most three length bytes.
		out = append(out, length...)
	}
	return append(out, body...)
}

// nullValue is an explicit ASN.1 NULL, which RSA algorithm identifiers carry
// and which is not the same thing as leaving the parameters out.
func nullValue() asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagNull, FullBytes: []byte{0x05, 0x00}}
}

// utf16BE renders a string the way Microsoft's BMPString wants it: two bytes
// per character, most significant first.
func utf16BE(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for _, r := range utf16.Encode([]rune(s)) {
		// Splitting a uint16 into its two bytes is the conversion, not a loss
		// of one.
		//nolint:gosec // G115: deliberate truncation, twice, by definition.
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}
