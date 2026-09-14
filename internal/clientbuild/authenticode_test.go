package clientbuild_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	// An independent Authenticode implementation. The point of this test is
	// that something which is not this package agrees the signature is valid,
	// which no amount of testing against our own reader would establish.
	//nolint:depguard // as above: the second opinion is the assertion.
	"github.com/sassoftware/relic/v8/lib/authenticode"
	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
)

// Signing a client, checked against somebody else's implementation.
//
// These tests verify with relic, which is an Authenticode implementation
// written by other people, used in production to sign software that Windows
// accepts, and imported here only by tests. That independence is the whole
// value of it: this package could hash the wrong bytes in exactly the way it
// then checked for, and a test written against its own idea of the format
// would pass. It found two real faults while this was being written — a
// missing layer of ASN.1 tagging, and a digest taken over a structure's
// encoding where the format wants the encoding's contents — neither of which a
// structural assertion would have caught.
//
// There is no Windows machine in this project, so this is as close to the
// article as it gets. It is much closer than nothing.

// signed is a fixture client, stamped and signed, and the certificate it was
// signed with.
func signed(tb testing.TB, opts clientbuildtest.CertificateOptions) []byte {
	tb.Helper()
	r := require.New(tb)

	pfx, _ := clientbuildtest.Certificate(tb, opts)
	cert, err := clientbuild.ParseCertificate(pfx, opts.Password)
	r.NoError(err)

	out, err := clientbuild.Sign(clientbuildtest.Client(clientbuildtest.BlankRegion()), cert,
		clientbuild.Opus{Program: "Pacenote", URL: "https://pacenote.example.com"})
	r.NoError(err)
	return out
}

// verified runs the signature past relic and returns what it read out of it.
func verified(tb testing.TB, body []byte) authenticode.PESignature {
	tb.Helper()
	r := require.New(tb)

	sigs, err := authenticode.VerifyPE(bytes.NewReader(body), false)
	r.NoError(err, "an independent verifier refused the signature this server made")
	r.Len(sigs, 1, "a client should carry exactly one signature")
	return sigs[0]
}

func TestASignedClientVerifies(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := signed(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League", Password: "trackside",
	})
	sig := verified(t, body)

	r.Equal(crypto.SHA256, sig.ImageHashFunc, "signed with something other than SHA-256")
	r.Equal("Spain GT League", sig.Certificate.Subject.CommonName)
	// What Windows shows a driver in the dialog that asks whether to run it.
	r.Equal("Pacenote", sig.OpusInfo.ProgramName.String())
	r.Equal("https://pacenote.example.com", sig.OpusInfo.MoreInfo.URL)
}

// A certificate issued by an authority signs, and the authority travels with
// the signature. Without the chain a machine that trusts the authority still
// could not build a path to it, so the signature would be useless to exactly
// the people it was for.
func TestASignatureCarriesTheChain(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	ca := clientbuildtest.NewAuthority(t, "Spain GT League Authority")
	body := signed(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League Signing", Issuer: ca, Password: "trackside",
	})
	sig := verified(t, body)

	r.Equal("Spain GT League Signing", sig.Certificate.Subject.CommonName)
	var found bool
	for _, c := range sig.Intermediates {
		if c.Subject.CommonName == "Spain GT League Authority" {
			found = true
		}
	}
	r.True(found, "the authority that issued the certificate is not in the signature")
}

func TestAnECDSACertificateSigns(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := signed(t, clientbuildtest.CertificateOptions{ECDSA: true, Password: "trackside"})
	sig := verified(t, body)
	r.Equal("ECDSA", sig.Certificate.PublicKeyAlgorithm.String())
}

// The signature covers the program, and a verifier says so by recomputing the
// hash. This is the assertion that matters most: a signature that verifies as
// a signature but covers the wrong bytes would let somebody change the client
// after it was signed.
func TestTheSignatureCoversTheProgram(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := signed(t, clientbuildtest.CertificateOptions{Password: "trackside"})
	verified(t, body) // relic recomputes the image hash and compares it.

	// One byte changed inside the program, and the signature no longer covers
	// what is there. The byte is chosen inside the section rather than in the
	// headers so that this is a change to the program itself.
	tampered := bytes.Clone(body)
	tampered[0x500] ^= 0xFF
	_, err := authenticode.VerifyPE(bytes.NewReader(tampered), false)
	r.Error(err, "a client edited after signing still passed")
	r.Contains(err.Error(), "digest mismatch")
}

