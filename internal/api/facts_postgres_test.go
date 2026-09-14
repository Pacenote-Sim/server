//go:build postgres

package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// The whole path the two new facts take, over HTTP and through the database:
// a client uploads a stint with a setup and a lap with its corner analysis, and
// a plugin is handed both as facts. Every other test of them is of one hop;
// this is the one that would catch a column that is written and never read.

// recorder is an api.EventSink that keeps what it was given, so a test can
// assert on the facts a plugin would receive without running one.
type recorder struct {
	mu     sync.Mutex
	events []plugin.Event
}

func (r *recorder) Notify(_ context.Context, e plugin.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// lap returns the lap.completed event for one lap number, waiting for it: the
// events are published after the write commits and the sink is called from the
// request goroutine, so by the time the response is read they are all in.
func (r *recorder) lap(tb testing.TB, number int) plugin.Event {
	tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind == plugin.EventLapCompleted && e.Lap != nil && e.Lap.Number == number {
			return e
		}
	}
	require.FailNowf(tb, "no event", "nothing was published for lap %d", number)
	return plugin.Event{}
}

// stint returns the stint.finished event.
func (r *recorder) stint(tb testing.TB) plugin.Event {
	tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind == plugin.EventStintFinished {
			return e
		}
	}
	require.FailNowf(tb, "no event", "no stint was announced as finished")
	return plugin.Event{}
}

// setupBody is the car setup a client uploads with its stint: a plausible GT3
// sheet with the front tyres running twelve degrees hotter on the inner edge
// than on the outer, which is the measurement setup advice is built on.
func setupBody() *wire.CarSetup {
	front := func(w wire.Wheel) wire.SetupTyre {
		return wire.SetupTyre{
			Wheel: w, ColdKpa: 165, HotKpa: 178.5,
			TempInnerC: 96.5, TempMiddleC: 90, TempOuterC: 84.5,
			TreadInnerPct: 94, TreadMiddlePct: 96.5, TreadOuterPct: 98,
		}
	}
	rear := func(w wire.Wheel) wire.SetupTyre {
		return wire.SetupTyre{
			Wheel: w, ColdKpa: 165, HotKpa: 174,
			TempInnerC: 88, TempMiddleC: 86.5, TempOuterC: 85,
			TreadInnerPct: 96.5, TreadMiddlePct: 98, TreadOuterPct: 98,
		}
	}
	return &wire.CarSetup{
		UpdateCount: 4,
		Tyres:       []wire.SetupTyre{front(wire.WheelLF), front(wire.WheelRF), rear(wire.WheelLR), rear(wire.WheelRR)},
		RearWing: &wire.SetupValue{
			Group: "TiresAero/AeroSettings", Name: "RearWingSetting",
			Text: "7 hole", Number: 7, Unit: "hole",
		},
		Values: []wire.SetupValue{
			{Group: "Chassis/LeftFront", Name: "Camber", Text: "-3.8 deg", Number: -3.8, Unit: "deg"},
			{Group: "Chassis/Front", Name: "ArbSize", Text: "Medium"},
		},
	}
}

// cornersBody is a lap's corner analysis as a client sends it, worst first.
func cornersBody() []wire.Corner {
	return []wire.Corner{
		{
			Turn: 3, ApexPct: 600, ApexKmh: 60, RefApexKmh: 80, DeficitKmh: 20,
			BrakeAtApex: 45, ThrottleLag: 40, Pattern: wire.PatternEarlyApex,
		},
		{Turn: 1, ApexPct: 150, ApexKmh: 102, RefApexKmh: 110, DeficitKmh: 8},
	}
}

