//go:build postgres

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/trace"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// stint creates a stint and returns its identifier, for the tests that are
// about what happens afterwards.
func (h *harness) stint(tb testing.TB) string {
	tb.Helper()
	id := randomUUIDv7(tb, startedAt)
	res := h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id,
		key: "stint-" + id, body: sampleStint(),
	})
	require.Equal(tb, http.StatusOK, res.status, res.body)
	return id
}

func TestLaps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("stores a batch and reports the best lap of the stint", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := h.stint(t)

		batch := wire.LapBatch{Laps: []wire.Lap{
			sampleLap(t, 1, 92100),
			sampleLap(t, 2, 90118),
			sampleLap(t, 3, 91402),
		}}
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "laps-" + id, body: batch,
		})
		r.Equal(http.StatusOK, res.status, res.body)

		var out wire.LapBatchResult
		r.NoError(json.Unmarshal([]byte(res.body), &out))
		r.Equal(3, out.Accepted)
		r.Equal(90118, out.BestLapMs, "the best over the whole stint, not over this batch")
	})

	t.Run("the stored trace decodes back to the points that were sent", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		id := h.stint(t)

		lap := sampleLap(t, 11, 90118)
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "round-trip-" + id, body: wire.LapBatch{Laps: []wire.Lap{lap}},
		}).status)

		ref, err := h.store.ReferenceLap(ctx, db.ReferenceQuery{
			DriverID: h.driver.ID, Sim: testSim, TrackID: "barcelona gp", Car: "Ferrari 296 GT3",
			CarClass: "gt3", Preference: []wire.Scope{wire.ScopeSelf},
		})
		r.NoError(err)
		r.Equal(trace.CodecVersion, ref.TraceCodec)

		got, err := trace.Decode(ref.Trace, nil)
		r.NoError(err)
		r.Equal(lap.Trace, got, "what came out of the database is what went in")
	})

	t.Run("a repeat of the same lap is stored once", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		id := h.stint(t)
		batch := wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 4, 91000)}}

		first := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "first-" + id, body: batch,
		})
		r.Equal(http.StatusOK, first.status, first.body)

		// A different key, so this is a fresh request rather than a replay: the
		// idempotency of (stint, number) is what has to hold here.
		second := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "second-" + id, body: batch,
		})
		r.Equal(http.StatusOK, second.status, second.body)

		var out wire.LapBatchResult
		r.NoError(json.Unmarshal([]byte(second.body), &out))
		r.Equal(0, out.Accepted, "the lap was counted on the call that stored it")

		parsed, err := db.ParseUUID(id)
		r.NoError(err)
		count, err := h.store.CountLapsForStint(ctx, parsed)
		r.NoError(err)
		r.EqualValues(1, count)
	})

	t.Run("the same lap number with different content is a conflict", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		id := h.stint(t)

		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "orig-" + id, body: wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 5, 91000)}},
		}).status)

		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "diff-" + id,
			body: wire.LapBatch{Laps: []wire.Lap{
				sampleLap(t, 5, 88000),
				sampleLap(t, 6, 91500),
			}},
		})
		r.Equal(http.StatusConflict, res.status, res.body)
		env := res.envelope(t)
		r.Equal(wire.CodeConflict, env.Code)
		r.Equal([]any{float64(5)}, env.Detail["lap_numbers"])

		parsed, err := db.ParseUUID(id)
		r.NoError(err)
		count, err := h.store.CountLapsForStint(ctx, parsed)
		r.NoError(err)
		r.EqualValues(1, count, "a refused batch stores none of itself, not just the lap that clashed")
	})

	t.Run("a conflict replays as a conflict", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := h.stint(t)
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "base-" + id, body: wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 9, 91000)}},
		}).status)

		clash := wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 9, 88000)}}
		key := "clash-" + id
		first := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps", key: key, body: clash,
		})
		second := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps", key: key, body: clash,
		})
		r.Equal(http.StatusConflict, first.status)
		r.Equal(first.status, second.status)
		r.Equal(first.body, second.body,
			"a refusal is an answer and is replayed, so a retry cannot succeed where the first was refused")
	})

	t.Run("laps for a stint that is not there", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "orphan-" + id, body: wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 1, 91000)}},
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
		r.Equal(wire.CodeNotFound, res.envelope(t).Code)
	})

	t.Run("the validation the contract asks for", func(t *testing.T) {
		t.Parallel()
		id := h.stint(t)
		long := make([]wire.TracePoint, 0, 301)
		for i := range 301 {
			long = append(long, wire.TracePoint{OffsetMs: i * 10, DistPct: i % 1000})
		}
		cases := []struct {
			name  string
			field string
			batch wire.LapBatch
		}{
			{"no laps at all", "laps", wire.LapBatch{}},
			{"a lap number no session reaches", "number", wire.LapBatch{Laps: []wire.Lap{{
				Number: -1, LapMs: 91000, Kind: wire.KindClean, StartedAt: lapStartedAt,
			}}}},
			{"a lap time no lap takes", "lap_ms", wire.LapBatch{Laps: []wire.Lap{{
				Number: 1, LapMs: 0, Kind: wire.KindClean, StartedAt: lapStartedAt,
			}}}},
			{"a kind this server does not know", "kind", wire.LapBatch{Laps: []wire.Lap{{
				Number: 1, LapMs: 91000, Kind: "hotlap", StartedAt: lapStartedAt,
			}}}},
			{"no start time", "started_at", wire.LapBatch{Laps: []wire.Lap{{
				Number: 1, LapMs: 91000, Kind: wire.KindClean,
			}}}},
			{"a trace longer than the published limit", "trace", wire.LapBatch{Laps: []wire.Lap{{
				Number: 1, LapMs: 91000, Kind: wire.KindClean, StartedAt: lapStartedAt, Trace: long,
			}}}},
			{"the same lap number twice in one batch", "laps", wire.LapBatch{Laps: []wire.Lap{
				sampleLap(t, 2, 91000), sampleLap(t, 2, 92000),
			}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				res := h.do(request{
					method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
					key: "invalid-" + tc.name, body: tc.batch,
				})
				r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
				env := res.envelope(t)
				r.Equal(wire.CodeInvalid, env.Code)
				r.Equal(tc.field, env.Detail["field"])
			})
		}
	})

	t.Run("a trace the codec refuses is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := h.stint(t)
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "bad-trace-" + id,
			body: wire.LapBatch{Laps: []wire.Lap{{
				Number: 1, LapMs: 91000, Kind: wire.KindClean, StartedAt: lapStartedAt,
				Trace: []wire.TracePoint{{OffsetMs: 10}, {OffsetMs: 5}},
			}}},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		env := res.envelope(t)
		r.Equal(wire.CodeInvalid, env.Code)
		r.Equal("trace", env.Detail["field"])
	})

	t.Run("more laps than the published batch size", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := h.stint(t)
		laps := make([]wire.Lap, 0, 51)
		for i := range 51 {
			laps = append(laps, wire.Lap{
				Number: i, LapMs: 91000, Kind: wire.KindClean, StartedAt: lapStartedAt,
			})
		}
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			key: "too-many-" + id, body: wire.LapBatch{Laps: laps},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		env := res.envelope(t)
		r.Equal(wire.CodeInvalid, env.Code)
		r.EqualValues(50, env.Detail["max"])
	})
}

