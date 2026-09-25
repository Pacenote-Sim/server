package marketplace_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/marketplace"
)

// site is a fake marketplace: an index signed with a key of its own, and one
// package per artifact.
type site struct {
	t        *testing.T
	pub      string
	priv     ed25519.PrivateKey
	server   *httptest.Server
	mu       chan struct{}
	index    []byte
	sig      []byte
	packages map[string][]byte
	hits     atomic.Int64
	// tamper makes the index served differ from the one signed.
	tamper bool
	// fail makes every request answer 500.
	fail bool
	// etag is what the index is served with, and 304 answers an If-None-Match
	// that matches it.
	etag string
}

func newSite(t *testing.T) *site {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := &site{t: t, pub: base64.StdEncoding.EncodeToString(pub), priv: priv, packages: map[string][]byte{}, mu: make(chan struct{}, 1)}
	s.mu <- struct{}{}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *site) lock() func() { <-s.mu; return func() { s.mu <- struct{}{} } }

// https is the test server's address spelled as the index would spell it. The
// clients under test rewrite it back to plain http; see httpsIsFine.
func (s *site) https(path string) string {
	return "https://" + strings.TrimPrefix(s.server.URL, "http://") + path
}

func (s *site) serve(w http.ResponseWriter, r *http.Request) {
	unlock := s.lock()
	defer unlock()
	s.hits.Add(1)
	if s.fail {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	switch {
	case r.URL.Path == "/index.json":
		if s.etag != "" {
			if r.Header.Get("If-None-Match") == s.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", s.etag)
		}
		body := s.index
		if s.tamper {
			body = bytes.Replace(body, []byte("voice"), []byte("noise"), 1)
		}
		_, _ = w.Write(body)
	case r.URL.Path == "/index.json.sig":
		_, _ = w.Write(s.sig)
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		pkg, ok := s.packages[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pkg)
	default:
		http.NotFound(w, r)
	}
}

// publish signs and serves an index listing the plugins given, each with a
// package for this machine built from the files given.
func (s *site) publish(plugins ...marketplace.Plugin) {
	s.t.Helper()
	idx := marketplace.Index{Format: marketplace.Format, Generated: time.Now(), Plugins: plugins}
	body, err := json.Marshal(idx)
	require.NoError(s.t, err)
	unlock := s.lock()
	defer unlock()
	s.index = body
	s.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, body)) + "\n")
}

// pack makes a zip holding <name>/… from the files given and registers it
// at /dl/<file>; the artifact returned points at it with the right hash.
func (s *site) pack(name, file string, files map[string]string) marketplace.Artifact { //nolint:unparam // the name is the folder the zip must hold; every fixture is "voice"
	s.t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, body := range files {
		h := &zip.FileHeader{Name: path, Method: zip.Deflate}
		h.SetMode(0o644)
		if strings.HasSuffix(path, "/"+name) || strings.HasSuffix(path, ".exe") {
			h.SetMode(0o755)
		}
		w, err := zw.CreateHeader(h)
		require.NoError(s.t, err)
		_, err = w.Write([]byte(body))
		require.NoError(s.t, err)
	}
	require.NoError(s.t, zw.Close())
	sum := sha256.Sum256(buf.Bytes())
	unlock := s.lock()
	s.packages[file] = buf.Bytes()
	unlock()
	return marketplace.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: s.https("/dl/" + file), SHA256: hex.EncodeToString(sum[:])}
}

func (s *site) plugin(name, tag string, art marketplace.Artifact) marketplace.Plugin { //nolint:unparam // the tag varies where a second version is appended
	return marketplace.Plugin{
		Name: name, Kind: marketplace.KindServer, Title: strings.ToUpper(name[:1]) + name[1:], Summary: "A plugin for the tests.",
		Author: "Test", Calls: []string{},
		Versions: []marketplace.Version{{Tag: tag, Approved: "2026-09-25", InterfaceVersion: 3, Status: marketplace.StatusApproved, Artifacts: []marketplace.Artifact{art}}},
	}
}

func voiceFiles(binary string) map[string]string {
	return map[string]string{
		"voice/plugin.json":               `{"name":"voice","version":"0.1.0","binary":"` + binary + `","interface_version":3}`,
		"voice/" + binary:                 "#!/bin/sh\necho voice\n",
		"voice/migrations/00001_init.sql": "CREATE TABLE t (id int);",
		"voice/LICENSE":                   "x",
	}
}

