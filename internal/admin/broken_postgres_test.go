//go:build postgres

package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/clientbuild/clientbuildtest"
	"github.com/pacenote-sim/server/internal/db"
)

// Every page of the panel with the thing it reads taken out from under it.
//
// This is the panel's one promise about failure: a page whose query fails still
// renders, with a sentence saying which part is missing. It matters because the
// panel is where an operator goes when something is wrong — a 500 tells them
// nothing they did not already know, and a blank page tells them less.
//
// The table is dropped rather than the connection closed, because closing the
// connection takes the session with it and the operator is bounced to the
// sign-in page instead of seeing the answer. A missing table is also the more
// realistic failure: it is what a half-applied migration looks like.
func TestEveryPageSurvivesWhatItReadsGoingMissing(t *testing.T) {
	t.Parallel()

	pages := []struct {
		name   string
		drop   []string
		path   string
		expect string
	}{
		{
			name: "the waiting pairings", drop: []string{"pairings"},
			path: "/admin/pairings", expect: "waiting requests could not be read",
		},
		{
			name: "the paired machines", drop: []string{"devices"},
			path: "/admin/devices", expect: "paired machines could not be read",
		},
		{
			name: "the roster", drop: []string{"drivers"},
			path: "/admin/drivers", expect: "roster could not be read",
		},
		{
			name: "what is stored", drop: []string{"laps"},
			path: "/admin/data", expect: "could not be read",
		},
		{
			name: "the build history", drop: []string{"client_builds"},
			path: admin.ClientsPath, expect: "build history could not be read",
		},
		{
			name: "the signing certificate", drop: []string{"signing_certificate"},
			path: admin.ClientsPath, expect: "stored certificate could not be read",
		},
	}
	for _, tc := range pages {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			p := newClientPanel(t)

			// The page works before anything is taken away, so a test that
			// passes for the wrong reason cannot hide here.
			r.NotContains(p.get(tc.path), tc.expect)

			conn, err := pgx.Connect(context.Background(), p.url)
			r.NoError(err)
			defer func() { _ = conn.Close(context.Background()) }()
			for _, table := range tc.drop {
				_, err = conn.Exec(context.Background(), `DROP TABLE `+table+` CASCADE`)
				r.NoError(err, "the fixture could not drop %s", table)
			}

			body := p.get(tc.path)
			r.Contains(body, tc.expect, "the page said nothing about the part that is missing")
			r.Contains(body, "</html>", "the page did not finish rendering")
		})
	}
}

// The same again for the buttons rather than the pages.
//
// A write that cannot be made must say so. The failure an operator must never
// meet is the quiet one: a form that comes back looking saved, when the row it
// was meant to write is not there. Each case below presses a real button with
// the table it writes to taken away, and checks the page comes back saying
// nothing happened.
func TestEveryActionSaysSoWhenItCannotBeDone(t *testing.T) {
	t.Parallel()

	actions := []struct {
		name   string
		drop   []string
		do     func(p *clientPanel) int
		expect string
	}{
		{
			name: "saving the organisation", drop: []string{"settings"},
			do: func(p *clientPanel) int {
				return p.postWithToken("/admin/settings/identity", "/admin/settings", "organisation", "Spain GT")
			},
			expect: "nothing was changed",
		},
		{
			name: "saving the published limits", drop: []string{"settings"},
			do: func(p *clientPanel) int {
				return p.postWithToken("/admin/settings/limits", "/admin/settings", "trace_points", "300")
			},
			expect: "could not be read",
		},
		{
			name: "revoking every machine", drop: []string{"devices"},
			do: func(p *clientPanel) int {
				return p.postWithToken("/admin/settings/danger", "/admin/settings",
					"action", "revoke_devices", "confirm", "revoke")
			},
			expect: "Nothing was revoked",
		},
		{
			name: "saving how long traces are kept", drop: []string{"settings"},
			do: func(p *clientPanel) int {
				return p.postWithToken("/admin/data/retention", "/admin/data", "trace_months", "6")
			},
			expect: "could not be read",
		},
		{
			name: "building a client", drop: []string{"client_builds"},
			do: func(p *clientPanel) int {
				return p.postWithToken(admin.ClientsPath+"/build", admin.ClientsPath,
					"address", "https://pacenote.example.com", "signing", "unsigned")
			},
			expect: "could not write it into the build history",
		},
		{
			name: "removing the signing certificate", drop: []string{"signing_certificate"},
			do: func(p *clientPanel) int {
				return p.postWithToken(admin.CertificateRemovePath, admin.ClientsPath)
			},
			expect: "could not be removed",
		},
	}
	for _, tc := range actions {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			p := newClientPanel(t)

			conn, err := pgx.Connect(context.Background(), p.url)
			r.NoError(err)
			defer func() { _ = conn.Close(context.Background()) }()
			for _, table := range tc.drop {
				_, err = conn.Exec(context.Background(), `DROP TABLE `+table+` CASCADE`)
				r.NoError(err, "the fixture could not drop %s", table)
			}

			status := tc.do(p)
			r.GreaterOrEqual(status, 400, "an action that could not be done answered as if it had been")
			r.Contains(p.body(), tc.expect, "the page did not say what had not happened")
		})
	}
}

