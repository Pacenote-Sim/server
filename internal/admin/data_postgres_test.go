//go:build postgres

package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/db"
)

func TestDataPage(t *testing.T) {
	t.Parallel()

	t.Run("an empty server says what will appear here", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/data")
		r.Contains(body, "Nothing is stored yet")
		r.Contains(body, "uploads laps as they drive")
		r.Contains(body, "Retention is set to keep every trace",
			"a fresh install keeps everything until the operator says otherwise")
	})

	t.Run("what is stored is measured and the traces are named as the bulk", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")
		p.seedStint(driver.ID, device.ID, "Jerez", "Cup", time.Now().Add(-2*time.Hour), []seedLap{
			{Number: 1, LapMs: 95_000, Trace: traceOf(2048)},
			{Number: 2, LapMs: 94_000, Trace: traceOf(2048)},
		})

		body := p.get("/admin/data")
		r.Contains(body, "Lap traces")
		r.Contains(body, "4.0 KiB", "the two traces are summed")
		r.Contains(body, "Storage per driver")
		r.Contains(body, "Marta Ferrer")
		r.Contains(body, "Oldest record")
	})

	t.Run("retention saves, records the change, and deletes nothing by saving", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")
		old := time.Now().AddDate(0, -4, 0)
		stint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", old, []seedLap{
			{Number: 1, LapMs: 95_000, StartedAt: old, Trace: traceOf(1024)},
		})

		page := p.get("/admin/data")
		status := p.post("/admin/data/retention", url.Values{
			"csrf": {csrfOf(t, page)}, "trace_months": {"1"},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "nothing has been deleted")
		r.Equal(1, p.auditCount(db.ActionRetentionChanged), "one audit row per change")
		r.Equal(1024, p.traceBytesOf(stint), "saving a setting is not a deletion")
	})

	t.Run("retention refuses a length it cannot keep", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name  string
			value string
			says  string
		}{
			{"not a number", "soon", "a number of months"},
			{"negative", "-3", "fewer than zero months"},
			{"longer than this server sets", "999", "the longest this server sets"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				p := newPanel(t)
				page := p.get("/admin/data")
				status := p.post("/admin/data/retention", url.Values{
					"csrf": {csrfOf(t, page)}, "trace_months": {tc.value},
				})
				r.Equal(http.StatusUnprocessableEntity, status)
				r.Contains(string(p.lastBody), tc.says)
				r.Zero(p.auditCount(db.ActionRetentionChanged), "a refusal is not a change")
			})
		}
	})

	t.Run("the preview says exactly what the prune then deletes", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")

		old := time.Now().AddDate(0, -4, 0)
		oldLaps := make([]seedLap, 0, 6)
		for i := range 6 {
			oldLaps = append(oldLaps, seedLap{
				Number: i, LapMs: 95_000 + i, StartedAt: old.Add(time.Duration(i) * time.Minute),
				Trace: traceOf(1024),
			})
		}
		oldStint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", old, oldLaps)

		recent := time.Now().Add(-2 * time.Hour)
		recentStint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", recent, []seedLap{
			{Number: 1, LapMs: 93_000, StartedAt: recent, Trace: traceOf(2048)},
		})

		p.setRetention(1)

		// What the page promises, before anything is pressed.
		preview, err := p.store.TracesOlderThan(context.Background(), time.Now().AddDate(0, -1, 0))
		r.NoError(err)
		r.EqualValues(6, preview.Laps)
		r.EqualValues(6*1024, preview.TraceBytes)

		body := p.get("/admin/data")
		r.Contains(body, "This would clear the traces of")
		r.Contains(body, "6 laps")
		r.Contains(body, "6.0 KiB")
		r.Contains(body, "Type prune to confirm")

		timesBefore := p.lapTimesOf(oldStint)

		page := p.get("/admin/data")
		status := p.post("/admin/data/prune", url.Values{
			"csrf": {csrfOf(t, page)}, "confirm": {admin.PruneConfirmation},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Pruning 6 lap traces")
		r.Equal(1, p.auditCount(db.ActionTracesPruned))

		done := p.awaitPrune()
		r.Empty(done.Err, "the job should finish cleanly")
		r.EqualValues(6, done.Done, "it cleared exactly what the preview promised")

		r.Zero(p.traceBytesOf(oldStint), "every trace past the line is gone")
		r.Equal(2048, p.traceBytesOf(recentStint), "a trace inside the line is untouched")
		r.Equal(timesBefore, p.lapTimesOf(oldStint),
			"a pruned trace leaves its lap and its time exactly as they were")
		r.Len(timesBefore, 6, "the laps themselves are all still there")

		after := p.get("/admin/data")
		r.Contains(after, "Nothing to prune", "a second prune has nothing left to do")
		r.Contains(after, "keep their time but no trace")
	})

	t.Run("a prune without the typed word deletes nothing", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			confirm string
		}{
			{"nothing typed", ""},
			{"the wrong word", "yes"},
			{"the right word with other text", "prune it all"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				p := newPanel(t)

				driver := p.seedDriver("Marta Ferrer", "")
				_, device := p.seedDevice(driver.ID, "laptop")
				old := time.Now().AddDate(0, -4, 0)
				stint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", old, []seedLap{
					{Number: 1, LapMs: 95_000, StartedAt: old, Trace: traceOf(1024)},
				})
				p.setRetention(1)

				page := p.get("/admin/data")
				status := p.post("/admin/data/prune", url.Values{
					"csrf": {csrfOf(t, page)}, "confirm": {tc.confirm},
				})
				r.Equal(http.StatusUnprocessableEntity, status)
				r.Contains(string(p.lastBody), "Type prune to confirm")
				r.Equal(1024, p.traceBytesOf(stint), "nothing was deleted")
				r.Zero(p.auditCount(db.ActionTracesPruned))
			})
		}
	})

	t.Run("a prune with retention set to keep everything is refused", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		page := p.get("/admin/data")
		status := p.post("/admin/data/prune", url.Values{
			"csrf": {csrfOf(t, page)}, "confirm": {admin.PruneConfirmation},
		})
		r.Equal(http.StatusConflict, status)
		r.Contains(string(p.lastBody), "Set a number of months first")
	})

	t.Run("a prune with nothing old enough says so rather than starting", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")
		recent := time.Now().Add(-time.Hour)
		p.seedStint(driver.ID, device.ID, "Jerez", "Cup", recent, []seedLap{
			{Number: 1, LapMs: 95_000, StartedAt: recent, Trace: traceOf(1024)},
		})
		p.setRetention(12)

		page := p.get("/admin/data")
		status := p.post("/admin/data/prune", url.Values{
			"csrf": {csrfOf(t, page)}, "confirm": {admin.PruneConfirmation},
		})
		r.Equal(http.StatusOK, status)
		r.Contains(string(p.lastBody), "Nothing to prune")
		r.Zero(p.auditCount(db.ActionTracesPruned), "a prune that did nothing is not a change")
	})

	t.Run("a pruned lap no longer stands as a reference the coach would fetch", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		driver := p.seedDriver("Marta Ferrer", "")
		_, device := p.seedDevice(driver.ID, "laptop")
		old := time.Now().AddDate(0, -4, 0)
		p.seedStint(driver.ID, device.ID, "Jerez", "Cup", old, []seedLap{
			{Number: 1, LapMs: 95_000, StartedAt: old, Trace: traceOf(1024)},
		})

		_, err := p.store.ReferenceLap(context.Background(), db.ReferenceQuery{
			DriverID: driver.ID, Sim: seedSim, TrackID: "Jerez", Car: "Cup",
			Preference: []wire.Scope{wire.ScopeSelf},
		})
		r.NoError(err, "the lap is the driver's own best before anything is pruned")

		p.setRetention(1)
		page := p.get("/admin/data")
		r.Equal(http.StatusOK, p.post("/admin/data/prune", url.Values{
			"csrf": {csrfOf(t, page)}, "confirm": {admin.PruneConfirmation},
		}))
		p.awaitPrune()

		_, err = p.store.ReferenceLap(context.Background(), db.ReferenceQuery{
			DriverID: driver.ID, Sim: seedSim, TrackID: "Jerez", Car: "Cup",
			Preference: []wire.Scope{wire.ScopeSelf},
		})
		r.ErrorIs(err, db.ErrNotFound,
			"a reference pointing at a cleared trace would be a server error on the next coaching call")
	})

	t.Run("the enterprise feature is shown and disabled rather than hidden", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		body := p.get("/admin/data")
		r.Contains(body, "Scheduled export")
		r.Contains(body, "Part of Enterprise")
	})

	t.Run("the page needs a session", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)
		p.signOut()

		status, _ := p.getStatus("/admin/data")
		r.Equal(http.StatusSeeOther, status)
	})
}

