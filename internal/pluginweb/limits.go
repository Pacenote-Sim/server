package pluginweb

import (
	"net/http"
	"strconv"
	"time"

	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/plugins"
)

// What a plugin route costs this server, and what bounds it.
//
// A route declared public is unauthenticated by design — a payment provider's
// webhook has no session — so the address is reachable by anyone who knows it.
// Everything else on this server that answers a stranger is bounded already:
// the setup token, the sign-in form, the device API. This is that same bound
// for the surface plugins open.
//
// Two different things are capped, because a plugin route can be expensive in
// two different ways:
//
//   - How often. A token bucket per address, with a burst wide enough for a
//     page that loads its own assets and a refill far below what a script
//     doing it deliberately would want.
//
//     The address is [httpx.ClientHost], which does not read X-Forwarded-For
//     because it cannot tell a proxy's header from a caller's. That is the
//     right refusal and it has a cost: behind the operator's own proxy every
//     caller keys to the proxy, and this limit becomes one limit for the whole
//     server. So it is sized for that — generous enough that a team whose
//     drivers all arrive as one address is never touched by it, which leaves
//     it a brake on a script rather than a tight quota. The cap below is what
//     actually bounds what this server will spend, and it does not depend on
//     telling callers apart.
//
//   - How many at once. Each call in progress holds a request body and an
//     answer in memory and occupies the plugin for as long as it takes to
//     answer, which may be [plugins.ServeTimeout]. Without a ceiling, enough
//     concurrent callers reserve MaxInFlight-free memory and leave the plugin
//     with nothing spare for the operator.
const (
	// RequestsPerSecond is the sustained per-address rate.
	RequestsPerSecond = 50
	// RequestBurst is what one address may spend at once — a whole team
	// opening a plugin page at the same moment, through one proxy.
	RequestBurst = 200
	// GlobalRequestsPerSecond and GlobalRequestBurst are the same ceiling over
	// every address at once, so that a limit per address is not simply a limit
	// per address an attacker is willing to rent.
	GlobalRequestsPerSecond = 500
	GlobalRequestBurst      = 1000

	// MaxInFlight is how many plugin calls may be in progress at once. At the
	// body caps in [plugins] this is the memory a plugin's routes can reserve:
	// MaxInFlight × (MaxServeRequestBytes + MaxServeResponseBytes).
	MaxInFlight = 16
	// AcquireGrace is how long a request waits for one of those slots before it
	// is turned away. It is short on purpose. Waiting absorbs a burst that is
	// merely unlucky; waiting long turns a slow plugin into a queue of held
	// connections, which is the thing the cap exists to prevent.
	AcquireGrace = 2 * time.Second
)

// MaxInFlightBytes is what [MaxInFlight] reserves at worst, for anyone sizing a
// machine.
const MaxInFlightBytes = MaxInFlight * (plugins.MaxServeRequestBytes + plugins.MaxServeResponseBytes)

// affordable reports whether this caller is within the rate, and answers them
// itself when they are not.
func (rt *Router) affordable(w http.ResponseWriter, r *http.Request) bool {
	if rt.limiter.Allow(httpx.ClientHost(r)) {
		return true
	}
	retryAfter(w, rt.limiter.Retry())
	httpx.Problem(w, r, http.StatusTooManyRequests,
		"Too many requests from this address. Wait a moment and try again.")
	return false
}

// acquire takes one of the in-flight slots, waiting up to [AcquireGrace] for
// one. The release is returned rather than deferred by the caller so that the
// slot is given back the moment the plugin has answered, and is not held for as
// long as it takes to write the answer to a slow reader.
func (rt *Router) acquire(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	select {
	case rt.inFlight <- struct{}{}:
		return func() { <-rt.inFlight }, true
	default:
	}

	timer := time.NewTimer(AcquireGrace)
	defer timer.Stop()
	select {
	case rt.inFlight <- struct{}{}:
		return func() { <-rt.inFlight }, true
	case <-r.Context().Done():
		// The caller gave up while waiting. Nothing to write.
		return nil, false
	case <-timer.C:
		retryAfter(w, AcquireGrace)
		httpx.Problem(w, r, http.StatusServiceUnavailable,
			"That part of this server is busy. Try again in a moment.")
		return nil, false
	}
}

// retryAfter writes the header, rounded up to whole seconds and never zero —
// a Retry-After of 0 reads as "immediately", which is the opposite of what
// either refusal means.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(s))
}
