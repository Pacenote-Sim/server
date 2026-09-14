package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// pairingRow is one waiting request as the page shows it.
type pairingRow struct {
	ID       int64
	UserCode string
	Waiting  time.Duration
	ExpireIn time.Duration
}

// pairingsForm is what the pairing page renders.
type pairingsForm struct {
	Pending []pairingRow
	Drivers []db.Driver
}

// getPairings lists the machines waiting to be let in.
//
// The user code is the whole point of the page: a driver reads six characters
// off their screen and the operator matches them here, which is what makes an
// approval an approval of that machine rather than of whoever happened to ask
// at the same moment.
func (p *Panel) getPairings(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.renderPairings(w, r, sess, "", "")
}

func (p *Panel) renderPairings(w http.ResponseWriter, r *http.Request, sess db.AdminSession, notice, problem string) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()

	var form pairingsForm
	pending, err := p.deps.Store.ListPendingPairings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the pairing requests could not be read", slog.Any("error", err))
		problem = "The waiting requests could not be read — the database did not answer."
	}
	for _, pr := range pending {
		form.Pending = append(form.Pending, pairingRow{
			ID:       pr.ID,
			UserCode: pr.UserCode,
			Waiting:  now.Sub(pr.CreatedAt),
			ExpireIn: pr.ExpiresAt.Sub(now),
		})
	}
	if form.Drivers, err = p.deps.Store.ListDrivers(ctx); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the drivers could not be read", slog.Any("error", err))
	}

	p.render(w, r, "pairings", pageData{
		Title:    "Pairing",
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Pairing"),
		Notice:   notice,
		Error:    problem,
		Form:     form,
	})
}

// postPairingDecision approves or denies one waiting request.
//
// Approving needs a driver, because a token belongs to a person and not to a
// machine. An operator either picks someone already here or types a name, and
// typing a name that is already here attaches the second machine to the same
// person rather than creating a second one with the same name.
func (p *Panel) postPairingDecision(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}

	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		p.renderPairings(w, r, sess, "", "That request is no longer on this page — load it again.")
		return
	}
	deny := r.PostFormValue("decision") == "deny"
	if deny {
		p.decide(w, r, sess, id, wire.StatusDenied, nil)
		return
	}

	driverID, problem := p.driverFor(r)
	if problem != "" {
		p.renderPairings(w, r, sess, "", problem)
		return
	}
	p.decide(w, r, sess, id, wire.StatusApproved, driverID)
}

// driverFor resolves the driver an approval is for: one already on the roster,
// or a new one from the name the operator typed.
func (p *Panel) driverFor(r *http.Request) (*int64, string) {
	if existing := strings.TrimSpace(r.PostFormValue("driver_id")); existing != "" {
		id, err := strconv.ParseInt(existing, 10, 64)
		if err != nil {
			return nil, "That driver is no longer on this page — load it again."
		}
		return &id, ""
	}
	name := strings.TrimSpace(r.PostFormValue("driver_name"))
	if name == "" {
		return nil, "An approval needs a driver — pick one, or type the name of a new one."
	}
	driver, err := p.deps.Store.EnsureDriver(r.Context(), name, strings.TrimSpace(r.PostFormValue("driver_class")))
	if err != nil {
		p.deps.Log.LogAttrs(r.Context(), slog.LevelError, "the driver could not be created", slog.Any("error", err))
		return nil, "That driver could not be created — the name has nothing in it a web address could carry."
	}
	return &driver.ID, ""
}

func (p *Panel) decide(w http.ResponseWriter, r *http.Request, sess db.AdminSession, id int64, status wire.PairStatus, driverID *int64) {
	changed, err := p.deps.Store.DecidePairing(r.Context(), id, status, driverID, sess.Email)
	if err != nil {
		p.deps.Log.LogAttrs(r.Context(), slog.LevelError, "the pairing decision could not be recorded",
			slog.Any("error", err))
		p.renderPairings(w, r, sess, "", "That decision was not recorded — the database did not answer.")
		return
	}
	if !changed {
		p.renderPairings(w, r, sess, "",
			"That request had already been decided or its code had run out — nothing changed.")
		return
	}

	p.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "pairing decided",
		slog.Int64("pairing_id", id), slog.String("status", string(status)))

	notice := "Approved — the machine picks up its token the next time it polls, within a couple of seconds."
	if status == wire.StatusDenied {
		notice = "Denied — that machine is told so and stops asking."
	}
	p.renderPairings(w, r, sess, notice, "")
}