// The answers the retention form gives that are not "saved".
//
// Each one is a different thing for the operator to understand: nothing needed
// doing, everything is kept from now on, or somebody is already clearing. A
// form that said "Saved" to all three would be lying about two of them.
func TestWhatTheRetentionFormSaysWhenItIsNotSaving(t *testing.T) {
	t.Parallel()

	t.Run("a setting that is already what is stored", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		page := p.get("/admin/data")
		r.Equal(http.StatusOK, p.post("/admin/data/retention", url.Values{
			"csrf": {csrfOf(t, page)}, "trace_months": {"6"},
		}))
		page = p.get("/admin/data")
		r.Equal(http.StatusOK, p.post("/admin/data/retention", url.Values{
			"csrf": {csrfOf(t, page)}, "trace_months": {"6"},
		}))
		r.Contains(string(p.lastBody), "Nothing to save")
	})

	t.Run("keeping everything for ever", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		p := newPanel(t)

		page := p.get("/admin/data")
		r.Equal(http.StatusOK, p.post("/admin/data/retention", url.Values{
			"csrf": {csrfOf(t, page)}, "trace_months": {"6"},
		}))
		page = p.get("/admin/data")
		r.Equal(http.StatusOK, p.post("/admin/data/retention", url.Values{
			"csrf": {csrfOf(t, page)}, "trace_months": {"0"},
		}))
		r.Contains(string(p.lastBody), "kept for ever")
		r.Contains(string(p.lastBody), "nothing for a prune to clear")
	})
}

// A driver who has paired and never driven does not take up a row in the
// storage table. The table is about where the disk went, and a driver using
// none of it is noise on a page an operator reads to find the one using most.
func TestADriverWithNothingStoredIsNotListedAsStorage(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	p := newPanel(t)

	driven := p.seedDriver("Marta Ferrer", "")
	_, device := p.seedDevice(driven.ID, "laptop")
	p.seedStint(driven.ID, device.ID, "Jerez", "Cup", time.Now().Add(-time.Hour), []seedLap{
		{Number: 1, LapMs: 95_000, Trace: traceOf(2048)},
	})
	p.seedDriver("Ana Ruiz", "")

	body := p.get("/admin/data")
	r.Contains(body, "Marta Ferrer")
	r.NotContains(body, "Ana Ruiz", "a driver storing nothing was listed as storage")
}
