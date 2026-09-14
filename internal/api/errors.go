package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/httpx"
)

// The messages this server sends a driver. They are gathered here rather than
// written at each call site because they are user interface: the client shows
// them verbatim, so they have to read like one voice and be changed like one
// thing.
//
// Each is a complete sentence, in the second person, with the qualifier after
// an em dash and the caveat stated inline. None names an identifier, a table or
// a status code: the machine-readable half of the answer is the code beside it.
const (
	msgNoToken          = "This request carried no token — pair this machine with the server and try again."
	msgBadToken         = "That token is not valid on this server — pair this machine again to get a new one."
	msgRevokedToken     = "That machine has been removed from your account — pair it again to carry on uploading."
	msgNoStint          = "There is no stint with that identifier — upload the stint before its laps."
	msgNoReference      = "There is no reference lap for that track and car yet — drive a clean lap and it becomes your own."
	msgNoRoute          = "There is nothing at that address on this server."
	msgKeyReused        = "That idempotency key has already been used for a different request — nothing was stored, and this is a bug in the client rather than something to retry."
	msgKeyMissing       = "This request carried no Idempotency-Key header — every write needs one so that a retry is safe."
	msgLapConflict      = "Some of those laps are already stored with different content — nothing in the batch was stored, and the lap numbers are in the detail."
	msgServerError      = "Something went wrong on the server. Try again."
	msgUnavailableAgain = "That write was interrupted and nothing was stored — send it again."
	msgTTSUnavailable   = "The voice coach is not available on this server — the operator has not installed a plugin that speaks."
	msgTTSUnconfigured  = "The voice plugin on this server has not been configured yet — the cue was written but not spoken."
	msgTTSFailed        = "Nothing spoke that cue — it was written, and driving carries on without it."
	msgFieldUnavailable = "The field relay is not available on this server — this is the community edition, which records one driver rather than a whole session."
	msgLiveUnavailable  = "Live telemetry is not available on this server — the operator has turned it off."
	msgCoachUnavailable = "The coach is not available on this server — the operator has not installed a plugin that does the coaching."
)

// fail writes the v1 error envelope and logs the fact at debug, which is where
// a refused request belongs: it is the client's business, not an operator's.
func (a *API) fail(w http.ResponseWriter, r *http.Request, code wire.Code, message string) {
	a.failDetail(w, r, code, message, nil)
}

// failDetail is [API.fail] with the free-form context a client logs. Detail is
// never shown to the driver, so it is the place for a lap number, a field name
// or anything else that would be jargon in a sentence.
func (a *API) failDetail(w http.ResponseWriter, r *http.Request, code wire.Code, message string, detail map[string]any) {
	env := wire.NewError(code, message)
	env.Error.Detail = detail
	a.deps.Log.LogAttrs(r.Context(), slog.LevelDebug, "request refused",
		slog.String("path", r.URL.Path), slog.String("code", string(code)))
	httpx.JSON(w, statusOf(code), env)
}

// failRateLimited is the one refusal that carries advice: how long to wait. The
// Retry-After header is set alongside retry_after_s, because a proxy in front
// of this server understands the header and not the body.
func (a *API) failRateLimited(w http.ResponseWriter, r *http.Request, retryAfterS int) {
	if retryAfterS < 1 {
		retryAfterS = 1
	}
	env := wire.NewError(wire.CodeRateLimited,
		"You are sending faster than this server accepts — wait "+plural(retryAfterS, "second")+" and carry on.")
	env.Error.RetryAfterS = retryAfterS
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterS))
	a.deps.Log.LogAttrs(r.Context(), slog.LevelDebug, "request rate limited",
		slog.String("path", r.URL.Path), slog.Int("retry_after_s", retryAfterS))
	httpx.JSON(w, http.StatusTooManyRequests, env)
}

// failServer logs the fault and tells the driver nothing about it. A stack, a
// table name or a driver error in a message the client shows verbatim would be
// jargon at best and a disclosure at worst.
func (a *API) failServer(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.deps.Log.LogAttrs(r.Context(), slog.LevelError, what,
		slog.String("path", r.URL.Path), slog.Any("error", err))
	httpx.JSON(w, http.StatusInternalServerError, wire.NewError(wire.CodeServerError, msgServerError))
}

// statusOf is the status the contract pairs with a code. An unknown code cannot
// occur — every constant in wire.Codes is handled — but a zero status would
// become a 200 with an error body, which is the one answer a client cannot make
// sense of.
func statusOf(code wire.Code) int {
	if s := code.HTTPStatus(); s != 0 {
		return s
	}
	return http.StatusInternalServerError
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
