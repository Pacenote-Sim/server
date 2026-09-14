package pluginweb_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/driverauth"
	"github.com/pacenote-sim/server/internal/logging"
	"github.com/pacenote-sim/server/internal/plugins"
	"github.com/pacenote-sim/server/internal/pluginweb"
)

// The boundary between a plugin and the server it is mounted on.
//
// These tests are about what a plugin is shown and what it is allowed to say.
// A plugin is somebody else's code with a URL on a league's own domain, next to
// the operator's session cookie, so most of what is asserted here is a refusal:
// the header that does not cross, the cookie that is not shown, the status that
// is not written.

// fake is a plugin that answers whatever a test wants and records what it was
// given.
type fake struct {
	access plugin.Access
	// routes overrides access with a real table, for the tests that are about
	// which address decides what.
	routes *plugin.HTTPCapability
	route  bool
	res    plugin.HTTPResponse
	err    error
	// got is the last request it was handed.
	got plugin.HTTPRequest
	// block, when set, holds every call open until it is closed. entered is
	// signalled once per call that has reached the plugin, so a test can know
	// the slots are actually taken rather than guessing at a sleep.
	block   chan struct{}
	entered chan struct{}
}

func (f *fake) Access(_, path string) (plugin.Access, bool) {
	if !f.route {
		return "", false
	}
	if f.routes != nil {
		return f.routes.For(path)
	}
	return f.access, true
}

func (f *fake) Serve(_ context.Context, _ string, r plugin.HTTPRequest) (plugin.HTTPResponse, error) {
	if f.block != nil {
		f.entered <- struct{}{}
		<-f.block
		return f.res, f.err
	}
	f.got = r
	return f.res, f.err
}

// someSettings is an organisation with a public address, for the links a plugin
// builds against it.
func someSettings(context.Context) (config.Settings, error) {
	return config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy), nil
}

// mounted is the router over one plugin called "results".
func mounted(t *testing.T, f *fake, who pluginweb.Caller) http.Handler {
	t.Helper()
	r := require.New(t)

	rt, err := pluginweb.New(pluginweb.Deps{
		Log:      logging.Discard(),
		Plugins:  f,
		Settings: someSettings,
		Who:      func(*http.Request) pluginweb.Caller { return who },
	})
	r.NoError(err)

	mux := http.NewServeMux()
	rt.Routes(mux)
	return mux
}

// call makes one request of the mounted router.
func call(t *testing.T, h http.Handler, method, target, body string, headers http.Header) *http.Response {
	t.Helper()
	var rdr io.Reader = http.NoBody
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	for k, values := range headers {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestAPluginServesItsOwnPage(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{
		route: true, access: plugin.AccessPublic,
		res: plugin.HTML(http.StatusOK, "<h1>Standings</h1>"),
	}
	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet,
		"/plugin/results/standings/gt3?season=2026", "", nil)
	defer func() { _ = res.Body.Close() }()

	r.Equal(http.StatusOK, res.StatusCode)
	body, _ := io.ReadAll(res.Body)
	r.Contains(string(body), "Standings")
	r.Equal("text/html; charset=utf-8", res.Header.Get("Content-Type"))

	// What the plugin was handed: its own path, its own prefix, and the address
	// it can build links against.
	r.Equal("/standings/gt3", f.got.Path, "the plugin was given the wrong path")
	r.Equal("season=2026", f.got.Query)
	r.Equal("/plugin/results", f.got.Prefix)
	r.Equal("https://pacenote.example.com", f.got.BaseURL)
	r.Equal("https://pacenote.example.com/plugin/results/standings", f.got.URL("standings"))

	// And the root of a plugin's mount is a path, not an empty string.
	root := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results", "", nil)
	_ = root.Body.Close()
	r.Equal("/", f.got.Path)
}

// The rule the whole package exists for: the operator's session cookie does not
// reach a plugin, and one plugin's cookies do not reach another.
func TestAPluginNeverSeesTheServersCookies(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.Text(http.StatusOK, "ok")}
	headers := http.Header{"Cookie": {strings.Join([]string{
		"pacenote_admin=the-operators-session",
		"pacenote_admin_csrf=the-operators-token",
		"pacenote_p_results_seen=1",
		"pacenote_p_other-plugin_secret=not-yours",
		"something_else=whatever",
	}, "; ")}}

	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results/", "", headers)
	_ = res.Body.Close()

	got := f.got.Header.Get("Cookie")
	r.NotContains(got, "the-operators-session", "a plugin was handed the operator's admin session")
	r.NotContains(got, "the-operators-token")
	r.NotContains(got, "not-yours", "a plugin was handed another plugin's cookie")
	r.NotContains(got, "something_else")
	r.Contains(got, "pacenote_p_results_seen=1", "a plugin was not given its own cookie")
}

