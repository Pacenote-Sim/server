package marketplace

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Installed is what an install did.
type Installed struct {
	Name string
	Tag  string
	// Replaced is whether a directory of that name was already there and was
	// swapped out. The caller decides whether the running plugin should be
	// restarted; this package moves files.
	Replaced bool
}

// ErrBusy is an install started while another is running.
var ErrBusy = errors.New("marketplace: another plugin is being installed right now")

// ErrNotAvailable is a plugin the index does not list for this machine.
var ErrNotAvailable = errors.New("marketplace: that plugin is not available for this machine")

// The shape a package must have. A plugin's folder is small; these bounds are
// there so a wrong or hostile zip cannot fill the disk or the inode table.
const (
	maxEntries  = 4096
	maxUnpacked = 512 << 20
)

// Install downloads the newest approved package of a server plugin, checks it
// against the signed index, unpacks it beside the plugin directory, and moves
// it into place. An existing directory of the same name is replaced whole,
// never merged, so a file the new version dropped does not linger.
//
// It does not tell the plugin host. The caller rescans, and restarts the plugin
// if it was running, because that is the host's business and the host has the
// context for it.
func (c *Client) Install(ctx context.Context, name string) (Installed, error) {
	c.mu.Lock()
	if c.installing {
		c.mu.Unlock()
		return Installed{}, ErrBusy
	}
	c.installing = true
	idx := c.snap.Index
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.installing = false
		c.mu.Unlock()
	}()

	p, ok := idx.Find(name)
	if !ok || p.Kind != KindServer {
		return Installed{}, ErrNotAvailable
	}
	v := p.Latest()
	if v == nil {
		return Installed{}, ErrNotAvailable
	}
	art, ok := v.ArtifactFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return Installed{}, ErrNotAvailable
	}
	if !strings.HasPrefix(art.URL, "https://") {
		return Installed{}, fmt.Errorf("marketplace: %s is not served over https", art.URL)
	}

	if err := os.MkdirAll(c.opts.Dir, 0o700); err != nil {
		return Installed{}, fmt.Errorf("marketplace: cannot create %s: %w", c.opts.Dir, err)
	}
	pkg, err := c.download(ctx, art)
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = os.Remove(pkg) }()

	if err := os.MkdirAll(c.opts.PluginsDir, 0o700); err != nil {
		return Installed{}, fmt.Errorf("marketplace: cannot create %s: %w", c.opts.PluginsDir, err)
	}
	staging, err := os.MkdirTemp(c.opts.PluginsDir, ".install-"+name+"-")
	if err != nil {
		return Installed{}, fmt.Errorf("marketplace: cannot unpack: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := unpack(pkg, name, staging); err != nil {
		return Installed{}, err
	}

	replaced, err := swap(filepath.Join(staging, name), filepath.Join(c.opts.PluginsDir, name))
	if err != nil {
		return Installed{}, err
	}
	return Installed{Name: name, Tag: v.Tag, Replaced: replaced}, nil
}

// download fetches the package to a file in the cache directory and checks
// its hash against the index before anything is opened.
func (c *Client) download(ctx context.Context, art Artifact) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, art.URL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("marketplace: %w", err)
	}
	if c.opts.UserAgent != "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("marketplace: download: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("marketplace: download: %s answered %s", art.URL, res.Status)
	}

	f, err := os.CreateTemp(c.opts.Dir, "package-*.zip")
	if err != nil {
		return "", fmt.Errorf("marketplace: download: %w", err)
	}
	tmp := f.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, c.opts.MaxPackage+1))
	cerr := f.Close()
	if err := errors.Join(err, cerr); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("marketplace: download: %w", err)
	}
	if n > c.opts.MaxPackage {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("marketplace: download: the package is larger than %d bytes", c.opts.MaxPackage)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, art.SHA256) {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("marketplace: the package does not match the index: sha256 %s, expected %s", got[:12], art.SHA256[:min(12, len(art.SHA256))])
	}
	return tmp, nil
}

