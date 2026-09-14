//go:build postgres

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

// TestRosterPagesEveryOrdering walks each of the roster's three orderings from
// the first row to the last, a page at a time, and checks that the walk visits
// every driver exactly once.
//
// That is the property a keyset cursor exists for, and it is the one a wrong
// tie-break silently breaks: a page boundary that falls between two rows with
// the same sort key either repeats one of them or steps over it, and a test
// that only asserted "the first page looks right" would never see it.
func TestRosterPagesEveryOrdering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		order db.RosterOrder
	}{
		{"by name", db.RosterByName},
		{"by last seen", db.RosterByLastSeen},
		{"by laps", db.RosterByLaps},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			ctx := context.Background()
			store, _ := migrated(t)

			// Every driver here has the same "last seen" and the same lap
			// count: nobody has a device and nobody has driven. That is the
			// hard case for the two orderings whose key is an aggregate,
			// because every row ties and the tie-break is doing all the work.
			const total = 23
			for i := range total {
				_, err := store.EnsureDriver(ctx, fmt.Sprintf("Driver %02d", i), "")
				r.NoError(err)
			}

			const page = 5
			seen := map[int64]int{}
			var cursor *db.RosterCursor
			for step := 0; ; step++ {
				r.Less(step, total, "paging should end rather than go round for ever")

				rows, err := store.Roster(ctx, db.RosterQuery{
					Order: tc.order, After: cursor, Limit: page,
				})
				r.NoError(err)
				if len(rows) == 0 {
					break
				}
				for _, row := range rows {
					seen[row.ID]++
				}
				last := rows[len(rows)-1].Cursor()
				cursor = &last
				if len(rows) < page {
					break
				}
			}

			r.Len(seen, total, "every driver is visited")
			for id, times := range seen {
				r.Equal(1, times, "driver %d was on more than one page", id)
			}
		})
	}
}

// TestRosterCountsWhatADriverHas checks the numbers the roster view reports,
// because every one of them is a page's column.
func TestRosterCountsWhatADriverHas(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	store, _ := migrated(t)

	driver, err := store.EnsureDriver(ctx, "Marta Ferrer", "GT3")
	r.NoError(err)

	entry, err := store.RosterEntryByID(ctx, driver.ID)
	r.NoError(err)
	r.Equal("Marta Ferrer", entry.Name)
	r.Equal("GT3", entry.Class)
	r.Zero(entry.Laps)
	r.Zero(entry.Stints)
	r.Zero(entry.TraceBytes)
	r.Zero(entry.Devices)
	r.Nil(entry.LastSeenAt, "a driver who has never connected has no last-seen moment")
	r.Nil(entry.LastUploadAt)

	device, err := store.CreateDevice(ctx, driver.ID, []byte("digest-one"), "prefix01", "laptop")
	r.NoError(err)
	r.NoError(store.TouchDevice(ctx, device.ID))

	entry, err = store.RosterEntryByID(ctx, driver.ID)
	r.NoError(err)
	r.EqualValues(1, entry.Devices)
	r.Zero(entry.RevokedDevices)
	r.NotNil(entry.LastSeenAt, "a machine that has spoken gives the driver a last-seen moment")

	revoked, err := store.RevokeDevice(ctx, device.ID)
	r.NoError(err)
	r.True(revoked)

	entry, err = store.RosterEntryByID(ctx, driver.ID)
	r.NoError(err)
	r.Zero(entry.Devices, "a revoked machine is not a paired one")
	r.EqualValues(1, entry.RevokedDevices)

	_, err = store.RosterEntryByID(ctx, 99999)
	r.ErrorIs(err, db.ErrNotFound)
}

// TestPruneKeepsTheLapAndItsTime is the promise the data page makes, checked at
// the level that keeps it.
func TestPruneKeepsTheLapAndItsTime(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	store, _ := migrated(t)

	driver, err := store.EnsureDriver(ctx, "Marta Ferrer", "")
	r.NoError(err)
	device, err := store.CreateDevice(ctx, driver.ID, []byte("digest-two"), "prefix02", "laptop")
	r.NoError(err)

	old := time.Now().AddDate(0, -8, 0)
	recent := time.Now().Add(-time.Hour)
	stint := storeStint(t, store, driver.ID, device.ID, old, []lapSeed{
		{Number: 1, LapMs: 95_000, At: old, Trace: bytesOf(1024)},
		{Number: 2, LapMs: 94_000, At: old.Add(time.Minute), Trace: bytesOf(1024)},
		{Number: 3, LapMs: 93_000, At: recent, Trace: bytesOf(2048)},
	})

	cutoff := time.Now().AddDate(0, -1, 0)
	preview, err := store.TracesOlderThan(ctx, cutoff)
	r.NoError(err)
	r.EqualValues(2, preview.Laps)
	r.EqualValues(2048, preview.TraceBytes)
	r.False(preview.Empty())

	cleared, err := store.PruneTracesOlderThan(ctx, cutoff, db.PruneBatch)
	r.NoError(err)
	r.EqualValues(2, cleared, "the prune clears exactly what the preview counted")

	again, err := store.PruneTracesOlderThan(ctx, cutoff, db.PruneBatch)
	r.NoError(err)
	r.Zero(again, "a second pass has nothing left, so the loop ends")

	laps, err := store.LapsForStint(ctx, stint, db.FirstLapCursor, 100)
	r.NoError(err)
	r.Len(laps, 3, "no lap was deleted")
	byNumber := map[int]db.PanelLap{}
	for _, l := range laps {
		byNumber[l.Number] = l
	}
	r.Equal(95_000, byNumber[1].LapMs, "the driver's record of the lap survives its trace")
	r.Equal(94_000, byNumber[2].LapMs)
	r.Zero(byNumber[1].TraceBytes)
	r.Zero(byNumber[2].TraceBytes)
	r.Equal(2048, byNumber[3].TraceBytes, "a lap inside the line keeps its trace")

	after, err := store.TracesOlderThan(ctx, cutoff)
	r.NoError(err)
	r.True(after.Empty())

	totals, err := store.DataTotals(ctx)
	r.NoError(err)
	r.EqualValues(3, totals.Laps)
	r.EqualValues(1, totals.Traces, "two of the three laps now hold no trace")
	r.EqualValues(2048, totals.TraceBytes)
	r.False(totals.OldestLapAt.IsZero())
}

