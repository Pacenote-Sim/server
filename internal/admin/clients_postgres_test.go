//go:build postgres

package admin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
	"github.com/pacenote-sim/server/internal/db"
)

// clientPanel is the panel with a prebuilt client on the server, which is what
// every test that presses the build button needs.
type clientPanel struct {
	*panel
	// prebuilt is the file the builds are stamped copies of.
	prebuilt string
	// dir is where the built clients land.
	dir string
}

func newClientPanel(t *testing.T) *clientPanel {
	t.Helper()
	return newClientPanelWith(t)
}

// newClientPanelWith is the build page with something else set as well, which
// is what the certificate tests need: the same page, plus a data key to seal a
// certificate with.
func newClientPanelWith(t *testing.T, opts ...func(*admin.Deps)) *clientPanel {
	t.Helper()
	prebuilt := clientbuildtest.Install(t)
	dir := filepath.Join(t.TempDir(), "client-builds")
	// The builder is set last, after whatever the caller asked for, because it
	// is built out of the rest of the dependencies: the certificate it signs
	// with comes from the store and the keyring, exactly as it does on a
	// running server. A test double there would leave the one path that opens a
	// private key untested.
	p := newPanel(t, append(opts, func(d *admin.Deps) {
		d.Clients = clientbuild.Builder{
			Source:      clientbuild.Source{Path: prebuilt},
			Dir:         dir,
			Certificate: admin.Certificate(d.Store, d.Keyring),
		}
	})...)
	return &clientPanel{panel: p, prebuilt: prebuilt, dir: dir}
}

// body is what the last post rendered.
func (p *clientPanel) body() string { return string(p.lastBody) }

// build presses the button with a real token from a real page.
func (p *clientPanel) build(address string, signing clientbuild.Signing) int {
	p.t.Helper()
	page := p.get(admin.ClientsPath)
	return p.post(admin.ClientsPath+"/build", url.Values{
		"csrf":    {csrfOf(p.t, page)},
		"address": {address},
		"signing": {string(signing)},
	})
}

// built lists the files in the builds directory.
func (p *clientPanel) built() []string {
	p.t.Helper()
	entries, err := os.ReadDir(p.dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(p.t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// download fetches one built client, which is not a page and so does not go
// through the helper that insists on a whole document.
func (p *panel) download(path string) (int, []byte) {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.server.URL+path, http.NoBody)
	r.NoError(err)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	r.NoError(err)
	return res.StatusCode, body
}

func TestBuildPageWithoutAClientToBuildFrom(t *testing.T) {
	t.Parallel()

	t.Run("a server that has not been given one says what to put where", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		dir := t.TempDir()
		missing := filepath.Join(dir, "client", "pacenote-telemetry.exe")
		p := newPanel(t, func(d *admin.Deps) {
			d.Clients = clientbuild.Builder{
				Source: clientbuild.Source{Path: missing},
				Dir:    filepath.Join(dir, "client-builds"),
			}
		})

		body := p.get(admin.ClientsPath)
		r.Contains(body, "There is no client on this server yet")
		r.Contains(body, missing, "the empty state names the path to copy the file to")
		r.Contains(body, "pacenote-telemetry.exe")
		r.Contains(body, "PACENOTE_CLIENT_BINARY", "and how to say it lives somewhere else")
		r.Contains(body, "disabled>Build client", "the button is there and plainly cannot be pressed")
		r.Contains(body, "Nothing has been built here yet")
	})

	t.Run("a server that was never told where to look says that instead", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get(admin.ClientsPath)
		r.Contains(body, "has not been told where to look")
		r.Contains(body, "disabled>Build client")
	})

	t.Run("pressing build anyway builds nothing and says why", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		dir := t.TempDir()
		p := newPanel(t, func(d *admin.Deps) {
			d.Clients = clientbuild.Builder{
				Source: clientbuild.Source{Path: filepath.Join(dir, "client", "pacenote-telemetry.exe")},
				Dir:    filepath.Join(dir, "client-builds"),
			}
		})

		page := p.get(admin.ClientsPath)
		status := p.post(admin.ClientsPath+"/build", url.Values{
			"csrf":    {csrfOf(t, page)},
			"address": {"https://pacenote.example.com"},
			"signing": {string(clientbuild.SigningNone)},
		})
		r.Equal(http.StatusConflict, status)
		r.Contains(string(p.lastBody), "There is no client on this server to build from")
		r.Zero(p.auditCount(db.ActionClientBuilt))
	})

	t.Run("a file that is not a client is named as one", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		dir := t.TempDir()
		path := clientbuildtest.WriteAt(t, filepath.Join(dir, "pacenote-telemetry.exe"),
			[]byte("not an executable at all"))
		p := newPanel(t, func(d *admin.Deps) {
			d.Clients = clientbuild.Builder{Source: clientbuild.Source{Path: path}, Dir: dir}
		})

		body := p.get(admin.ClientsPath)
		r.Contains(body, "not a Windows executable")
		r.Contains(body, "disabled>Build client")
	})
}

