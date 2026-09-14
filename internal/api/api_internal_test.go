package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
)

func TestOlderThan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		got   string
		older bool
	}{
		{"the minimum itself is new enough", "1.0.0", false},
		{"a patch behind", "0.9.9", true},
		{"a major behind", "0.1.0", true},
		{"a major ahead", "2.0.0", false},
		{"a minor ahead", "1.1.0", false},
		{"a v prefix is not a different version", "v1.0.0", false},
		{"a release candidate counts as its release", "1.0.0-rc1", false},
		{"a release candidate of an older release is still older", "0.9.0-rc1", true},
		{"a build suffix is dropped", "1.0.0+deadbeef", false},
		{"a two-part version fills in the patch", "1.0", false},
		{"a one-part version fills in the rest", "1", false},
		{"an older two-part version", "0.9", true},
		{"nonsense claims nothing, so it is served", "the newest one", false},
		{"an empty string claims nothing", "", false},
		{"four parts is not a version", "1.0.0.0", false},
		{"a negative part is not a version", "1.-2.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.older, olderThan(tc.got, MinClient))
		})
	}
}

func TestScopePreference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ask  wire.Scope
		want []wire.Scope
	}{
		{"the narrowest asks for itself alone", wire.ScopeSelf, []wire.Scope{wire.ScopeSelf}},
		{"the car narrows to the driver's own", wire.ScopeCar, []wire.Scope{wire.ScopeCar, wire.ScopeSelf}},
		{"the class narrows twice", wire.ScopeClass, []wire.Scope{wire.ScopeClass, wire.ScopeCar, wire.ScopeSelf}},
		{"anything else is not a scope", "everyone", nil},
		{"an empty scope is not a scope either, and the caller defaults before asking", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, scopePreference(tc.ask))
		})
	}
}

func TestFeatures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		keys    Keys
		answers []plugin.RequestKind
		present []wire.Feature
		absent  []wire.Feature
	}{
		{
			name:    "a server with no plugins and nothing configured",
			present: []wire.Feature{wire.FeatureTelemetry, wire.FeatureReference, wire.FeatureLive},
			absent:  []wire.Feature{wire.FeatureCoach, wire.FeatureSetups, wire.FeatureTTS},
		},
		{
			name:    "a plugin answering cues",
			answers: []plugin.RequestKind{plugin.RequestCueTraining},
			present: []wire.Feature{wire.FeatureCoach},
			absent:  []wire.Feature{wire.FeatureSetups, wire.FeatureTTS},
		},
		{
			// The two are advertised separately because a plugin may answer
			// one and not the other, and a client that was told it had setup
			// advice shows a button that returns nothing.
			name:    "a plugin answering setup advice and not cues",
			answers: []plugin.RequestKind{plugin.RequestSetup},
			present: []wire.Feature{wire.FeatureSetups},
			absent:  []wire.Feature{wire.FeatureCoach},
		},
		{
			name:    "a plugin answering everything",
			answers: []plugin.RequestKind{plugin.RequestCueRace, plugin.RequestCueTraining, plugin.RequestSetup},
			present: []wire.Feature{wire.FeatureCoach, wire.FeatureSetups},
		},
		{
			name:    "a plugin that speaks",
			answers: []plugin.RequestKind{plugin.RequestSpeak},
			present: []wire.Feature{wire.FeatureTTS},
			absent:  []wire.Feature{wire.FeatureCoach, wire.FeatureSetups},
		},
		{
			// A plugin that coaches but does not speak. The two are separate
			// features because they are separate plugins, and a client told it
			// had a voice would show a button that returns nothing.
			name:    "a coach with nothing to speak it",
			answers: []plugin.RequestKind{plugin.RequestCueRace},
			present: []wire.Feature{wire.FeatureCoach},
			absent:  []wire.Feature{wire.FeatureTTS},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			doc := wire.Discovery{Features: Features(answering(tc.answers), tc.keys)}
			for _, f := range tc.present {
				r.True(doc.Has(f), "%s should be present", f)
			}
			for _, f := range tc.absent {
				r.False(doc.Has(f), "%s should be absent", f)
			}
			for _, f := range EnterpriseFeatures {
				r.False(doc.Has(f), "%s is an enterprise feature and is never in this build", f)
			}
		})
	}
}

