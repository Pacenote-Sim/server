package clientbuild

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509/pkix"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
)

// The parts of signing that have no reachable way in from outside: the DER
// writer, the header checksum, and the header reader's refusals.
//
// They are tested from inside the package because the alternative is a fixture
// malformed in exactly the right way for each branch, which would be more
// fixture than test. What they have in common is that every one of them is a
// bound check against a file an operator copied onto their server.

func TestTheDERLengthsAreWrittenTheWayDERWritesThem(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Under 128 is one byte.
	r.Equal([]byte{0x30, 0x03, 1, 2, 3}, sequence([]byte{1, 2, 3}))
	r.Equal([]byte{0x31, 0x00}, set(nil))

	// 127 is the last one-byte length, and 128 is the first that is not.
	r.Equal(byte(0x7f), sequence(make([]byte, 127))[1])
	long := sequence(make([]byte, 128))
	r.Equal([]byte{0x30, 0x81, 0x80}, long[:3])
	r.Len(long, 3+128)

	// And two length bytes past 255.
	longer := sequence(make([]byte, 4000))
	r.Equal([]byte{0x30, 0x82, 0x0f, 0xa0}, longer[:4])
	r.Len(longer, 4+4000)

	// Context tags: constructed sets the sixth bit, primitive does not.
	r.Equal([]byte{0xa0, 0x01, 0x07}, tagged(0, true, []byte{7}))
	r.Equal([]byte{0x82, 0x01, 0x07}, tagged(2, false, []byte{7}))
}

func TestTheHeaderChecksum(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := clientbuildtest.PE(clientbuildtest.BlankRegion())
	layout, err := readPE(body)
	r.NoError(err)

	sum := checksum(body, layout.checkSumAt)
	r.NotZero(sum)

	// Whatever is in the field itself does not change the answer, which is
	// what lets a file be checksummed after the field has been written.
	binary.LittleEndian.PutUint32(body[layout.checkSumAt:], sum)
	r.Equal(sum, checksum(body, layout.checkSumAt), "the checksum counted itself")
	binary.LittleEndian.PutUint32(body[layout.checkSumAt:], 0xDEADBEEF)
	r.Equal(sum, checksum(body, layout.checkSumAt))

	// A change anywhere else does change it.
	body[0x500] ^= 0xFF
	r.NotEqual(sum, checksum(body, layout.checkSumAt))

	// A file of odd length still has every byte counted.
	odd := append(clientbuildtest.PE(clientbuildtest.BlankRegion()), 0x01)
	r.NotEqual(checksum(odd[:len(odd)-1], layout.checkSumAt), checksum(odd, layout.checkSumAt))
}

