package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/trace"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// putSummary replaces a stint's summary document.
//
// It is a whole-document replace on purpose: the client sends one periodically
// during a stint and once more at the end, and a merge could combine two halves
// of two different states. A present finished_at means this one is final and
// also closes the stint.
func (a *API) putSummary(w http.ResponseWriter, r *http.Request, s session) {
	id, ok := a.stintID(w, r)
	if !ok {
		return
	}
	key, ok := a.idempotencyKey(w, r)
	if !ok {
		return
	}

	var body wire.Summary
	hash, err := httpx.DecodeJSONFingerprint(w, r, &body, a.bodyLimit(s.settings))
	if err != nil {
		a.invalid(w, r, err)
		return
	}
	if f := validateSummary(body, s.settings.Limits); f != nil {
		a.failDetail(w, r, wire.CodeInvalid, f.Message, f.Detail)
		return
	}

	bestTrace, err := trace.Encode(body.BestTrace)
	if err != nil {
		a.failDetail(w, r, wire.CodeInvalid,
			"That summary's best lap carries a trace this server cannot store — the summary was dropped.",
			map[string]any{"field": "best_trace", "reason": err.Error()})
		return
	}
	conditions, carState, err := summaryDocuments(body)
	if err != nil {
		a.failServer(w, r, "the summary could not be encoded", err)
		return
	}

	// The event a finished stint produces, filled in by the write and
	// dispatched after it has committed. A summary sent mid-stint produces
	// nothing: a debrief is written about a session that is over.
	var events []plugin.Event

	stored := a.idempotent(w, r, s, key, hash, func(ctx context.Context, tx *db.Tx) (db.Response, error) {
		stint, err := tx.Stint(ctx, id, s.driver.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return refusal(wire.CodeNotFound, msgNoStint, map[string]any{"stint_id": id.String()})
			}
			return db.Response{}, err
		}
		err = tx.ReplaceSummary(ctx, db.SummaryWrite{
			StintID:        id,
			Laps:           body.Laps,
			Incidents:      body.Incidents,
			BestLapMs:      body.BestLapMs,
			AvgLapMs:       body.AvgLapMs,
			ConsistencyPct: body.ConsistencyPct,
			TopSpeedKmh:    body.TopSpeedKmh,
			Conditions:     conditions,
			CarState:       carState,
			BestTrace:      bestTrace,
			BestTraceCodec: trace.CodecVersion,
			FinishedAt:     body.FinishedAt,
		})
		if err != nil {
			return db.Response{}, err
		}
		a.deps.Log.LogAttrs(ctx, slog.LevelInfo, "summary stored",
			slog.String("stint_id", id.String()),
			slog.String("driver", s.driver.Slug),
			slog.Bool("final", body.FinishedAt != nil))

		if a.deps.Plugins != nil && body.FinishedAt != nil {
			events = []plugin.Event{stintEvent(a.deps.Log, s, stint, body)}
		}

		return answer(http.StatusOK, wire.OK{OK: true})
	})
	if stored {
		a.publish(r.Context(), events)
	}
}

// summaryDocuments renders the two nested objects as the jsonb columns hold
// them. They are stored whole rather than flattened into columns because
// nothing queries inside them: they are read back to be shown, and a simulator
// that starts reporting a new channel should not need a migration.
func summaryDocuments(s wire.Summary) (conditions, carState []byte, err error) {
	conditions, err = json.Marshal(s.Conditions)
	if err != nil {
		return nil, nil, fmt.Errorf("api: cannot encode the conditions: %w", err)
	}
	carState, err = json.Marshal(s.CarState)
	if err != nil {
		return nil, nil, fmt.Errorf("api: cannot encode the car state: %w", err)
	}
	return conditions, carState, nil
}