// answering is a plugin host that answers these kinds and no others. A nil slice
// is a server with no plugin host at all, which is not the same as one whose
// plugins answer nothing — and both have no coach.
func answering(kinds []plugin.RequestKind) Answering {
	if kinds == nil {
		return nil
	}
	return fakeAnswering(kinds)
}

type fakeAnswering []plugin.RequestKind

func (f fakeAnswering) Answering(kind plugin.RequestKind) []string {
	for _, k := range f {
		if k == kind {
			return []string{"someplugin"}
		}
	}
	return nil
}

// TestLimiterComesFromTheDiscoveryLimits is D-7's guarantee in a test: the
// intervals a client is told to obey are the intervals the limiter enforces, so
// a client that honours the document it read is never refused.
func TestLimiterComesFromTheDiscoveryLimits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		class Class
		// interval reads the published limit this class is built from.
		interval func(wire.Limits) int
	}{
		{"live", ClassLive, func(l wire.Limits) int { return l.LiveIntervalMs }},
		{"field", ClassField, func(l wire.Limits) int { return l.FieldIntervalMs }},
		{"summary", ClassSummary, func(l wire.Limits) int { return l.SummaryIntervalMs }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			// A limiter built from a document that allows ten a second, and a
			// client sending far faster than any published interval.
			limits := config.DefaultLimits()
			limits.LiveIntervalMs, limits.FieldIntervalMs, limits.SummaryIntervalMs = 100, 100, 100
			l := NewLimiter(limits)
			r.Equal(100, tc.interval(limits))

			allowed := 0
			for range 100 {
				if ok, _ := l.Allow(tc.class, "one device"); ok {
					allowed++
				}
			}
			r.LessOrEqual(allowed, int(IntervalBurst)+1,
				"a client ignoring the published interval spends its burst and is then refused")

			ok, retry := l.Allow(tc.class, "one device")
			r.False(ok)
			r.Positive(retry, "a refusal says how long to back off for")
			r.LessOrEqual(retry, 1, "at ten a second the advice is a second, not a minute")
		})
	}
}

func TestLimiterIsPerDeviceUnderACeiling(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	l := NewLimiter(config.DefaultLimits())

	for range int(ReadBurst) {
		ok, _ := l.Allow(ClassRead, "greedy")
		r.True(ok)
	}
	ok, _ := l.Allow(ClassRead, "greedy")
	r.False(ok, "one device spends its own bucket")

	ok, _ = l.Allow(ClassRead, "quiet")
	r.True(ok, "and not anybody else's")
}

func TestBearer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"the ordinary spelling", "Bearer abc123", "abc123", true},
		{"the scheme is case-insensitive, as RFC 7235 says", "bearer abc123", "abc123", true},
		{"an upper-case scheme", "BEARER abc123", "abc123", true},
		{"trailing space is trimmed", "Bearer  abc123  ", "abc123", true},
		{"a different scheme is not a bearer token", "Basic abc123", "", false},
		{"a scheme with nothing after it", "Bearer ", "", false},
		{"no header at all", "", "", false},
		{"a bare token with no scheme", "abc123", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", http.NoBody)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got, ok := bearer(req)
			r.Equal(tc.ok, ok)
			r.Equal(tc.want, got)
		})
	}
}

