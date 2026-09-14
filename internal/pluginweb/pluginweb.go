// Package pluginweb mounts every plugin that asked for a route.
//
// One route on the server — /plugin/<name>/... — forwards to the plugin of that
// name and writes back what it answers. There is no second listener, no port
// for an operator to open and nothing behind their proxy but this server.
//
// What this package is really for is the boundary. A plugin is somebody else's
// code running on a team's machine, and giving it a URL means deciding
// exactly what it may see of a request and what it may say in reply. Those
// decisions are here rather than spread through the host:
//
//   - The server's own cookies never reach a plugin, and one plugin's cookies
//     never reach another. A plugin sees only cookies whose names begin with
//     its own prefix, and may set only those. Without that rule, installing a
//     leaderboard would hand it the operator's admin session.
//   - Who the caller is comes from the server's sessions and never from a
//     header. A plugin is told; it is not asked, and nothing a browser can send
//     becomes an identity.
//   - Both bodies are capped, the call has a deadline, and hop-by-hop headers
//     are dropped in both directions. A plugin answers one request; it cannot
//     stream, upgrade or hold a connection.
//   - Everything a plugin serves carries nosniff, because a plugin that gets
//     its own content type wrong should serve the wrong thing rather than
//     something a browser decides to execute.
package pluginweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	stdpath "path"
	"strings"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/plugins"
)

// Prefix is where every plugin's route is mounted.
const Prefix = "/plugin/"

// Host is the part of the plugin host this package uses. It is an interface so
// that the router can be tested against a plugin that answers exactly what a
// test wants, without a subprocess.
type Host interface {
	// Access is what a plugin requires of a caller at this path, and whether
	// it serves the path at all.
	Access(name, path string) (plugin.Access, bool)
	// Serve hands one request over.
	Serve(ctx context.Context, name string, r plugin.HTTPRequest) (plugin.HTTPResponse, error)
}

// Caller is what the server knows about whoever made a request, resolved from
// the server's own sessions.
type Caller struct {
	DriverSlug string
	DriverName string
	AdminEmail string
}

// Deps is what the router needs.
type Deps struct {
	Log *slog.Logger
	// Plugins is the host. A nil one is a server not running plugins, and
	// every route under the prefix answers not-found.
	Plugins Host
	// Settings is the organisation's own settings, for the public address a
	// plugin builds links against.
	Settings func(ctx context.Context) (config.Settings, error)
	// Who resolves the caller from the server's sessions. It must never read a
	// header the caller controls.
	Who func(*http.Request) Caller
	// SignIn establishes a driver session for this browser, by the slug a
	// plugin named. It is how a plugin that authenticates drivers hands the
	// result back: the plugin says who, and this mints the session.
	//
	// Nil is a server that cannot sign drivers in, which answers a plugin that
	// asks with a refusal rather than pretending it worked.
	SignIn func(w http.ResponseWriter, r *http.Request, slug, by string) error
	// SignOut ends whatever driver session this browser has.
	SignOut func(w http.ResponseWriter, r *http.Request) error
}

// Router serves the plugin routes.
type Router struct {
	deps    Deps
	limiter *httpx.Limiter
	// inFlight is the ceiling on plugin calls in progress. See limits.go.
	inFlight chan struct{}
}

// New builds it.
func New(d Deps) (*Router, error) {
	switch {
	case d.Log == nil:
		return nil, errors.New("pluginweb: there is nowhere to log")
	case d.Settings == nil:
		return nil, errors.New("pluginweb: there is no way to read the settings")
	}
	if d.Who == nil {
		d.Who = func(*http.Request) Caller { return Caller{} }
	}
	return &Router{
		deps:     d,
		limiter:  httpx.NewLimiter(RequestsPerSecond, RequestBurst, GlobalRequestsPerSecond, GlobalRequestBurst),
		inFlight: make(chan struct{}, MaxInFlight),
	}, nil
}

// Routes mounts the one route this package has.
func (rt *Router) Routes(mux *http.ServeMux) {
	mux.HandleFunc(Prefix+"{name}/{path...}", rt.serve)
	mux.HandleFunc(Prefix+"{name}", rt.serve)
}