// And the same rule on the way out.
func TestAPluginMaySetOnlyItsOwnCookies(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.HTTPResponse{
		Status: http.StatusOK,
		Header: http.Header{"Set-Cookie": {
			"pacenote_p_results_seen=1; Path=/",
			"pacenote_admin=i-am-the-operator-now; Path=/",
			"pacenote_p_other-plugin_x=1; Path=/",
			"session=anything; Path=/",
		}},
	}}
	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results/", "", nil)
	_ = res.Body.Close()

	set := res.Header.Values("Set-Cookie")
	r.Len(set, 1, "the server wrote a cookie a plugin may not set: %v", set)
	r.Contains(set[0], "pacenote_p_results_seen=1")
}

// Headers that describe the connection, and headers that are the server's own
// policy, are not a plugin's to set.
func TestThereAreHeadersAPluginMayNotSet(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.HTTPResponse{
		Status: http.StatusOK,
		Header: http.Header{
			"Content-Type":              {"text/html; charset=utf-8"},
			"X-Content-Type-Options":    {"nosniff-but-actually-sniff"},
			"Strict-Transport-Security": {"max-age=0"},
			"Transfer-Encoding":         {"chunked"},
			"Connection":                {"upgrade"},
			"X-Something-Of-Its-Own":    {"kept"},
		},
	}}
	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results/", "", nil)
	_ = res.Body.Close()

	r.Equal("nosniff", res.Header.Get("X-Content-Type-Options"),
		"a plugin turned off the server's own content-type protection")
	r.Empty(res.Header.Get("Strict-Transport-Security"))
	r.Empty(res.Header.Get("Transfer-Encoding"))
	r.Empty(res.Header.Get("Connection"))
	r.Equal("kept", res.Header.Get("X-Something-Of-Its-Own"), "a plugin's own header was dropped")
	r.Equal("text/html; charset=utf-8", res.Header.Get("Content-Type"))
}

// Who the caller is comes from the server and never from the request. A browser
// that sends a header naming a driver is a browser sending a header.
func TestACallerCannotNameThemselves(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.Text(http.StatusOK, "ok")}
	who := pluginweb.Caller{DriverSlug: "ana-ruiz", DriverName: "Ana Ruiz"}
	headers := http.Header{
		"X-Forwarded-For": {"10.0.0.1"},
		"X-Real-Ip":       {"10.0.0.1"},
	}

	res := call(t, mounted(t, f, who), http.MethodGet, "/plugin/results/", "", headers)
	_ = res.Body.Close()

	r.Equal("ana-ruiz", f.got.Caller.DriverSlug)
	r.Equal("Ana Ruiz", f.got.Caller.DriverName)
	r.True(f.got.Caller.SignedIn())

	// The proxy headers the server resolves for itself are not forwarded, so a
	// plugin cannot make its own answer out of something a client can set.
	r.Empty(f.got.Header.Get("X-Forwarded-For"))
	r.Empty(f.got.Header.Get("X-Real-Ip"))
	r.NotEmpty(f.got.Caller.Remote, "the plugin was told nothing about where the request came from")
}

