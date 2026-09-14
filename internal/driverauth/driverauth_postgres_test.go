//go:build postgres

package driverauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/driverauth"
	"github.com/pacenote-sim/server/internal/logging"
)

// A driver signed in to a browser.
//
// This server does not authenticate them and has no way to — a plugin does that
// and then says who. What is tested here is the half that is left: that the
// session is real, that it ends when it is ended, and that nothing in the
// database can be used to sign in as somebody.

// opened is a session store over a database of its own, with one driver on the
// roster, and a clock a test can move.
func opened(t *testing.T) (*driverauth.Sessions, *db.Store, db.Driver, *time.Time) {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	store, err := db.Open(ctx, dbtest.URL(t), logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	driver, err := store.EnsureDriver(ctx, "Ana Ruiz", "gt3")
	r.NoError(err)

	now := time.Now()
	return driverauth.New(store, func() time.Time { return now }), store, driver, &now
}

// a request and a recorder, with whatever cookies were set on the last answer.
func exchange(t *testing.T, prev *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/", http.NoBody)
	req.Header.Set("User-Agent", "Mozilla/5.0 (a laptop)")
	if prev != nil {
		for _, c := range prev.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	return httptest.NewRecorder(), req
}

func TestADriverSignsInAndBackOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	sessions, store, driver, _ := opened(t)

	// Nobody, before anything happens.
	w, req := exchange(t, nil)
	_, ok := sessions.Current(ctx, req)
	r.False(ok)

	sess, err := sessions.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)
	r.Equal(driver.ID, sess.DriverID)
	r.Equal("Ana Ruiz", sess.Name)
	r.Equal("driver-login", sess.SignedInBy)

	// The cookie is set, and is not readable as a token by anything that only
	// has the database.
	cookies := w.Result().Cookies()
	r.Len(cookies, 1)
	r.Equal(driverauth.SessionCookie, cookies[0].Name)
	r.NotEmpty(cookies[0].Value)
	r.True(cookies[0].HttpOnly, "the session cookie is readable by script")

	stored, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(stored, 1)
	r.Equal("driver-login", stored[0].SignedInBy)
	r.Contains(stored[0].UserAgent, "Mozilla")

	// The next request knows them.
	next, req2 := exchange(t, w)
	back, ok := sessions.Current(ctx, req2)
	r.True(ok, "a browser that had just signed in was not recognised")
	r.Equal("ana-ruiz", back.Slug)

	// And signing out ends it in the database as well as in the browser: a
	// cookie cleared and a row left behind is a session anybody who kept the
	// value can still use.
	r.NoError(sessions.End(ctx, next, req2))
	_, req3 := exchange(t, w)
	_, ok = sessions.Current(ctx, req3)
	r.False(ok, "a signed-out session still worked")

	stored, err = store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Empty(stored)
}

// A plugin naming somebody who is not on the roster. It is refused rather than
// guessed at: creating a driver because somebody signed in would let a login
// plugin fill the roster with whoever it liked.
func TestAPluginCannotSignInSomebodyWhoIsNotHere(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	sessions, store, _, _ := opened(t)

	w, req := exchange(t, nil)
	_, err := sessions.Start(context.Background(), w, req, "somebody-else", "driver-login")
	r.ErrorIs(err, driverauth.ErrNoDriver)
	r.Empty(w.Result().Cookies(), "a cookie was set for a driver that does not exist")

	drivers, err := store.ListDrivers(context.Background())
	r.NoError(err)
	r.Len(drivers, 1, "a driver was created by somebody signing in")
}

// A session that has run out is simply absent. Expiry is enforced by the
// lookup, so there is no window in which an old cookie still works and no sweep
// that has to have run first.
func TestASessionThatHasRunOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	sessions, store, driver, now := opened(t)

	// Signed in long enough ago that it has already run out. The clock moves
	// backwards rather than forwards because expiry is enforced by the database
	// against the database's own clock — which is the point of doing it there,
	// and which means a test cannot expire a session by moving its own.
	*now = time.Now().Add(-driverauth.SessionTTL - time.Hour)
	w, req := exchange(t, nil)
	_, err := sessions.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)

	*now = time.Now()
	_, req2 := exchange(t, w)
	_, ok := sessions.Current(ctx, req2)
	r.False(ok, "a session past its life still worked")

	// The row is still there until the sweep, which is housekeeping and not a
	// check: the lookup already refused it.
	n, err := sessions.Sweep(ctx)
	r.NoError(err)
	r.EqualValues(1, n)
	left, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Empty(left)
}