// serve forwards one request to a plugin.
func (rt *Router) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if rt.deps.Plugins == nil || !plugin.ValidName(name) {
		rt.notFound(w, r)
		return
	}
	// The rate before the route, so that working through addresses to find out
	// what a plugin serves costs the same as using it.
	if !rt.affordable(w, r) {
		return
	}

	// The path first, because the plugin's own declaration decides both
	// whether this address is served at all and who may reach it. An address
	// no route covers is refused here, without the plugin being asked.
	path, ok := pathOf(r)
	if !ok {
		rt.notFound(w, r)
		return
	}
	access, ok := rt.deps.Plugins.Access(name, path)
	if !ok {
		rt.notFound(w, r)
		return
	}

	who := rt.deps.Who(r)
	if !rt.allowed(w, r, access, who) {
		return
	}

	body, ok := rt.read(w, r)
	if !ok {
		return
	}

	settings, err := rt.deps.Settings(r.Context())
	if err != nil {
		rt.deps.Log.LogAttrs(r.Context(), slog.LevelError, "settings unreadable", slog.Any("error", err))
	}

	release, ok := rt.acquire(w, r)
	if !ok {
		return
	}
	res, err := rt.deps.Plugins.Serve(r.Context(), name, plugin.HTTPRequest{
		Method:  r.Method,
		Path:    path,
		Query:   r.URL.RawQuery,
		Header:  inbound(name, r.Header),
		Body:    body,
		Prefix:  Prefix + name,
		BaseURL: settings.BaseURL(),
		Caller: plugin.Caller{
			DriverSlug: who.DriverSlug,
			DriverName: who.DriverName,
			AdminEmail: who.AdminEmail,
			Remote:     httpx.ClientHost(r),
		},
	})
	release()
	if err != nil {
		rt.failed(w, r, name, err)
		return
	}
	if !rt.session(w, r, name, res) {
		return
	}
	rt.write(w, r, name, res)
}

// session acts on what the plugin asked for about this browser's identity, and
// reports whether to go on and write the answer.
//
// It happens before the answer is written because both are cookies, and a
// header written after the status is a header nobody receives. A plugin that
// asked for something this server will not do is answered with a gateway error
// rather than the page it meant to serve: a sign-in page that renders "you are
// signed in" while nothing was signed in is worse than a plain failure.
func (rt *Router) session(w http.ResponseWriter, r *http.Request, name string, res plugin.HTTPResponse) bool {
	if res.SignOut {
		if rt.deps.SignOut == nil {
			rt.cannot(w, r, name, "sign a driver out", nil)
			return false
		}
		if err := rt.deps.SignOut(w, r); err != nil {
			rt.cannot(w, r, name, "sign a driver out", err)
			return false
		}
	}
	if res.SignIn == "" {
		return true
	}
	if rt.deps.SignIn == nil {
		rt.cannot(w, r, name, "sign a driver in", nil)
		return false
	}
	if err := rt.deps.SignIn(w, r, res.SignIn, name); err != nil {
		rt.cannot(w, r, name, "sign a driver in", err)
		return false
	}
	rt.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "a plugin signed a driver in",
		slog.String("plugin", name), slog.String("driver", res.SignIn))
	return true
}

// cannot answers a plugin that asked this server to do something it would not.
// The driver's slug is in the log and not on the page: whether a given driver
// exists on this server is not something an unauthenticated caller learns by
// guessing at it.
func (rt *Router) cannot(w http.ResponseWriter, r *http.Request, name, what string, err error) {
	rt.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "a plugin asked for something this server would not do",
		slog.String("plugin", name), slog.String("asked", what), slog.Any("error", err))
	httpx.Problem(w, r, http.StatusBadGateway,
		"That part of this server could not finish what it was doing. Try again.")
}

// allowed reports whether this caller may reach this plugin, and answers them
// itself when they may not.
func (rt *Router) allowed(w http.ResponseWriter, r *http.Request, access plugin.Access, who Caller) bool {
	switch access {
	case plugin.AccessPublic, plugin.AccessCustom:
		return true
	case plugin.AccessAdmin:
		if who.AdminEmail != "" {
			return true
		}
		// Not-found rather than unauthorised. A plugin an operator installed
		// for themselves is not something a stranger should be able to confirm
		// the existence of by the shape of the refusal.
		rt.notFound(w, r)
		return false
	case plugin.AccessDriver:
		if who.DriverSlug != "" {
			return true
		}
		httpx.Problem(w, r, http.StatusUnauthorized,
			"Sign in to see this. It is a page for drivers of this team.")
		return false
	default:
		// A plugin whose declared access this server does not understand serves
		// nobody. It is the same rule as everywhere else here: the safe answer
		// and the useful answer are different ones, and this takes the safe one.
		rt.notFound(w, r)
		return false
	}
}

