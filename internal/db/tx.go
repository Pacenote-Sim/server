package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// Tx is one database transaction, handed to the callback of
// [Store.Idempotent]. It carries the writes an API request performs and nothing
// else: pgx stays inside this package, so a handler cannot open a transaction
// of its own, cannot leave one open, and cannot run a statement this package
// has not reviewed.
type Tx struct {
	tx pgx.Tx
	q  *gen.Queries
}

// Stint is one continuous capture, as the database holds it. The four
// denormalised columns are here because the laps written against this stint
// copy them down, which is what lets the reference lookup be one index entry.
//
// Sim is one of them, and it is the one that keeps the others honest: two
// simulators agree about the slug of a circuit and the name of a car and agree
// about nothing underneath it, so a lap is only ever compared against a lap
// from the same simulator.
type Stint struct {
	ID         UUID
	DriverID   int64
	Sim        string
	TrackID    string
	Car        string
	CarClass   string
	StartedAt  time.Time
	FinishedAt *time.Time
	// Setup is the car's setup as the client sent it, still encoded, or nil
	// when the client sent none. It is carried as bytes because nothing here
	// looks inside it: the API decodes it once, on the one path that turns it
	// into the facts a plugin is given.
	Setup []byte
}

// StintWrite is the body of PUT /stints/{id}, ready to store.
type StintWrite struct {
	ID          UUID
	DriverID    int64
	Sim         string
	Track       string
	TrackID     string
	Car         string
	CarClass    string
	SessionType string
	Sectors     []float64
	StartedAt   time.Time
	// Setup is the encoded car setup, or nil. Nil clears any setup the stint
	// already had, because the client is the only thing that knows what the
	// car was set to and a client that has stopped seeing one is saying so.
	Setup []byte
}

// UpsertStint creates the stint or updates its mutable fields, and reports
// nothing but success: the identifier came from the client and is already known
// to it.
//
// A stint identifier that already belongs to another driver is [ErrNotFound].
// Two clients cannot collide on a UUIDv7 by accident, so this is the answer to
// somebody trying one, and "no such stint" is the right thing to tell them
// rather than "that one is not yours".
func (t *Tx) UpsertStint(ctx context.Context, w StintWrite) error {
	sectors := make([]float32, 0, len(w.Sectors))
	for _, s := range w.Sectors {
		sectors = append(sectors, float32(s))
	}
	_, err := t.q.UpsertStint(ctx, gen.UpsertStintParams{
		ID:          w.ID.pg(),
		DriverID:    w.DriverID,
		Sim:         w.Sim,
		TrackID:     w.TrackID,
		Track:       w.Track,
		Car:         w.Car,
		CarClass:    w.CarClass,
		SessionType: w.SessionType,
		Sectors:     sectors,
		StartedAt:   pgtype.Timestamptz{Time: w.StartedAt, Valid: true},
		Setup:       w.Setup,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("db: cannot store the stint: %w", err)
	}
	return nil
}

// Stint reads one of this driver's stints. It is the ownership check every
// write against a stint makes before it touches anything.
func (t *Tx) Stint(ctx context.Context, id UUID, driverID int64) (Stint, error) {
	row, err := t.q.StintForDriver(ctx, gen.StintForDriverParams{ID: id.pg(), DriverID: driverID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stint{}, ErrNotFound
		}
		return Stint{}, fmt.Errorf("db: cannot read the stint: %w", err)
	}
	return Stint{
		ID:         uuidFromPg(row.ID),
		DriverID:   row.DriverID,
		Sim:        row.Sim,
		TrackID:    row.TrackID,
		Car:        row.Car,
		CarClass:   row.CarClass,
		StartedAt:  row.StartedAt.Time,
		FinishedAt: optionalTime(row.FinishedAt),
		Setup:      row.Setup,
	}, nil
}

// Stint reads one of a driver's stints outside a transaction, for the reads
// that check ownership without writing anything.
func (s *Store) Stint(ctx context.Context, id UUID, driverID int64) (Stint, error) {
	row, err := s.q.StintForDriver(ctx, gen.StintForDriverParams{ID: id.pg(), DriverID: driverID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stint{}, ErrNotFound
		}
		return Stint{}, fmt.Errorf("db: cannot read the stint: %w", err)
	}
	return Stint{
		ID:         uuidFromPg(row.ID),
		DriverID:   row.DriverID,
		Sim:        row.Sim,
		TrackID:    row.TrackID,
		Car:        row.Car,
		CarClass:   row.CarClass,
		StartedAt:  row.StartedAt.Time,
		FinishedAt: optionalTime(row.FinishedAt),
		Setup:      row.Setup,
	}, nil
}

// SummaryWrite is the body of PUT /stints/{id}/summary, ready to store.
// Conditions and CarState are already-encoded JSON documents, and BestTrace is
// already compressed by the protocol codec.
type SummaryWrite struct {
	StintID        UUID
	Laps           int
	Incidents      int
	BestLapMs      int
	AvgLapMs       int
	ConsistencyPct int
	TopSpeedKmh    int
	Conditions     []byte
	CarState       []byte
	BestTrace      []byte
	BestTraceCodec int
	FinishedAt     *time.Time
}

// ReplaceSummary writes the whole summary document, replacing whatever was
// there. A present FinishedAt also closes the stint, which is what makes the
// last summary of a session the one that ends it.
func (t *Tx) ReplaceSummary(ctx context.Context, w SummaryWrite) error {
	finished := pgtype.Timestamptz{}
	if w.FinishedAt != nil {
		finished = pgtype.Timestamptz{Time: *w.FinishedAt, Valid: true}
	}
	err := t.q.ReplaceStintSummary(ctx, gen.ReplaceStintSummaryParams{
		StintID:        w.StintID.pg(),
		Laps:           int32(w.Laps),           //nolint:gosec // G115: bounded by validation before it reaches here.
		Incidents:      int32(w.Incidents),      //nolint:gosec // G115: bounded by validation before it reaches here.
		BestLapMs:      int32(w.BestLapMs),      //nolint:gosec // G115: bounded by validation before it reaches here.
		AvgLapMs:       int32(w.AvgLapMs),       //nolint:gosec // G115: bounded by validation before it reaches here.
		ConsistencyPct: int32(w.ConsistencyPct), //nolint:gosec // G115: bounded by validation before it reaches here.
		TopSpeedKmh:    int32(w.TopSpeedKmh),    //nolint:gosec // G115: bounded by validation before it reaches here.
		Conditions:     w.Conditions,
		CarState:       w.CarState,
		BestTrace:      w.BestTrace,
		BestTraceCodec: int16(w.BestTraceCodec), //nolint:gosec // G115: the codec version is a small constant.
		FinishedAt:     finished,
	})
	if err != nil {
		return fmt.Errorf("db: cannot store the summary: %w", err)
	}
	if w.FinishedAt != nil {
		if err := t.q.FinishStint(ctx, gen.FinishStintParams{ID: w.StintID.pg(), FinishedAt: finished}); err != nil {
			return fmt.Errorf("db: cannot close the stint: %w", err)
		}
	}
	return nil
}