func TestReadPERefusesHeadersThatDoNotHangTogether(t *testing.T) {
	t.Parallel()

	// Offsets into the fixture, spelled out rather than read from the layout
	// under test.
	const (
		peAt         = 0x80
		optAt        = peAt + 24
		sectionCount = peAt + 4 + 2
		optSize      = peAt + 4 + 16
	)
	cases := []struct {
		name    string
		breakIt func(b []byte) []byte
		want    string
	}{
		{
			name: "an optional header longer than the file",
			breakIt: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[optSize:], 0xFFFF)
				return b
			},
			want: "past the end of the file",
		},
		{
			name: "neither PE32 nor PE32+",
			breakIt: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[optAt:], 0x1234)
				return b
			},
			want: "neither PE32 nor PE32+",
		},
		{
			name: "no room recorded for a signature",
			breakIt: func(b []byte) []byte {
				binary.LittleEndian.PutUint32(b[optAt+108:], 2)
				return b
			},
			want: "nowhere to record a signature",
		},
		{
			name: "more sections than there is table",
			breakIt: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[sectionCount:], 400)
				return b
			},
			want: "section table runs past the end",
		},
		{
			name: "a section claiming bytes past the end",
			breakIt: func(b []byte) []byte {
				// The first section's size on disk, made larger than the file.
				binary.LittleEndian.PutUint32(b[peAt+4+20+240+16:], 0x00FFFFFF)
				return b
			},
			want: "past the end of the file",
		},
		{
			name: "a signature that is not where the header says",
			breakIt: func(b []byte) []byte {
				dir := optAt + 112 + 4*8
				binary.LittleEndian.PutUint32(b[dir:], 0x10)
				binary.LittleEndian.PutUint32(b[dir+4:], 0x20)
				return b
			},
			want: "the signature is not where it says",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := readPE(tc.breakIt(clientbuildtest.PE(clientbuildtest.BlankRegion())))
			require.ErrorIs(t, err, ErrNotPE)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A PE32 client — a 32-bit build — keeps its data directories somewhere else,
// and the reader has to find them there. Nothing this project ships is 32-bit,
// which is exactly why the branch would otherwise never be exercised.
func TestReadPEFindsThePE32Directories(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := clientbuildtest.PE(clientbuildtest.BlankRegion())
	optAt := 0x80 + 24
	binary.LittleEndian.PutUint16(body[optAt:], pe32Magic)
	// The PE32 optional header keeps the directory count sixteen bytes earlier
	// than PE32+ does, because it has no 64-bit fields.
	binary.LittleEndian.PutUint32(body[optAt+offNumberOfRvaAndSizes32:], 16)

	layout, err := readPE(body)
	r.NoError(err)
	r.Equal(optAt+offNumberOfRvaAndSizes32+4+certificateDirIndex*8, layout.certDirAt)
}

// Signing a file that already carries a signature drops the old one rather than
// hashing it. It is the case an operator meets by pointing this server at a
// client somebody else signed: stamping broke that signature the moment the
// address went in, so what is left is bytes nobody can use.
func TestPrepareDropsASignatureThatIsAlreadyThere(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	plain := clientbuildtest.PE(clientbuildtest.BlankRegion())
	layout, err := readPE(plain)
	r.NoError(err)
	once, err := attach(prepare(plain, layout), layout, []byte("pretend this is a signature"))
	r.NoError(err)
	r.Greater(len(once), len(plain))

	// Read back, the file says it is signed; prepared again, it is the program
	// it started as with the entry cleared.
	signedLayout, err := readPE(once)
	r.NoError(err)
	r.NotZero(signedLayout.certBytes)

	again := prepare(once, signedLayout)
	r.Len(again, len(plain), "the old signature was hashed rather than dropped")
	r.Equal(make([]byte, 8), again[layout.certDirAt:layout.certDirAt+8],
		"the directory still points at a signature that is no longer there")

	// Everything but the checksum, which attach wrote when it signed and will
	// write again. It is not part of the hash, so a stale one changes nothing —
	// but the assertion says which field it is rather than allowing any.
	blank := func(b []byte) []byte {
		out := bytes.Clone(b)
		clear(out[layout.checkSumAt : layout.checkSumAt+4])
		clear(out[layout.certDirAt : layout.certDirAt+8])
		return out
	}
	r.Equal(blank(plain), blank(again), "preparing a signed file did not put the program back")
}

func TestAttachRefusesWhatItCannotAttachTo(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	body := clientbuildtest.PE(clientbuildtest.BlankRegion())
	layout, err := readPE(body)
	r.NoError(err)

	_, err = attach(prepare(body, layout), layout, nil)
	r.ErrorContains(err, "no signature to attach")

	_, err = attach(body[:len(body)-1], layout, []byte("signature"))
	r.ErrorContains(err, "not prepared for a signature")
}

// How a certificate is described when it carries less than the usual.
func TestDescribingTheUnusual(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A certificate with no common name renders as its whole distinguished
	// name rather than as an empty cell on the page.
	r.Equal("Spain GT League", name("CN=Spain GT League", "Spain GT League"))
	r.Equal("O=Spain GT,C=ES", name("O=Spain GT,C=ES", "  "))

	// A key of a kind nobody uses for this still renders as something.
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	r.NoError(err)
	r.Equal("ECDSA P-384", keyWords(&key.PublicKey))
	r.Equal("*pkix.Name", keyWords(&pkix.Name{}))
}
