//go:build postgres

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/config"
)

// The placeholders that stand in for the values that are random by definition.
// A device code and a token are secrets minted from crypto/rand, so committing
// the ones a run happened to produce would pin nothing and publish a habit.
const (
	placeholderDeviceCode = "0R0dMyBK3ZHB4zAOWSHpwzO0Oa_Chz0bXvEnDLgvVtc"
	placeholderUserCode   = "H4T-9KQ"
	placeholderToken      = "b2xG8dPPdcBcCw1jD7zvaJ8QY7kyu1rHJMDZMZ7hUxo"
)

// fixtureStintID is the identifier every fixture's stint carries. It is a real
// UUIDv7 built from the fixtures' own instant, so the committed documents show
// a client what it is expected to send.
var fixtureStintID = uuidV7(startedAt, []byte{0x2a})

// TestGoldenFixtures drives every endpoint and pins what it answered.
//
// The fixtures are written from real requests and real responses, not from
// hand-built structs, so a committed file is a thing this server actually said.
// They live outside internal/ because the capture client is built in another
// repository by people who cannot see this code, and what keeps the two
// interoperable is a set of files both sides assert against.
//
// Run with -update to rewrite them. A fixture that changes is a change to the
// contract, so read what changed before committing it.
func TestGoldenFixtures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A fully configured installation, so the documents show every field a
	// client can meet rather than the subset a bare server has. No plugin is
	// installed: a voice is served on a plugin's own route now, and nothing
	// about it appears in these documents.
	h := newHarness(t, harnessOptions{
		features: withFeatures(wire.FeatureField),
		settings: func(s *config.Settings) {
			s.Logo = "https://pacenote.example.com/logo.png"
			s.Accent = "#C6F24B"
		},
	})

	g := &fixtureWriter{t: t}

	// Discovery, and the pairing flow that follows from it.
	g.capture("discovery.response", new(wire.Discovery),
		h.do(request{method: http.MethodGet, path: config.DiscoveryPath, noAuth: true}), nil)

	start := h.startPairing()
	g.capture("pair-start.response", new(wire.PairStart),
		h.do(request{method: http.MethodPost, path: "/api/v1/pair/start", noAuth: true}),
		func(v any) {
			s, ok := v.(*wire.PairStart)
			require.True(t, ok)
			s.DeviceCode, s.UserCode = placeholderDeviceCode, placeholderUserCode
		})
	g.write("pair-poll.request", wire.PairPollRequest{DeviceCode: placeholderDeviceCode})

	g.capture("pair-poll-pending.response", new(wire.PairPoll),
		h.do(request{
			method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
			body: wire.PairPollRequest{DeviceCode: start.DeviceCode},
		}), nil)

	driver := h.mustDriver(ctx, "Ana Ruiz")
	_, err := h.store.DecidePairing(ctx, h.pendingID(start.UserCode), wire.StatusApproved, &driver.ID, testAdmin)
	require.NoError(t, err)
	g.capture("pair-poll-approved.response", new(wire.PairPoll),
		h.do(request{
			method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
			body: wire.PairPollRequest{DeviceCode: start.DeviceCode},
		}), func(v any) {
			poll, ok := v.(*wire.PairPoll)
			require.True(t, ok)
			poll.Token = placeholderToken
		})

	denied := h.startPairing()
	_, err = h.store.DecidePairing(ctx, h.pendingID(denied.UserCode), wire.StatusDenied, nil, testAdmin)
	require.NoError(t, err)
	g.capture("pair-poll-denied.response", new(wire.PairPoll),
		h.do(request{
			method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
			body: wire.PairPollRequest{DeviceCode: denied.DeviceCode},
		}), nil)

	g.capture("pair-poll-expired.response", new(wire.PairPoll),
		h.do(request{
			method: http.MethodPost, path: "/api/v1/pair/poll", noAuth: true,
			body: wire.PairPollRequest{DeviceCode: "a device code that ran out"},
		}), nil)

	// Identity.
	g.capture("me.response", new(wire.Me),
		h.do(request{method: http.MethodGet, path: "/api/v1/me"}), nil)

	// The three writes, request and response.
	stint := sampleStint()
	g.write("stint.request", stint)
	g.capture("stint.response", new(wire.StintResult),
		h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + fixtureStintID,
			key: "fixture-stint", body: stint,
		}), nil)

	batch := wire.LapBatch{Laps: []wire.Lap{sampleLap(t, 7, 91234)}}
	g.write("laps.request", batch)
	g.capture("laps.response", new(wire.LapBatchResult),
		h.do(request{
			method: http.MethodPost, path: "/api/v1/stints/" + fixtureStintID + "/laps",
			key: "fixture-laps", body: batch,
		}), nil)

	finished := startedAt.Add(20 * time.Minute)
	summary := sampleSummary(t, &finished)
	g.write("summary.request", summary)
	g.capture("summary.response", new(wire.OK),
		h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + fixtureStintID + "/summary",
			key: "fixture-summary", body: summary,
		}), nil)

	// The reference the coach compares against.
	g.capture("reference.response", new(wire.ReferenceLap),
		h.do(request{
			method: http.MethodGet,
			path:   referenceURL(testSim, "barcelona gp", "self"),
		}), nil)

	// The optional features.
	live := sampleLiveSample(fixtureStintID)
	g.write("live.request", live)
	require.Equal(t, http.StatusNoContent,
		h.do(request{method: http.MethodPost, path: "/api/v1/live", body: live}).status,
		"POST /live answers 204 and has no response document to pin")

	field := sampleFieldReport(fixtureStintID)
	g.write("field.request", field)
	g.capture("field.response", new(wire.FieldResult),
		h.do(request{
			method: http.MethodPost, path: "/api/v1/field",
			key: "fixture-field", body: field,
		}), nil)

	h.captureErrorFixtures(t, g)
}

