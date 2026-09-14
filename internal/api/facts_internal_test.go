package api

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// The two facts this server did not use to carry: where a lap was lost, and
// what the car was set to. Both are measured by the client and copied here
// rather than recomputed, so these tests are about the copy being exact — a
// fact that changes on the way through is worse than one that never arrives,
// because nothing downstream can tell.

// worstCorner is the corner analysis the client's own test builds, in the units
// the wire declares. Its twin is TestLapsToWire_CarriesTheCornerAnalysis in the
// telemetry client's internal/desktop, which asserts that its detector produces
// these numbers; this file asserts that a plugin is handed them unchanged. The
// two are written against the same figures on purpose, so that a change to
// either side shows up as a disagreement between them.
func worstCorner() wire.Corner {
	return wire.Corner{
		Turn: 3, ApexPct: 600, ApexKmh: 60, RefApexKmh: 80, DeficitKmh: 20,
		BrakeAtApex: 45, ThrottleLag: 40, Pattern: wire.PatternEarlyApex,
	}
}

// TestLapEvent_CarriesTheCorners is the server half of the round trip: what the
// client detected reaches plugin.LapFacts.Corners unchanged.
func TestLapEvent_CarriesTheCorners(t *testing.T) {
	t.Parallel()

	t.Run("every measurement survives the crossing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		sent := worstCorner()
		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), wire.Lap{
			Number: 7, LapMs: 91234, Kind: wire.KindClean,
			Corners: []wire.Corner{sent},
		}, 90400)

		r.NoError(e.Validate())
		r.Len(e.Lap.Corners, 1)

		got := e.Lap.Corners[0]
		r.Equal(sent.Turn, got.Turn)
		r.Equal(sent.ApexPct, got.ApexPct)
		r.Equal(sent.ApexKmh, got.ApexKmh)
		r.Equal(sent.RefApexKmh, got.ReferenceApexKmh)
		r.Equal(sent.DeficitKmh, got.DeficitKmh)
		r.Equal(sent.BrakeAtApex, got.BrakeAtApex)
		r.Equal(sent.ThrottleLag, got.ThrottleLag)
		r.Equal(plugin.PatternEarlyApex, got.Pattern)
		r.Equal(got.ReferenceApexKmh-got.ApexKmh, got.DeficitKmh,
			"the deficit is still the two speeds beside it after the crossing")
	})

	t.Run("the order the client ranked them in is kept", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), wire.Lap{
			Number: 7, LapMs: 91234, Kind: wire.KindClean,
			Corners: []wire.Corner{
				{Turn: 3, ApexPct: 600, ApexKmh: 60, RefApexKmh: 80, DeficitKmh: 20},
				{Turn: 1, ApexPct: 150, ApexKmh: 102, RefApexKmh: 110, DeficitKmh: 8},
				{Turn: 4, ApexPct: 800, ApexKmh: 146, RefApexKmh: 150, DeficitKmh: 4},
			},
		}, 90400)

		turns := []int{e.Lap.Corners[0].Turn, e.Lap.Corners[1].Turn, e.Lap.Corners[2].Turn}
		r.Equal([]int{3, 1, 4}, turns, "worst first is the client's ranking and this server does not re-sort it")
	})

	t.Run("a corner with nothing conclusive keeps its empty pattern", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), wire.Lap{
			Number: 2, LapMs: 92000, Kind: wire.KindClean,
			Corners: []wire.Corner{{Turn: 1, ApexPct: 150, ApexKmh: 102, RefApexKmh: 110, DeficitKmh: 8}},
		}, 90400)

		r.Empty(e.Lap.Corners[0].Pattern, "an empty pattern is an answer and must not become a guess")
	})

	t.Run("a lap that arrived with no corners is given none", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), wire.Lap{
			Number: 1, LapMs: 95000, Kind: wire.KindOut,
		}, 0)

		r.Nil(e.Lap.Corners)

		// The facts cross to a plugin as JSON, and a plugin that decodes an
		// empty array where it expected an absent key is being told something
		// about the corners. It must be told nothing.
		encoded, err := json.Marshal(e.Lap)
		r.NoError(err)
		r.NotContains(string(encoded), "corners")
	})
}