func TestWhoMayReachAPlugin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		access plugin.Access
		who    pluginweb.Caller
		want   int
	}{
		{name: "public, nobody signed in", access: plugin.AccessPublic, want: http.StatusOK},
		{
			name:   "custom, nobody signed in — the plugin decides",
			access: plugin.AccessCustom, want: http.StatusOK,
		},
		{
			name: "admin, an operator", access: plugin.AccessAdmin,
			who: pluginweb.Caller{AdminEmail: "ana@example.com"}, want: http.StatusOK,
		},
		{
			// Not-found rather than unauthorised. A plugin an operator
			// installed for themselves is not something a stranger gets to
			// confirm the existence of.
			name: "admin, a stranger", access: plugin.AccessAdmin, want: http.StatusNotFound,
		},
		{
			name: "driver, a signed-in driver", access: plugin.AccessDriver,
			who: pluginweb.Caller{DriverSlug: "ana-ruiz"}, want: http.StatusOK,
		},
		{name: "driver, nobody", access: plugin.AccessDriver, want: http.StatusUnauthorized},
		{
			name:   "an access this server does not understand",
			access: plugin.Access("whoever"), want: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fake{route: true, access: tc.access, res: plugin.Text(http.StatusOK, "ok")}
			res := call(t, mounted(t, f, tc.who), http.MethodGet, "/plugin/results/", "", nil)
			_ = res.Body.Close()
			require.Equal(t, tc.want, res.StatusCode)
		})
	}
}

func TestWhatIsNotAPluginRoute(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A plugin that is installed and did not ask for a route.
	noRoute := &fake{route: false}
	res := call(t, mounted(t, noRoute, pluginweb.Caller{}), http.MethodGet, "/plugin/engineer/", "", nil)
	_ = res.Body.Close()
	r.Equal(http.StatusNotFound, res.StatusCode)

	// A name that is not one. It is refused before anything is asked, so the
	// address bar cannot be used to probe what is installed.
	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.Text(http.StatusOK, "ok")}
	for _, bad := range []string{"Results", "results!", "-results", strings.Repeat("a", 80)} {
		res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/"+bad+"/", "", nil)
		_ = res.Body.Close()
		r.Equal(http.StatusNotFound, res.StatusCode, "%q was accepted as a plugin name", bad)
	}
}

func TestWhatAPluginAnswersWithThatTheServerWillNotWrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  plugin.HTTPResponse
		err  error
		want int
	}{
		{
			name: "a status out of range",
			res:  plugin.HTTPResponse{Status: 42, Body: []byte("ok")}, want: http.StatusBadGateway,
		},
		{
			name: "more body than the server passes on",
			res: plugin.HTTPResponse{
				Status: http.StatusOK,
				Body:   make([]byte, plugins.MaxServeResponseBytes+1),
			},
			want: http.StatusBadGateway,
		},
		{name: "a plugin that is not running", err: plugins.ErrUnavailable, want: http.StatusServiceUnavailable},
		{name: "a plugin that is not installed", err: plugins.ErrNoPlugin, want: http.StatusNotFound},
		{name: "a plugin that broke", err: plugins.ErrOverCap, want: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fake{route: true, access: plugin.AccessPublic, res: tc.res, err: tc.err}
			res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results/", "", nil)
			_ = res.Body.Close()
			require.Equal(t, tc.want, res.StatusCode)
		})
	}
}

// A request body larger than the server hands over is refused rather than
// truncated, because half a form is worse than a refusal.
func TestARequestBodyLargerThanTheServerPassesOn(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.Text(http.StatusOK, "ok")}
	big := strings.Repeat("x", int(plugins.MaxServeRequestBytes)+1)
	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodPost, "/plugin/results/", big, nil)
	_ = res.Body.Close()

	r.Equal(http.StatusRequestEntityTooLarge, res.StatusCode)
	r.Empty(f.got.Body, "an oversized body reached the plugin anyway")

	// And one inside the cap arrives whole.
	small := strings.Repeat("x", 1024)
	res = call(t, mounted(t, f, pluginweb.Caller{}), http.MethodPost, "/plugin/results/", small, nil)
	_ = res.Body.Close()
	r.Len(f.got.Body, 1024)
	r.Equal(http.MethodPost, f.got.Method)
}

