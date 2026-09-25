// Package marketplace reads the index of approved plugins and installs server
// plugins from it.
//
// The index is a signed JSON document published by the marketplace
// repository's CI at a fixed address. This package fetches it, verifies the
// signature with the key compiled in below, keeps the last good copy on disk,
// and answers the panel's questions: what is available, what is withdrawn,
// and what to download. Nothing here runs a plugin; installing is putting a
// verified folder where the plugin host will find it.
//
// This is the one place the server goes online, and it goes only when the
// operator has turned the marketplace on. Off is the default.
package marketplace

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DefaultURL is where the index lives. It is served by pacenote.tech so that
// the address a server is given never changes even if the hosting does.
const DefaultURL = "https://www.pacenote.tech/marketplace/index.json"

// PublicKey verifies the index. It is the public half of the key the
// marketplace's CI signs with, and it is public by nature: anyone may check
// an index against it, and nobody can sign one with it.
const PublicKey = "phEZ3SnBskxv2pCvHS1BQK4RsQrCXMWnVRXUKT6YKGk="

// Format is the index format this server understands. An index in another
// format is refused rather than guessed at.
const Format = 1

// DefaultRefresh is how often the index is re-read while the marketplace is
// on.
const DefaultRefresh = time.Hour

// Limits on what is fetched. An index is a few kilobytes; a package is a
// plugin binary, tens of megabytes. Both are bounded so a wrong or hostile
// answer cannot fill the disk.
const (
	MaxIndexSize   = 4 << 20
	MaxPackageSize = 256 << 20
)

// The kinds and statuses the index uses.
const (
	KindServer      = "server"
	StatusApproved  = "approved"
	StatusWithdrawn = "withdrawn"
)

// Index is index.json.
type Index struct {
	Format    int       `json:"format"`
	Generated time.Time `json:"generated"`
	Plugins   []Plugin  `json:"plugins"`
}

// Plugin is one listing.
type Plugin struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Author      string    `json:"author"`
	Repository  string    `json:"repository"`
	Module      string    `json:"module"`
	Licence     string    `json:"licence"`
	Visibility  string    `json:"visibility"`
	Pricing     string    `json:"pricing"`
	Website     string    `json:"website,omitempty"`
	Simulators  []string  `json:"simulators,omitempty"`
	Calls       []string  `json:"calls"`
	CompanionOf string    `json:"companion_of,omitempty"`
	Versions    []Version `json:"versions"`
}

// Version is one tag of one plugin.
type Version struct {
	Tag              string     `json:"tag"`
	Approved         string     `json:"approved"`
	InterfaceVersion int        `json:"interface_version"`
	Status           string     `json:"status"`
	Notes            string     `json:"notes,omitempty"`
	ModuleHash       string     `json:"module_hash,omitempty"`
	Artifacts        []Artifact `json:"artifacts,omitempty"`
}

// Artifact is one built package.
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Latest is the newest version that is still approved, or nil.
func (p Plugin) Latest() *Version {
	for i := len(p.Versions) - 1; i >= 0; i-- {
		if p.Versions[i].Status == StatusApproved {
			return &p.Versions[i]
		}
	}
	return nil
}

// Find is the plugin by name, if listed.
func (idx *Index) Find(name string) (Plugin, bool) {
	if idx == nil {
		return Plugin{}, false
	}
	for _, p := range idx.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return Plugin{}, false
}

// ArtifactFor is the package built for this machine, if the version has one.
func (v Version) ArtifactFor(goos, goarch string) (Artifact, bool) {
	for _, a := range v.Artifacts {
		if a.OS == goos && a.Arch == goarch {
			return a, true
		}
	}
	return Artifact{}, false
}

// Snapshot is what the panel is shown: the last good index, when it was
// fetched, and why the last attempt failed if it did.
type Snapshot struct {
	Index     *Index
	FetchedAt time.Time
	// Err is the last refresh's failure, empty when it succeeded. The index
	// beside it is still the last good one, which is the point of keeping it.
	Err string
}

