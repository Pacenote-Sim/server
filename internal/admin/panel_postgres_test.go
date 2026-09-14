//go:build postgres

package admin_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
)

// seedUUID mints an identifier shaped like the one a client generates: version
// 7, which is what API v1 asks for and what the stints table is keyed by.
func seedUUID(tb testing.TB) db.UUID {
	tb.Helper()
	var b [16]byte
	_, err := rand.Read(b[:])
	require.NoError(tb, err)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	id, err := db.ParseUUID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32])
	require.NoError(tb, err)
	return id
}

// This file is what the drivers, devices and data tests build their world out
// of: real rows, written through the same package the server writes them
// through, so that a page assertion is about the page and not about a fixture
// that happens to agree with it.

// seedLap is one lap to store. Trace is arbitrary bytes: nothing in the panel
// decodes a trace, it only ever reports how many bytes one is, so a real
// compressed blob would make these tests slower and prove nothing extra.
type seedLap struct {
	Number    int
	LapMs     int
	Kind      string
	StartedAt time.Time
	Trace     []byte
}

// seedDriver adds a driver.
func (p *panel) seedDriver(name, class string) db.Driver {
	p.t.Helper()
	r := require.New(p.t)
	driver, err := p.store.EnsureDriver(context.Background(), name, class)
	r.NoError(err)
	return driver
}

// seedDevice pairs a machine to a driver and returns the token the client would
// hold along with the row. The token is returned because a revocation test has
// to present it afterwards and be refused.
func (p *panel) seedDevice(driverID int64, label string) (string, db.Device) {
	p.t.Helper()
	r := require.New(p.t)
	token, err := auth.NewDeviceToken()
	r.NoError(err)
	device, err := p.store.CreateDevice(context.Background(), driverID, token.Sum, token.Prefix, label)
	r.NoError(err)
	return token.Plain, device
}

// seedStint stores a stint and its laps the way the API does: one transaction
// through the idempotency table, so the reference rows are maintained too and a
// prune has something real to delete.
// seedSim is the simulator every seeded stint and every lap under it comes
// from. It is named rather than left empty because a lap is only ever compared
// against a lap from the same simulator, and a fixture driving that comparison
// with an empty one would be asserting against a bucket no real upload lands in.
const seedSim = "iracing"

func (p *panel) seedStint(driverID, deviceID int64, track, car string, startedAt time.Time, laps []seedLap) db.UUID {
	p.t.Helper()
	r := require.New(p.t)
	ctx := context.Background()

	id := seedUUID(p.t)
	rows := make([]db.LapRow, 0, len(laps))
	for _, l := range laps {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", id.String(), l.Number)))
		kind := l.Kind
		if kind == "" {
			kind = "clean"
		}
		at := l.StartedAt
		if at.IsZero() {
			at = startedAt.Add(time.Duration(l.Number) * time.Minute)
		}
		rows = append(rows, db.LapRow{
			Number:     l.Number,
			LapMs:      l.LapMs,
			Kind:       kind,
			StartedAt:  at,
			ContentSum: sum[:],
			TraceCodec: 1,
			Trace:      l.Trace,
		})
	}

	finished := startedAt.Add(time.Duration(len(laps)+1) * time.Minute)
	_, _, err := p.store.Idempotent(ctx,
		db.Write{DeviceID: deviceID, Key: id.String(), Hash: []byte(id.String()), Now: time.Now()},
		func(ctx context.Context, tx *db.Tx) (db.Response, error) {
			if err := tx.UpsertStint(ctx, db.StintWrite{
				ID: id, DriverID: driverID, Sim: seedSim,
				Track: track, TrackID: track, Car: car, CarClass: "",
				SessionType: "practice", StartedAt: startedAt,
			}); err != nil {
				return db.Response{}, err
			}
			if len(rows) > 0 {
				if _, err := tx.AppendLaps(ctx, db.LapWrite{
					StintID: id, DriverID: driverID, Sim: seedSim,
					TrackID: track, Car: car, CarClass: "",
					Laps: rows,
				}); err != nil {
					return db.Response{}, err
				}
			}
			if err := tx.ReplaceSummary(ctx, db.SummaryWrite{
				StintID:    id,
				Laps:       len(rows),
				Conditions: []byte("{}"),
				CarState:   []byte("{}"),
				BestTrace:  []byte{},
				FinishedAt: &finished,
			}); err != nil {
				return db.Response{}, err
			}
			return db.Response{Status: http.StatusOK, Body: []byte("{}")}, nil
		})
	r.NoError(err)
	return id
}

