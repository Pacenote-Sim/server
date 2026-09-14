package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

// migrationsFS carries the schema inside the binary. An operator downloads one
// file; there is no migrations directory to copy alongside it and no migrate
// command to run before starting (D-4).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationState is what the admin panel's overview shows about the schema.
type MigrationState struct {
	// Version is the migration the database is at.
	Version int64
	// Target is the newest migration this binary carries.
	Target int64
	// Applied and Pending count the migrations either side of the line.
	Applied, Pending int
	// LastAppliedAt is when the newest applied migration ran, or the zero time
	// if nothing has been applied.
	LastAppliedAt time.Time
}

// UpToDate reports whether the database carries every migration this binary
// knows about.
func (m MigrationState) UpToDate() bool { return m.Pending == 0 }

// Migrate applies every migration the binary carries that the database has not
// already run. It is safe to call on every startup and safe to call from two
// instances at once: a PostgreSQL advisory lock means the second waits rather
// than racing.
func (s *Store) Migrate(ctx context.Context) error {
	p, closeProvider, err := s.provider()
	if err != nil {
		return err
	}
	defer closeProvider()

	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("db: the migrations did not apply: %w", err)
	}
	for _, r := range results {
		s.log.LogAttrs(ctx, levelInfo, "migration applied",
			attrString("migration", r.Source.Path),
			attrInt64("version", r.Source.Version),
			attrString("took", r.Duration.Round(time.Millisecond).String()),
		)
	}
	return nil
}

// MigrationState reports where the database's schema sits relative to this
// binary's.
func (s *Store) MigrationState(ctx context.Context) (MigrationState, error) {
	p, closeProvider, err := s.provider()
	if err != nil {
		return MigrationState{}, err
	}
	defer closeProvider()

	current, target, err := p.GetVersions(ctx)
	if err != nil {
		return MigrationState{}, fmt.Errorf("db: cannot read the migration state: %w", err)
	}
	st := MigrationState{Version: current, Target: target}
	statuses, err := p.Status(ctx)
	if err != nil {
		return st, fmt.Errorf("db: cannot read the migration state: %w", err)
	}
	for _, m := range statuses {
		if m.State == goose.StateApplied {
			st.Applied++
			at := wallClock(m.AppliedAt)
			if at.After(st.LastAppliedAt) {
				st.LastAppliedAt = at
			}
			continue
		}
		st.Pending++
	}
	return st, nil
}

// wallClock reinterprets one of goose's timestamps in the server's own zone.
//
// goose keeps its history in a column with no time zone, written with the
// database session's clock. The driver hands those back labelled UTC, so
// showing one would shift it by the offset — an hour or two out on an admin
// page sitting next to a log file. Keeping the same wall clock and changing
// only the label puts it back. It is right whenever the database and the server
// agree about the time zone, which is every deployment where both are the
// operator's own machines, and no worse than the alternative when they do not.
func wallClock(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
}

// SchemaInstalled reports whether the schema has been created at all. It is the
// question that has to be answerable before setup_state can be read, so it is
// asked with to_regclass rather than by selecting from a table that may not
// exist — a failed SELECT would abort the surrounding transaction.
func (s *Store) SchemaInstalled(ctx context.Context) (bool, error) {
	var installed bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.setup_state') IS NOT NULL`).Scan(&installed)
	if err != nil {
		return false, classify(err, s.pool.Config().ConnConfig)
	}
	return installed, nil
}

// provider builds a goose provider over this pool. The returned function closes
// the database/sql handle that wraps the pool; the pool itself is untouched.
func (s *Store) provider() (*goose.Provider, func(), error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("db: the embedded migrations are unreadable: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(s.pool)
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockTimeout(2, 60), // probe every 2 s for up to two minutes
	)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("db: cannot prepare the migration lock: %w", err)
	}
	p, err := goose.NewProvider(database.DialectPostgres, sqlDB, sub, goose.WithSessionLocker(locker))
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("db: cannot prepare the migrations: %w", err)
	}
	return p, func() { _ = sqlDB.Close() }, nil
}

// MigrationSources lists the migrations this binary carries, oldest first. The
// admin panel shows them; a test asserts the list is not empty, which is the
// cheapest possible guard against an embed pattern that silently matches
// nothing.
func MigrationSources() ([]string, error) {
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("db: the embedded migrations are unreadable: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("db: the binary carries no migrations")
	}
	return entries, nil
}