func binaryName(name string) string { //nolint:unparam // one plugin name in the fixtures
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

type dirs struct{ cache, plugins string }

func newDirs(t *testing.T) dirs {
	t.Helper()
	root := t.TempDir()
	return dirs{cache: filepath.Join(root, "marketplace"), plugins: filepath.Join(root, "plugins")}
}

func client(t *testing.T, s *site, d dirs, on bool) *marketplace.Client {
	t.Helper()
	c, err := marketplace.New(marketplace.Options{
		URL: s.server.URL + "/index.json", PublicKey: s.pub, Dir: d.cache, PluginsDir: d.plugins,
		Enabled: func(context.Context) bool { return on },
		Now:     func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
		HTTP:    &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 10 * time.Second},
	})
	require.NoError(t, err)
	return c
}

func TestNew_Refuses(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	_, err := marketplace.New(marketplace.Options{})
	r.Error(err)
	_, err = marketplace.New(marketplace.Options{Dir: "x", PluginsDir: "y", PublicKey: "not a key"})
	r.Error(err)
	_, err = marketplace.New(marketplace.Options{Dir: "x", PluginsDir: "y", PublicKey: base64.StdEncoding.EncodeToString([]byte("short"))})
	r.Error(err)
	c, err := marketplace.New(marketplace.Options{Dir: "x", PluginsDir: "y"})
	r.NoError(err, "the built-in key and the default address")
	r.False(c.Enabled(context.Background()), "off by default")
	r.Nil(c.Snapshot().Index)
}

func TestRefresh_VerifiesAndCaches(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	s.publish(s.plugin("voice", "v0.1.0", art))

	c := client(t, s, d, true)
	r.NoError(c.Refresh(context.Background()))
	snap := c.Snapshot()
	r.NotNil(snap.Index)
	r.Equal("voice", snap.Index.Plugins[0].Name)
	r.Empty(snap.Err)
	r.Equal(2026, snap.FetchedAt.Year())
	r.Len(c.Available(), 1)

	// The cached copy comes back on a fresh client, without the network.
	again := client(t, s, d, false)
	r.NotNil(again.Snapshot().Index)
	r.Equal("voice", again.Snapshot().Index.Plugins[0].Name)
	r.False(again.Snapshot().FetchedAt.IsZero())

	// A tampered index is refused and the last good one kept.
	unlock := s.lock()
	s.tamper = true
	unlock()
	err := c.Refresh(context.Background())
	r.ErrorIs(err, marketplace.ErrSignature)
	r.Equal("voice", c.Snapshot().Index.Plugins[0].Name, "the last good index stays")
	r.Contains(c.Snapshot().Err, "signature")

	// A cached copy that was tampered with on disk is discarded on load.
	data, err := os.ReadFile(filepath.Join(d.cache, "index.json"))
	r.NoError(err)
	r.NoError(os.WriteFile(filepath.Join(d.cache, "index.json"), bytes.Replace(data, []byte("voice"), []byte("noise"), 1), 0o600))
	r.Nil(client(t, s, d, false).Snapshot().Index)
}

func TestRefresh_Failures(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	c := client(t, s, d, true)

	unlock := s.lock()
	s.fail = true
	unlock()
	r.ErrorContains(c.Refresh(context.Background()), "500")

	unlock = s.lock()
	s.fail = false
	s.index = []byte("{not json")
	s.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, s.index)))
	unlock()
	r.ErrorContains(c.Refresh(context.Background()), "not readable")

	other, err := json.Marshal(marketplace.Index{Format: 99})
	r.NoError(err)
	unlock = s.lock()
	s.index = other
	s.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, other)))
	unlock()
	r.ErrorContains(c.Refresh(context.Background()), "format 99")

	unlock = s.lock()
	s.sig = []byte("not base64!!")
	unlock()
	r.ErrorIs(c.Refresh(context.Background()), marketplace.ErrSignature)

	big := bytes.Repeat([]byte("x"), marketplace.MaxIndexSize+1)
	unlock = s.lock()
	s.index = big
	unlock()
	r.ErrorContains(c.Refresh(context.Background()), "larger than")

	// An address nothing answers.
	dead, err := marketplace.New(marketplace.Options{URL: "http://127.0.0.1:1/index.json", PublicKey: s.pub, Dir: d.cache, PluginsDir: d.plugins})
	r.NoError(err)
	r.Error(dead.Refresh(context.Background()))
	bad, err := marketplace.New(marketplace.Options{URL: "::not a url", PublicKey: s.pub, Dir: d.cache, PluginsDir: d.plugins})
	r.NoError(err)
	r.Error(bad.Refresh(context.Background()))
}

func TestRefresh_ETag(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	s.publish(s.plugin("voice", "v0.1.0", art))
	unlock := s.lock()
	s.etag = `"one"`
	unlock()

	c := client(t, s, d, true)
	r.NoError(c.Refresh(context.Background()))
	before := s.hits.Load()
	r.NoError(c.Refresh(context.Background()))
	r.Equal(before+1, s.hits.Load(), "an unchanged index costs one request and no signature fetch")
	r.NotNil(c.Snapshot().Index)
}