// The audit trail going missing stops nothing. It is a record of what was done,
// not a permission to do it — a server that refused to work because it could
// not write its own history would be a server broken by its own bookkeeping.
func TestTheAuditTrailGoingMissingStopsNothing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	cert := newCertPanel(t)
	p := cert.clientPanel

	conn, err := pgx.Connect(context.Background(), p.url)
	r.NoError(err)
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(context.Background(), `DROP TABLE audit_log CASCADE`)
	r.NoError(err)

	// A build, a certificate stored, and the same certificate removed: three
	// actions that each write a row nobody can write now.
	r.Equal(http.StatusOK, p.build("https://pacenote.example.com", clientbuild.SigningNone))
	r.Contains(p.body(), "Built")

	pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{})
	r.Equal(http.StatusOK, cert.upload(pfx, ""))
	r.Equal(http.StatusOK, cert.remove())
}

// postWithToken posts a form to path, taking the CSRF token from the page at
// from. The pairs are the form's own fields.
func (p *clientPanel) postWithToken(path, from string, pairs ...string) int {
	p.t.Helper()
	values := url.Values{"csrf": {csrfOf(p.t, p.get(from))}}
	for i := 0; i+1 < len(pairs); i += 2 {
		values.Set(pairs[i], pairs[i+1])
	}
	return p.post(path, values)
}

// Signing out, which is the one thing on this server that has to work when
// everything else is in doubt.
func TestSigningOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newClientPanel(t)

	r.Contains(p.get("/admin/overview"), "Overview")
	r.Equal(http.StatusSeeOther, p.postWithToken("/admin/logout", "/admin/overview"))

	// The session is gone from this browser and from the database, so a cookie
	// copied off the machine before now is no longer a way in.
	res := p.get("/admin/overview")
	r.Contains(res, "See Other", "a signed-out browser was still shown the panel")

	// Signing out is behind the same stale-form check as everything else. A
	// link somebody else can make that signs an operator out is a nuisance
	// rather than a breach, and it is still not something this server does.
	r.Equal(http.StatusForbidden, p.post("/admin/logout", url.Values{"csrf": {"anything"}}))
}