// lapSeed is one lap for the helper below.
type lapSeed struct {
	Number int
	LapMs  int
	At     time.Time
	Trace  []byte
}

// storeStint writes a stint and its laps through the same transaction the API
// uses, so that the reference rows exist and a prune has to deal with them.
func storeStint(tb testing.TB, store *db.Store, driverID, deviceID int64, startedAt time.Time, laps []lapSeed) db.UUID {
	tb.Helper()
	r := require.New(tb)
	ctx := context.Background()

	id, err := db.ParseUUID("01a099cb-0a7c-7000-a846-2001c572d8fc")
	r.NoError(err)

	rows := make([]db.LapRow, 0, len(laps))
	for _, l := range laps {
		rows = append(rows, db.LapRow{
			Number:     l.Number,
			LapMs:      l.LapMs,
			Kind:       "clean",
			StartedAt:  l.At,
			ContentSum: bytesOf(32),
			TraceCodec: 1,
			Trace:      l.Trace,
		})
	}
	// Every lap needs its own fingerprint, or the batch looks like one lap
	// repeated.
	for i := range rows {
		rows[i].ContentSum[0] = byte(rows[i].Number)
	}

	_, _, err = store.Idempotent(ctx,
		db.Write{DeviceID: deviceID, Key: "seed", Hash: []byte("seed"), Now: time.Now()},
		func(ctx context.Context, tx *db.Tx) (db.Response, error) {
			if bad := tx.UpsertStint(ctx, db.StintWrite{
				ID: id, DriverID: driverID, Sim: "iracing",
				Track: "Jerez", TrackID: "jerez", Car: "Cup", CarClass: "",
				SessionType: "practice", StartedAt: startedAt,
			}); bad != nil {
				return db.Response{}, bad
			}
			_, bad := tx.AppendLaps(ctx, db.LapWrite{
				StintID: id, DriverID: driverID, Sim: "iracing",
				TrackID: "jerez", Car: "Cup", CarClass: "",
				Laps: rows,
			})
			return db.Response{Status: 200, Body: []byte("{}")}, bad
		})
	r.NoError(err)
	return id
}

func bytesOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// The operator's code-signing certificate, stored and read back.
//
// The sealed fields go in and come out untouched — this package never opens
// them and has no key to open them with — and everything a page reads is stored
// beside them in the clear, so the page still renders on a server whose data
// key is gone.
func TestTheSigningCertificateRoundTrips(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	store, _ := migrated(t)

	_, err := store.SigningCertificate(ctx)
	r.ErrorIs(err, db.ErrNotFound, "a fresh server came with a certificate")

	expires := time.Now().Add(200 * 24 * time.Hour).Truncate(time.Microsecond)
	want := db.NewSigningCertificate{
		PfxSealed:      []byte{0x01, 0x02, 0x03},
		PasswordSealed: []byte{0x04, 0x05},
		Subject:        "Spain GT League",
		Issuer:         "Spain GT League Authority",
		NotBefore:      expires.Add(-365 * 24 * time.Hour),
		NotAfter:       expires,
		Thumbprint:     "cafebabe",
		Algorithm:      "RSA 3072",
		SelfSigned:     false,
		CodeSigning:    true,
		UploadedBy:     "ana@example.com",
	}
	r.NoError(store.SaveSigningCertificate(ctx, want))

	got, err := store.SigningCertificate(ctx)
	r.NoError(err)
	r.Equal(want.PfxSealed, got.PfxSealed)
	r.Equal(want.PasswordSealed, got.PasswordSealed)
	r.Equal("Spain GT League", got.Subject)
	r.Equal("Spain GT League Authority", got.Issuer)
	r.WithinDuration(expires, got.NotAfter, time.Millisecond)
	r.Equal("cafebabe", got.Thumbprint)
	r.Equal("RSA 3072", got.Algorithm)
	r.False(got.SelfSigned)
	r.True(got.CodeSigning)
	r.Equal("ana@example.com", got.UploadedBy)
	r.WithinDuration(time.Now(), got.UploadedAt, time.Minute)

	// Saving again replaces rather than adding a second, which is what makes
	// "the certificate this server signs with" a question with one answer.
	next := want
	next.Subject = "Spain GT League 2027"
	next.PfxSealed = []byte{0x09}
	next.UploadedBy = "marc@example.com"
	r.NoError(store.SaveSigningCertificate(ctx, next))

	got, err = store.SigningCertificate(ctx)
	r.NoError(err)
	r.Equal("Spain GT League 2027", got.Subject)
	r.Equal([]byte{0x09}, got.PfxSealed)
	r.Equal("marc@example.com", got.UploadedBy)

	r.NoError(store.DeleteSigningCertificate(ctx))
	_, err = store.SigningCertificate(ctx)
	r.ErrorIs(err, db.ErrNotFound)

	// Removing one that is not there is the state that was asked for.
	r.NoError(store.DeleteSigningCertificate(ctx))
}
