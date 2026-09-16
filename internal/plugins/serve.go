package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pacenote-sim/plugin"
)

// Handing one HTTP request to a plugin.
//
// A plugin that declared a route is mounted at /plugin/<its name>/ and is given
// every request made under it. This file is the host half: it is what decides
// what a plugin is allowed to see of the request, how long it has, and what it
// is allowed to say in reply.
//
// The decisions worth stating, because none of them can be taken back once a
// plugin is installed:
//
//   - The host's own cookies never cross. A plugin holding the operator's admin
//     session would be the operator, and an operator installing a leaderboard
//     is not agreeing to that. A plugin sees only cookies it set itself.
//   - The plugin is told who the caller is; it is never asked. Every field of
//     [plugin.Caller] is the host's own word, resolved from the host's own
//     sessions, so a header a browser can set cannot become an identity.
//   - Both bodies are capped and the call has a deadline. A plugin cannot hold
//     a connection, stream, or upgrade one: it gets a request and returns an
//     answer, and the host writes it.

// The limits on one request. They are small on purpose: this is a page or an
// endpoint on a team's server, not a file transfer, and a plugin that needs
// to move more than this should be handing out a link rather than the bytes.
const (
	// MaxServeRequestBytes is the largest request body forwarded to a plugin.
	MaxServeRequestBytes int64 = 1 << 20
	// MaxServeResponseBytes is the largest answer taken back from one.
	MaxServeResponseBytes = 8 << 20
	// MaxServeHeaders is how many headers may cross in either direction.
	MaxServeHeaders = 64
	// ServeTimeout is how long a plugin has to answer one request.
	ServeTimeout = 15 * time.Second
)

// CookiePrefix is what a plugin's own cookies are called. A plugin may read and
// set cookies whose names begin with this and its own name; everything else on
// the way in is hidden from it and everything else on the way out is refused.
//
// It is a prefix rather than a list so that a plugin can have as many as it
// needs without asking, and so that the host can tell one plugin's cookies from
// another's by looking at the name.
const CookiePrefix = "pacenote_p_"

// ErrNoRoute reports that a plugin was asked to serve a request and did not ask
// for a route. It is not an error about this request: it is an operator
// following a link to a plugin that does not have pages.
var ErrNoRoute = errors.New("plugins: this plugin does not serve a route")

// Serve hands one request to a plugin and returns what to answer with.
//
// The caller has already decided that whoever made the request is allowed to
// reach this plugin. What is decided here is everything else: what the plugin
// is shown, how long it has, and whether what it said back is something the
// host is willing to write.
func (h *Host) Serve(ctx context.Context, name string, r plugin.HTTPRequest) (plugin.HTTPResponse, error) {
	inst, err := h.running(name)
	if err != nil {
		return plugin.HTTPResponse{}, err
	}
	if !inst.serves() {
		return plugin.HTTPResponse{}, fmt.Errorf("%w: %s", ErrNoRoute, name)
	}
	if err := h.checkCap(ctx, name); err != nil {
		return plugin.HTTPResponse{}, err
	}
	return inst.serve(ctx, r)
}

// Access is what this plugin requires of a caller at this path, and whether it
// serves the path at all.
//
// A path no route covers is not served. That is the rule the whole declaration
// rests on: a plugin puts on the operator's server exactly what its manifest
// lists, and an address nobody thought about is refused here without the plugin
// being asked.
func (h *Host) Access(name, path string) (plugin.Access, bool) {
	inst, err := h.running(name)
	if err != nil {
		return "", false
	}
	return inst.access(path)
}

// serve is one request, with the plugin's configuration attached and the
// host's limits applied to what comes back.
func (i *instance) serve(ctx context.Context, r plugin.HTTPRequest) (plugin.HTTPResponse, error) {
	impl, ok := i.live()
	if !ok {
		return plugin.HTTPResponse{}, fmt.Errorf("%w: %s stopped while it was serving", ErrUnavailable, i.name())
	}
	server, ok := impl.(plugin.Server)
	if !ok {
		// The manifest said it serves and the binary does not. The host refuses
		// this at install, so reaching here means a plugin was replaced under a
		// running server.
		return plugin.HTTPResponse{}, fmt.Errorf("%w: %s", ErrNoRoute, i.name())
	}

	// A page is served whether or not the plugin is finished being set up.
	//
	// Answering a request and serving a page are not the same promise. A plugin
	// that has not been configured cannot be asked for a cue — there is no key
	// to make one with — but its pages are frequently how an operator gets it
	// configured in the first place, and a webhook has to keep answering a
	// provider that does not know or care. Refusing here made both impossible:
	// every address a plugin served answered 502 until every required setting
	// was filled in, including the address that existed to fill them in.
	//
	// So the plugin is told what is set and left to decide. It already has to:
	// it receives the settings and can see what is missing.
	values, secrets, err := i.configure(ctx)
	if err != nil && !errors.Is(err, plugin.ErrNotConfigured) {
		return plugin.HTTPResponse{}, err
	}
	r.Settings = values
	r.Secrets = secrets

	callCtx, cancel := context.WithTimeout(ctx, ServeTimeout)
	defer cancel()

	res, err := server.ServeHTTP(callCtx, r)
	if err != nil {
		reason := i.scrub.cleanError(err)
		i.host.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin did not answer a request made of it",
			slog.String("plugin", i.name()),
			slog.String("method", r.Method),
			slog.String("path", r.Path),
			slog.String("reason", reason))
		return plugin.HTTPResponse{}, err
	}

	i.meter(ctx, res.Usage, nil)
	return res, nil
}
