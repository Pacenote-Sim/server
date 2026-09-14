//go:build postgres

package db

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db/dbtest"
)

// beforeSim is the schema version this package's fixtures are built against:
// the last one before laps and reference_laps learned which simulator a lap
// came from. It is an internal test because reaching a single version rather
// than the newest one means reaching the migration provider, and nothing
// outside this package is allowed to.
const beforeSim = 5

// simFixture is the world these tests migrate: one driver, one circuit, one car
// name, three simulators, and one clean lap in each.
const (
	fixtureTrack    = "spa"
	fixtureCar      = "Ferrari 296 GT3"
	fixtureClass    = "gt3"
	fixtureIracing  = "iracing"
	fixtureACC      = "acc"
	fixturePruned   = "rfactor2"
	iracingLapMs    = 138400
	accLapMs        = 134900
	prunedLapMs     = 130000
	fixtureDriverNm = "Lucía Ferrán"
)

// TestMigrationBackfillsTheSimulator drives the migration against rows created
// before it existed.
//
// The world it builds is the one the bug produced: a driver with a Spa lap in
// two simulators, and a single reference bucket holding whichever of them
// happened to be quicker — the Assetto Corsa Competizione lap, which the coach
// was serving to the same driver when they were sitting in iRacing.
//
// After the migration every lap says which simulator it came from, the bucket
// that existed names the simulator of the lap it points at, and the bucket that
// could not exist before — the same driver's best in the other simulator — is
// there. Nothing is invented: a lap whose trace has already been pruned stays
// out, because a bucket pointing at a cleared blob is a server error on the
// next coaching request rather than an answer.
func TestMigrationBackfillsTheSimulator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedTo(t, beforeSim)
	driverID := seedBeforeSim(t, store)
	require.NoError(t, store.Migrate(ctx), "the simulator migration applies over rows that predate it")

	t.Run("every lap carries the simulator of the stint it belongs to", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		cases := []struct {
			lapMs int
			want  string
		}{
			{iracingLapMs, fixtureIracing},
			{accLapMs, fixtureACC},
			{prunedLapMs, fixturePruned},
		}
		for _, tc := range cases {
			var got string
			r.NoError(store.pool.QueryRow(ctx,
				`SELECT sim FROM laps WHERE lap_ms = $1`, tc.lapMs).Scan(&got))
			r.Equal(tc.want, got, "the lap of %d ms was driven in %s", tc.lapMs, tc.want)
		}
	})

	buckets := []struct {
		name     string
		scope    string
		sim      string
		byDriver bool
		want     int
	}{
		{
			name:  "the bucket that existed keeps its lap and gains its simulator",
			scope: "self", sim: fixtureACC, byDriver: true, want: accLapMs,
		},
		{
			name:  "the driver's best in the other simulator is no longer hidden behind it",
			scope: "self", sim: fixtureIracing, byDriver: true, want: iracingLapMs,
		},
		{
			name:  "the same holds for the best anyone has driven in that car",
			scope: "car", sim: fixtureACC, want: accLapMs,
		},
		{
			name:  "and for the other simulator's best in that car",
			scope: "car", sim: fixtureIracing, want: iracingLapMs,
		},
	}
	for _, tc := range buckets {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			var lapMs int
			owner := "driver_id IS NULL"
			args := []any{tc.scope, tc.sim, fixtureTrack}
			if tc.byDriver {
				owner = "driver_id = $4"
				args = append(args, driverID)
			}
			r.NoError(store.pool.QueryRow(ctx,
				`SELECT lap_ms FROM reference_laps
				  WHERE scope = $1 AND sim = $2 AND track_id = $3 AND `+owner,
				args...).Scan(&lapMs))
			r.Equal(tc.want, lapMs, "the %s bucket of %s holds that simulator's own best", tc.scope, tc.sim)
		})
	}

	t.Run("a lap whose trace was pruned is not resurrected as a reference", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		var rows int
		r.NoError(store.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM reference_laps WHERE sim = $1`, fixturePruned).Scan(&rows))
		r.Zero(rows, "a bucket pointing at a cleared blob would fail the next request that read it")
	})
}

// TestBucketsStillRefuseDuplicatesWithinOneSimulator pins the half of the
// rebuilt index that is easy to lose: widening a unique key must not weaken it.
// One simulator, one circuit, one car, one scope is still one row.
func TestBucketsStillRefuseDuplicatesWithinOneSimulator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedTo(t, beforeSim)
	driverID := seedBeforeSim(t, store)
	require.NoError(t, store.Migrate(ctx))

	cases := []struct {
		name     string
		scope    string
		byDriver bool
	}{
		{name: "the driver's own best", scope: "self", byDriver: true},
		{name: "the best anyone has driven in that car", scope: "car"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			var lapID int64
			r.NoError(store.pool.QueryRow(ctx,
				`SELECT lap_id FROM reference_laps WHERE scope = $1 AND sim = $2 LIMIT 1`,
				tc.scope, fixtureACC).Scan(&lapID))

			class := ""
			var owner *int64
			if tc.byDriver {
				class, owner = fixtureClass, &driverID
			}
			_, err := store.pool.Exec(ctx,
				`INSERT INTO reference_laps (scope, sim, track_id, car, car_class, driver_id, lap_id, lap_ms)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				tc.scope, fixtureACC, fixtureTrack, fixtureCar, class, owner, lapID, accLapMs)
			r.Error(err, "a second row in one simulator's bucket must not be accepted")

			var pgErr *pgconn.PgError
			r.ErrorAs(err, &pgErr)
			r.Equal("23505", pgErr.Code, "the refusal is the unique index and not something else")
		})
	}
}

