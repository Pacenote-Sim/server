package admin

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pacenote-sim/server/internal/db"
)

// DevicesPath is where the devices page is mounted.
const DevicesPath = "/admin/devices"

// paramRevokeAll carries the driver whose machines the page is about to offer
// to sign out. It is a parameter rather than a page of its own because the
// confirmation belongs beside the list it is about.
const paramRevokeAll = "revoke_all"

// paramBack says which page a revoke was pressed on, so that the answer is
// rendered where the operator is rather than somewhere they have to navigate
// back from. It takes one of two words and never a path, because a redirect
// target read from a form is how an open redirect is written by accident.
const paramBack = "back"

// The two places a revoke button lives.
const (
	backDevices = "devices"
	backDriver  = "driver"
)

// deviceRow is one paired machine as a page lists it.
type deviceRow struct {
	ID         int64
	DriverID   int64
	DriverName string
	// Label is what the client called itself when it paired — its version and
	// platform, usually. It is not unique and it is not trusted; it is there so
	// an operator can tell a driver's desktop from their laptop.
	Label string
	// TokenPrefix is the short clear-text head of the token. The rest was
	// never stored, so this is the whole of what can be shown
	// and it identifies a row rather than authenticating anything.
	TokenPrefix string
	Paired      moment
	LastUsed    moment
	Revoked     moment
	IsRevoked   bool
	// DriverHref is the driver's own page.
	DriverHref string
}

// revokeAllForm is the confirmation that stands between the button and the
// deed. It names the driver and counts the machines, because "revoke all" with
// no number is a button nobody can press with any confidence.
type revokeAllForm struct {
	DriverID   int64
	DriverName string
	Live       int64
	Machines   []deviceRow
	CancelHref string
}

// devicesForm is the devices page.
type devicesForm struct {
	Rows  []deviceRow
	Links pageLinks
	// Total and Live count every machine ever paired and the ones that can
	// still upload. They come from one query, not from the page of rows above.
	Total, Live int64
	// Confirm is the revoke-all confirmation, when one is being asked for.
	Confirm *revokeAllForm
	// Back is the word a revoke form on this page posts, so that the handler
	// renders its answer here.
	Back string
}

// getDevices lists every paired machine, newest first.
func (p *Panel) getDevices(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.renderDevices(w, r, sess, "", "", http.StatusOK)
}

func (p *Panel) renderDevices(w http.ResponseWriter, r *http.Request, sess db.AdminSession, notice, problem string, status int) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()
	q := r.URL.Query()

	form := devicesForm{Back: backDevices}
	cursor := deviceCursorOf(q)

	rows, err := p.deps.Store.PanelDevices(ctx, db.DeviceQuery{After: cursor, Limit: db.PageSize + 1})
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the devices could not be read", slog.Any("error", err))
		if problem == "" {
			problem = "The paired machines could not be read — the database did not answer."
		}
	}

	pg := pager[db.PanelDevice]{base: DevicesPath}
	kept, links := pg.page(rows, db.PageSize, cursor == nil, func(d db.PanelDevice) url.Values {
		c := d.Cursor()
		return url.Values{paramAfter: {itoa(c.ID)}, paramFrom: {stamp(c.CreatedAt)}}
	})
	form.Links = links
	for _, d := range kept {
		form.Rows = append(form.Rows, deviceRowOf(now, d))
	}

	if form.Total, form.Live, err = p.deps.Store.DeviceCounts(ctx); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the devices could not be counted", slog.Any("error", err))
	}

	if id, ok := driverIDOf(q.Get(paramRevokeAll)); ok {
		form.Confirm = p.revokeAllConfirmation(r, id, now)
		if form.Confirm == nil && problem == "" {
			problem = "That driver is not on this server, so there is nothing to sign out."
		}
	}

	p.render(w, r, "devices", pageData{
		Title:    "Devices",
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Devices"),
		Notice:   notice,
		Error:    problem,
		Status:   status,
		Form:     form,
	})
}

// revokeAllConfirmation builds what the operator reads before every one of a
// driver's machines is signed out: the driver, the count, and the machines
// themselves. A driver who is not there is nil, which the caller says plainly.
func (p *Panel) revokeAllConfirmation(r *http.Request, driverID int64, now time.Time) *revokeAllForm {
	ctx := r.Context()
	driver, err := p.deps.Store.DriverByID(ctx, driverID)
	if err != nil {
		return nil
	}
	live, err := p.deps.Store.CountLiveDevicesForDriver(ctx, driverID)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's devices could not be counted", slog.Any("error", err))
		return nil
	}
	machines, err := p.deps.Store.PanelDevicesForDriver(ctx, driverID, db.PageSize)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's devices could not be read", slog.Any("error", err))
	}
	form := &revokeAllForm{
		DriverID:   driverID,
		DriverName: driver.Name,
		Live:       live,
		CancelHref: DevicesPath,
	}
	for _, m := range machines {
		if m.Revoked() {
			continue
		}
		form.Machines = append(form.Machines, deviceRowOf(now, m))
	}
	return form
}