// A server not running plugins answers the same way as one where the plugin is
// not there, because the two are the same thing to whoever asked.
func TestAServerWithNoPluginHost(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	rt, err := pluginweb.New(pluginweb.Deps{
		Log: logging.Discard(),
		Settings: func(context.Context) (config.Settings, error) {
			return config.Settings{}, nil
		},
	})
	r.NoError(err)
	mux := http.NewServeMux()
	rt.Routes(mux)

	res := call(t, mux, http.MethodGet, "/plugin/results/", "", nil)
	_ = res.Body.Close()
	r.Equal(http.StatusNotFound, res.StatusCode)
}

// A plugin signing a driver in.
//
// This is the whole point of the arrangement: the plugin decides who somebody
// is by whatever means the operator chose, and the server turns that into a
// session. The plugin says the name; it never sees a token and cannot make one.
func TestAPluginSignsADriverIn(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var signedIn, signedInBy string
	f := &fake{route: true, access: plugin.AccessCustom, res: plugin.HTTPResponse{
		Status: http.StatusSeeOther,
		Header: http.Header{"Location": {"/plugin/results/"}},
		SignIn: "ana-ruiz",
	}}
	rt, err := pluginweb.New(pluginweb.Deps{
		Log:      logging.Discard(),
		Plugins:  f,
		Settings: someSettings,
		SignIn: func(_ http.ResponseWriter, _ *http.Request, slug, by string) error {
			signedIn, signedInBy = slug, by
			return nil
		},
	})
	r.NoError(err)
	mux := http.NewServeMux()
	rt.Routes(mux)

	res := call(t, mux, http.MethodPost, "/plugin/results/callback?code=abc", "", nil)
	_ = res.Body.Close()

	r.Equal(http.StatusSeeOther, res.StatusCode, "the plugin's own answer was not written")
	r.Equal("/plugin/results/", res.Header.Get("Location"))
	r.Equal("ana-ruiz", signedIn)
	r.Equal("results", signedInBy, "the server did not record which plugin vouched for them")
}

// And signing out, which a plugin asks for the same way.
func TestAPluginSignsADriverOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var out int
	f := &fake{route: true, access: plugin.AccessDriver, res: plugin.HTTPResponse{
		Status: http.StatusOK, Body: []byte("bye"), SignOut: true,
	}}
	rt, err := pluginweb.New(pluginweb.Deps{
		Log:      logging.Discard(),
		Plugins:  f,
		Settings: someSettings,
		Who:      func(*http.Request) pluginweb.Caller { return pluginweb.Caller{DriverSlug: "ana-ruiz"} },
		SignOut:  func(http.ResponseWriter, *http.Request) error { out++; return nil },
	})
	r.NoError(err)
	mux := http.NewServeMux()
	rt.Routes(mux)

	res := call(t, mux, http.MethodPost, "/plugin/results/signout", "", nil)
	_ = res.Body.Close()
	r.Equal(http.StatusOK, res.StatusCode)
	r.Equal(1, out)
}

