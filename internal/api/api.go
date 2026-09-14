package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// Prefix is where version 1 is mounted. It is the value discovery publishes as
// [wire.Discovery.API], taken from one constant so the document and the routes
// cannot disagree.
const Prefix = config.APIBase

// settingsTTL is how long the organisation's settings are held before they are
// read again. GET /me has a five-millisecond budget and settings change about
// once a year, so re-reading them on every request would be spending the whole
// budget on an answer that has not changed.
const settingsTTL = 10 * time.Second

// touchInterval is how often a device's last_used_at is written. Every request
// would be a write per lap per driver for a column an operator reads once a
// week; a minute is still "when did that machine last upload" to anyone asking.
const touchInterval = time.Minute

// Deps is everything the API needs from the rest of the server.
type Deps struct {
	// Log receives the API's own lines. A token is never one of them.
	Log *slog.Logger
	// Store is the database.
	Store *db.Store
	// Keyring holds the data key that opens the operator's sealed
	// credentials. It is a keyring rather than a key because an operator can
	// regenerate the data key in the admin panel while the server runs, and a
	// key captured here would leave every sealed value unreadable until a
	// restart. A nil keyring, or one holding no key, is the language-model and
	// voice features being off.
	Keyring *auth.Keyring
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Features computes what this installation has, from what the plugins
	// answer and what the operator has configured. nil means [Features], which
	// is the community edition's answer; it is a field so that a build with
	// more in it changes one function rather than every gate.
	Features func(Answering, Keys) []wire.Feature
	// OnRequest is called when a route finishes, with the route pattern rather
	// than the path so that a metric does not grow a series per driver. nil
	// means nothing is measured.
	OnRequest func(route string, status int, took time.Duration)
	// Plugins receives the events plugins react to — a lap completed, a stint
	// finished — and says which requests anything will answer, which is what
	// the discovery document advertises. nil is a server with no plugin host,
	// which is every server built without one and every test that does not
	// care; it has no coach and says so.
	Plugins EventSink
}

// API serves version 1.
type API struct {
	deps Deps

	mu        sync.RWMutex
	settings  config.Settings
	features  []wire.Feature
	keys      Keys
	readAt    time.Time
	limiter   *Limiter
	touched   map[int64]time.Time
	touchedMu sync.Mutex

	live  *liveState
	field *fieldState
}

// New builds the API. It reads the settings once so that the first request does
// not pay for them, and it is not an error if that read fails: the settings are
// re-read on the next request that needs them.
func New(ctx context.Context, d Deps) (*API, error) {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Features == nil {
		d.Features = Features
	}
	if d.Store == nil {
		return nil, errors.New("api: there is no database to serve from")
	}
	a := &API{
		deps:    d,
		touched: make(map[int64]time.Time),
		live:    newLiveState(d.Now),
		field:   newFieldState(d.Now),
	}
	a.limiter = NewLimiter(config.DefaultLimits())
	_ = a.current(ctx)
	return a, nil
}

// Routes registers version 1 and the discovery document on a mux. It does not
// own the mux: the same server carries the admin panel.
//
// Which writes are replayed, and which are not:
//
//   - PUT /stints/{id}, POST /stints/{id}/laps and PUT /stints/{id}/summary are
//     backed by the idempotency table. They are what the client's offline queue
//     drains, so a retry has to be safe however it failed the first time.
//   - POST /field and POST /tts require the header and are not replayed. A
//     field report is a snapshot of this instant and replaying a stale one
//     would be worse than refusing; audio is cached by the client already.
//   - POST /live carries no key. The contract's own exception: it is never
//     retried, so a key on a call made once a second would be overhead for a
//     guarantee nobody uses.
//   - Pairing is unauthenticated, so there is no device to scope a key to. The
//     device code is the idempotency of that flow: polling twice is safe
//     because the grant records the machine it produced.
func (a *API) Routes(mux *http.ServeMux) {
	a.route(mux, "GET "+config.DiscoveryPath, a.getDiscovery)

	a.route(mux, "POST "+Prefix+"/pair/start", a.postPairStart)
	a.route(mux, "POST "+Prefix+"/pair/poll", a.postPairPoll)

	a.route(mux, "GET "+Prefix+"/me", a.authed(ClassRead, a.getMe))
	a.route(mux, "GET "+Prefix+"/reference", a.authed(ClassRead, a.getReference))
	a.route(mux, "PUT "+Prefix+"/stints/{id}", a.authed(ClassWrite, a.putStint))
	a.route(mux, "POST "+Prefix+"/stints/{id}/laps", a.authed(ClassWrite, a.postLaps))
	a.route(mux, "PUT "+Prefix+"/stints/{id}/summary", a.authed(ClassSummary, a.putSummary))

	a.route(mux, "POST "+Prefix+"/live", a.authed(ClassLive, a.postLive))
	a.route(mux, "POST "+Prefix+"/field", a.authed(ClassField, a.postField))
	a.route(mux, "POST "+Prefix+"/tts", a.authed(ClassTTS, a.postTTS))

	// Anything else under the prefix is a client talking to a version of this
	// API that does not exist, and it must get the envelope rather than the
	// admin panel's plain text.
	a.route(mux, Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		a.fail(w, r, wire.CodeNotFound, msgNoRoute)
	})
}

