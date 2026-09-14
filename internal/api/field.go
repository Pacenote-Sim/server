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

// FieldQuietPeriod is how long an armed relay may go silent before another
// client may take over. Three times the published interval: long enough that a
// dropped packet does not change who is authoritative, short enough that a
// driver who quits mid-session does not hold the relay for the rest of it.
const FieldQuietPeriod = 3

// FieldMaxCars caps one report. A full grid is forty-odd cars; this is well
// above any of them and is here so that a body inside the size limit cannot
// still be a hundred thousand entries.
const FieldMaxCars = 200

// FieldMaxSessions caps how many sessions are relayed at once, for the same
// reason [LiveMaxStints] does.
const FieldMaxSessions = 256

// postField takes one whole-field report and says whether this client is the
// one relaying it.
//
// Only one client per session needs to send this, and the server decides which:
// the first to report for a session is armed, and the rest are told armed false
// and stop until their next stint. That is not an error and the driver is never
// told. Like live samples, nothing here is persisted (D-5).
func (a *API) postField(w http.ResponseWriter, r *http.Request, s session) {
	if !s.has(wire.FeatureField) {
		a.fail(w, r, wire.CodeForbidden, msgFieldUnavailable)
		return
	}
	if _, ok := a.idempotencyKey(w, r); !ok {
		return
	}

	var body wire.FieldReport
	if _, err := httpx.DecodeJSONFingerprint(w, r, &body, a.bodyLimit(s.settings)); err != nil {
		a.invalid(w, r, err)
		return
	}
	id, err := db.ParseUUID(body.StintID)
	if err != nil {
		a.failDetail(w, r, wire.CodeInvalid,
			"That field report names a stint identifier that is not a UUID — it was dropped.",
			map[string]any{"field": "stint_id"})
		return
	}
	if len(body.Cars) > FieldMaxCars {
		a.failDetail(w, r, wire.CodeInvalid,
			"That field report carries more cars than any session holds — it was dropped.",
			map[string]any{"field": "cars", "count": len(body.Cars), "max": FieldMaxCars})
		return
	}

	stint, err := a.deps.Store.Stint(r.Context(), id, s.driver.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		a.failDetail(w, r, wire.CodeNotFound, msgNoStint, map[string]any{"stint_id": id.String()})
		return
	case err != nil:
		a.failServer(w, r, "the stint could not be read", err)
		return
	}

	quiet := time.Duration(FieldQuietPeriod*s.settings.Limits.FieldIntervalMs) * time.Millisecond
	armed := a.field.report(fieldBucket(stint.TrackID, string(body.SessionType)), id, body, quiet)
	a.ok(w, wire.FieldResult{OK: true, Armed: armed})
}

// fieldBucket names one session. The community edition has no competition
// sessions, so a session is what the reports themselves agree on: a track and a
// session type, which is what every car on the same server at the same moment
// shares.
func fieldBucket(trackID, sessionType string) string { return trackID + "\x00" + sessionType }

// fieldState is the armed relay per session, held in memory and never
// persisted.
type fieldState struct {
	mu      sync.Mutex
	entries map[string]*fieldEntry
	now     func() time.Time
}

// fieldEntry is who is relaying one session and what they last said.
type fieldEntry struct {
	stint  db.UUID
	at     time.Time
	report wire.FieldReport
}

func newFieldState(now func() time.Time) *fieldState {
	return &fieldState{entries: make(map[string]*fieldEntry), now: now}
}

// report records one relay and reports whether this stint is the armed one.
//
// The rule is first come, first armed, until the armed client goes quiet for
// longer than quiet. A client that is not armed is still answered 200: it is
// not wrong, it is simply not the one, and the contract has it stop sending
// until its next stint.
func (f *fieldState) report(bucket string, stint db.UUID, rep wire.FieldReport, quiet time.Duration) bool {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()

	held, ok := f.entries[bucket]
	switch {
	case ok && held.stint == stint:
	case ok && now.Sub(held.at) <= quiet:
		return false
	case !ok && len(f.entries) >= FieldMaxSessions:
		f.dropQuietest(now)
	}
	f.entries[bucket] = &fieldEntry{stint: stint, at: now, report: rep}
	return true
}

// Field returns the latest whole-field report for one session, and when it
// arrived, for a timing view to read. Like the live samples it is held in
// memory only, so it is this instance's answer and nothing else's.
func (a *API) Field(trackID, sessionType string) (wire.FieldReport, time.Time, bool) {
	return a.field.latest(fieldBucket(trackID, sessionType))
}

func (f *fieldState) latest(bucket string) (wire.FieldReport, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.entries[bucket]
	if !ok {
		return wire.FieldReport{}, time.Time{}, false
	}
	return held.report, held.at, true
}

func (f *fieldState) dropQuietest(now time.Time) {
	var quietest string
	oldest := now
	found := false
	for bucket, entry := range f.entries {
		if !found || entry.at.Before(oldest) {
			quietest, oldest, found = bucket, entry.at, true
		}
	}
	if found {
		delete(f.entries, quietest)
	}
}
