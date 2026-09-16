//go:build postgres

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

// The organisation every test in this package is set up as. A fixed name keeps
// the golden fixtures stable and a failing assertion readable.
const (
	testOrganisation = "Iberian GT Championship"
	testHost         = "pacenote.example.com"
	testAdmin        = "ana@example.com"
	testDriverName   = "Ana Ruiz"
)

// harness is one API behind an httptest server, with a real database of its
// own and a paired device in hand.
type harness struct {
	t      testing.TB
	store  *db.Store
	url    string
	api    *api.API
	server *httptest.Server
	client *http.Client
	token  string
	driver db.Driver
	device db.Device
}

type harnessOptions struct {
	// features overrides what this installation has, which is how the two
	// enterprise endpoints are tested as present as well as as absent.
	features func() []wire.Feature
	// settings is applied over the defaults before the API is built.
	settings func(*config.Settings)
	// onRequest receives the route pattern, status and duration of every
	// finished call, for the test that asserts the metric is labelled by
	// pattern rather than by path.
	onRequest func(route string, status int, took time.Duration)
	// skipPairing leaves the harness with no token, for the tests that pair
	// through the real flow themselves.
	skipPairing bool
	// plugins receives the events the API publishes, for the tests that assert
	// what a plugin is actually handed. A nil sink is a server built without
	// plugins, which is what every other test here runs as.
	plugins api.EventSink
}

func newHarness(tb testing.TB, opts harnessOptions) *harness {
	tb.Helper()
	r := require.New(tb)
	ctx := context.Background()

	url := dbtest.URL(tb)
	store, err := db.Open(ctx, url, logging.Discard())
	r.NoError(err)
	tb.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	settings := config.DefaultSettings(testOrganisation, testHost, config.TLSProxy)
	if opts.settings != nil {
		opts.settings(&settings)
	}
	hash, err := auth.HashPassword("correct horse battery staple")
	r.NoError(err)
	_, err = store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        testAdmin,
		AdminPasswordHash: hash,
		Settings:          settings,
	})
	r.NoError(err)

	h := &harness{t: tb, store: store, url: url}

	// The clock is the real one. The pairing table's expiry is compared against
	// the database's now(), so a server clock that disagreed with it would make
	// every grant either already expired or never expiring; nothing in a
	// response body carries a server timestamp, so the fixtures are stable
	// without one.
	v1, err := api.New(ctx, api.Deps{
		Log:       logging.Discard(),
		Store:     store,
		Features:  opts.features,
		OnRequest: opts.onRequest,
		Plugins:   opts.plugins,
	})
	r.NoError(err)
	h.api = v1

	mux := http.NewServeMux()
	v1.Routes(mux)
	srv := httptest.NewServer(mux)
	tb.Cleanup(srv.Close)

	h.server = srv
	h.client = &http.Client{Timeout: 30 * time.Second}
	if !opts.skipPairing {
		h.token, h.driver, h.device = h.pair(testDriverName)
	}
	return h
}

// column reads one value straight out of the schema, for the assertions that
// are about what was stored rather than about what an endpoint answered. It
// opens a connection of its own rather than reaching inside the store, because
// a raw query is a thing tests want and production does not.
func (h *harness) column(sql string, args ...any) string {
	h.t.Helper()
	r := require.New(h.t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, h.url)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	var out string
	r.NoError(conn.QueryRow(ctx, sql, args...).Scan(&out))
	return out
}

// pair runs the real device-code flow end to end: the client asks, the operator
// approves in the panel's place, and the client polls again and gets a token.
func (h *harness) pair(name string) (string, db.Driver, db.Device) {
	h.t.Helper()
	r := require.New(h.t)
	ctx := context.Background()

	var start wire.PairStart
	res := h.do(request{method: http.MethodPost, path: "/api/v1/pair/start"})
	r.Equal(http.StatusOK, res.status, res.body)
	r.NoError(json.Unmarshal([]byte(res.body), &start))
	r.NotEmpty(start.DeviceCode)
	r.NotEmpty(start.UserCode)

	driver, err := h.store.EnsureDriver(ctx, name, "gt3")
	r.NoError(err)

	pending, err := h.store.ListPendingPairings(ctx)
	r.NoError(err)
	var id int64
	for _, p := range pending {
		if p.UserCode == start.UserCode {
			id = p.ID
		}
	}
	r.NotZero(id, "the pairing the client opened should be waiting for a decision")
	changed, err := h.store.DecidePairing(ctx, id, wire.StatusApproved, &driver.ID, testAdmin)
	r.NoError(err)
	r.True(changed)

	var poll wire.PairPoll
	res = h.do(request{
		method: http.MethodPost, path: "/api/v1/pair/poll",
		body: wire.PairPollRequest{DeviceCode: start.DeviceCode},
	})
	r.Equal(http.StatusOK, res.status, res.body)
	r.NoError(json.Unmarshal([]byte(res.body), &poll))
	r.Equal(wire.StatusApproved, poll.Status)
	r.NotEmpty(poll.Token)

	sum, prefix, err := auth.SplitDeviceToken(poll.Token)
	r.NoError(err)
	device, err := h.store.DeviceByToken(ctx, prefix, sum)
	r.NoError(err)
	return poll.Token, driver, device
}

