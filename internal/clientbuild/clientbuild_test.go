package clientbuild_test

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
)

// The fixtures come from clientbuildtest, which spells the region's layout out
// from the format rather than importing it from the package under test.
const (
	regionSize  = clientbuildtest.RegionSize
	markerSize  = clientbuildtest.MarkerSize
	offVersion  = clientbuildtest.OffVersion
	offReserved = clientbuildtest.OffReserved
	offLength   = clientbuildtest.OffLength
	offChecksum = clientbuildtest.OffChecksum
	offPayload  = clientbuildtest.OffPayload
	offTail     = clientbuildtest.OffTail
)

var (
	head = clientbuildtest.Head()
	tail = clientbuildtest.Tail()
)

func blankRegion() []byte { return clientbuildtest.BlankRegion() }

func stampedRegion(payload string) []byte { return clientbuildtest.StampedRegion(payload) }

func peFile(body []byte) []byte { return clientbuildtest.PE(body) }

func client(region []byte) []byte { return clientbuildtest.Client(region) }

// writeClient puts a fixture client on the disk and returns its path.
func writeClient(tb testing.TB, body []byte) string {
	tb.Helper()
	return clientbuildtest.WriteAt(tb, filepath.Join(tb.TempDir(), "pacenote-telemetry.exe"), body)
}

func TestFind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		file    func() []byte
		wantErr error
	}{
		{
			name: "one region, found by scanning rather than by an offset",
			file: func() []byte { return client(blankRegion()) },
		},
		{
			name: "a region that has already been stamped",
			file: func() []byte {
				return client(stampedRegion("url=https://x.example"))
			},
		},
		{
			name: "a head marker with nothing after it is not a region",
			file: func() []byte {
				var body bytes.Buffer
				body.Write(head)
				body.Write(bytes.Repeat([]byte{0x11}, 64))
				body.Write(blankRegion())
				return peFile(body.Bytes())
			},
		},
		{
			name: "a head marker at the very end of the file is not a region",
			file: func() []byte {
				var body bytes.Buffer
				body.Write(blankRegion())
				body.Write(head)
				return peFile(body.Bytes())
			},
		},
		{
			name:    "no region at all",
			file:    func() []byte { return peFile(bytes.Repeat([]byte{0xAB}, 4096)) },
			wantErr: clientbuild.ErrNoRegion,
		},
		{
			name: "two regions is a file this server will not guess between",
			file: func() []byte {
				var body bytes.Buffer
				body.Write(blankRegion())
				body.Write(bytes.Repeat([]byte{0x22}, 100))
				body.Write(blankRegion())
				return peFile(body.Bytes())
			},
			wantErr: clientbuild.ErrManyRegions,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			file := tc.file()
			at, err := clientbuild.Find(file)
			if tc.wantErr != nil {
				r.ErrorIs(err, tc.wantErr)
				return
			}
			r.NoError(err)
			r.Equal(head, file[at:at+markerSize], "the head marker is at the offset it reported")
			r.Equal(tail, file[at+offTail:at+regionSize], "and the tail marker closes it")
		})
	}
}

func TestStampRoundTrip(t *testing.T) {
	t.Parallel()

	long := "https://" + strings.Repeat("a", 200) + ".example.com"

	cases := []struct {
		name    string
		address string
	}{
		{"a host name", "https://pacenote.example.com"},
		{"a host and a port", "http://192.168.1.10:8080"},
		{"a long one", long},
		{"one that fills the region", "https://" + strings.Repeat("b", clientbuild.PayloadMax-len("url=https://")-len("\nbuild=0123456789abcdef"))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			original := client(blankRegion())
			stamped := bytes.Clone(original)
			r.NoError(clientbuild.Write(stamped, clientbuild.Stamp{Address: tc.address, Build: "0123456789abcdef"}))

			got, err := clientbuild.Read(stamped)
			r.NoError(err)
			r.Equal(tc.address, got.Address)
			r.Equal("0123456789abcdef", got.Build)

			// The size and the layout are untouched, and so is every byte
			// outside the region: that is what makes the stamped copy the same
			// program as the one it was copied from.
			r.Len(stamped, len(original), "stamping changed the file's size")
			at, err := clientbuild.Find(original)
			r.NoError(err)
			r.Equal(original[:at], stamped[:at], "a byte before the region changed")
			r.Equal(original[at+regionSize:], stamped[at+regionSize:], "a byte after the region changed")

			// And it is still a Windows executable.
			_, err = pe.NewFile(bytes.NewReader(stamped))
			r.NoError(err, "the stamped copy is no longer a valid PE executable")

			// Stamping twice is stamping once: the second address replaces the
			// first rather than being appended to it.
			r.NoError(clientbuild.Write(stamped, clientbuild.Stamp{Address: "https://second.example"}))
			again, err := clientbuild.Read(stamped)
			r.NoError(err)
			r.Equal("https://second.example", again.Address)
			r.Empty(again.Build)
		})
	}
}

