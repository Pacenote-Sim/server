package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
)

// goldenDir is where the request and response fixtures live. It is outside
// internal/ on purpose: the capture client is built in another repository by
// people who cannot see this code, and what keeps the two interoperable is a
// set of files both assert against.
const goldenDir = "../../testdata/api-v1"

// fixtures names every committed request and response document and the type on
// the wire that carries it.
//
// The list is the contract's own table of endpoints, written out: a fixture
// with no entry here, or an entry with no fixture, fails the test below, so the
// set cannot quietly lose a case.
var fixtures = map[string]func() any{
	"discovery.response":            func() any { return new(wire.Discovery) },
	"pair-start.response":           func() any { return new(wire.PairStart) },
	"pair-poll.request":             func() any { return new(wire.PairPollRequest) },
	"pair-poll-pending.response":    func() any { return new(wire.PairPoll) },
	"pair-poll-approved.response":   func() any { return new(wire.PairPoll) },
	"pair-poll-denied.response":     func() any { return new(wire.PairPoll) },
	"pair-poll-expired.response":    func() any { return new(wire.PairPoll) },
	"me.response":                   func() any { return new(wire.Me) },
	"stint.request":                 func() any { return new(wire.Stint) },
	"stint.response":                func() any { return new(wire.StintResult) },
	"laps.request":                  func() any { return new(wire.LapBatch) },
	"laps.response":                 func() any { return new(wire.LapBatchResult) },
	"summary.request":               func() any { return new(wire.Summary) },
	"summary.response":              func() any { return new(wire.OK) },
	"reference.response":            func() any { return new(wire.ReferenceLap) },
	"live.request":                  func() any { return new(wire.LiveSample) },
	"field.request":                 func() any { return new(wire.FieldReport) },
	"field.response":                func() any { return new(wire.FieldResult) },
	"tts.request":                   func() any { return new(wire.TTSRequest) },
	"error-unauthorized.response":   func() any { return new(wire.ErrorEnvelope) },
	"error-forbidden.response":      func() any { return new(wire.ErrorEnvelope) },
	"error-not_found.response":      func() any { return new(wire.ErrorEnvelope) },
	"error-conflict.response":       func() any { return new(wire.ErrorEnvelope) },
	"error-invalid.response":        func() any { return new(wire.ErrorEnvelope) },
	"error-rate_limited.response":   func() any { return new(wire.ErrorEnvelope) },
	"error-client_too_old.response": func() any { return new(wire.ErrorEnvelope) },
	"error-server_error.response":   func() any { return new(wire.ErrorEnvelope) },
}

// TestFixturesAreCanonical is what runs without a database, and it is the check
// the client repository can run against the same files.
//
// Every fixture decodes into its wire type and re-encodes to exactly the bytes
// on disk. That pins three things at once: the file is valid JSON, it carries
// no key the type does not have, and it carries every key the type does — so a
// fixture cannot drift from the contract without this failing.
func TestFixturesAreCanonical(t *testing.T) {
	t.Parallel()

	names := fixtureNames(t)
	require.Len(t, names, len(fixtures),
		"every committed fixture needs an entry in the table above, and every entry a file")

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			newValue, known := fixtures[name]
			r.True(known, "%s.json has no entry in the fixture table", name)

			committed, err := os.ReadFile(fixturePath(name))
			r.NoError(err)

			value := newValue()
			dec := json.NewDecoder(bytes.NewReader(committed))
			dec.DisallowUnknownFields()
			r.NoError(dec.Decode(value), "%s.json must decode into the type the contract gives it", name)

			produced, err := renderFixture(value)
			r.NoError(err)
			r.Equal(string(committed), string(produced),
				"%s.json is not what the wire type produces; if this is intended, run: go test -tags postgres ./internal/api -update", name)
		})
	}
}

// TestErrorFixturesCoverEveryCode asserts that the committed set holds one
// response for each of the eight codes, with the status the contract pairs with
// it and a sentence a driver can read.
func TestErrorFixturesCoverEveryCode(t *testing.T) {
	t.Parallel()

	for _, code := range wire.Codes() {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			raw, err := os.ReadFile(fixturePath("error-" + string(code) + ".response"))
			r.NoError(err, "every code the contract publishes needs a committed example")

			var env wire.ErrorEnvelope
			r.NoError(json.Unmarshal(raw, &env))
			r.NotNil(env.Error)
			r.Equal(code, env.Error.Code)
			r.NotZero(code.HTTPStatus())

			message := env.Error.Message
			r.NotEmpty(message, "the client shows this to the driver verbatim")
			r.True(strings.HasSuffix(message, "."), "a complete sentence ends in a full stop: %q", message)
			r.NotContains(message, "!", "nothing this server says to a driver is exclaimed")
			r.Equal(strings.ToUpper(message[:1]), message[:1], "sentence case: %q", message)
		})
	}
}

// renderFixture is the one spelling a committed fixture is written in: indented
// so it can be read in a review, with a trailing newline so it is a text file.
func renderFixture(value any) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func fixturePath(name string) string { return filepath.Join(goldenDir, name+".json") }

func fixtureNames(tb testing.TB) []string {
	tb.Helper()
	entries, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	require.NoError(tb, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(filepath.Base(e), ".json"))
	}
	sort.Strings(out)
	return out
}