func TestLiveStateHoldsTheLatestOnly(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	clock := &now
	state := newLiveState(func() time.Time { return *clock })
	id := db.UUID{1}

	state.put(id, 7, wire.LiveSample{Sample: wire.LivePoint{Lap: 1}})
	state.put(id, 7, wire.LiveSample{Sample: wire.LivePoint{Lap: 2}})

	held, err := state.get(id)
	r.NoError(err)
	r.Equal(2, held.Sample.Sample.Lap, "the latest sample only, which is all a live view wants")
	r.EqualValues(7, held.DriverID)

	*clock = now.Add(LiveTTL + time.Second)
	_, err = state.get(id)
	r.ErrorIs(err, ErrNoLiveSample, "a sample past its time to live is gone, not stale")
}

func TestLiveStateIsCapped(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	clock := &now
	state := newLiveState(func() time.Time { return *clock })

	for i := range LiveMaxStints + 50 {
		var id db.UUID
		id[0], id[1] = byte(i), byte(i>>8)
		state.put(id, int64(i), wire.LiveSample{})
	}
	r.LessOrEqual(len(state.entries), LiveMaxStints,
		"a flood of identifiers cannot grow the map without bound")
}

func TestFieldStateArmsOneClientPerSession(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	clock := &now
	state := newFieldState(func() time.Time { return *clock })

	bucket := fieldBucket("barcelona gp", "race")
	first, second := db.UUID{1}, db.UUID{2}
	quiet := 6 * time.Second

	r.True(state.report(bucket, first, wire.FieldReport{LapsTotal: 32}, quiet))
	r.False(state.report(bucket, second, wire.FieldReport{}, quiet),
		"only one client per session needs to send this")
	r.True(state.report(bucket, first, wire.FieldReport{LapsTotal: 32}, quiet),
		"the armed client carries on")

	report, _, held := state.latest(bucket)
	r.True(held)
	r.Equal(32, report.LapsTotal, "what is held is the armed client's report")

	*clock = now.Add(quiet + time.Second)
	r.True(state.report(bucket, second, wire.FieldReport{}, quiet),
		"a client that quit mid-session does not hold the relay for the rest of it")

	other := fieldBucket("jerez", "race")
	r.True(state.report(other, second, wire.FieldReport{}, quiet),
		"a different session is a different relay")
}

// TestMessagesFollowTheHouseStyle checks the one thing about these strings a
// test can check: they are sentences, in the second person, that a driver reads
// as written. The client shows them verbatim, so they are user interface.
func TestMessagesFollowTheHouseStyle(t *testing.T) {
	t.Parallel()
	messages := map[string]string{
		"no token":          msgNoToken,
		"bad token":         msgBadToken,
		"revoked token":     msgRevokedToken,
		"no stint":          msgNoStint,
		"no reference":      msgNoReference,
		"no route":          msgNoRoute,
		"key reused":        msgKeyReused,
		"key missing":       msgKeyMissing,
		"lap conflict":      msgLapConflict,
		"server error":      msgServerError,
		"unavailable again": msgUnavailableAgain,
		"tts unavailable":   msgTTSUnavailable,
		"tts failed":        msgTTSFailed,
		"field unavailable": msgFieldUnavailable,
		"live unavailable":  msgLiveUnavailable,
		"coach unavailable": msgCoachUnavailable,
	}
	for name, message := range messages {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.NotEmpty(message)
			r.True(strings.HasSuffix(message, "."), "a complete sentence ends in a full stop: %q", message)
			r.NotContains(message, "!", "nothing this server says to a driver is exclaimed")
			r.NotContains(message, "  ", "one space between words: %q", message)
			r.Equal(strings.ToUpper(message[:1]), message[:1], "sentence case: %q", message)
			for _, jargon := range []string{"HTTP", "SQL", "nil", "goroutine", "postgres", "PostgreSQL", "422", "500"} {
				r.NotContains(message, jargon, "a driver is not shown %q: %q", jargon, message)
			}
		})
	}
}

func TestStatusOfEveryCode(t *testing.T) {
	t.Parallel()
	for _, code := range wire.Codes() {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(code.HTTPStatus(), statusOf(code))
			r.GreaterOrEqual(statusOf(code), 400)
		})
	}
}