func TestReadRefusesAStampItCannotTrust(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		region  func() []byte
		wantErr error
	}{
		{
			name:    "one that was never stamped",
			region:  blankRegion,
			wantErr: clientbuild.ErrBlank,
		},
		{
			name: "a payload one byte different from its checksum",
			region: func() []byte {
				b := stampedRegion("url=https://x.example")
				b[offPayload+9]++
				return b
			},
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name: "a length that runs past the end of the region",
			region: func() []byte {
				b := stampedRegion("url=https://x.example")
				binary.BigEndian.PutUint16(b[offLength:], clientbuild.PayloadMax+1)
				return b
			},
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name: "a length short of the payload, so the checksum disagrees",
			region: func() []byte {
				b := stampedRegion("url=https://x.example")
				binary.BigEndian.PutUint16(b[offLength:], 4)
				return b
			},
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name: "a version written by a newer server",
			region: func() []byte {
				b := stampedRegion("url=https://x.example")
				b[offVersion] = clientbuild.Version1 + 1
				return b
			},
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name: "the reserved byte carrying something",
			region: func() []byte {
				b := stampedRegion("url=https://x.example")
				b[offReserved] = 7
				return b
			},
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name:    "a payload with no address in it",
			region:  func() []byte { return stampedRegion("build=abc") },
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name:    "a payload that is not text",
			region:  func() []byte { return stampedRegion("url=\xff\xfe") },
			wantErr: clientbuild.ErrCorrupt,
		},
		{
			name: "a region whose tail marker was overwritten is not a region at all",
			region: func() []byte {
				b := stampedRegion("url=https://x.example")
				b[regionSize-1] = 'X'
				return b
			},
			wantErr: clientbuild.ErrNoRegion,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			got, err := clientbuild.Read(client(tc.region()))
			r.ErrorIs(err, tc.wantErr)
			r.Equal(clientbuild.Stamp{}, got)
		})
	}
}

func TestWriteRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		file  func() []byte
		stamp clientbuild.Stamp
		want  string
	}{
		{
			name:  "a stamp with no address",
			file:  func() []byte { return client(blankRegion()) },
			stamp: clientbuild.Stamp{Build: "abc"},
			want:  "no server address",
		},
		{
			name:  "a payload with no room in the region",
			file:  func() []byte { return client(blankRegion()) },
			stamp: clientbuild.Stamp{Address: "https://" + strings.Repeat("c", clientbuild.PayloadMax)},
			want:  "does not fit",
		},
		{
			name:  "a binary with nowhere to write",
			file:  func() []byte { return peFile(bytes.Repeat([]byte{0xAB}, 4096)) },
			stamp: clientbuild.Stamp{Address: "https://x.example"},
			want:  "no reserved region",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			file := tc.file()
			before := bytes.Clone(file)
			err := clientbuild.Write(file, tc.stamp)
			r.Error(err)
			r.Contains(err.Error(), tc.want)
			r.Equal(before, file, "a refused stamp still changed the file")
		})
	}
}

