package api

import (
	"context"
	"log/slog"
	"time"
)

// SweepInterval is how often the two tables that hold short-lived rows are
// cleared out. Neither expiry is enforced by the sweep — the lookups check the
// clock — so this is housekeeping, and an hour is often enough that a week away
// does not leave a table full of last week.
const SweepInterval = time.Hour

// Sweep clears out expired idempotency keys and finished pairings until ctx is
// cancelled. It is a goroutine the caller owns, and it returns when the context
// does.
//
// Nothing depends on it having run. The idempotency lookup and the pairing poll
// both check expiry themselves, so a server whose sweep never runs is correct
// and merely larger than it needs to be.
func (a *API) Sweep(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = SweepInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweepOnce(ctx)
		}
	}
}

func (a *API) sweepOnce(ctx context.Context) {
	now := a.deps.Now()
	keys, err := a.deps.Store.DeleteExpiredIdempotencyKeys(ctx, now)
	if err != nil {
		a.deps.Log.LogAttrs(ctx, slog.LevelWarn, "the expired idempotency keys could not be cleared out",
			slog.Any("error", err))
		return
	}
	pairings, err := a.deps.Store.DeleteFinishedPairings(ctx, now.Add(-db24h))
	if err != nil {
		a.deps.Log.LogAttrs(ctx, slog.LevelWarn, "the finished pairings could not be cleared out",
			slog.Any("error", err))
		return
	}
	if keys == 0 && pairings == 0 {
		return
	}
	a.deps.Log.LogAttrs(ctx, slog.LevelInfo, "swept",
		slog.Int64("idempotency_keys", keys),
		slog.Int64("pairings", pairings))
}

// db24h is how long a finished pairing is kept after its code ran out. The row
// is useless by then, but an operator looking at the admin panel an hour later
// should still see what they approved.
const db24h = 24 * time.Hour
