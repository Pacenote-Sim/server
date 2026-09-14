package db

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// LapRow is one completed lap ready to store. ContentSum fingerprints the lap
// as the client sent it and is what makes a repeat of lap 7 either the same lap
// 7 or a conflict; Trace is already compressed by the protocol codec.
type LapRow struct {
	Number     int
	LapMs      int
	Kind       string
	StartedAt  time.Time
	ContentSum []byte
	TraceCodec int
	Trace      []byte
	// Corners is the client's own corner analysis, already encoded as the JSON
	// document the column holds. Nothing here looks inside it — it is written
	// with the lap and read back whole — so it is carried as bytes. Nil is a lap
	// with nothing to say about its corners and is stored as an empty array:
	// the column is NOT NULL, and one spelling of "no corners" is less to get
	// wrong than two.
	Corners []byte
}

// LapWrite is one POST /stints/{id}/laps, ready to store. The simulator, track,
// car and class are copied down from the stint onto every lap, which is the one
// denormalisation the reference lookup rests on.
//
// Sim is part of that identity and not a label beside it. A track_id is a slug
// the client derives from the name its simulator prints, so two simulators
// arrive at the same slug for the same circuit and mean two different
// circuits — different physics, different lap times — and a lap of one is never
// the reference for a lap of the other.
type LapWrite struct {
	StintID  UUID
	DriverID int64
	Sim      string
	TrackID  string
	Car      string
	CarClass string
	Laps     []LapRow
}

// LapResult is what the ingest did. Accepted counts the laps this call stored,
// so a lap already held counts once across the two calls and not twice.
// Conflicts names the lap numbers already held with different content, and a
// non-empty Conflicts means nothing was stored.
// Stored names the lap numbers this call actually wrote, in the order they
// were sent. It is not derivable from Accepted: a batch that repeats a lap
// already held stores the rest and counts the repeat once, so "the first
// Accepted of them" is the wrong set. A caller that has to act on each new
// lap — publishing an event, say — needs to know which.
type LapResult struct {
	Accepted  int
	BestLapMs int
	Conflicts []int
	Stored    []int
}

// lapColumns is the column order the COPY writes in. It is a package variable
// rather than a literal at the call site so that it cannot drift from the
// values [lapSource] produces.
var lapColumns = []string{
	"stint_id", "number", "lap_ms", "kind", "started_at",
	"sim", "track_id", "car", "car_class", "driver_id", "content_sha256",
	"trace_codec", "trace", "corners",
}

