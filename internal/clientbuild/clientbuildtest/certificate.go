package clientbuildtest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"software.sslmate.com/src/go-pkcs12"
)

// Certificates for the signing tests: a certificate authority, a code-signing
// certificate under it, and the PKCS#12 file an operator would upload.
//
// They are minted rather than checked in. A fixture certificate committed to a
// repository expires, and a test suite that starts failing on a date nobody
// chose is worse than one that mints what it needs — which takes milliseconds
// at the key sizes below.

// TestKeyBits is the RSA key size the fixtures use. It is small on purpose:
// these keys sign a fixture and are thrown away at the end of the test, and
// three thousand bits would put a second on every run that uses one.
const TestKeyBits = 2048

// CertificateOptions is what to mint. The zero value is a plain self-signed
// code-signing certificate that is valid now, which is what most tests want.
type CertificateOptions struct {
	// CommonName is who the certificate says signed. Empty is a usable
	// default rather than a certificate with no name.
	CommonName string
	// NotBefore and NotAfter are its window. Zero values are an hour ago and a
	// year from now.
	NotBefore, NotAfter time.Time
	// NoCodeSigning mints one without the extended key usage that marks it for
	// signing software — the certificate an operator exports from the wrong
	// place, which signs fine and which Windows refuses.
	NoCodeSigning bool
	// ECDSA mints an elliptic-curve key instead of RSA.
	ECDSA bool
	// Issuer is the authority to issue under. Nil is self-signed, which is
	// what an operator making their own certificate has.
	Issuer *Authority
	// Password is what the PKCS#12 file is locked with.
	Password string
}

// Authority is a certificate authority the fixtures can issue under, for the
// tests that care about the difference between a certificate an operator
// issued to themselves and one issued to them.
type Authority struct {
	Certificate *x509.Certificate
	Key         *rsa.PrivateKey
}

// NewAuthority mints a certificate authority.
func NewAuthority(tb testing.TB, name string) *Authority {
	tb.Helper()
	r := require.New(tb)

	key, err := rsa.GenerateKey(rand.Reader, TestKeyBits)
	r.NoError(err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	r.NoError(err)
	cert, err := x509.ParseCertificate(der)
	r.NoError(err)
	return &Authority{Certificate: cert, Key: key}
}

// Certificate mints a certificate and returns the PKCS#12 file holding it and
// its key — the file an operator uploads — along with the certificate itself
// for a test that wants to assert about what is inside.
func Certificate(tb testing.TB, opts CertificateOptions) ([]byte, *x509.Certificate) {
	tb.Helper()
	r := require.New(tb)

	if opts.CommonName == "" {
		opts.CommonName = "Spain GT League"
	}
	if opts.NotBefore.IsZero() {
		opts.NotBefore = time.Now().Add(-time.Hour)
	}
	if opts.NotAfter.IsZero() {
		opts.NotAfter = time.Now().Add(365 * 24 * time.Hour)
	}

	var key, public any
	if opts.ECDSA {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		r.NoError(err)
		key, public = k, &k.PublicKey
	} else {
		k, err := rsa.GenerateKey(rand.Reader, TestKeyBits)
		r.NoError(err)
		key, public = k, &k.PublicKey
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: opts.CommonName},
		NotBefore:    opts.NotBefore,
		NotAfter:     opts.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if !opts.NoCodeSigning {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}
	}

	parent, signer, chain := template, key, []*x509.Certificate(nil)
	if opts.Issuer != nil {
		parent, signer = opts.Issuer.Certificate, opts.Issuer.Key
		chain = []*x509.Certificate{opts.Issuer.Certificate}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	r.NoError(err)
	cert, err := x509.ParseCertificate(der)
	r.NoError(err)

	// Modern2023 is what a current OpenSSL and a current Windows both write.
	// A test that minted a legacy file would be testing the reader against
	// something no operator is likely to upload.
	pfx, err := pkcs12.Modern2023.Encode(key, cert, chain, opts.Password)
	r.NoError(err)
	return pfx, cert
}

// Ed25519Certificate is a certificate holding a key that is fine everywhere
// except here: Authenticode names RSA and ECDSA and nothing else, so a server
// that accepted one would make signatures no Windows machine can check.
func Ed25519Certificate(tb testing.TB) []byte {
	tb.Helper()
	r := require.New(tb)

	public, private, err := ed25519.GenerateKey(rand.Reader)
	r.NoError(err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Spain GT League"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	r.NoError(err)
	cert, err := x509.ParseCertificate(der)
	r.NoError(err)
	pfx, err := pkcs12.Modern2023.Encode(private, cert, nil, "")
	r.NoError(err)
	return pfx
}
