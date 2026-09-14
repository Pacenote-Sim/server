package admin

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pacenote-sim/server/internal/db"
)

// moment is an instant the panel shows twice: as a clock time, and as how long
// ago it was.
//
// Both are on the page because they answer different questions. "Is Marta's
// client working" is answered by "four minutes ago" without any arithmetic;
// "what happened at 14:02" is answered by the clock time, next to a log file.
// Known is false for a thing that has never happened, which the page says
// rather than printing an epoch.
type moment struct {
	At    time.Time
	Ago   time.Duration
	Known bool
}

// momentOf builds a moment from an instant that may not have happened.
func momentOf(now time.Time, at *time.Time) moment {
	if at == nil || at.IsZero() {
		return moment{}
	}
	return moment{At: *at, Ago: now.Sub(*at), Known: true}
}

// momentAt builds a moment from an instant that is known to have happened, with
// the zero time still meaning "never" — a database column that was never
// written arrives that way.
func momentAt(now, at time.Time) moment {
	if at.IsZero() {
		return moment{}
	}
	return moment{At: at, Ago: now.Sub(at), Known: true}
}

// Future reports whether this moment has not happened yet.
//
// Almost nothing the panel shows is in the future — a page about a server is a
// page about what has already happened — and the one thing that is, the day a
// certificate expires, is the one an operator most needs read the right way
// round. "In three weeks" and "three weeks ago" are opposite answers to the
// question of whether they have to do something today.
func (m moment) Future() bool { return m.Known && m.Ago < 0 }

// Until is how long there is to go, for a moment that has not happened yet.
func (m moment) Until() time.Duration { return -m.Ago }

// Stale reports whether this moment is old enough to be worth pointing at. It
// is used on the roster, where the point of the page is to notice the driver
// whose client stopped talking without anybody saying so.
func (m moment) Stale() bool { return m.Known && m.Ago > staleAfter }

// staleAfter is how long a machine can go without connecting before the roster
// marks it. A week covers a driver who races at weekends and does not fire on
// somebody who simply did not drive on Tuesday.
const staleAfter = 7 * 24 * time.Hour

// pageLinks is the navigation under a list: where the next page is, and where
// the first one is so that an operator six pages in can get back.
//
// There is no "previous". Keyset pagination walks one way — the cursor is the
// last row rendered, and a backwards cursor would be a second set of queries
// for a link nobody uses when the browser's own back button is right there.
type pageLinks struct {
	// Next is the address of the page after this one, or empty when this is
	// the last page.
	Next string
	// First is the address of the first page, or empty when this is it.
	First string
}

// pager turns one over-read page of rows into the rows to render and the links
// under them.
//
// The over-read is the whole trick: the query asks for one row more than the
// page shows, and whether that row came back is how the page knows there is a
// next one without counting the table.
type pager[T any] struct {
	// base is the page's own path, and query the parameters to keep on the
	// next link — the sort order, and nothing else.
	base  string
	query url.Values
}

// page trims an over-read slice to the page size and builds the links. cursor
// renders the last row of the page as the parameters that resume after it.
func (p pager[T]) page(rows []T, size int, onFirst bool, cursor func(T) url.Values) ([]T, pageLinks) {
	links := pageLinks{}
	if !onFirst {
		links.First = p.href(nil)
	}
	if len(rows) > size {
		rows = rows[:size]
		links.Next = p.href(cursor(rows[len(rows)-1]))
	}
	return rows, links
}

// href builds one of the links, carrying the kept parameters and adding the
// cursor's.
func (p pager[T]) href(cursor url.Values) string {
	q := url.Values{}
	for k, v := range p.query {
		q[k] = v
	}
	for k, v := range cursor {
		q[k] = v
	}
	if len(q) == 0 {
		return p.base
	}
	return p.base + "?" + q.Encode()
}

// The query parameters a cursor travels in. They are two rather than one
// because a keyset cursor is a sort key and a tie-break, and spelling them out
// keeps the address readable by the operator who is looking at it.
const (
	// paramAfter carries the identifier of the last row of the previous page.
	paramAfter = "after"
	// paramFrom carries that row's sort key.
	paramFrom = "from"
	// paramSort carries which ordering a list is in.
	paramSort = "sort"
)

// rosterCursorOf reads the roster cursor out of a request's parameters, or
// reports that this is the first page.
//
// Anything unreadable is the first page rather than an error. A cursor is a
// position in a list, it arrives through an address bar that a person can edit,
// and starting again at the top is what they expect from a link that has gone
// stale — an error page would tell them nothing they can act on.
func rosterCursorOf(q url.Values, order db.RosterOrder) *db.RosterCursor {
	id, err := strconv.ParseInt(q.Get(paramAfter), 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	from := q.Get(paramFrom)
	cursor := db.RosterCursor{ID: id}
	switch order {
	case db.RosterByLastSeen:
		at, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return nil
		}
		cursor.Seen = at
	case db.RosterByLaps:
		laps, err := strconv.ParseInt(from, 10, 64)
		if err != nil {
			return nil
		}
		cursor.Laps = laps
	case db.RosterByName:
		cursor.Name = from
	default:
		cursor.Name = from
	}
	return &cursor
}

// stintCursorOf reads the cursor for a page of a driver's stints.
func stintCursorOf(q url.Values) *db.StintCursor {
	id, err := db.ParseUUID(q.Get(paramAfter))
	if err != nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, q.Get(paramFrom))
	if err != nil {
		return nil
	}
	return &db.StintCursor{StartedAt: at, ID: id}
}

// deviceCursorOf reads the cursor for a page of the devices list.
func deviceCursorOf(q url.Values) *db.DeviceCursor {
	id, err := strconv.ParseInt(q.Get(paramAfter), 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, q.Get(paramFrom))
	if err != nil {
		return nil
	}
	return &db.DeviceCursor{CreatedAt: at, ID: id}
}

// lapCursorOf reads the cursor for a page of a stint's laps, which is the last
// lap number rendered.
func lapCursorOf(q url.Values) int {
	n, err := strconv.Atoi(q.Get(paramAfter))
	if err != nil || n < 0 {
		return db.FirstLapCursor
	}
	return n
}

// orderOf reads which ordering a roster page is in, falling back to the
// alphabetical one that an operator looking somebody up wants.
func orderOf(q url.Values) db.RosterOrder {
	o := db.RosterOrder(strings.TrimSpace(q.Get(paramSort)))
	if !o.Valid() {
		return db.RosterByName
	}
	return o
}

// stamp renders an instant as a cursor carries it: to the nanosecond, so that
// two rows written in the same millisecond are still ordered by it.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// itoa is strconv.FormatInt at the width every identifier here has.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