// Stamping and signing in the order the builder does them. Signing has to come
// last: any edit to a signed executable breaks the signature, so a server that
// stamped after signing would hand out files that fail to verify.
func TestSigningAfterStampingKeepsBoth(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)

	stamped := clientbuildtest.Client(clientbuildtest.BlankRegion())
	r.NoError(clientbuild.Write(stamped, clientbuild.Stamp{
		Address: "https://pacenote.example.com", Build: "0011223344556677",
	}))
	out, err := clientbuild.Sign(stamped, cert, clientbuild.Opus{})
	r.NoError(err)

	verified(t, out)
	read, err := clientbuild.Read(out)
	r.NoError(err, "the address could not be read out of the signed copy")
	r.Equal("https://pacenote.example.com", read.Address)
	r.Equal("0011223344556677", read.Build)
}

// A signature is added on the end and nothing before it moves. This is what
// makes the two operations compose: the stamped program is still there, byte
// for byte, at the offsets it was at.
func TestSigningOnlyAppends(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	before := clientbuildtest.Client(clientbuildtest.BlankRegion())
	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	after, err := clientbuild.Sign(before, cert, clientbuild.Opus{})
	r.NoError(err)

	r.Greater(len(after), len(before), "signing did not add a signature")
	r.Equal(before[0x400:], after[0x400:len(before)],
		"signing changed the program rather than only appending to it")

	// The two things in the headers that do change, and nothing else: the
	// directory entry that points at the signature, and the checksum.
	changed := 0
	for i := range before[:0x400] {
		if before[i] != after[i] {
			changed++
		}
	}
	r.LessOrEqual(changed, 12, "signing rewrote more of the headers than the two fields it should")
}

// Signing twice replaces the signature rather than stacking a second one on
// the end. A file carrying two is one Windows reads differently from either.
func TestSigningAgainReplaces(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "First", Password: "x",
	})
	first, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	once, err := clientbuild.Sign(clientbuildtest.Client(clientbuildtest.BlankRegion()), first, clientbuild.Opus{})
	r.NoError(err)

	pfx, _ = clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "Second", Password: "x",
	})
	second, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	twice, err := clientbuild.Sign(once, second, clientbuild.Opus{})
	r.NoError(err)

	sig := verified(t, twice)
	r.Equal("Second", sig.Certificate.Subject.CommonName)
}

// A signature on the real article. The fixture is a few kilobytes with one
// section; a client is eleven megabytes with a dozen, debug data after the last
// of them, and a length that is not a round number. Every one of those is a
// place the hash could go wrong in a way the fixture cannot show.
func TestSigningAClientWithSeveralSections(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)

	body := clientbuildtest.MultiSectionPE(clientbuildtest.BlankRegion())
	out, err := clientbuild.Sign(body, cert, clientbuild.Opus{})
	r.NoError(err)
	verified(t, out)
}

// The sections are hashed in the order they sit in the file, not the order the
// section table lists them in. The two are the same in anything a linker
// produces, which is exactly why getting it wrong would go unnoticed until it
// met a file where they differ.
//
// This cannot be checked against relic, which assumes the table is already in
// file order and refuses a file where it is not. It is checked against
// [referenceDigest] below instead — a second, deliberately literal reading of
// the same specification — and that reference is itself checked against relic
// on a file relic will read, so the two agree about the ordinary case before
// either is trusted about the awkward one.
func TestTheHashFollowsTheFileAndNotTheSectionTable(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := clientbuildtest.MultiSectionPE(clientbuildtest.BlankRegion())
	shuffled := clientbuildtest.ShuffleSectionTable(body)
	r.NotEqual(body, shuffled, "the fixture did not actually reorder the table")

	// The reference agrees with relic about a file relic will read.
	inOrder := sign(t, body)
	r.Equal(verified(t, inOrder).Indirect.MessageDigest.Digest, referenceDigest(t, inOrder),
		"the reference in this file disagrees with relic about an ordinary client")

	// And the signature over the reordered file covers what the reference says
	// it should, which it can only do by reading the sections in file order.
	outOfOrder := sign(t, shuffled)
	r.Equal(referenceDigest(t, outOfOrder), embeddedDigest(t, outOfOrder),
		"reordering the section table changed which bytes the signature covers")
}

