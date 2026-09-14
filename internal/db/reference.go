package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db/gen"
)

// ReferenceQuery asks for the lap the coach compares a driver against.
//
// Sim is required and is matched exactly. A lap is only ever compared against a
// lap from the same simulator: two simulators arrive at the same slug for the
// same circuit and the same name for the same car, and mean two different
// things by both, so serving one for the other would tell a driver they are
// seconds off a time nobody in their simulator has ever set.
//
// Preference is the scopes to consider, best first. The caller builds it from
// the scope the client asked for and the ones narrower than it, so a server
// with no class data still answers with the driver's own best rather than
// nothing at all.
type ReferenceQuery struct {
	DriverID   int64
	Sim        string
	TrackID    string
	Car        string
	CarClass   string
	Preference []wire.Scope
}

// ReferenceResult is the lap that was found, and the scope it actually came
// from, which may be narrower than the one asked for.
type ReferenceResult struct {
	Scope      wire.Scope
	LapMs      int
	DriverID   int64
	DriverName string
	TraceCodec int
	Trace      []byte
}

// ReferenceLap finds the best lap in the narrowest scope the server has, within
// the caller's preference. It is [ErrNotFound] when there is none, which is the
// contract's 404 and an ordinary answer for a track nobody has driven yet.
//
// It is one statement: three lookups against the two partial unique indexes on
// reference_laps, then two primary-key joins to fetch the winner's trace and
// the name to show beside it. Nothing here scans laps.
func (s *Store) ReferenceLap(ctx context.Context, q ReferenceQuery) (ReferenceResult, error) {
	pref := make([]string, 0, len(q.Preference))
	for _, sc := range q.Preference {
		pref = append(pref, string(sc))
	}
	row, err := s.q.ReferenceLap(ctx, gen.ReferenceLapParams{
		DriverID:   q.DriverID,
		Sim:        q.Sim,
		TrackID:    q.TrackID,
		Car:        q.Car,
		CarClass:   q.CarClass,
		Preference: pref,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReferenceResult{}, ErrNotFound
		}
		return ReferenceResult{}, fmt.Errorf("db: cannot read the reference lap: %w", err)
	}
	return ReferenceResult{
		Scope:      wire.Scope(row.Scope),
		LapMs:      int(row.LapMs),
		DriverID:   row.DriverID,
		DriverName: row.DriverName,
		TraceCodec: int(row.TraceCodec),
		Trace:      row.Trace,
	}, nil
}

// CountLapsForStint reports how many laps a stint holds. The admin panel and
// the tests both want it; nothing on the request path does.
func (s *Store) CountLapsForStint(ctx context.Context, id UUID) (int64, error) {
	n, err := s.q.CountLapsForStint(ctx, id.pg())
	if err != nil {
		return 0, fmt.Errorf("db: cannot count the laps: %w", err)
	}
	return n, nil
}
