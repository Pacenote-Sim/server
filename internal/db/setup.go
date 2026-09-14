package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db/gen"
)

// ErrSetupAlreadyComplete is returned by [Store.CompleteSetup] when the
// database already holds a finished setup. It is the answer to the second of
// two concurrent attempts, and to anyone who deleted a data directory hoping
// the wizard would come back.
var ErrSetupAlreadyComplete = errors.New("db: setup has already been completed")

// ErrAdminEmailTaken is returned when the administrator's email address is
// already in use. During setup it cannot happen — there are no accounts yet —
// but the same query creates accounts later, so the case is named.
var ErrAdminEmailTaken = errors.New("db: that email address already has an account")

// SetupState is the answer to the only question that matters at startup: may
// the setup wizard run?
//
// The rule is that the database decides. A data directory is a cache of a
// connection string and can be deleted or mounted on a volume that does not
// survive a restart; if its absence reopened the wizard, anyone who could
// delete it would be handed the administrator account of a database that
// already has one.
type SetupState struct {
	// Installed reports whether the schema has been created at all.
	Installed bool
	// Complete reports whether setup has finished. When it is true the wizard
	// must not run, whatever is or is not on disk.
	Complete bool
	// CompletedAt and CompletedBy are the moment it finished and the email
	// address of the administrator it created.
	CompletedAt time.Time
	CompletedBy string
	// ServerVersion is the build that ran the setup.
	ServerVersion string
}

// SetupState reads the completion marker. A database with no schema is
// Installed false and Complete false, which is a first run; a database with a
// schema but no marker is a setup that was interrupted, which is also a first
// run because the transaction that would have created an administrator did not
// commit.
func (s *Store) SetupState(ctx context.Context) (SetupState, error) {
	installed, err := s.SchemaInstalled(ctx)
	if err != nil {
		return SetupState{}, err
	}
	if !installed {
		return SetupState{}, nil
	}
	st := SetupState{Installed: true}
	row := s.pool.QueryRow(ctx,
		`SELECT completed_at, completed_by, server_version FROM setup_state WHERE id = true`)
	var completedAt time.Time
	err = row.Scan(&completedAt, &st.CompletedBy, &st.ServerVersion)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return st, nil
	case err != nil:
		return st, classify(err, s.pool.Config().ConnConfig)
	}
	st.Complete = true
	st.CompletedAt = completedAt
	return st, nil
}

// SetupRequest is everything the wizard collected, ready to be committed.
type SetupRequest struct {
	// AdminEmail is the administrator's address, as typed.
	AdminEmail string
	// AdminPasswordHash is the argon2id hash. The plain password never
	// reaches this package.
	AdminPasswordHash string
	// Settings are the organisation's own settings, including the sealed
	// settings the wizard collected. No vendor credential is among them: a
	// fresh installation stores none, and a plugin asks for its own.
	Settings config.Settings
	// ServerVersion is the build running the wizard, recorded for diagnostics.
	ServerVersion string
}

// CompleteSetup writes the completion marker, the first administrator and the
// organisation's settings in one transaction. Either all of it lands or none of
// it does, so a setup interrupted halfway leaves the server in setup mode with
// no administrator rather than half-configured.
//
// The marker's primary key is a single fixed value, so two requests arriving
// together contend on it: one commits, and the other is
// [ErrSetupAlreadyComplete] with its administrator rolled back. That is the
// guarantee that exactly one account is ever created this way.
func (s *Store) CompleteSetup(ctx context.Context, req SetupRequest) (time.Time, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return time.Time{}, classify(err, s.pool.Config().ConnConfig)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	// The marker goes first so that the loser of a race blocks here, before it
	// has written anything, rather than after.
	completedAt, err := q.MarkSetupComplete(ctx, gen.MarkSetupCompleteParams{
		CompletedBy:   req.AdminEmail,
		ServerVersion: req.ServerVersion,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return time.Time{}, ErrSetupAlreadyComplete
		}
		return time.Time{}, fmt.Errorf("db: cannot record that setup finished: %w", err)
	}

	if _, err := q.CreateAdmin(ctx, gen.CreateAdminParams{
		Email:        req.AdminEmail,
		PasswordHash: req.AdminPasswordHash,
	}); err != nil {
		if isUniqueViolation(err) {
			return time.Time{}, ErrAdminEmailTaken
		}
		return time.Time{}, fmt.Errorf("db: cannot create the administrator: %w", err)
	}

	if err := saveSettings(ctx, q, req.Settings); err != nil {
		return time.Time{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return time.Time{}, ErrSetupAlreadyComplete
		}
		return time.Time{}, fmt.Errorf("db: cannot finish setup: %w", err)
	}
	return completedAt.Time, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
