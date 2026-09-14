//go:build postgres

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
)

// startedAt is the instant every fixture in this package is stamped with, so a
// committed response is the same bytes on every run.
var startedAt = time.Date(2026, 9, 12, 14, 3, 11, 0, time.FixedZone("CEST", 2*60*60))

// testSim is the simulator every fixture in this package comes from. A lap is
// only ever compared against another lap from the same one, so a test that
// means to cross that line names both simulators itself.
const testSim = "iracing"

func sampleStint() wire.Stint {
	return wire.Stint{
		Sim:         testSim,
		Track:       "Circuit de Barcelona-Catalunya",
		TrackID:     "barcelona gp",
		Car:         "Ferrari 296 GT3",
		CarClass:    "gt3",
		SessionType: wire.SessionPractice,
		StartedAt:   startedAt,
		Sectors:     []float64{0.0, 0.31, 0.68},
	}
}

func TestPairing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{skipPairing: true})
	ctx := context.Background()

	t.Run("a code that nobody opened is expired rather than missing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
			body: wire.PairPollRequest{DeviceCode: "not-a-code-anyone-issued"},
		})
		r.Equal(http.StatusOK, res.status)
		var poll wire.PairPoll
		r.NoError(json.Unmarshal([]byte(res.body), &poll))
		r.Equal(wire.StatusExpired, poll.Status, "a client is told to start again, not that the route is missing")
		r.Empty(poll.Token)
	})

	t.Run("a poll with no device code is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
			body: wire.PairPollRequest{},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status)
		r.Equal(wire.CodeInvalid, res.envelope(t).Code)
	})

	t.Run("pending until an operator decides", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		start := h.startPairing()
		poll := h.poll(start.DeviceCode)
		r.Equal(wire.StatusPending, poll.Status)
		r.Empty(poll.Token, "a pending grant hands nothing over")
	})

	t.Run("denied is final", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		start := h.startPairing()
		id := h.pendingID(start.UserCode)
		changed, err := h.store.DecidePairing(ctx, id, wire.StatusDenied, nil, testAdmin)
		r.NoError(err)
		r.True(changed)

		poll := h.poll(start.DeviceCode)
		r.Equal(wire.StatusDenied, poll.Status)
		r.Empty(poll.Token)
	})

	t.Run("a grant issues its token once", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		start := h.startPairing()
		id := h.pendingID(start.UserCode)
		driver, err := h.store.EnsureDriver(ctx, "Marco Costa", "gt3")
		r.NoError(err)
		_, err = h.store.DecidePairing(ctx, id, wire.StatusApproved, &driver.ID, testAdmin)
		r.NoError(err)

		first := h.poll(start.DeviceCode)
		r.Equal(wire.StatusApproved, first.Status)
		r.NotEmpty(first.Token)
		r.NotNil(first.Driver)
		r.Equal("Marco Costa", first.Driver.Name)
		r.Equal("marco-costa", first.Driver.Slug)

		second := h.poll(start.DeviceCode)
		r.Equal(wire.StatusApproved, second.Status)
		r.Empty(second.Token, "a device-code grant hands its token over exactly once")
	})

	t.Run("a code that ran out is expired", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// The grant is written straight into the table with an expiry already
		// past, which is a client polling eleven minutes late without eleven
		// minutes passing in the test.
		codes, err := auth.NewPairingCodes()
		r.NoError(err)
		_, err = h.store.CreatePairing(ctx, codes.DeviceCodeSum, codes.UserCode,
			time.Now().Add(-time.Minute))
		r.NoError(err)

		poll := h.poll(codes.DeviceCode)
		r.Equal(wire.StatusExpired, poll.Status, "a grant past its expiry hands nothing over")
		r.Empty(poll.Token)
	})
}