// AppendLaps stores a batch of laps and maintains the reference table.
//
// Laps are append-only and idempotent on (stint_id, number): a repeated number
// with identical content is accepted and stored once, and with different
// content the whole batch is refused and the numbers that disagree are named.
// Refusing the whole batch rather than the offending lap is deliberate — a
// client whose lap 7 disagrees with the server's has a bug, and half-applying
// the rest of its batch would hide it.
//
// The insert is one COPY. Everything else in the path is a single statement
// against an index, so the cost of a forty-lap batch is four round trips and
// the bytes of the traces, which is what the fifty milliseconds this path is
// allowed are spent on.
//
// Two batches that carry the same lap number and arrive at once are serialised
// by the unique index: one commits and the other fails and is rolled back
// whole, so the client's retry sees the committed state and is answered
// normally.
func (t *Tx) AppendLaps(ctx context.Context, w LapWrite) (LapResult, error) {
	if len(w.Laps) == 0 {
		best, err := t.bestCleanLap(ctx, w.StintID)
		return LapResult{BestLapMs: best}, err
	}

	numbers := make([]int32, 0, len(w.Laps))
	for i := range w.Laps {
		numbers = append(numbers, int32(w.Laps[i].Number)) //nolint:gosec // G115: bounded by validation before it reaches here.
	}

	held, err := t.heldLaps(ctx, w.StintID, numbers)
	if err != nil {
		return LapResult{}, err
	}

	var conflicts []int
	fresh := make([]int, 0, len(w.Laps))
	for i := range w.Laps {
		sum, seen := held[w.Laps[i].Number]
		switch {
		case !seen:
			fresh = append(fresh, i)
		case !bytes.Equal(sum, w.Laps[i].ContentSum):
			conflicts = append(conflicts, w.Laps[i].Number)
		}
	}
	if len(conflicts) > 0 {
		return LapResult{Conflicts: conflicts}, nil
	}

	inserted := make([]int32, 0, len(fresh))
	stored := make([]int, 0, len(fresh))
	if len(fresh) > 0 {
		src := &lapSource{w: &w, order: fresh}
		n, copyErr := t.tx.CopyFrom(ctx, pgx.Identifier{"laps"}, lapColumns, src)
		if copyErr != nil {
			return LapResult{}, fmt.Errorf("db: cannot store the laps: %w", copyErr)
		}
		if int(n) != len(fresh) {
			return LapResult{}, fmt.Errorf("db: %d of %d laps were stored", n, len(fresh))
		}
		for _, i := range fresh {
			inserted = append(inserted, int32(w.Laps[i].Number)) //nolint:gosec // G115: bounded by validation before it reaches here.
			stored = append(stored, w.Laps[i].Number)
		}
		if refErr := t.refreshReference(ctx, w.StintID, inserted); refErr != nil {
			return LapResult{}, refErr
		}
	}

	best, err := t.bestCleanLap(ctx, w.StintID)
	if err != nil {
		return LapResult{}, err
	}
	return LapResult{Accepted: len(inserted), BestLapMs: best, Stored: stored}, nil
}

// heldLaps reads the fingerprints of the laps of this batch that the stint
// already holds. It is one index scan over the unique key the laps table
// already carries.
func (t *Tx) heldLaps(ctx context.Context, stint UUID, numbers []int32) (map[int][]byte, error) {
	rows, err := t.tx.Query(ctx,
		`SELECT number, content_sha256 FROM laps WHERE stint_id = $1 AND number = ANY($2::int[])`,
		stint.pg(), numbers)
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the laps already stored: %w", err)
	}
	defer rows.Close()

	held := make(map[int][]byte, len(numbers))
	for rows.Next() {
		var number int32
		var sum []byte
		if err := rows.Scan(&number, &sum); err != nil {
			return nil, fmt.Errorf("db: cannot read the laps already stored: %w", err)
		}
		held[int(number)] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: cannot read the laps already stored: %w", err)
	}
	return held, nil
}