// Options is what the client needs.
type Options struct {
	// URL is the index. Empty means [DefaultURL].
	URL string
	// PublicKey verifies it. Empty means [PublicKey]; a test sets its own.
	PublicKey string
	// Dir is where the last good copy is kept, and where a download is
	// unpacked before it is moved into place.
	Dir string
	// PluginsDir is where an installed plugin ends up: one directory per
	// plugin, the same directory the plugin host reads.
	PluginsDir string
	// Enabled says whether the operator has turned the marketplace on. It is
	// asked on every tick, so turning it off stops the next fetch without a
	// restart. nil means off.
	Enabled func(context.Context) bool
	// HTTP is the client used for every request. nil means one with sensible
	// timeouts.
	HTTP *http.Client
	// UserAgent is sent with every request, so that the marketplace can tell
	// server versions apart in its logs.
	UserAgent string
	Log       *slog.Logger
	Now       func() time.Time
	// MaxPackage caps a download. Zero means [MaxPackageSize]; a test sets
	// something it can afford to serve.
	MaxPackage int64
}

// Client fetches, verifies, caches and installs.
type Client struct {
	opts Options
	key  ed25519.PublicKey
	http *http.Client

	mu   sync.Mutex
	snap Snapshot
	etag string
	// installing is the one install at a time. Two at once could race for the
	// same directory, and there is no reason to run two.
	installing bool
}

// New builds a client and loads the last good copy, if there is one. It does
// not go online: [Client.Refresh] and [Client.Run] do, when asked.
func New(opts Options) (*Client, error) {
	if opts.Dir == "" || opts.PluginsDir == "" {
		return nil, errors.New("marketplace: the cache directory and the plugins directory are both required")
	}
	if opts.URL == "" {
		opts.URL = DefaultURL
	}
	if opts.PublicKey == "" {
		opts.PublicKey = PublicKey
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Enabled == nil {
		opts.Enabled = func(context.Context) bool { return false }
	}
	if opts.MaxPackage <= 0 {
		opts.MaxPackage = MaxPackageSize
	}
	key, err := decodeKey(opts.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("marketplace: the public key is not a 32-byte ed25519 key")
	}
	c := &Client{opts: opts, key: ed25519.PublicKey(key), http: opts.HTTP}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	c.load()
	return c, nil
}

// Snapshot is the last good index and how the last refresh went.
func (c *Client) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

// Enabled reports whether the operator has the marketplace on.
func (c *Client) Enabled(ctx context.Context) bool { return c.opts.Enabled(ctx) }

// Run refreshes the index on a tick while the marketplace is on, and returns
// when the context does. It refreshes once at the start, so a server that comes
// up with the marketplace on does not wait an hour for its first list.
func (c *Client) Run(ctx context.Context, every time.Duration) {
	c.tick(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *Client) tick(ctx context.Context) {
	if !c.opts.Enabled(ctx) {
		return
	}
	if err := c.Refresh(ctx); err != nil {
		c.opts.Log.LogAttrs(ctx, slog.LevelWarn, "the marketplace index could not be refreshed",
			slog.String("reason", err.Error()))
	}
}

// Refresh fetches the index and its signature, verifies them, and replaces the
// cached copy. A failure leaves the last good copy in place and is recorded in
// the snapshot, so the page can say when the list was last true.
func (c *Client) Refresh(ctx context.Context) error {
	err := c.refresh(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.snap.Err = err.Error()
		return err
	}
	c.snap.Err = ""
	c.snap.FetchedAt = c.opts.Now()
	return nil
}

func (c *Client) refresh(ctx context.Context) error {
	c.mu.Lock()
	etag := c.etag
	c.mu.Unlock()

	body, newTag, changed, err := c.fetch(ctx, c.opts.URL, MaxIndexSize, etag)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if !changed {
		return nil
	}
	sig, _, _, err := c.fetch(ctx, c.opts.URL+".sig", 4096, "")
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	idx, err := c.verify(body, sig)
	if err != nil {
		return err
	}
	if err := c.store(body, sig); err != nil {
		return err
	}
	c.mu.Lock()
	c.snap.Index = idx
	c.etag = newTag
	c.mu.Unlock()
	return nil
}

// fetch reads one URL with a size cap. It reports whether the body changed
// since the ETag given, so an unchanged index costs one small request.
func (c *Client) fetch(ctx context.Context, url string, limit int64, etag string) (body []byte, newTag string, changed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, "", false, err
	}
	if c.opts.UserAgent != "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotModified && etag != "" {
		return nil, etag, false, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("%s answered %s", url, res.Status)
	}
	body, err = io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", false, err
	}
	if int64(len(body)) > limit {
		return nil, "", false, fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return body, res.Header.Get("ETag"), true, nil
}