// route registers one handler and measures it. The pattern is the metric's
// label, which is why registration and measurement happen in the same call:
// the pattern is known here and nowhere else.
func (a *API) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, httpx.Measure(pattern, a.deps.OnRequest)(h))
}

// session is one authenticated request's identity: the machine, the person
// behind it, and what they may do.
type session struct {
	device   db.Device
	driver   db.Driver
	features []wire.Feature
	settings config.Settings
	keys     Keys
}

// has reports whether this driver may use the feature here. It is the two-sided
// check the contract asks for — the server has it and the driver is entitled to
// it — collapsed into the one list GET /me publishes.
func (s session) has(f wire.Feature) bool {
	for _, have := range s.features {
		if have == f {
			return true
		}
	}
	return false
}

// current re-reads the settings, features, keys and limiter if the held ones
// have aged past [settingsTTL]. A read that fails leaves the last good answer
// in place, because a database hiccup must not turn every feature off for
// everyone; the caller logs it and carries on with what it has.
func (a *API) current(ctx context.Context) error {
	now := a.deps.Now()
	a.mu.RLock()
	fresh := now.Sub(a.readAt) < settingsTTL
	a.mu.RUnlock()
	if fresh {
		return nil
	}

	loaded, err := a.deps.Store.Settings(ctx)
	if err != nil {
		return err
	}
	keys := OpenKeys(loaded, a.deps.Keyring.Key())
	features := a.deps.Features(a.answering(), keys)

	a.applyLoaded(loaded, keys, features, now)
	return nil
}

// answering is the plugin host as the feature list sees it, or nil when there
// is none. The two-step is because a nil interface holding a nil pointer is not
// a nil interface, and a server with no plugins must be the second kind.
func (a *API) answering() Answering {
	if sink, ok := a.deps.Plugins.(Answering); ok && sink != nil {
		return sink
	}
	return nil
}

// asking is the plugin host as a caller that needs an answer sees it, or nil
// when there is none. The two-step is the same one [API.answering] makes, for
// the same reason: a nil interface holding a nil pointer is not a nil interface.
func (a *API) asking() Asking {
	if sink, ok := a.deps.Plugins.(Asking); ok && sink != nil {
		return sink
	}
	return nil
}

// applyLoaded installs a freshly read set. It is separate from [API.current] so
// that the rule below can be tested without a database, because it is the rule
// the admin panel's every save depends on.
//
// The limiter is rebuilt only when the published limits actually change.
// Rebuilding it on every refresh would empty every bucket, so a client that
// sent too fast would be forgiven every time the settings aged out or an
// operator saved an unrelated form — which is to say there would be no rate
// limiting at all.
func (a *API) applyLoaded(loaded config.Settings, keys Keys, features []wire.Feature, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.limiter == nil || a.settings.Limits != loaded.Limits {
		a.limiter = NewLimiter(loaded.Limits)
	}
	a.settings, a.keys, a.features, a.readAt = loaded, keys, features, now
}

// Invalidate drops the held settings so that the next request reads them
// again. It is what makes a change in the admin panel take effect without a
// restart: the panel writes, calls this, and the following request rebuilds
// the features, the keys and the discovery document from what is now stored.
//
// It deliberately does not touch the limiter. Emptying the buckets on every
// settings change would forgive a client that was sending too fast each time
// an operator saved a form, so the limiter is rebuilt in [API.current] and
// only when the published limits themselves have changed.
func (a *API) Invalidate() {
	a.mu.Lock()
	a.readAt = time.Time{}
	a.mu.Unlock()
}

// snapshot is the settings, features, keys and limiter as one consistent set.
// Reading them one at a time could straddle a refresh and hand a handler
// features computed from one set of keys and a limiter built from another.
func (a *API) snapshot() (config.Settings, []wire.Feature, Keys, *Limiter) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings, a.features, a.keys, a.limiter
}