// sign is a fixture client signed with a throwaway certificate.
func sign(tb testing.TB, body []byte) []byte {
	tb.Helper()
	r := require.New(tb)

	pfx, _ := clientbuildtest.Certificate(tb, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	out, err := clientbuild.Sign(body, cert, clientbuild.Opus{})
	r.NoError(err)
	return out
}

// embeddedDigest is the image hash inside a signed file, read out with relic's
// parser and without its verifier — which is the only part of relic that can
// look at a file whose section table is not in order.
func embeddedDigest(tb testing.TB, body []byte) []byte {
	tb.Helper()
	r := require.New(tb)

	sigs, err := authenticode.VerifyPE(bytes.NewReader(body), true)
	r.NoError(err)
	r.Len(sigs, 1)
	return sigs[0].Indirect.MessageDigest.Digest
}

// referenceDigest is the Authenticode image hash, written out the long way
// from Microsoft's description of it rather than from the code under test.
//
// It exists to disagree. Written against the same specification by the same
// person on the same afternoon it would be worth very little, so it is pinned
// to relic by the test that uses it: the two have to agree about a file relic
// can read before this one is believed about a file relic cannot.
func referenceDigest(tb testing.TB, body []byte) []byte {
	tb.Helper()

	// The optional header, and the two windows left out of the hash: the
	// header checksum, and the directory entry that points at the signature.
	optAt := int(binary.LittleEndian.Uint32(body[0x3c:])) + 24
	checkSum := optAt + 64
	certDir := optAt + 112 + 4*8
	headers := int(binary.LittleEndian.Uint32(body[optAt+60:]))

	h := sha256.New()
	h.Write(body[:checkSum])
	h.Write(body[checkSum+4 : certDir])
	h.Write(body[certDir+8 : headers])

	// Every section with bytes on disk, in the order those bytes appear in the
	// file — which is what the section table happens to say and is not the
	// same statement.
	type span struct{ at, size int }
	var spans []span
	count := int(binary.LittleEndian.Uint16(body[optAt-24+4+2:]))
	table := optAt + int(binary.LittleEndian.Uint16(body[optAt-24+4+16:]))
	for i := range count {
		row := body[table+i*40:]
		size := int(binary.LittleEndian.Uint32(row[16:]))
		at := int(binary.LittleEndian.Uint32(row[20:]))
		if size > 0 {
			spans = append(spans, span{at: at, size: size})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].at < spans[j].at })

	hashed := headers
	for _, s := range spans {
		h.Write(body[s.at : s.at+s.size])
		hashed += s.size
	}

	// Whatever is left between the last section and the signature.
	end := len(body)
	if size := int(binary.LittleEndian.Uint32(body[certDir+4:])); size > 0 {
		end = int(binary.LittleEndian.Uint32(body[certDir:]))
	}
	if end > hashed {
		h.Write(body[hashed:end])
	}
	return h.Sum(nil)
}

// A file whose length is not a multiple of eight is padded up to one before the
// signature is put on the end, and that padding is inside the region the
// signature covers.
//
// It has to be, and the reason is not obvious: a verifier finds the end of the
// hashed region by reading where the certificate table starts, so anything
// written before the table is inside what it hashes. Padding a file and then
// hashing what it used to be produces a signature that verifies nowhere, and
// nothing about the resulting file looks wrong.
func TestTheAlignmentPaddingIsInsideTheSignature(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The fixture's trailing bytes make its length a number that is not a
	// multiple of eight, which is the case this is about.
	body := clientbuildtest.MultiSectionPE(clientbuildtest.BlankRegion())
	r.NotZero(len(body)%8, "the fixture is already aligned, so this test proves nothing")

	out := sign(t, body)
	verified(t, out)

	// The program is where it was — everything past the headers, which is the
	// part signing has no business touching — the file grew to the boundary,
	// and the signature starts there.
	headers := int(binary.LittleEndian.Uint32(out[0x80+24+60:]))
	r.Equal(body[headers:], out[headers:len(body)], "signing changed the program while padding it")
	dir := 0x80 + 24 + 112 + 4*8
	at := int(binary.LittleEndian.Uint32(out[dir:]))
	r.Equal((len(body)+7)/8*8, at, "the signature does not start on the boundary after the program")
	r.Equal(make([]byte, at-len(body)), out[len(body):at], "the padding is not zeroes")
}

