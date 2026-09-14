package admin

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/logging"
)

func TestOrderOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want db.RosterOrder
	}{
		{"nothing asked for", "", db.RosterByName},
		{"by name", "name", db.RosterByName},
		{"by last seen", "seen", db.RosterByLastSeen},
		{"by laps", "laps", db.RosterByLaps},
		{"spaces around it", "  laps  ", db.RosterByLaps},
		{"a word that is not an ordering", "sideways", db.RosterByName},
		{"a column name from the database", "trace_bytes", db.RosterByName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, orderOf(url.Values{paramSort: {tc.raw}}))
		})
	}
}

func TestRosterCursorOf(t *testing.T) {
	t.Parallel()
	seen := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	cases := []struct {
		name  string
		order db.RosterOrder
		query url.Values
		want  *db.RosterCursor
	}{
		{
			name:  "no parameters is the first page",
			order: db.RosterByName,
			query: url.Values{},
		},
		{
			name:  "an identifier that is not a number is the first page",
			order: db.RosterByName,
			query: url.Values{paramAfter: {"marta"}, paramFrom: {"Marta"}},
		},
		{
			name:  "a zero identifier is the first page",
			order: db.RosterByName,
			query: url.Values{paramAfter: {"0"}, paramFrom: {"Marta"}},
		},
		{
			name:  "a name cursor carries the name",
			order: db.RosterByName,
			query: url.Values{paramAfter: {"7"}, paramFrom: {"Marta Ferrer"}},
			want:  &db.RosterCursor{ID: 7, Name: "Marta Ferrer"},
		},
		{
			name:  "a last-seen cursor carries the instant",
			order: db.RosterByLastSeen,
			query: url.Values{paramAfter: {"7"}, paramFrom: {seen.Format(time.RFC3339Nano)}},
			want:  &db.RosterCursor{ID: 7, Seen: seen},
		},
		{
			name:  "a last-seen cursor that is not a time is the first page",
			order: db.RosterByLastSeen,
			query: url.Values{paramAfter: {"7"}, paramFrom: {"yesterday"}},
		},
		{
			name:  "a laps cursor carries the count",
			order: db.RosterByLaps,
			query: url.Values{paramAfter: {"7"}, paramFrom: {"412"}},
			want:  &db.RosterCursor{ID: 7, Laps: 412},
		},
		{
			name:  "a laps cursor that is not a number is the first page",
			order: db.RosterByLaps,
			query: url.Values{paramAfter: {"7"}, paramFrom: {"lots"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			got := rosterCursorOf(tc.query, tc.order)
			if tc.want == nil {
				r.Nil(got, "an unreadable cursor starts the list again rather than failing")
				return
			}
			r.NotNil(got)
			r.Equal(tc.want.ID, got.ID)
			r.Equal(tc.want.Name, got.Name)
			r.Equal(tc.want.Laps, got.Laps)
			r.True(tc.want.Seen.Equal(got.Seen))
		})
	}
}

func TestLapCursorOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"nothing asked for starts at the beginning", "", db.FirstLapCursor},
		{"lap zero is a real cursor", "0", 0},
		{"a lap number resumes after it", "24", 24},
		{"a negative number starts at the beginning", "-5", db.FirstLapCursor},
		{"a word starts at the beginning", "twelve", db.FirstLapCursor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, lapCursorOf(url.Values{paramAfter: {tc.raw}}))
		})
	}
}

func TestPagerTrimsAndLinks(t *testing.T) {
	t.Parallel()
	cursor := func(n int) url.Values { return url.Values{paramAfter: {itoa(int64(n))}} }

	cases := []struct {
		name      string
		rows      []int
		size      int
		onFirst   bool
		query     url.Values
		wantRows  int
		wantNext  string
		wantFirst string
	}{
		{
			name: "a short page has no links at all",
			rows: []int{1, 2}, size: 5, onFirst: true,
			wantRows: 2,
		},
		{
			name: "an exactly full page has no next, because nothing came back to prove one",
			rows: []int{1, 2, 3}, size: 3, onFirst: true,
			wantRows: 3,
		},
		{
			name: "one row over the size is trimmed and offers the next page",
			rows: []int{1, 2, 3, 4}, size: 3, onFirst: true,
			wantRows: 3, wantNext: "/list?after=3",
		},
		{
			name: "a later page can always get back to the first",
			rows: []int{1, 2}, size: 3, onFirst: false,
			wantRows: 2, wantFirst: "/list",
		},
		{
			name: "kept parameters travel with both links",
			rows: []int{1, 2, 3, 4}, size: 3, onFirst: false,
			query:    url.Values{paramSort: {"laps"}},
			wantRows: 3, wantNext: "/list?after=3&sort=laps", wantFirst: "/list?sort=laps",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			pg := pager[int]{base: "/list", query: tc.query}
			rows, links := pg.page(tc.rows, tc.size, tc.onFirst, cursor)
			r.Len(rows, tc.wantRows)
			r.Equal(tc.wantNext, links.Next)
			r.Equal(tc.wantFirst, links.First)
		})
	}
}

