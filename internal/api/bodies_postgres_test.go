//go:build postgres

package api_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/config"
)

// Every route that takes a body, given one it cannot read.
//
// The answer has to be the error envelope rather than whatever the JSON decoder
// would have said, because the thing on the other end is a client that decides
// what to do from the code in it. A route that let a decode failure out as
// plain text would be a route a client cannot tell apart from a proxy having a
// bad day.
func TestEveryRouteRefusesABodyItCannotRead(t *testing.T) {
	t.Parallel()
	// Every optional feature on, so that each route answers about the body it
	// was given rather than about not being installed.
	h := newHarness(t, harnessOptions{features: withFeatures(
		wire.FeatureTelemetry, wire.FeatureReference, wire.FeatureLive,
		wire.FeatureField, wire.FeatureTTS,
	)})
	stint := h.stint(t)

	routes := []struct {
		name   string
		method string
		path   string
		key    string
	}{
		{name: "pair poll", method: http.MethodPost, path: api.Prefix + "/pair/poll"},
		{name: "a stint", method: http.MethodPut, path: api.Prefix + "/stints/" + stint, key: "k-stint"},
		{name: "laps", method: http.MethodPost, path: api.Prefix + "/stints/" + stint + "/laps", key: "k-laps"},
		{name: "a summary", method: http.MethodPut, path: api.Prefix + "/stints/" + stint + "/summary", key: "k-sum"},
		{name: "live", method: http.MethodPost, path: api.Prefix + "/live"},
		{name: "the field", method: http.MethodPost, path: api.Prefix + "/field", key: "k-field"},
		{name: "a spoken cue", method: http.MethodPost, path: api.Prefix + "/tts", key: "k-tts"},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			res := h.do(request{method: tc.method, path: tc.path, raw: "{ this is not JSON", key: tc.key})
			r.GreaterOrEqual(res.status, 400, "a body that is not JSON was accepted")
			r.Less(res.status, 500, "a client's mistake was answered as this server's")

			env := res.envelope(t)
			r.NotEmpty(env.Code, "the answer carries no code for a client to act on")
			r.NotEmpty(env.Message)
		})
	}
}

// The values inside a body that is readable and wrong. Each one is a client
// that has misunderstood the contract rather than a driver who did anything,
// so each says which field and stops there.
func TestTheValuesARouteRefuses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{features: withFeatures(
		wire.FeatureTelemetry, wire.FeatureLive, wire.FeatureField,
	)})

	t.Run("a field report about a stint that is not a stint", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		res := h.do(request{
			method: http.MethodPost, path: api.Prefix + "/field", key: "k-field-uuid",
			body: wire.FieldReport{StintID: "not-a-uuid", SessionType: "race"},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status)
		r.Contains(res.envelope(t).Message, "not a UUID")
	})

	t.Run("a field report carrying more cars than a grid holds", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		cars := make([]wire.FieldCar, api.FieldMaxCars+1)
		for i := range cars {
			cars[i] = wire.FieldCar{Num: strconv.Itoa(i), Lap: 1}
		}
		res := h.do(request{
			method: http.MethodPost, path: api.Prefix + "/field", key: "k-field-cars",
			body: wire.FieldReport{StintID: h.stint(t), SessionType: "race", Cars: cars},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status)
		r.Contains(res.envelope(t).Message, "more cars than any session holds")
	})

	t.Run("a live sample with more trace points than are published", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		points := make([]wire.TracePoint, config.DefaultLimits().TracePoints+1)
		res := h.do(request{
			method: http.MethodPost, path: api.Prefix + "/live",
			body: wire.LiveSample{StintID: h.stint(t), At: time.Now(), CurLap: points},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status)
		r.Contains(res.envelope(t).Message, "trace")
	})
}