func TestMe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("says who the token belongs to", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{method: http.MethodGet, path: "/api/v1/me"})
		r.Equal(http.StatusOK, res.status, res.body)

		var me wire.Me
		r.NoError(json.Unmarshal([]byte(res.body), &me))
		r.Equal(testDriverName, me.Driver.Name)
		r.Equal("ana-ruiz", me.Driver.Slug)
		r.Equal("gt3", me.Driver.Class)
		r.Nil(me.Team, "a community driver drives alone")
		r.True(me.Has(wire.FeatureTelemetry))
		r.True(me.Has(wire.FeatureReference))
		r.False(me.Has(wire.FeatureTTS), "no voice service is configured here")
	})

	t.Run("records the machine as used", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()

		// A machine of its own, and not the harness's. The server records a
		// machine as used at most once every few minutes and keeps the
		// bookkeeping for that in memory, so a second caller arriving while the
		// first is still writing is told the work is already done — correctly,
		// and not yet visibly. Two parallel subtests sharing one machine turn
		// that into a test that passes on whichever of them gets there first.
		token, driver, _ := h.pair("Sofia Marques")
		h.do(request{method: http.MethodGet, path: "/api/v1/me", token: token})

		devices, err := h.store.ListDevicesForDriver(ctx, driver.ID)
		r.NoError(err)
		r.NotEmpty(devices)
		r.NotNil(devices[0].LastUsedAt, "a request marks the machine as seen")
	})
}

func TestStints(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("stores a stint and echoes its identifier", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id,
			key: "stint-" + id, body: sampleStint(),
		})
		r.Equal(http.StatusOK, res.status, res.body)

		var out wire.StintResult
		r.NoError(json.Unmarshal([]byte(res.body), &out))
		r.Equal(id, out.StintID)
		r.Nil(out.SessionID, "competition is an enterprise feature, so no session is attached")
	})

	t.Run("repeating it updates the mutable fields", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		id := randomUUIDv7(t, startedAt)

		body := sampleStint()
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: "a-" + id, body: body,
		}).status)

		body.Car = "Porsche 911 GT3 R"
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: "b-" + id, body: body,
		}).status)

		parsed, err := db.ParseUUID(id)
		r.NoError(err)
		stint, err := h.store.Stint(ctx, parsed, h.driver.ID)
		r.NoError(err)
		r.Equal("Porsche 911 GT3 R", stint.Car, "a repeat is an update, not a second stint")
	})

	t.Run("an identifier that is not a UUID is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/not-a-uuid",
			key: "k", body: sampleStint(),
		})
		r.Equal(http.StatusUnprocessableEntity, res.status)
		r.Equal(wire.CodeInvalid, res.envelope(t).Code)
	})

	t.Run("a stint that belongs to somebody else is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: "mine-" + id, body: sampleStint(),
		}).status)

		other := h.newToken("Bea Lopez")
		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id,
			token: other, key: "theirs-" + id, body: sampleStint(),
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
		r.Equal(wire.CodeNotFound, res.envelope(t).Code,
			"a client must not learn that somebody else's identifier exists")
	})

	t.Run("the validation the contract asks for", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name  string
			field string
			mut   func(*wire.Stint)
		}{
			{"no simulator", "sim", func(s *wire.Stint) { s.Sim = "" }},
			{"no track", "track_id", func(s *wire.Stint) { s.TrackID = "" }},
			{"no car", "car", func(s *wire.Stint) { s.Car = "" }},
			{"an unknown session type", "session_type", func(s *wire.Stint) { s.SessionType = "hotlap" }},
			{"no start time", "started_at", func(s *wire.Stint) { s.StartedAt = time.Time{} }},
			{"sectors out of order", "sectors", func(s *wire.Stint) { s.Sectors = []float64{0.5, 0.2} }},
			{"a sector past the lap", "sectors", func(s *wire.Stint) { s.Sectors = []float64{0.5, 1.5} }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				body := sampleStint()
				tc.mut(&body)
				id := randomUUIDv7(t, startedAt)
				res := h.do(request{
					method: http.MethodPut, path: "/api/v1/stints/" + id,
					key: "v-" + id, body: body,
				})
				r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
				env := res.envelope(t)
				r.Equal(wire.CodeInvalid, env.Code)
				r.Equal(tc.field, env.Detail["field"])
				r.NotEmpty(env.Message)
			})
		}
	})

	t.Run("an unknown field in the body is refused rather than dropped", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: "u-" + id,
			raw: `{"sim":"iracing","track":"x","track_id":"x","car":"x","car_class":"gt3",` +
				`"session_type":"practice","started_at":"2026-09-12T14:03:11+02:00","sectors":[],"tyres":"soft"}`,
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		r.Equal(wire.CodeInvalid, res.envelope(t).Code)
	})
}

