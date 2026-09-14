//go:build postgres

package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

func TestDevicesPage(t *testing.T) {
	t.Parallel()

	t.Run("an empty list says how a machine gets here", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/devices")
		r.Contains(body, "No machine has been paired yet")
		r.Contains(body, "approve the code on the pairing page")
	})

	t.Run("a paired machine shows its driver, its label and its token prefix", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		token, device := p.seedDevice(driver.ID, "race laptop")

		body := p.get("/admin/devices")
		r.Contains(body, "Marta Ferrer")
		r.Contains(body, "race laptop")
		r.Contains(body, device.TokenPrefix, "the prefix identifies the row")
		r.NotContains(body, token, "the token itself is never on a page")
		r.Contains(body, "Revoking signs this machine out — the driver pairs again to keep uploading.",
			"the page says what the button does before it is pressed")
	})

	t.Run("the list pages across a boundary", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		for i := range db.PageSize + 3 {
			p.seedDevice(driver.ID, fmt.Sprintf("machine %02d", i))
		}

		first := p.get("/admin/devices")
		r.Contains(first, "Next page")
		r.Contains(first, "machine 27", "newest first, so the last one paired is at the top")
		r.NotContains(first, "machine 02")

		next := p.get(hrefAfter(t, first, "Next page"))
		r.Contains(next, "machine 02")
		r.Contains(next, "machine 00")
		r.NotContains(next, "machine 27", "the second page starts after the first ends")
		r.NotContains(next, "Next page")
	})

	t.Run("revoking one machine refuses its very next request", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		token, device := p.seedDevice(driver.ID, "race laptop")

		r.Equal(http.StatusOK, p.apiStatus(token), "a paired machine can upload before anything happens")

		before := p.auditCount(db.ActionDeviceRevoked)
		page := p.get("/admin/devices")
		status := p.post("/admin/devices/revoke", url.Values{
			"csrf": {csrfOf(t, page)}, "device_id": {itoa(device.ID)},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Signed out")
		r.Contains(string(p.lastBody), "Marta Ferrer")

		r.Equal(http.StatusUnauthorized, p.apiStatus(token),
			"the point of the feature is that it takes effect at once")
		r.Equal(before+1, p.auditCount(db.ActionDeviceRevoked), "one audit row per change")
	})

	t.Run("revoking a machine twice changes nothing the second time", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")

		page := p.get("/admin/devices")
		form := url.Values{"csrf": {csrfOf(t, page)}, "device_id": {itoa(device.ID)}}
		r.Equal(http.StatusOK, p.post("/admin/devices/revoke", form))
		r.Equal(http.StatusOK, p.post("/admin/devices/revoke", form))
		r.Contains(string(p.lastBody), "already been signed out")
		r.Equal(1, p.auditCount(db.ActionDeviceRevoked), "a repeat is not a second change")
	})

	t.Run("the confirmation names the machines before any go", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		p.seedDevice(driver.ID, "race laptop")
		p.seedDevice(driver.ID, "spare desktop")

		body := p.get(fmt.Sprintf("/admin/devices?revoke_all=%d", driver.ID))
		r.Contains(body, "Sign out every machine of Marta Ferrer")
		r.Contains(body, "race laptop")
		r.Contains(body, "spare desktop")
		r.Contains(body, "Sign out 2 machines")
		r.Contains(body, "no other driver is affected")
		r.Contains(body, "Cancel")
	})

	t.Run("revoking every machine of one driver leaves every other driver alone", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		marta := p.seedDriver("Marta Ferrer", "")
		ana := p.seedDriver("Ana Ruiz", "")
		martaLaptop, _ := p.seedDevice(marta.ID, "laptop")
		martaDesktop, _ := p.seedDevice(marta.ID, "desktop")
		anaLaptop, _ := p.seedDevice(ana.ID, "laptop")

		before := p.auditCount(db.ActionDriverDevicesRevoked)
		page := p.get(fmt.Sprintf("/admin/devices?revoke_all=%d", marta.ID))
		status := p.post("/admin/devices/revoke-all", url.Values{
			"csrf": {csrfOf(t, page)}, "driver_id": {itoa(marta.ID)},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Signed out 2 machines")
		r.Contains(string(p.lastBody), "No other driver is affected")

		r.Equal(http.StatusUnauthorized, p.apiStatus(martaLaptop))
		r.Equal(http.StatusUnauthorized, p.apiStatus(martaDesktop))
		r.Equal(http.StatusOK, p.apiStatus(anaLaptop), "Ana was not part of this")
		r.Equal(before+1, p.auditCount(db.ActionDriverDevicesRevoked))
	})

	t.Run("revoking every machine of a driver with none changes nothing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Nobody Yet", "")
		page := p.get("/admin/devices")
		status := p.post("/admin/devices/revoke-all", url.Values{
			"csrf": {csrfOf(t, page)}, "driver_id": {itoa(driver.ID)},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "no machine still signed in")
		r.Zero(p.auditCount(db.ActionDriverDevicesRevoked))
	})

	t.Run("a revoke pressed on the driver page answers on the driver page", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "race laptop")

		page := p.get(fmt.Sprintf("/admin/drivers/%d", driver.ID))
		status := p.post("/admin/devices/revoke", url.Values{
			"csrf": {csrfOf(t, page)}, "device_id": {itoa(device.ID)}, "back": {"driver"},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Signed out")
		r.Contains(string(p.lastBody), "Recorded", "this is the driver's own page, not the devices list")
	})

	t.Run("a machine or a driver that is not here is refused rather than guessed at", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			path   string
			form   url.Values
			status int
			says   string
		}{
			{
				name: "a device identifier that is not a number", path: "/admin/devices/revoke",
				form:   url.Values{"device_id": {"laptop"}},
				status: http.StatusUnprocessableEntity, says: "no longer on this page",
			},
			{
				name: "a device that does not exist", path: "/admin/devices/revoke",
				form:   url.Values{"device_id": {"99999"}},
				status: http.StatusNotFound, says: "not on this server",
			},
			{
				name: "a driver that does not exist", path: "/admin/devices/revoke-all",
				form:   url.Values{"driver_id": {"99999"}},
				status: http.StatusNotFound, says: "not on this server",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				p := newPanel(t)
				page := p.get("/admin/devices")
				form := url.Values{"csrf": {csrfOf(t, page)}}
				for k, v := range tc.form {
					form[k] = v
				}
				r.Equal(tc.status, p.post(tc.path, form))
				r.Contains(string(p.lastBody), tc.says)
			})
		}
	})

	t.Run("the enterprise feature is shown and disabled rather than hidden", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/devices")
		r.Contains(body, "Automatic expiry")
		r.Contains(body, "Part of Enterprise")
	})

	t.Run("the page needs a session", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		p.signOut()

		status, _ := p.getStatus("/admin/devices")
		r.Equal(http.StatusSeeOther, status)
	})
}