func TestSigningRefusesWhatIsNotAWindowsExecutable(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)

	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"empty", nil, "DOS header"},
		{"a text file", []byte("this is not a program, it is a note"), "DOS header"},
		{"a DOS header pointing nowhere", dosOnly(), "outside the file"},
		{"a DOS header pointing at rubbish", noPESignature(), "no PE header"},
		{"headers longer than the file", shortHeaders(t), "cannot be right"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := clientbuild.Sign(tc.body, cert, clientbuild.Opus{})
			require.ErrorIs(t, err, clientbuild.ErrNotPE)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// dosOnly is a file that begins like a program and does not continue like one.
func dosOnly() []byte {
	b := make([]byte, 0x40)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x7FFFFFF0)
	return b
}

// noPESignature points at a place in the file where the PE header is not.
func noPESignature() []byte {
	b := make([]byte, 0x100)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	return b
}

// shortHeaders is a real PE claiming its headers are longer than the whole
// file, which is the shape of a truncated download.
func shortHeaders(tb testing.TB) []byte {
	tb.Helper()
	b := clientbuildtest.PE(clientbuildtest.BlankRegion())
	// SizeOfHeaders, in the optional header, past the end of the file.
	binary.LittleEndian.PutUint32(b[0x80+4+20+60:], 0x7FFFFFF0)
	return b
}

// What the file looks like after signing, read back from the header rather
// than taken on trust: the certificate table is where the directory says, it
// is the length it says, and it is on the boundary Windows expects.
func TestTheCertificateTableIsWhereTheHeaderSaysItIs(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := signed(t, clientbuildtest.CertificateOptions{Password: "x"})

	// Data directory 4 of a PE32+ optional header, which starts 24 bytes after
	// the PE signature. Spelled out rather than read with this package's own
	// code, so that the two have to agree.
	dir := 0x80 + 24 + 112 + 4*8
	at := int(binary.LittleEndian.Uint32(body[dir:]))
	size := int(binary.LittleEndian.Uint32(body[dir+4:]))

	r.NotZero(at, "the header does not say the file is signed")
	r.Equal(len(body), at+size, "the certificate table does not run to the end of the file")
	r.Zero(at%8, "the certificate table is not on an eight-byte boundary")
	r.Zero(size%8, "the certificate table's length is not a multiple of eight")

	length := int(binary.LittleEndian.Uint32(body[at:]))
	r.LessOrEqual(length, size)
	r.Greater(length, 8, "the entry holds no signature")
	r.EqualValues(0x0200, binary.LittleEndian.Uint16(body[at+4:]), "not revision 2")
	r.EqualValues(0x0002, binary.LittleEndian.Uint16(body[at+6:]), "not a PKCS#7 signature")
}

// The same client signed twice with the same key is the same file. Nothing in
// the signature is a timestamp or a random value, which means an operator who
// rebuilds can compare digests and see that nothing changed.
func TestSigningIsRepeatable(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	body := clientbuildtest.Client(clientbuildtest.BlankRegion())

	first, err := clientbuild.Sign(body, cert, clientbuild.Opus{Program: "Pacenote"})
	r.NoError(err)
	second, err := clientbuild.Sign(body, cert, clientbuild.Opus{Program: "Pacenote"})
	r.NoError(err)
	r.Equal(sha256.Sum256(first), sha256.Sum256(second),
		"signing the same client twice produced two different files")
}

// The clock is not consulted, so a certificate that is valid now and a
// certificate that is valid now both sign. Expiry is the panel's to refuse,
// because "this expired last week" is a sentence for an operator rather than
// an error at the moment somebody presses build.
func TestSigningDoesNotCheckTheClock(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		NotBefore: time.Now().Add(-48 * time.Hour),
		NotAfter:  time.Now().Add(-24 * time.Hour),
		Password:  "x",
	})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)
	_, err = clientbuild.Sign(clientbuildtest.Client(clientbuildtest.BlankRegion()), cert, clientbuild.Opus{})
	r.NoError(err, "signing refused an expired certificate, which is not its decision to make")
}

