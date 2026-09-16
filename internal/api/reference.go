package api

import (
	"errors"
	"net/http"

	"github.com/pacenote-sim/protocol/trace"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// DefaultScope is what GET /reference uses when the client names none. It is
// the widest, because an unqualified request means "the best lap you can give
// me" and the fallback below narrows it to whatever this server actually holds.
const DefaultScope = wire.ScopeClass

// getReference answers with the lap a driver's own is compared against.
//
// Scope is a preference and not a demand. The server tries the scope asked for
// and then the narrower ones, and says which it used, so a server with no class
// data still answers with the driver's own best rather than nothing at all. The
// community edition maintains the driver's own best and the best in each car;
// the class-wide scope is an enterprise feature, so asking for it here is
// answered with the best in that car.
//
// The simulator is not a preference. It is required, it is matched exactly, and
// it is named by the caller rather than guessed at from their last upload: a
// driver who owns two simulators has a most recent stint in one of them at the
// moment they are sitting in the other, and a server that inferred it would
// hand them a reference lap from the wrong physics at exactly the moment it
// matters. A request that names no simulator is a request the server cannot
// answer correctly, so it is refused rather than answered.
func (a *API) getReference(w http.ResponseWriter, r *http.Request, s session) {
	q := r.URL.Query()
	sim := q.Get("sim")
	trackID := q.Get("track_id")
	car := q.Get("car")
	class := q.Get("class")

	if blank(sim) {
		a.failDetail(w, r, wire.CodeInvalid,
			"That request did not say which simulator to compare against — no lap was looked up.",
			map[string]any{"field": "sim"})
		return
	}
	if blank(trackID) {
		a.failDetail(w, r, wire.CodeInvalid,
			"That request did not say which track to compare against — no lap was looked up.",
			map[string]any{"field": "track_id"})
		return
	}
	scope := wire.Scope(q.Get("scope"))
	if scope == "" {
		scope = DefaultScope
	}
	pref := scopePreference(scope)
	if pref == nil {
		a.failDetail(w, r, wire.CodeInvalid,
			"That request asked for a scope this server does not know — no lap was looked up.",
			map[string]any{"field": "scope", "value": string(scope)})
		return
	}

	res, err := a.deps.Store.ReferenceLap(r.Context(), db.ReferenceQuery{
		DriverID:   s.driver.ID,
		Sim:        sim,
		TrackID:    trackID,
		Car:        car,
		CarClass:   class,
		Preference: pref,
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		a.failDetail(w, r, wire.CodeNotFound, msgNoReference,
			map[string]any{"sim": sim, "track_id": trackID, "car": car, "class": class})
		return
	case err != nil:
		a.failServer(w, r, "the reference lap could not be read", err)
		return
	}

	points, err := trace.Decode(res.Trace, nil)
	if err != nil {
		// A blob that will not decode is this server's problem, not the
		// driver's: it was written by this server's own codec.
		a.failServer(w, r, "the stored reference trace could not be decoded", err)
		return
	}

	// The name is shown beside someone else's lap and left off the driver's
	// own, which is what the contract asks for: "M. Costa" next to a rival's
	// time means something, and next to your own it is noise.
	name := res.DriverName
	if res.DriverID == s.driver.ID {
		name = ""
	}
	a.ok(w, wire.ReferenceLap{
		Scope:      res.Scope,
		LapMs:      res.LapMs,
		DriverName: name,
		Trace:      points,
	})
}

// scopePreference is the scope asked for followed by the narrower ones, which
// is the order the lookup tries them in. An unknown scope is nil, which the
// caller answers as invalid rather than guessing at.
func scopePreference(s wire.Scope) []wire.Scope {
	switch s {
	case wire.ScopeSelf:
		return []wire.Scope{wire.ScopeSelf}
	case wire.ScopeCar:
		return []wire.Scope{wire.ScopeCar, wire.ScopeSelf}
	case wire.ScopeClass:
		return []wire.Scope{wire.ScopeClass, wire.ScopeCar, wire.ScopeSelf}
	default:
		return nil
	}
}
