package setup_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/setup"
)

func TestFreshDirectoryServesTheWizard(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{})

	res := h.get("/setup")
	r.Equal(http.StatusOK, res.status)
	r.Contains(res.body, "Set up this server")
	r.Contains(res.body, "Setup token")
	r.NotContains(res.body, token, "the page must not echo the token back")

	// The root goes to the wizard, so an operator who types the bare host
	// lands in the right place.
	root := h.get("/")
	r.Equal(http.StatusSeeOther, root.status)
	r.Equal("/setup", root.location)

	// Nothing is written until setup finishes.
	_, err := os.Stat(h.dataDir)
	r.ErrorIs(err, os.ErrNotExist)
}

func TestSetupModeServesNothingElse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"the admin panel", "/admin"},
		{"the api", "/api/v1/me"},
		{"discovery", "/.well-known/sim-telemetry.json"},
		{"pairing", "/pair"},
		{"anything else", "/whatever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{})
			res := h.get(tc.path)
			r.Equal(http.StatusNotFound, res.status)
			r.Contains(res.body, "has not been set up yet")
		})
	}
}

func TestTheStylesheetIsServed(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{})
	res := h.get("/assets/app.css")
	r.Equal(http.StatusOK, res.status)
	r.Contains(res.body, "--bg:")
}

func TestWrongTokenIsRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		typed string
	}{
		{"a completely different token", "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF-GGGG-HHHH"},
		{"one character wrong", "7QK4-M2XF-8DNA-0123-4567-89AB-CDEF-GHJM"},
		{"empty", ""},
		{"a prefix of the real token", "7QK4-M2XF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			var attempts []string
			h := newHarness(t, harnessOptions{attempts: &attempts})

			res := h.post("/setup", url.Values{"token": {tc.typed}})
			r.Equal(http.StatusUnauthorized, res.status)
			r.Contains(res.body, "That token is not right")
			r.Equal([]string{"rejected"}, attempts)

			// No session was minted, so the next step is still closed.
			next := h.get("/setup/database")
			r.Equal(http.StatusSeeOther, next.status)
			r.Equal("/setup", next.location)
		})
	}
}

func TestRightTokenIsAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		typed string
	}{
		{"exactly as printed", token},
		{"lower case", "7qk4-m2xf-8dna-0123-4567-89ab-cdef-ghjk"},
		{"without hyphens", "7QK4M2XF8DNA0123456789ABCDEFGHJK"},
		{"with stray spaces", "  " + token + " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			var attempts []string
			h := newHarness(t, harnessOptions{attempts: &attempts})

			res := h.post("/setup", url.Values{"token": {tc.typed}})
			r.Equal(http.StatusSeeOther, res.status)
			r.Equal("/setup/database", res.location)
			r.Equal([]string{"accepted"}, attempts)

			page := h.follow(res)
			r.Equal(http.StatusOK, page.status)
			r.Contains(page.body, "Connection string")
		})
	}
}

func TestTokenIsSingleUse(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{})
	h.unlock()

	// A second browser, with no session, offering the same token.
	res := h.browser().post("/setup", url.Values{"token": {token}})
	r.Equal(http.StatusForbidden, res.status)
	r.Contains(res.body, "has been used")
	r.Contains(res.body, "start it again")
}

func TestTokenAttemptsAreRateLimited(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	var attempts []string
	h := newHarness(t, harnessOptions{attempts: &attempts})

	var last response
	for range int(setup.TokenAttemptsBurst) + 1 {
		last = h.post("/setup", url.Values{"token": {"WRONG"}})
	}
	r.Equal(http.StatusTooManyRequests, last.status)
	r.Contains(last.body, "Too many attempts")
	r.Contains(attempts, "rate_limited")

	// Even the right token has to wait: the limit is on the endpoint, not on
	// whether the guess was good.
	good := h.post("/setup", url.Values{"token": {token}})
	r.Equal(http.StatusTooManyRequests, good.status)
}

func TestFormsNeedTheirCSRFToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		values url.Values
	}{
		{"no token at all", url.Values{"database_url": {"postgres://x/y"}}},
		{"a token from somewhere else", url.Values{"database_url": {"postgres://x/y"}, "csrf": {"nope"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{})
			h.unlock()
			res := h.post("/setup/database", tc.values)
			r.Equal(http.StatusForbidden, res.status)
			r.Contains(res.body, "stale")
		})
	}
}

func TestBadConnectionStringReportsTheReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "empty",
			url:  "",
			want: "Enter a connection string",
		},
		{
			name: "not a connection string at all",
			url:  "this is not a url",
			want: "not a PostgreSQL connection string",
		},
		{
			name: "a host name that does not resolve",
			url:  "postgres://pacenote:secret@no-such-host.invalid:5432/pacenote",
			want: "does not resolve",
		},
		{
			name: "nothing listening on that port",
			url:  "postgres://pacenote:secret@127.0.0.1:1/pacenote?connect_timeout=2",
			want: "Nothing answered at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{})
			page := h.unlock()

			res := h.submit("/setup/database", page, url.Values{"database_url": {tc.url}})
			r.Equal(http.StatusUnprocessableEntity, res.status)
			r.Contains(res.body, tc.want)
			// The connection string is put back in the field so the operator
			// can fix a typo, but the diagnosis itself must never quote the
			// password: that text is what ends up in a screenshot on a forum.
			r.NotContains(notice(t, res.body), "secret")
		})
	}
}

// notice extracts the error banner from a rendered page.
func notice(t *testing.T, body string) string {
	t.Helper()
	const open = `<div class="notice bad">`
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</div>")
	require.GreaterOrEqual(t, j, 0)
	return rest[:j]
}

