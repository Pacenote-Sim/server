package setup

import (
	"net/http"
	"sync"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
)

// step is how far through the wizard a session has got. The zero value is the
// token gate, so a session that does not exist is at the beginning.
type step int

// The wizard's steps, in the order it asks the questions.
const (
	stepToken step = iota
	stepDatabase
	stepOrganisation
	stepAccount
	stepAddress
	stepFinish
	stepDone
)

// path is where a step is served.
func (s step) path() string {
	switch s {
	case stepToken:
		return "/setup"
	case stepDatabase:
		return "/setup/database"
	case stepOrganisation:
		return "/setup/organisation"
	case stepAccount:
		return "/setup/account"
	case stepAddress:
		return "/setup/address"
	case stepFinish:
		return "/setup/finish"
	case stepDone:
		return "/setup/done"
	default:
		return "/setup"
	}
}

// page is the template that renders a step.
func (s step) page() string {
	switch s {
	case stepToken:
		return "token"
	case stepDatabase:
		return "database"
	case stepOrganisation:
		return "organisation"
	case stepAccount:
		return "account"
	case stepAddress:
		return "address"
	case stepFinish:
		return "finish"
	case stepDone:
		return "done"
	default:
		return "token"
	}
}

// title is the step's heading, also used in the browser tab.
func (s step) title() string {
	switch s {
	case stepToken:
		return "Setup"
	case stepDatabase:
		return "Database"
	case stepOrganisation:
		return "Organisation"
	case stepAccount:
		return "Administrator"
	case stepAddress:
		return "Public address"
	case stepFinish:
		return "Finish"
	case stepDone:
		return "Done"
	default:
		return "Setup"
	}
}

// wizardSession is one operator's progress. It lives in memory and dies with
// the process, because none of it is worth persisting before it is committed —
// and the password in it is worth not persisting at all, which is why only its
// hash is kept.
//
// Everything below mu is guarded by it. Two requests can share one session —
// an operator with two tabs, or a double click on the last step — so the
// session is locked for the whole of a request rather than around each field.
// The lock is not what makes setup one-time: the database is. It is what keeps
// this struct consistent while that is decided.
type wizardSession struct {
	// id, csrf and created are set once, before the session is published, and
	// never change.
	id      string
	csrf    string
	created time.Time

	mu   sync.Mutex
	step step

	databaseURL  string
	organisation string
	email        string
	passwordHash string
	host         string
	tlsMode      config.TLSMode
}

// newSession mints a session once the token has been accepted.
func (w *Wizard) newSession() (*wizardSession, error) {
	id, _, err := auth.NewSessionToken()
	if err != nil {
		return nil, err
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		return nil, err
	}
	s := &wizardSession{
		id:          id,
		csrf:        csrf,
		created:     w.deps.Now(),
		step:        stepDatabase,
		databaseURL: w.deps.Base.DatabaseURL,
		tlsMode:     config.TLSProxy,
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweepSessions()
	w.sessions[s.id] = s
	return s, nil
}

// session returns the session this request belongs to, or nil.
func (w *Wizard) session(r *http.Request) *wizardSession {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.sessions[c.Value]
	if !ok {
		return nil
	}
	if w.deps.Now().Sub(s.created) > SessionTTL {
		delete(w.sessions, s.id)
		return nil
	}
	return s
}

// sweepSessions drops the wizard sessions that have run out. The caller holds
// the lock.
func (w *Wizard) sweepSessions() {
	now := w.deps.Now()
	for id, s := range w.sessions {
		if now.Sub(s.created) > SessionTTL {
			delete(w.sessions, id)
		}
	}
}

// setCookies writes the session and CSRF cookies. Both are host-only, both are
// HttpOnly, and both are SameSite=Lax so that a form posted from another site
// arrives without them.
//
// Secure is set only when the request arrived over TLS. Setup commonly runs
// over plain HTTP on a port an operator reached directly, and a Secure cookie
// on that connection is simply dropped, which would lock them out of their own
// wizard.
func (w *Wizard) setCookies(rw http.ResponseWriter, r *http.Request, s *wizardSession) {
	secure := r.TLS != nil
	//nolint:gosec // G124: Secure is conditional on purpose — see the comment above; HttpOnly and SameSite are set.
	http.SetCookie(rw, &http.Cookie{
		Name: SessionCookie, Value: s.id, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(SessionTTL / time.Second),
	})
	//nolint:gosec // G124: Secure is conditional on purpose — see the comment above; HttpOnly and SameSite are set.
	http.SetCookie(rw, &http.Cookie{
		Name: CSRFCookie, Value: s.csrf, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(SessionTTL / time.Second),
	})
}

// clearCookies removes both cookies once there is nothing left to do with them.
func (w *Wizard) clearCookies(rw http.ResponseWriter) {
	for _, name := range []string{SessionCookie, CSRFCookie} {
		//nolint:gosec // G124: this deletes a cookie; there is no value to protect.
		http.SetCookie(rw, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	}
}

// checkCSRF compares the form's token against the cookie's, in constant time.
// Both have to be present and both have to match the session, so a form
// replayed from another site fails whether or not it guessed the session.
func (w *Wizard) checkCSRF(r *http.Request, s *wizardSession) bool {
	c, err := r.Cookie(CSRFCookie)
	if err != nil || c.Value == "" {
		return false
	}
	form := r.PostFormValue("csrf")
	return auth.EqualString(c.Value, s.csrf) && auth.EqualString(form, s.csrf)
}