// A machine whose token was revoked while its client was running. The next
// request it makes is refused, and refused with the code that tells the client
// to pair again rather than to retry.
func TestARevokedMachineIsRefusedOnItsNextRequest(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{})

	token, _, device := h.pair("Ana Ruiz")
	res := h.do(request{method: http.MethodGet, path: api.Prefix + "/me", token: token})
	r.Equal(http.StatusOK, res.status)

	_, err := h.store.RevokeDevice(context.Background(), device.ID)
	r.NoError(err)

	res = h.do(request{method: http.MethodGet, path: api.Prefix + "/me", token: token})
	r.Equal(http.StatusUnauthorized, res.status)
	r.Equal(wire.CodeUnauthorized, res.envelope(t).Code)
}

// Pairing is rate-limited per address, because starting one is the only thing
// on this server that a stranger can do.
func TestStartingAPairingIsRateLimited(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{})

	var limited bool
	for range 200 {
		res := h.do(request{method: http.MethodPost, path: api.Prefix + "/pair/start", noAuth: true})
		if res.status == http.StatusTooManyRequests {
			limited = true
			r.NotEmpty(res.headers.Get("Retry-After"), "a rate-limited client was not told when to come back")
			r.Equal(wire.CodeRateLimited, res.envelope(t).Code)
			break
		}
		r.Equal(http.StatusOK, res.status)
	}
	r.True(limited, "two hundred pairings from one address were all opened")
}

// The discovery document a client reads before it does anything else.
func TestTheDiscoveryDocumentIsBuiltFromTheSettings(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	settings := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy)
	doc := api.Discovery(settings, []wire.Feature{wire.FeatureTTS})

	r.Equal("Iberian GT Championship", doc.Name)
	r.Contains(doc.Features, wire.FeatureTTS)
	r.NotEmpty(doc.PairURI, "a client is printed this address and a driver types it")
	r.True(strings.HasPrefix(doc.PairURI, "https://pacenote.example.com"), doc.PairURI)
}

// A database that stops while clients are talking to it.
//
// Every route answers with the envelope and a server-error code, because that
// is what tells a client to hold its uploads and try later. A panic would take
// the process with it; a bare 500 would have the client decide it had sent
// something wrong and throw the lap away.
func TestEveryRouteSurvivesADatabaseThatStopped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{features: withFeatures(
		wire.FeatureTelemetry, wire.FeatureReference, wire.FeatureLive, wire.FeatureField,
	)})
	stint := h.stint(t)

	// Everything the routes need is in hand before the database goes away.
	h.store.Close()

	calls := []struct {
		name   string
		method string
		path   string
		body   any
		key    string
		noAuth bool
	}{
		{name: "who am I", method: http.MethodGet, path: api.Prefix + "/me"},
		{name: "a reference lap", method: http.MethodGet, path: api.Prefix + "/reference?track_id=spa&car_id=gt3"},
		{
			name: "opening a pairing", method: http.MethodPost,
			path: api.Prefix + "/pair/start", noAuth: true,
		},
		{
			name: "polling a pairing", method: http.MethodPost, path: api.Prefix + "/pair/poll",
			noAuth: true, body: wire.PairPollRequest{DeviceCode: strings.Repeat("a", 43)},
		},
		{
			name: "a field report", method: http.MethodPost, path: api.Prefix + "/field",
			key: "down-field", body: wire.FieldReport{StintID: stint, SessionType: "race"},
		},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(request{
				method: tc.method, path: tc.path, body: tc.body, key: tc.key, noAuth: tc.noAuth,
			})
			require.Equal(t, http.StatusInternalServerError, res.status, res.body)
			require.Equal(t, wire.CodeServerError, res.envelope(t).Code)
		})
	}
}

// An idempotency key longer than the contract allows. It is the client's
// mistake and it is named, because a key that was silently truncated would make
// two different writes look like one retry of the same.
func TestAnIdempotencyKeyThatIsTooLong(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{})

	res := h.do(request{
		method: http.MethodPut, path: api.Prefix + "/stints/" + h.stint(t),
		key: strings.Repeat("k", api.IdempotencyKeyMax+1), body: sampleStint(),
	})
	r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
	r.Equal(wire.CodeInvalid, res.envelope(t).Code)
}
