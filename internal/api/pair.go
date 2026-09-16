package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// pairBodyLimit caps the two pairing bodies. Start has none at all and poll
// carries one short string, so a kilobyte is generous; the cap matters because
// these are the only two endpoints anyone can reach without a token.
const pairBodyLimit int64 = 1 << 10

// DeviceLabelMax is how much of a client's User-Agent is kept as the label a
// driver sees against a paired machine. It is the only thing the pairing flow
// learns about a machine, and the contract sends nothing else.
const DeviceLabelMax = 80

// postPairStart opens a device-code grant.
//
// It is unauthenticated, because a pairing is how a client gets its first token
// and so cannot present one, and it is rate-limited by address for the same
// reason. The call has no request body: everything it needs, it generates.
func (a *API) postPairStart(w http.ResponseWriter, r *http.Request) {
	if a.tooOld(w, r) {
		return
	}
	if !a.allowByAddress(w, r) {
		return
	}
	if err := a.current(r.Context()); err != nil {
		a.deps.Log.LogAttrs(r.Context(), slog.LevelWarn,
			"the settings could not be read, so the pairing link may be stale", slog.Any("error", err))
	}
	settings, _, _ := a.snapshot()

	codes, err := auth.NewPairingCodes()
	if err != nil {
		a.failServer(w, r, "the pairing codes could not be minted", err)
		return
	}
	expires := a.deps.Now().Add(db.PairingTTL)
	if _, err := a.deps.Store.CreatePairing(r.Context(), codes.DeviceCodeSum, codes.UserCode, expires); err != nil {
		a.failServer(w, r, "the pairing could not be opened", err)
		return
	}

	a.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "pairing opened",
		slog.String("user_code", codes.UserCode))

	a.ok(w, wire.PairStart{
		DeviceCode:      codes.DeviceCode,
		UserCode:        codes.UserCode,
		VerificationURI: settings.BaseURL() + "/pair",
		IntervalS:       PairIntervalS,
		ExpiresInS:      int(db.PairingTTL.Seconds()),
	})
}

// postPairPoll answers whether the operator has decided yet, and hands the
// token over once when they have approved.
//
// A device code nobody knows is answered "expired" rather than "not found".
// That is deliberate: the four states are what the contract gives a client to
// branch on, and both of them mean the same thing to it — start again — while
// a 404 would have it decide the server does not implement pairing.
func (a *API) postPairPoll(w http.ResponseWriter, r *http.Request) {
	if a.tooOld(w, r) {
		return
	}
	if !a.allowByAddress(w, r) {
		return
	}

	var req wire.PairPollRequest
	if _, err := httpx.DecodeJSONFingerprint(w, r, &req, pairBodyLimit); err != nil {
		a.invalid(w, r, err)
		return
	}
	if req.DeviceCode == "" {
		a.failDetail(w, r, wire.CodeInvalid,
			"That pairing request named no device code — start the pairing again.",
			map[string]any{"field": "device_code"})
		return
	}

	pairing, err := a.deps.Store.PairingByDeviceCode(r.Context(), auth.HashToken(req.DeviceCode))
	switch {
	case errors.Is(err, db.ErrNotFound):
		a.ok(w, wire.PairPoll{Status: wire.StatusExpired})
		return
	case err != nil:
		a.failServer(w, r, "the pairing could not be read", err)
		return
	}

	if pairing.Expired(a.deps.Now()) {
		if pairing.Status == wire.StatusPending {
			if err := a.deps.Store.ExpirePairing(r.Context(), pairing.ID); err != nil {
				a.deps.Log.LogAttrs(r.Context(), slog.LevelWarn,
					"the pairing could not be marked expired", slog.Any("error", err))
			}
		}
		a.ok(w, wire.PairPoll{Status: wire.StatusExpired})
		return
	}

	switch pairing.Status {
	case wire.StatusPending:
		a.ok(w, wire.PairPoll{Status: wire.StatusPending})
	case wire.StatusDenied:
		a.ok(w, wire.PairPoll{Status: wire.StatusDenied})
	case wire.StatusExpired:
		a.ok(w, wire.PairPoll{Status: wire.StatusExpired})
	case wire.StatusApproved:
		a.issueToken(w, r, pairing)
	default:
		a.failServer(w, r, "the pairing is in a state this build does not know",
			errors.New("api: unknown pairing status "+string(pairing.Status)))
	}
}

// issueToken mints the device token, once. A pairing that has already produced
// one is answered "approved" with no token: the client that paired has its
// token, and one that has lost it pairs again rather than being handed a second
// credential for the same grant.
func (a *API) issueToken(w http.ResponseWriter, r *http.Request, pairing db.Pairing) {
	if pairing.DeviceID != nil {
		a.ok(w, wire.PairPoll{Status: wire.StatusApproved})
		return
	}
	token, err := auth.NewDeviceToken()
	if err != nil {
		a.failServer(w, r, "the device token could not be minted", err)
		return
	}
	device, err := a.deps.Store.IssueDeviceForPairing(r.Context(), pairing.ID,
		token.Sum, token.Prefix, label(r))
	switch {
	case errors.Is(err, db.ErrPairingSpent):
		// Another poll of the same grant won the race and has the token.
		a.ok(w, wire.PairPoll{Status: wire.StatusApproved})
		return
	case err != nil:
		a.failServer(w, r, "the device could not be paired", err)
		return
	}

	driver, err := a.deps.Store.DriverByID(r.Context(), device.DriverID)
	if err != nil {
		a.failServer(w, r, "the driver could not be read", err)
		return
	}

	// The token itself is never logged; the prefix is what an operator matches
	// a row against.
	a.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "device paired",
		slog.Int64("device_id", device.ID),
		slog.String("driver", driver.Slug))

	out := driverOf(driver)
	a.ok(w, wire.PairPoll{Status: wire.StatusApproved, Token: token.Plain, Driver: &out})
}

// allowByAddress rate-limits the two unauthenticated endpoints. There is no
// device to key a bucket on, so the caller's address is what there is; see
// [httpx.ClientHost] for why a forwarded header is not trusted here.
func (a *API) allowByAddress(w http.ResponseWriter, r *http.Request) bool {
	_, _, limiter := a.snapshot()
	if limiter == nil {
		return true
	}
	if allowed, retry := limiter.Allow(ClassPair, httpx.ClientHost(r)); !allowed {
		a.failRateLimited(w, r, retry)
		return false
	}
	return true
}

// label is what a driver sees against this machine in their list of paired
// devices. The User-Agent is the only thing the contract has a client send that
// describes it, and it is truncated because it is shown in a table.
func label(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > DeviceLabelMax {
		ua = ua[:DeviceLabelMax]
	}
	return ua
}
