package api

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// The two documents the client sends and this server does not read: where a lap
// was lost, and what the car was set to. Both are stored as they arrived and
// handed to a plugin as they were stored. These tests are about the handover
// being exact — a document that changes on the way through is worse than one
// that never arrives, because nothing downstream can tell — and about this
// server having no opinion of its own about what is inside.

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

// storedLap is a lap as encodeLaps writes it, with its corner analysis encoded
// the way the column holds it.
func storedLap(t *testing.T, number int, corners []wire.Corner) db.LapRow {
	t.Helper()
	raw, err := json.Marshal(cornersOrEmpty(corners))
	require.NoError(t, err)
	return db.LapRow{Number: number, LapMs: 91234, Kind: string(wire.KindClean), Corners: raw}
}

// decodeCorners reads the document back the way a plugin would, with the wire
// types the protocol module publishes.
func decodeCorners(t *testing.T, doc json.RawMessage) []wire.Corner {
	t.Helper()
	var out []wire.Corner
	require.NoError(t, json.Unmarshal(doc, &out))
	return out
}

// TestLapEvent_CarriesTheCorners is the server half of the round trip: what the
// client detected reaches plugin.LapFacts.Corners unchanged.
func TestLapEvent_CarriesTheCorners(t *testing.T) {
	t.Parallel()

	t.Run("the document is the one that was stored, byte for byte", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		row := storedLap(t, 7, []wire.Corner{worstCorner()})
		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), row, 90400)

		r.NoError(e.Validate())
		r.Equal(string(row.Corners), string(e.Lap.Corners),
			"not re-encoded, not re-ordered, not read: the stored bytes are the fact")
	})

	t.Run("every measurement survives the crossing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		sent := worstCorner()
		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t),
			storedLap(t, 7, []wire.Corner{sent}), 90400)

		got := decodeCorners(t, e.Lap.Corners)
		r.Len(got, 1)
		r.Equal(sent, got[0])
		r.Equal(got[0].RefApexKmh-got[0].ApexKmh, got[0].DeficitKmh,
			"the deficit is still the two speeds beside it after the crossing")
	})

	t.Run("the order the client ranked them in is kept", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), storedLap(t, 7, []wire.Corner{
			{Turn: 3, ApexPct: 600, ApexKmh: 60, RefApexKmh: 80, DeficitKmh: 20},
			{Turn: 1, ApexPct: 150, ApexKmh: 102, RefApexKmh: 110, DeficitKmh: 8},
			{Turn: 4, ApexPct: 800, ApexKmh: 146, RefApexKmh: 150, DeficitKmh: 4},
		}), 90400)

		got := decodeCorners(t, e.Lap.Corners)
		turns := []int{got[0].Turn, got[1].Turn, got[2].Turn}
		r.Equal([]int{3, 1, 4}, turns, "worst first is the client's ranking and this server does not re-sort it")
	})

	t.Run("a corner with nothing conclusive keeps its empty pattern", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), storedLap(t, 2, []wire.Corner{
			{Turn: 1, ApexPct: 150, ApexKmh: 102, RefApexKmh: 110, DeficitKmh: 8},
		}), 90400)

		r.Empty(decodeCorners(t, e.Lap.Corners)[0].Pattern, "an empty pattern is an answer and must not become a guess")
	})

	t.Run("a lap that arrived with no corners is given none", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), storedLap(t, 1, nil), 0)
		r.Nil(e.Lap.Corners, "the column holds an empty list, and the plugin is told nothing")

		// The facts cross to a plugin as JSON, and a plugin that decodes an
		// empty array where it expected an absent key is being told something
		// about the corners. It must be told nothing.
		encoded, err := json.Marshal(e.Lap)
		r.NoError(err)
		r.NotContains(string(encoded), "corners")
	})

	t.Run("a field this server has never heard of reaches the plugin anyway", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// The reason the document is not a struct. A client that starts
		// measuring something new writes it into the analysis, and every
		// plugin sees it, and this server was not edited.
		row := db.LapRow{
			Number: 9, LapMs: 91234, Kind: string(wire.KindClean),
			Corners: []byte(`[{"turn":2,"apex_pct":300,"apex_kmh":90,"deficit_kmh":5,"brake_at_pct":288}]`),
		}
		e := lapEvent(slog.New(slog.DiscardHandler), testSession(), testStint(t), row, 90400)

		r.Contains(string(e.Lap.Corners), `"brake_at_pct":288`)
	})
}

// TestStintEvent_CarriesTheSetup is the same for the stint's half: the car the
// driver actually drove, as it was stored, handed over as it was stored.
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

	t.Run("the document is the one that was stored, byte for byte", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		stint := stintWith(t, stored)
		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stint, finalSummary())
		r.NoError(e.Validate())
		r.Equal(string(stint.Setup), string(e.Stint.Setup))
	})

	t.Run("every measurement survives the crossing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stintWith(t, stored), finalSummary())

		var got wire.CarSetup
		r.NoError(json.Unmarshal(e.Stint.Setup, &got))
		r.Equal(stored, got)
		r.InDelta(12, got.Tyres[0].TempInnerC-got.Tyres[0].TempOuterC, 0,
			"the spread across the tread is the camber reading, and it is the point of all this")
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

	t.Run("a stored null is the same as nothing stored", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stintWith(t, []byte(`null`)), finalSummary())
		r.Nil(e.Stint.Setup)
	})

	t.Run("a document this server would not have understood is handed over anyway", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// This server does not read the setup, so it cannot judge it. Whether
		// this is a setup is the plugin's to decide, and a server that dropped
		// it for not understanding it would be deciding that for every plugin
		// at once.
		odd := []byte(`{"tyres": "not a list", "ride_height_mm": 54}`)
		e := stintEvent(slog.New(slog.DiscardHandler), testSession(), stintWith(t, odd), finalSummary())

		r.NoError(e.Validate(), "the stint still finished, and the rest of the facts are still good")
		r.Equal(string(odd), string(e.Stint.Setup))
		r.Equal(94, e.Stint.ConsistencyPct, "the rest of the facts are untouched")
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