func TestInspect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		path        func(tb testing.TB) string
		wantPresent bool
		wantReady   bool
		wantProblem string
	}{
		{
			name: "a client this server can stamp",
			path: func(tb testing.TB) string {
				tb.Helper()
				return writeClient(tb, client(blankRegion()))
			},
			wantReady: true, wantPresent: true,
		},
		{
			name: "a client that already carries an address",
			path: func(tb testing.TB) string {
				tb.Helper()
				return writeClient(tb, client(stampedRegion("url=https://old.example")))
			},
			wantReady: true, wantPresent: true,
		},
		{
			name: "nothing there yet",
			path: func(tb testing.TB) string {
				tb.Helper()
				return filepath.Join(tb.TempDir(), "pacenote-telemetry.exe")
			},
			wantPresent: false,
		},
		{
			name:        "this server was never told where the client is",
			path:        func(testing.TB) string { return "" },
			wantProblem: "has not been told",
		},
		{
			name: "a directory where the client should be",
			path: func(tb testing.TB) string {
				tb.Helper()
				return tb.TempDir()
			},
			wantPresent: true,
			wantProblem: "is a directory",
		},
		{
			name: "something that is not a Windows executable",
			path: func(tb testing.TB) string {
				tb.Helper()
				return writeClient(tb, []byte("this is a text file, not a client"))
			},
			wantPresent: true,
			wantProblem: "not a Windows executable",
		},
		{
			name: "a client with no reserved region",
			path: func(tb testing.TB) string {
				tb.Helper()
				return writeClient(tb, peFile(bytes.Repeat([]byte{0xAB}, 4096)))
			},
			wantPresent: true,
			wantProblem: "no reserved region",
		},
		{
			name: "a client with two",
			path: func(tb testing.TB) string {
				tb.Helper()
				var body bytes.Buffer
				body.Write(blankRegion())
				body.Write(blankRegion())
				return writeClient(tb, peFile(body.Bytes()))
			},
			wantPresent: true,
			wantProblem: "more than one reserved region",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			got := clientbuild.Source{Path: tc.path(t)}.Inspect()
			r.Equal(tc.wantPresent, got.Present)
			r.Equal(tc.wantReady, got.Ready())
			if tc.wantProblem == "" {
				r.Empty(got.Problem)
			} else {
				r.Contains(got.Problem, tc.wantProblem)
			}
			if tc.wantReady {
				r.Equal("x86-64", got.Machine)
				r.NotEmpty(got.SHA256)
				r.Positive(got.Size)
			}
		})
	}
}

func TestInspectReadsWhatIsAlreadyStamped(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	path := writeClient(t, client(stampedRegion("url=https://old.example\nbuild=aa")))
	got := clientbuild.Source{Path: path}.Inspect()
	r.True(got.Ready())
	r.Equal("https://old.example", got.StampedWith)
}

func TestBuild(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		address string
	}{
		{"a public host", "https://pacenote.example.com"},
		{"a host on the league's own network", "http://192.168.1.10:8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			source := client(blankRegion())
			path := writeClient(t, source)
			dir := t.TempDir()
			b := clientbuild.Builder{
				Source: clientbuild.Source{Path: path},
				Dir:    dir,
				Now:    func() time.Time { return at },
			}
			r.True(b.Ready())

			art, err := b.Build(t.Context(), clientbuild.Request{Address: tc.address, Signing: clientbuild.SigningNone})
			r.NoError(err)
			r.Equal(tc.address, art.Address, "the address is read back out of the file that was written")
			r.Equal("pacenote-telemetry.exe", art.Name)
			r.Equal(at, art.At)
			r.Equal(int64(len(source)), art.Size, "a built client is the size of the one it was copied from")
			r.True(clientbuild.ValidReference(art.Reference))

			built, err := os.ReadFile(art.Path)
			r.NoError(err)
			r.Len(built, len(source))

			read, err := clientbuild.Read(built)
			r.NoError(err)
			r.Equal(tc.address, read.Address)
			r.Equal(art.Reference, read.Build, "the file says which build it came from")

			_, err = pe.NewFile(bytes.NewReader(built))
			r.NoError(err, "the built client is not a valid PE executable")

			// The prebuilt client is not touched by a build.
			after, err := os.ReadFile(path)
			r.NoError(err)
			r.Equal(source, after)

			// The download route finds it, and the digest on the page is the
			// digest of what a driver downloads.
			f, err := b.Open(art.Reference)
			r.NoError(err)
			t.Cleanup(func() { _ = f.Close() })
			r.Equal(art.SHA256, sumOf(t, art.Path))
		})
	}
}

func TestBuildRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		builder func(tb testing.TB) clientbuild.Builder
		req     clientbuild.Request
		want    string
	}{
		{
			name: "a signing route this build does not have",
			builder: func(tb testing.TB) clientbuild.Builder {
				tb.Helper()
				return clientbuild.Builder{
					Source: clientbuild.Source{Path: writeClient(tb, client(blankRegion()))},
					Dir:    tb.TempDir(),
				}
			},
			req:  clientbuild.Request{Address: "https://x.example", Signing: clientbuild.SigningOwnCertificate},
			want: "no certificate to sign with",
		},
		{
			name: "no address",
			builder: func(tb testing.TB) clientbuild.Builder {
				tb.Helper()
				return clientbuild.Builder{
					Source: clientbuild.Source{Path: writeClient(tb, client(blankRegion()))},
					Dir:    tb.TempDir(),
				}
			},
			req:  clientbuild.Request{Signing: clientbuild.SigningNone},
			want: "needs a server address",
		},
		{
			name: "no prebuilt client to copy",
			builder: func(tb testing.TB) clientbuild.Builder {
				tb.Helper()
				return clientbuild.Builder{
					Source: clientbuild.Source{Path: filepath.Join(tb.TempDir(), "pacenote-telemetry.exe")},
					Dir:    tb.TempDir(),
				}
			},
			req:  clientbuild.Request{Address: "https://x.example", Signing: clientbuild.SigningNone},
			want: "no prebuilt client",
		},
		{
			name: "a prebuilt client with nowhere to write an address",
			builder: func(tb testing.TB) clientbuild.Builder {
				tb.Helper()
				return clientbuild.Builder{
					Source: clientbuild.Source{Path: writeClient(tb, peFile(bytes.Repeat([]byte{0xAB}, 4096)))},
					Dir:    tb.TempDir(),
				}
			},
			req:  clientbuild.Request{Address: "https://x.example", Signing: clientbuild.SigningNone},
			want: "no reserved region",
		},
		{
			name: "nowhere to keep what was built",
			builder: func(tb testing.TB) clientbuild.Builder {
				tb.Helper()
				return clientbuild.Builder{
					Source: clientbuild.Source{Path: writeClient(tb, client(blankRegion()))},
				}
			},
			req:  clientbuild.Request{Address: "https://x.example", Signing: clientbuild.SigningNone},
			want: "nowhere to keep",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			b := tc.builder(t)
			art, err := b.Build(t.Context(), tc.req)
			r.Error(err)
			r.Contains(err.Error(), tc.want)
			r.Empty(art.Reference)
			if b.Dir != "" {
				entries, readErr := os.ReadDir(b.Dir)
				r.NoError(readErr)
				r.Empty(entries, "a refused build still left a file behind")
			}
		})
	}
}

func TestPathForTakesOnlyAReferenceThisServerMinted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		reference string
		ok        bool
	}{
		{"one that was minted here", "0123456789abcdef", true},
		{"upper case, which nothing here mints", "0123456789ABCDEF", false},
		{"too short", "0123", false},
		{"a path", "../../etc/passwd", false},
		{"a path dressed as a reference", "0123456789abcde/", false},
		{"empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			b := clientbuild.Builder{Source: clientbuild.Source{Path: "/tmp/pacenote-telemetry.exe"}, Dir: t.TempDir()}
			path, err := b.PathFor(tc.reference)
			r.Equal(tc.ok, clientbuild.ValidReference(tc.reference))
			if !tc.ok {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(filepath.Join(b.Dir, tc.reference+".exe"), path)
		})
	}
}

