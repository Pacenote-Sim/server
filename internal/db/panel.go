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

// PageSize is how many rows the admin panel's lists ask for at a time. It is
// one constant because every list on those pages is read the same way and an
// operator scrolling one should not find another paged differently.
const PageSize = 25

// PruneBatch is how many laps one step of a retention prune clears. It is small
// enough that each statement is short — a long UPDATE holds its locks and its
// snapshot for as long as it runs — and large enough that a hundred thousand
// laps is a few hundred statements rather than a few hundred thousand.
const PruneBatch = 500

// RosterOrder is how the drivers page is sorted. The three orderings are
// separate queries rather than one query with a sort parameter, because the
// keyset predicate has to agree with the ORDER BY about direction and about the
// tie-break, and a mismatch there skips rows instead of failing.
type RosterOrder string

// The orderings the roster offers.
const (
	// RosterByName is alphabetical, which is how an operator looks somebody up.
	RosterByName RosterOrder = "name"
	// RosterByLastSeen is the most recently connected machine first, which is
	// how an operator answers "is anybody's client broken".
	RosterByLastSeen RosterOrder = "seen"
	// RosterByLaps is the busiest driver first.
	RosterByLaps RosterOrder = "laps"
)

// Valid reports whether o is one of the three orderings.
func (o RosterOrder) Valid() bool {
	return o == RosterByName || o == RosterByLastSeen || o == RosterByLaps
}

// RosterCursor is where a page of the roster resumes: the sort key of the last
// row of the page before it, and that row's identifier to break a tie between
// two rows that share a key.
//
// Only the field belonging to the current ordering is read. The others are
// carried because the cursor travels through a URL, where the ordering is a
// separate parameter and either can be edited.
type RosterCursor struct {
	// ID is the identifier of the last row of the previous page.
	ID int64
	// Name, Seen and Laps are that row's key under each ordering.
	Name string
	Seen time.Time
	Laps int64
}

// RosterQuery asks for one page of the roster.
type RosterQuery struct {
	// Order is the sort. An invalid one is [RosterByName].
	Order RosterOrder
	// After is the cursor, or nil for the first page.
	After *RosterCursor
	// Limit is how many rows to read. Zero is [PageSize].
	Limit int
}

// RosterEntry is one driver as the roster page shows them: who they are, and
// every number the page prints beside them.
//
// The numbers come from one grouped query over each source table rather than a
// lookup per driver, so a page of twenty-five drivers costs the same whether
// each of them has ten laps or ten thousand.
type RosterEntry struct {
	// ID, Name, Slug and Class are the driver.
	ID    int64
	Name  string
	Slug  string
	Class string
	// CreatedAt is when the driver was first paired.
	CreatedAt time.Time
	// Laps and Stints are what they have recorded.
	Laps, Stints int64
	// TraceBytes is what their traces occupy, compressed.
	TraceBytes int64
	// Devices is how many of their machines are still paired, and
	// RevokedDevices how many have been signed out.
	Devices, RevokedDevices int64
	// LastUploadAt is when their newest lap arrived, or nil if they have never
	// uploaded one.
	LastUploadAt *time.Time
	// LastSeenAt is when any of their machines last made an authenticated
	// request, or nil if none ever has. It is the "is their client working"
	// answer, and it moves without a lap being uploaded.
	LastSeenAt *time.Time
}

// Cursor is where a page resumes after this row.
func (e RosterEntry) Cursor() RosterCursor {
	return RosterCursor{ID: e.ID, Name: e.Name, Seen: e.seenKey(), Laps: e.Laps}
}

// seenKey is [RosterEntry.LastSeenAt] with "never" spelled as a real instant,
// because a keyset cursor cannot compare against a null and the database orders
// by the same substitution.
func (e RosterEntry) seenKey() time.Time {
	if e.LastSeenAt == nil {
		return time.Unix(0, 0).UTC()
	}
	return *e.LastSeenAt
}

