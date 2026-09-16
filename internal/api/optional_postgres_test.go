//go:build postgres

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/db"
)

// withFeatures builds a feature set for a harness: the community answer plus
// whatever this test needs present.
func withFeatures(extra ...wire.Feature) func() []wire.Feature {
	return func() []wire.Feature { return append(api.Features(), extra...) }
}

func sampleLiveSample(stintID string) wire.LiveSample {
	return wire.LiveSample{
		StintID: stintID,
		At:      lapStartedAt.Add(42 * time.Second),
		Sample: wire.LivePoint{
			TracePoint: wire.TracePoint{
				OffsetMs: 42000, SpeedKmh: 213, Throttle: 64, Brake: 0, Gear: 4,
				RPM: 8100, Steer: -37, DistPct: 462, LatG: 118, LongG: -22,
				La: 4157000, Lo: 210399,
			},
			Lap: 7, OnTrack: true, OnPitRoad: false,
		},
		CurLap: []wire.TracePoint{
			{OffsetMs: 0, SpeedKmh: 205, Throttle: 100, Gear: 4, RPM: 7900, DistPct: 0},
			{OffsetMs: 300, SpeedKmh: 209, Throttle: 100, Gear: 4, RPM: 8000, DistPct: 3},
		},
	}
}

func TestLive(t *testing.T) {
	t.Parallel()

	t.Run("a sample is taken and never written down", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{})
		id := h.stint(t)

		res := h.do(request{method: http.MethodPost, path: "/api/v1/live", body: sampleLiveSample(id)})
		r.Equal(http.StatusNoContent, res.status, res.body)
		r.Empty(res.body, "fire and forget has nothing to say")

		parsed, err := db.ParseUUID(id)
		r.NoError(err)
		held, err := h.api.Live(parsed)
		r.NoError(err, "the sample is in memory, where D-5 says it lives")
		r.Equal(7, held.Sample.Sample.Lap)
		r.Equal(h.driver.ID, held.DriverID)

		count, err := h.store.CountLapsForStint(context.Background(), parsed)
		r.NoError(err)
		r.Zero(count, "a live sample is never a lap")
	})

	t.Run("no idempotency key is needed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{})
		id := h.stint(t)
		res := h.do(request{method: http.MethodPost, path: "/api/v1/live", body: sampleLiveSample(id)})
		r.Equal(http.StatusNoContent, res.status,
			"the contract's own exception: a call that is never retried needs no key")
	})

	t.Run("a stint identifier that is not a UUID is invalid", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/live",
			body: sampleLiveSample("not-a-uuid"),
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		r.Equal("stint_id", res.envelope(t).Detail["field"])
	})

	t.Run("an installation without it answers forbidden rather than 404", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{
			features: func() []wire.Feature {
				return []wire.Feature{wire.FeatureTelemetry, wire.FeatureReference}
			},
		})
		id := h.stint(t)
		res := h.do(request{method: http.MethodPost, path: "/api/v1/live", body: sampleLiveSample(id)})
		r.Equal(http.StatusForbidden, res.status, res.body)
		r.Equal(wire.CodeForbidden, res.envelope(t).Code)
	})
}

func sampleFieldReport(stintID string) wire.FieldReport {
	return wire.FieldReport{
		StintID:     stintID,
		SessionType: wire.SessionRace,
		Flag:        wire.FlagGreen,
		LapsTotal:   32,
		Cars: []wire.FieldCar{
			{
				Num: "17", Driver: "A. Ruiz", Class: "gt3", Lap: 12, DistPct: 0.42,
				LastMs: 91020, BestMs: 90440, GapMs: 0, Pit: false, Inc: 4, IsPlayer: true,
			},
			{
				Num: "7", Driver: "M. Costa", Class: "gt3", Lap: 12, DistPct: 0.39,
				LastMs: 91240, BestMs: 90118, GapMs: 3120, Pit: false, Inc: 2, IsPlayer: false,
			},
		},
	}
}

func TestField(t *testing.T) {
	t.Parallel()

	t.Run("the first client for a session is the one relaying it", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{features: withFeatures(wire.FeatureField)})

		first := h.stint(t)
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/field",
			key: "field-1", body: sampleFieldReport(first),
		})
		r.Equal(http.StatusOK, res.status, res.body)
		var out wire.FieldResult
		r.NoError(json.Unmarshal([]byte(res.body), &out))
		r.True(out.OK)
		r.True(out.Armed)

		// A second client on the same track and session type is told it is not
		// the one. That is not an error and the driver is never told.
		second := h.newToken("Second Relay")
		other := randomUUIDv7(t, startedAt)
		r.Equal(http.StatusOK, h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + other,
			token: second, key: "second-stint", body: sampleStint(),
		}).status)

		res = h.do(request{
			method: http.MethodPost, path: "/api/v1/field", token: second,
			key: "field-2", body: sampleFieldReport(other),
		})
		r.Equal(http.StatusOK, res.status, res.body)
		r.NoError(json.Unmarshal([]byte(res.body), &out))
		r.True(out.OK)
		r.False(out.Armed, "only one client per session needs to send this")

		report, _, held := h.api.Field("barcelona gp", string(wire.SessionRace))
		r.True(held)
		r.Len(report.Cars, 2, "what is held is the armed client's report")
	})

	t.Run("a report for somebody else's stint is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{features: withFeatures(wire.FeatureField)})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/field",
			key: "field-orphan", body: sampleFieldReport(randomUUIDv7(t, startedAt)),
		})
		r.Equal(http.StatusNotFound, res.status, res.body)
	})

	t.Run("it needs an idempotency key like every other write", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		h := newHarness(t, harnessOptions{features: withFeatures(wire.FeatureField)})
		id := h.stint(t)
		res := h.do(request{method: http.MethodPost, path: "/api/v1/field", body: sampleFieldReport(id)})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)
		r.Equal(api.IdempotencyHeader, res.envelope(t).Detail["header"])
	})
}
