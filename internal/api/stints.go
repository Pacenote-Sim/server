package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// stintBodyLimit caps PUT /stints/{id}. A stint is a dozen short strings and a
// handful of numbers, so the published body limit — which exists for laps and
// their traces — would be three orders of magnitude too generous here. D-3 puts
// the cap on the endpoint, and this is that endpoint's.
const stintBodyLimit int64 = 16 << 10

// putStint creates a stint or updates its mutable fields.
//
// The identifier comes from the client, so the call is naturally idempotent and
// a client can create a stint offline and upload it hours later. The
// idempotency key is still required: a retry must replay rather than run the
// upsert again, because the stored response is what the client is owed.
func (a *API) putStint(w http.ResponseWriter, r *http.Request, s session) {
	id, ok := a.stintID(w, r)
	if !ok {
		return
	}
	key, ok := a.idempotencyKey(w, r)
	if !ok {
		return
	}

	var body wire.Stint
	hash, err := httpx.DecodeJSONFingerprint(w, r, &body, stintBodyLimit)
	if err != nil {
		a.invalid(w, r, err)
		return
	}
	if f := validateStint(body); f != nil {
		a.failDetail(w, r, wire.CodeInvalid, f.Message, f.Detail)
		return
	}
	setup, err := encodeSetup(body.Setup)
	if err != nil {
		a.failServer(w, r, "the car setup could not be encoded", err)
		return
	}

	a.idempotent(w, r, s, key, hash, func(ctx context.Context, tx *db.Tx) (db.Response, error) {
		err := tx.UpsertStint(ctx, db.StintWrite{
			ID:          id,
			DriverID:    s.driver.ID,
			Sim:         body.Sim,
			Track:       body.Track,
			TrackID:     body.TrackID,
			Car:         body.Car,
			CarClass:    body.CarClass,
			SessionType: string(body.SessionType),
			Sectors:     body.Sectors,
			StartedAt:   body.StartedAt,
			Setup:       setup,
		})
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return refusal(wire.CodeNotFound, msgNoStint, map[string]any{"stint_id": id.String()})
			}
			return db.Response{}, err
		}
		a.deps.Log.LogAttrs(ctx, slog.LevelInfo, "stint stored",
			slog.String("stint_id", id.String()),
			slog.String("driver", s.driver.Slug),
			slog.String("track_id", body.TrackID))

		// SessionID is the server's own competition session. Competition is an
		// enterprise feature, so a community server never attaches one and the
		// field is explicitly null rather than absent.
		return answer(http.StatusOK, wire.StintResult{StintID: id.String(), SessionID: nil})
	})
}

// encodeSetup renders the car setup as the jsonb column holds it, and nil when
// the client sent none.
//
// Nil and not an empty document: the column is nullable precisely so that "the
// simulator published no setup" and "the setup is empty" stay different
// answers, and a plugin reading null knows it was not told.
func encodeSetup(setup *wire.CarSetup) ([]byte, error) {
	if setup == nil {
		return nil, nil
	}
	out, err := json.Marshal(setup)
	if err != nil {
		return nil, fmt.Errorf("api: cannot encode the car setup: %w", err)
	}
	return out, nil
}

// stintID reads the identifier out of the path and reports whether it answered
// the request itself. The contract asks clients to generate a UUIDv7, which
// sorts by creation time; any UUID is accepted, because refusing a v4 would
// break a client for a property only this server's indexes benefit from.
func (a *API) stintID(w http.ResponseWriter, r *http.Request) (db.UUID, bool) {
	id, err := db.ParseUUID(r.PathValue("id"))
	if err != nil {
		a.failDetail(w, r, wire.CodeInvalid,
			"That stint identifier is not a UUID — the upload was dropped, and this is a bug in the client.",
			map[string]any{"field": "stint_id"})
		return db.UUID{}, false
	}
	return id, true
}
