//go:build postgres

package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

const (
	panelEmail    = "ana@example.com"
	panelPassword = "correct horse battery staple"
)

// panel is the admin panel behind an httptest server, signed in, with a real
// database of its own.
type panel struct {
	t     *testing.T
	store *db.Store
	// url is the database this panel is talking to, for the tests that take
	// something out from under a page to see what it says.
	url    string
	server *httptest.Server
	client *http.Client
	// object is the panel itself, for the one piece of its state that outlives
	// a request: the retention prune.
	object *admin.Panel

	// lastBody is what the last form post rendered, which is where the panel
	// puts the answer to a decision.
	lastBody []byte
}

// newPanel builds the panel over a database of its own. The options are how a
// test adds a dependency only its own page needs — the build page's prebuilt
// client, for instance — without every other test carrying one.
func newPanel(t *testing.T, opts ...func(*admin.Deps)) *panel {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	dsn := dbtest.URL(t)
	store, err := db.Open(ctx, dsn, logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	hash, err := auth.HashPassword(panelPassword)
	r.NoError(err)
	settings := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy)
	_, err = store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail: panelEmail, AdminPasswordHash: hash, Settings: settings,
	})
	r.NoError(err)

	deps := admin.Deps{
		Log:       logging.Discard(),
		Store:     store,
		Version:   "v0.0.0-test",
		StartedAt: time.Now(),
		// Live settings rather than a captured value: the data page saves a
		// retention and then reads it back, which only works if the panel is
		// looking at the database the save went to.
		Settings: store.Settings,
	}
	for _, opt := range opts {
		opt(&deps)
	}
	p, err := admin.New(deps)
	r.NoError(err)

	// The API is mounted beside the panel because two of the things these
	// tests assert are only true across both: that revoking a machine in the
	// panel refuses that machine's very next API request, and that it refuses
	// nobody else's.
	v1, err := api.New(ctx, api.Deps{
		Log:   logging.Discard(),
		Store: store,
	})
	r.NoError(err)

	mux := http.NewServeMux()
	v1.Routes(mux)
	p.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	r.NoError(err)

	out := &panel{
		t: t, store: store, url: dsn, server: srv, object: p,
		client: &http.Client{
			Jar:           jar,
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	out.signIn()
	return out
}

func (p *panel) signIn() {
	p.t.Helper()
	r := require.New(p.t)
	page := p.get("/admin/login")
	res := p.post("/admin/login", url.Values{
		"csrf": {csrfOf(p.t, page)}, "email": {panelEmail}, "password": {panelPassword},
	})
	r.Equal(http.StatusSeeOther, res, "the admin account made during setup should sign in")
}

func (p *panel) get(path string) string {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.server.URL+path, http.NoBody)
	r.NoError(err)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	r.NoError(err)
	requireWholePage(p.t, string(body))
	return string(body)
}

func (p *panel) post(path string, form url.Values) int {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.server.URL+path, strings.NewReader(form.Encode()))
	r.NoError(err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	p.lastBody, _ = io.ReadAll(res.Body)
	return res.StatusCode
}

func csrfOf(tb testing.TB, body string) string {
	tb.Helper()
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	require.GreaterOrEqual(tb, i, 0, "the page carries no CSRF token:\n%s", body)
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	require.GreaterOrEqual(tb, j, 0)
	return rest[:j]
}

// openPairing puts one grant in front of the operator and returns the code the
// driver would be reading out.
func (p *panel) openPairing() (deviceCode, userCode string) {
	p.t.Helper()
	r := require.New(p.t)
	codes, err := auth.NewPairingCodes()
	r.NoError(err)
	_, err = p.store.CreatePairing(context.Background(), codes.DeviceCodeSum, codes.UserCode,
		time.Now().Add(db.PairingTTL))
	r.NoError(err)
	return codes.DeviceCode, codes.UserCode
}