// postRevokeDevice signs one machine out.
//
// It takes effect on that machine's next request and not at its next restart:
// the token is looked up on every call, so the driver's client starts being
// refused within seconds. That immediacy is the feature, which is why the page
// says so before the button is pressed rather than after.
func (p *Panel) postRevokeDevice(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()
	back := backOf(r)

	id, err := strconv.ParseInt(r.PostFormValue("device_id"), 10, 64)
	if err != nil {
		p.afterRevoke(w, r, sess, back, 0, "", "That machine is no longer on this page — load it again.", http.StatusUnprocessableEntity)
		return
	}

	device, err := p.deps.Store.PanelDeviceByID(ctx, id)
	if err != nil {
		p.afterRevoke(w, r, sess, back, 0, "", "That machine is not on this server — nothing was signed out.", http.StatusNotFound)
		return
	}

	revoked, err := p.deps.Store.RevokeDevice(ctx, id)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the device could not be revoked", slog.Any("error", err))
		p.afterRevoke(w, r, sess, back, device.DriverID, "",
			"Nothing was signed out — the database did not answer.", http.StatusInternalServerError)
		return
	}
	if !revoked {
		p.afterRevoke(w, r, sess, back, device.DriverID, "",
			"That machine had already been signed out — nothing changed.", http.StatusOK)
		return
	}

	// The audit row names the machine by its prefix and its owner, never by
	// anything that could be presented as a credential.
	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionDeviceRevoked,
		device.DriverName+" — "+device.TokenPrefix, []string{"revoked_at"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the revocation could not be recorded", slog.Any("error", err))
	}
	p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "device revoked",
		slog.Int64("device_id", id), slog.Int64("driver_id", device.DriverID))

	p.afterRevoke(w, r, sess, back, device.DriverID,
		"Signed out — that machine's next upload is refused, and "+device.DriverName+
			" pairs again from their client to carry on.", "", http.StatusOK)
}

// postRevokeDriverDevices signs out every machine of one driver.
//
// It is the lost-laptop action, and it is scoped: no other driver stops
// uploading because of it, which is the whole difference between this and the
// danger zone's "revoke every device token".
func (p *Panel) postRevokeDriverDevices(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()
	back := backOf(r)

	driverID, err := strconv.ParseInt(r.PostFormValue("driver_id"), 10, 64)
	if err != nil {
		p.afterRevoke(w, r, sess, back, 0, "", "That driver is no longer on this page — load it again.", http.StatusUnprocessableEntity)
		return
	}
	driver, err := p.deps.Store.DriverByID(ctx, driverID)
	if err != nil {
		p.afterRevoke(w, r, sess, back, 0, "",
			"That driver is not on this server — nothing was signed out.", http.StatusNotFound)
		return
	}

	n, err := p.deps.Store.RevokeDevicesForDriver(ctx, driverID)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's devices could not be revoked", slog.Any("error", err))
		p.afterRevoke(w, r, sess, back, driverID, "",
			"Nothing was signed out — the database did not answer.", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		p.afterRevoke(w, r, sess, back, driverID, "",
			"That driver had no machine still signed in — nothing changed.", http.StatusOK)
		return
	}

	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionDriverDevicesRevoked,
		driver.Name, []string{"revoked_at"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the revocation could not be recorded", slog.Any("error", err))
	}
	p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "every device of one driver revoked",
		slog.Int64("driver_id", driverID), slog.Int64("devices", n))

	p.afterRevoke(w, r, sess, back, driverID, "Signed out "+plural(n, "machine")+" — "+driver.Name+
		" pairs again before their next upload. No other driver is affected.", "", http.StatusOK)
}

// afterRevoke renders the answer on the page the button was pressed on.
func (p *Panel) afterRevoke(
	w http.ResponseWriter, r *http.Request, sess db.AdminSession,
	back string, driverID int64, notice, problem string, status int,
) {
	if back == backDriver && driverID > 0 {
		p.renderDriver(w, r, sess, driverID, notice, problem, status)
		return
	}
	p.renderDevices(w, r, sess, notice, problem, status)
}

// backOf reads which page a revoke was pressed on. Anything but the one known
// word is the devices list, so a form that was edited cannot send the operator
// anywhere this panel did not write.
func backOf(r *http.Request) string {
	if strings.TrimSpace(r.PostFormValue(paramBack)) == backDriver {
		return backDriver
	}
	return backDevices
}

// driverIDOf reads a driver identifier out of a query parameter.
func driverIDOf(raw string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// deviceRowOf renders one machine against a single clock, so that every "ago"
// on one page is measured from the same instant rather than from whenever each
// line happened to be built.
func deviceRowOf(now time.Time, d db.PanelDevice) deviceRow {
	return deviceRow{
		ID:          d.ID,
		DriverID:    d.DriverID,
		DriverName:  d.DriverName,
		Label:       d.Label,
		TokenPrefix: d.TokenPrefix,
		Paired:      momentAt(now, d.CreatedAt),
		LastUsed:    momentOf(now, d.LastUsedAt),
		Revoked:     momentOf(now, d.RevokedAt),
		IsRevoked:   d.Revoked(),
		DriverHref:  driverHref(d.DriverID),
	}
}

// driverHref is one driver's own page.
func driverHref(id int64) string { return DriversPath + "/" + itoa(id) }

// revokeAllHref is the devices page asking to confirm signing out everything
// this driver has.
func revokeAllHref(id int64) string {
	return DevicesPath + "?" + url.Values{paramRevokeAll: {itoa(id)}}.Encode()
}