// migratedTo opens a database of this test's own and brings it up to one
// migration rather than to the newest, so that a test can build the rows a
// later migration has to cope with.
func migratedTo(tb testing.TB, version int64) *Store {
	tb.Helper()
	r := require.New(tb)

	store, err := Open(context.Background(), dbtest.URL(tb), slog.New(slog.DiscardHandler))
	r.NoError(err)
	tb.Cleanup(store.Close)

	p, closeProvider, err := store.provider()
	r.NoError(err)
	defer closeProvider()

	_, err = p.UpTo(context.Background(), version)
	r.NoError(err)
	return store
}

// seedBeforeSim writes the rows the schema held before the simulator migration:
// laps with no sim of their own, and the single reference bucket per track and
// car that the two simulators were sharing. It returns the driver they belong
// to.
func seedBeforeSim(tb testing.TB, store *Store) int64 {
	tb.Helper()
	r := require.New(tb)
	ctx := context.Background()

	var driverID int64
	r.NoError(store.pool.QueryRow(ctx,
		`INSERT INTO drivers (name, slug, class) VALUES ($1, 'lucia-ferran', $2) RETURNING id`,
		fixtureDriverNm, fixtureClass).Scan(&driverID))

	// One lap in each simulator, and one whose trace has already been pruned —
	// the state PruneTracesOlderThan leaves behind.
	laps := []struct {
		sim   string
		lapMs int
		trace []byte
	}{
		{fixtureIracing, iracingLapMs, []byte{0x01, 0x02}},
		{fixtureACC, accLapMs, []byte{0x03, 0x04}},
		{fixturePruned, prunedLapMs, []byte{}},
	}
	var accLapID int64
	for i, l := range laps {
		var stintID string
		r.NoError(store.pool.QueryRow(ctx,
			`INSERT INTO stints (id, driver_id, sim, track_id, track, car, car_class, session_type, started_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $3, $4, $5, 'practice', now())
			 RETURNING id`,
			driverID, l.sim, fixtureTrack, fixtureCar, fixtureClass).Scan(&stintID))

		var lapID int64
		r.NoError(store.pool.QueryRow(ctx,
			`INSERT INTO laps (stint_id, number, lap_ms, kind, started_at,
			                   track_id, car, car_class, driver_id,
			                   content_sha256, trace_codec, trace)
			 VALUES ($1, $2, $3, 'clean', now(), $4, $5, $6, $7, $8, 1, $9)
			 RETURNING id`,
			stintID, i+1, l.lapMs, fixtureTrack, fixtureCar, fixtureClass, driverID,
			[]byte{byte(i)}, l.trace).Scan(&lapID))
		if l.sim == fixtureACC {
			accLapID = lapID
		}
	}

	// What the old code had written: one bucket per scope, holding whichever
	// simulator's lap was quicker, which is the collision this migration ends.
	for _, scope := range []string{"self", "car"} {
		class, owner := "", (*int64)(nil)
		if scope == "self" {
			class, owner = fixtureClass, &driverID
		}
		_, err := store.pool.Exec(ctx,
			`INSERT INTO reference_laps (scope, track_id, car, car_class, driver_id, lap_id, lap_ms)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			scope, fixtureTrack, fixtureCar, class, owner, accLapID, accLapMs)
		r.NoError(err)
	}
	return driverID
}
