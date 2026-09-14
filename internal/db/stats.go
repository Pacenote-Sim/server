package db

import (
	"context"
	"fmt"
)

// Stats is what the admin panel's overview page shows about the data.
type Stats struct {
	// DatabaseBytes is what PostgreSQL reports for the whole database,
	// including its indexes and its own overhead.
	DatabaseBytes int64
	// TraceBytes is how much of that is compressed lap traces. They are the
	// bulk of the data and an operator should be able to see that plainly.
	TraceBytes int64
	// Drivers, Devices, Stints and Laps are row counts. Devices counts only
	// the ones that have not been revoked.
	Drivers, Devices, Stints, Laps int64
}

// Stats reads the overview counters. Each is a single statement and the page is
// loaded by someone waiting for it, so they run in order rather than fanning
// out across the pool.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	steps := []struct {
		name string
		run  func(context.Context) (int64, error)
		into *int64
	}{
		{"database size", s.q.DatabaseSizeBytes, &out.DatabaseBytes},
		{"trace size", s.q.TraceBytes, &out.TraceBytes},
		{"drivers", s.q.CountDrivers, &out.Drivers},
		{"devices", s.q.CountActiveDevices, &out.Devices},
		{"stints", s.q.CountStints, &out.Stints},
		{"laps", s.q.CountLaps, &out.Laps},
	}
	for _, step := range steps {
		v, err := step.run(ctx)
		if err != nil {
			return out, fmt.Errorf("db: cannot read the %s: %w", step.name, err)
		}
		*step.into = v
	}
	return out, nil
}