// A plugin that asks for something the server will not do gets an error rather
// than the page it meant to serve.
//
// The page is the dangerous part. A sign-in page that renders "you are signed
// in" while nothing was signed in sends somebody away believing a thing that is
// not true, and the next page they open will not know them.
func TestASignInTheServerWillNotDo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		deps func(*pluginweb.Deps)
		res  plugin.HTTPResponse
	}{
		{
			name: "a driver this server does not have",
			deps: func(d *pluginweb.Deps) {
				d.SignIn = func(http.ResponseWriter, *http.Request, string, string) error {
					return driverauth.ErrNoDriver
				}
			},
			res: plugin.HTTPResponse{Status: http.StatusOK, Body: []byte("welcome"), SignIn: "nobody"},
		},
		{
			name: "a server that cannot sign drivers in at all",
			deps: func(*pluginweb.Deps) {},
			res:  plugin.HTTPResponse{Status: http.StatusOK, Body: []byte("welcome"), SignIn: "ana-ruiz"},
		},
		{
			name: "a server that cannot sign them out",
			deps: func(*pluginweb.Deps) {},
			res:  plugin.HTTPResponse{Status: http.StatusOK, Body: []byte("bye"), SignOut: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			f := &fake{route: true, access: plugin.AccessCustom, res: tc.res}
			deps := pluginweb.Deps{Log: logging.Discard(), Plugins: f, Settings: someSettings}
			tc.deps(&deps)
			rt, err := pluginweb.New(deps)
			r.NoError(err)
			mux := http.NewServeMux()
			rt.Routes(mux)

			res := call(t, mux, http.MethodPost, "/plugin/results/callback", "", nil)
			defer func() { _ = res.Body.Close() }()

			r.Equal(http.StatusBadGateway, res.StatusCode)
			body, _ := io.ReadAll(res.Body)
			r.NotContains(string(body), "welcome", "the plugin's page was written after its sign-in failed")
			r.NotContains(string(body), "nobody", "the page named a driver that does not exist here")
		})
	}
}

// A router built without what it needs is an error at startup rather than a
// panic on the first request.
func TestARouterThatCannotBeBuilt(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	_, err := pluginweb.New(pluginweb.Deps{Settings: someSettings})
	r.ErrorContains(err, "nowhere to log")

	_, err = pluginweb.New(pluginweb.Deps{Log: logging.Discard()})
	r.ErrorContains(err, "no way to read the settings")

	// And one with no way to resolve a caller still works: nobody is signed in.
	rt, err := pluginweb.New(pluginweb.Deps{Log: logging.Discard(), Settings: someSettings})
	r.NoError(err)
	r.NotNil(rt)
}

// The page still renders when the settings behind it will not read. A plugin
// loses the address it builds links against and nothing else, which is better
// than a league's public leaderboard going down because a row is unreadable.
func TestAPluginIsStillServedWhenTheSettingsWillNot(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.Text(http.StatusOK, "standings")}
	rt, err := pluginweb.New(pluginweb.Deps{
		Log:     logging.Discard(),
		Plugins: f,
		Settings: func(context.Context) (config.Settings, error) {
			return config.Settings{}, errors.New("the database did not answer")
		},
	})
	r.NoError(err)
	mux := http.NewServeMux()
	rt.Routes(mux)

	res := call(t, mux, http.MethodGet, "/plugin/results/", "", nil)
	defer func() { _ = res.Body.Close() }()
	r.Equal(http.StatusOK, res.StatusCode)
	body, _ := io.ReadAll(res.Body)
	r.Contains(string(body), "standings")
}

// A plugin answering with no status at all is answering 200. It is what a Go
// handler that writes a body and no status does, and a plugin author should not
// have to know that this one is different.
func TestAPluginThatNamesNoStatus(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{
		route: true, access: plugin.AccessPublic,
		res: plugin.HTTPResponse{Body: []byte("standings")},
	}
	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results/", "", nil)
	defer func() { _ = res.Body.Close() }()

	r.Equal(http.StatusOK, res.StatusCode)
	// And no content type of its own means one that a browser will not guess
	// at, because guessing is how a page becomes a script.
	r.Equal("application/octet-stream", res.Header.Get("Content-Type"))
	r.Equal("nosniff", res.Header.Get("X-Content-Type-Options"))
}