func TestIdempotency(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("the same key and body replays the stored answer", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		key := "replay-" + id

		first := h.do(request{method: http.MethodPut, path: "/api/v1/stints/" + id, key: key, body: sampleStint()})
		second := h.do(request{method: http.MethodPut, path: "/api/v1/stints/" + id, key: key, body: sampleStint()})
		r.Equal(first.status, second.status)
		r.Equal(first.body, second.body, "a replay is byte for byte the first answer")
	})

	t.Run("the same key with a different body is a conflict", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		key := "clash-" + id

		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: key, body: sampleStint(),
		}).status)

		changed := sampleStint()
		changed.Car = "Aston Martin Vantage GT3"
		res := h.do(request{method: http.MethodPut, path: "/api/v1/stints/" + id, key: key, body: changed})
		r.Equal(http.StatusConflict, res.status, res.body)
		r.Equal(wire.CodeConflict, res.envelope(t).Code)
	})

	t.Run("the same key on a different endpoint is a conflict", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		key := "crossed-" + id
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: key, body: sampleStint(),
		}).status)

		res := h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id + "/summary",
			key: key, body: wire.Summary{Laps: 3},
		})
		r.Equal(http.StatusConflict, res.status, res.body)
	})

	t.Run("a write with no key is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		id := randomUUIDv7(t, startedAt)
		res := h.do(request{method: http.MethodPut, path: "/api/v1/stints/" + id, body: sampleStint()})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		env := res.envelope(t)
		r.Equal(wire.CodeInvalid, env.Code)
		r.Equal(api.IdempotencyHeader, env.Detail["header"])
	})

	t.Run("two of the same request at once is stored once", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		ctx := context.Background()
		id := randomUUIDv7(t, startedAt)
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id, key: "s-" + id, body: sampleStint(),
		}).status)

		batch := wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 7, 91234)}}
		key := "double-" + id

		const racers = 6
		bodies := make([]string, racers)
		statuses := make([]int, racers)
		var g errgroup.Group
		for i := range racers {
			g.Go(func() error {
				res := h.do(request{
					method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
					key: key, body: batch,
				})
				bodies[i], statuses[i] = res.body, res.status
				return nil
			})
		}
		r.NoError(g.Wait())
		for i := range racers {
			r.Equal(http.StatusOK, statuses[i], bodies[i])
			r.Equal(bodies[0], bodies[i], "every one of them is answered the same bytes")
		}

		parsed, err := db.ParseUUID(id)
		r.NoError(err)
		stored, err := h.store.CountLapsForStint(ctx, parsed)
		r.NoError(err)
		r.EqualValues(1, stored, "the work happened once")
	})
}

// TestRoutesAreMeasuredByPattern pins the label a metric carries. A path holds
// a stint identifier, so labelling by path would be one time series per stint
// and a metrics port nobody can query; the pattern is what this server's
// observability decision asks for.
func TestRoutesAreMeasuredByPattern(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var mu sync.Mutex
	seen := map[string]int{}
	h := newHarness(t, harnessOptions{
		onRequest: func(route string, status int, _ time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			seen[route] = status
		},
	})

	id := randomUUIDv7(t, startedAt)
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id,
		key: "measured-" + id, body: sampleStint(),
	}).status)
	r.Equal(http.StatusOK, h.do(request{method: http.MethodGet, path: "/api/v1/me"}).status)

	mu.Lock()
	defer mu.Unlock()
	r.Equal(http.StatusOK, seen["PUT /api/v1/stints/{id}"],
		"one series for every stint, not one per stint")
	r.Equal(http.StatusOK, seen["GET /api/v1/me"])
	for route := range seen {
		r.NotContains(route, id, "an identifier must never become a metric label")
	}
}