func TestPlural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		want string
	}{
		{1, "1 second"},
		{2, "2 seconds"},
		{30, "30 seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			require.New(t).Equal(tc.want, plural(tc.n, "second"))
		})
	}
}

// TestOpenKeysTreatsAnUnreadableKeyAsAbsent is D-8's honest answer: the data
// directory was lost and the database survived, so the feature is off until the
// operator enters the key again rather than failing one call at a time.
func TestOpenKeysTreatsAnUnreadableKeyAsAbsent(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// This server holds no vendor credential of any kind: the coaching key went
	// with the coaching and the voice key went with the voice, and both belong
	// to a plugin now. OpenKeys stays as the seam a build with credentials of
	// its own would use, and in this one it opens nothing.
	key, err := auth.NewSecretKey()
	r.NoError(err)
	r.Equal(Keys{}, OpenKeys(config.Settings{}, key))
	r.Equal(Keys{}, OpenKeys(config.Settings{}, nil))
}

// TestInvalidateKeepsTheLimitersBuckets is the rule the settings page depends
// on. Saving a form has to reach discovery on the next request, and it must not
// hand every client a fresh allowance while it is at it: a limiter rebuilt on
// each save would forgive a client that was sending too fast every time an
// operator touched an unrelated field, which is to say there would be no rate
// limiting at all.
func TestInvalidateKeepsTheLimitersBuckets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		change  func(*config.Settings)
		rebuilt bool
	}{
		{
			name:    "a name discovery publishes",
			change:  func(s *config.Settings) { s.Organisation = "Campeonato de España GT" },
			rebuilt: false,
		},
		{
			name:    "an accent colour",
			change:  func(s *config.Settings) { s.Accent = "#FF3B30" },
			rebuilt: false,
		},
		{
			name:    "a retention change, which no client is told about",
			change:  func(s *config.Settings) { s.Retention.TraceMonths = 6 },
			rebuilt: false,
		},
		{
			name:    "a published limit, which the limiter is built from",
			change:  func(s *config.Settings) { s.Limits.LiveIntervalMs = 2000 },
			rebuilt: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			stored := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy)
			a := &API{
				deps:     Deps{Log: nil, Now: time.Now, Features: Features},
				settings: stored,
				limiter:  NewLimiter(stored.Limits),
				readAt:   time.Now(),
			}
			before := a.limiter

			// Spend a token, so a rebuilt bucket is visibly a fresh one.
			for range int(IntervalBurst) {
				ok, _ := a.limiter.Allow(ClassLive, "a-device")
				r.True(ok)
			}
			refused, _ := a.limiter.Allow(ClassLive, "a-device")
			r.False(refused, "the bucket is empty before the settings change")

			a.Invalidate()
			r.True(a.readAt.IsZero(), "the next request re-reads the settings")

			// current() is what a request calls. Drive its second half
			// directly: the database is not what this test is about.
			next := stored
			tc.change(&next)
			a.applyLoaded(next, Keys{}, Features(nil, Keys{}), time.Now())

			if tc.rebuilt {
				r.NotSame(before, a.limiter, "new limits are a new limiter")
				allowed, _ := a.limiter.Allow(ClassLive, "a-device")
				r.True(allowed, "and a new limiter starts full, which is the point of changing it")
				return
			}
			r.Same(before, a.limiter, "an unrelated change leaves the limiter alone")
			stillRefused, _ := a.limiter.Allow(ClassLive, "a-device")
			r.False(stillRefused, "and the buckets it was holding are still empty")
		})
	}
}

