package pluginweb

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/plugins"
)

// What crosses the boundary in each direction.
//
// This is the whole of what a plugin can see of a request and say in an answer.
// It is a short file and it is the most security-carrying one in this package,
// so every rule below says why it is there rather than only what it does.

// hopByHop are the headers that describe one connection rather than the message
// on it. They are dropped in both directions because a plugin is not on the
// connection: the server holds it, and a plugin that could set Connection or
// Upgrade would be describing a socket it does not have.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// hiddenFromPlugin are the request headers a plugin is not shown even though
// they are not hop-by-hop. Authorization is handled beside them, in inbound: it
// is hidden when it carries this server's own device token and kept otherwise.
//
// The forwarded-for family is the operator's proxy telling this server where a
// request came from. The server resolves that once, into
// [plugin.Caller.Remote], and hides the raw headers — so a plugin reads the
// server's answer rather than making its own from something a client can set.
var hiddenFromPlugin = map[string]bool{
	"Cookie":            true, // rebuilt below, with only this plugin's own
	"Forwarded":         true,
	"X-Forwarded-For":   true,
	"X-Forwarded-Host":  true,
	"X-Forwarded-Proto": true,
	"X-Real-Ip":         true,
}

// refusedFromPlugin are the response headers the server will not take from a
// plugin, because they are the server's own to decide.
//
// Content-Length and Date are written by net/http from what is actually sent; a
// plugin setting either would be describing a message it does not control. The
// security headers are the server's policy for everything it serves, and a
// plugin relaxing them for its own pages would relax them on the operator's own
// domain, where the panel's cookies live.
var refusedFromPlugin = map[string]bool{
	"Content-Length":            true,
	"Date":                      true,
	"Strict-Transport-Security": true,
	"X-Content-Type-Options":    true,
	"X-Frame-Options":           true,
}

// inbound is the request as the plugin sees it.
func inbound(name string, h http.Header) http.Header {
	out := make(http.Header, len(h))
	for key, values := range h {
		if hopByHop[key] || hiddenFromPlugin[key] {
			continue
		}
		if key == "Authorization" && carriesDeviceToken(values) {
			continue
		}
		if len(out) >= plugins.MaxServeHeaders {
			break
		}
		out[key] = append([]string(nil), values...)
	}
	if cookies := ownCookies(name, h); cookies != "" {
		out.Set("Cookie", cookies)
	}
	return out
}

// carriesDeviceToken reports whether an Authorization header holds one of this
// server's own device tokens — the credential a client uploads with.
//
// The host has already turned it into [plugin.Caller]; forwarding it as well
// would hand a plugin a credential good against the whole API in that driver's
// name, and an operator installing a leaderboard is not agreeing to that. It is
// hidden whether or not it resolved: a revoked token is still ours. A Bearer
// that is not shaped like ours passes through, for a plugin that runs its own
// scheme on a custom route.
func carriesDeviceToken(values []string) bool {
	for _, v := range values {
		token, ok := httpx.ParseBearer(v)
		if !ok {
			continue
		}
		if _, _, err := auth.SplitDeviceToken(token); err == nil {
			return true
		}
	}
	return false
}

// ownCookies is the Cookie header rebuilt with only the cookies belonging to
// this plugin.
//
// It is the rule this package exists for. The server's admin session lives in a
// cookie on the same host, and a plugin handed the raw Cookie header would hold
// the operator's session — an operator installing a leaderboard is not agreeing
// to that. One plugin's cookies are hidden from another for the same reason at
// a smaller scale: a login plugin's cookie must not be readable by a plugin
// that merely draws a table.
func ownCookies(name string, h http.Header) string {
	prefix := plugins.CookiePrefix + name + "_"
	var kept []string
	for _, c := range (&http.Request{Header: h}).Cookies() {
		if strings.HasPrefix(c.Name, prefix) {
			kept = append(kept, c.Name+"="+c.Value)
		}
	}
	return strings.Join(kept, "; ")
}

// outbound writes what the plugin answered, minus what it may not say.
//
// It returns what was refused, so that a plugin author sees why their header
// did not arrive rather than wondering. The refusal is logged and the response
// is still written: a plugin that tried to set one header it may not is not a
// reason to fail a page a driver is waiting for.
func outbound(w http.ResponseWriter, name string, res plugin.HTTPResponse, log *slog.Logger) []string {
	var refused []string
	written := 0
	for key, values := range res.Header {
		switch {
		case hopByHop[key] || refusedFromPlugin[key]:
			refused = append(refused, key)
			continue
		case key == "Set-Cookie":
			kept, bad := ownSetCookies(name, values)
			refused = append(refused, bad...)
			values = kept
		}
		for _, v := range values {
			if written >= plugins.MaxServeHeaders {
				refused = append(refused, key)
				break
			}
			w.Header().Add(key, v)
			written++
		}
	}

	// The server's own, after the plugin's, so that nothing above can have
	// replaced them. Nosniff matters most: a plugin that gets its own content
	// type wrong should serve the wrong thing rather than something a browser
	// decides to execute on the operator's domain.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	if len(refused) > 0 && log != nil {
		log.Warn("a plugin set headers this server will not write",
			slog.String("plugin", name), slog.String("headers", strings.Join(refused, ", ")))
	}
	return refused
}

// ownSetCookies keeps the cookies a plugin may set and names the rest.
//
// A plugin may set cookies under its own prefix and nowhere else. Anything else
// — a cookie named like the server's session, a cookie scoped to a parent
// domain, a cookie for another plugin — is dropped rather than written.
func ownSetCookies(name string, values []string) (kept, refused []string) {
	prefix := plugins.CookiePrefix + name + "_"
	for _, v := range values {
		cookieName, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(strings.TrimSpace(cookieName), prefix) {
			kept = append(kept, v)
			continue
		}
		refused = append(refused, "Set-Cookie: "+strings.TrimSpace(cookieName))
	}
	return kept, refused
}
