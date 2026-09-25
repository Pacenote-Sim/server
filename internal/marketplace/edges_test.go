package marketplace_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/marketplace"
)

// rawPackage registers arbitrary bytes as a package and returns its artifact.
func (s *site) rawPackage(file string, body []byte) marketplace.Artifact {
	unlock := s.lock()
	s.packages[file] = body
	unlock()
	sum := sha256.Sum256(body)
	return marketplace.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: s.https("/dl/" + file), SHA256: hex.EncodeToString(sum[:])}
}

func zipOf(t *testing.T, build func(*zip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	build(zw)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// smallCap is the download cap the tests run with; the real one is a quarter
// of a gigabyte, which nobody wants a test to serve.
const smallCap = 64 << 10

func TestInstall_PackageShapes(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	bin := binaryName("voice")

	empty := s.rawPackage("empty.zip", zipOf(t, func(*zip.Writer) {}))
	symlink := s.rawPackage("symlink.zip", zipOf(t, func(zw *zip.Writer) {
		h := &zip.FileHeader{Name: "voice/plugin.json"}
		h.SetMode(0o644)
		w, _ := zw.CreateHeader(h)
		_, _ = w.Write([]byte(`{"name":"voice"}`))
		l := &zip.FileHeader{Name: "voice/" + bin}
		l.SetMode(os.ModeSymlink | 0o777)
		w, _ = zw.CreateHeader(l)
		_, _ = w.Write([]byte("/bin/sh"))
	}))
	manyDirs := s.rawPackage("dirs.zip", zipOf(t, func(zw *zip.Writer) {
		d := &zip.FileHeader{Name: "voice/"}
		d.SetMode(os.ModeDir | 0o755)
		_, _ = zw.CreateHeader(d)
		d = &zip.FileHeader{Name: "voice/migrations/"}
		d.SetMode(os.ModeDir | 0o755)
		_, _ = zw.CreateHeader(d)
		h := &zip.FileHeader{Name: "voice/plugin.json"}
		h.SetMode(0o644)
		w, _ := zw.CreateHeader(h)
		_, _ = w.Write([]byte(`{"name":"voice","binary":"` + bin + `"}`))
		h = &zip.FileHeader{Name: "voice/" + bin}
		h.SetMode(0o755)
		w, _ = zw.CreateHeader(h)
		_, _ = w.Write([]byte("x"))
	}))
	oversize := s.rawPackage("oversize.zip", bytes.Repeat([]byte("x"), smallCap+1))

	cases := []struct {
		name string
		art  marketplace.Artifact
		want string
	}{
		{"empty", empty, "holds 0 entries"},
		{"symlink", symlink, "not a regular file"},
		{"oversize download", oversize, "larger than"},
		{"directories are fine", manyDirs, ""},
	}
	for _, tc := range cases { //nolint:paralleltest // each case publishes its own index on the shared site
		t.Run(tc.name, func(t *testing.T) {
			d := newDirs(t)
			s.publish(s.plugin("voice", "v0.1.0", tc.art))
			c, err := marketplace.New(marketplace.Options{
				URL: s.server.URL + "/index.json", PublicKey: s.pub, Dir: d.cache, PluginsDir: d.plugins,
				HTTP: &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 30 * time.Second}, MaxPackage: smallCap,
			})
			require.NoError(t, err)
			require.NoError(t, c.Refresh(context.Background()))
			_, err = c.Install(context.Background(), "voice")
			if tc.want == "" {
				require.NoError(t, err)
				require.DirExists(t, filepath.Join(d.plugins, "voice", "migrations"))
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestInstall_UnwritablePlaces(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	s.publish(s.plugin("voice", "v0.1.0", art))

	// The plugins directory is a file: nothing can be staged under it.
	root := t.TempDir()
	blocked := filepath.Join(root, "plugins")
	r.NoError(os.WriteFile(blocked, []byte("x"), 0o600))
	c, err := marketplace.New(marketplace.Options{
		URL: s.server.URL + "/index.json", PublicKey: s.pub, Dir: filepath.Join(root, "cache"), PluginsDir: blocked,
		HTTP: &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 30 * time.Second},
	})
	r.NoError(err)
	r.NoError(c.Refresh(context.Background()))
	_, err = c.Install(context.Background(), "voice")
	r.Error(err)

	// The cache directory is a file: the index cannot be stored, and the
	// download has nowhere to go.
	root = t.TempDir()
	cache := filepath.Join(root, "cache")
	r.NoError(os.WriteFile(cache, []byte("x"), 0o600))
	c, err = marketplace.New(marketplace.Options{
		URL: s.server.URL + "/index.json", PublicKey: s.pub, Dir: cache, PluginsDir: filepath.Join(root, "plugins"),
		HTTP: &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 30 * time.Second},
	})
	r.NoError(err)
	r.ErrorContains(c.Refresh(context.Background()), "cannot create")
	r.Nil(c.Snapshot().Index, "an index that could not be stored is not kept either")

	// The existing plugin folder cannot be moved aside.
	root = t.TempDir()
	plugins := filepath.Join(root, "plugins")
	r.NoError(os.MkdirAll(filepath.Join(plugins, "voice"), 0o700))
	c, err = marketplace.New(marketplace.Options{
		URL: s.server.URL + "/index.json", PublicKey: s.pub, Dir: filepath.Join(root, "cache"), PluginsDir: plugins,
		HTTP: &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 30 * time.Second},
	})
	r.NoError(err)
	r.NoError(c.Refresh(context.Background()))
	r.NoError(os.Chmod(plugins, 0o500))
	t.Cleanup(func() { _ = os.Chmod(plugins, 0o700) })
	_, err = c.Install(context.Background(), "voice")
	r.Error(err)
	r.NoError(os.Chmod(plugins, 0o700))
	r.DirExists(filepath.Join(plugins, "voice"), "the old folder is still there")
}

func TestAvailable_Filters(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	other := art
	other.OS = "plan9"

	companion := s.plugin("client-voice", "v0.1.0", art)
	companion.Kind = "companion"
	withdrawn := s.plugin("engineer", "v0.1.0", art)
	withdrawn.Versions[0].Status = marketplace.StatusWithdrawn
	elsewhere := s.plugin("visual-telemetry", "v0.1.0", other)
	fine := s.plugin("voice", "v0.1.0", art)
	s.publish(companion, withdrawn, elsewhere, fine)

	c := client(t, s, d, true)
	r.NoError(c.Refresh(context.Background()))
	avail := c.Available()
	r.Len(avail, 1)
	r.Equal("voice", avail[0].Name)

	// Withdrawn asks about a plugin that has no such version.
	_, gone := c.Withdrawn("voice", "9.9.9")
	r.False(gone)
	_, gone = c.Withdrawn("engineer", "v0.1.0")
	r.True(gone)

	// A version with an artifact for another machine only.
	v := elsewhere.Versions[0]
	_, ok := v.ArtifactFor(runtime.GOOS, runtime.GOARCH)
	r.False(ok)
	r.Nil(withdrawn.Latest())
	_, ok = (&marketplace.Index{}).Find("nobody")
	r.False(ok)
	var none *marketplace.Index
	_, ok = none.Find("voice")
	r.False(ok)
}

func TestSignatureWithoutPadding(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	s.publish(s.plugin("voice", "v0.1.0", art))
	unlock := s.lock()
	s.sig = []byte(strings.TrimRight(strings.TrimSpace(string(s.sig)), "=") + "\n")
	unlock()
	c, err := marketplace.New(marketplace.Options{URL: s.server.URL + "/index.json", PublicKey: strings.TrimRight(s.pub, "="), Dir: d.cache, PluginsDir: d.plugins})
	r.NoError(err)
	r.NoError(c.Refresh(context.Background()))
}
