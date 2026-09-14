package api

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// LiveTTL is how long a live sample is worth anything. Past it the entry is
// swept: a driver whose client stopped sending is not on track, and showing
// their last known position for the rest of the afternoon would be a lie.
const LiveTTL = 30 * time.Second

// LiveMaxStints caps how many stints are held at once, so a flood of
// identifiers cannot grow the map without bound. It is far above any team's
// concurrent drivers; the oldest entry is dropped when it is reached.
const LiveMaxStints = 1024

// postLive takes one live sample and holds it in memory.
//
// Nothing here is written to the database: a sample
// two seconds old is worthless, and persisting it would cost a write per second
// per driver for data nobody reads twice. It follows that live state is
// per-instance, which is correct for the one instance every deployment of this
// edition is.
//
// The answer is 204 and there is no body. The call is fire and forget: a client
// never retries it and never queues it, so it carries no idempotency key.
func (a *API) postLive(w http.ResponseWriter, r *http.Request, s session) {
	if !s.has(wire.FeatureLive) {
		a.fail(w, r, wire.CodeForbidden, msgLiveUnavailable)
		return
	}

	var body wire.LiveSample
	if _, err := httpx.DecodeJSONFingerprint(w, r, &body, a.bodyLimit(s.settings)); err != nil {
		a.invalid(w, r, err)
		return
	}
	id, err := db.ParseUUID(body.StintID)
	if err != nil {
		a.failDetail(w, r, wire.CodeInvalid,
			"That live sample names a stint identifier that is not a UUID — it was dropped.",
			map[string]any{"field": "stint_id"})
		return
	}
	if len(body.CurLap) > s.settings.Limits.TracePoints {
		a.failDetail(w, r, wire.CodeInvalid,
			"That live sample carries more trace points than this server takes — it was dropped.",
			map[string]any{"field": "cur_lap", "points": len(body.CurLap), "max": s.settings.Limits.TracePoints})
		return
	}

	a.live.put(id, s.driver.ID, body)
	w.WriteHeader(http.StatusNoContent)
}

// LiveSample is the latest sample held for one stint, with the driver it
// belongs to and when it arrived. It is what a live view reads.
type LiveSample struct {
	DriverID int64
	At       time.Time
	Sample   wire.LiveSample
}

// ErrNoLiveSample reports that nothing recent is held for a stint.
var ErrNoLiveSample = errors.New("api: no live sample for that stint")

// Live returns the latest sample for a stint, or [ErrNoLiveSample].
func (a *API) Live(id db.UUID) (LiveSample, error) { return a.live.get(id) }

// liveState is the map D-5 describes: the latest sample only, behind an
// RWMutex, with a time to live and a cap.
//
// Entries are pointers and a write swaps one, so a reader holding the read lock
// never waits on a writer for longer than that swap — the sample itself is
// never mutated in place.
type liveState struct {
	mu      sync.RWMutex
	entries map[db.UUID]*LiveSample
	now     func() time.Time
	sweptAt time.Time
}

func newLiveState(now func() time.Time) *liveState {
	return &liveState{entries: make(map[db.UUID]*LiveSample), now: now}
}

func (l *liveState) put(id db.UUID, driverID int64, sample wire.LiveSample) {
	entry := &LiveSample{DriverID: driverID, At: l.now(), Sample: sample}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(entry.At)
	if _, held := l.entries[id]; !held && len(l.entries) >= LiveMaxStints {
		l.dropOldest()
	}
	l.entries[id] = entry
}

func (l *liveState) get(id db.UUID) (LiveSample, error) {
	l.mu.RLock()
	entry, ok := l.entries[id]
	l.mu.RUnlock()
	if !ok || l.now().Sub(entry.At) > LiveTTL {
		return LiveSample{}, ErrNoLiveSample
	}
	return *entry, nil
}

// sweep drops entries that have gone stale. It runs at most once a second, on
// the write path, so there is no goroutine to leak and no timer to stop.
func (l *liveState) sweep(now time.Time) {
	if now.Sub(l.sweptAt) < time.Second {
		return
	}
	l.sweptAt = now
	for id, entry := range l.entries {
		if now.Sub(entry.At) > LiveTTL {
			delete(l.entries, id)
		}
	}
}

// dropOldest makes room when the cap is reached. It is a scan of the map, which
// happens only at the cap and only on the write that reaches it.
func (l *liveState) dropOldest() {
	var oldestID db.UUID
	var oldest time.Time
	first := true
	for id, entry := range l.entries {
		if first || entry.At.Before(oldest) {
			oldestID, oldest, first = id, entry.At, false
		}
	}
	if !first {
		delete(l.entries, oldestID)
	}
}
