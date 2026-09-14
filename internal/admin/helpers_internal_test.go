package admin

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/db"
)

// The small functions between a request's query string and a database query,
// and between a build and the file name a browser is handed.
//
// Every one of them takes something off the wire. A cursor comes out of the
// address bar and a file name comes out of a path an operator configured, so
// each is checked for what it does with a value nobody sensible would send.

func TestACursorThatIsNotOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A stint cursor needs both halves and both have to parse. Anything else
	// is the first page, which is the safe answer: it shows the operator
	// something rather than an error about a value they did not type.
	r.Nil(stintCursorOf(url.Values{}))
	r.Nil(stintCursorOf(url.Values{paramAfter: {"not-a-uuid"}, paramFrom: {stamp(time.Now())}}))
	r.Nil(stintCursorOf(url.Values{paramAfter: {db.UUID{1}.String()}, paramFrom: {"yesterday"}}))

	at := time.Now().UTC().Truncate(time.Microsecond)
	got := stintCursorOf(url.Values{paramAfter: {db.UUID{1}.String()}, paramFrom: {stamp(at)}})
	r.NotNil(got)
	r.WithinDuration(at, got.StartedAt, time.Microsecond)

	// And the build history's, which is keyed on a plain integer.
	r.Nil(clientBuildCursorOf(url.Values{paramAfter: {"0"}, paramFrom: {stamp(at)}}))
	r.Nil(clientBuildCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {"yesterday"}}))
	r.NotNil(clientBuildCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {stamp(at)}}))
}

// The roster's cursor, which is one of three depending on how the page is
// sorted. A sort nobody asked for falls back to the name, which is the ordering
// the page shows by default.
func TestTheRosterCursorFollowsTheSort(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	at := time.Now().UTC().Truncate(time.Microsecond)
	c := db.RosterCursor{Name: "Ana Ruiz", Laps: 42, Seen: at, ID: 7}

	r.Equal("Ana Ruiz", rosterCursorParams(db.RosterByName, c).Get(paramFrom))
	r.Equal("Ana Ruiz", rosterCursorParams(db.RosterOrder("something nobody added"), c).Get(paramFrom),
		"an ordering nobody added paged by something the query does not read")
	r.Equal("42", rosterCursorParams(db.RosterByLaps, c).Get(paramFrom))
	r.Equal(stamp(at), rosterCursorParams(db.RosterByLastSeen, c).Get(paramFrom))

	// And reading them back, which is the other half of the same rule.
	r.Nil(rosterCursorOf(url.Values{}, db.RosterByName))
	r.Nil(rosterCursorOf(url.Values{paramAfter: {"0"}}, db.RosterByName))
	r.Nil(rosterCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {"yesterday"}}, db.RosterByLastSeen))
	r.Nil(rosterCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {"lots"}}, db.RosterByLaps))
	r.NotNil(rosterCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {"42"}}, db.RosterByLaps))
	r.NotNil(rosterCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {"Ana"}},
		db.RosterOrder("something nobody added")))

	// The devices list pages the same way.
	r.Nil(deviceCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {"yesterday"}}))
	r.NotNil(deviceCursorOf(url.Values{paramAfter: {"7"}, paramFrom: {stamp(at)}}))
}

// What a built client is called when it reaches a browser.
//
// The name comes from the path an operator configured, and it is written into a
// Content-Disposition header. A quote or a backslash in it would end the header
// early, so nothing that could is allowed through.
func TestTheNameABuiltClientIsDownloadedAs(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("pacenote-telemetry.exe", sanitiseFileName("pacenote-telemetry.exe"))
	r.Equal("pacenote-telemetry.exe", sanitiseFileName("/srv/clients/pacenote-telemetry.exe"))
	r.Equal("client.exe", sanitiseFileName(`"client\.exe"`))
	r.Equal("client.exe", sanitiseFileName(""), "a file with no name is still downloadable")
	r.Equal("client.exe", sanitiseFileName("."))
	r.NotContains(sanitiseFileName("a\r\nSet-Cookie: x=1"), "\n")
}

// Which version the build history records: the module version where the
// toolchain wrote one, the commit where it did not, and "unknown" where there
// is neither — because a client built outside a repository still has to be
// distinguishable from the next one months later.
func TestTheVersionRecordedForABuild(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("v1.4.0", clientVersionOf(clientbuild.Client{Version: "v1.4.0"}))
	r.Equal("0123456789ab", clientVersionOf(clientbuild.Client{
		Version: "(devel)", Revision: "0123456789abcdef0123456789abcdef01234567",
	}))
	r.Equal("0123456789ab", clientVersionOf(clientbuild.Client{
		Revision: "0123456789abcdef0123456789abcdef01234567",
	}))
	r.Equal("unknown", clientVersionOf(clientbuild.Client{}))
	r.Equal("unknown", clientVersionOf(clientbuild.Client{Version: "(devel)"}))
}

// One prune at a time. The second press is refused rather than queued, because
// two prunes walking the same rows would each report a total the other is
// already deleting — and the page shows that total as progress.
func TestOnlyOnePruneRunsAtATime(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	now := time.Now()
	cutoff := now.AddDate(0, -6, 0)
	var p pruner

	r.True(p.begin("ana@example.com", cutoff, now, 100, 4096))
	r.False(p.begin("marc@example.com", cutoff, now, 100, 4096),
		"a second prune started beside the first")

	got := p.snapshot()
	r.True(got.Running)
	r.Equal("ana@example.com", got.Actor, "the second press took the first one's prune")
	r.EqualValues(100, got.Total)

	p.advance(40)
	r.EqualValues(40, p.snapshot().Done)

	// A prune that stopped early says why, and lets the next one start.
	p.finish(now.Add(time.Minute), errors.New("the database did not answer"))
	got = p.snapshot()
	r.False(got.Running)
	r.Equal("the database did not answer", got.Err)
	r.True(p.begin("marc@example.com", cutoff, now, 100, 4096))

	// And one that finished cleanly leaves no error behind it.
	p.finish(now.Add(2*time.Minute), nil)
	r.Empty(p.snapshot().Err)
}
