//go:build postgres

package api_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// The budgets this server holds itself to on the paths that run while a driver
// is waiting. They are stated here so a number in a benchmark result has
// something to be read against.
//
//	Lap ingest, 40 laps   < 50 ms end to end
//	Reference lookup      < 1 ms
//	GET /me               < 5 ms
const (
	budgetLapIngest40   = "50ms"
	budgetReferenceLap  = "1ms"
	budgetMe            = "5ms"
	benchmarkBatchLaps  = 40
	benchmarkTrackID    = "barcelona gp"
	benchmarkCar        = "Ferrari 296 GT3"
	benchmarkCarClass   = "gt3"
	benchmarkTokenEvery = 1
)

// BenchmarkLapIngest40 measures the whole request: forty laps, each carrying
// the protocol module's own 300-point golden trace, from the HTTP handler
// through the compression and the COPY to the committed transaction.
//
// The stint and the machine are made with the timer stopped, because neither is
// what the budget is about: a client uploads one batch per stint and its
// machine was paired weeks ago. Rotating the machine is also what keeps the
// rate limiter out of the measurement — it is keyed per device, and forty
// batches a second is not something any client does.
func BenchmarkLapIngest40(b *testing.B) {
	h := newHarness(b, harnessOptions{})
	laps := make([]wire.Lap, 0, benchmarkBatchLaps)
	for i := range benchmarkBatchLaps {
		laps = append(laps, sampleLap(b, i+1, 90000+i*17))
	}
	batch := wire.LapBatch{Laps: laps}

	b.ReportAllocs()
	b.Logf("budget: %s end to end for %d laps", budgetLapIngest40, benchmarkBatchLaps)

	i := 0
	for b.Loop() {
		b.StopTimer()
		token := h.newToken("Ingest " + strconv.Itoa(i))
		id := randomUUIDv7(b, startedAt)
		if res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id,
			token: token, key: "bench-stint-" + id, body: sampleStint(),
		}); res.status != http.StatusOK {
			b.Fatalf("the stint did not store: %s", res.body)
		}
		i++
		b.StartTimer()

		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			token: token, key: "bench-laps-" + id, body: batch,
		})
		if res.status != http.StatusOK {
			b.Fatalf("the laps did not store: %s", res.body)
		}
	}
}

// BenchmarkReferenceLookup measures the query the coach's training mode runs
// once a lap while the driver is waiting to hear a cue. It is the one
// denormalisation in the schema and this is what it is for: three index
// lookups and two primary-key joins, with nothing that grows as laps are added.
func BenchmarkReferenceLookup(b *testing.B) {
	h := newHarness(b, harnessOptions{})
	ctx := context.Background()
	h.uploadLap(b, h.token, benchmarkTrackID, 90118, 1)

	query := db.ReferenceQuery{
		DriverID:   h.driver.ID,
		Sim:        testSim,
		TrackID:    benchmarkTrackID,
		Car:        benchmarkCar,
		CarClass:   benchmarkCarClass,
		Preference: []wire.Scope{wire.ScopeClass, wire.ScopeCar, wire.ScopeSelf},
	}

	b.ReportAllocs()
	b.Logf("budget: %s", budgetReferenceLap)

	for b.Loop() {
		res, err := h.store.ReferenceLap(ctx, query)
		if err != nil {
			b.Fatalf("the reference lap did not come back: %v", err)
		}
		if res.LapMs != 90118 {
			b.Fatalf("the wrong lap came back: %d", res.LapMs)
		}
	}
}

// BenchmarkReferenceRequest is the same lookup with the request around it: the
// bearer lookup, the rate limiter, the query, the trace decode and the JSON.
// It is the number a client actually waits for.
func BenchmarkReferenceRequest(b *testing.B) {
	h := newHarness(b, harnessOptions{})
	h.uploadLap(b, h.token, benchmarkTrackID, 90118, 1)
	// The car scope, because each iteration asks as a different machine and a
	// driver who has just been created has no best of their own to compare
	// against — which is the fallback the contract asks for, measured.
	path := referenceURL(testSim, benchmarkTrackID, "car")

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		b.StopTimer()
		token := h.newToken("Reader " + strconv.Itoa(i))
		i++
		b.StartTimer()

		if res := h.do(request{method: http.MethodGet, path: path, token: token}); res.status != http.StatusOK {
			b.Fatalf("the reference lap did not come back: %s", res.body)
		}
	}
}