func TestMoment(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	t.Run("a thing that never happened is known to have never happened", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		r.False(momentOf(now, nil).Known)
		r.False(momentAt(now, time.Time{}).Known)
		r.False(momentOf(now, &time.Time{}).Known)
		r.False(momentOf(now, nil).Stale(), "never is not stale; it is never")
	})

	t.Run("a recent moment is known and not stale", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		at := now.Add(-2 * time.Hour)
		m := momentAt(now, at)
		r.True(m.Known)
		r.Equal(2*time.Hour, m.Ago)
		r.False(m.Stale())
	})

	t.Run("a moment older than a week is marked quiet", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		at := now.Add(-8 * 24 * time.Hour)
		r.True(momentAt(now, at).Stale(), "a driver who raced last weekend is not quiet; this one is")
	})
}

func TestBackOfAcceptsOnlyItsOwnTwoWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"nothing", "", backDevices},
		{"the devices list", "devices", backDevices},
		{"a driver's page", "driver", backDriver},
		{"spaces around it", "  driver ", backDriver},
		{"an address somebody typed in", "https://example.com/", backDevices},
		{"a path somebody typed in", "/admin/settings", backDevices},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			form := url.Values{paramBack: {tc.raw}}
			req := postFormRequest(t, form)
			r.Equal(tc.want, backOf(req))
		})
	}
}

func TestShareOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		part, whole int64
		want        string
	}{
		{"nothing of nothing has no share", 0, 0, ""},
		{"nothing of something has no share", 0, 100, ""},
		{"something of nothing has no share", 100, 0, ""},
		{"a whole", 100, 100, "100%"},
		{"a half", 50, 100, "50%"},
		{"a sliver is not rounded to nothing", 1, 1000, "under 1%"},
		{"rounded to the nearest point", 333, 1000, "33%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, shareOf(tc.part, tc.whole))
		})
	}
}

func TestPruneProgressPercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		total, done int64
		want        int
	}{
		{"a job with nothing to do is finished", 0, 0, 100},
		{"none of it yet", 100, 0, 0},
		{"a quarter", 100, 25, 25},
		{"all of it", 100, 100, 100},
		{"more than it expected, because laps aged past the line while it ran", 100, 140, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, PruneProgress{Total: tc.total, Done: tc.done}.Percent())
		})
	}
}

func TestPrunerRunsOneJobAtATime(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, -6, 0)

	p := &pruner{}
	r.False(p.snapshot().Started(), "nothing has run yet")
	r.True(p.begin("ana@example.com", cutoff, now, 400, 4096))
	r.False(p.begin("someone@example.com", cutoff, now, 400, 4096),
		"a second prune would report the first one's progress")

	p.advance(150)
	state := p.snapshot()
	r.True(state.Running)
	r.EqualValues(150, state.Done)
	r.Equal(37, state.Percent())

	p.finish(now.Add(time.Minute), nil)
	done := p.snapshot()
	r.False(done.Running)
	r.True(done.Started())
	r.Empty(done.Err)
	r.True(p.begin("ana@example.com", cutoff, now, 10, 10), "a finished job frees the slot")
}

// postFormRequest builds a request whose form is already parsed, so that a test
// of a form reader is about the reader and not about the parsing.
func postFormRequest(tb testing.TB, form url.Values) *http.Request {
	tb.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/devices/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(tb, req.ParseForm())
	return req
}

// A template that fails part-way through is a 500 with nothing written, not a
// 200 with half a page on it.
//
// This test is here because it happened. Removing the voice card left a dangling
// {{if .Form.Voice.Configured}} behind it; the settings page stopped rendering at
// that line; the server answered 200 with two thirds of a page. The browser
// showed something that looked nearly right, the log carried the only evidence,
// and every test passed — because the page was written as it was executed, so by
// the time the failure was known the status had been sent.
func TestAPageThatWillNotRenderIsAnErrorRatherThanHalfAPage(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A template that reaches for a field the data does not have, which is
	// exactly the shape of the mistake this is about.
	broken, err := template.New("broken").Funcs(funcMap()).Parse(
		`{{define "layout"}}<!doctype html><html><body>{{.Form.NotAField.AtAll}}</body></html>{{end}}`)
	r.NoError(err)

	p := &Panel{
		deps:      Deps{Log: logging.Discard(), Version: "v0.0.0-test"},
		templates: map[string]*template.Template{"broken": broken},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/settings", http.NoBody)
	p.render(rec, req, "broken", pageData{Title: "Broken", Organisation: "A league"})

	r.Equal(http.StatusInternalServerError, rec.Code)
	r.NotContains(rec.Body.String(), "<!doctype html>",
		"half a page was written before the failure was noticed")

	// And a page that renders is still written whole, with its own status.
	fine, err := template.New("fine").Funcs(funcMap()).Parse(
		`{{define "layout"}}<!doctype html><html><body>{{.Organisation}}</body></html>{{end}}`)
	r.NoError(err)
	p.templates["fine"] = fine

	rec = httptest.NewRecorder()
	p.render(rec, req, "fine", pageData{Organisation: "A league", Status: http.StatusTeapot})
	r.Equal(http.StatusTeapot, rec.Code)
	r.Contains(rec.Body.String(), "A league")
	r.Contains(rec.Body.String(), "</html>")
}