// A plugin cannot flood the answer with headers. The cap is on both directions
// because a header list is the one part of a message that costs nothing to
// write and something to carry.
func TestThereIsALimitToHowManyHeadersCross(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	many := make(http.Header, plugins.MaxServeHeaders*2)
	for i := range plugins.MaxServeHeaders * 2 {
		many.Set("X-Plugin-"+strconv.Itoa(i), "value")
	}
	f := &fake{
		route: true, access: plugin.AccessPublic,
		res: plugin.HTTPResponse{Status: http.StatusOK, Header: many},
	}

	res := call(t, mounted(t, f, pluginweb.Caller{}), http.MethodGet, "/plugin/results/", "", many.Clone())
	_ = res.Body.Close()

	r.LessOrEqual(len(f.got.Header), plugins.MaxServeHeaders,
		"a plugin was handed more headers than the server carries")

	written := 0
	for key := range res.Header {
		if strings.HasPrefix(key, "X-Plugin-") {
			written++
		}
	}
	r.LessOrEqual(written, plugins.MaxServeHeaders,
		"a plugin wrote more headers than the server carries")
	r.Equal(http.StatusOK, res.StatusCode, "the answer was lost rather than trimmed")
}

// Signing out that fails is the same refusal as signing in that fails: the
// plugin's page is not written, because a page saying "you are signed out"
// while the session is still live is worse than a plain failure.
func TestASignOutThatFails(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessCustom, res: plugin.HTTPResponse{
		Status: http.StatusOK, Body: []byte("goodbye"), SignOut: true,
	}}
	rt, err := pluginweb.New(pluginweb.Deps{
		Log:      logging.Discard(),
		Plugins:  f,
		Settings: someSettings,
		SignOut: func(http.ResponseWriter, *http.Request) error {
			return errors.New("the database did not answer")
		},
	})
	r.NoError(err)
	mux := http.NewServeMux()
	rt.Routes(mux)

	res := call(t, mux, http.MethodPost, "/plugin/results/signout", "", nil)
	defer func() { _ = res.Body.Close() }()
	r.Equal(http.StatusBadGateway, res.StatusCode)
	body, _ := io.ReadAll(res.Body)
	r.NotContains(string(body), "goodbye")
}

// One plugin, two addresses, two answers.
//
// This is the payments shape: a webhook a payment provider has to be able to
// post to, and pages only the operator may open. Before routes were declared
// one at a time a plugin had to pick one answer for both, and the two choices
// were "the webhook is broken" and "the refund button is on the internet".
func TestOnePluginCanServeAPublicAddressAndAPrivateOne(t *testing.T) {
	t.Parallel()

	table := &plugin.HTTPCapability{Title: "Payments", Routes: []plugin.Route{
		{Path: "/webhook", Access: plugin.AccessPublic},
		{Path: "/", Access: plugin.AccessAdmin},
	}}

	cases := []struct {
		name   string
		path   string
		who    pluginweb.Caller
		want   int
		served bool
	}{
		{name: "the provider posts a webhook", path: "/webhook", want: http.StatusOK, served: true},
		{
			name: "and under it", path: "/webhook/stripe",
			want: http.StatusOK, served: true,
		},
		{
			name: "a stranger opens the pages", path: "/refunds",
			want: http.StatusNotFound,
		},
		{
			name: "the operator opens the pages", path: "/refunds",
			who:  pluginweb.Caller{AdminEmail: "ana@example.com"},
			want: http.StatusOK, served: true,
		},
		{
			// The check that a character-wise prefix would get wrong. "/webhooks"
			// is not under "/webhook", so it falls to the admin route.
			name: "an address that only looks like the webhook", path: "/webhooks",
			want: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			f := &fake{route: true, routes: table, res: plugin.Text(http.StatusOK, "ok")}
			res := call(t, mounted(t, f, tc.who), http.MethodGet, "/plugin/payments"+tc.path, "", nil)
			_ = res.Body.Close()

			r.Equal(tc.want, res.StatusCode)
			if !tc.served {
				r.Empty(f.got.Path, "a request the server refused was handed to the plugin anyway")
			}
		})
	}
}