func TestStepsCannotBeSkipped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"the organisation", "/setup/organisation"},
		{"the account", "/setup/account"},
		{"the address", "/setup/address"},
		{"the last step", "/setup/finish"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{})
			h.unlock()
			res := h.get(tc.path)
			r.Equal(http.StatusSeeOther, res.status)
			r.Equal("/setup/database", res.location, "a session is sent back to where it actually is")
		})
	}
}

func TestOrganisationValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "Enter the name drivers should see"},
		{"only spaces", "   ", "Enter the name drivers should see"},
		{"far too long", string(make([]rune, 200)), "longer than 120"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})
			h.unlock()
			page := h.walkTo(stepOrganisation)
			res := h.submit("/setup/organisation", page, url.Values{"organisation": {tc.in}})
			r.Equal(http.StatusUnprocessableEntity, res.status)
			r.Contains(res.body, tc.want)
		})
	}
}

func TestAccountValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		email    string
		password string
		again    string
		want     string
	}{
		{"not an email address", "ana", "a long enough password", "a long enough password", "not an email address"},
		{"a short password", "ana@example.com", "brief42", "brief42", "use at least"},
		{"two different passwords", "ana@example.com", "a long enough password", "a different one entirely", "not the same"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})
			h.unlock()
			page := h.walkTo(stepAccount)
			res := h.submit("/setup/account", page, url.Values{
				"email": {tc.email}, "password": {tc.password}, "password2": {tc.again},
			})
			r.Equal(http.StatusUnprocessableEntity, res.status)
			r.Contains(res.body, tc.want)
			r.NotContains(res.body, tc.password, "a password is never echoed back into the page")
		})
	}
}

func TestAddressValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		host string
		mode string
		want string
	}{
		{"an IP address", "203.0.113.10", "proxy", "host name rather than an IP"},
		{"a scheme", "https://pacenote.example.com", "proxy", "Leave the scheme off"},
		{"a port", "pacenote.example.com:8443", "proxy", "Leave the port off"},
		{"no choice of how it is reached", "pacenote.example.com", "", "Choose how this server is reached"},
		{"a certificate for localhost", "localhost", "auto", "cannot be issued for localhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})
			h.unlock()
			page := h.walkTo(stepAddress)
			res := h.submit("/setup/address", page, url.Values{
				"host": {tc.host}, "tls_mode": {tc.mode},
			})
			r.Equal(http.StatusUnprocessableEntity, res.status)
			r.Contains(res.body, tc.want)
		})
	}
}

func TestTheConnectionStringFromTheEnvironmentIsOffered(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{databaseURL: "postgres://pacenote@db.example.com:5432/pacenote"})
	page := h.unlock()
	r.Contains(page.body, "postgres://pacenote@db.example.com:5432/pacenote",
		"an operator who set the variable should not have to type it again")
}

func TestNothingIsWrittenBeforeTheLastStep(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})
	h.unlock()
	h.walkTo(stepFinish)

	_, err := os.Stat(filepath.Join(h.dataDir, "config.json"))
	r.ErrorIs(err, os.ErrNotExist)
	r.False(h.switched(), "the server must not switch modes until setup is committed")
}

// The wizard's own front door, for somebody who is already inside it.
//
// A half-finished wizard is a session holding an administrator's password, so
// coming back to the token page must not start a second one — it takes the
// operator back to the step they had reached.
func TestComingBackToTheTokenPageResumes(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})

	h.unlock()
	res := h.get("/setup")
	r.Equal(http.StatusSeeOther, res.status, "the token page started a second wizard")
	r.Equal("/setup/database", res.location)
}

// A step reached out of order sends the operator back to the one they are on.
// Each step reads what the one before it wrote, so arriving at the end first
// would be a confirmation page confirming nothing.
func TestAStepReachedOutOfOrderSendsTheOperatorBack(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})

	page := h.unlock()
	res := h.submit("/setup/address", page, url.Values{"host": {"pacenote.example.com"}})
	r.Equal(http.StatusSeeOther, res.status)
	r.Equal("/setup/database", res.location, "a skipped step was accepted")
}

// The last page has nothing on it until there is something to say. Reaching it
// early is a redirect rather than a page describing a server that does not
// exist yet.
func TestTheLastPageIsOnlyThereAtTheEnd(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})

	// With no session at all.
	res := h.get("/setup/done")
	r.Equal(http.StatusSeeOther, res.status)
	r.Equal("/setup", res.location)

	// And with one that has not finished.
	h.unlock()
	res = h.get("/setup/done")
	r.Equal(http.StatusSeeOther, res.status)
	r.Equal("/setup", res.location)
}

// Forms this server cannot read at all. Every one of them answers rather than
// letting a parse failure out as something else, because the wizard is the one
// part of this server with no signed-in operator behind it.
func TestTheWizardRefusesFormsItCannotRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})
	h.unlock()

	for _, path := range []string{"/setup", "/setup/database", "/setup/organisation", "/setup/account"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			h.server.URL+path, strings.NewReader("%zz&x=1"))
		r.NoError(err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := h.do(req)
		r.Equal(http.StatusBadRequest, res.status, "%s read a body that is not a form", path)
	}
}

// A step posted by somebody with no session at all lands back at the beginning.
// It is the tab left open across a restart: the wizard keeps its sessions in
// memory, so the session is genuinely gone and there is nothing to resume.
func TestAStepPostedWithNoSession(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t, harnessOptions{probe: acceptAnyDatabase})

	page := h.unlock()
	fresh := h.browser()
	res := fresh.post("/setup/organisation", url.Values{
		"csrf": {h.csrf(page.body)}, "organisation": {"Iberian GT Championship"},
	})
	r.Equal(http.StatusSeeOther, res.status, "a form with no session was acted on")
	r.Equal("/setup", res.location)
}