// apiStatus presents a device token to the API and returns what it answered. It
// is how a revocation is proven to have done something rather than merely to
// have written a row: the same token, through the same middleware a driver's
// client goes through.
func (p *panel) apiStatus(token string) int {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		p.server.URL+"/api/v1/me", http.NoBody)
	r.NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}

// Signing out every machine of one driver, and what the page says when the
// driver named is not one.
func TestSigningOutOneDriversMachines(t *testing.T) {
	t.Parallel()

	t.Run("a driver identifier that is not one", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		page := p.get("/admin/devices")
		r.Equal(http.StatusUnprocessableEntity, p.post("/admin/devices/revoke-all", url.Values{
			"csrf": {csrfOf(t, page)}, "driver_id": {"whoever"},
		}))
		r.Contains(string(p.lastBody), "no longer on this page")
	})

	t.Run("a driver who is not on this server", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/devices?revoke_all=999999")
		r.Contains(body, "not on this server, so there is nothing to sign out")
	})

	t.Run("machines already signed out are not offered again", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		ctx := context.Background()

		driver := p.seedDriver("Marta Ferrer", "gt3")
		_, live := p.seedDevice(driver.ID, "the sim rig")
		_, gone := p.seedDevice(driver.ID, "an old laptop")
		revoked, err := p.store.RevokeDevice(ctx, gone.ID)
		r.NoError(err)
		r.True(revoked)

		body := p.get("/admin/devices?revoke_all=" + strconv.FormatInt(driver.ID, 10))

		// Only the confirmation card, because the list above it shows every
		// machine ever paired — including the ones already signed out.
		at := strings.Index(body, "Sign out every machine of")
		r.GreaterOrEqual(at, 0, "the confirmation card is not on the page")
		confirm := body[at:]
		end := strings.Index(confirm, "</form>")
		r.GreaterOrEqual(end, 0, "the confirmation card never ends")
		confirm = confirm[:end]
		r.Contains(confirm, "the sim rig")
		r.NotContains(confirm, "an old laptop",
			"a machine that is already signed out was offered for signing out again")
		r.Contains(confirm, "1 machine", "the count does not match what is listed")
		_ = live
	})
}
