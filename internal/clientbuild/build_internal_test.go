package clientbuild

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
)

// The check that stands between a stamped copy and a driver, and the file
// writing under it.
//
// [verify] is the reason a built client can be trusted at all: it re-reads what
// was written rather than believing the code that wrote it. That makes it the
// one function here whose failures must be exercised deliberately, because
// nothing that happens in the ordinary course of a build will reach them — by
// construction, a build where any of them fires is a build that never happened.

func TestVerifyRefusesACopyThatIsNotTheOriginal(t *testing.T) {
	t.Parallel()

	const address = "https://pacenote.example.com"
	original := clientbuildtest.Client(clientbuildtest.BlankRegion())
	good := func() []byte {
		out := bytes.Clone(original)
		require.NoError(t, Write(out, Stamp{Address: address, Build: "0011223344556677"}))
		return out
	}
	require.NoError(t, verify(original, good(), address), "a good stamp was refused")

	at, err := Find(original)
	require.NoError(t, err)

	cases := []struct {
		name    string
		stamped func() []byte
		want    string
	}{
		{
			name:    "a copy that changed size",
			stamped: func() []byte { return append(good(), 0x00) },
			want:    "changed the file's size",
		},
		{
			name: "a copy with no region left in it",
			stamped: func() []byte {
				out := good()
				copy(out[at:], make([]byte, MarkerSize))
				return out
			},
			want: "no reserved region",
		},
		{
			// The check that matters most: a byte changed outside the 1024
			// this server is allowed to touch. Anything here means the stamping
			// code wrote somewhere it should not have, and the copy is not the
			// program the operator thinks it is.
			name: "a copy changed outside the region",
			stamped: func() []byte {
				out := good()
				out[at-1] ^= 0xFF
				return out
			},
			want: "outside the reserved region",
		},
		{
			name: "a copy whose region no longer reads back",
			stamped: func() []byte {
				out := good()
				// The payload's checksum, left pointing at a payload that has
				// changed since it was written.
				out[at+clientbuildtest.OffPayload] ^= 0xFF
				return out
			},
			want: "does not read back",
		},
		{
			name: "a copy pointing somewhere else",
			stamped: func() []byte {
				out := bytes.Clone(original)
				require.NoError(t, Write(out, Stamp{
					Address: "https://somebody-elses-league.example.com", Build: "0011223344556677",
				}))
				return out
			},
			want: "points at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := verify(original, tc.stamped(), address)
			require.Error(t, err, "a copy that is wrong was passed as good")
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// The copy is checked for still being a Windows executable, and reaching that
// check means starting from something that is not one — a copy identical to its
// original outside the region cannot have stopped being a PE unless the
// original never was.
//
// It is a check against this package's own future rather than against any file:
// [Source.load] refuses a prebuilt client that is not a PE long before a build
// starts. It stays because stamping writes into a binary, and the day that
// writing learns to move a byte is the day this is the only thing that notices.
func TestVerifyRefusesACopyThatIsNotAWindowsExecutable(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	const address = "https://pacenote.example.com"
	original := append(bytes.Repeat([]byte{0xAB}, 64), clientbuildtest.BlankRegion()...)
	stamped := bytes.Clone(original)
	r.NoError(Write(stamped, Stamp{Address: address, Build: "0011223344556677"}))

	err := verify(original, stamped, address)
	r.Error(err)
	r.Contains(err.Error(), "not a valid Windows executable")
}

// Writing a built client where it cannot be written. The failure has to be an
// error and not a half-written file, because the next thing that happens to a
// built client is that a league installs it.
func TestWritingABuiltClientWhereItCannotGo(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A builds directory that is a file. Creating it fails, and nothing is
	// written anywhere.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	r.NoError(os.WriteFile(file, []byte("this is a file"), 0o600))
	b := Builder{Dir: filepath.Join(file, "builds")}
	err := b.write(filepath.Join(file, "builds", "x.exe"), []byte("client"))
	r.Error(err)
	r.Contains(err.Error(), "could not be created")

	// A directory that exists and cannot be written into.
	locked := filepath.Join(t.TempDir(), "locked")
	r.NoError(os.MkdirAll(locked, 0o700))
	r.NoError(os.Chmod(locked, 0o500))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write into a directory it may not")
	}
	err = Builder{Dir: locked}.write(filepath.Join(locked, "x.exe"), []byte("client"))
	r.Error(err)
	r.Contains(err.Error(), "temporary file could not be created")
}

// Where a build is kept, and the refusal that keeps the address bar out of the
// file system.
func TestWhereABuildIsKept(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A server with nowhere to keep one has no path to give.
	_, err := Builder{}.PathFor("0011223344556677")
	r.ErrorIs(err, ErrNotReady)
	r.False(Builder{}.Exists("0011223344556677"))
	_, err = Builder{}.Open("0011223344556677")
	r.ErrorIs(err, ErrNotReady)

	b := Builder{Dir: t.TempDir(), Source: Source{Path: "/somewhere/pacenote-telemetry.exe"}}
	path, err := b.PathFor("0011223344556677")
	r.NoError(err)
	r.Equal(filepath.Join(b.Dir, "0011223344556677.exe"), path)

	// Anything that is not a reference this package minted is refused before it
	// reaches the file system, so no address can be turned into a path
	// traversal.
	for _, bad := range []string{"", "../../etc/passwd", "0011223344556677aa", "ZZ11223344556677"} {
		_, err := b.PathFor(bad)
		r.Error(err, "%q was accepted as a build reference", bad)
		r.False(b.Exists(bad))
		_, err = b.Open(bad)
		r.Error(err)
	}

	// A reference that is one and has no file behind it: the history outlives
	// the files, so this is an ordinary state and not a failure.
	r.False(b.Exists("aabbccddeeff0011"))
	_, err = b.Open("aabbccddeeff0011")
	r.Error(err)
	r.Contains(err.Error(), "could not be opened")
}

// A signing route that is not one is refused before a certificate is looked
// for, because there is no route to look one up for.
func TestABuildDownARouteThatIsNotOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	_, err := Builder{}.certificate(t.Context(), Signing("hand-delivered"))
	r.Error(err)
	r.Contains(err.Error(), "not one of the ways")
}

// The two shapes of optional header that stop before the thing being looked for
// in them. Both are a truncated or hand-edited file rather than anything a
// linker produces, and both would be read past the end of if they were trusted.
func TestReadPERefusesAnOptionalHeaderThatStopsTooSoon(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	const optSizeAt = 0x80 + 4 + 16

	// Long enough to be an optional header and too short to reach the count of
	// data directories.
	body := clientbuildtest.PE(clientbuildtest.BlankRegion())
	writeUint16(body[optSizeAt:], offNumberOfRvaAndSizes32+4)
	_, err := readPE(body)
	r.ErrorIs(err, ErrNotPE)
	r.Contains(err.Error(), "stops before the data directories")

	// Long enough to reach the count and too short to hold the entry it
	// promises.
	body = clientbuildtest.PE(clientbuildtest.BlankRegion())
	writeUint16(body[optSizeAt:], offNumberOfRvaAndSizes64+4+8)
	_, err = readPE(body)
	r.ErrorIs(err, ErrNotPE)
	r.Contains(err.Error(), "falls outside its optional header")
}

func writeUint16(b []byte, v int) {
	b[0], b[1] = byte(v), byte(v>>8)
}
