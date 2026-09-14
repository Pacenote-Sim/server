// Package driverauth holds a driver's session.
//
// A driver has never had a way to sign in to this server. Their *machine* holds
// a token, issued when the operator approved its pairing, and that token is the
// client's rather than the person's. This is the other thing: the person, in a
// browser, reading something about themselves.
//
// This server does not authenticate them and has no way to. There is no driver
// password here, no email flow and no identity provider — a plugin does that,
// by whatever means the operator chose, and then tells this server who the
// driver is. This package is the half that is left: minting the session,
// reading it back, and ending it.
//
// It lives in the core rather than in that plugin because of a rule in
// pluginweb: a plugin's cookies are namespaced to that plugin, so one plugin
// cannot read another's. A login plugin holding the session privately would be
// the only thing that knew who was signed in — not this server, and not the
// plugin next to it. An identity two things have to agree on has to live where
// both can see it.
package driverauth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
)

// SessionCookie carries the driver's session. The value is random and only its
// SHA-256 reaches the database, so a dump of that table cannot be used to sign
// in as anybody.
//
// It is named apart from the panel's on purpose: a driver signing in must never
// touch the operator's session, and two cookies with one name would be one
// mistake away from doing exactly that.
const SessionCookie = "pacenote_driver"

// SessionTTL is how long a driver stays signed in without coming back. It is
// longer than the panel's because the consequences are smaller — a driver's
// session reads their own laps, where an operator's runs the team — and
// because a driver who has to sign in every evening will stop bothering.
const SessionTTL = 30 * 24 * time.Hour

// touchAfter is how stale a session has to be before using it writes to the
// database. Every request would otherwise be a write, for a column nobody reads
// to the minute.
const touchAfter = time.Hour

// ErrNoDriver reports that a plugin named a driver this server does not have.
// It is the plugin's mistake and it is refused rather than guessed at: creating
// a driver because somebody signed in would let a login plugin fill the roster.
var ErrNoDriver = errors.New("driverauth: no driver of that name is on this server")

// Sessions mints and reads driver sessions.
type Sessions struct {
	store *db.Store
	now   func() time.Time
}

// New builds the session store.
func New(store *db.Store, now func() time.Time) *Sessions {
	if now == nil {
		now = time.Now
	}
	return &Sessions{store: store, now: now}
}

// Session is a signed-in driver, as a caller reads it.
type Session struct {
	DriverID   int64
	Slug       string
	Name       string
	SignedInBy string
}

// Start signs a driver in and sets the cookie.
//
// The slug is the plugin's word for who this is, and it is checked against the
// roster rather than trusted: a plugin naming a driver who is not here is a
// plugin that has got it wrong, and the answer is a refusal rather than a new
// driver nobody added.
func (s *Sessions) Start(ctx context.Context, w http.ResponseWriter, r *http.Request,
	slug, by string,
) (Session, error) {
	driver, err := s.store.DriverBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return Session{}, ErrNoDriver
		}
		return Session{}, err
	}
	plain, sum, err := auth.NewSessionToken()
	if err != nil {
		return Session{}, err
	}
	expires := s.now().Add(SessionTTL)
	if err := s.store.CreateDriverSession(ctx, driver.ID, sum, expires, userAgent(r), by); err != nil {
		return Session{}, err
	}
	setCookie(w, r, plain)
	return Session{DriverID: driver.ID, Slug: driver.Slug, Name: driver.Name, SignedInBy: by}, nil
}

// Current is the driver this request is signed in as, if any. A session that
// has run out is simply absent: the lookup enforces expiry, so there is no
// window in which an old cookie works.
func (s *Sessions) Current(ctx context.Context, r *http.Request) (Session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return Session{}, false
	}
	sum := auth.HashToken(c.Value)
	sess, err := s.store.DriverSessionByToken(ctx, sum)
	if err != nil {
		return Session{}, false
	}
	// Extended only when it is worth a write. A driver reading a page of their
	// own laps should not cost a row update per image on it.
	if s.now().Sub(sess.LastSeenAt) > touchAfter {
		_ = s.store.TouchDriverSession(ctx, sum, s.now().Add(SessionTTL))
	}
	return Session{
		DriverID:   sess.DriverID,
		Slug:       sess.Slug,
		Name:       sess.Name,
		SignedInBy: sess.SignedInBy,
	}, true
}

// End signs this browser out, in the database as well as in the browser. A
// cookie cleared and a row left behind would be a session that still works for
// anybody who kept the value.
func (s *Sessions) End(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var err error
	if c, cookieErr := r.Cookie(SessionCookie); cookieErr == nil && c.Value != "" {
		err = s.store.DeleteDriverSession(ctx, auth.HashToken(c.Value))
	}
	clearCookie(w)
	return err
}

// Sweep clears out the sessions that have run out. Nothing depends on it: the
// lookup refuses an expired session either way, so a server whose sweep never
// runs is correct and merely larger than it needs to be.
func (s *Sessions) Sweep(ctx context.Context) (int64, error) {
	return s.store.DeleteExpiredDriverSessions(ctx)
}

// setCookie writes the driver's session cookie. Secure is conditional on the
// request having arrived over TLS, for the same reason the panel's is: a server
// behind the operator's own proxy is commonly reached over plain HTTP on its
// port, and a Secure cookie there is dropped by the browser.
//
//nolint:gosec // G124: Secure is conditional by design; HttpOnly and SameSite are always set.
func setCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

func clearCookie(w http.ResponseWriter) {
	//nolint:gosec // G124: this deletes a cookie; there is no value to protect.
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
}

// userAgent is what the browser said it was, cut to what a table cell holds.
func userAgent(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	return ua
}
