//go:build postgres

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/db"
)

// withFeatures builds a feature set for a harness: the community answer plus
// whatever this test needs present.
func withFeatures(extra ...wire.Feature) func(api.Answering, api.Keys) []wire.Feature {
	return func(a api.Answering, k api.Keys) []wire.Feature {
		return append(api.Features(a, k), extra...)
	}
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
			features: func(api.Answering, api.Keys) []wire.Feature {
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

// speaker is a plugin host that answers a request for audio however the test
// says. It stands in for a voice plugin, which is where the vendor, the
// credential and the engine's dialect all live now.
type speaker struct {
	audio     []byte
	audioType string
	err       error
	asked     atomic.Int64
	lastText  atomic.Pointer[string]
}

func (s *speaker) Notify(context.Context, plugin.Event) {}

func (s *speaker) Ask(_ context.Context, r plugin.Request) (plugin.Response, error) {
	s.asked.Add(1)
	text := r.Text
	s.lastText.Store(&text)
	if s.err != nil {
		return plugin.Response{}, s.err
	}
	return plugin.Response{Kind: r.Kind, Audio: s.audio, AudioType: s.audioType}, nil
}

func (s *speaker) Answering(kind plugin.RequestKind) []string {
	if kind == plugin.RequestSpeak {
		return []string{"voice"}
	}
	return nil
}

// TestTTS is the one endpoint whose answer is bytes. The server has no voice of
// its own: it hands the line to whichever plugin answers and relays what comes
// back, so what is tested here is the relaying and the refusing.
func TestTTS(t *testing.T) {
	t.Parallel()

	const wav = "RIFF....WAVEfmt "
	cue := wire.TTSRequest{Text: "Turn 4, more entry speed.", Lang: "en"}

	t.Run("a plugin speaks and the audio is relayed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		voice := &speaker{audio: []byte(wav), audioType: "audio/wav"}
		h := newHarness(t, harnessOptions{plugins: voice})

		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-1", body: cue,
		})
		r.Equal(http.StatusOK, res.status, res.body)
		r.Equal("audio/wav", res.headers.Get("Content-Type"))
		r.Equal(wav, res.body)
		r.EqualValues(1, voice.asked.Load())

		// The plugin is given the line and nothing it has to parse out of
		// something larger.
		sent := voice.lastText.Load()
		r.NotNil(sent)
		r.Equal(cue.Text, *sent)
	})

	t.Run("a server with no voice plugin says so", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// Nothing answers, so the feature is not advertised and the endpoint
		// refuses. A client turns the button off rather than showing an error.
		h := newHarness(t, harnessOptions{})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-none", body: cue,
		})
		r.Equal(http.StatusForbidden, res.status, res.body)
		r.NotContains(res.body, "Anthropic")
		r.Contains(res.body, "plugin")
	})

	t.Run("a plugin that is not configured yet", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		voice := &speaker{err: plugin.ErrNotConfigured}
		h := newHarness(t, harnessOptions{plugins: voice})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-unconf", body: cue,
		})
		r.Equal(http.StatusForbidden, res.status, res.body)
		r.Contains(res.body, "not been configured")
	})

	t.Run("a plugin that will not answer", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		voice := &speaker{err: errors.New("the vendor refused: invalid api key")}
		h := newHarness(t, harnessOptions{plugins: voice})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-fail", body: cue,
		})
		r.Equal(http.StatusInternalServerError, res.status, res.body)
		// The plugin's own words are not shown to a driver: they are about the
		// operator's account and mean nothing in a car.
		r.NotContains(res.body, "invalid api key")
	})

	t.Run("audio a client cannot play is refused rather than relayed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		// The likeliest shape of this is a plugin passing on an HTML error page
		// it did not notice. Relaying it would have the client try to play it.
		voice := &speaker{audio: []byte("<html>rate limited</html>"), audioType: "text/html"}
		h := newHarness(t, harnessOptions{plugins: voice})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-html", body: cue,
		})
		r.Equal(http.StatusInternalServerError, res.status, res.body)
		r.NotContains(res.body, "<html>")
	})

	t.Run("a plugin that answers with nothing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		voice := &speaker{audioType: "audio/wav"}
		h := newHarness(t, harnessOptions{plugins: voice})
		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-empty", body: cue,
		})
		r.Equal(http.StatusInternalServerError, res.status, res.body)
	})

	t.Run("a cue with nothing to say, and one too long", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		voice := &speaker{audio: []byte(wav), audioType: "audio/wav"}
		h := newHarness(t, harnessOptions{plugins: voice})

		res := h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-blank",
			body: wire.TTSRequest{Text: "   ", Lang: "en"},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)

		res = h.do(request{
			method: http.MethodPost, path: "/api/v1/tts", key: "tts-long",
			body: wire.TTSRequest{Text: strings.Repeat("a", api.MaxTTSTextLen+1), Lang: "en"},
		})
		r.Equal(http.StatusUnprocessableEntity, res.status, res.body)

		// Neither reached the plugin: a refusal here costs the operator nothing.
		r.Zero(voice.asked.Load())
	})
}
