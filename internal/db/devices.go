package db

import (
	"context"
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// Device is one paired machine belonging to a driver. The token it was issued
// is not here, and cannot be: only its digest was ever stored (D-2).
type Device struct {
	ID          int64
	DriverID    int64
	TokenPrefix string
	Label       string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

// Revoked reports whether this device has been revoked, which a driver sees as
// removing a machine from their list.
func (d Device) Revoked() bool { return d.RevokedAt != nil }

// CreateDevice stores a paired machine. sum and prefix come from
// auth.SplitDeviceToken; the token itself is returned to the client once by the
// pairing endpoint and never stored.
func (s *Store) CreateDevice(ctx context.Context, driverID int64, sum []byte, prefix, label string) (Device, error) {
	row, err := s.q.CreateDevice(ctx, gen.CreateDeviceParams{
		DriverID:    driverID,
		TokenSha256: sum,
		TokenPrefix: prefix,
		Label:       label,
	})
	if err != nil {
		return Device{}, fmt.Errorf("db: cannot store the device: %w", err)
	}
	return Device{
		ID:          row.ID,
		DriverID:    row.DriverID,
		TokenPrefix: row.TokenPrefix,
		Label:       row.Label,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
}

// DeviceByToken finds the live device a presented token belongs to.
//
// The lookup is by the clear-text prefix, which is indexed, and the decision is
// made by comparing digests in constant time. That is the whole reason the
// prefix column exists: without it the query would be a scan, and with the
// prefix as the decision it would be a 48-bit credential.
func (s *Store) DeviceByToken(ctx context.Context, prefix string, sum []byte) (Device, error) {
	rows, err := s.q.DevicesByTokenPrefix(ctx, prefix)
	if err != nil {
		return Device{}, fmt.Errorf("db: cannot read the device: %w", err)
	}
	found := -1
	for i := range rows {
		if subtle.ConstantTimeCompare(rows[i].TokenSha256, sum) == 1 {
			found = i
		}
	}
	if found < 0 {
		return Device{}, ErrNotFound
	}
	r := rows[found]
	return Device{
		ID:          r.ID,
		DriverID:    r.DriverID,
		TokenPrefix: r.TokenPrefix,
		Label:       r.Label,
		CreatedAt:   r.CreatedAt.Time,
		LastUsedAt:  optionalTime(r.LastUsedAt),
		RevokedAt:   optionalTime(r.RevokedAt),
	}, nil
}

// TouchDevice records that a device was used, which the driver sees as "last
// seen" against the machine in their list.
func (s *Store) TouchDevice(ctx context.Context, id int64) error {
	if err := s.q.TouchDevice(ctx, id); err != nil {
		return fmt.Errorf("db: cannot record the device as used: %w", err)
	}
	return nil
}

// RevokeDevice stops a device from uploading. It reports whether a live device
// was revoked, so revoking one twice is not an error.
func (s *Store) RevokeDevice(ctx context.Context, id int64) (bool, error) {
	n, err := s.q.RevokeDevice(ctx, id)
	if err != nil {
		return false, fmt.Errorf("db: cannot revoke the device: %w", err)
	}
	return n > 0, nil
}

// ListDevicesForDriver lists a driver's machines, newest first.
func (s *Store) ListDevicesForDriver(ctx context.Context, driverID int64) ([]Device, error) {
	rows, err := s.q.ListDevicesForDriver(ctx, driverID)
	if err != nil {
		return nil, fmt.Errorf("db: cannot list the devices: %w", err)
	}
	out := make([]Device, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, Device{
			ID:          r.ID,
			DriverID:    r.DriverID,
			TokenPrefix: r.TokenPrefix,
			Label:       r.Label,
			CreatedAt:   r.CreatedAt.Time,
			LastUsedAt:  optionalTime(r.LastUsedAt),
			RevokedAt:   optionalTime(r.RevokedAt),
		})
	}
	return out, nil
}

// CreateDriver adds a driver. The roster page and the pairing flow both need
// it; neither is built yet, and the devices above are useless without it.
func (s *Store) CreateDriver(ctx context.Context, externalID, name, slug, class, avatarURL string) (int64, error) {
	row, err := s.q.CreateDriver(ctx, gen.CreateDriverParams{
		ExternalID: externalID,
		Name:       name,
		Slug:       slug,
		Class:      class,
		AvatarUrl:  avatarURL,
	})
	if err != nil {
		return 0, fmt.Errorf("db: cannot create the driver: %w", err)
	}
	return row.ID, nil
}

// RevokeAllDevices revokes every live device token and reports how many it
// revoked. It is the danger-zone action an operator reaches for when a token
// has leaked: every paired machine stops being able to upload, and every
// driver pairs again.
//
// Nothing is deleted. The devices stay, with the moment they were revoked on
// them, because "when did we cut everyone off" is a question somebody asks
// afterwards.
func (s *Store) RevokeAllDevices(ctx context.Context) (int64, error) {
	n, err := s.q.RevokeAllDevices(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: cannot revoke the device tokens: %w", err)
	}
	return n, nil
}