// read takes the request body, capped.
func (rt *Router) read(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	httpx.Limit(w, r, plugins.MaxServeRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.Problem(w, r, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("That request is larger than the %d bytes this server hands to a plugin.",
				plugins.MaxServeRequestBytes))
		return nil, false
	}
	return body, true
}

// failed answers for a plugin that would not.
func (rt *Router) failed(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case errors.Is(err, plugins.ErrNoRoute), errors.Is(err, plugins.ErrNoPlugin):
		rt.notFound(w, r)
		return
	case errors.Is(err, plugins.ErrUnavailable):
		httpx.Problem(w, r, http.StatusServiceUnavailable,
			"That part of this server is not running just now. Try again in a minute.")
	default:
		httpx.Problem(w, r, http.StatusBadGateway,
			"That part of this server did not answer. The operator can see why on the plugins page.")
	}
	// The reason is logged and not shown: it is a plugin's own words, and a
	// plugin's own words are not something to put in front of the internet.
	rt.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "a plugin did not serve a request",
		slog.String("plugin", name),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err))
}

// notFound is the answer to every request for a plugin that is not there, does
// not serve, or is not for this caller. They are one answer on purpose.
func (rt *Router) notFound(w http.ResponseWriter, r *http.Request) {
	httpx.Problem(w, r, http.StatusNotFound, "There is nothing at that address on this server.")
}

// pathOf is what follows the plugin's own prefix, always beginning with a slash,
// and whether it is an address this server will serve at all.
//
// A path that is not already in its simplest form is refused rather than
// simplified, and that refusal is load-bearing. Access is decided by the
// longest route covering the path, so "/webhook/../admin" matches the public
// "/webhook" — and then reaches a plugin whose own router resolves it to
// "/admin", which is the route that was meant to need an administrator. The
// server would have checked one address and the plugin would have served
// another.
//
// net/http redirects the literal form before it ever reaches here, which is
// exactly what made this worth testing: "%2e%2e" and "..%2f" arrive decoded,
// past that redirect, and looked like ordinary paths. Refusing rather than
// cleaning means this server and the plugin cannot disagree about which address
// a request was for, whatever the plugin normalises with.
func pathOf(r *http.Request) (string, bool) {
	rest := r.PathValue("path")
	if rest == "" {
		return "/", true
	}
	out := "/" + strings.TrimPrefix(rest, "/")

	// path.Clean removes "." and ".." segments and doubled slashes, and drops a
	// trailing slash, which is meaningful here — so it is put back before the
	// comparison rather than being treated as a difference.
	cleaned := stdpath.Clean(out)
	if strings.HasSuffix(out, "/") && cleaned != "/" {
		cleaned += "/"
	}
	if cleaned != out {
		return "", false
	}
	return out, true
}

// write is what the server is willing to say on a plugin's behalf.
//
// The status is checked before anything is written, because net/http panics on
// one out of range and a plugin is somebody else's code: a bad number must be a
// bad gateway, not a dead process. The body is capped for the same reason a
// request body is — this is a page, not a file transfer — and a plugin that
// answered with more than the cap is reported rather than truncated, because
// half a page is worse than a refusal.
func (rt *Router) write(w http.ResponseWriter, r *http.Request, name string, res plugin.HTTPResponse) {
	status := res.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 599 {
		rt.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "a plugin answered with a status this server will not write",
			slog.String("plugin", name), slog.Int("status", status))
		httpx.Problem(w, r, http.StatusBadGateway,
			"That part of this server answered with something this server will not pass on.")
		return
	}
	if len(res.Body) > plugins.MaxServeResponseBytes {
		rt.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "a plugin answered with more than this server will pass on",
			slog.String("plugin", name), slog.Int("bytes", len(res.Body)))
		httpx.Problem(w, r, http.StatusBadGateway,
			"That part of this server answered with more than this server will pass on.")
		return
	}

	outbound(w, name, res, rt.deps.Log)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(res.Body)
	}
}