// Using a session extends it, but not on every request: a driver reading a page
// of their own laps should not cost a row update per image on it.
func TestUsingASessionExtendsItWithoutWritingEveryTime(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	sessions, store, driver, now := opened(t)

	w, req := exchange(t, nil)
	_, err := sessions.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)
	first, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)

	// A second request straight away writes nothing.
	_, req2 := exchange(t, w)
	_, ok := sessions.Current(ctx, req2)
	r.True(ok)
	same, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Equal(first[0].ExpiresAt, same[0].ExpiresAt, "a session was extended on a request moments later")

	// One much later does.
	*now = now.Add(2 * time.Hour)
	_, req3 := exchange(t, w)
	_, ok = sessions.Current(ctx, req3)
	r.True(ok)
	later, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.True(later[0].ExpiresAt.After(first[0].ExpiresAt),
		"a driver who came back the same day was not kept signed in")
}

// Only the digest of a session token is stored, so a dump of the database
// cannot be used to sign in as anybody.
func TestTheDatabaseHoldsNothingThatSignsAnybodyIn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	sessions, store, driver, _ := opened(t)

	w, req := exchange(t, nil)
	_, err := sessions.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)
	token := w.Result().Cookies()[0].Value
	r.NotEmpty(token)

	// Everything the table holds about this session, as a string.
	stored, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(stored, 1)
	all := strings.Join([]string{
		stored[0].UserAgent, stored[0].SignedInBy,
		stored[0].CreatedAt.String(), stored[0].ExpiresAt.String(),
	}, " ")
	r.NotContains(all, token, "the session token is readable in the database")
}

// One browser at a time. An operator ending the session on a lost laptop must
// not end the one on the driver's phone.
func TestEndingOneBrowserLeavesTheOthers(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	sessions, store, driver, _ := opened(t)

	laptop, req := exchange(t, nil)
	_, err := sessions.Start(ctx, laptop, req, driver.Slug, "driver-login")
	r.NoError(err)
	phone, req2 := exchange(t, nil)
	_, err = sessions.Start(ctx, phone, req2, driver.Slug, "driver-login")
	r.NoError(err)

	all, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(all, 2)

	gone, err := store.DeleteDriverSessionByID(ctx, all[0].ID)
	r.NoError(err)
	r.True(gone)

	// Pressing it again is not an error: the operator got what they wanted.
	gone, err = store.DeleteDriverSessionByID(ctx, all[0].ID)
	r.NoError(err)
	r.False(gone)

	left, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(left, 1, "ending one browser ended both")

	// And the lost-laptop action ends every one of them.
	n, err := store.DeleteDriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.EqualValues(1, n)
}

// A browser that announces itself at length. The panel shows what it said, so
// what it said is cut to something a table cell holds.
func TestASessionRemembersOnlySoMuchOfABrowser(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	sessions, store, driver, _ := opened(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/", http.NoBody)
	req.Header.Set("User-Agent", strings.Repeat("Mozilla/5.0 ", 100))
	_, err := sessions.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)

	stored, err := store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(stored, 1)
	r.LessOrEqual(len(stored[0].UserAgent), 200, "a browser's own description was stored whole")
}

// A store with no clock of its own uses the real one, which is how the server
// builds it.
func TestASessionStoreWithNoClock(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	store, err := db.Open(ctx, dbtest.URL(t), logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))
	driver, err := store.EnsureDriver(ctx, "Ana Ruiz", "gt3")
	r.NoError(err)

	sessions := driverauth.New(store, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/", http.NoBody)
	_, err = sessions.Start(ctx, w, req, driver.Slug, "driver-login")
	r.NoError(err)
	r.Len(w.Result().Cookies(), 1)
}

// Signing out of a browser that was never signed in is not an error. It is what
// a sign-out link does when somebody follows it twice.
func TestSigningOutWhenNobodyIsSignedIn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	sessions, _, _, _ := opened(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/", http.NoBody)
	r.NoError(sessions.End(context.Background(), w, req))

	// The cookie is cleared regardless, so a browser holding a value this
	// server has forgotten stops sending it.
	cookies := w.Result().Cookies()
	r.Len(cookies, 1)
	r.Empty(cookies[0].Value)
	r.Negative(cookies[0].MaxAge)
}

// A session token that is not one, and a cookie with nothing in it.
func TestACookieThatIsNotASession(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	sessions, _, _, _ := opened(t)
	ctx := context.Background()

	for _, value := range []string{"", "not-a-token", strings.Repeat("a", 200)} {
		req := httptest.NewRequest(http.MethodGet, "/plugin/driver-login/", http.NoBody)
		req.AddCookie(&http.Cookie{Name: driverauth.SessionCookie, Value: value})
		_, ok := sessions.Current(ctx, req)
		r.False(ok, "%q was accepted as a session", value)
	}
}
