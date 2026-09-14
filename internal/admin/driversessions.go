package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// The browsers a driver is signed in to.
//
// A driver signs in through whichever plugin the operator installed for it, and
// this server holds the session that comes out. Which means this server is also
// the only place it can be ended — a plugin cannot reach into it, and an
// operator whose driver has lost a laptop needs somewhere to press a button.
//
// It sits on the driver's own page rather than on a page of its own, beside
// their paired machines, because those are the same question asked twice: what
// of this person's is currently able to reach my server.

// SessionsRevokePath is where a session is ended.
const SessionsRevokePath = "/admin/drivers/sessions/revoke"

// driverSessionRow is one signed-in browser as the page lists it.
type driverSessionRow struct {
	ID int64
	// Signed and LastSeen are when it started and when it was last used.
	Signed   moment
	LastSeen moment
	// Expires is when it runs out on its own.
	Expires moment
	// Browser is what the browser said it was, which is all this server knows
	// about the machine: no driver session carries a device token, so there is
	// nothing to match it against a paired machine.
	Browser string
	// By is the plugin that signed this driver in.
	By string
}

// driverSessionRowOf turns a stored session into what the page shows.
func driverSessionRowOf(now time.Time, s db.DriverSession) driverSessionRow {
	browser := s.UserAgent
	if browser == "" {
		browser = "a browser that did not say what it was"
	}
	by := s.SignedInBy
	if by == "" {
		by = "a plugin that is no longer installed"
	}
	return driverSessionRow{
		ID:       s.ID,
		Signed:   momentAt(now, s.CreatedAt),
		LastSeen: momentAt(now, s.LastSeenAt),
		Expires:  momentAt(now, s.ExpiresAt),
		Browser:  browser,
		By:       by,
	}
}

// postRevokeDriverSession signs one browser out.
func (p *Panel) postRevokeDriverSession(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()

	driverID, err := strconv.ParseInt(r.PostFormValue("driver_id"), 10, 64)
	if err != nil || driverID <= 0 {
		httpx.Problem(w, r, http.StatusUnprocessableEntity,
			"That driver is no longer on this page — load it again.")
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("session_id"), 10, 64)
	if err != nil || id <= 0 {
		p.redirectToDriver(w, r, driverID)
		return
	}

	// Ended whether or not it was there. An operator pressing the button twice,
	// or pressing it on a session that expired while they read the page, has
	// got what they wanted either way.
	if _, err := p.deps.Store.DeleteDriverSessionByID(ctx, id); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "a driver session could not be ended", slog.Any("error", err))
		httpx.Problem(w, r, http.StatusInternalServerError,
			"That was not signed out — the database did not answer.")
		return
	}
	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionDriverSignedOut,
		itoa(driverID), []string{"driver_sessions"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the sign-out could not be recorded", slog.Any("error", err))
	}
	p.redirectToDriver(w, r, driverID)
}

// postRevokeDriverSessions signs one driver out of everything, which is the
// lost-laptop action for the person rather than for the machine. It is separate
// from revoking their devices: a machine's token uploads laps, and a browser
// session reads them, and an operator may want to end one without the other.
func (p *Panel) postRevokeDriverSessions(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()

	driverID, err := strconv.ParseInt(r.PostFormValue("driver_id"), 10, 64)
	if err != nil || driverID <= 0 {
		httpx.Problem(w, r, http.StatusUnprocessableEntity,
			"That driver is no longer on this page — load it again.")
		return
	}
	n, err := p.deps.Store.DeleteDriverSessionsForDriver(ctx, driverID)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's sessions could not be ended", slog.Any("error", err))
		httpx.Problem(w, r, http.StatusInternalServerError,
			"Nothing was signed out — the database did not answer.")
		return
	}
	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionDriverSignedOut,
		plural(n, "browser"), []string{"driver_sessions"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the sign-out could not be recorded", slog.Any("error", err))
	}
	p.redirectToDriver(w, r, driverID)
}

// redirectToDriver sends the operator back to the page they pressed the button
// on, so that what they see afterwards is the list without the row.
func (p *Panel) redirectToDriver(w http.ResponseWriter, r *http.Request, driverID int64) {
	http.Redirect(w, r, driverHref(driverID), http.StatusSeeOther)
}
