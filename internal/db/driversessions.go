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

// A driver signed in to a browser.
//
// The shape is the admin session's, for the same reasons: only the digest of
// the token is stored, so a dump of this database cannot be used to sign in as
// anybody, and expiry is enforced by the lookup rather than by a sweep, so
// there is no window in which an old cookie still works.
//
// What is different is who decided. An administrator proves themselves to this
// server with a password this server holds; a driver proves themselves to a
// plugin, which then tells this server who they are. [DriverSession.SignedInBy]
// records which plugin said so, because an operator looking at a session they
// did not expect needs to know which one vouched for it.

// DriverSession is one browser signed in as one driver.
type DriverSession struct {
	ID       int64
	DriverID int64
	// Slug and Name are the driver's, so the caller need not read the row.
	Slug string
	Name string
	// CreatedAt, LastSeenAt and ExpiresAt are the session's own life.
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	// UserAgent is what the browser said it was, cut to something a table cell
	// can hold.
	UserAgent string
	// SignedInBy is the plugin that authenticated this driver.
	SignedInBy string
}

// CreateDriverSession records a signed-in browser.
func (s *Store) CreateDriverSession(ctx context.Context, driverID int64, sum []byte,
	expires time.Time, userAgent, by string,
) error {
	err := s.q.CreateDriverSession(ctx, gen.CreateDriverSessionParams{
		DriverID:    driverID,
		TokenSha256: sum,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
		UserAgent:   userAgent,
		SignedInBy:  by,
	})
	if err != nil {
		return fmt.Errorf("db: cannot store the driver session: %w", err)
	}
	return nil
}

// DriverSessionByToken finds a live session by its token's digest, with the
// driver it belongs to. An expired one is [ErrNotFound]: the query enforces
// that, so nothing downstream has to remember to check.
func (s *Store) DriverSessionByToken(ctx context.Context, sum []byte) (DriverSession, error) {
	row, err := s.q.DriverSessionByToken(ctx, sum)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return DriverSession{}, ErrNotFound
	case err != nil:
		return DriverSession{}, fmt.Errorf("db: cannot read the driver session: %w", err)
	}
	return DriverSession{
		ID:         row.ID,
		DriverID:   row.DriverID,
		Slug:       row.Slug,
		Name:       row.Name,
		CreatedAt:  row.CreatedAt.Time,
		LastSeenAt: row.LastSeenAt.Time,
		ExpiresAt:  row.ExpiresAt.Time,
		UserAgent:  row.UserAgent,
		SignedInBy: row.SignedInBy,
	}, nil
}

// TouchDriverSession extends a session that is being used.
func (s *Store) TouchDriverSession(ctx context.Context, sum []byte, expires time.Time) error {
	err := s.q.TouchDriverSession(ctx, gen.TouchDriverSessionParams{
		TokenSha256: sum,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("db: cannot extend the driver session: %w", err)
	}
	return nil
}

// DeleteDriverSession ends one browser's session, which is a driver signing out.
func (s *Store) DeleteDriverSession(ctx context.Context, sum []byte) error {
	if err := s.q.DeleteDriverSession(ctx, sum); err != nil {
		return fmt.Errorf("db: cannot end the driver session: %w", err)
	}
	return nil
}

// DeleteDriverSessionByID ends one session the operator picked off a page. It
// reports whether there was one, so pressing the button twice is not an error
// the second time.
func (s *Store) DeleteDriverSessionByID(ctx context.Context, id int64) (bool, error) {
	n, err := s.q.DeleteDriverSessionByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("db: cannot end that driver session: %w", err)
	}
	return n > 0, nil
}

// DeleteDriverSessionsForDriver signs one driver out of everything. It is the
// lost-laptop action, for the person rather than for the machine.
func (s *Store) DeleteDriverSessionsForDriver(ctx context.Context, driverID int64) (int64, error) {
	n, err := s.q.DeleteDriverSessionsForDriver(ctx, driverID)
	if err != nil {
		return 0, fmt.Errorf("db: cannot end that driver's sessions: %w", err)
	}
	return n, nil
}

// DriverSessionsForDriver is every browser this driver is signed in to, newest
// use first.
func (s *Store) DriverSessionsForDriver(ctx context.Context, driverID int64) ([]DriverSession, error) {
	rows, err := s.q.DriverSessionsForDriver(ctx, driverID)
	if err != nil {
		return nil, fmt.Errorf("db: cannot read that driver's sessions: %w", err)
	}
	out := make([]DriverSession, 0, len(rows))
	for _, row := range rows {
		out = append(out, DriverSession{
			ID:         row.ID,
			DriverID:   row.DriverID,
			CreatedAt:  row.CreatedAt.Time,
			LastSeenAt: row.LastSeenAt.Time,
			ExpiresAt:  row.ExpiresAt.Time,
			UserAgent:  row.UserAgent,
			SignedInBy: row.SignedInBy,
		})
	}
	return out, nil
}

// DeleteExpiredDriverSessions clears out what has run out. Nothing depends on
// it having run — the lookup refuses an expired session either way — so this is
// housekeeping and not a check.
func (s *Store) DeleteExpiredDriverSessions(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredDriverSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: cannot clear out the expired driver sessions: %w", err)
	}
	return n, nil
}

// DriverBySlug finds a driver by the slug a plugin names when it signs one in.
func (s *Store) DriverBySlug(ctx context.Context, slug string) (Driver, error) {
	row, err := s.q.DriverBySlug(ctx, slug)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Driver{}, ErrNotFound
	case err != nil:
		return Driver{}, fmt.Errorf("db: cannot read that driver: %w", err)
	}
	return driverOf(row), nil
}
