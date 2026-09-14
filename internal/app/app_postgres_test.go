//go:build postgres

package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/app"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

const (
	testEmail    = "ana@example.com"
	testPassword = "correct horse battery staple"
)

// setUpDatabase completes setup against dsn without going through the wizard,
// which is what every test below needs as a starting point.
func setUpDatabase(t *testing.T, dsn string) {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	store, err := db.Open(ctx, dsn, logging.Discard())
	r.NoError(err)
	defer store.Close()
	r.NoError(store.Migrate(ctx))

	hash, err := auth.HashPassword(testPassword)
	r.NoError(err)
	_, err = store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        testEmail,
		AdminPasswordHash: hash,
		Settings:          config.DefaultSettings("Iberian GT Championship", "localhost", config.TLSProxy),
		ServerVersion:     "v0.0.0-test",
	})
	r.NoError(err)
}

func TestARestartDoesNotReopenSetup(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dsn := dbtest.URL(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	setUpDatabase(t, dsn)
	writeConfig(t, dir, dsn)

	for i := range 2 {
		mode, addr, err := runUntilListening(t, baseOptions(t, dir))
		r.NoError(err)
		r.Equal(app.ModeNormal, mode, "start %d must come up serving, not asking", i+1)
		r.NotEmpty(addr)
	}
}

// This one cannot run in parallel: it sets the environment variable that a
// container deployment uses, which is the whole point of the case.
func TestADeletedDataDirectoryDoesNotReopenSetup(t *testing.T) {
	r := require.New(t)

	dsn := dbtest.URL(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	setUpDatabase(t, dsn)
	writeConfig(t, dir, dsn)

	// The container restarts on a volume that did not survive. The data
	// directory is gone; the database is not.
	r.NoError(os.RemoveAll(dir))
	_, err := os.Stat(dir)
	r.ErrorIs(err, os.ErrNotExist)

	t.Setenv(config.EnvDatabaseURL, dsn)

	mode, _, err := runUntilListening(t, baseOptions(t, dir))
	r.NoError(err)
	r.Equal(app.ModeNormal, mode,
		"the database holds an administrator, so the wizard must not reopen")
}

func TestAMigratedDatabaseWithNoMarkerRunsTheWizard(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Setup got as far as the migrations and then the process died before its
	// transaction committed. There is a schema and no administrator, so the
	// wizard must run.
	dsn := dbtest.URL(t)
	ctx := context.Background()
	store, err := db.Open(ctx, dsn, logging.Discard())
	r.NoError(err)
	r.NoError(store.Migrate(ctx))
	store.Close()

	dir := filepath.Join(t.TempDir(), "pacenote-data")
	writeConfig(t, dir, dsn)

	mode, _, err := runUntilListening(t, baseOptions(t, dir))
	r.NoError(err)
	r.Equal(app.ModeSetup, mode)
}

// TestSetupThenAdminLogin is the whole promise, end to end: a fresh directory,
// the wizard, the switch to normal mode with no restart, and a sign-in.
func TestSetupThenAdminLogin(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dsn := dbtest.URL(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var terminal strings.Builder
	modes := make(chan app.Mode, 4)
	addrs := make(chan string, 4)
	opts := baseOptions(t, dir)
	opts.Terminal = &terminal
	opts.Log = logging.Discard()
	opts.OnListen = func(m app.Mode, a string) {
		modes <- m
		addrs <- a
	}

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, opts) }()

	r.Equal(app.ModeSetup, <-modes)
	base := "http://" + <-addrs

	token := tokenFromTerminal(t, terminal.String())
	r.NotEmpty(token)

	client := newClient(t)
	page := post(t, client, base+"/setup", url.Values{"token": {token}})
	r.Equal(http.StatusSeeOther, page.status)

	page = get(t, client, base+"/setup/database")
	page = post(t, client, base+"/setup/database", url.Values{
		"csrf": {csrfFrom(t, page.body)}, "database_url": {dsn},
	})
	r.Equal(http.StatusSeeOther, page.status, page.body)

	page = get(t, client, base+"/setup/organisation")
	page = post(t, client, base+"/setup/organisation", url.Values{
		"csrf": {csrfFrom(t, page.body)}, "organisation": {"Iberian GT Championship"},
	})
	r.Equal(http.StatusSeeOther, page.status, page.body)

	page = get(t, client, base+"/setup/account")
	page = post(t, client, base+"/setup/account", url.Values{
		"csrf": {csrfFrom(t, page.body)}, "email": {testEmail},
		"password": {testPassword}, "password2": {testPassword},
	})
	r.Equal(http.StatusSeeOther, page.status, page.body)

	page = get(t, client, base+"/setup/address")
	page = post(t, client, base+"/setup/address", url.Values{
		"csrf": {csrfFrom(t, page.body)}, "host": {"localhost"}, "tls_mode": {"proxy"},
	})
	r.Equal(http.StatusSeeOther, page.status, page.body)

	page = get(t, client, base+"/setup/finish")
	page = post(t, client, base+"/setup/finish", url.Values{"csrf": {csrfFrom(t, page.body)}})
	r.Equal(http.StatusOK, page.status, page.body)
	r.Contains(page.body, "Setup is done")

	// The server switches without a restart, and comes back on the same port
	// because nothing about the address changed.
	r.Equal(app.ModeNormal, <-modes)
	base = "http://" + <-addrs

	// Signing in works, with the account the wizard created.
	admin := newClient(t)
	login := get(t, admin, base+"/admin/login")
	r.Equal(http.StatusOK, login.status)
	r.Contains(login.body, "Sign in")
	r.Contains(login.body, "Iberian GT Championship")

	res := post(t, admin, base+"/admin/login", url.Values{
		"csrf": {csrfFrom(t, login.body)}, "email": {testEmail}, "password": {testPassword},
	})
	r.Equal(http.StatusSeeOther, res.status, res.body)
	r.Equal("/admin/overview", res.location)

	overview := get(t, admin, base+"/admin/overview")
	r.Equal(http.StatusOK, overview.status)
	r.Contains(overview.body, "Iberian GT Championship")
	r.Contains(overview.body, "v0.0.0-test")
	r.Contains(overview.body, "up to date")
	r.Contains(overview.body, "Uptime")
	r.Contains(overview.body, testEmail)

	// A wrong password is refused, and says nothing about which half was wrong.
	wrong := newClient(t)
	form := get(t, wrong, base+"/admin/login")
	bad := post(t, wrong, base+"/admin/login", url.Values{
		"csrf": {csrfFrom(t, form.body)}, "email": {testEmail}, "password": {"not the password"},
	})
	r.Equal(http.StatusUnauthorized, bad.status)
	r.Contains(bad.body, "do not match an account here")

	// Signing out ends the session for good.
	out := post(t, admin, base+"/admin/logout", url.Values{"csrf": {csrfFrom(t, overview.body)}})
	r.Equal(http.StatusSeeOther, out.status)
	after := get(t, admin, base+"/admin/overview")
	r.Equal(http.StatusSeeOther, after.status)
	r.Equal("/admin/login", after.location)

	// The setup wizard is gone from this server entirely.
	gone := get(t, admin, base+"/setup")
	r.Equal(http.StatusNotFound, gone.status)

	cancel()
	select {
	case err := <-done:
		r.NoError(err)
	case <-time.After(30 * time.Second):
		r.Fail("the server did not stop")
	}
}

// --------------------------------------------------------------------------
// A very small browser
// --------------------------------------------------------------------------

type page struct {
	status   int
	location string
	body     string
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{
		Jar:           jar,
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(t *testing.T, c *http.Client, u string) page {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, http.NoBody)
	require.NoError(t, err)
	return do(t, c, req)
}

func post(t *testing.T, c *http.Client, u string, form url.Values) page {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(t, c, req)
}

func do(t *testing.T, c *http.Client, req *http.Request) page {
	t.Helper()
	res, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return page{status: res.StatusCode, location: res.Header.Get("Location"), body: string(body)}
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "no CSRF token on the page:\n%s", body)
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	require.GreaterOrEqual(t, j, 0)
	return rest[:j]
}

// tokenFromTerminal reads the setup token off the banner, the same way an
// operator does.
func tokenFromTerminal(t *testing.T, printed string) string {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		if after, ok := strings.CutPrefix(line, "Token "); ok {
			return strings.TrimSpace(strings.Split(strings.TrimSpace(after), " ")[0])
		}
	}
	require.Fail(t, "the terminal carried no setup token", printed)
	return ""
}