func TestBuildAClient(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		typed   string
		stamped string
	}{
		{"a public host", "https://pacenote.example.com", "https://pacenote.example.com"},
		{"a host and a port", "https://pacenote.example.com:8443", "https://pacenote.example.com:8443"},
		{"a host typed without a scheme", "pacenote.example.com", "https://pacenote.example.com"},
		{"a server on the driver's own machine", "http://localhost:8080", "http://localhost:8080"},
		{"a trailing slash, which is not part of an address", "https://pacenote.example.com/", "https://pacenote.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			p := newClientPanel(t)

			r.Equal(http.StatusOK, p.build(tc.typed, clientbuild.SigningNone))
			body := string(p.lastBody)
			r.Contains(body, "Built — the file below is a client that talks to "+tc.stamped)
			r.Contains(body, "The client you just built")
			r.Contains(body, "Windows will warn your drivers the app is untrusted")

			// One file, and one row in the history that names it.
			r.Len(p.built(), 1)
			r.Equal(1, p.auditCount(db.ActionClientBuilt), "one audit row per build")

			builds, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
			r.NoError(err)
			r.Len(builds, 1)
			r.Equal(tc.stamped, builds[0].Address)
			r.Equal(panelEmail, builds[0].Actor)
			r.Equal(string(clientbuild.SigningNone), builds[0].Signing)
			r.Equal("pacenote-telemetry.exe", builds[0].FileName)

			// The page shows the digest and the address back, so a mistake is
			// visible before the file is handed to a league.
			r.Contains(body, builds[0].SHA256)
			r.Contains(body, tc.stamped)

			// And the file itself is a stamped copy of the prebuilt client.
			status, downloaded := p.download(admin.ClientDownloadPath + builds[0].Reference)
			r.Equal(http.StatusOK, status)

			original, err := os.ReadFile(p.prebuilt)
			r.NoError(err)
			r.Len(downloaded, len(original), "a built client is the size of the one it was copied from")

			at, err := clientbuild.Find(original)
			r.NoError(err)
			r.Equal(original[:at], downloaded[:at], "a byte before the region changed")
			r.Equal(original[at+clientbuild.RegionSize:], downloaded[at+clientbuild.RegionSize:],
				"a byte after the region changed")

			read, err := clientbuild.Read(downloaded)
			r.NoError(err)
			r.Equal(tc.stamped, read.Address, "the client reads back the address it was built for")
			r.Equal(builds[0].Reference, read.Build, "and says which build it came from")

			_, err = pe.NewFile(bytes.NewReader(downloaded))
			r.NoError(err, "the downloaded client is not a valid PE executable")

			sum := sha256.Sum256(downloaded)
			r.Equal(builds[0].SHA256, hex.EncodeToString(sum[:]),
				"the digest on the page is the digest of what a driver downloads")
		})
	}
}

func TestBuildRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		address string
		signing clientbuild.Signing
		status  int
		says    string
	}{
		{
			name: "no address at all", address: "", signing: clientbuild.SigningNone,
			status: http.StatusUnprocessableEntity, says: "needs a server address",
		},
		{
			name:    "plain HTTP to somewhere that is not the driver's machine",
			address: "http://pacenote.example.com", signing: clientbuild.SigningNone,
			status: http.StatusUnprocessableEntity, says: "the client will refuse to start",
		},
		{
			name: "a scheme no client speaks", address: "ftp://pacenote.example.com",
			signing: clientbuild.SigningNone,
			status:  http.StatusUnprocessableEntity, says: "not an address it can reach",
		},
		{
			name: "a path rather than a host", address: "https://pacenote.example.com/api/v1",
			signing: clientbuild.SigningNone,
			status:  http.StatusUnprocessableEntity, says: "is a host, not a path",
		},
		{
			name: "a password in the address", address: "https://ana:secret@pacenote.example.com",
			signing: clientbuild.SigningNone,
			status:  http.StatusUnprocessableEntity, says: "no user name or password",
		},
		{
			name:    "signing with a certificate this server has not been given",
			address: "https://pacenote.example.com", signing: clientbuild.SigningOwnCertificate,
			status: http.StatusUnprocessableEntity, says: "no certificate to sign with",
		},
		{
			name: "a signing route that does not exist", address: "https://pacenote.example.com",
			signing: clientbuild.Signing("free"),
			status:  http.StatusUnprocessableEntity, says: "not one of the ways",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			p := newClientPanel(t)

			r.Equal(tc.status, p.build(tc.address, tc.signing))
			body := string(p.lastBody)
			r.Contains(body, tc.says)

			r.Empty(p.built(), "a refused build still wrote a file")
			r.Zero(p.auditCount(db.ActionClientBuilt))
			builds, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
			r.NoError(err)
			r.Empty(builds, "a refused build still reached the history")

			if tc.address != "" {
				r.Contains(body, tc.address, "the address comes back in the form rather than being lost")
			}
		})
	}
}

