package admin

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// DriversPath is where the roster is mounted.
const DriversPath = "/admin/drivers"

// StintsPath is where one stint's laps are mounted. A stint hangs off a driver
// on the page, but it is addressed on its own: an operator pastes a stint
// identifier out of a log line more often than they walk to it from the roster.
const StintsPath = "/admin/stints"

// sortOption is one of the roster's orderings, as the page offers it.
type sortOption struct {
	Name string
	Href string
	On   bool
}

// driverRow is one line of the roster.
type driverRow struct {
	ID    int64
	Name  string
	Class string
	Href  string
	// LastSeen is when one of their machines last spoke to this server, and
	// LastUpload when their newest lap arrived. Both are here because they
	// answer different questions: a client that is running but not recording
	// has a recent LastSeen and a stale LastUpload.
	LastSeen, LastUpload moment
	Laps, Stints         int64
	TraceBytes           int64
	Devices              int64
	RevokedDevices       int64
}

// driversForm is the roster page.
type driversForm struct {
	Rows  []driverRow
	Links pageLinks
	Sorts []sortOption
	// OnFirstPage says whether an empty list means "no drivers at all" or
	// "nothing past the page you were on", which are different sentences.
	OnFirstPage bool
}

// stintRow is one stint as a driver's page lists it.
type stintRow struct {
	ID          string
	Href        string
	Track       string
	Car         string
	CarClass    string
	SessionType string
	Started     moment
	// Ran is how long the stint lasted, and Finished says whether it was ever
	// closed — a client that stopped mid-session leaves one open.
	Ran      time.Duration
	Finished bool
	Laps     int64
	// BestLapMs is the quickest clean lap, or zero when the stint holds none.
	BestLapMs  int
	TraceBytes int64
}

// driverForm is one driver's own page.
type driverForm struct {
	ID    int64
	Name  string
	Slug  string
	Class string
	// Paired is when this driver was first created, which is when their first
	// pairing was approved.
	Paired               moment
	LastSeen, LastUpload moment
	LapCount, StintCount int64
	TraceBytes           int64
	Devices              []deviceRow
	LiveDevices          int64
	// Sessions is every browser this driver is signed in to, and Signins is
	// where the button that ends one of them posts.
	Sessions      []driverSessionRow
	SignoutHref   string
	SignoutAllRef string
	RevokeAllHref string
	Recent        []stintRow
	Links         pageLinks
	OnFirstPage   bool
	// Back is what a revoke form on this page posts, so that its answer comes
	// back here rather than on the devices list.
	Back string
}

// lapRow is one lap as the stint page lists it.
type lapRow struct {
	Number  int
	LapMs   int
	Kind    string
	Clean   bool
	Started moment
	// TraceBytes is int64 rather than int because the byte formatter in the
	// templates takes an int64 and html/template will not convert one integer
	// kind to another for a function argument — it stops rendering instead.
	TraceBytes int64
	// HasTrace is false for a lap whose trace retention has cleared, and for
	// one that never carried one. The lap and its time are still here, which is
	// the distinction the page has to make plain.
	HasTrace bool
}

// stintForm is one stint's page.
type stintForm struct {
	ID          string
	DriverID    int64
	DriverName  string
	DriverHref  string
	Sim         string
	Track       string
	TrackID     string
	Car         string
	CarClass    string
	SessionType string
	Started     moment
	Finished    moment
	Ran         time.Duration
	IsFinished  bool
	LapCount    int64
	BestLapMs   int
	TraceBytes  int64
	Laps        []lapRow
	Links       pageLinks
	OnFirstPage bool
}

// getDrivers lists the roster.
//
// The three orderings are the three questions an operator opens this page with:
// who is here, whose client last spoke to this server, and who is driving the
// most. Last seen and last upload are columns rather than something to click
// into, because "is Marta's client working" has to be answerable in one look.
func (p *Panel) getDrivers(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()
	q := r.URL.Query()

	order := orderOf(q)
	cursor := rosterCursorOf(q, order)
	form := driversForm{OnFirstPage: cursor == nil, Sorts: sortOptions(order)}

	var problem string
	rows, err := p.deps.Store.Roster(ctx, db.RosterQuery{
		Order: order, After: cursor, Limit: db.PageSize + 1,
	})
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the roster could not be read", slog.Any("error", err))
		problem = "The roster could not be read — the database did not answer."
	}

	pg := pager[db.RosterEntry]{base: DriversPath, query: url.Values{paramSort: {string(order)}}}
	kept, links := pg.page(rows, db.PageSize, cursor == nil, func(e db.RosterEntry) url.Values {
		return rosterCursorParams(order, e.Cursor())
	})
	form.Links = links
	for i := range kept {
		form.Rows = append(form.Rows, driverRowOf(now, kept[i]))
	}

	p.render(w, r, "drivers", pageData{
		Title:    "Drivers",
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Drivers"),
		Error:    problem,
		Form:     form,
	})
}