// The bare address of a running server sends a person to the panel.
//
// It is the address an operator types from memory — the host and nothing else —
// and it has to lead somewhere. A 404 there is a server that looks broken to
// the one person who can fix it.
func TestTheBareAddressLeadsToThePanel(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A server that is past the wizard, which is the only one that has a panel
	// to be sent to.
	dsn := dbtest.URL(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	setUpDatabase(t, dsn)
	writeConfig(t, dir, dsn)

	stop, addr := runInBackground(t, baseOptions(t, dir))
	defer stop()

	res, err := (&http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Get("http://" + addr + "/")
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()

	r.Equal(http.StatusSeeOther, res.StatusCode)
	r.Contains(res.Header.Get("Location"), "/admin")
}

// runInBackground starts a server and returns a stop function and the address
// it bound, for the tests that want to make a request of it rather than only
// watch it start.
//
// It lives in this file rather than beside [runUntilListening] because the
// tests that need a server still running are the ones that need a database,
// and a helper that only the tagged build uses belongs in the tagged build.
func runInBackground(t *testing.T, opts app.Options) (stop func(), addr string) {
	t.Helper()
	r := require.New(t)

	ctx, cancel := context.WithCancel(context.Background())
	listening := make(chan struct{})
	var once sync.Once
	opts.Log = logging.Discard()
	opts.OnListen = func(_ app.Mode, a string) {
		// The first address is the public one, which is the one a test wants.
		// Writing it inside the Once and reading it only after the close is
		// what makes it safe to return.
		once.Do(func() {
			addr = a
			close(listening)
		})
	}

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, opts) }()

	select {
	case err := <-done:
		cancel()
		r.NoError(err, "the server stopped before it was listening")
		r.FailNow("the server stopped before it was listening")
	case <-listening:
	}

	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			r.FailNow("the server did not stop")
		}
	}, addr
}