// More of the same, for the paths that need something other than a missing
// table: a directory that cannot be written, a file that has gone, a
// configuration file that will not take a new key.
func TestTheOtherWaysAnActionFails(t *testing.T) {
	t.Parallel()

	t.Run("signing in when there are no accounts to check against", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		conn, err := pgx.Connect(context.Background(), p.url)
		r.NoError(err)
		defer func() { _ = conn.Close(context.Background()) }()
		_, err = conn.Exec(context.Background(), `DROP TABLE admins CASCADE`)
		r.NoError(err)

		// A browser with no session of its own, so this is the sign-in form and
		// not a redirect.
		out := signedOut(t, p.panel)
		page := out.get("/admin/login")
		r.Equal(http.StatusInternalServerError, out.post("/admin/login", url.Values{
			"csrf": {csrfOf(t, page)}, "email": {panelEmail}, "password": {panelPassword},
		}))
		r.Contains(string(out.lastBody), "nobody can sign in right now")
	})

	t.Run("storing a certificate the database will not take", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		cert := newCertPanel(t)

		conn, err := pgx.Connect(context.Background(), cert.url)
		r.NoError(err)
		defer func() { _ = conn.Close(context.Background()) }()
		_, err = conn.Exec(context.Background(), `DROP TABLE signing_certificate CASCADE`)
		r.NoError(err)

		pfx, _ := clientbuildtest.Certificate(t, clientbuildtest.CertificateOptions{})
		r.Equal(http.StatusInternalServerError, cert.upload(pfx, ""))
		r.Contains(cert.body(), "could not be stored")
	})

	t.Run("building a client with nowhere to put it", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		// The builds directory exists and cannot be written into, which is
		// what a full disk or a wrong owner looks like from here.
		if os.Geteuid() == 0 {
			t.Skip("running as root, which can write into a directory it may not")
		}
		r.NoError(os.MkdirAll(p.dir, 0o700))
		r.NoError(os.Chmod(p.dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(p.dir, 0o700) })

		r.Equal(http.StatusInternalServerError,
			p.build("https://pacenote.example.com", clientbuild.SigningNone))
		r.Contains(p.body(), "That client was not built")
	})

	t.Run("downloading a build whose history is gone", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		r.Equal(http.StatusOK, p.build("https://pacenote.example.com", clientbuild.SigningNone))
		builds, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
		r.NoError(err)
		r.Len(builds, 1)

		conn, err := pgx.Connect(context.Background(), p.url)
		r.NoError(err)
		defer func() { _ = conn.Close(context.Background()) }()
		_, err = conn.Exec(context.Background(), `DROP TABLE client_builds CASCADE`)
		r.NoError(err)

		r.Contains(p.get(admin.ClientDownloadPath+builds[0].Reference), "Something went wrong")
	})

	t.Run("the file behind a build having been deleted", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		r.Equal(http.StatusOK, p.build("https://pacenote.example.com", clientbuild.SigningNone))
		builds, err := p.store.ClientBuilds(context.Background(), db.ClientBuildQuery{})
		r.NoError(err)
		r.NoError(os.RemoveAll(p.dir))

		body := p.get(admin.ClientDownloadPath + builds[0].Reference)
		r.Contains(body, "no longer on this server")
		r.Contains(p.get(admin.ClientsPath), "not on this server any more",
			"the history offered a link to a file that is gone")
	})

	t.Run("the build page when the settings will not read", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newClientPanel(t)

		conn, err := pgx.Connect(context.Background(), p.url)
		r.NoError(err)
		defer func() { _ = conn.Close(context.Background()) }()
		_, err = conn.Exec(context.Background(), `DROP TABLE settings CASCADE`)
		r.NoError(err)

		// The address field comes from the settings. It comes back empty rather
		// than taking the page with it, because an operator can type it.
		body := p.get(admin.ClientsPath)
		r.Contains(body, "Build client")
		r.Contains(body, `name="address"`)
	})
}

// A data key that cannot be written to the configuration file. Nothing has
// changed when that happens, which is the only safe direction: a process using
// a key its own file does not carry would lose every sealed credential at the
// next restart.
func TestADataKeyThatCannotBeWrittenChangesNothing(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	panel := newSettingsPanel(t, func(d *admin.Deps) {
		d.SaveDataKey = func(context.Context, auth.SecretKey) error {
			return errors.New("the configuration file is read-only")
		}
	})
	before := panel.keyring.Key()

	_, csrf := panel.settingsPage()
	r.Equal(http.StatusInternalServerError, panel.post("/admin/settings/danger", url.Values{
		"csrf": {csrf}, "action": {"regenerate_data_key"}, "confirm": {"regenerate"},
	}))
	r.Contains(string(panel.lastBody), "nothing has changed")
	r.Equal([]byte(before), []byte(panel.keyring.Key()),
		"the process moved to a key its configuration file does not carry")
}