// getDriver opens one driver.
func (p *Panel) getDriver(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Problem(w, r, http.StatusNotFound, "There is no driver with that identifier on this server.")
		return
	}
	p.renderDriver(w, r, sess, id, "", "", http.StatusOK)
}

// renderDriver builds one driver's page. It is separate from the handler
// because the revoke buttons on it answer here.
func (p *Panel) renderDriver(
	w http.ResponseWriter, r *http.Request, sess db.AdminSession,
	id int64, notice, problem string, status int,
) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()
	q := r.URL.Query()

	entry, err := p.deps.Store.RosterEntryByID(ctx, id)
	if err != nil {
		httpx.Problem(w, r, http.StatusNotFound, "There is no driver with that identifier on this server.")
		return
	}

	form := driverForm{
		ID:            entry.ID,
		Name:          entry.Name,
		Slug:          entry.Slug,
		Class:         entry.Class,
		Paired:        momentAt(now, entry.CreatedAt),
		LastSeen:      momentOf(now, entry.LastSeenAt),
		LastUpload:    momentOf(now, entry.LastUploadAt),
		LapCount:      entry.Laps,
		StintCount:    entry.Stints,
		TraceBytes:    entry.TraceBytes,
		LiveDevices:   entry.Devices,
		RevokeAllHref: revokeAllHref(entry.ID),
		Back:          backDriver,
	}

	devices, err := p.deps.Store.PanelDevicesForDriver(ctx, id, db.PageSize)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's devices could not be read", slog.Any("error", err))
		if problem == "" {
			problem = "This driver's machines could not be read — the database did not answer."
		}
	}
	for _, d := range devices {
		form.Devices = append(form.Devices, deviceRowOf(now, d))
	}

	form.SignoutHref = SessionsRevokePath
	form.SignoutAllRef = SessionsRevokePath + "-all"
	sessions, err := p.deps.Store.DriverSessionsForDriver(ctx, id)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's sessions could not be read", slog.Any("error", err))
		if problem == "" {
			problem = "This driver's signed-in browsers could not be read — the database did not answer."
		}
	}
	for _, s := range sessions {
		form.Sessions = append(form.Sessions, driverSessionRowOf(now, s))
	}

	cursor := stintCursorOf(q)
	form.OnFirstPage = cursor == nil
	stints, err := p.deps.Store.StintsForDriver(ctx, db.StintQuery{
		DriverID: id, After: cursor, Limit: db.PageSize + 1,
	})
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "that driver's stints could not be read", slog.Any("error", err))
		if problem == "" {
			problem = "This driver's stints could not be read — the database did not answer."
		}
	}

	pg := pager[db.StintRow]{base: driverHref(id)}
	kept, links := pg.page(stints, db.PageSize, cursor == nil, func(s db.StintRow) url.Values {
		c := s.Cursor()
		return url.Values{paramAfter: {c.ID.String()}, paramFrom: {stamp(c.StartedAt)}}
	})
	form.Links = links
	for i := range kept {
		form.Recent = append(form.Recent, stintRowOf(now, kept[i]))
	}

	p.render(w, r, "driver", pageData{
		Title:    entry.Name,
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Drivers"),
		Notice:   notice,
		Error:    problem,
		Status:   status,
		Form:     form,
	})
}

