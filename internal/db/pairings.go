package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db/gen"
)

// PairingTTL is how long a device code stays good, and it is the value
// POST /pair/start reports as expires_in_s. Ten minutes is long enough for a
// driver to find the operator and short enough that an abandoned code is not
// sitting there tomorrow.
const PairingTTL = 10 * time.Minute

// ErrPairingSpent is returned when a pairing has already handed its token over.
// A device-code grant issues exactly once: a second poll is answered "approved"
// with no token, and a client that has lost its token pairs again.
var ErrPairingSpent = errors.New("db: that pairing has already issued its token")

// Pairing is one device-code grant in progress.
type Pairing struct {
	ID        int64
	UserCode  string
	Status    wire.PairStatus
	DriverID  *int64
	DeviceID  *int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether the code has run out, whatever the stored status
// says. The status is corrected lazily, by the poll that notices.
func (p Pairing) Expired(now time.Time) bool { return !now.Before(p.ExpiresAt) }

// CreatePairing opens a device-code grant. sum is the SHA-256 of the device
// code; the code itself is returned to the client once and never stored.
func (s *Store) CreatePairing(ctx context.Context, sum []byte, userCode string, expires time.Time) (Pairing, error) {
	row, err := s.q.CreatePairing(ctx, gen.CreatePairingParams{
		DeviceCodeSha256: sum,
		UserCode:         userCode,
		ExpiresAt:        pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return Pairing{}, fmt.Errorf("db: cannot open the pairing: %w", err)
	}
	return Pairing{
		ID:        row.ID,
		UserCode:  row.UserCode,
		Status:    wire.StatusPending,
		CreatedAt: row.CreatedAt.Time,
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// PairingByDeviceCode finds the grant a polling client is asking about.
func (s *Store) PairingByDeviceCode(ctx context.Context, sum []byte) (Pairing, error) {
	row, err := s.q.PairingByDeviceCode(ctx, sum)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Pairing{}, ErrNotFound
		}
		return Pairing{}, fmt.Errorf("db: cannot read the pairing: %w", err)
	}
	return Pairing{
		ID:        row.ID,
		UserCode:  row.UserCode,
		Status:    wire.PairStatus(row.Status),
		DriverID:  row.DriverID,
		DeviceID:  row.DeviceID,
		CreatedAt: row.CreatedAt.Time,
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// ListPendingPairings is what the admin panel shows: the grants waiting for a
// decision, oldest first, with the user code an operator matches against what
// the driver read out.
func (s *Store) ListPendingPairings(ctx context.Context) ([]Pairing, error) {
	rows, err := s.q.ListPendingPairings(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: cannot list the pairing requests: %w", err)
	}
	out := make([]Pairing, 0, len(rows))
	for i := range rows {
		out = append(out, Pairing{
			ID:        rows[i].ID,
			UserCode:  rows[i].UserCode,
			Status:    wire.StatusPending,
			CreatedAt: rows[i].CreatedAt.Time,
			ExpiresAt: rows[i].ExpiresAt.Time,
		})
	}
	return out, nil
}

// DecidePairing records an operator's approval or denial. It reports whether a
// pending, unexpired grant was actually changed, so approving one twice — two
// tabs, or a double click — is not an error and does not undo anything.
func (s *Store) DecidePairing(ctx context.Context, id int64, status wire.PairStatus, driverID *int64, by string) (bool, error) {
	if status != wire.StatusApproved && status != wire.StatusDenied {
		return false, fmt.Errorf("db: %q is not a decision", string(status))
	}
	n, err := s.q.DecidePairing(ctx, gen.DecidePairingParams{
		ID:        id,
		Status:    string(status),
		DriverID:  driverID,
		DecidedBy: by,
	})
	if err != nil {
		return false, fmt.Errorf("db: cannot record the decision: %w", err)
	}
	return n > 0, nil
}

// ExpirePairing marks a grant expired, which the poll that noticed does so that
// the admin panel stops offering a decision on a code nobody can use.
func (s *Store) ExpirePairing(ctx context.Context, id int64) error {
	if err := s.q.ExpirePairing(ctx, id); err != nil {
		return fmt.Errorf("db: cannot expire the pairing: %w", err)
	}
	return nil
}

// IssueDeviceForPairing mints the device a successful poll returns, once.
//
// The row is locked for the length of the transaction, so two polls arriving
// together cannot both mint one: the second waits, sees the device already
// attached, and is answered [ErrPairingSpent]. That is the whole reason this is
// a transaction and not three statements.
func (s *Store) IssueDeviceForPairing(ctx context.Context, pairingID int64, sum []byte, prefix, label string) (Device, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Device{}, classify(err, s.pool.Config().ConnConfig)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var driverID *int64
	var deviceID *int64
	err = tx.QueryRow(ctx,
		`SELECT driver_id, device_id FROM pairings WHERE id = $1 FOR UPDATE`, pairingID).
		Scan(&driverID, &deviceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Device{}, ErrNotFound
	case err != nil:
		return Device{}, fmt.Errorf("db: cannot read the pairing: %w", err)
	case deviceID != nil:
		return Device{}, ErrPairingSpent
	case driverID == nil:
		return Device{}, errors.New("db: that pairing was approved without a driver")
	}

	q := s.q.WithTx(tx)
	row, err := q.CreateDevice(ctx, gen.CreateDeviceParams{
		DriverID:    *driverID,
		TokenSha256: sum,
		TokenPrefix: prefix,
		Label:       label,
	})
	if err != nil {
		return Device{}, fmt.Errorf("db: cannot store the device: %w", err)
	}
	if err := q.AttachPairingDevice(ctx, gen.AttachPairingDeviceParams{ID: pairingID, DeviceID: &row.ID}); err != nil {
		return Device{}, fmt.Errorf("db: cannot record the device against the pairing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Device{}, fmt.Errorf("db: cannot finish the pairing: %w", err)
	}
	return Device{
		ID:          row.ID,
		DriverID:    row.DriverID,
		TokenPrefix: row.TokenPrefix,
		Label:       row.Label,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
}

// DeleteFinishedPairings removes grants whose codes ran out before the given
// instant, whatever they were decided. It is housekeeping: expiry is enforced
// by the lookup, so nothing depends on this having run.
func (s *Store) DeleteFinishedPairings(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.q.DeleteFinishedPairings(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("db: cannot clear out the finished pairings: %w", err)
	}
	return n, nil
}
