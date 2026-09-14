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

// ErrNotFound is returned when a row that was asked for by key is not there. It
// is separate from a failure, because "no such session" is an ordinary answer
// on a request path and a database fault is not.
var ErrNotFound = errors.New("db: not found")

// Admin is an administrator of the panel.
type Admin struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

// AdminSession is a signed-in browser.
type AdminSession struct {
	ID         int64
	AdminID    int64
	Email      string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// AdminByEmail finds an administrator by email address, case-insensitively.
func (s *Store) AdminByEmail(ctx context.Context, email string) (Admin, error) {
	row, err := s.q.AdminByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Admin{}, ErrNotFound
		}
		return Admin{}, fmt.Errorf("db: cannot read the administrator: %w", err)
	}
	return Admin{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		CreatedAt:    row.CreatedAt.Time,
		LastLoginAt:  optionalTime(row.LastLoginAt),
	}, nil
}

// AdminByID finds an administrator by primary key.
func (s *Store) AdminByID(ctx context.Context, id int64) (Admin, error) {
	row, err := s.q.AdminByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Admin{}, ErrNotFound
		}
		return Admin{}, fmt.Errorf("db: cannot read the administrator: %w", err)
	}
	return Admin{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		CreatedAt:    row.CreatedAt.Time,
		LastLoginAt:  optionalTime(row.LastLoginAt),
	}, nil
}

// CountAdmins reports how many administrator accounts exist. It is a
// diagnostic, not a gate: whether setup may run is [Store.SetupState]'s answer.
func (s *Store) CountAdmins(ctx context.Context) (int64, error) {
	n, err := s.q.CountAdmins(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: cannot count the administrators: %w", err)
	}
	return n, nil
}

// TouchAdminLogin records a successful sign-in.
func (s *Store) TouchAdminLogin(ctx context.Context, id int64) error {
	if err := s.q.TouchAdminLogin(ctx, id); err != nil {
		return fmt.Errorf("db: cannot record the sign-in: %w", err)
	}
	return nil
}

// SetAdminPasswordHash replaces an administrator's password hash. It is called
// after a password change, and after a successful sign-in against a hash made
// with weaker parameters than the current ones.
func (s *Store) SetAdminPasswordHash(ctx context.Context, id int64, hash string) error {
	if err := s.q.SetAdminPasswordHash(ctx, gen.SetAdminPasswordHashParams{ID: id, PasswordHash: hash}); err != nil {
		return fmt.Errorf("db: cannot store the new password: %w", err)
	}
	return nil
}

// CreateAdminSession stores a session. sum is the SHA-256 of the cookie value;
// the cookie value itself is never stored, so a database dump cannot be used to
// sign in as anyone.
func (s *Store) CreateAdminSession(ctx context.Context, adminID int64, sum []byte, expires time.Time, userAgent string) error {
	_, err := s.q.CreateAdminSession(ctx, gen.CreateAdminSessionParams{
		AdminID:     adminID,
		TokenSha256: sum,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
		UserAgent:   userAgent,
	})
	if err != nil {
		return fmt.Errorf("db: cannot store the session: %w", err)
	}
	return nil
}

// AdminSessionByToken finds an unexpired session by the digest of its cookie.
func (s *Store) AdminSessionByToken(ctx context.Context, sum []byte) (AdminSession, error) {
	row, err := s.q.AdminSessionByToken(ctx, sum)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminSession{}, ErrNotFound
		}
		return AdminSession{}, fmt.Errorf("db: cannot read the session: %w", err)
	}
	return AdminSession{
		ID:         row.ID,
		AdminID:    row.AdminID,
		Email:      row.Email,
		CreatedAt:  row.CreatedAt.Time,
		LastSeenAt: row.LastSeenAt.Time,
		ExpiresAt:  row.ExpiresAt.Time,
	}, nil
}

// TouchAdminSession extends a session that is in use.
func (s *Store) TouchAdminSession(ctx context.Context, sum []byte, expires time.Time) error {
	err := s.q.TouchAdminSession(ctx, gen.TouchAdminSessionParams{
		TokenSha256: sum,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("db: cannot extend the session: %w", err)
	}
	return nil
}

// DeleteAdminSession signs one browser out.
func (s *Store) DeleteAdminSession(ctx context.Context, sum []byte) error {
	if err := s.q.DeleteAdminSession(ctx, sum); err != nil {
		return fmt.Errorf("db: cannot end the session: %w", err)
	}
	return nil
}

// DeleteAdminSessionsForAdmin signs every browser of one administrator out,
// which is what a password change does.
func (s *Store) DeleteAdminSessionsForAdmin(ctx context.Context, adminID int64) (int64, error) {
	n, err := s.q.DeleteAdminSessionsForAdmin(ctx, adminID)
	if err != nil {
		return 0, fmt.Errorf("db: cannot end the sessions: %w", err)
	}
	return n, nil
}

// DeleteExpiredAdminSessions removes sessions that have run out. Expiry is
// enforced by the lookup, so this is housekeeping rather than security.
func (s *Store) DeleteExpiredAdminSessions(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredAdminSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: cannot clear out the expired sessions: %w", err)
	}
	return n, nil
}

func optionalTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