// Building a client down the signing route, end to end: the builder stamps,
// checks, signs, and writes one file that carries both the address and the
// signature.
func TestABuiltClientIsStampedAndSigned(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{
		CommonName: "Spain GT League", Password: "x",
	})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)

	source := clientbuildtest.Client(clientbuildtest.BlankRegion())
	path := clientbuildtest.WriteAt(t, filepath.Join(t.TempDir(), "pacenote-telemetry.exe"), source)
	b := clientbuild.Builder{
		Source:      clientbuild.Source{Path: path},
		Dir:         t.TempDir(),
		Certificate: func(context.Context) (*clientbuild.Certificate, error) { return &cert, nil },
	}

	art, err := b.Build(t.Context(), clientbuild.Request{
		Address: "https://pacenote.example.com",
		Signing: clientbuild.SigningOwnCertificate,
		Program: "Spain GT",
	})
	r.NoError(err)
	r.Equal("Spain GT League", art.SignedBy, "the build does not record which certificate signed it")
	r.Greater(art.Size, int64(len(source)), "a signed client is no larger than the unsigned one")

	built, err := os.ReadFile(art.Path)
	r.NoError(err)
	r.Len(built, int(art.Size))
	r.Equal(art.SHA256, sumOf(t, art.Path),
		"the digest on the page is not the digest of the file a driver downloads")

	// It is signed, and it still knows where to talk.
	sig := verified(t, built)
	r.Equal("Spain GT League", sig.Certificate.Subject.CommonName)
	r.Equal("Spain GT", sig.OpusInfo.ProgramName.String())
	read, err := clientbuild.Read(built)
	r.NoError(err)
	r.Equal("https://pacenote.example.com", read.Address)
	r.Equal(art.Reference, read.Build)
}

// An unsigned build on a server that holds a certificate is unsigned. The
// operator chose the route and the presence of a certificate does not override
// that choice.
func TestAnUnsignedBuildStaysUnsigned(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{Password: "x"})
	cert, err := clientbuild.ParseCertificate(pfx, "x")
	r.NoError(err)

	source := clientbuildtest.Client(clientbuildtest.BlankRegion())
	b := clientbuild.Builder{
		Source: clientbuild.Source{
			Path: clientbuildtest.WriteAt(t, filepath.Join(t.TempDir(), "c.exe"), source),
		},
		Dir:         t.TempDir(),
		Certificate: func(context.Context) (*clientbuild.Certificate, error) { return &cert, nil },
	}

	art, err := b.Build(t.Context(), clientbuild.Request{
		Address: "https://pacenote.example.com", Signing: clientbuild.SigningNone,
	})
	r.NoError(err)
	r.Empty(art.SignedBy)
	r.EqualValues(len(source), art.Size, "an unsigned build grew, so something signed it")
}

// Asking for a signed client on a server with no certificate is refused, and
// refused before anything is written. A file on the disk that an operator
// believes is signed and is not would be worse than no file.
func TestASignedBuildWithNothingToSignWith(t *testing.T) {
	t.Parallel()

	source := clientbuildtest.Client(clientbuildtest.BlankRegion())
	cases := map[string]func(context.Context) (*clientbuild.Certificate, error){
		"no certificate at all": nil,
		"none stored":           func(context.Context) (*clientbuild.Certificate, error) { return nil, nil },
	}
	for name, certificate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			dir := t.TempDir()
			b := clientbuild.Builder{
				Source: clientbuild.Source{
					Path: clientbuildtest.WriteAt(t, filepath.Join(t.TempDir(), "c.exe"), source),
				},
				Dir:         dir,
				Certificate: certificate,
			}
			_, err := b.Build(t.Context(), clientbuild.Request{
				Address: "https://pacenote.example.com", Signing: clientbuild.SigningOwnCertificate,
			})
			r.ErrorIs(err, clientbuild.ErrNoCertificate)

			entries, readErr := os.ReadDir(dir)
			r.NoError(readErr)
			r.Empty(entries, "a build that could not be signed still left a file behind")
		})
	}
}

// A certificate this server cannot open — the data key changed under it — fails
// the build rather than quietly producing an unsigned one. An operator who
// asked for a signature and got a file has to be told they did not get one.
func TestABuildWhoseCertificateWillNotOpen(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := t.TempDir()
	b := clientbuild.Builder{
		Source: clientbuild.Source{
			Path: clientbuildtest.WriteAt(t, filepath.Join(t.TempDir(), "c.exe"),
				clientbuildtest.Client(clientbuildtest.BlankRegion())),
		},
		Dir: dir,
		Certificate: func(context.Context) (*clientbuild.Certificate, error) {
			return nil, errors.New("the stored certificate does not open with this server's data key")
		},
	}
	_, err := b.Build(t.Context(), clientbuild.Request{
		Address: "https://pacenote.example.com", Signing: clientbuild.SigningOwnCertificate,
	})
	r.Error(err)
	r.Contains(err.Error(), "does not open")

	entries, readErr := os.ReadDir(dir)
	r.NoError(readErr)
	r.Empty(entries)
}