// unpack extracts <name>/… from the zip into dir, refusing anything that is not
// a plain file or directory under that one folder.
func unpack(pkg, name, dir string) error {
	zr, err := zip.OpenReader(pkg)
	if err != nil {
		return fmt.Errorf("marketplace: the package is not a zip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) == 0 || len(zr.File) > maxEntries {
		return fmt.Errorf("marketplace: the package holds %d entries", len(zr.File))
	}
	var total uint64
	sawManifest := false
	for _, f := range zr.File {
		clean := path.Clean(f.Name)
		if !strings.HasPrefix(clean, name+"/") && clean != name {
			return fmt.Errorf("marketplace: the package holds %q, outside the %s folder", f.Name, name)
		}
		if strings.Contains(clean, "..") || path.IsAbs(clean) {
			return fmt.Errorf("marketplace: the package holds %q, which is not a plain path", f.Name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			continue
		case !mode.IsRegular():
			return fmt.Errorf("marketplace: the package holds %q, which is not a regular file", f.Name)
		}
		total += f.UncompressedSize64
		if total > maxUnpacked {
			return fmt.Errorf("marketplace: the package unpacks to more than %d bytes", maxUnpacked)
		}
		if clean == name+"/plugin.json" {
			sawManifest = true
		}
	}
	if !sawManifest {
		return fmt.Errorf("marketplace: the package holds no %s/plugin.json", name)
	}
	for _, f := range zr.File {
		if err := extract(f, dir); err != nil {
			return err
		}
	}
	return checkManifest(filepath.Join(dir, name, "plugin.json"), name)
}

func extract(f *zip.File, dir string) error {
	target := filepath.Join(dir, filepath.FromSlash(path.Clean(f.Name)))
	if f.Mode().IsDir() {
		return os.MkdirAll(target, 0o700)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("marketplace: unpack: %w", err)
	}
	r, err := f.Open()
	if err != nil {
		return fmt.Errorf("marketplace: unpack %s: %w", f.Name, err)
	}
	defer func() { _ = r.Close() }()
	mode := fs.FileMode(0o600)
	if f.Mode()&0o111 != 0 {
		mode = 0o700
	}
	w, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // the path was checked to sit under the staging folder
	if err != nil {
		return fmt.Errorf("marketplace: unpack %s: %w", f.Name, err)
	}
	_, cerr := io.Copy(w, io.LimitReader(r, int64(f.UncompressedSize64)+1)) //nolint:gosec // bounded by the entry's declared size and the total cap
	if err := errors.Join(cerr, w.Close()); err != nil {
		return fmt.Errorf("marketplace: unpack %s: %w", f.Name, err)
	}
	return nil
}

// checkManifest confirms the folder calls itself what the index does, which
// is what the plugin host will insist on anyway; refusing here means the old
// version is never replaced by a folder the host would then reject.
func checkManifest(name, want string) error {
	data, err := os.ReadFile(name) //nolint:gosec // a file this install just unpacked
	if err != nil {
		return fmt.Errorf("marketplace: %w", err)
	}
	var m struct {
		Name   string `json:"name"`
		Binary string `json:"binary"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("marketplace: the package's plugin.json is not readable: %w", err)
	}
	if m.Name != want {
		return fmt.Errorf("marketplace: the package's plugin.json calls the plugin %q, the index %q", m.Name, want)
	}
	if m.Binary == "" {
		m.Binary = want
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(m.Binary, ".exe") {
		m.Binary += ".exe"
	}
	bin := filepath.Join(filepath.Dir(name), m.Binary)
	if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // it is the plugin's program; the host refuses one without the execute bit
		return fmt.Errorf("marketplace: the package holds no %s to run: %w", m.Binary, err)
	}
	return nil
}

// swap moves the unpacked folder into place. An existing one is moved aside
// first and removed after, so a failure between the two leaves the old version
// recoverable rather than half a new one.
func swap(from, to string) (replaced bool, err error) {
	_, statErr := os.Stat(to)
	exists := statErr == nil
	aside := to + ".replaced"
	if exists {
		_ = os.RemoveAll(aside)
		if err := os.Rename(to, aside); err != nil {
			return false, fmt.Errorf("marketplace: cannot move the old %s aside: %w", filepath.Base(to), err)
		}
	}
	if err := os.Rename(from, to); err != nil {
		if exists {
			_ = os.Rename(aside, to)
		}
		return false, fmt.Errorf("marketplace: cannot move %s into place: %w", filepath.Base(to), err)
	}
	if exists {
		_ = os.RemoveAll(aside)
	}
	return exists, nil
}
