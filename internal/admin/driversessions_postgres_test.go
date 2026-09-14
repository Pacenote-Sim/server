//go:build postgres

package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/db"
)

// The browsers a driver is signed in to, on the operator's side.
//
// A driver signs in through a plugin, which means this server is the only place
// the session can be ended: the plugin cannot reach into it, and an operator
// whose driver has lost a laptop needs a button. It is a separate button from
// revoking their machines on purpose — a machine's token uploads laps and a
// browser session reads them, and ending one is not ending the other.

// signedIn puts a browser session on a driver and returns it.
func (p *panel) signedIn(driverID int64, browser, by string) db.DriverSession {
	p.t.Helper()
	r := require.New(p.t)
	ctx := context.Background()

	sum := []byte("digest-" + browser + "-" + strconv.FormatInt(driverID, 10))
	r.NoError(p.store.CreateDriverSession(ctx, driverID, sum,
		time.Now().Add(30*24*time.Hour), browser, by))

	all, err := p.store.DriverSessionsForDriver(ctx, driverID)
	r.NoError(err)
	r.NotEmpty(all)
	return all[0]
}

func TestADriversSignedInBrowsers(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	driver := p.seedDriver("Marta Ferrer", "gt3")
	href := "/admin/drivers/" + strconv.FormatInt(driver.ID, 10)

	// Nothing signed in yet, and the page says what would put something here.
	body := p.get(href)
	r.Contains(body, "is not signed in anywhere")
	r.Contains(body, "Signing in is something a plugin does")

	p.signedIn(driver.ID, "Mozilla/5.0 (a laptop)", "driver-login")

	body = p.get(href)
	r.Contains(body, "Signed-in browsers")
	r.Contains(body, "Mozilla/5.0 (a laptop)")
	r.Contains(body, "driver-login", "the page does not say which plugin vouched for the session")
	r.Contains(body, "Runs out")
	r.Contains(body, "Sign Marta Ferrer out everywhere")
}

// A session with nothing to say about itself still renders. A browser that sent
// no user agent, and a plugin that has since been removed, are both ordinary.
func TestASessionThatSaysLittleAboutItself(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	driver := p.seedDriver("Marta Ferrer", "gt3")
	p.signedIn(driver.ID, "", "")

	body := p.get("/admin/drivers/" + strconv.FormatInt(driver.ID, 10))
	r.Contains(body, "a browser that did not say what it was")
	r.Contains(body, "a plugin that is no longer installed")
}

func TestSigningOneBrowserOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	p := newPanel(t)

	driver := p.seedDriver("Marta Ferrer", "gt3")
	laptop := p.signedIn(driver.ID, "a laptop", "driver-login")
	p.signedIn(driver.ID, "a phone", "driver-login")

	href := "/admin/drivers/" + strconv.FormatInt(driver.ID, 10)
	page := p.get(href)
	r.Equal(http.StatusSeeOther, p.post(admin.SessionsRevokePath, url.Values{
		"csrf":       {csrfOf(t, page)},
		"driver_id":  {strconv.FormatInt(driver.ID, 10)},
		"session_id": {strconv.FormatInt(laptop.ID, 10)},
	}))

	left, err := p.store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(left, 1, "signing one browser out signed both out")
	r.Equal("a phone", left[0].UserAgent)

	r.Equal(1, p.auditCount(db.ActionDriverSignedOut))

	// Pressing it again is not an error: the operator got what they wanted.
	page = p.get(href)
	r.Equal(http.StatusSeeOther, p.post(admin.SessionsRevokePath, url.Values{
		"csrf":       {csrfOf(t, page)},
		"driver_id":  {strconv.FormatInt(driver.ID, 10)},
		"session_id": {strconv.FormatInt(laptop.ID, 10)},
	}))
}

// The lost-laptop action, for the person rather than the machine. Their
// machines keep uploading: a driver who cannot find their laptop still wants
// the laps their rig is sending.
func TestSigningADriverOutEverywhere(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	p := newPanel(t)

	driver := p.seedDriver("Marta Ferrer", "gt3")
	token, _ := p.seedDevice(driver.ID, "the sim rig")
	p.signedIn(driver.ID, "a laptop", "driver-login")
	p.signedIn(driver.ID, "a phone", "driver-login")

	page := p.get("/admin/drivers/" + strconv.FormatInt(driver.ID, 10))
	r.Equal(http.StatusSeeOther, p.post(admin.SessionsRevokePath+"-all", url.Values{
		"csrf":      {csrfOf(t, page)},
		"driver_id": {strconv.FormatInt(driver.ID, 10)},
	}))

	left, err := p.store.DriverSessionsForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Empty(left)

	// And the machine is untouched.
	_ = token
	devices, err := p.store.ListDevicesForDriver(ctx, driver.ID)
	r.NoError(err)
	r.Len(devices, 1, "signing a driver out of their browsers revoked their machine")
	r.False(devices[0].Revoked())

	rows, err := p.store.AuditFor(ctx, db.ActionDriverSignedOut, 10)
	r.NoError(err)
	r.Len(rows, 1)
	r.Equal("2 browsers", rows[0].Subject, "the trail does not count the way a sentence does")
}

// The values these forms refuse. Both post an identifier out of a page that may
// have moved on, so neither may act on one it cannot read.
func TestTheSignOutFormsRefuseWhatTheyCannotRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	driver := p.seedDriver("Marta Ferrer", "gt3")
	session := p.signedIn(driver.ID, "a laptop", "driver-login")
	href := "/admin/drivers/" + strconv.FormatInt(driver.ID, 10)

	for _, path := range []string{admin.SessionsRevokePath, admin.SessionsRevokePath + "-all"} {
		// A stale form.
		r.Equal(http.StatusForbidden, p.post(path, url.Values{"csrf": {"not-the-token"}}),
			"%s accepted a stale form", path)

		// A driver identifier that is not one.
		page := p.get(href)
		r.Equal(http.StatusUnprocessableEntity, p.post(path, url.Values{
			"csrf": {csrfOf(t, page)}, "driver_id": {"whoever"},
		}), "%s acted on a driver it could not read", path)
	}

	// A session identifier that is not one goes back to the page rather than
	// ending something else.
	page := p.get(href)
	r.Equal(http.StatusSeeOther, p.post(admin.SessionsRevokePath, url.Values{
		"csrf":       {csrfOf(t, page)},
		"driver_id":  {strconv.FormatInt(driver.ID, 10)},
		"session_id": {"whichever"},
	}))
	left, err := p.store.DriverSessionsForDriver(context.Background(), driver.ID)
	r.NoError(err)
	r.Len(left, 1, "a session was ended by a form naming no session")
	r.Equal(session.ID, left[0].ID)
}