func TestRun(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	s.publish(s.plugin("voice", "v0.1.0", art))

	// Off: the loop never goes online.
	off := client(t, s, d, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	off.Run(ctx, 10*time.Millisecond)
	cancel()
	r.Zero(s.hits.Load())

	// On: it fetches at once and then on the tick, and logs a failure.
	on := client(t, s, d, true)
	ctx, cancel = context.WithTimeout(context.Background(), 80*time.Millisecond)
	on.Run(ctx, 10*time.Millisecond)
	cancel()
	r.GreaterOrEqual(s.hits.Load(), int64(2))
	r.NotNil(on.Snapshot().Index)
	unlock := s.lock()
	s.fail = true
	unlock()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	on.Run(ctx, 10*time.Millisecond)
	cancel()
	r.NotEmpty(on.Snapshot().Err)
}

func TestInstall(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	bin := binaryName("voice")
	art := s.pack("voice", "voice.zip", voiceFiles(bin))
	s.publish(s.plugin("voice", "v0.1.0", art))
	c := client(t, s, d, true)
	r.NoError(c.Refresh(context.Background()))

	done, err := c.Install(context.Background(), "voice")
	r.NoError(err)
	r.Equal(marketplace.Installed{Name: "voice", Tag: "v0.1.0"}, done)
	r.FileExists(filepath.Join(d.plugins, "voice", "plugin.json"))
	r.FileExists(filepath.Join(d.plugins, "voice", "migrations", "00001_init.sql"))
	info, err := os.Stat(filepath.Join(d.plugins, "voice", bin))
	r.NoError(err)
	if runtime.GOOS != "windows" {
		r.NotZero(info.Mode().Perm()&0o100, "the binary is executable")
	}
	entries, err := os.ReadDir(d.plugins)
	r.NoError(err)
	r.Len(entries, 1, "no staging directory is left behind")
	left, err := filepath.Glob(filepath.Join(d.cache, "package-*"))
	r.NoError(err)
	r.Empty(left, "the download is not kept")

	// A newer version replaces the folder whole: a file the old one had is gone.
	r.NoError(os.WriteFile(filepath.Join(d.plugins, "voice", "stale.txt"), []byte("x"), 0o600))
	files := voiceFiles(bin)
	files["voice/plugin.json"] = `{"name":"voice","version":"0.2.0","binary":"` + bin + `","interface_version":3}`
	art2 := s.pack("voice", "voice2.zip", files)
	p := s.plugin("voice", "v0.1.0", art)
	p.Versions = append(p.Versions, marketplace.Version{Tag: "v0.2.0", Approved: "2026-09-26", InterfaceVersion: 3, Status: marketplace.StatusApproved, Artifacts: []marketplace.Artifact{art2}})
	s.publish(p)
	r.NoError(c.Refresh(context.Background()))
	done, err = c.Install(context.Background(), "voice")
	r.NoError(err)
	r.True(done.Replaced)
	r.Equal("v0.2.0", done.Tag)
	r.NoFileExists(filepath.Join(d.plugins, "voice", "stale.txt"))
	r.NoDirExists(filepath.Join(d.plugins, "voice.replaced"))

	// Withdrawing the installed version is reported, in either spelling.
	p.Versions[1].Status = marketplace.StatusWithdrawn
	p.Versions[1].Notes = "leaked keys"
	s.publish(p)
	r.NoError(c.Refresh(context.Background()))
	v, gone := c.Withdrawn("voice", "0.2.0")
	r.True(gone)
	r.Equal("leaked keys", v.Notes)
	_, gone = c.Withdrawn("voice", "v0.1.0")
	r.False(gone)
	_, gone = c.Withdrawn("nobody", "v1.0.0")
	r.False(gone)
	r.Len(c.Available(), 1, "the older approved version is what is left")
	done, err = c.Install(context.Background(), "voice")
	r.NoError(err)
	r.Equal("v0.1.0", done.Tag, "installing again goes back to the newest approved tag")

	// With every version withdrawn there is nothing to install.
	p.Versions[0].Status = marketplace.StatusWithdrawn
	p.Versions[0].Notes = "same"
	s.publish(p)
	r.NoError(c.Refresh(context.Background()))
	r.Empty(c.Available())
	_, err = c.Install(context.Background(), "voice")
	r.ErrorIs(err, marketplace.ErrNotAvailable)
}

func TestInstall_Refusals(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	bin := binaryName("voice")

	good := s.pack("voice", "voice.zip", voiceFiles(bin))
	wrongHash := good
	wrongHash.SHA256 = strings.Repeat("0", 64)
	escape := s.pack("voice", "escape.zip", map[string]string{"voice/plugin.json": "{}", "../evil": "x"})
	outside := s.pack("voice", "outside.zip", map[string]string{"other/plugin.json": "{}"})
	noManifest := s.pack("voice", "nomanifest.zip", map[string]string{"voice/" + bin: "x"})
	wrongName := s.pack("voice", "wrongname.zip", map[string]string{"voice/plugin.json": `{"name":"other"}`, "voice/" + bin: "x"})
	badJSON := s.pack("voice", "badjson.zip", map[string]string{"voice/plugin.json": `{`, "voice/" + bin: "x"})
	noBinary := s.pack("voice", "nobinary.zip", map[string]string{"voice/plugin.json": `{"name":"voice"}`})
	notZip := marketplace.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: s.https("/dl/text.zip")}
	unlock := s.lock()
	s.packages["text.zip"] = []byte("this is not a zip")
	unlock()
	sum := sha256.Sum256([]byte("this is not a zip"))
	notZip.SHA256 = hex.EncodeToString(sum[:])
	missing := good
	missing.URL = s.https("/dl/missing.zip")
	plain := good
	plain.URL = s.server.URL + "/dl/voice.zip"
	otherOS := good
	otherOS.OS = "plan9"

	cases := map[string]struct {
		art  marketplace.Artifact
		want string
	}{
		"hash":        {wrongHash, "does not match the index"},
		"escape":      {escape, "outside the voice folder"},
		"outside":     {outside, "outside the voice folder"},
		"no manifest": {noManifest, "no voice/plugin.json"},
		"wrong name":  {wrongName, `calls the plugin "other"`},
		"bad json":    {badJSON, "not readable"},
		"no binary":   {noBinary, "to run"},
		"not a zip":   {notZip, "not a zip"},
		"missing":     {missing, "404"},
		"plain http":  {plain, "not served over https"},
		"other os":    {otherOS, "not available"},
	}
	for name, tc := range cases { //nolint:paralleltest // each case publishes its own index on the shared site
		t.Run(name, func(t *testing.T) {
			dd := newDirs(t)
			s.publish(s.plugin("voice", "v0.1.0", tc.art))
			cc, err := marketplace.New(marketplace.Options{
				URL: s.server.URL + "/index.json", PublicKey: s.pub, Dir: dd.cache, PluginsDir: dd.plugins,
				HTTP: &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 10 * time.Second},
			})
			require.NoError(t, err)
			require.NoError(t, cc.Refresh(context.Background()))
			_, err = cc.Install(context.Background(), "voice")
			require.ErrorContains(t, err, tc.want)
			require.NoDirExists(t, filepath.Join(dd.plugins, "voice"), "nothing is installed")
		})
	}

	// Not listed at all, and not a server plugin.
	client(t, s, d, true)
	c := client(t, s, d, true)
	_, err := c.Install(context.Background(), "voice")
	r.ErrorIs(err, marketplace.ErrNotAvailable, "no index yet")
	cp := s.plugin("client-voice", "v0.1.0", good)
	cp.Kind = "companion"
	s.publish(cp)
	r.NoError(c.Refresh(context.Background()))
	_, err = c.Install(context.Background(), "client-voice")
	r.ErrorIs(err, marketplace.ErrNotAvailable)
}

