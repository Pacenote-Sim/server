//go:build postgres

package setup_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

func store(t *testing.T, dsn string) *db.Store {
	t.Helper()
	s, err := db.Open(context.Background(), dsn, logging.Discard())
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func TestSetupCompletes(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dsn := dbtest.URL(t)
	h := newHarness(t, harnessOptions{walkURL: dsn})

	h.unlock()
	page := h.walkTo(stepFinish)
	r.Contains(page.body, testOrganisation)
	r.Contains(page.body, testEmail)
	r.Contains(page.body, "https://"+testHost)
	r.NotContains(page.body, "secret", "the confirmation page shows the database without its password")

	// Nothing to fill in on the last step: it confirms and commits. The wizard
	// used to ask for an Anthropic key here, which was asking an operator to
	// make a vendor decision before they had seen the thing it was for. The
	// coaching plugin asks for it on its own page, once it is installed.
	done := h.submit("/setup/finish", page, url.Values{})
	r.Equal(http.StatusOK, done.status)
	r.Contains(done.body, "Setup is done")
	r.Contains(done.body, "/admin")
	r.True(h.switched(), "the server is told to switch to normal mode")

	s := store(t, dsn)
	ctx := context.Background()

	state, err := s.SetupState(ctx)
	r.NoError(err)
	r.True(state.Complete)
	r.Equal(testEmail, state.CompletedBy)

	admin, err := s.AdminByEmail(ctx, testEmail)
	r.NoError(err)
	ok, err := auth.VerifyPassword(admin.PasswordHash, testPassword)
	r.NoError(err)
	r.True(ok, "the administrator can sign in with the password they chose")

	settings, err := s.Settings(ctx)
	r.NoError(err)
	r.Equal(testOrganisation, settings.Organisation)
	r.Equal(testHost, settings.PublicHost)
	r.Equal(config.TLSProxy, settings.TLSMode)

	// The configuration file is written, with the connection string and a
	// fresh data key, and nobody but the operator can read it.
	raw, err := os.ReadFile(filepath.Join(h.dataDir, config.FileName))
	r.NoError(err)
	var cfg config.Config
	r.NoError(json.Unmarshal(raw, &cfg))
	r.Equal(dsn, cfg.DatabaseURL)
	r.NotEmpty(cfg.SecretKey)

	info, err := os.Stat(filepath.Join(h.dataDir, config.FileName))
	r.NoError(err)
	r.Equal(config.FileMode, info.Mode().Perm())

	// A fresh installation stores no vendor credential at all — there is no
	// field left to put one in. The data key is there and ready for the first
	// plugin that needs one.
	key, err := auth.ParseSecretKey(cfg.SecretKey)
	r.NoError(err)
	r.NotNil(key)
}

func TestSetupModeRefusesToRunAgain(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dsn := dbtest.URL(t)

	// One server completes setup.
	first := newHarness(t, harnessOptions{walkURL: dsn})
	first.unlock()
	page := first.walkTo(stepFinish)
	r.Equal(http.StatusOK, first.submit("/setup/finish", page, url.Values{}).status)

	// A second server, started fresh with no data directory, is pointed at the
	// same database. The wizard must refuse it: the database holds an
	// administrator, and the filesystem does not get a say.
	second := newHarness(t, harnessOptions{walkURL: dsn})
	next := second.unlock()
	res := second.submit("/setup/database", next, url.Values{"database_url": {dsn}})
	r.Equal(http.StatusConflict, res.status)
	r.Contains(res.body, "already been set up")
	r.Contains(res.body, config.EnvDatabaseURL)

	s := store(t, dsn)
	n, err := s.CountAdmins(context.Background())
	r.NoError(err)
	r.Equal(int64(1), n)
}

func TestTokenIsRejectedAfterCompletion(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dsn := dbtest.URL(t)
	h := newHarness(t, harnessOptions{walkURL: dsn})

	h.unlock()
	page := h.walkTo(stepFinish)
	r.Equal(http.StatusOK, h.submit("/setup/finish", page, url.Values{}).status)

	// The token was spent when it opened the wizard, and setup is finished.
	// Both are reasons to refuse; either is enough.
	res := h.browser().post("/setup", url.Values{"token": {token}})
	r.Equal(http.StatusForbidden, res.status)
	r.Contains(res.body, "has been used")
}

func TestConcurrentFinishMakesOneAdministrator(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dsn := dbtest.URL(t)
	h := newHarness(t, harnessOptions{walkURL: dsn})

	h.unlock()
	page := h.walkTo(stepFinish)
	csrf := h.csrf(page.body)

	// Two identical submissions at once, which is what a double click on a
	// slow connection produces.
	const attempts = 4
	var wg sync.WaitGroup
	results := make([]response, attempts)
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = h.post("/setup/finish", url.Values{"csrf": {csrf}})
		}()
	}
	close(start)
	wg.Wait()

	completed, refused := 0, 0
	for _, res := range results {
		switch res.status {
		case http.StatusOK:
			completed++
		case http.StatusConflict:
			refused++
			r.Contains(res.body, "already been completed")
		default:
			r.Failf("unexpected answer", "status %d: %s", res.status, res.body)
		}
	}
	r.Equal(1, completed, "exactly one submission finishes setup")
	r.Equal(attempts-1, refused)

	s := store(t, dsn)
	n, err := s.CountAdmins(context.Background())
	r.NoError(err)
	r.Equal(int64(1), n, "a concurrent double submit produces exactly one administrator")
}

