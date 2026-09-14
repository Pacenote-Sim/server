package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
)

// SessionCookie carries the admin session. The value is random; only its
// SHA-256 reaches the database, so a dump cannot be used to sign in.
const SessionCookie = "pacenote_admin"

// CSRFCookie is the other half of the double-submit check on every form that
// changes something.
const CSRFCookie = "pacenote_admin_csrf"

// SessionTTL is how long a session lasts without use. Each request that finds
// one extends it, so an operator working in the panel is not signed out
// underneath them and one who walked away is.
const SessionTTL = 12 * time.Hour

// Sessions stores admin sessions in the database rather than in this process,
// so that signing out actually ends a session and a restart does not sign
// everyone out.
type Sessions struct {
	store *db.Store
	now   func() time.Time
}

// NewSessions builds the session store.
func NewSessions(store *db.Store, now func() time.Time) *Sessions {
	if now == nil {
		now = time.Now
	}
	return &Sessions{store: store, now: now}
}

// Start creates a session for an administrator and sets both cookies.
func (s *Sessions) Start(ctx context.Context, w http.ResponseWriter, r *http.Request, adminID int64) (string, error) {
	plain, sum, err := auth.NewSessionToken()
	if err != nil {
		return "", err
	}
	expires := s.now().Add(SessionTTL)
	if storeErr := s.store.CreateAdminSession(ctx, adminID, sum, expires, userAgent(r)); storeErr != nil {
		return "", storeErr
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		return "", err
	}
	setCookie(w, r, SessionCookie, plain, SessionTTL)
	setCookie(w, r, CSRFCookie, csrf, SessionTTL)
	return csrf, nil
}

// Current returns the session this request carries, and extends it. A session
// that has expired is simply absent: expiry is enforced by the lookup, so there
// is no window in which an old cookie works.
func (s *Sessions) Current(ctx context.Context, r *http.Request) (db.AdminSession, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return db.AdminSession{}, false
	}
	sum := auth.HashToken(c.Value)
	sess, err := s.store.AdminSessionByToken(ctx, sum)
	if err != nil {
		return db.AdminSession{}, false
	}
	// Extend at most once a minute: a page with several requests on it should
	// not write to the database several times.
	if s.now().Sub(sess.LastSeenAt) > time.Minute {
		_ = s.store.TouchAdminSession(ctx, sum, s.now().Add(SessionTTL))
	}
	return sess, true
}

// End signs this browser out and removes the row, so the cookie is dead
// everywhere rather than only in this browser.
func (s *Sessions) End(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	c, err := r.Cookie(SessionCookie)
	if err == nil && c.Value != "" {
		if err := s.store.DeleteAdminSession(ctx, auth.HashToken(c.Value)); err != nil {
			return err
		}
	}
	clearCookie(w, SessionCookie)
	clearCookie(w, CSRFCookie)
	return nil
}

// EnsureCSRF returns this browser's CSRF token, minting one if it has none. The
// sign-in form needs it before there is a session, which is why it is not tied
// to one.
func (s *Sessions) EnsureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(CSRFCookie); err == nil && c.Value != "" {
		return c.Value
	}
	token, err := auth.NewCSRFToken()
	if err != nil {
		return ""
	}
	setCookie(w, r, CSRFCookie, token, SessionTTL)
	return token
}

// CheckCSRF compares the form's token against the cookie's, in constant time. A
// request with neither fails, so a missing cookie is not a way past it.
func (s *Sessions) CheckCSRF(r *http.Request) bool {
	c, err := r.Cookie(CSRFCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return auth.EqualString(c.Value, r.PostFormValue("csrf"))
}

// Authenticate checks an email and password and returns the administrator.
//
// A missing account and a wrong password are the same answer on purpose: the
// sign-in page must not tell a stranger which addresses have accounts. The
// hash of a known-bad password is verified even when there is no account, so
// the two paths take the same time.
func Authenticate(ctx context.Context, store *db.Store, email, password string) (db.Admin, bool, error) {
	admin, err := store.AdminByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Burn the same work an existing account would have cost, so that
			// a timing difference does not enumerate accounts.
			_, _ = auth.VerifyPassword(decoyHash(), password)
			return db.Admin{}, false, nil
		}
		return db.Admin{}, false, err
	}
	ok, err := auth.VerifyPassword(admin.PasswordHash, password)
	if err != nil {
		return db.Admin{}, false, err
	}
	return admin, ok, nil
}

// decoyHash is a valid argon2id hash of a value nobody knows, used only to
// spend the same time on a sign-in for an address that has no account. It is
// built on first use rather than at startup, because hashing costs 64 MiB and a
// server that never sees a sign-in should never pay it.
var decoyHash = sync.OnceValue(func() string {
	key, err := auth.NewCSRFToken()
	if err != nil {
		key = "pacenote-decoy"
	}
	h, err := auth.HashPassword(key)
	if err != nil {
		return ""
	}
	return h
})

// setCookie writes one of the panel's cookies. Secure is conditional on the
// request having arrived over TLS: a server behind the operator's own proxy is
// commonly reached over plain HTTP on its port, and a Secure cookie on that
// connection is dropped by the browser, which would lock the operator out of
// their own panel.
//
//nolint:gosec // G124: Secure is conditional by design; HttpOnly and SameSite are always set.
func setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	//nolint:gosec // G124: this deletes a cookie; there is no value to protect.
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

func userAgent(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	return ua
}

// SignedInAdmin is the operator reading this request, or empty for nobody.
//
// It exists for the one caller outside this package that has to know: the
// router that mounts plugins, which will not forward an operator-only page to
// somebody who is not one. The answer comes from the session table and never
// from a header, so nothing a browser sends can become an identity.
func (p *Panel) SignedInAdmin(r *http.Request) string {
	sess, ok := p.sessions.Current(r.Context(), r)
	if !ok {
		return ""
	}
	return sess.Email
}
