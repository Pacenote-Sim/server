package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// IdempotencyHeader is the header every authenticated write carries. It is what
// makes the client's offline queue safe: because a retry is free, a lap upload
// that failed for any reason can simply be sent again.
const IdempotencyHeader = "Idempotency-Key"

// IdempotencyKeyMax is the longest key this server stores. A UUID is 36
// characters and the contract does not say what a key looks like beyond being
// unique per request, so the cap is generous and exists only to keep a table
// row bounded.
const IdempotencyKeyMax = 200

// idempotencyKey reads and checks the header, and reports whether it answered
// the request itself.
func (a *API) idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get(IdempotencyHeader))
	switch {
	case key == "":
		a.failDetail(w, r, wire.CodeInvalid, msgKeyMissing,
			map[string]any{"header": IdempotencyHeader})
		return "", false
	case len(key) > IdempotencyKeyMax:
		a.failDetail(w, r, wire.CodeInvalid,
			"That idempotency key is longer than this server stores — use a UUID.",
			map[string]any{"header": IdempotencyHeader, "max_bytes": IdempotencyKeyMax})
		return "", false
	}
	return key, true
}

// idempotent runs one write exactly once and answers with what it produced, or
// replays what the same key produced before.
//
// fn returns a [db.Response] for anything the client should be told, refusals
// included, and an error only for a fault. A refusal is stored and replayed
// like any other answer, because a client that retries a request the server
// already refused must be refused the same way rather than have it succeed on
// the second attempt.
// idempotent runs one write under the caller's idempotency key and answers the
// request from it. It reports whether the write actually happened here — false
// for a failure and false for a replay of a key that was already answered — so
// that a caller can publish an event for a lap that has just been stored and
// stay quiet about one that was stored an hour ago.
func (a *API) idempotent(w http.ResponseWriter, r *http.Request, s session, key string, hash []byte,
	fn func(context.Context, *db.Tx) (db.Response, error),
) bool {
	res, replayed, err := a.deps.Store.Idempotent(r.Context(), db.Write{
		DeviceID: s.device.ID,
		Key:      key,
		Hash:     hash,
		Now:      a.deps.Now(),
	}, fn)
	switch {
	case errors.Is(err, db.ErrIdempotencyConflict):
		a.failDetail(w, r, wire.CodeConflict, msgKeyReused, map[string]any{"header": IdempotencyHeader})
		return false
	case errors.Is(err, db.ErrNotFound):
		// The request that held this key rolled back between the collision and
		// the read of its answer. Nothing was stored and nothing is owed: say
		// so and let the client send it again.
		a.fail(w, r, wire.CodeServerError, msgUnavailableAgain)
		return false
	case err != nil:
		a.failServer(w, r, "the write did not finish", err)
		return false
	}
	if replayed {
		a.deps.Log.LogAttrs(r.Context(), slog.LevelDebug, "idempotent write replayed",
			slog.String("path", r.URL.Path))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
	// A refusal — a conflict, a stint that is not there — is a write that did
	// not happen, however successfully it was answered.
	return !replayed && res.Status < http.StatusMultipleChoices
}

// answer marshals one success body into the form the idempotency table stores
// and the client receives. The bytes are stored, not the value, so a replay is
// byte-for-byte the first answer.
func answer(status int, body any) (db.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return db.Response{}, fmt.Errorf("api: cannot encode the response: %w", err)
	}
	return db.Response{Status: status, Body: b}, nil
}

// refusal marshals one error envelope the same way. A refusal produced inside a
// transaction is an answer and is stored beside the successes.
func refusal(code wire.Code, message string, detail map[string]any) (db.Response, error) {
	env := wire.NewError(code, message)
	env.Error.Detail = detail
	b, err := json.Marshal(env)
	if err != nil {
		return db.Response{}, fmt.Errorf("api: cannot encode the refusal: %w", err)
	}
	return db.Response{Status: statusOf(code), Body: b}, nil
}

// invalid turns a body that would not decode into the contract's 422, with a
// sentence the driver can read and the decoder's own words in the detail, where
// they are logged rather than shown.
func (a *API) invalid(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, httpx.ErrTooLarge):
		a.failDetail(w, r, wire.CodeInvalid,
			"That upload is larger than this server accepts — send fewer laps in one batch.",
			map[string]any{"reason": err.Error()})
	default:
		a.failDetail(w, r, wire.CodeInvalid,
			"That request could not be read — it is not the shape this server expects.",
			map[string]any{"reason": err.Error()})
	}
}
