//go:build postgres

package admin_test

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

func TestDriversPage(t *testing.T) {
	t.Parallel()

	t.Run("an empty roster says how a driver gets here", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/drivers")
		r.Contains(body, "Nobody yet")
		r.Contains(body, "the first time you approve their pairing")
		r.Contains(body, "Go to pairing", "the empty state points at the page that fills it")
	})

	t.Run("a driver shows their laps, stints and storage", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "GT3")
		_, device := p.seedDevice(driver.ID, "laptop")
		p.seedStint(driver.ID, device.ID, "Jerez", "Porsche 992 GT3 R", time.Now().Add(-2*time.Hour), []seedLap{
			{Number: 1, LapMs: 95_400, Trace: traceOf(1500)},
			{Number: 2, LapMs: 94_900, Trace: traceOf(1600)},
		})

		body := p.get("/admin/drivers")
		r.Contains(body, "Marta Ferrer")
		r.Contains(body, "GT3")
		r.Contains(body, "Last seen")
		r.Contains(body, "Last upload")
		r.Contains(body, "3.0 KiB", "the two traces are summed into one storage figure")
	})

	t.Run("the three orderings are offered and each renders", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			sort string
		}{
			{"by name", "name"},
			{"by last seen", "seen"},
			{"by laps", "laps"},
			{"an ordering that does not exist falls back to name", "sideways"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				p := newPanel(t)
				driver := p.seedDriver("Ana Ruiz", "")
				_, device := p.seedDevice(driver.ID, "desktop")
				p.seedStint(driver.ID, device.ID, "Jerez", "Cup", time.Now().Add(-time.Hour), []seedLap{
					{Number: 1, LapMs: 90_000, Trace: traceOf(1000)},
				})

				body := p.get("/admin/drivers?sort=" + tc.sort)
				r.Contains(body, "Ana Ruiz")
				r.Contains(body, "sort=laps", "every ordering is reachable from every other")
			})
		}
	})

	t.Run("ordering by laps puts the busiest driver first", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		quiet := p.seedDriver("Zoe Quiet", "")
		busy := p.seedDriver("Al Busy", "")
		_, quietDevice := p.seedDevice(quiet.ID, "one")
		_, busyDevice := p.seedDevice(busy.ID, "two")
		p.seedStint(quiet.ID, quietDevice.ID, "Jerez", "Cup", time.Now().Add(-time.Hour), []seedLap{
			{Number: 1, LapMs: 90_000, Trace: traceOf(100)},
		})
		p.seedStint(busy.ID, busyDevice.ID, "Jerez", "Cup", time.Now().Add(-time.Hour), []seedLap{
			{Number: 1, LapMs: 90_000, Trace: traceOf(100)},
			{Number: 2, LapMs: 90_000, Trace: traceOf(100)},
			{Number: 3, LapMs: 90_000, Trace: traceOf(100)},
		})

		body := p.get("/admin/drivers?sort=laps")
		r.Less(strings.Index(body, "Al Busy"), strings.Index(body, "Zoe Quiet"),
			"three laps sorts above one, whatever the names do")
	})

	t.Run("the roster pages across a boundary without repeating or losing a driver", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		const total = db.PageSize + 4
		for i := range total {
			p.seedDriver(fmt.Sprintf("Driver %02d", i), "")
		}

		first := p.get("/admin/drivers")
		r.Contains(first, "Next page", "a roster longer than a page offers the next one")
		r.Contains(first, "Driver 00")
		r.NotContains(first, "Driver 25", "the page stops at its size")

		next := p.get(hrefAfter(t, first, "Next page"))
		r.Contains(next, "Driver 25")
		r.Contains(next, "Driver 28")
		r.NotContains(next, "Driver 24", "the second page starts after the first ends")
		r.NotContains(next, "Next page", "four rows is the end of it")
		r.Contains(next, "Back to the first page")
	})

	t.Run("a driver opens to their machines and their stints", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "GT3")
		_, device := p.seedDevice(driver.ID, "race laptop")
		p.seedStint(driver.ID, device.ID, "Circuit de Barcelona", "Porsche 992 GT3 R",
			time.Now().Add(-90*time.Minute), []seedLap{
				{Number: 1, LapMs: 105_200, Trace: traceOf(2000)},
				{Number: 2, LapMs: 104_100, Trace: traceOf(2000)},
			})

		body := p.get(fmt.Sprintf("/admin/drivers/%d", driver.ID))
		r.Contains(body, "Marta Ferrer")
		r.Contains(body, "race laptop")
		r.Contains(body, "Circuit de Barcelona")
		r.Contains(body, "Porsche 992 GT3 R")
		r.Contains(body, "1:44.100", "the best clean lap is shown as a lap time")
		r.Contains(body, "Sign out", "a revoke button sits beside every machine")
		r.Contains(body, "Sign out every machine of Marta Ferrer")
	})

	t.Run("a driver with nothing recorded says so", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Nobody Yet", "")
		body := p.get(fmt.Sprintf("/admin/drivers/%d", driver.ID))
		r.Contains(body, "has no machine paired")
		r.Contains(body, "Nothing recorded yet")
	})

	t.Run("a driver that is not here is not found", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			path string
		}{
			{"an identifier with no driver", "/admin/drivers/99999"},
			{"an identifier that is not a number", "/admin/drivers/marta"},
			{"a negative identifier", "/admin/drivers/-1"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				p := newPanel(t)
				status, _ := p.getStatus(tc.path)
				r.Equal(404, status)
			})
		}
	})

	t.Run("a stint opens to its laps and pages across a boundary", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")
		laps := make([]seedLap, 0, db.PageSize+5)
		for i := range db.PageSize + 5 {
			laps = append(laps, seedLap{Number: i, LapMs: 95_000 + i, Trace: traceOf(1200)})
		}
		stint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", time.Now().Add(-3*time.Hour), laps)

		first := p.get("/admin/stints/" + stint.String())
		r.Contains(first, "Jerez")
		r.Contains(first, "Laps")
		r.Contains(first, "Next page")

		next := p.get(hrefAfter(t, first, "Next page"))
		r.Contains(next, "Back to the first page")
		r.NotContains(next, "Next page", "the second page is the last one")
	})

	t.Run("a stint that is not here is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		status, _ := p.getStatus("/admin/stints/not-a-uuid")
		r.Equal(404, status)

		status, _ = p.getStatus("/admin/stints/" + seedUUID(t).String())
		r.Equal(404, status)
	})

	t.Run("a stint with no lap says what that means", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")
		stint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", time.Now().Add(-time.Hour), nil)

		body := p.get("/admin/stints/" + stint.String())
		r.Contains(body, "This stint holds no lap")
	})

	t.Run("the enterprise feature is shown and disabled rather than hidden", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/drivers")
		r.Contains(body, "Teams")
		r.Contains(body, "Part of Enterprise")
		r.Contains(body, "disabled")
	})

	t.Run("the page needs a session", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		p.signOut()

		for _, path := range []string{"/admin/drivers", "/admin/drivers/1", "/admin/stints/" + seedUUID(t).String()} {
			status, body := p.getStatus(path)
			r.Equal(303, status, "%s should send an anonymous visitor to sign in", path)
			r.NotContains(body, "Teams")
		}
	})
}