// An address the plugin did not declare never reaches it.
//
// The declaration is a wall the server puts up rather than a promise the author
// makes: a plugin cannot serve a path it did not list, however its own code is
// written. It is what makes the manifest worth reading — somebody reviewing one
// has read the whole of what it puts on the operator's server.
func TestAnUndeclaredAddressNeverReachesThePlugin(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{
		route: true,
		routes: &plugin.HTTPCapability{Routes: []plugin.Route{
			{Path: "/webhook", Access: plugin.AccessPublic},
		}},
		res: plugin.Text(http.StatusOK, "ok"),
	}
	h := mounted(t, f, pluginweb.Caller{AdminEmail: "ana@example.com"})

	for _, path := range []string{"/", "/admin", "/secret", "/web"} {
		res := call(t, h, http.MethodGet, "/plugin/payments"+path, "", nil)
		_ = res.Body.Close()
		r.Equal(http.StatusNotFound, res.StatusCode, "%s was served", path)
		r.Empty(f.got.Path, "%s was handed to the plugin", path)
	}

	// The one it did declare works, so the refusals above are the rule and not
	// a plugin that is simply unreachable.
	res := call(t, h, http.MethodGet, "/plugin/payments/webhook", "", nil)
	_ = res.Body.Close()
	r.Equal(http.StatusOK, res.StatusCode)
	r.Equal("/webhook", f.got.Path)
}

func TestAnAddressCannotBeDressedUpAsAnother(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The shape that makes this worth testing: one public address and one
	// administrator's, under the same plugin. Access is decided by the longest
	// route covering the path, so a path that reads as "/webhook" to this
	// server and as "/admin" to the plugin would be served to anybody.
	f := &fake{
		route: true,
		routes: &plugin.HTTPCapability{Routes: []plugin.Route{
			{Path: "/webhook", Access: plugin.AccessPublic},
			{Path: "/admin", Access: plugin.AccessAdmin},
		}},
		res: plugin.Text(http.StatusOK, "ok"),
	}
	h := mounted(t, f, pluginweb.Caller{}) // nobody is signed in

	// net/http redirects the first two before they arrive. The encoded forms
	// are the ones that matter: PathValue hands back the decoded remainder, so
	// these reach the router looking like ordinary paths.
	for _, target := range []string{
		"/plugin/payments/webhook/../admin",
		"/plugin/payments/webhook/./../admin",
		"/plugin/payments/webhook/%2e%2e/admin",
		"/plugin/payments/webhook/..%2fadmin",
		"/plugin/payments/webhook%2f..%2fadmin",
		"/plugin/payments/%2e%2e/%2e%2e/etc/passwd",
		"/plugin/payments/webhook/..%2F..%2Fadmin",
	} {
		f.got = plugin.HTTPRequest{}
		res := call(t, h, http.MethodGet, target, "", nil)
		_ = res.Body.Close()
		r.NotEqual(http.StatusOK, res.StatusCode, "%s was served", target)
		r.Empty(f.got.Path, "%s was handed to the plugin", target)
	}

	// What the plugin declared still works, including under the route rather
	// than only at it — otherwise the refusals above would prove nothing.
	for path, want := range map[string]string{
		"/webhook":        "/webhook",
		"/webhook/":       "/webhook/",
		"/webhook/stripe": "/webhook/stripe",
	} {
		f.got = plugin.HTTPRequest{}
		res := call(t, h, http.MethodGet, "/plugin/payments"+path, "", nil)
		_ = res.Body.Close()
		r.Equal(http.StatusOK, res.StatusCode, "%s was refused", path)
		r.Equal(want, f.got.Path)
	}
}

