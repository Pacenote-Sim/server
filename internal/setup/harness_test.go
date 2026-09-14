package setup_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/logging"
	"github.com/pacenote-sim/server/internal/setup"
)

// token is the setup token every test in this package uses. A fixed value is
// fine here: the randomness of a real token is pinned in internal/auth, and a
// fixed one makes a failing assertion readable.
const token = "7QK4-M2XF-8DNA-0123-4567-89AB-CDEF-GHJK"

// harness is one wizard behind an httptest server, with a browser in front of
// it that keeps cookies the way a real one would.
type harness struct {
	t       *testing.T
	wizard  *setup.Wizard
	server  *httptest.Server
	client  *http.Client
	done    chan struct{}
	dataDir string
	walkURL string
}

type harnessOptions struct {
	databaseURL string
	now         func() time.Time
	attempts    *[]string
	// probe stands in for a real database. nil leaves the wizard testing
	// connection strings for real, which is what the diagnosis tests want.
	probe        func(ctx context.Context, url string) error
	alreadySetUp func(ctx context.Context, url string) (bool, error)
	// walkURL is the connection string walkTo fills the first step in with.
	// The tests that have no database use one that is never dialled.
	walkURL string
}

// acceptAnyDatabase lets a test walk past the first step without a database.
// The steps after it are forms and validation, and they are worth testing on a
// machine that has no PostgreSQL on it.
func acceptAnyDatabase(context.Context, string) error { return nil }

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	r := require.New(t)

	dataDir := t.TempDir() + "/pacenote-data"
	done := make(chan struct{}, 1)

	// A stubbed probe means a test that has no database, so the database
	// cannot be asked whether it has been set up either.
	if opts.probe != nil && opts.alreadySetUp == nil {
		opts.alreadySetUp = func(context.Context, string) (bool, error) { return false, nil }
	}

	wz, err := setup.New(setup.Deps{
		Log:     logging.Discard(),
		Version: "v0.0.0-test",
		Token:   token,
		DataDir: dataDir,
		Base: config.Config{
			DatabaseURL:   opts.databaseURL,
			Listen:        ":8080",
			MetricsListen: "127.0.0.1:9090",
		},
		Done:         done,
		Now:          opts.now,
		Probe:        opts.probe,
		AlreadySetUp: opts.alreadySetUp,
		OnAttempt: func(outcome string) {
			if opts.attempts != nil {
				*opts.attempts = append(*opts.attempts, outcome)
			}
		},
	})
	r.NoError(err)

	srv := httptest.NewServer(wz.Handler())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	r.NoError(err)

	walkURL := opts.walkURL
	if walkURL == "" {
		walkURL = "postgres://pacenote:secret@db.example.com:5432/pacenote"
	}

	h := &harness{
		t:       t,
		wizard:  wz,
		server:  srv,
		done:    done,
		dataDir: dataDir,
		walkURL: walkURL,
		client: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			// Redirects are followed by hand so a test can assert on the
			// intermediate answer as well as on the page it lands on.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return h
}

// response is what a request came back with, read into memory so a test can
// assert on it without worrying about closing anything.
type response struct {
	status   int
	location string
	body     string
}

func (h *harness) get(path string) response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.server.URL+path, http.NoBody)
	require.NoError(h.t, err)
	return h.do(req)
}

func (h *harness) post(path string, form url.Values) response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		h.server.URL+path, strings.NewReader(form.Encode()))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return h.do(req)
}

func (h *harness) do(req *http.Request) response {
	h.t.Helper()
	res, err := h.client.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(h.t, err)
	return response{status: res.StatusCode, location: res.Header.Get("Location"), body: string(body)}
}

// follow walks a redirect chain to the page it ends on, which is what a browser
// does and what most assertions want.
func (h *harness) follow(res response) response {
	h.t.Helper()
	for range 5 {
		if res.location == "" {
			return res
		}
		res = h.get(res.location)
	}
	require.Fail(h.t, "too many redirects")
	return res
}

// csrf pulls the token out of the form on the page the wizard is currently
// showing, the same way a browser would submit it.
func (h *harness) csrf(body string) string {
	h.t.Helper()
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	require.GreaterOrEqual(h.t, i, 0, "the page carries no CSRF token:\n%s", body)
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	require.GreaterOrEqual(h.t, j, 0)
	return rest[:j]
}

// unlock exchanges the token for a session, which is the first thing every
// test past the token gate needs.
func (h *harness) unlock() response {
	h.t.Helper()
	res := h.post("/setup", url.Values{"token": {token}})
	require.Equal(h.t, http.StatusSeeOther, res.status, "the right token should open the wizard")
	return h.follow(res)
}

// submit posts a form to path, carrying the CSRF token from the page currently
// on screen.
func (h *harness) submit(path string, page response, values url.Values) response {
	h.t.Helper()
	values.Set("csrf", h.csrf(page.body))
	return h.post(path, values)
}

// browser is a second visitor to the same server, with its own cookie jar.
func (h *harness) browser() *harness {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(h.t, err)
	out := *h
	out.client = &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &out
}

// The wizard's steps, as a test walks them.
const (
	stepDatabase = iota
	stepOrganisation
	stepAccount
	stepAddress
	stepFinish
)

// The values a test fills the wizard in with when it is not the thing under
// test.
const (
	testOrganisation = "Iberian GT Championship"
	testEmail        = "ana@example.com"
	testPassword     = "correct horse battery staple"
	testHost         = "pacenote.example.com"
)

// walkTo fills the wizard in as far as target and returns the page it lands
// on. The token must already have been exchanged.
func (h *harness) walkTo(target int) response {
	h.t.Helper()
	page := h.follow(h.get("/setup/database"))
	if target == stepDatabase {
		return page
	}
	page = h.follow(h.submit("/setup/database", page,
		url.Values{"database_url": {h.walkURL}}))
	if target == stepOrganisation {
		return page
	}
	page = h.follow(h.submit("/setup/organisation", page,
		url.Values{"organisation": {testOrganisation}}))
	if target == stepAccount {
		return page
	}
	page = h.follow(h.submit("/setup/account", page, url.Values{
		"email": {testEmail}, "password": {testPassword}, "password2": {testPassword},
	}))
	if target == stepAddress {
		return page
	}
	page = h.follow(h.submit("/setup/address", page, url.Values{
		"host": {testHost}, "tls_mode": {"proxy"},
	}))
	return page
}

// switched reports whether the wizard has told the server to change modes.
func (h *harness) switched() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}