// refreshReference maintains the best-per-bucket rows for the laps just
// stored, which is what makes GET /reference a key lookup instead of a sort
// over every lap ever driven.
//
// Two scopes are maintained here: the driver's own best, and the best anyone
// has driven in that car. The class-wide scope is an enterprise feature and no
// row is ever written for it, which is why GET /reference falls back rather
// than answering 404 when a client asks for it.
//
// Every bucket is keyed by the simulator the lap came from, which the lap
// carries because the stint did. Two simulators are two sets of buckets that
// never meet: the same driver's Spa best in iRacing and in Assetto Corsa
// Competizione are two rows, not one row the quicker of them wins.
//
// DISTINCT ON picks the best lap per bucket out of the batch before the upsert
// sees it, because ON CONFLICT DO UPDATE cannot touch one row twice in one
// statement, and a batch of forty laps at one track is exactly that case. A
// batch belongs to one stint and therefore to one simulator, so sim is constant
// across it; it is named in the key anyway, because what makes the bucket is
// the column and not the fact that one caller cannot vary it.
func (t *Tx) refreshReference(ctx context.Context, stint UUID, numbers []int32) error {
	const self = `
INSERT INTO reference_laps (scope, sim, track_id, car, car_class, driver_id, lap_id, lap_ms)
SELECT DISTINCT ON (l.driver_id, l.sim, l.track_id, l.car, l.car_class)
       'self', l.sim, l.track_id, l.car, l.car_class, l.driver_id, l.id, l.lap_ms
  FROM laps l
 WHERE l.stint_id = $1 AND l.number = ANY($2::int[]) AND l.kind = 'clean'
 ORDER BY l.driver_id, l.sim, l.track_id, l.car, l.car_class, l.lap_ms, l.id
    ON CONFLICT (driver_id, scope, sim, track_id, car, car_class) WHERE driver_id IS NOT NULL
    DO UPDATE SET lap_id = EXCLUDED.lap_id, lap_ms = EXCLUDED.lap_ms, updated_at = now()
     WHERE reference_laps.lap_ms > EXCLUDED.lap_ms`

	const car = `
INSERT INTO reference_laps (scope, sim, track_id, car, car_class, driver_id, lap_id, lap_ms)
SELECT DISTINCT ON (l.sim, l.track_id, l.car)
       'car', l.sim, l.track_id, l.car, '', NULL::bigint, l.id, l.lap_ms
  FROM laps l
 WHERE l.stint_id = $1 AND l.number = ANY($2::int[]) AND l.kind = 'clean'
 ORDER BY l.sim, l.track_id, l.car, l.lap_ms, l.id
    ON CONFLICT (scope, sim, track_id, car, car_class) WHERE driver_id IS NULL
    DO UPDATE SET lap_id = EXCLUDED.lap_id, lap_ms = EXCLUDED.lap_ms, updated_at = now()
     WHERE reference_laps.lap_ms > EXCLUDED.lap_ms`

	for _, stmt := range [...]string{self, car} {
		if _, err := t.tx.Exec(ctx, stmt, stint.pg(), numbers); err != nil {
			return fmt.Errorf("db: cannot maintain the reference laps: %w", err)
		}
	}
	return nil
}

func (t *Tx) bestCleanLap(ctx context.Context, stint UUID) (int, error) {
	best, err := t.q.BestCleanLapMs(ctx, stint.pg())
	if err != nil {
		return 0, fmt.Errorf("db: cannot read the best lap: %w", err)
	}
	return int(best), nil
}

// lapSource feeds the COPY without building a slice of rows first: pgx asks for
// one row at a time and this hands back a reused slice of fourteen values, so a
// forty-lap batch allocates once rather than forty times.
type lapSource struct {
	w     *LapWrite
	order []int
	at    int
	row   [14]any
}

func (s *lapSource) Next() bool {
	s.at++
	return s.at <= len(s.order)
}

func (s *lapSource) Values() ([]any, error) {
	l := &s.w.Laps[s.order[s.at-1]]
	s.row = [14]any{
		s.w.StintID.pg(),
		int32(l.Number), //nolint:gosec // G115: bounded by validation before it reaches here.
		int32(l.LapMs),  //nolint:gosec // G115: bounded by validation before it reaches here.
		l.Kind,
		pgtype.Timestamptz{Time: l.StartedAt, Valid: true},
		s.w.Sim,
		s.w.TrackID,
		s.w.Car,
		s.w.CarClass,
		s.w.DriverID,
		l.ContentSum,
		int16(l.TraceCodec), //nolint:gosec // G115: the codec version is a small constant.
		l.Trace,
		cornersOrEmptyJSON(l.Corners),
	}
	return s.row[:], nil
}

func (s *lapSource) Err() error { return nil }

// emptyCorners is the document a lap with no corner analysis stores. The COPY
// names every column, so the column's own default never applies and this is
// what stands in for it.
var emptyCorners = []byte("[]")

// cornersOrEmptyJSON keeps the NOT NULL promise of the column for a caller that
// built a row without corners, which every caller that predates them did.
func cornersOrEmptyJSON(corners []byte) []byte {
	if len(corners) == 0 {
		return emptyCorners
	}
	return corners
}