// hrefAfter pulls the address out of the link whose text is label. It is how
// these tests follow pagination the way an operator does — by pressing the link
// the page rendered, rather than by constructing a cursor the page might not
// agree with.
func hrefAfter(tb testing.TB, body, label string) string {
	tb.Helper()
	marker := ">" + label + "<"
	at := strings.Index(body, marker)
	require.GreaterOrEqual(tb, at, 0, "no %q link on the page:\n%s", label, body)

	open := strings.LastIndex(body[:at], `href="`)
	require.GreaterOrEqual(tb, open, 0, "the %q link has no address", label)
	rest := body[open+len(`href="`):]
	end := strings.Index(rest, `"`)
	require.GreaterOrEqual(tb, end, 0)
	return html.UnescapeString(rest[:end])
}

// The roster past its first page, in each of the three orders it can be sorted
// by.
//
// The cursor is a different value in each — a name, a lap count, a timestamp —
// and only the one belonging to the current sort is written into the link. A
// page that carried the wrong one would resume from a position the query does
// not understand, which shows up as rows repeated or rows skipped rather than
// as an error, so nothing but walking the pages catches it.
func TestTheRosterPagesInEveryOrder(t *testing.T) {
	t.Parallel()
	p := newPanel(t)

	// One more driver than fits, each with a lap so the lap ordering has
	// something to order by.
	const total = db.PageSize + 3
	for i := range total {
		driver := p.seedDriver(fmt.Sprintf("Driver %02d", i), "gt3")
		_, device := p.seedDevice(driver.ID, "a sim rig")
		p.seedStint(driver.ID, device.ID, "Jerez", "Cup",
			time.Now().Add(-time.Duration(i+1)*time.Hour), []seedLap{
				{Number: 1, LapMs: 95_000 + i, Trace: traceOf(64)},
			})
	}

	for _, sort := range []string{"", "name", "laps", "seen"} {
		t.Run("sorted by "+sort, func(t *testing.T) {
			path := "/admin/drivers"
			if sort != "" {
				path += "?sort=" + sort
			}
			first := p.get(path)
			require.Contains(t, first, "Next page", "the roster did not offer a second page")

			next := p.get(hrefAfter(t, first, "Next page"))
			require.NotContains(t, next, "Next page", "there was a third page of %d drivers", total)
			require.Contains(t, next, "Back to the first page")

			// Every driver appears once across the two pages and none twice,
			// which is the property a wrong cursor breaks.
			seen := 0
			for i := range total {
				name := fmt.Sprintf("Driver %02d", i)
				on := strings.Count(first, name) + strings.Count(next, name)
				require.Equal(t, 1, on, "%s appeared %d times across the two pages", name, on)
				seen++
			}
			require.Equal(t, total, seen)

			// And the link back goes back.
			require.Contains(t, p.get(hrefAfter(t, next, "Back to the first page")), "Next page")
		})
	}
}

// One driver's stints past the first page. A driver who has been racing for a
// season has more than a page of them, and the page under the list is the only
// way to reach the older ones.
func TestADriversStintsPage(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	driver := p.seedDriver("Marta Ferrer", "gt3")
	_, device := p.seedDevice(driver.ID, "a sim rig")
	const total = db.PageSize + 2
	for i := range total {
		p.seedStint(driver.ID, device.ID, fmt.Sprintf("Track %02d", i), "Cup",
			time.Now().Add(-time.Duration(i+1)*time.Hour), []seedLap{
				{Number: 1, LapMs: 95_000, Trace: traceOf(64)},
			})
	}

	path := "/admin/drivers/" + strconv.FormatInt(driver.ID, 10)
	first := p.get(path)
	r.Contains(first, "Next page", "a driver with a season of stints had one page")

	next := p.get(hrefAfter(t, first, "Next page"))
	r.NotContains(next, "Next page")
	for i := range total {
		track := fmt.Sprintf("Track %02d", i)
		on := strings.Count(first, track) + strings.Count(next, track)
		r.Equal(1, on, "%s appeared %d times across the two pages", track, on)
	}
}