// startPairing opens a grant the way a client does.
func (h *harness) startPairing() wire.PairStart {
	h.t.Helper()
	r := require.New(h.t)
	res := h.do(request{method: http.MethodPost, path: "/api/v1/pair/start", noAuth: true})
	r.Equal(http.StatusOK, res.status, res.body)
	var start wire.PairStart
	r.NoError(json.Unmarshal([]byte(res.body), &start))
	return start
}

// poll asks about a grant the way a client does.
func (h *harness) poll(deviceCode string) wire.PairPoll {
	h.t.Helper()
	r := require.New(h.t)
	res := h.do(request{
		method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
		body: wire.PairPollRequest{DeviceCode: deviceCode},
	})
	r.Equal(http.StatusOK, res.status, res.body)
	var poll wire.PairPoll
	r.NoError(json.Unmarshal([]byte(res.body), &poll))
	return poll
}

// pendingID finds the waiting grant an operator would be looking at, matched by
// the code the driver read out.
func (h *harness) pendingID(userCode string) int64 {
	h.t.Helper()
	r := require.New(h.t)
	pending, err := h.store.ListPendingPairings(context.Background())
	r.NoError(err)
	for _, p := range pending {
		if p.UserCode == userCode {
			return p.ID
		}
	}
	r.FailNow("no waiting pairing carries the code " + userCode)
	return 0
}

// mustDriver is EnsureDriver with the error asserted away.
func (h *harness) mustDriver(ctx context.Context, name string) db.Driver {
	h.t.Helper()
	driver, err := h.store.EnsureDriver(ctx, name, "gt3")
	require.NoError(h.t, err)
	return driver
}

// newToken mints a token for a machine belonging to name, without going
// through the pairing endpoints.
//
// The flow itself is tested in TestPairing; everywhere else a token is a
// precondition rather than the thing under test, and minting it directly keeps
// a test from spending the pairing rate limit on setup. Each token is its own
// rate-limit bucket, which is what a test that means to exhaust one needs.
func (h *harness) newToken(name string) string {
	h.t.Helper()
	r := require.New(h.t)
	ctx := context.Background()

	driver, err := h.store.EnsureDriver(ctx, name, "gt3")
	r.NoError(err)
	token, err := auth.NewDeviceToken()
	r.NoError(err)
	_, err = h.store.CreateDevice(ctx, driver.ID, token.Sum, token.Prefix, "test")
	r.NoError(err)
	return token.Plain
}

// request is one call to the API, as a test writes it.
type request struct {
	method  string
	path    string
	body    any
	raw     string
	token   string
	key     string
	version string
	headers map[string]string
	noAuth  bool
}

// response is what came back, read into memory so a test can assert on it
// without worrying about closing anything.
type response struct {
	status  int
	body    string
	headers http.Header
}

// envelope decodes the error envelope, failing the test if the body is not one.
func (res response) envelope(tb testing.TB) wire.Error {
	tb.Helper()
	var env wire.ErrorEnvelope
	require.NoError(tb, json.Unmarshal([]byte(res.body), &env), res.body)
	require.NotNil(tb, env.Error, "a non-2xx answer must carry an error: %s", res.body)
	return *env.Error
}

func (h *harness) do(rq request) response {
	h.t.Helper()
	r := require.New(h.t)

	var body io.Reader = http.NoBody
	switch {
	case rq.raw != "":
		body = bytes.NewBufferString(rq.raw)
	case rq.body != nil:
		b, err := json.Marshal(rq.body)
		r.NoError(err)
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(context.Background(), rq.method, h.server.URL+rq.path, body)
	r.NoError(err)
	if rq.body != nil || rq.raw != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	token := rq.token
	if token == "" && !rq.noAuth {
		token = h.token
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rq.key != "" {
		req.Header.Set(api.IdempotencyHeader, rq.key)
	}
	if rq.version != "" {
		req.Header.Set("X-Client-Version", rq.version)
	}
	for k, v := range rq.headers {
		req.Header.Set(k, v)
	}

	res, err := h.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	r.NoError(err)
	return response{status: res.StatusCode, body: string(raw), headers: res.Header}
}
