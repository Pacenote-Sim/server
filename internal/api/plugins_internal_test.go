package api

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// The facts a plugin is given are assembled here, and the assembly is where a
// coach ends up telling a driver something that is not true. These are the
// arithmetic and the honesty: what is computed, and what is deliberately left
// empty rather than guessed at.

func testStint(t *testing.T) db.Stint {
	t.Helper()
	id, err := db.ParseUUID("018f3b2a-0000-7000-8000-000000000001")
	require.NoError(t, err)
	return db.Stint{
		ID:          id,
		DriverID:    7,
		Sim:         "iracing",
		TrackID:     "barcelona gp",
		Track:       "Circuit de Barcelona-Catalunya",
		Car:         "Ferrari 296 GT3",
		CarClass:    "GT3",
		SessionType: "practice",
		StartedAt:   time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC),
	}
}

func testSession() session {
	return session{driver: db.Driver{ID: 7, Slug: "ana", Name: "Ana Ruiz"}}
}

// lapRow is a stored lap with no corner analysis, as encodeLaps writes one.
func lapRow(number, lapMs int, kind wire.Kind) db.LapRow {
	return db.LapRow{Number: number, LapMs: lapMs, Kind: string(kind), Corners: []byte("[]")}
}

// TestLapEventFacts covers the delta and the reference, which are the two
// numbers a cue is built on.
func TestLapEventFacts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		row              db.LapRow
		best             int
		wantDelta        int
		wantReference    string
		wantPersonalBest bool
	}{
		{
			name:          "a lap slower than the stint's best",
			row:           lapRow(14, 91240, wire.KindClean),
			best:          90400,
			wantDelta:     840,
			wantReference: "your best lap of this stint",
		},
		{
			name:             "the best lap of the stint",
			row:              lapRow(15, 90400, wire.KindClean),
			best:             90400,
			wantDelta:        0,
			wantReference:    "your best lap of this stint",
			wantPersonalBest: true,
		},
		{
			name:          "a lap that is not clean is never a personal best",
			row:           lapRow(16, 89000, wire.KindIn),
			best:          90400,
			wantDelta:     -1400,
			wantReference: "your best lap of this stint",
		},
		{
			name: "the first lap of a stint, with nothing to compare against",
			row:  lapRow(1, 93000, wire.KindClean),
			best: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), tc.row, tc.best)
			r.NoError(e.Validate())
			r.Equal(plugin.EventLapCompleted, e.Kind)
			r.NotEmpty(e.ID, "a plugin deduplicates on it")
			r.Equal("ana", e.Driver.Slug)
			r.Equal("iracing", e.Session.Sim)
			r.Equal(plugin.LapKind(tc.row.Kind), e.Lap.Kind)

			r.Equal(tc.wantDelta, e.Lap.DeltaMs)
			r.Equal(tc.wantReference, e.Lap.Reference)
			r.Equal(tc.wantPersonalBest, e.Lap.PersonalBest)
			r.Empty(e.Lap.Corners, "a lap that arrived with no corner analysis gets none invented for it")
			r.Nil(e.Lap.Position, "and no race picture either")
		})
	}
}

// TestLapEventsOnlyForLapsThatWereStored. A lap the stint already held happened
// once, so it is announced once — and the accepted laps are not necessarily the
// first few of the batch, which is the mistake this guards.
func TestLapEventsOnlyForLapsThatWereStored(t *testing.T) {
	t.Parallel()

	rows := []db.LapRow{
		lapRow(10, 92000, wire.KindClean),
		lapRow(11, 91500, wire.KindClean),
		lapRow(12, 91240, wire.KindClean),
	}

	cases := []struct {
		name   string
		result db.LapResult
		want   []int
	}{
		{
			name:   "a fresh batch produces one event each",
			result: db.LapResult{Accepted: 3, BestLapMs: 91240, Stored: []int{10, 11, 12}},
			want:   []int{10, 11, 12},
		},
		{
			name:   "a batch that repeats a lap already held announces only the new ones",
			result: db.LapResult{Accepted: 2, BestLapMs: 91240, Stored: []int{11, 12}},
			want:   []int{11, 12},
		},
		{
			name:   "a repeat in the middle does not shift the others",
			result: db.LapResult{Accepted: 2, BestLapMs: 91240, Stored: []int{10, 12}},
			want:   []int{10, 12},
		},
		{
			name:   "a batch that stored nothing announces nothing",
			result: db.LapResult{BestLapMs: 91240},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			events := lapEvents(slog.New(slog.DiscardHandler), testSession(), testStint(t), rows, tc.result)
			got := make([]int, 0, len(events))
			for i := range events {
				got = append(got, events[i].Lap.Number)
			}
			if tc.want == nil {
				r.Empty(got)
				return
			}
			r.Equal(tc.want, got)
		})
	}
}

// TestStintEventFacts covers the whole-stint facts: every one is a number the
// client reported, and the two this server used to work out — the fuel per lap
// and the spread across the tyres — are no longer here, because a plugin that
// wants them has the numbers and a server that computes them for one plugin is
// computing them for every plugin.
func TestStintEventFacts(t *testing.T) {
	t.Parallel()

	finished := time.Date(2026, 9, 13, 18, 45, 0, 0, time.UTC)
	body := wire.Summary{
		Laps: 12, Incidents: 1, BestLapMs: 90400, AvgLapMs: 91800,
		ConsistencyPct: 94, TopSpeedKmh: 271,
		Conditions: wire.Conditions{TrackTempC: 38.5, AirTempC: 26},
		CarState: wire.CarState{
			FuelUsedL: 36, FuelLevelL: 12,
			TyreTempC: wire.TyreTemps{LF: 92, RF: 101, LR: 88, RR: 90},
		},
		FinishedAt: &finished,
	}

	r := require.New(t)
	e := stintEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), body)
	r.NoError(e.Validate())
	r.Equal(plugin.EventStintFinished, e.Kind)
	// Where and what, in the client's own words: a debrief that cannot say
	// which circuit, or a radio that cannot tell a race from practice, is a
	// plugin the server left guessing.
	r.Equal("Circuit de Barcelona-Catalunya", e.Session.Track)
	r.Equal("barcelona gp", e.Session.TrackID)
	r.Equal(plugin.SessionPractice, e.Session.Type)
	r.Equal(94, e.Stint.ConsistencyPct)
	r.InDelta(36.0, e.Stint.Fuel.UsedL, 0.001)
	r.InDelta(12.0, e.Stint.Fuel.RemainingL, 0.001)
	r.Equal(plugin.TyreSummary{LF: 92, RF: 101, LR: 88, RR: 90}, e.Stint.Tyres,
		"the four temperatures as reported, and nothing derived from them")
	r.Equal(finished, e.Stint.FinishedAt)
	r.Zero(e.Stint.CleanLaps, "the summary does not carry it, so the plugin is told nothing rather than something false")
	r.Empty(e.Stint.Setup, "no setup was stored, so none is handed over")
}