func TestSigningRoutes(t *testing.T) {
	t.Parallel()

	// The routes, each asked twice: on a server holding a certificate and on
	// one that is not. The second column is the whole point of the type — the
	// same route is offered or explained depending on what the operator has
	// given this server, and the answer changes without a restart.
	cases := []struct {
		name                  string
		signing               clientbuild.Signing
		valid                 bool
		withCert, withoutCert bool
		wantUnavailable       string
	}{
		{
			name: "unsigned always works", signing: clientbuild.SigningNone, valid: true,
			withCert: true, withoutCert: true,
		},
		{
			// Valid always, available only once a certificate is stored. The
			// route that was a paid service we had not built is gone entirely
			// rather than sitting here disabled: a choice an operator can read
			// and never use wastes their attention on every visit.
			name: "your own certificate", signing: clientbuild.SigningOwnCertificate, valid: true,
			withCert: true, wantUnavailable: "no certificate to sign with",
		},
		{
			name: "something else entirely", signing: clientbuild.Signing("magic"),
			wantUnavailable: "not one of the ways",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			r.Equal(tc.valid, tc.signing.Valid())
			r.Equal(tc.withCert, tc.signing.Available(true), "with a certificate stored")
			r.Equal(tc.withoutCert, tc.signing.Available(false), "with no certificate stored")
			r.NotEmpty(tc.signing.Label())

			// A route that can be taken says nothing; one that cannot says why
			// in words the operator can act on.
			if tc.withCert {
				r.Empty(tc.signing.Unavailable(true))
			}
			if tc.withoutCert {
				r.Empty(tc.signing.Unavailable(false))
				return
			}
			r.Contains(tc.signing.Unavailable(false), tc.wantUnavailable)
		})
	}
}

// sumOf is the digest of a file on disk, so that what the page prints can be
// checked against what a driver would download.
func sumOf(tb testing.TB, path string) string {
	tb.Helper()
	body, err := os.ReadFile(path)
	require.NoError(tb, err)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// A prebuilt client the server cannot read, and one far too large to be one.
//
// Both are the operator's own file system rather than anything this code did,
// which is why each is a sentence on the page and not an error: the fix is to
// put a different file there, and a page that said "internal server error"
// would not have told them that.
func TestInspectWhatCannotBeRead(t *testing.T) {
	t.Parallel()

	t.Run("a file this server may not open", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		if os.Geteuid() == 0 {
			t.Skip("running as root, which can read a file it may not")
		}

		path := writeClient(t, client(blankRegion()))
		r.NoError(os.Chmod(path, 0o000))
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

		got := clientbuild.Source{Path: path}.Inspect()
		r.True(got.Present)
		r.False(got.Ready())
		r.Contains(got.Problem, "could not be read")
	})

	t.Run("a directory where the client should be", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		got := clientbuild.Source{Path: t.TempDir()}.Inspect()
		r.True(got.Present)
		r.False(got.Ready())
		r.Contains(got.Problem, "a directory, not the client executable")
	})

	t.Run("something far larger than a client", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// A sparse file: it claims to be larger than this server will read and
		// costs nothing on disk, which is the only way to test a limit set in
		// hundreds of megabytes.
		path := filepath.Join(t.TempDir(), "pacenote-telemetry.exe")
		f, err := os.Create(path)
		r.NoError(err)
		r.NoError(f.Truncate(clientbuild.MaxClientBytes + 1))
		r.NoError(f.Close())

		got := clientbuild.Source{Path: path}.Inspect()
		r.True(got.Present)
		r.False(got.Ready())
		r.EqualValues(clientbuild.MaxClientBytes+1, got.Size)
		r.Contains(got.Problem, "check the path")
		r.Empty(got.SHA256, "a file too large to read was digested anyway")
	})

	t.Run("a client carrying two reserved regions", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// Two regions is a file this code will not guess between: it cannot
		// tell which one the client reads, and writing the wrong one would
		// produce a client that silently talks to nobody.
		var body bytes.Buffer
		body.Write(blankRegion())
		body.Write(bytes.Repeat([]byte{0xAB}, 64))
		body.Write(blankRegion())

		got := clientbuild.Source{Path: writeClient(t, clientbuildtest.PE(body.Bytes()))}.Inspect()
		r.True(got.Present)
		r.False(got.Ready())
		r.Contains(got.Problem, "more than one reserved region")
	})
}