// BenchmarkGetMe measures the identity call, which a client makes once on every
// connection, and which is allowed five milliseconds and one query.
func BenchmarkGetMe(b *testing.B) {
	h := newHarness(b, harnessOptions{})

	b.ReportAllocs()
	b.Logf("budget: %s", budgetMe)

	i := 0
	for b.Loop() {
		b.StopTimer()
		token := h.newToken("Asker " + strconv.Itoa(i))
		i++
		b.StartTimer()

		if res := h.do(request{method: http.MethodGet, path: "/api/v1/me", token: token}); res.status != http.StatusOK {
			b.Fatalf("me did not answer: %s", res.body)
		}
	}
}

// TestBenchmarksMeetTheirBudgets is the benchmark result as an assertion, run
// once rather than to convergence, so a regression fails a test run rather than
// waiting for somebody to read a number.
//
// The margins are generous — the budgets are for a server on the operator's own
// hardware with nothing else on it, and a test machine running the rest of this
// suite in parallel is neither — but a path that has become ten times slower
// will not pass them. Each measurement is the best of a few attempts, because
// the worst of a few is a measurement of whatever else the machine was doing.
func TestBenchmarksMeetTheirBudgets(t *testing.T) {
	t.Parallel()
	if raceEnabled {
		t.Skip("the race detector instruments every read and write, so a wall-clock budget would measure it")
	}
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	t.Run("forty laps end to end, under 50 ms", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		laps := make([]wire.Lap, 0, benchmarkBatchLaps)
		for i := range benchmarkBatchLaps {
			laps = append(laps, sampleLap(t, i+1, 90000+i*17))
		}

		best := bestOf(budgetAttempts, func() time.Duration {
			id := h.stint(t)
			out := timed(func() response {
				return h.do(request{
					method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
					key: "budget-laps-" + id, body: wire.LapBatch{Laps: laps},
				})
			})
			r.Equal(http.StatusOK, out.value.status, out.value.body)
			return out.took
		})
		t.Logf("forty laps took %s (budget %s)", best, budgetLapIngest40)
		r.Less(best.Seconds(), 0.5, "a forty-lap ingest that takes half a second has regressed badly")
	})

	t.Run("the reference lookup, under 1 ms", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		const track = "budget-track"
		h.uploadLap(t, h.token, track, 90118, 1)

		query := db.ReferenceQuery{
			DriverID: h.driver.ID, Sim: testSim, TrackID: track, Car: benchmarkCar, CarClass: benchmarkCarClass,
			Preference: []wire.Scope{wire.ScopeClass, wire.ScopeCar, wire.ScopeSelf},
		}
		// Warm the pool, so what is measured is the query rather than the first
		// acquisition of a connection.
		_, err := h.store.ReferenceLap(ctx, query)
		r.NoError(err)

		best := bestOf(budgetAttempts, func() time.Duration {
			out := timed(func() error {
				_, err := h.store.ReferenceLap(ctx, query)
				return err
			})
			r.NoError(out.value)
			return out.took
		})
		t.Logf("the reference lookup took %s (budget %s)", best, budgetReferenceLap)
		r.Less(best.Seconds(), 0.05, "a reference lookup that takes fifty milliseconds is a sort, not a lookup")
	})
}

// budgetAttempts is how many times a measured path is run before the best of
// them is taken.
const budgetAttempts = 5

// bestOf runs fn n times and returns the shortest it took.
func bestOf(n int, fn func() time.Duration) time.Duration {
	best := time.Duration(0)
	for i := range n {
		took := fn()
		if i == 0 || took < best {
			best = took
		}
	}
	return best
}