func TestEveryBuildIsRecorded(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newClientPanel(t)

	r.Equal(http.StatusOK, p.build("https://one.example.com", clientbuild.SigningNone))
	r.Equal(http.StatusOK, p.build("https://two.example.com", clientbuild.SigningNone))

	r.Equal(2, p.auditCount(db.ActionClientBuilt), "one audit row per build, not one per page")
	r.Len(p.built(), 2, "each build is its own file")

	builds, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
	r.NoError(err)
	r.Len(builds, 2)
	r.Equal("https://two.example.com", builds[0].Address, "newest first")
	r.Equal("https://one.example.com", builds[1].Address)
	r.NotEqual(builds[0].Reference, builds[1].Reference)
	r.NotEqual(builds[0].SHA256, builds[1].SHA256, "two addresses are two different files")

	// The older build still downloads, and still carries its own address: a
	// client an operator handed out last month keeps working.
	status, older := p.download(admin.ClientDownloadPath + builds[1].Reference)
	r.Equal(http.StatusOK, status)
	read, err := clientbuild.Read(older)
	r.NoError(err)
	r.Equal("https://one.example.com", read.Address)

	body := p.get(admin.ClientsPath)
	r.Contains(body, "https://one.example.com")
	r.Contains(body, "https://two.example.com")
}

func TestBuildHistoryPages(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newClientPanel(t)
	ctx := context.Background()

	// Rows rather than builds: the page under test is the history, and
	// twenty-eight real stampings would be twenty-eight copies of a file to
	// prove something about a cursor.
	for i := range db.PageSize + 3 {
		_, err := p.store.RecordClientBuild(ctx, db.NewClientBuild{
			Actor:         panelEmail,
			Address:       fmt.Sprintf("https://league-%02d.example.com", i),
			Signing:       string(clientbuild.SigningNone),
			Reference:     fmt.Sprintf("%016x", i),
			FileName:      "pacenote-telemetry.exe",
			FileBytes:     4096,
			SHA256:        strings.Repeat("a", 64),
			ClientVersion: "v1.2.3",
		})
		r.NoError(err)
	}

	first := p.get(admin.ClientsPath)
	r.Contains(first, "Next page")
	r.Contains(first, "league-27", "newest first")
	r.NotContains(first, "league-02")
	r.Contains(first, "not on this server any more", "a row whose file has gone says so rather than offering a link")

	next := p.get(hrefAfter(t, first, "Next page"))
	r.Contains(next, "league-02")
	r.Contains(next, "league-00")
	r.NotContains(next, "league-27", "the second page starts after the first one ends")
	r.NotContains(next, "Next page")

	// And the way back, because an operator three pages into a history has no
	// browser button in their muscle memory for a keyset cursor.
	back := p.get(hrefAfter(t, next, "Back to the first page"))
	r.Contains(back, "league-27")
	r.NotContains(back, "league-00")

	// The latest build's card belongs to the first page only: it is the answer
	// to "what did I just build", not a row that follows the operator around.
	r.Contains(first, "The last client you built")
	r.NotContains(next, "The last client you built")
}

func TestDownloadingSomethingThatIsNotThere(t *testing.T) {
	t.Parallel()

	t.Run("a reference nothing was built under", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		status, _ := p.download(admin.ClientDownloadPath + "00112233445566ff")
		r.Equal(http.StatusNotFound, status)
	})

	t.Run("a reference that is not one", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		status, _ := p.download(admin.ClientDownloadPath + "not-a-reference")
		r.Equal(http.StatusNotFound, status)
	})

	t.Run("a build whose file has been cleared away", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		r.Equal(http.StatusOK, p.build("https://pacenote.example.com", clientbuild.SigningNone))
		builds, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
		r.NoError(err)
		r.Len(builds, 1)

		for _, name := range p.built() {
			r.NoError(os.Remove(filepath.Join(p.dir, name)))
		}

		status, body := p.download(admin.ClientDownloadPath + builds[0].Reference)
		r.Equal(http.StatusNotFound, status)
		r.Contains(string(body), "no longer on this server")

		page := p.get(admin.ClientsPath)
		r.Contains(page, "The file itself is no longer on this server")
		r.Contains(page, "https://pacenote.example.com", "the history still says what it pointed at")
	})
}

func TestTheBuildPageIsHonestAboutSigning(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newClientPanel(t)

	body := p.get(admin.ClientsPath)

	// Both routes are named and described, and the page says plainly what a
	// driver sees in each case.
	r.Contains(body, "Unsigned")
	r.Contains(body, "Your own certificate")
	r.Contains(body, "Windows will warn your drivers the app is untrusted, and they choose to run it anyway")
	r.Contains(body, "who the file came from and that nobody changed it")
	r.Contains(body, `value="own-certificate"`)

	// The one that cannot be chosen yet explains itself rather than being
	// greyed out in silence, and the one that works is already selected.
	r.Contains(body, `value="unsigned" checked>`)
	r.Contains(body, `value="own-certificate" disabled>`)
	r.NotContains(body, `value="unsigned" disabled>`)
	r.Contains(body, "no certificate to sign with")

	// The paid service we had not built is gone rather than disabled. Offering
	// to sell something that does not exist is worse than not mentioning it.
	r.NotContains(body, "Signed by us")
	r.NotContains(body, `value="hosted"`)
	r.NotContains(body, "our infrastructure")
}