// The whole-field relay's memory, and what it forgets when it is full.
//
// It holds one report per session in memory and nothing on disk, so the only
// thing bounding it is this: a server that has seen more sessions than it keeps
// drops the one nobody has spoken to for longest. Without that a long-running
// server accumulates a bucket for every track and session type anybody ever
// drove, and nothing ever removes one.
func TestTheFieldRelayForgetsTheQuietestSession(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Now()
	f := newFieldState(func() time.Time { return now })
	const quiet = time.Minute

	// One more session than it keeps, each a minute quieter than the last.
	buckets := make([]string, 0, FieldMaxSessions+1)
	for i := range FieldMaxSessions {
		now = now.Add(time.Minute)
		bucket := fmt.Sprintf("spa/practice/%d", i)
		buckets = append(buckets, bucket)
		r.True(f.report(bucket, db.UUID{}, wire.FieldReport{}, quiet), "%s was not armed", bucket)
	}
	r.Len(f.entries, FieldMaxSessions)

	// The first one in is the one nobody has spoken to for longest.
	now = now.Add(time.Minute)
	r.True(f.report("spa/race/new", db.UUID{}, wire.FieldReport{}, quiet))
	r.Len(f.entries, FieldMaxSessions, "the relay grew past what it keeps")
	_, _, ok := f.latest(buckets[0])
	r.False(ok, "the quietest session was kept and something else was dropped")
	_, _, ok = f.latest(buckets[len(buckets)-1])
	r.True(ok, "a session that was still talking was dropped")

	// A session nobody has reported is not a report.
	_, _, ok = f.latest("nurburgring/race")
	r.False(ok)
}

// Two clients in the same session: the first is armed and the second is not,
// until the first goes quiet.
func TestTheFieldRelayArmsOneClientAtATime(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Now()
	f := newFieldState(func() time.Time { return now })
	const quiet = time.Minute

	first, second := db.UUID{1}, db.UUID{2}
	r.True(f.report("spa/race", first, wire.FieldReport{}, quiet))
	r.False(f.report("spa/race", second, wire.FieldReport{}, quiet),
		"two clients were armed for the same session")
	r.True(f.report("spa/race", first, wire.FieldReport{}, quiet),
		"the armed client stopped being armed")

	// Once the armed one has been quiet for long enough, the next one takes
	// over — a driver whose client crashed must not silence the session.
	now = now.Add(quiet + time.Second)
	r.True(f.report("spa/race", second, wire.FieldReport{}, quiet),
		"a session stayed armed to a client that had gone away")
}

// The bearer-token comparison and the whitespace around it.
//
// It is written out by hand rather than taken from strings because it is on the
// path of every authenticated request, and because the scheme is ASCII: the
// Unicode case folding in the standard library would fold characters that
// cannot appear in an HTTP scheme and would cost more doing it.
func TestComparingTheAuthorizationScheme(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.True(equalFold("Bearer", "bearer"))
	r.True(equalFold("BEARER", "bearer"))
	r.True(equalFold("", ""))
	r.False(equalFold("Bearer", "Basic"))
	r.False(equalFold("Bearer", "Bear"), "a prefix is not a match")
	r.False(equalFold("bearer", "bearex"))

	r.Equal("token", trimSpace("  \ttoken \t "))
	r.Empty(trimSpace(" \t "))
	r.Equal("a b", trimSpace("a b"))
}

// A server whose published limits say nothing still caps every body. The number
// in the discovery document is where each route's cap comes from, and a
// settings row that has lost it must not become "no limit".
func TestEveryBodyIsCappedEvenWithNoPublishedLimit(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	a := &API{}
	r.EqualValues(config.DefaultLimits().MaxBodyBytes, a.bodyLimit(config.Settings{}),
		"a server with no published limit stopped capping bodies")

	var settings config.Settings
	settings.Limits.MaxBodyBytes = 4096
	r.EqualValues(4096, a.bodyLimit(settings))
}

// The housekeeping sweep stops when its context does, and takes its ticker
// with it. Nothing depends on it having run — both tables it clears check
// expiry for themselves — so the only thing it must not do is outlive the
// phase that started it.
func TestTheSweepStopsWithItsContext(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	// An interval of zero means the default, which is an hour: the sweep has to
	// come back on the cancelled context rather than on the tick.
	go func() { defer close(done); (&API{}).Sweep(ctx, 0) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.Fail("the sweeper did not stop when its context was cancelled")
	}
}
