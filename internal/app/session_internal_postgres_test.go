//go:build postgres

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/driverauth"
	"github.com/pacenote-sim/server/internal/logging"
)

// What this server tells a plugin about whoever is asking, and what it does
// when a plugin asks it to sign somebody in.
//
// These three functions are the join between two things that must not be joined
// any other way: the plugin router, which has a request, and this server's own
// session tables, which have the only true answer about who is holding it.

// wired is a store with one driver, and the three functions the router is given.
func wired(t *testing.T) (*db.Store, db.Driver, *admin.Panel, *driverauth.Sessions) {
	t.Helper()
	store, driver, panel, sessions, _ := wiredAt(t)
	return store, driver, panel, sessions
}

// wiredAt is the same, with the database's own address for the tests that have
// to look at a table rather than through the store.
func wiredAt(t *testing.T) (*db.Store, db.Driver, *admin.Panel, *driverauth.Sessions, string) {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	url := dbtest.URL(t)
	store, err := db.Open(ctx, url, logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	hash, err := auth.HashPassword("correct horse battery staple")
	r.NoError(err)
	_, err = store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        "ana@example.com",
		AdminPasswordHash: hash,
		Settings:          config.DefaultSettings("Iberian GT", "pacenote.example.com", config.TLSProxy),
	})
	r.NoError(err)

	driver, err := store.EnsureDriver(ctx, "Marta Ferrer", "gt3")
	r.NoError(err)

	panel, err := admin.New(admin.Deps{
		Log:       logging.Discard(),
		Store:     store,
		Version:   "v0.0.0-test",
		StartedAt: time.Now(),
		Settings:  store.Settings,
	})
	r.NoError(err)

	return store, driver, panel, driverauth.New(store, time.Now), url
}

// liveRows counts what is actually in driver_sessions, expired or not.
//
// It goes round the store on purpose. Every read there filters expiry out, so a
// test that used one could not tell a row that has been swept from a row that
// is merely past its date — and the sweep is the only thing this distinguishes.
func liveRows(t *testing.T, url string) int {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, url)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	var n int
	r.NoError(conn.QueryRow(ctx, `SELECT count(*) FROM driver_sessions`).Scan(&n))
	return n
}

func TestWhatAPluginIsToldAboutTheCaller(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	_, driver, panel, drivers := wired(t)
	who := callerFor(panel, drivers, nil, logging.Discard())

	// Nobody, which is most requests to a public plugin page.
	req := httptest.NewRequest(http.MethodGet, "/plugin/results/", http.NoBody)
	caller := who(req)
	r.Empty(caller.DriverSlug)
	r.Empty(caller.AdminEmail)

	// A driver signed in by a plugin.
	w := httptest.NewRecorder()
	_, err := drivers.Start(req.Context(), w, req, driver.Slug, "driver-login")
	r.NoError(err)

	signed := httptest.NewRequest(http.MethodGet, "/plugin/results/", http.NoBody)
	for _, c := range w.Result().Cookies() {
		signed.AddCookie(c)
	}
	caller = who(signed)
	r.Equal("marta-ferrer", caller.DriverSlug)
	r.Equal("Marta Ferrer", caller.DriverName)
	r.Empty(caller.AdminEmail, "a signed-in driver was reported as an operator")
}

// The client has no browser and no session. It has the device token it uploads
// with, and on a plugin's route that token is the same driver — checked the way
// the API checks it, and never handed to the plugin.
func TestAClientsTokenIsTheDriverOnAPluginRoute(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, driver, panel, drivers := wired(t)
	ctx := context.Background()

	tokens, err := api.New(ctx, api.Deps{Store: store, Log: logging.Discard()})
	r.NoError(err)
	who := callerFor(panel, drivers, tokens, logging.Discard())

	token, err := auth.NewDeviceToken()
	r.NoError(err)
	device, err := store.CreateDevice(ctx, driver.ID, token.Sum, token.Prefix, "the client")
	r.NoError(err)

	withToken := func(value string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/plugin/engineer/laps", http.NoBody)
		req.Header.Set("Authorization", value)
		return req
	}

	caller := who(withToken("Bearer " + token.Plain))
	r.Equal("marta-ferrer", caller.DriverSlug)
	r.Equal("Marta Ferrer", caller.DriverName)
	r.Empty(caller.AdminEmail)

	// A token nobody issued, and no token, are nobody — the route's own
	// access rule answers them, not this.
	r.Empty(who(withToken("Bearer not-a-token")).DriverSlug)
	r.Empty(who(httptest.NewRequest(http.MethodGet, "/plugin/engineer/me", http.NoBody)).DriverSlug)

	// A token the operator took back is nobody too.
	revoked, err := store.RevokeDevice(ctx, device.ID)
	r.NoError(err)
	r.True(revoked)
	r.Empty(who(withToken("Bearer "+token.Plain)).DriverSlug, "a revoked token still named its driver")
}

func TestSigningADriverInFromAPlugin(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, driver, panel, drivers := wired(t)
	ctx := context.Background()

	in := signInWith(drivers)
	out := signOutWith(drivers)
	who := callerFor(panel, drivers, nil, logging.Discard())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/callback", http.NoBody)
	r.NoError(in(w, req, driver.Slug, "driver-login"))

	stored, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(stored, 1)
	r.Equal("driver-login", stored[0].SignedInBy)

	// The browser that was signed in is recognised, and signing it out ends it.
	signed := httptest.NewRequest(http.MethodGet, "/plugin/results/", http.NoBody)
	for _, c := range w.Result().Cookies() {
		signed.AddCookie(c)
	}
	r.Equal(driver.Slug, who(signed).DriverSlug)

	r.NoError(out(httptest.NewRecorder(), signed))
	r.Empty(who(signed).DriverSlug, "a signed-out browser was still recognised")

	// A plugin naming somebody who is not on the roster is refused, and no
	// cookie is set: a login plugin must not be able to fill the roster.
	r.ErrorIs(in(httptest.NewRecorder(), req, "somebody-else", "driver-login"), driverauth.ErrNoDriver)
}

// The housekeeping that clears out sessions nobody came back for. Nothing
// depends on it having run — the lookup refuses an expired session either way —
// so what matters is that it stops when the phase does.
func TestTheDriverSessionSweep(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, driver, _, _, url := wiredAt(t)
	ctx := context.Background()

	// A session that ran out before the server started.
	past := driverauth.New(store, func() time.Time {
		return time.Now().Add(-driverauth.SessionTTL - time.Hour)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/", http.NoBody)
	_, err := past.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)

	// Checked before the sweeper starts, not after: it runs every few
	// milliseconds and would otherwise have cleared the row before the
	// assertion that there was one to clear.
	r.Equal(1, liveRows(t, url), "the fixture did not leave a row to sweep")

	sessions := driverauth.New(store, time.Now)
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweepDriverSessions(sweepCtx, sessions, logging.Discard(), 5*time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		return liveRows(t, url) == 0
	}, 10*time.Second, 20*time.Millisecond, "the expired session was never cleared out")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.Fail("the sweep did not stop when its context was cancelled")
	}
}