// Roster reads one page of the drivers page's list.
func (s *Store) Roster(ctx context.Context, q RosterQuery) ([]RosterEntry, error) {
	limit := int32(pageLimit(q.Limit)) //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	first := q.After == nil
	after := RosterCursor{}
	if !first {
		after = *q.After
	}

	var (
		rows []gen.DriverRoster
		err  error
	)
	switch q.Order {
	case RosterByLastSeen:
		rows, err = s.q.DriversByLastSeen(ctx, gen.DriversByLastSeenParams{
			First:     first,
			AfterSeen: pgtype.Timestamptz{Time: after.Seen, Valid: true},
			AfterID:   after.ID,
			Lim:       limit,
		})
	case RosterByLaps:
		rows, err = s.q.DriversByLaps(ctx, gen.DriversByLapsParams{
			First:     first,
			AfterLaps: after.Laps,
			AfterID:   after.ID,
			Lim:       limit,
		})
	case RosterByName:
		rows, err = s.q.DriversByName(ctx, gen.DriversByNameParams{
			First:     first,
			AfterName: after.Name,
			AfterID:   after.ID,
			Lim:       limit,
		})
	default:
		rows, err = s.q.DriversByName(ctx, gen.DriversByNameParams{
			First:     first,
			AfterName: after.Name,
			AfterID:   after.ID,
			Lim:       limit,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the roster: %w", err)
	}

	out := make([]RosterEntry, 0, len(rows))
	for i := range rows {
		out = append(out, rosterEntryOf(rows[i]))
	}
	return out, nil
}

// RosterEntryByID reads one driver with the same numbers the list shows, for
// the detail page.
func (s *Store) RosterEntryByID(ctx context.Context, id int64) (RosterEntry, error) {
	row, err := s.q.DriverRosterByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RosterEntry{}, ErrNotFound
		}
		return RosterEntry{}, fmt.Errorf("db: cannot read the driver: %w", err)
	}
	return rosterEntryOf(row), nil
}

func rosterEntryOf(r gen.DriverRoster) RosterEntry {
	return RosterEntry{
		ID:             r.ID,
		Name:           r.Name,
		Slug:           r.Slug,
		Class:          r.Class,
		CreatedAt:      r.CreatedAt.Time,
		Laps:           r.Laps,
		Stints:         r.Stints,
		TraceBytes:     r.TraceBytes,
		Devices:        r.Devices,
		RevokedDevices: r.RevokedDevices,
		LastUploadAt:   optionalTime(r.LastUploadAt),
		LastSeenAt:     optionalTime(r.LastSeenAt),
	}
}

// StintCursor is where a page of a driver's stints resumes.
type StintCursor struct {
	StartedAt time.Time
	ID        UUID
}

// StintQuery asks for one page of a driver's stints, newest first.
type StintQuery struct {
	// DriverID is whose stints to read.
	DriverID int64
	// After is the cursor, or nil for the first page.
	After *StintCursor
	// Limit is how many rows to read. Zero is [PageSize].
	Limit int
}

// StintRow is one stint as the driver's page lists it.
type StintRow struct {
	// ID is the identifier the client generated.
	ID UUID
	// Track, Car, CarClass and SessionType are what was being driven.
	Track, Car, CarClass, SessionType string
	// StartedAt is when it began, and FinishedAt when it was closed, or nil
	// for one that never was.
	StartedAt  time.Time
	FinishedAt *time.Time
	// Laps is how many laps it holds and BestLapMs the quickest clean one, or
	// zero when it holds no clean lap.
	Laps      int64
	BestLapMs int
	// TraceBytes is what its traces occupy, compressed.
	TraceBytes int64
}

// Cursor is where a page resumes after this row.
func (r StintRow) Cursor() StintCursor { return StintCursor{StartedAt: r.StartedAt, ID: r.ID} }

// StintsForDriver reads one page of a driver's stints, newest first.
func (s *Store) StintsForDriver(ctx context.Context, q StintQuery) ([]StintRow, error) {
	after := StintCursor{}
	if q.After != nil {
		after = *q.After
	}
	rows, err := s.q.StintsForDriver(ctx, gen.StintsForDriverParams{
		DriverID:     q.DriverID,
		First:        q.After == nil,
		AfterStarted: pgtype.Timestamptz{Time: after.StartedAt, Valid: true},
		AfterID:      after.ID.pg(),
		Lim:          int32(pageLimit(q.Limit)), //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	})
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the stints: %w", err)
	}
	out := make([]StintRow, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, StintRow{
			ID:          uuidFromPg(r.ID),
			Track:       r.Track,
			Car:         r.Car,
			CarClass:    r.CarClass,
			SessionType: r.SessionType,
			StartedAt:   r.StartedAt.Time,
			FinishedAt:  optionalTime(r.FinishedAt),
			Laps:        r.Laps,
			BestLapMs:   int(r.BestLapMs),
			TraceBytes:  r.TraceBytes,
		})
	}
	return out, nil
}

