//go:build postgres

package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

func TestSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("it stops when its context does", func(t *testing.T) {
		t.Parallel()

		// The assertion goleak in TestMain cannot make on its own: a phase that
		// ends must take the housekeeping goroutine with it.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.api.Sweep(ctx, time.Millisecond)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the sweeper did not stop when its context was cancelled")
		}
	})

	t.Run("expired replay entries are cleared out", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		id := randomUUIDv7(t, startedAt)
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id,
			key: "sweepable-" + id, body: sampleStint(),
		}).status)

		// Nothing has expired yet, so nothing goes.
		gone, err := h.store.DeleteExpiredIdempotencyKeys(ctx, time.Now())
		r.NoError(err)
		r.Zero(gone)

		// A day and a bit later, it has.
		gone, err = h.store.DeleteExpiredIdempotencyKeys(ctx, time.Now().Add(api.SweepInterval).Add(25*time.Hour))
		r.NoError(err)
		r.Positive(gone, "a replay entry is kept for a day and then let go")
	})

	t.Run("finished pairings are cleared out", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		h.startPairing()
		gone, err := h.store.DeleteFinishedPairings(ctx, time.Now())
		r.NoError(err)
		r.Zero(gone, "a grant that has not run out yet stays")

		gone, err = h.store.DeleteFinishedPairings(ctx, time.Now().Add(24*time.Hour))
		r.NoError(err)
		r.Positive(gone)
	})
}

// The sweep on a server whose database has gone away.
//
// Both clear-outs are best-effort by design: the idempotency lookup and the
// pairing poll check expiry themselves, so a sweep that cannot run leaves a
// correct server that is merely larger than it needs to be. What it must not do
// is stop the goroutine, because then it would never run again either.
func TestTheSweepSurvivesADatabaseThatStopped(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	logs := &logSink{}
	store, err := db.Open(ctx, dbtest.URL(t), logging.New(logging.Options{
		Level: slog.LevelDebug, Output: logs,
	}))
	r.NoError(err)
	r.NoError(store.Migrate(ctx))

	a, err := api.New(ctx, api.Deps{
		Log:     logging.New(logging.Options{Level: slog.LevelDebug, Output: logs}),
		Store:   store,
		Keyring: auth.NewKeyring(nil),
	})
	r.NoError(err)

	// One sweep against a live database, so the quiet path is the one that
	// runs when there is nothing to clear.
	sweepCtx, cancel := context.WithCancel(ctx)
	go a.Sweep(sweepCtx, time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	store.Close()
	time.Sleep(50 * time.Millisecond)
	cancel()

	r.Contains(logs.String(), "could not be cleared out",
		"a sweep against a database that had gone away said nothing")
}

// A sweep that actually clears something out, and says so.
//
// The quiet path — nothing to clear — is the common one and is silent on
// purpose: an hourly line saying nothing happened is a line nobody reads. The
// one that did something is worth a line, and this is the test that it gets one.
func TestASweepThatClearsSomethingOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	logs := &logSink{}
	store, err := db.Open(ctx, dbtest.URL(t), logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	// A pairing nobody finished, old enough to be cleared.
	codes, err := auth.NewPairingCodes()
	r.NoError(err)
	pairing, err := store.CreatePairing(ctx, codes.DeviceCodeSum, codes.UserCode,
		time.Now().Add(-48*time.Hour))
	r.NoError(err)
	r.NoError(store.ExpirePairing(ctx, pairing.ID))

	a, err := api.New(ctx, api.Deps{
		Log:     logging.New(logging.Options{Level: slog.LevelDebug, Output: logs}),
		Store:   store,
		Keyring: auth.NewKeyring(nil),
		Now:     func() time.Time { return time.Now().Add(72 * time.Hour) },
	})
	r.NoError(err)

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Sweep(sweepCtx, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "swept")
	}, 10*time.Second, 20*time.Millisecond, "a sweep that cleared something out said nothing")
	r.Contains(logs.String(), `"pairings":1`)
}

// logSink is a log destination a test can read while something is still
// writing to it.
//
// bytes.Buffer is not that: the sweep runs in its own goroutine, and polling
// the buffer for what it wrote is a read racing a write that the detector
// finds every time.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