// TestStintEvent_CarriesTheSetup is the same for the stint's half: the car the
// driver actually drove, as it was stored, handed over as facts.
func TestStintEvent_CarriesTheSetup(t *testing.T) {
	t.Parallel()

	stored := wire.CarSetup{
		UpdateCount: 4,
		Tyres: []wire.SetupTyre{
			{
				Wheel: wire.WheelLF, ColdKpa: 165, HotKpa: 178.5,
				TempInnerC: 96.5, TempMiddleC: 90, TempOuterC: 84.5,
				TreadInnerPct: 94, TreadMiddlePct: 96.5, TreadOuterPct: 98,
			},
			{Wheel: wire.WheelRF, ColdKpa: 165, HotKpa: 178.5},
		},
		RearWing: &wire.SetupValue{
			Group: "TiresAero/AeroSettings", Name: "RearWingSetting",
			Text: "7 hole", Number: 7, Unit: "hole",
		},
		Values: []wire.SetupValue{
			{Group: "Chassis/LeftFront", Name: "Camber", Text: "-3.8 deg", Number: -3.8, Unit: "deg"},
			{Group: "Chassis/Front", Name: "ArbSize", Text: "Medium"},
		},
	}

	stintWith := func(t *testing.T, setup any) db.Stint {
		t.Helper()
		s := testStint(t)
		switch v := setup.(type) {
		case nil:
		case []byte:
			s.Setup = v
		default:
			raw, err := json.Marshal(v)
			require.NoError(t, err)
			s.Setup = raw
		}
		return s
	}

	t.Run("every measurement survives the crossing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stintWith(t, stored), finalSummary())
		r.NoError(e.Validate())
		r.NotNil(e.Stint.Setup)

		got := e.Stint.Setup
		r.Equal(4, got.UpdateCount)
		r.Len(got.Tyres, 2)

		lf, ok := got.TyreAt(plugin.WheelLF)
		r.True(ok)
		r.InDelta(165, lf.ColdKpa, 0)
		r.InDelta(178.5, lf.HotKpa, 0)
		r.InDelta(13.5, lf.HotKpa-lf.ColdKpa, 0,
			"hot against cold is how far the cold setting has to move")
		r.InDelta(12, lf.TempInnerC-lf.TempOuterC, 0,
			"the spread across the tread is the camber reading, and it is the point of all this")
		r.InDelta(94, lf.TreadInnerPct, 0)

		r.NotNil(got.RearWing)
		r.Equal("7 hole", got.RearWing.Text)
		r.InDelta(7, got.RearWing.Number, 0)

		camber, ok := got.Value("Camber")
		r.True(ok)
		r.InDelta(-3.8, camber.Number, 0)
		r.Equal("deg", camber.Unit)

		arb, ok := got.Value("ArbSize")
		r.True(ok)
		r.Equal("Medium", arb.Text)
		r.Zero(arb.Number, "a word has no number and must not become one")
	})

	t.Run("a stint stored with no setup says nothing about one", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stintWith(t, nil), finalSummary())
		r.Nil(e.Stint.Setup, "a simulator that publishes no setup is normal, not an error")

		encoded, err := json.Marshal(e.Stint)
		r.NoError(err)
		r.NotContains(string(encoded), "setup")
	})

	t.Run("a stored document that cannot be read is an absent setup, not a broken stint", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := stintEvent(slog.New(slog.DiscardHandler), testSession(),
			stintWith(t, []byte(`{"tyres": "not a list"}`)), finalSummary())

		r.NoError(e.Validate(), "the stint still finished, and a debrief about it is worth more than none")
		r.Nil(e.Stint.Setup)
		r.Equal(94, e.Stint.ConsistencyPct, "the rest of the facts are untouched")
	})

	t.Run("a document that decodes to nothing is not a setup", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stintWith(t, []byte(`{}`)), finalSummary())
		r.Nil(e.Stint.Setup, "a plugin reading an empty setup would think it had been told something")
	})
}

// finalSummary is a plausible closing summary, so that no test here has to
// build one to talk about the setup beside it.
func finalSummary() wire.Summary {
	finished := time.Date(2026, 9, 13, 18, 45, 0, 0, time.UTC)
	return wire.Summary{
		Laps: 12, Incidents: 1, BestLapMs: 90400, AvgLapMs: 91800,
		ConsistencyPct: 94, TopSpeedKmh: 271,
		CarState: wire.CarState{
			FuelUsedL: 36, FuelLevelL: 12,
			TyreTempC: wire.TyreTemps{LF: 92, RF: 101, LR: 88, RR: 90},
		},
		FinishedAt: &finished,
	}
}
