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

// The facts a plugin is given are derived here, and the derivation is where a
// coach ends up telling a driver something that is not true. These are the
// arithmetic and the honesty: what is computed, and what is deliberately left
// empty rather than guessed at.

func testStint(t *testing.T) db.Stint {
	t.Helper()
	id, err := db.ParseUUID("018f3b2a-0000-7000-8000-000000000001")
	require.NoError(t, err)
	return db.Stint{
		ID:        id,
		DriverID:  7,
		Sim:       "iracing",
		TrackID:   "barcelona gp",
		Car:       "Ferrari 296 GT3",
		CarClass:  "GT3",
		StartedAt: time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC),
	}
}

func testSession() session {
	return session{driver: db.Driver{ID: 7, Slug: "ana", Name: "Ana Ruiz"}}
}

// TestLapEventFacts covers the delta and the reference, which are the two
// numbers a cue is built on.
func TestLapEventFacts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		lap              wire.Lap
		best             int
		wantDelta        int
		wantReference    string
		wantPersonalBest bool
	}{
		{
			name:          "a lap slower than the stint's best",
			lap:           wire.Lap{Number: 14, LapMs: 91240, Kind: wire.KindClean},
			best:          90400,
			wantDelta:     840,
			wantReference: "your best lap of this stint",
		},
		{
			name:             "the best lap of the stint",
			lap:              wire.Lap{Number: 15, LapMs: 90400, Kind: wire.KindClean},
			best:             90400,
			wantDelta:        0,
			wantReference:    "your best lap of this stint",
			wantPersonalBest: true,
		},
		{
			name:          "a lap that is not clean is never a personal best",
			lap:           wire.Lap{Number: 16, LapMs: 89000, Kind: wire.KindIn},
			best:          90400,
			wantDelta:     -1400,
			wantReference: "your best lap of this stint",
		},
		{
			name: "the first lap of a stint, with nothing to compare against",
			lap:  wire.Lap{Number: 1, LapMs: 93000, Kind: wire.KindClean},
			best: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), tc.lap, tc.best)
			r.NoError(e.Validate())
			r.Equal(plugin.EventLapCompleted, e.Kind)
			r.NotEmpty(e.ID, "a plugin deduplicates on it")
			r.Equal("ana", e.Driver.Slug)
			r.Equal("iracing", e.Session.Sim)

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

	rows := []wire.Lap{
		{Number: 10, LapMs: 92000, Kind: wire.KindClean},
		{Number: 11, LapMs: 91500, Kind: wire.KindClean},
		{Number: 12, LapMs: 91240, Kind: wire.KindClean},
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

// TestStintEventFacts covers the whole-stint arithmetic: the spread that says
// whether one corner of the car is working alone, and the fuel figure a
// strategy is built on.
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
	r.Equal(94, e.Stint.ConsistencyPct)
	r.InDelta(13.0, e.Stint.Tyres.SpreadC, 0.001, "the hottest minus the coldest")
	r.InDelta(3.0, e.Stint.Fuel.PerLapL, 0.001)
	r.InDelta(12.0, e.Stint.Fuel.RemainingL, 0.001)
	r.Equal(finished, e.Stint.FinishedAt)
	r.Zero(e.Stint.CleanLaps, "the summary does not carry it, so the plugin is told nothing rather than something false")
}

// TestPerLapAndSpread covers the two pieces of arithmetic on their own,
// including the divisions nobody expects.
func TestPerLapAndSpread(t *testing.T) {
	t.Parallel()

	t.Run("fuel over no laps is zero, not a division by zero", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		r.InDelta(0.0, perLap(36, 0), 0.001)
		r.InDelta(0.0, perLap(36, -1), 0.001)
	})

	t.Run("the spread of four equal temperatures is nothing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		r.InDelta(0.0, spread(wire.TyreTemps{LF: 90, RF: 90, LR: 90, RR: 90}), 0.001)
	})

	t.Run("the spread finds the hottest and the coldest wherever they are", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		r.InDelta(20.0, spread(wire.TyreTemps{LF: 80, RF: 90, LR: 100, RR: 85}), 0.001)
		r.InDelta(20.0, spread(wire.TyreTemps{LF: 100, RF: 90, LR: 80, RR: 85}), 0.001)
	})
}