// PanelStint is one stint with the driver it belongs to, for the page that
// opens a stint and lists its laps.
type PanelStint struct {
	StintRow
	// DriverID and DriverName are who drove it.
	DriverID   int64
	DriverName string
	// Sim and TrackID are the identifiers the client reported.
	Sim, TrackID string
}

// PanelStintByID reads one stint for the panel. A stint that is not there is
// [ErrNotFound].
func (s *Store) PanelStintByID(ctx context.Context, id UUID) (PanelStint, error) {
	row, err := s.q.StintForPanel(ctx, id.pg())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PanelStint{}, ErrNotFound
		}
		return PanelStint{}, fmt.Errorf("db: cannot read the stint: %w", err)
	}
	return PanelStint{
		StintRow: StintRow{
			ID:          uuidFromPg(row.ID),
			Track:       row.Track,
			Car:         row.Car,
			CarClass:    row.CarClass,
			SessionType: row.SessionType,
			StartedAt:   row.StartedAt.Time,
			FinishedAt:  optionalTime(row.FinishedAt),
			Laps:        row.Laps,
			BestLapMs:   int(row.BestLapMs),
			TraceBytes:  row.TraceBytes,
		},
		DriverID:   row.DriverID,
		DriverName: row.DriverName,
		Sim:        row.Sim,
		TrackID:    row.TrackID,
	}, nil
}

// PanelLap is one lap as the stint page lists it. The trace itself is never
// read here: the page shows how big it is, and a page that fetched two hundred
// compressed blobs to print their sizes would be the one slow page in the
// panel.
type PanelLap struct {
	ID        int64
	Number    int
	LapMs     int
	Kind      string
	StartedAt time.Time
	CreatedAt time.Time
	// TraceBytes is the size of the stored trace, and zero means there is
	// none — either the lap arrived without one or retention has cleared it.
	TraceBytes int
}

// FirstLapCursor is the cursor that starts at a stint's first lap. Lap numbers
// begin at zero, so the value below every real one is -1.
const FirstLapCursor = -1

// LapsForStint reads one page of a stint's laps, in lap order, resuming after
// the lap number given. [FirstLapCursor] starts at the beginning.
func (s *Store) LapsForStint(ctx context.Context, stint UUID, afterNumber, limit int) ([]PanelLap, error) {
	rows, err := s.q.LapsForStint(ctx, gen.LapsForStintParams{
		StintID:     stint.pg(),
		AfterNumber: int32(afterNumber),      //nolint:gosec // G115: a lap number is bounded by validation on ingest.
		Lim:         int32(pageLimit(limit)), //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	})
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the laps: %w", err)
	}
	out := make([]PanelLap, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, PanelLap{
			ID:         r.ID,
			Number:     int(r.Number),
			LapMs:      int(r.LapMs),
			Kind:       r.Kind,
			StartedAt:  r.StartedAt.Time,
			CreatedAt:  r.CreatedAt.Time,
			TraceBytes: int(r.TraceBytes),
		})
	}
	return out, nil
}

// DeviceCursor is where a page of the devices list resumes.
type DeviceCursor struct {
	CreatedAt time.Time
	ID        int64
}

// DeviceQuery asks for one page of the devices list, newest first.
type DeviceQuery struct {
	// After is the cursor, or nil for the first page.
	After *DeviceCursor
	// Limit is how many rows to read. Zero is [PageSize].
	Limit int
}