func TestDatabaseStepDiagnosesRealFailures(t *testing.T) {
	t.Parallel()
	admin := dbtest.AdminURL(t)

	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "a database that does not exist",
			dsn:  replaceDatabase(admin, "no_such_database_at_all"),
			want: "no database called",
		},
		{
			name: "wrong credentials",
			dsn:  withUser(admin, "no-such-user", "definitely-not-the-password"),
			want: "refused the user name or password",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{})
			page := h.unlock()
			res := h.submit("/setup/database", page, url.Values{"database_url": {tc.dsn}})
			r.Equal(http.StatusUnprocessableEntity, res.status)
			r.Contains(res.body, tc.want)
		})
	}
}

func replaceDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

func withUser(dsn, user, password string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// Finishing with the server holding its own certificate.
//
// It is the one path through the wizard where the address an operator is using
// right now stops working the moment they press the button: the server was on
// some port for setup and moves to 443. The last page has to say so, and has to
// give them the address that will work — otherwise the first thing a new
// operator meets is their own server appearing to have died.
func TestFinishingWithAnAutomaticCertificate(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dsn := dbtest.URL(t)
	h := newHarness(t, harnessOptions{walkURL: dsn})

	h.unlock()
	page := h.follow(h.get("/setup/database"))
	page = h.follow(h.submit("/setup/database", page, url.Values{"database_url": {h.walkURL}}))
	page = h.follow(h.submit("/setup/organisation", page, url.Values{"organisation": {testOrganisation}}))
	page = h.follow(h.submit("/setup/account", page, url.Values{
		"email": {testEmail}, "password": {testPassword}, "password2": {testPassword},
	}))
	page = h.follow(h.submit("/setup/address", page, url.Values{
		"host": {testHost}, "tls_mode": {"auto"},
	}))
	r.Contains(page.body, "gets its own certificate")

	done := h.submit("/setup/finish", page, url.Values{})
	r.Equal(http.StatusOK, done.status)
	r.Contains(done.body, "Setup is done")
	r.Contains(done.body, "https://"+testHost+"/admin",
		"the operator was not given the address that will work")
	r.Contains(done.body, "443", "nothing said the ports had changed")

	settings, err := store(t, dsn).Settings(context.Background())
	r.NoError(err)
	r.Equal(config.TLSAuto, settings.TLSMode)

	// And the last page is still there on a reload, because an operator reads
	// it, closes the tab, and comes back for the address.
	again := h.get("/setup/done")
	r.Equal(http.StatusOK, again.status)
	r.Contains(again.body, "Setup is done")
}