// captureErrorFixtures provokes each of the eight codes for real and pins the
// envelope it produced.
func (h *harness) captureErrorFixtures(t *testing.T, g *fixtureWriter) {
	t.Helper()
	r := require.New(t)

	g.captureError("error-unauthorized.response", wire.CodeUnauthorized,
		h.do(request{method: http.MethodGet, path: "/api/v1/me", noAuth: true}))

	// A server with no plugins, which is what a fresh installation is: it
	// records laps and compares them, and the features that need something
	// running are absent rather than broken. It produces the forbidden
	// envelope for real rather than by construction.
	plain := newHarness(t, harnessOptions{})

	g.captureError("error-forbidden.response", wire.CodeForbidden,
		plain.do(request{
			method: http.MethodPost, path: "/api/v1/field", key: "fixture-forbidden",
			body: sampleFieldReport(plain.stint(t)),
		}))

	g.captureError("error-not_found.response", wire.CodeNotFound,
		h.do(request{
			method: http.MethodGet,
			path:   referenceURL(testSim, "a track nobody has driven", "self"),
		}))

	conflictKey := "fixture-conflict"
	changed := sampleStint()
	changed.Car = "Porsche 911 GT3 R"
	id := randomUUIDv7(t, startedAt)
	r.Equal(http.StatusOK, h.do(request{
		method: http.MethodPut, path: "/api/v1/stints/" + id, key: conflictKey, body: sampleStint(),
	}).status)
	g.captureError("error-conflict.response", wire.CodeConflict,
		h.do(request{method: http.MethodPut, path: "/api/v1/stints/" + id, key: conflictKey, body: changed}))

	bad := sampleStint()
	bad.SessionType = "hotlap"
	g.captureError("error-invalid.response", wire.CodeInvalid,
		h.do(request{
			method: http.MethodPut, path: "/api/v1/stints/" + randomUUIDv7(t, startedAt),
			key: "fixture-invalid", body: bad,
		}))

	g.captureError("error-client_too_old.response", wire.CodeClientTooOld,
		h.do(request{method: http.MethodGet, path: "/api/v1/me", version: "0.9.3"}))

	token := h.newToken("Fixture Flooder")
	var limited response
	for range 40 {
		res := h.do(request{method: http.MethodGet, path: "/api/v1/me", token: token})
		if res.status == http.StatusTooManyRequests {
			limited = res
			break
		}
	}
	r.Equal(http.StatusTooManyRequests, limited.status)
	g.captureError("error-rate_limited.response", wire.CodeRateLimited, limited)

	// A database that stopped, which is the shape of every server error a
	// driver actually meets now that nothing here calls a vendor. The harness
	// pairs its device first, so the request is a real one from a real
	// machine meeting a store that has gone away.
	broken := newHarness(t, harnessOptions{})
	broken.store.Close()
	g.captureError("error-server_error.response", wire.CodeServerError,
		broken.do(request{method: http.MethodGet, path: "/api/v1/me"}))
}

// fixtureWriter turns a response into a committed document, or asserts that the
// committed one is still what the server says.
type fixtureWriter struct{ t *testing.T }

// capture decodes one response into its wire type, applies whatever
// normalisation the random parts of it need, and pins the result.
func (g *fixtureWriter) capture(name string, into any, res response, normalise func(any)) {
	g.t.Helper()
	r := require.New(g.t)
	r.Less(res.status, 300, "%s: %s", name, res.body)
	r.NoError(json.Unmarshal([]byte(res.body), into), "%s: %s", name, res.body)
	if normalise != nil {
		normalise(into)
	}
	g.write(name, into)
}

// captureError pins one error envelope, having checked it is the code the
// contract pairs with the status that came back.
func (g *fixtureWriter) captureError(name string, code wire.Code, res response) {
	g.t.Helper()
	r := require.New(g.t)
	r.Equal(code.HTTPStatus(), res.status, "%s: %s", name, res.body)

	var env wire.ErrorEnvelope
	r.NoError(json.Unmarshal([]byte(res.body), &env), "%s: %s", name, res.body)
	r.NotNil(env.Error)
	r.Equal(code, env.Error.Code)
	g.write(name, env)
}

// write commits a document, or asserts that the committed one still matches.
func (g *fixtureWriter) write(name string, value any) {
	g.t.Helper()
	r := require.New(g.t)

	produced, err := renderFixture(value)
	r.NoError(err)
	path := fixturePath(name)

	if *update {
		r.NoError(os.MkdirAll(goldenDir, 0o755))
		r.NoError(os.WriteFile(path, produced, 0o644))
		return
	}
	committed, err := os.ReadFile(path)
	r.NoError(err, "%s.json is missing; run: go test -tags postgres ./internal/api -update", name)
	r.Equal(string(committed), string(produced),
		"%s.json is not what this server answered; if this is intended, run: go test -tags postgres ./internal/api -update", name)
}