// PanelDevice is one paired machine with the driver it belongs to.
//
// The token is not here and cannot be: only its digest was ever stored (D-2).
// TokenPrefix is the short clear-text head of it, which is enough for an
// operator to tell two of a driver's machines apart and useless to anyone else.
type PanelDevice struct {
	ID          int64
	DriverID    int64
	DriverName  string
	TokenPrefix string
	Label       string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

// Revoked reports whether this machine has been signed out.
func (d PanelDevice) Revoked() bool { return d.RevokedAt != nil }

// Cursor is where a page resumes after this row.
func (d PanelDevice) Cursor() DeviceCursor {
	return DeviceCursor{CreatedAt: d.CreatedAt, ID: d.ID}
}

// PanelDevices reads one page of every paired machine, newest first.
func (s *Store) PanelDevices(ctx context.Context, q DeviceQuery) ([]PanelDevice, error) {
	after := DeviceCursor{}
	if q.After != nil {
		after = *q.After
	}
	rows, err := s.q.DevicesPage(ctx, gen.DevicesPageParams{
		First:        q.After == nil,
		AfterCreated: pgtype.Timestamptz{Time: after.CreatedAt, Valid: true},
		AfterID:      after.ID,
		Lim:          int32(pageLimit(q.Limit)), //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	})
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the devices: %w", err)
	}
	out := make([]PanelDevice, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, PanelDevice{
			ID:          r.ID,
			DriverID:    r.DriverID,
			DriverName:  r.DriverName,
			TokenPrefix: r.TokenPrefix,
			Label:       r.Label,
			CreatedAt:   r.CreatedAt.Time,
			LastUsedAt:  optionalTime(r.LastUsedAt),
			RevokedAt:   optionalTime(r.RevokedAt),
		})
	}
	return out, nil
}

// PanelDevicesForDriver reads a driver's machines, newest first, for the
// driver's own page.
func (s *Store) PanelDevicesForDriver(ctx context.Context, driverID int64, limit int) ([]PanelDevice, error) {
	rows, err := s.q.DevicesForDriverPage(ctx, gen.DevicesForDriverPageParams{
		DriverID: driverID,
		Lim:      int32(pageLimit(limit)), //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	})
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the devices: %w", err)
	}
	out := make([]PanelDevice, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, PanelDevice{
			ID:          r.ID,
			DriverID:    r.DriverID,
			DriverName:  r.DriverName,
			TokenPrefix: r.TokenPrefix,
			Label:       r.Label,
			CreatedAt:   r.CreatedAt.Time,
			LastUsedAt:  optionalTime(r.LastUsedAt),
			RevokedAt:   optionalTime(r.RevokedAt),
		})
	}
	return out, nil
}

// PanelDeviceByID reads one machine, so that the page naming what a revoke will
// sign out is naming the machine it is about to sign out.
func (s *Store) PanelDeviceByID(ctx context.Context, id int64) (PanelDevice, error) {
	row, err := s.q.DeviceForPanel(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PanelDevice{}, ErrNotFound
		}
		return PanelDevice{}, fmt.Errorf("db: cannot read the device: %w", err)
	}
	return PanelDevice{
		ID:          row.ID,
		DriverID:    row.DriverID,
		DriverName:  row.DriverName,
		TokenPrefix: row.TokenPrefix,
		Label:       row.Label,
		CreatedAt:   row.CreatedAt.Time,
		LastUsedAt:  optionalTime(row.LastUsedAt),
		RevokedAt:   optionalTime(row.RevokedAt),
	}, nil
}

// RevokeDevicesForDriver signs out every one of one driver's machines and
// reports how many it signed out. No other driver is touched, which is what
// separates it from the danger zone's [Store.RevokeAllDevices].
func (s *Store) RevokeDevicesForDriver(ctx context.Context, driverID int64) (int64, error) {
	n, err := s.q.RevokeDevicesForDriver(ctx, driverID)
	if err != nil {
		return 0, fmt.Errorf("db: cannot revoke that driver's devices: %w", err)
	}
	return n, nil
}

// CountLiveDevicesForDriver counts a driver's machines that have not been
// revoked. The confirmation page names this number before anything happens.
func (s *Store) CountLiveDevicesForDriver(ctx context.Context, driverID int64) (int64, error) {
	n, err := s.q.CountLiveDevicesForDriver(ctx, driverID)
	if err != nil {
		return 0, fmt.Errorf("db: cannot count that driver's devices: %w", err)
	}
	return n, nil
}

// DeviceCounts reports how many machines have ever been paired and how many are
// still able to upload.
func (s *Store) DeviceCounts(ctx context.Context) (total, live int64, err error) {
	row, err := s.q.CountAllDevices(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("db: cannot count the devices: %w", err)
	}
	return row.Total, row.Live, nil
}