// TestFactsReachAPlugin drives the whole path: upload, store, announce.
func TestFactsReachAPlugin(t *testing.T) {
	t.Parallel()

	sink := &recorder{}
	h := newHarness(t, harnessOptions{plugins: sink})

	id := randomUUIDv7(t, startedAt)
	stint := sampleStint()
	stint.Setup = setupBody()

	r := require.New(t)
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id, key: "stint-" + id, body: stint,
	}).status)

	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps", key: "laps-" + id,
		body: wire.LapBatch{Laps: []wire.Lap{
			{
				Number: 7, LapMs: 91234, Kind: wire.KindClean, StartedAt: startedAt,
				Trace:   []wire.TracePoint{{OffsetMs: 0, SpeedKmh: 205, DistPct: 0}},
				Corners: cornersBody(),
			},
			{Number: 8, LapMs: 90800, Kind: wire.KindClean, StartedAt: startedAt.Add(time.Minute)},
		}},
	}).status)

	finished := startedAt.Add(45 * time.Minute)
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary", key: "summary-" + id,
		body: wire.Summary{
			Laps: 2, BestLapMs: 90800, AvgLapMs: 91017, ConsistencyPct: 96, TopSpeedKmh: 271,
			CarState:   wire.CarState{FuelUsedL: 6, FuelLevelL: 72, TyreTempC: wire.TyreTemps{LF: 92, RF: 91, LR: 88, RR: 89}},
			FinishedAt: &finished,
		},
	}).status)

	t.Run("the corner analysis is stored with the lap", func(t *testing.T) {
		r := require.New(t)

		stored := h.column(`SELECT corners::text FROM laps WHERE stint_id = $1 AND number = 7`, id)
		r.Contains(stored, `"deficit_kmh": 20`)
		r.Contains(stored, `"early_apex"`)
	})

	t.Run("a lap with no corners stores an empty list, never a null", func(t *testing.T) {
		r := require.New(t)

		r.Equal("[]", h.column(`SELECT corners::text FROM laps WHERE stint_id = $1 AND number = 8`, id))
	})

	t.Run("the lap facts carry the corners", func(t *testing.T) {
		r := require.New(t)

		facts := sink.lap(t, 7).Lap
		r.Len(facts.Corners, 2)
		r.Equal(3, facts.Corners[0].Turn, "worst first, as the client ranked them")
		r.Equal(20, facts.Corners[0].DeficitKmh)
		r.Equal(60, facts.Corners[0].ApexKmh)
		r.Equal(80, facts.Corners[0].ReferenceApexKmh)
		r.Equal(600, facts.Corners[0].ApexPct)
		r.Equal(45, facts.Corners[0].BrakeAtApex)
		r.Equal(40, facts.Corners[0].ThrottleLag)
		r.Equal(plugin.PatternEarlyApex, facts.Corners[0].Pattern)
		r.Equal(1, facts.Corners[1].Turn)
		r.Equal(8, facts.Corners[1].DeficitKmh)
	})

	t.Run("a lap that carried none is given none", func(t *testing.T) {
		r := require.New(t)
		r.Empty(sink.lap(t, 8).Lap.Corners)
	})

	t.Run("the stint facts carry the setup", func(t *testing.T) {
		r := require.New(t)

		facts := sink.stint(t).Stint
		r.NotNil(facts.Setup, "the setup was uploaded with the stint and has to come back with it")
		r.Equal(4, facts.Setup.UpdateCount)
		r.Len(facts.Setup.Tyres, 4)

		lf, ok := facts.Setup.TyreAt(plugin.WheelLF)
		r.True(ok)
		r.InDelta(165, lf.ColdKpa, 0.001)
		r.InDelta(178.5, lf.HotKpa, 0.001)
		r.InDelta(12, lf.TempInnerC-lf.TempOuterC, 0.001,
			"the camber reading survives the database")

		lr, ok := facts.Setup.TyreAt(plugin.WheelLR)
		r.True(ok)
		r.InDelta(3, lr.TempInnerC-lr.TempOuterC, 0.001, "and so does the rear axle being closer to even")

		r.NotNil(facts.Setup.RearWing)
		r.Equal("7 hole", facts.Setup.RearWing.Text)

		camber, ok := facts.Setup.Value("Camber")
		r.True(ok)
		r.InDelta(-3.8, camber.Number, 0.001)
		r.Equal("deg", camber.Unit)
	})

	t.Run("a repeat of the same lap with different corners is a conflict", func(t *testing.T) {
		r := require.New(t)

		changed := cornersBody()
		changed[0].DeficitKmh = 21
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps", key: "again-" + id,
			body: wire.LapBatch{Laps: []wire.Lap{{
				Number: 7, LapMs: 91234, Kind: wire.KindClean, StartedAt: startedAt,
				Trace:   []wire.TracePoint{{OffsetMs: 0, SpeedKmh: 205, DistPct: 0}},
				Corners: changed,
			}}},
		})
		r.Equal(http.StatusConflict, res.status, res.body)
		r.Equal(wire.CodeConflict, res.envelope(t).Code,
			"the corner analysis is part of the lap's content, so two answers about it are two different laps")
	})
}

// TestSetupIsOptionalEndToEnd is the degradation the brief asks for, proved
// over the real path: a simulator that publishes no setup costs nothing.
func TestSetupIsOptionalEndToEnd(t *testing.T) {
	t.Parallel()

	sink := &recorder{}
	h := newHarness(t, harnessOptions{plugins: sink})
	ctx := context.Background()

	r := require.New(t)
	id := randomUUIDv7(t, startedAt)
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id, key: "stint-" + id, body: sampleStint(),
	}).status)

	finished := startedAt.Add(20 * time.Minute)
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary", key: "summary-" + id,
		body: wire.Summary{Laps: 1, BestLapMs: 91000, AvgLapMs: 91000, FinishedAt: &finished},
	}).status)

	parsed, err := db.ParseUUID(id)
	r.NoError(err)
	stored, err := h.store.Stint(ctx, parsed, h.driver.ID)
	r.NoError(err)
	r.Nil(stored.Setup, "no setup was sent, so none is stored — and null is not an empty document")

	r.Nil(sink.stint(t).Stint.Setup, "a plugin is told nothing rather than something false")
}

// TestSetupIsClearedWhenAClientStopsSendingOne pins the update rule: the setup
// a stint was driven on is whatever the client last said it was.
func TestSetupIsClearedWhenAClientStopsSendingOne(t *testing.T) {
	t.Parallel()

	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	r := require.New(t)
	id := randomUUIDv7(t, startedAt)

	withSetup := sampleStint()
	withSetup.Setup = setupBody()
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id, key: "a-" + id, body: withSetup,
	}).status)

	parsed, err := db.ParseUUID(id)
	r.NoError(err)
	stored, err := h.store.Stint(ctx, parsed, h.driver.ID)
	r.NoError(err)
	r.NotEmpty(stored.Setup)

	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id, key: "b-" + id, body: sampleStint(),
	}).status)

	stored, err = h.store.Stint(ctx, parsed, h.driver.ID)
	r.NoError(err)
	r.Nil(stored.Setup, "a client that has stopped seeing a setup is telling us something")
}