// getStint opens one stint and lists its laps.
func (p *Panel) getStint(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)
	now := p.deps.Now()

	id, err := db.ParseUUID(r.PathValue("id"))
	if err != nil {
		httpx.Problem(w, r, http.StatusNotFound, "There is no stint with that identifier on this server.")
		return
	}
	stint, err := p.deps.Store.PanelStintByID(ctx, id)
	if err != nil {
		httpx.Problem(w, r, http.StatusNotFound, "There is no stint with that identifier on this server.")
		return
	}

	form := stintForm{
		ID:          stint.ID.String(),
		DriverID:    stint.DriverID,
		DriverName:  stint.DriverName,
		DriverHref:  driverHref(stint.DriverID),
		Sim:         stint.Sim,
		Track:       stint.Track,
		TrackID:     stint.TrackID,
		Car:         stint.Car,
		CarClass:    stint.CarClass,
		SessionType: stint.SessionType,
		Started:     momentAt(now, stint.StartedAt),
		LapCount:    stint.Laps,
		BestLapMs:   stint.BestLapMs,
		TraceBytes:  stint.TraceBytes,
	}
	if stint.FinishedAt != nil {
		form.Finished = momentOf(now, stint.FinishedAt)
		form.IsFinished = true
		form.Ran = stint.FinishedAt.Sub(stint.StartedAt)
	}

	after := lapCursorOf(r.URL.Query())
	form.OnFirstPage = after == db.FirstLapCursor

	var problem string
	laps, err := p.deps.Store.LapsForStint(ctx, id, after, db.PageSize+1)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the laps could not be read", slog.Any("error", err))
		problem = "The laps of this stint could not be read — the database did not answer."
	}

	pg := pager[db.PanelLap]{base: StintsPath + "/" + form.ID}
	kept, links := pg.page(laps, db.PageSize, form.OnFirstPage, func(l db.PanelLap) url.Values {
		return url.Values{paramAfter: {strconv.Itoa(l.Number)}}
	})
	form.Links = links
	for _, l := range kept {
		form.Laps = append(form.Laps, lapRowOf(now, l))
	}

	p.render(w, r, "stint", pageData{
		Title:    stint.Track,
		SignedIn: true,
		Email:    sess.Email,
		CSRF:     csrf,
		Nav:      navFor("Drivers"),
		Error:    problem,
		Form:     form,
	})
}

// sortOptions are the roster's orderings with the current one marked.
func sortOptions(current db.RosterOrder) []sortOption {
	orders := []struct {
		name  string
		order db.RosterOrder
	}{
		{"Name", db.RosterByName},
		{"Last seen", db.RosterByLastSeen},
		{"Laps", db.RosterByLaps},
	}
	out := make([]sortOption, 0, len(orders))
	for _, o := range orders {
		out = append(out, sortOption{
			Name: o.name,
			Href: DriversPath + "?" + url.Values{paramSort: {string(o.order)}}.Encode(),
			On:   o.order == current,
		})
	}
	return out
}

// rosterCursorParams renders a cursor as the parameters that resume after it.
// Only the key belonging to the current ordering is written, because that is
// the only one the query on the other side reads.
func rosterCursorParams(order db.RosterOrder, c db.RosterCursor) url.Values {
	v := url.Values{paramAfter: {itoa(c.ID)}}
	switch order {
	case db.RosterByLastSeen:
		v.Set(paramFrom, stamp(c.Seen))
	case db.RosterByLaps:
		v.Set(paramFrom, itoa(c.Laps))
	case db.RosterByName:
		v.Set(paramFrom, c.Name)
	default:
		v.Set(paramFrom, c.Name)
	}
	return v
}

func driverRowOf(now time.Time, e db.RosterEntry) driverRow {
	return driverRow{
		ID:             e.ID,
		Name:           e.Name,
		Class:          e.Class,
		Href:           driverHref(e.ID),
		LastSeen:       momentOf(now, e.LastSeenAt),
		LastUpload:     momentOf(now, e.LastUploadAt),
		Laps:           e.Laps,
		Stints:         e.Stints,
		TraceBytes:     e.TraceBytes,
		Devices:        e.Devices,
		RevokedDevices: e.RevokedDevices,
	}
}

func stintRowOf(now time.Time, s db.StintRow) stintRow {
	row := stintRow{
		ID:          s.ID.String(),
		Href:        StintsPath + "/" + s.ID.String(),
		Track:       s.Track,
		Car:         s.Car,
		CarClass:    s.CarClass,
		SessionType: s.SessionType,
		Started:     momentAt(now, s.StartedAt),
		Laps:        s.Laps,
		BestLapMs:   s.BestLapMs,
		TraceBytes:  s.TraceBytes,
	}
	if s.FinishedAt != nil {
		row.Finished = true
		row.Ran = s.FinishedAt.Sub(s.StartedAt)
	}
	return row
}

func lapRowOf(now time.Time, l db.PanelLap) lapRow {
	return lapRow{
		Number:     l.Number,
		LapMs:      l.LapMs,
		Kind:       l.Kind,
		Clean:      l.Kind == "clean",
		Started:    momentAt(now, l.StartedAt),
		TraceBytes: int64(l.TraceBytes),
		HasTrace:   l.TraceBytes > 0,
	}
}