// DataStats is everything the data page says about what is stored. It comes
// back from one statement, because a page of eight numbers that asked eight
// questions would be eight round trips for one screen.
type DataStats struct {
	// DatabaseBytes is what PostgreSQL reports for the whole database,
	// indexes and its own overhead included.
	DatabaseBytes int64
	// TraceBytes is how much of that is compressed lap traces.
	TraceBytes int64
	// Drivers, Stints and Laps are row counts, and Traces is how many of
	// those laps still hold a trace — the two differ once retention has run.
	Drivers, Stints, Laps, Traces int64
	// OldestStintAt and OldestLapAt are the oldest records held, or the zero
	// time when there are none.
	OldestStintAt, OldestLapAt time.Time
}

// DataTotals reads the data page's figures.
func (s *Store) DataTotals(ctx context.Context) (DataStats, error) {
	row, err := s.q.DataTotals(ctx)
	if err != nil {
		return DataStats{}, fmt.Errorf("db: cannot read what is stored: %w", err)
	}
	return DataStats{
		DatabaseBytes: row.DatabaseBytes,
		TraceBytes:    row.TraceBytes,
		Drivers:       row.Drivers,
		Stints:        row.Stints,
		Laps:          row.Laps,
		Traces:        row.Traces,
		OldestStintAt: row.OldestStintAt.Time,
		OldestLapAt:   row.OldestLapAt.Time,
	}, nil
}

// DriverStorage is one line of the data page's "who is using the disk" list.
type DriverStorage struct {
	ID         int64
	Name       string
	Laps       int64
	TraceBytes int64
}

// StorageByDriver lists what each driver's traces occupy, largest first.
func (s *Store) StorageByDriver(ctx context.Context, limit int) ([]DriverStorage, error) {
	rows, err := s.q.StorageByDriver(ctx, int32(pageLimit(limit))) //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the storage per driver: %w", err)
	}
	out := make([]DriverStorage, 0, len(rows))
	for i := range rows {
		out = append(out, DriverStorage{
			ID:         rows[i].ID,
			Name:       rows[i].Name,
			Laps:       rows[i].Laps,
			TraceBytes: rows[i].TraceBytes,
		})
	}
	return out, nil
}

// TraceSet is a set of laps that still hold a trace, as the retention preview
// describes it before anything is deleted.
type TraceSet struct {
	// Laps is how many laps would lose their trace, and TraceBytes how much
	// would be freed.
	Laps, TraceBytes int64
	// OldestAt and NewestAt bound them, or the zero time when the set is
	// empty.
	OldestAt, NewestAt time.Time
}

// Empty reports whether there is nothing in the set.
func (t TraceSet) Empty() bool { return t.Laps == 0 }

// TracesOlderThan describes the traces a prune with this cutoff would clear. It
// reads exactly the set [Store.PruneTracesOlderThan] writes, so the number the
// page promises is the number the job does.
func (s *Store) TracesOlderThan(ctx context.Context, before time.Time) (TraceSet, error) {
	row, err := s.q.TracesOlderThan(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		return TraceSet{}, fmt.Errorf("db: cannot read what retention would delete: %w", err)
	}
	return TraceSet{
		Laps:       row.Laps,
		TraceBytes: row.TraceBytes,
		OldestAt:   row.OldestAt.Time,
		NewestAt:   row.NewestAt.Time,
	}, nil
}

// PruneTracesOlderThan clears one batch of traces older than the cutoff and
// reports how many laps it cleared. Zero means there is nothing left to do.
//
// The lap row and its time survive: only the blob is emptied, so a driver's
// record of having driven that lap in that time is untouched and only the shape
// of the lap is gone. It is one batch rather than the whole set on purpose —
// the caller loops, so a prune of a hundred thousand laps is a few hundred short
// statements rather than one that holds its locks for minutes.
func (s *Store) PruneTracesOlderThan(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch <= 0 || batch > 10_000 {
		batch = PruneBatch
	}
	n, err := s.q.PruneTracesOlderThan(ctx, gen.PruneTracesOlderThanParams{
		Before: pgtype.Timestamptz{Time: before, Valid: true},
		Lim:    int32(batch),
	})
	if err != nil {
		return 0, fmt.Errorf("db: cannot clear the traces: %w", err)
	}
	return n, nil
}

// pageLimit bounds what a caller may ask for in one page. A page is rendered
// into HTML a person reads, so there is no reason to allow a request for a
// million rows and a good reason not to.
func pageLimit(n int) int {
	switch {
	case n <= 0:
		return PageSize
	case n > 1000:
		return 1000
	default:
		return n
	}
}