func TestPairingPage(t *testing.T) {
	t.Parallel()

	t.Run("an empty list says what to expect", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		body := p.get("/admin/pairings")
		r.Contains(body, "Nothing is waiting")
		r.Contains(body, "Pairing")
	})

	t.Run("a waiting request shows the code the driver reads out", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		_, userCode := p.openPairing()

		body := p.get("/admin/pairings")
		r.Contains(body, userCode, "an operator matches this against what the driver says")
		r.Contains(body, "Approve")
		r.Contains(body, "Deny")
	})

	t.Run("approving names a driver and lets the machine in", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		p := newPanel(t)
		deviceCode, userCode := p.openPairing()

		page := p.get("/admin/pairings")
		pending, err := p.store.ListPendingPairings(ctx)
		r.NoError(err)
		r.Len(pending, 1)

		status := p.post("/admin/pairings", url.Values{
			"csrf":        {csrfOf(t, page)},
			"id":          {itoa(pending[0].ID)},
			"decision":    {"approve"},
			"driver_name": {"Ana Ruiz"},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Approved")
		r.NotContains(string(p.lastBody), userCode, "a decided request leaves the waiting list")

		grant, err := p.store.PairingByDeviceCode(ctx, auth.HashToken(deviceCode))
		r.NoError(err)
		r.Equal(wire.StatusApproved, grant.Status)
		r.NotNil(grant.DriverID)

		driver, err := p.store.DriverByID(ctx, *grant.DriverID)
		r.NoError(err)
		r.Equal("Ana Ruiz", driver.Name)
		r.Equal("ana-ruiz", driver.Slug)
	})

	t.Run("approving with no driver changes nothing and says why", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		p := newPanel(t)
		p.openPairing()

		page := p.get("/admin/pairings")
		pending, err := p.store.ListPendingPairings(ctx)
		r.NoError(err)

		status := p.post("/admin/pairings", url.Values{
			"csrf": {csrfOf(t, page)}, "id": {itoa(pending[0].ID)}, "decision": {"approve"},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "An approval needs a driver")

		still, err := p.store.ListPendingPairings(ctx)
		r.NoError(err)
		r.Len(still, 1, "nothing was decided")
	})

	t.Run("denying is final", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		p := newPanel(t)
		deviceCode, _ := p.openPairing()

		page := p.get("/admin/pairings")
		pending, err := p.store.ListPendingPairings(ctx)
		r.NoError(err)

		status := p.post("/admin/pairings", url.Values{
			"csrf": {csrfOf(t, page)}, "id": {itoa(pending[0].ID)}, "decision": {"deny"},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Denied")

		grant, err := p.store.PairingByDeviceCode(ctx, auth.HashToken(deviceCode))
		r.NoError(err)
		r.Equal(wire.StatusDenied, grant.Status)
	})

	t.Run("deciding the same request twice changes nothing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		p := newPanel(t)
		p.openPairing()

		page := p.get("/admin/pairings")
		pending, err := p.store.ListPendingPairings(ctx)
		r.NoError(err)
		form := url.Values{
			"csrf": {csrfOf(t, page)}, "id": {itoa(pending[0].ID)},
			"decision": {"approve"}, "driver_name": {"Ana Ruiz"},
		}
		r.Equal(http.StatusOK, p.post("/admin/pairings", form))
		r.Equal(http.StatusOK, p.post("/admin/pairings", form))
		r.Contains(string(p.lastBody), "already been decided")
	})

	t.Run("a second machine for a name already here joins that driver", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		p := newPanel(t)

		for range 2 {
			p.openPairing()
			page := p.get("/admin/pairings")
			pending, err := p.store.ListPendingPairings(ctx)
			r.NoError(err)
			r.Len(pending, 1)
			r.Equal(http.StatusOK, p.post("/admin/pairings", url.Values{
				"csrf": {csrfOf(t, page)}, "id": {itoa(pending[0].ID)},
				"decision": {"approve"}, "driver_name": {"Ana Ruiz"},
			}))
		}

		drivers, err := p.store.ListDrivers(ctx)
		r.NoError(err)
		r.Len(drivers, 1, "typing the same name twice is one person with two machines")
	})

	t.Run("the page needs a session and its form needs a token", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		p.client.Jar, _ = cookiejar.New(nil)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			p.server.URL+"/admin/pairings", http.NoBody)
		r.NoError(err)
		res, err := p.client.Do(req)
		r.NoError(err)
		defer func() { _ = res.Body.Close() }()
		r.Equal(http.StatusSeeOther, res.StatusCode)
		r.Equal("/admin/login", res.Header.Get("Location"))
	})
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// The pairing form, given things an operator's browser would not send.
//
// It is the one page where a wrong click grants a machine a token, so every
// value on it is checked rather than trusted, and each refusal says what to do:
// load the page again, because what it is describing has moved on.
func TestThePairingFormRefusesWhatItCannotTrust(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	page := p.get("/admin/pairings")
	csrf := csrfOf(t, page)

	// A stale form.
	r.Equal(http.StatusForbidden, p.post("/admin/pairings", url.Values{
		"csrf": {"not-the-token"}, "id": {"1"}, "decision": {"deny"},
	}))

	// A body that is not a form at all.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.server.URL+"/admin/pairings", strings.NewReader("%zz&x=1"))
	r.NoError(err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.client.Do(req)
	r.NoError(err)
	_ = res.Body.Close()
	r.Equal(http.StatusBadRequest, res.StatusCode)

	// A request identifier that is not one.
	r.Equal(http.StatusOK, p.post("/admin/pairings", url.Values{
		"csrf": {csrf}, "id": {"the third one"}, "decision": {"deny"},
	}))
	r.Contains(string(p.lastBody), "no longer on this page")

	// A driver identifier that is not one.
	page = p.get("/admin/pairings")
	r.Equal(http.StatusOK, p.post("/admin/pairings", url.Values{
		"csrf": {csrfOf(t, page)}, "id": {"1"}, "driver_id": {"whoever"},
	}))
	r.Contains(string(p.lastBody), "That driver is no longer on this page")

	// A name with nothing in it a web address could carry. The driver's slug
	// comes out of the name, and a driver with no slug has no page.
	page = p.get("/admin/pairings")
	r.Equal(http.StatusOK, p.post("/admin/pairings", url.Values{
		"csrf": {csrfOf(t, page)}, "id": {"1"}, "driver_name": {"!!!"},
	}))
	r.Contains(string(p.lastBody), "could not be created")
}

// Approving onto a driver who is already on the roster, which is the second
// machine for somebody who already races here.
func TestApprovingOntoADriverWhoIsAlreadyHere(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)
	ctx := context.Background()

	driver := p.seedDriver("Marta Ferrer", "gt3")
	_, code := p.openPairing()
	waiting, err := p.store.ListPendingPairings(ctx)
	r.NoError(err)
	r.Len(waiting, 1)

	page := p.get("/admin/pairings")
	r.Equal(http.StatusOK, p.post("/admin/pairings", url.Values{
		"csrf":      {csrfOf(t, page)},
		"id":        {strconv.FormatInt(waiting[0].ID, 10)},
		"driver_id": {strconv.FormatInt(driver.ID, 10)},
	}))
	r.Contains(string(p.lastBody), "Approved")
	r.NotContains(string(p.lastBody), code, "a decided request stayed on the waiting list")

	// No second driver was made with the same name.
	drivers, err := p.store.ListDrivers(ctx)
	r.NoError(err)
	r.Len(drivers, 1)
	r.Equal(driver.ID, drivers[0].ID)
}