// traceOf is a trace of a given size, so that a test can assert on bytes freed
// without caring what is in them.
func traceOf(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// getStatus fetches a page and returns its status alongside its body, for the
// paths where the status is the assertion.
func (p *panel) getStatus(path string) (int, string) {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.server.URL+path, http.NoBody)
	r.NoError(err)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	r.NoError(err)
	requireWholePage(p.t, string(body))
	return res.StatusCode, string(body)
}

// requireWholePage fails if a body stops before the end of the document.
//
// html/template abandons a page part-way through when a helper is handed the
// wrong type — an int where the byte formatter wants an int64, say — and what
// reaches the browser is a page that simply stops mid-row. Nothing else in a
// test would notice, so every fetch checks for the closing tag.
func requireWholePage(tb testing.TB, body string) {
	tb.Helper()
	if strings.Contains(body, "<!doctype html>") {
		require.Contains(tb, body, "</html>", "the page stopped rendering part-way through")
	}
}

// setRetention stores a retention setting the way the page's own form does, for
// the tests that are about what happens next rather than about the form.
func (p *panel) setRetention(months int) {
	p.t.Helper()
	r := require.New(p.t)
	ctx := context.Background()
	settings, err := p.store.Settings(ctx)
	r.NoError(err)
	settings.Retention = config.Retention{TraceMonths: months}
	r.NoError(p.store.SaveSettings(ctx, settings))
}

// awaitPrune waits for the background prune to finish and returns what it did.
//
// It also keeps the test honest about lifetimes: the store is closed when the
// test ends, and a job still running then would be writing to a closed pool.
func (p *panel) awaitPrune() admin.PruneProgress {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		state := p.object.PruneProgress()
		if state.Started() && !state.Running {
			return state
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("the prune did not finish within 30 seconds: %+v", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// signOut throws this browser's cookies away, so the next request arrives as an
// anonymous visitor and the panel has to turn it away.
func (p *panel) signOut() {
	p.t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(p.t, err)
	p.client.Jar = jar
}

// auditCount counts the audit rows written for one action, which is how every
// mutating test here checks that the change was recorded.
func (p *panel) auditCount(action string) int {
	p.t.Helper()
	r := require.New(p.t)
	rows, err := p.store.AuditFor(context.Background(), action, 50)
	r.NoError(err)
	return len(rows)
}

// lapTimesOf reads the lap times a stint holds, so that a prune can be shown to
// have left them alone.
func (p *panel) lapTimesOf(stint db.UUID) map[int]int {
	p.t.Helper()
	r := require.New(p.t)
	laps, err := p.store.LapsForStint(context.Background(), stint, db.FirstLapCursor, 1000)
	r.NoError(err)
	out := make(map[int]int, len(laps))
	for _, l := range laps {
		out[l.Number] = l.LapMs
	}
	return out
}

// traceBytesOf reads how many bytes of trace a stint still holds.
func (p *panel) traceBytesOf(stint db.UUID) int {
	p.t.Helper()
	r := require.New(p.t)
	laps, err := p.store.LapsForStint(context.Background(), stint, db.FirstLapCursor, 1000)
	r.NoError(err)
	total := 0
	for _, l := range laps {
		total += l.TraceBytes
	}
	return total
}
