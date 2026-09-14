//go:build postgres

package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
)

// referenceURL builds the query GET /reference takes. Every test here drives
// the same car in the same class, because what varies between them is the
// simulator, the track and the scope.
func referenceURL(sim, trackID, scope string) string {
	q := url.Values{}
	if sim != "" {
		q.Set("sim", sim)
	}
	q.Set("track_id", trackID)
	q.Set("car", benchmarkCar)
	q.Set("class", benchmarkCarClass)
	if scope != "" {
		q.Set("scope", scope)
	}
	return "/api/v1/reference?" + q.Encode()
}

// uploadLap puts one clean lap on one track for one machine, in the simulator
// every other fixture in this package uses.
func (h *harness) uploadLap(tb testing.TB, token, trackID string, lapMs, number int) {
	tb.Helper()
	h.uploadLapIn(tb, token, testSim, trackID, lapMs, number)
}

// uploadLapIn puts one clean lap on one track for one machine in one simulator,
// creating the stint it belongs to. It is the precondition of every reference
// assertion, and each caller uses its own track so that the reference buckets
// of two parallel subtests do not overlap.
func (h *harness) uploadLapIn(tb testing.TB, token, sim, trackID string, lapMs, number int) {
	tb.Helper()
	r := require.New(tb)
	id := randomUUIDv7(tb, startedAt)

	stint := sampleStint()
	stint.Sim = sim
	stint.TrackID = trackID
	stint.Track = trackID
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id,
		token: token, key: "ul-stint-" + id, body: stint,
	}).status)

	res := h.do(request{
		method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
		token: token, key: "ul-laps-" + id,
		body: wire.LapBatch{Laps: []wire.Lap{sampleLap(tb, number, lapMs)}},
	})
	r.Equal(http.StatusOK, res.status, res.body)
}

func TestReference(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	t.Run("the driver's own best, with no name beside it", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		const track = "barcelona gp"
		h.uploadLap(t, h.token, track, 92100, 1)
		h.uploadLap(t, h.token, track, 90118, 2)

		res := h.do(request{
			method: http.MethodGet,
			path:   referenceURL(testSim, track, "self"),
		})
		r.Equal(http.StatusOK, res.status, res.body)

		var ref wire.ReferenceLap
		r.NoError(json.Unmarshal([]byte(res.body), &ref))
		r.Equal(wire.ScopeSelf, ref.Scope)
		r.Equal(90118, ref.LapMs, "the best of the two, not the last of them")
		r.Empty(ref.DriverName, "a driver's own lap needs no name beside it")
		r.Equal(protocolGolden(t, "lap-300"), ref.Trace,
			"the trace comes back out of the database as the points that went in")
	})

	t.Run("somebody else's best in the same car, named", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// Its own track, because the reference table is one row per track, car
		// and class and two parallel subtests must not share a bucket.
		const track = "jerez"
		h.uploadLap(t, h.token, track, 90118, 1)
		rival := h.newToken("Marco Costa")
		h.uploadLap(t, rival, track, 93000, 1)

		res := h.do(request{
			method: http.MethodGet, token: rival,
			path: referenceURL(testSim, track, "car"),
		})
		r.Equal(http.StatusOK, res.status, res.body)

		var ref wire.ReferenceLap
		r.NoError(json.Unmarshal([]byte(res.body), &ref))
		r.Equal(wire.ScopeCar, ref.Scope)
		r.Equal(90118, ref.LapMs, "the best anyone has driven in that car")
		r.Equal(testDriverName, ref.DriverName, "somebody else's lap carries their name")
	})

	t.Run("a scope this server has no data for falls back and says so", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		const track = "monza"
		h.uploadLap(t, h.token, track, 90118, 1)

		res := h.do(request{
			method: http.MethodGet,
			path:   referenceURL(testSim, track, "class"),
		})
		r.Equal(http.StatusOK, res.status, res.body)

		var ref wire.ReferenceLap
		r.NoError(json.Unmarshal([]byte(res.body), &ref))
		r.Equal(wire.ScopeCar, ref.Scope,
			"the class-wide scope is an enterprise feature, so the answer narrows to the car and says which it used")
	})

	t.Run("the reference is the best, not the latest", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		const track = "portimao"
		token := h.newToken("Iker Vidal")

		h.uploadLap(t, token, track, 92000, 1)
		h.uploadLap(t, token, track, 89440, 2)
		h.uploadLap(t, token, track, 95000, 3)

		res := h.do(request{
			method: http.MethodGet, token: token,
			path: referenceURL(testSim, track, "self"),
		})
		r.Equal(http.StatusOK, res.status, res.body)
		var ref wire.ReferenceLap
		r.NoError(json.Unmarshal([]byte(res.body), &ref))
		r.Equal(89440, ref.LapMs)
	})

	t.Run("a lap that is not clean is never a reference", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		const track = "imola"
		token := h.newToken("Nuria Sanz")
		id := randomUUIDv7(t, startedAt)

		stint := sampleStint()
		stint.TrackID, stint.Track = track, track
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + id,
			token: token, key: "dirty-stint", body: stint,
		}).status)

		lap := sampleLap(t, 1, 80000)
		lap.Kind = wire.KindInvalid
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + id + "/laps",
			token: token, key: "dirty-laps", body: wire.LapBatch{Laps: []wire.Lap{lap}},
		}).status)

		res := h.do(request{
			method: http.MethodGet, token: token,
			path: referenceURL(testSim, track, "self"),
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
	})

	t.Run("a track nobody has driven is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodGet,
			path:   referenceURL(testSim, "spa", "self"),
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
		env := res.envelope(t)
		r.Equal(wire.CodeNotFound, env.Code)
		r.Equal("spa", env.Detail["track_id"])
		r.NotEmpty(env.Message)
	})

	t.Run("no track at all is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{method: http.MethodGet, path: "/api/v1/reference?sim=" + testSim + "&car=x"})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		r.Equal("track_id", res.envelope(t).Detail["field"])
	})

	t.Run("a scope this server does not know is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodGet,
			path:   referenceURL(testSim, "barcelona gp", "everyone"),
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		r.Equal("scope", res.envelope(t).Detail["field"])
	})
}