// ErrSignature is an index whose signature does not verify, which is an index
// this server will not act on whatever it says.
var ErrSignature = errors.New("marketplace: the index's signature does not verify")

func (c *Client) verify(body, sig []byte) (*Index, error) {
	raw, err := decodeKey(strings.TrimSpace(string(sig)))
	if err != nil {
		return nil, fmt.Errorf("%w: the signature is not base64", ErrSignature)
	}
	if !ed25519.Verify(c.key, body, raw) {
		return nil, ErrSignature
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("marketplace: the index is not readable: %w", err)
	}
	if idx.Format != Format {
		return nil, fmt.Errorf("marketplace: the index is format %d and this server understands %d — upgrade the server", idx.Format, Format)
	}
	return &idx, nil
}

// decodeKey reads base64 with or without its padding, and with the whitespace
// a copy tends to add.
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	return b, nil
}

const (
	cacheIndex     = "index.json"
	cacheSignature = "index.json.sig"
)

// store writes the verified index and its signature beside each other, each
// through a temporary file, so a crash mid-write leaves the old copy whole.
func (c *Client) store(body, sig []byte) error {
	if err := os.MkdirAll(c.opts.Dir, 0o700); err != nil {
		return fmt.Errorf("marketplace: cannot create %s: %w", c.opts.Dir, err)
	}
	if err := writeFile(filepath.Join(c.opts.Dir, cacheIndex), body); err != nil {
		return err
	}
	return writeFile(filepath.Join(c.opts.Dir, cacheSignature), sig)
}

func writeFile(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".*")
	if err != nil {
		return fmt.Errorf("marketplace: cannot write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return fmt.Errorf("marketplace: cannot write %s: %w", name, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("marketplace: cannot write %s: %w", name, err)
	}
	if err := os.Rename(tmp, name); err != nil {
		return fmt.Errorf("marketplace: cannot write %s: %w", name, err)
	}
	return nil
}

// load reads the cached copy, verifying it again: a file on disk is not
// trusted more than one from the network.
func (c *Client) load() {
	body, err := os.ReadFile(filepath.Join(c.opts.Dir, cacheIndex))
	if err != nil {
		return
	}
	sig, err := os.ReadFile(filepath.Join(c.opts.Dir, cacheSignature))
	if err != nil {
		return
	}
	idx, err := c.verify(body, sig)
	if err != nil {
		c.opts.Log.LogAttrs(context.Background(), slog.LevelWarn, "the cached marketplace index was discarded",
			slog.String("reason", err.Error()))
		return
	}
	info, _ := os.Stat(filepath.Join(c.opts.Dir, cacheIndex))
	c.snap = Snapshot{Index: idx}
	if info != nil {
		c.snap.FetchedAt = info.ModTime()
	}
}

// Available is every server plugin in the index that has an approved version
// built for this machine, in the index's order.
func (c *Client) Available() []Plugin {
	return available(c.Snapshot().Index, runtime.GOOS, runtime.GOARCH)
}

func available(idx *Index, goos, goarch string) []Plugin {
	if idx == nil {
		return nil
	}
	var out []Plugin
	for _, p := range idx.Plugins {
		if p.Kind != KindServer {
			continue
		}
		v := p.Latest()
		if v == nil {
			continue
		}
		if _, ok := v.ArtifactFor(goos, goarch); !ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Withdrawn reports whether the index has withdrawn the version of a plugin
// that is installed, and why. The plugin's own manifest carries "0.2.0"; the
// index carries "v0.2.0"; both spellings are accepted.
func (c *Client) Withdrawn(name, version string) (Version, bool) {
	p, ok := c.Snapshot().Index.Find(name)
	if !ok {
		return Version{}, false
	}
	tag := version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	for _, v := range p.Versions {
		if v.Tag == tag && v.Status == StatusWithdrawn {
			return v, true
		}
	}
	return Version{}, false
}