// authed is the middleware every route past discovery and pairing wears: the
// client version check, the bearer token, and the rate limit, in that order.
//
// The order is the point. A client that is too old is told so before its token
// is looked up, because it must stop uploading whatever else is true; and the
// rate limit is applied after authentication so that the bucket belongs to a
// device rather than to an address, which is what D-7 asks for and what a
// client behind one team's shared connection needs.
func (a *API) authed(class Class, next func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.tooOld(w, r) {
			return
		}
		if err := a.current(r.Context()); err != nil {
			a.deps.Log.LogAttrs(r.Context(), slog.LevelError,
				"the settings could not be read, so the feature list may be stale", slog.Any("error", err))
		}
		settings, features, keys, limiter := a.snapshot()

		token, ok := bearer(r)
		if !ok {
			a.fail(w, r, wire.CodeUnauthorized, msgNoToken)
			return
		}
		sum, prefix, err := auth.SplitDeviceToken(token)
		if err != nil {
			a.fail(w, r, wire.CodeUnauthorized, msgBadToken)
			return
		}
		if allowed, retry := limiter.Allow(class, prefix); !allowed {
			a.failRateLimited(w, r, retry)
			return
		}
		device, err := a.deps.Store.DeviceByToken(r.Context(), prefix, sum)
		switch {
		case errors.Is(err, db.ErrNotFound):
			a.fail(w, r, wire.CodeUnauthorized, msgBadToken)
			return
		case err != nil:
			a.failServer(w, r, "the device could not be read", err)
			return
		case device.Revoked():
			a.fail(w, r, wire.CodeUnauthorized, msgRevokedToken)
			return
		}
		driver, err := a.deps.Store.DriverByID(r.Context(), device.DriverID)
		if err != nil {
			a.failServer(w, r, "the driver could not be read", err)
			return
		}
		a.touch(r.Context(), device.ID)

		next(w, r, session{
			device:   device,
			driver:   driver,
			features: Entitled(features, driver.ID),
			settings: settings,
			keys:     keys,
		})
	}
}

// tooOld enforces MinClient against the X-Client-Version header and reports
// whether it answered the request.
//
// A request with no header is allowed through. The header is how a client says
// which version it is, and a caller that does not send one — curl, a health
// check, a client older than the header itself — has not claimed to be
// anything; refusing it would break the one tool an operator debugs with.
func (a *API) tooOld(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get("X-Client-Version")
	if got == "" || !olderThan(got, MinClient) {
		return false
	}
	a.failDetail(w, r, wire.CodeClientTooOld,
		"This client is too old for this server — update it to version "+MinClient+" or newer and it will carry on where it left off.",
		map[string]any{"min_client": MinClient, "client_version": got})
	return true
}

// touch records that a device uploaded, at most once every [touchInterval].
// It is deliberately best effort: an operator's "last seen" column is not worth
// failing a driver's lap upload over.
func (a *API) touch(ctx context.Context, deviceID int64) {
	now := a.deps.Now()
	a.touchedMu.Lock()
	last, seen := a.touched[deviceID]
	if seen && now.Sub(last) < touchInterval {
		a.touchedMu.Unlock()
		return
	}
	a.touched[deviceID] = now
	a.touchedMu.Unlock()

	if err := a.deps.Store.TouchDevice(ctx, deviceID); err != nil {
		a.deps.Log.LogAttrs(ctx, slog.LevelWarn, "the device could not be marked as used", slog.Any("error", err))
	}
}

// bearer pulls the token out of the Authorization header. The scheme is matched
// case-insensitively because RFC 7235 says it is case-insensitive, and a client
// that sends "bearer" is not wrong.
func bearer(r *http.Request) (string, bool) {
	const scheme = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(scheme) {
		return "", false
	}
	if !equalFold(h[:len(scheme)], scheme) {
		return "", false
	}
	token := trimSpace(h[len(scheme):])
	return token, token != ""
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func trimSpace(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for s != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// bodyLimit is the cap on one request body, from the settings the operator
// published. D-3 puts a cap on every route and this is where each route's comes
// from: the number in the discovery document the client read.
func (a *API) bodyLimit(settings config.Settings) int64 {
	if settings.Limits.MaxBodyBytes > 0 {
		return int64(settings.Limits.MaxBodyBytes)
	}
	return int64(config.DefaultLimits().MaxBodyBytes)
}

// ok writes a success body.
func (a *API) ok(w http.ResponseWriter, body any) { httpx.JSON(w, http.StatusOK, body) }