// TestReferenceIsScopedBySimulator is the case the sim column exists for.
//
// One driver, one circuit, one car name, two simulators. Assetto Corsa
// Competizione and iRacing disagree about the physics of Spa and therefore
// about what a lap of it costs, so a lap from one is not a reference for a lap
// from the other: told they are seconds off a time nobody driving their
// simulator has ever set, a driver would chase a number that does not exist.
// Every lookup here asks for one simulator and must be answered from it.
func TestReferenceIsScopedBySimulator(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	const (
		track   = "spa"
		iracing = "iracing"
		acc     = "acc"
	)

	// The same driver, the same circuit, the same car name, both simulators.
	// The ACC lap is the quicker of the two, so a lookup that ignored the
	// simulator would answer with it whichever one was asked for.
	token := h.newToken("Lucía Ferrán")
	h.uploadLapIn(t, token, iracing, track, 138400, 1)
	h.uploadLapIn(t, token, acc, track, 134900, 1)

	cases := []struct {
		name  string
		sim   string
		scope string
		want  int
	}{
		{
			name: "their own iRacing best, not their quicker ACC lap",
			sim:  iracing, scope: "self", want: 138400,
		},
		{
			name: "their own ACC best, not their slower iRacing lap",
			sim:  acc, scope: "self", want: 134900,
		},
		{
			name: "the best in that car in iRacing",
			sim:  iracing, scope: "car", want: 138400,
		},
		{
			name: "the best in that car in ACC",
			sim:  acc, scope: "car", want: 134900,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			res := h.do(request{
				method: http.MethodGet, token: token,
				path: referenceURL(tc.sim, track, tc.scope),
			})
			r.Equal(http.StatusOK, res.status, res.body)

			var ref wire.ReferenceLap
			r.NoError(json.Unmarshal([]byte(res.body), &ref))
			r.Equal(tc.want, ref.LapMs,
				"a lap from another simulator was served as the reference for %s", tc.sim)
		})
	}

	t.Run("a simulator nobody has driven this circuit in is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodGet, token: token,
			path: referenceURL("rfactor2", track, "class"),
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
		r.Equal("rfactor2", res.envelope(t).Detail["sim"])
	})

	t.Run("no simulator at all is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		res := h.do(request{
			method: http.MethodGet, token: token,
			path: referenceURL("", track, "self"),
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		r.Equal("sim", res.envelope(t).Detail["field"])
	})
}