func TestAPluginCannotSmuggleOneHeaderInsideAnother(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A plugin names its own response headers, so it can try to end one early
	// and start another — a Set-Cookie of its own choosing, on the league's
	// domain, beside the operator's session.
	//
	// net/http is what stops this, and only when it writes to a connection:
	// a newline in a value becomes a space, and a header whose name is not a
	// name is dropped. httptest.ResponseRecorder does none of that, so this
	// test goes over a real one. It is here to pin the assumption rather than
	// to test the standard library — this package hands third-party header
	// values straight to the writer, and that is only safe while the writer is
	// the one doing it.
	f := &fake{
		route:  true,
		access: plugin.AccessPublic,
		res: plugin.HTTPResponse{
			Status: http.StatusOK,
			Header: http.Header{
				"X-Thing":               {"fine\r\nSet-Cookie: pacenote_admin=stolen"},
				"X-Other\r\nSet-Cookie": {"pacenote_admin=stolen"},
			},
			Body: []byte("ok"),
		},
	}
	srv := httptest.NewServer(mounted(t, f, pluginweb.Caller{}))
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/plugin/payments/")
	r.NoError(err)
	t.Cleanup(func() { _ = res.Body.Close() })

	r.Empty(res.Header.Values("Set-Cookie"))
	for name, values := range res.Header {
		for _, v := range values {
			r.NotContains(v, "\n", "%s crossed with a newline in it", name)
			r.NotContains(name, "\n")
		}
	}
}

func TestHowOftenOneAddressMayReachAPlugin(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{route: true, access: plugin.AccessPublic, res: plugin.Text(http.StatusOK, "ok")}
	h := mounted(t, f, pluginweb.Caller{})

	// The burst is meant to be wide enough that a page loading its own assets
	// never sees this, so spending it takes a while on purpose.
	for i := range pluginweb.RequestBurst {
		res := call(t, h, http.MethodGet, "/plugin/payments/", "", nil)
		_ = res.Body.Close()
		r.Equal(http.StatusOK, res.StatusCode, "refused on request %d of the burst", i+1)
	}

	res := call(t, h, http.MethodGet, "/plugin/payments/", "", nil)
	_ = res.Body.Close()
	r.Equal(http.StatusTooManyRequests, res.StatusCode)
	r.NotEmpty(res.Header.Get("Retry-After"), "a refusal with no idea when to come back")

	// An address that has spent nothing still gets through, so this is a limit
	// per caller and not a server that has stopped.
	req := httptest.NewRequest(http.MethodGet, "/plugin/payments/", http.NoBody)
	req.RemoteAddr = "198.51.100.7:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	r.Equal(http.StatusOK, rec.Code)
}

// waitForever is how long a test waits before calling something stuck.
//
// It is long on purpose. These are guards against a hang, not measurements of
// anything: the test below starts MaxInFlight goroutines and waits for all of
// them to reach the plugin, and the whole suite runs its packages at once under
// the race detector. A guard tight enough to be a timing assertion is a guard
// that fails on a busy machine and says nothing true when it does.
const waitForever = 30 * time.Second

func TestHowManyCallsAPluginMayBeHoldingAtOnce(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fake{
		route:   true,
		access:  plugin.AccessPublic,
		res:     plugin.Text(http.StatusOK, "ok"),
		block:   make(chan struct{}),
		entered: make(chan struct{}, pluginweb.MaxInFlight),
	}
	h := mounted(t, f, pluginweb.Caller{})

	// Fill every slot and wait until each call is genuinely inside the plugin.
	for range pluginweb.MaxInFlight {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/plugin/payments/", http.NoBody)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for range pluginweb.MaxInFlight {
		select {
		case <-f.entered:
		case <-time.After(waitForever):
			t.Fatal("the calls never reached the plugin")
		}
	}

	// One more, from a caller who gives up rather than waiting out the grace.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/plugin/payments/", http.NoBody).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(waitForever):
		t.Fatal("a caller who gave up was still being waited on")
	}

	// And one who waits it out, and is told to come back.
	req := httptest.NewRequest(http.MethodGet, "/plugin/payments/", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	r.Equal(http.StatusServiceUnavailable, rec.Code)
	r.NotEmpty(rec.Header().Get("Retry-After"))

	// Let the held calls finish, and the slots come back.
	close(f.block)
	res := call(t, h, http.MethodGet, "/plugin/payments/", "", nil)
	_ = res.Body.Close()
	r.Equal(http.StatusOK, res.StatusCode)
}