// httpsIsFine rewrites https:// test URLs to the plain test server, so the
// "must be https" rule can be exercised without a certificate.
type httpsIsFine struct{ next http.RoundTripper }

func (h httpsIsFine) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		req.URL.Scheme = "http"
	}
	return h.next.RoundTrip(req)
}

func TestInstall_Busy(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	s := newSite(t)
	d := newDirs(t)
	art := s.pack("voice", "voice.zip", voiceFiles(binaryName("voice")))
	s.publish(s.plugin("voice", "v0.1.0", art))

	// A download that blocks until released, so a second install can be
	// refused while the first is in flight. started is closed when the
	// download has arrived, so the assertion never races the goroutine; the
	// release is also a cleanup, so a failed assertion cannot wedge the server.
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/dl/") {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
		}
		s.serve(w, req)
	}))
	t.Cleanup(slow.Close)
	c, err := marketplace.New(marketplace.Options{
		URL: slow.URL + "/index.json", PublicKey: s.pub, Dir: d.cache, PluginsDir: d.plugins,
		HTTP: &http.Client{Transport: httpsIsFine{http.DefaultTransport}, Timeout: 10 * time.Second},
	})
	r.NoError(err)
	// The index says the package is on the fast server; point it at the slow one.
	art.URL = "https://" + strings.TrimPrefix(slow.URL, "http://") + "/dl/voice.zip"
	s.publish(s.plugin("voice", "v0.1.0", art))
	r.NoError(c.Refresh(context.Background()))

	errc := make(chan error, 1)
	go func() { _, err := c.Install(context.Background(), "voice"); errc <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		r.FailNow("the download never started")
	}
	_, err = c.Install(context.Background(), "voice")
	r.ErrorIs(err, marketplace.ErrBusy)
	once.Do(func() { close(release) })
	r.NoError(<-errc)
}