func TestSummary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("replaces the whole document", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := h.stint(t)

		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary",
			key: "sum-a-" + id, body: sampleSummary(t, nil),
		})
		r.Equal(http.StatusOK, res.status, res.body)
		var ok wire.OK
		r.NoError(json.Unmarshal([]byte(res.body), &ok))
		r.True(ok.OK)

		final := lapStartedAt.Add(20 * time.Minute)
		res = h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary",
			key: "sum-b-" + id, body: sampleSummary(t, &final),
		})
		r.Equal(http.StatusOK, res.status, res.body)

		parsed, err := db.ParseUUID(id)
		r.NoError(err)
		stint, err := h.store.Stint(context.Background(), parsed, h.driver.ID)
		r.NoError(err)
		r.NotNil(stint.FinishedAt, "a summary with finished_at closes the stint")
	})

	t.Run("a summary for a stint that is not there", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary",
			key: "orphan-sum-" + id, body: sampleSummary(t, nil),
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
		r.Equal(wire.CodeNotFound, res.envelope(t).Code)
	})

	t.Run("the validation the contract asks for", func(t *testing.T) {
		t.Parallel()
		id := h.stint(t)
		cases := []struct {
			name  string
			field string
			mut   func(*wire.Summary)
		}{
			{"a consistency that is not a percentage", "consistency_pct", func(s *wire.Summary) { s.ConsistencyPct = 140 }},
			{"a top speed no car reaches", "top_speed_kmh", func(s *wire.Summary) { s.TopSpeedKmh = 5000 }},
			{"a best lap no lap takes", "best_lap_ms", func(s *wire.Summary) { s.BestLapMs = -1 }},
			{"more incidents than a session holds", "incidents", func(s *wire.Summary) { s.Incidents = -3 }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				body := sampleSummary(t, nil)
				tc.mut(&body)
				// Its own machine, because the summary interval is thirty
				// seconds and four refused summaries in a row from one would
				// be a rate limit rather than four answers.
				res := h.do(request{
					method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary",
					token: h.newToken("Summary " + tc.name),
					key:   "bad-sum-" + tc.name, body: body,
				})
				r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
				env := res.envelope(t)
				r.Equal(wire.CodeInvalid, env.Code)
				r.Equal(tc.field, env.Detail["field"])
			})
		}
	})
}

func sampleSummary(tb testing.TB, finished *time.Time) wire.Summary {
	tb.Helper()
	return wire.Summary{
		Laps:           12,
		Incidents:      2,
		BestLapMs:      90118,
		AvgLapMs:       91402,
		ConsistencyPct: 94,
		TopSpeedKmh:    271,
		BestTrace:      protocolGolden(tb, "lap-300"),
		Conditions: wire.Conditions{
			Skies: 1, Wetness: 0, WindKmh: 8.4, Humidity: 41.0,
			TrackTempC: 34.2, AirTempC: 24.9,
		},
		CarState: wire.CarState{
			FuelUsedL: 48.2, FuelLevelL: 12.0,
			TyreTempC: wire.TyreTemps{LF: 82.1, RF: 84.0, LR: 79.6, RR: 81.2},
		},
		FinishedAt: finished,
	}
}
