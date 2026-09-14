package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
)

// A handler that cannot encode its own answer is a bug in this server, not a
// bad request. It must still be a well-formed error rather than a half-written
// body with a 200 already on it.
func TestJSONAnswersWhenTheBodyCannotBeEncoded(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	w := httptest.NewRecorder()
	// A channel cannot be marshalled, which is the shape of a handler that was
	// handed something it should not have been.
	JSON(w, http.StatusOK, map[string]any{"bad": make(chan int)})

	r.Equal(http.StatusInternalServerError, w.Code)
	r.Equal("application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var body wire.ErrorEnvelope
	r.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	r.Equal(wire.CodeServerError, body.Error.Code)
	r.NotEmpty(body.Error.Message)
}

// A code with no status of its own is a server error rather than a zero status,
// which net/http would turn into 200.
func TestAPIErrorNeverWritesAZeroStatus(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	w := httptest.NewRecorder()
	APIError(w, wire.Code("nothing_like_this"), "Something went wrong.")
	r.Equal(http.StatusInternalServerError, w.Code)
}

// Every status the server actually answers with maps to a code, and anything
// else falls back rather than inventing one.
func TestCodeForEveryStatusWeAnswerWith(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	for _, c := range wire.Codes() {
		if s := c.HTTPStatus(); s != 0 {
			r.NotEmpty(codeFor(s), "no code for status %d", s)
		}
	}
	r.Equal(wire.CodeServerError, codeFor(418))
}

// Retry is a sentence in an error message, not a promise, but it must never be
// zero or negative — a client told to wait no time at all is a client that
// hammers.
func TestRetryIsAlwaysAWaitWorthPrinting(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal(time.Minute, (&Limiter{}).Retry(), "a limiter with no rate still has to name a wait")
	r.Equal(time.Second, (&Limiter{rate: 1}).Retry())
	r.Equal(10*time.Second, (&Limiter{rate: 0.1}).Retry())
	// A rate faster than one a second still rounds up to a second: there is no
	// useful way to tell somebody to wait for part of one.
	r.Equal(time.Second, (&Limiter{rate: 5}).Retry())
}

// The sweep is what stops a flood of one-off addresses growing the map without
// bound. It runs at most once a minute, and it keeps buckets that are still in
// use.
func TestSweepDropsOnlyBucketsThatHaveRefilled(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	start := time.Now()
	l := &Limiter{
		rate:    1,
		burst:   5,
		buckets: map[string]*bucket{},
		last:    start,
	}
	// A bucket is dropped once it has been idle long enough to have refilled,
	// which at one a second with a burst of five is five seconds. Anything
	// longer than that is indistinguishable from a bucket that was never there.
	const sweepAt = 10 * time.Minute
	l.buckets["idle"] = &bucket{tokens: 0, seen: start}
	l.buckets["busy"] = &bucket{tokens: 0, seen: start.Add(sweepAt - time.Second)}

	// Too soon: the sweep is rate limited itself, or it would walk the whole map
	// on every request.
	l.sweep(start.Add(30 * time.Second))
	r.Len(l.buckets, 2)

	l.sweep(start.Add(sweepAt))
	r.Len(l.buckets, 1)
	r.Contains(l.buckets, "busy")

	// A limiter with no rate never expires anything, because "refilled" has no
	// meaning without one.
	off := &Limiter{buckets: map[string]*bucket{"x": {seen: start}}, last: start}
	off.sweep(start.Add(time.Hour))
	r.Len(off.buckets, 1)
}

// statusRecorder has to stay unwrappable or a handler that flushes — the live
// endpoint, anything streaming — silently stops being able to.
func TestStatusRecorderStaysUnwrappable(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w}
	r.Same(http.ResponseWriter(w), rec.Unwrap())

	// Which is the property http.ResponseController depends on.
	rec.WriteHeader(http.StatusAccepted)
	_, err := rec.Write([]byte("x"))
	r.NoError(err)
	r.NoError(http.NewResponseController(rec).Flush())
	r.Equal(http.StatusAccepted, rec.status)
	r.EqualValues(1, rec.written)

	// A second WriteHeader is ignored: the first status is the one that went out
	// on the wire, and a handler that writes two has already sent the first.
	rec.WriteHeader(http.StatusTeapot)
	r.Equal(http.StatusAccepted, rec.status)
}
