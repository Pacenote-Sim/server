package clientbuild_test

import (
	"crypto"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
)

// Reading the file an operator uploads, and saying what is in it.
//
// Every failure here is one the operator has to fix, so every one of these
// tests is as much about the sentence as about the refusal: "that password does
// not open the file" and "that file is not a certificate store" need different
// actions, and a single message covering both would leave somebody retyping a
// password that was right all along.

func TestACertificateIsReadAndDescribed(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	expires := time.Now().Add(200 * 24 * time.Hour).Truncate(time.Second)
	pfx, want := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League", NotAfter: expires, Password: "trackside",
	})

	cert, err := clientbuild.ParseCertificate(pfx, "trackside")
	r.NoError(err)
	info := cert.Describe()

	r.Equal("Spain GT League", info.Subject)
	r.Equal("Spain GT League", info.Issuer, "a self-signed certificate vouches for itself")
	r.True(info.SelfSigned)
	r.True(info.CodeSigning)
	r.Equal("RSA 2048", info.Algorithm)
	r.WithinDuration(expires, info.NotAfter, time.Second)
	r.Len(info.Thumbprint, 64, "the thumbprint is not a SHA-256")
	r.NotEqual(want.Subject.CommonName, info.Thumbprint)

	r.False(info.Expired(time.Now()))
	r.False(info.Expiring(time.Now()))
	r.True(info.Expiring(expires.Add(-24*time.Hour)), "a certificate a day from expiry is not expiring")
	r.True(info.Expired(expires.Add(time.Second)))
	r.False(info.Expiring(expires.Add(time.Second)), "an expired certificate is expired, not expiring")
}

func TestACertificateFromAnAuthority(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	ca := clientbuildtest.NewAuthority(t, "Spain GT League Authority")
	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League Signing", Issuer: ca, Password: "x",
	})

	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	info := cert.Describe()
	r.Equal("Spain GT League Signing", info.Subject)
	r.Equal("Spain GT League Authority", info.Issuer)
	r.False(info.SelfSigned, "a certificate somebody else issued read as self-signed")
	r.Len(cert.Chain, 1, "the authority did not come with the certificate")
}

// A certificate exported from the wrong place: it has a key, it signs, and
// Windows refuses the signature because it is not marked for signing software.
// It is stored rather than refused, and the page says what will happen — an
// operator who has only this one still gets a signature that says who built the
// file, which is worth something inside a league.
func TestACertificateNotMarkedForCodeSigning(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		NoCodeSigning: true, Password: "x",
	})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err, "a certificate without the code-signing marking was refused outright")
	r.False(cert.Describe().CodeSigning)
}

func TestAnECDSACertificateIsDescribed(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{ECDSA: true, Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	r.Equal("ECDSA P-256", cert.Describe().Algorithm)
}

func TestACertificateWithNoPassword(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{})
	cert, err := clientbuild.ParseCertificate(pfx, "")
	r.NoError(err)
	r.NotEmpty(cert.Describe().Subject)
}

func TestWhatIsNotACertificate(t *testing.T) {
	t.Parallel()

	good, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "trackside"})

	cases := []struct {
		name     string
		body     []byte
		password string
		want     string
	}{
		{name: "nothing at all", want: "no file to read"},
		{
			name: "the wrong password", body: good, password: "pitlane",
			want: "password does not open the file",
		},
		{
			name: "no password where one is needed", body: good,
			want: "password does not open the file",
		},
		{
			name: "a text file", body: []byte("-----BEGIN CERTIFICATE-----\nnot really\n"),
			want: "not a certificate store",
		},
		{
			// The commonest wrong file: the certificate on its own, exported
			// without its key. It looks like the right thing and cannot sign.
			name: "a certificate with no key", body: certificateOnly(t), want: "not a certificate store",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			_, err := clientbuild.ParseCertificate(tc.body, tc.password)
			r.ErrorIs(err, clientbuild.ErrCertificate)
			r.Contains(err.Error(), tc.want)
			// Whatever went wrong, the password the operator typed is not
			// repeated back in a string that ends up in a log.
			r.NotContains(err.Error(), "pitlane")
			r.NotContains(err.Error(), "trackside")
		})
	}
}

// certificateOnly is a DER certificate with no key and no PKCS#12 around it.
func certificateOnly(tb testing.TB) []byte {
	tb.Helper()
	_, cert := clientbuildtest.Certificate(tb, clientbuildtest.CertificateOptions{})
	return cert.Raw
}

// A key Windows has no way to check. Ed25519 is a perfectly good signing key
// and Authenticode does not define it, so a certificate holding one is refused
// with the reason rather than accepted into a signature nothing will verify.
func TestAKeyWindowsCannotUse(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx := clientbuildtest.Ed25519Certificate(t)
	_, err := clientbuild.ParseCertificate(pfx, "")
	r.ErrorIs(err, clientbuild.ErrCertificate)
	r.Contains(err.Error(), "only RSA and ECDSA")
}

// A key that will not sign — a hardware token that has been unplugged, in the
// shape this code can be given one. The failure is the certificate's and says
// so, because the operator's next move is to check the token and not the file.
func TestAKeyThatWillNotSign(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	cert.Key = refusingKey{cert.Key}

	_, err = clientbuild.Sign(clientbuildtest.Client(clientbuildtest.BlankRegion()), cert, clientbuild.Opus{})
	r.ErrorIs(err, clientbuild.ErrCertificate)
	r.Contains(err.Error(), "would not sign")
}

// refusingKey is a key that is there and will not do the one thing wanted of it.
type refusingKey struct{ crypto.Signer }

func (refusingKey) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("the token is not plugged in")
}
