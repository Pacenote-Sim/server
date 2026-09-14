package setup

import (
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// pageData is what every template is given. Form carries whatever that one page
// needs on top.
type pageData struct {
	Title   string
	Version string
	Steps   []stepView
	Error   string
	Notice  string
	CSRF    string
	Status  int
	Form    any
}

// stepView is one entry in the progress strip along the top.
type stepView struct {
	Name  string
	Class string
}

func stepViews(current step) []stepView {
	all := []step{stepDatabase, stepOrganisation, stepAccount, stepAddress, stepFinish}
	out := make([]stepView, 0, len(all))
	for _, s := range all {
		v := stepView{Name: s.title()}
		switch {
		case s == current:
			v.Class = "on"
		case s < current:
			v.Class = "done"
		}
		out = append(out, v)
	}
	return out
}

// --------------------------------------------------------------------------
// The token gate
// --------------------------------------------------------------------------

func (w *Wizard) getToken(rw http.ResponseWriter, r *http.Request) {
	if s := w.session(r); s != nil {
		http.Redirect(rw, r, s.step.path(), http.StatusSeeOther)
		return
	}
	if w.tokenSpent() {
		w.render(rw, r, "blocked", pageData{
			Title:  "Setup",
			Status: http.StatusForbidden,
			Form: blockedForm{
				Heading: "That token has been used",
				Body:    "The setup token is good once. Stop the server and start it again to get a new one — it is printed in the terminal, not in any log.",
			},
		})
		return
	}
	w.render(rw, r, "token", pageData{Title: "Setup", CSRF: ""})
}

func (w *Wizard) postToken(rw http.ResponseWriter, r *http.Request) {
	if err := httpx.ParseForm(rw, r, httpx.FormLimit); err != nil {
		httpx.Problem(rw, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	host := httpx.ClientHost(r)
	if !w.limiter.Allow(host) {
		w.deps.OnAttempt("rate_limited")
		w.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "setup token attempt refused",
			slog.String("reason", "rate_limited"), slog.String("remote", host))
		w.render(rw, r, "token", pageData{
			Title:  "Setup",
			Status: http.StatusTooManyRequests,
			Error:  "Too many attempts from this address. Wait a minute and try again.",
		})
		return
	}

	typed := r.PostFormValue("token")
	if !auth.EqualSetupToken(w.deps.Token, typed) {
		w.deps.OnAttempt("rejected")
		// The token itself is never logged — not the right one and not the
		// wrong one, because a wrong one is often a right one mistyped.
		w.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "setup token attempt refused",
			slog.String("reason", "wrong_token"), slog.String("remote", host))
		w.render(rw, r, "token", pageData{
			Title:  "Setup",
			Status: http.StatusUnauthorized,
			Error:  "That token is not right. It is the one printed in the terminal when this server started.",
		})
		return
	}

	if !w.spendToken() {
		w.deps.OnAttempt("rejected")
		w.render(rw, r, "blocked", pageData{
			Title:  "Setup",
			Status: http.StatusForbidden,
			Form: blockedForm{
				Heading: "That token has been used",
				Body:    "The setup token is good once. Stop the server and start it again to get a new one.",
			},
		})
		return
	}

	s, err := w.newSession()
	if err != nil {
		httpx.Problem(rw, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}
	w.deps.OnAttempt("accepted")
	w.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "setup token accepted", slog.String("remote", host))
	w.setCookies(rw, r, s)
	http.Redirect(rw, r, s.step.path(), http.StatusSeeOther)
}

// tokenSpent reports whether the one-time token has been exchanged already.
func (w *Wizard) tokenSpent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tokenUsed
}

// spendToken consumes the token, and reports whether this caller was the one
// that got it. Two correct submissions arriving together therefore produce one
// session, not two.
func (w *Wizard) spendToken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tokenUsed {
		return false
	}
	w.tokenUsed = true
	return true
}

// --------------------------------------------------------------------------
// Serving a step
// --------------------------------------------------------------------------

// page renders one step, after checking that the session is allowed to be on
// it. A session that tries to skip ahead is sent back to where it actually is.
func (w *Wizard) page(want step) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		s := w.session(r)
		if s == nil {
			http.Redirect(rw, r, "/setup", http.StatusSeeOther)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if want > s.step {
			http.Redirect(rw, r, s.step.path(), http.StatusSeeOther)
			return
		}
		w.renderStep(rw, r, s, want, "")
	}
}

// renderStep writes one step's page. The caller holds s.mu.
func (w *Wizard) renderStep(rw http.ResponseWriter, r *http.Request, s *wizardSession, at step, errMsg string) {
	data := pageData{
		Title: at.title(),
		Steps: stepViews(at),
		CSRF:  s.csrf,
		Error: errMsg,
		Form:  w.formFor(s, at),
	}
	if errMsg != "" {
		data.Status = http.StatusUnprocessableEntity
	}
	w.render(rw, r, at.page(), data)
}

// --------------------------------------------------------------------------
// The forms
// --------------------------------------------------------------------------

type databaseForm struct {
	DatabaseURL string
	MinVersion  string
}

type organisationForm struct{ Organisation string }

type accountForm struct {
	Email          string
	MinPasswordLen int
}

type addressForm struct {
	Host        string
	Proxy, Auto bool
}

type finishForm struct {
	Organisation    string
	Email           string
	BaseURL         string
	TLSDescription  string
	DatabaseSummary string
	DataDir         string
}

type doneForm struct {
	AdminURL   string
	Email      string
	PortChange string
}

type blockedForm struct {
	Heading  string
	Body     string
	AdminURL string
}

func (w *Wizard) formFor(s *wizardSession, at step) any {
	switch at {
	case stepDatabase:
		return databaseForm{DatabaseURL: s.databaseURL, MinVersion: db.MinServerVersion}
	case stepOrganisation:
		return organisationForm{Organisation: s.organisation}
	case stepAccount:
		return accountForm{Email: s.email, MinPasswordLen: auth.MinPasswordLen}
	case stepAddress:
		return addressForm{
			Host:  s.host,
			Proxy: s.tlsMode != config.TLSAuto,
			Auto:  s.tlsMode == config.TLSAuto,
		}
	case stepFinish:
		settings := config.DefaultSettings(s.organisation, s.host, s.tlsMode)
		return finishForm{
			Organisation:    s.organisation,
			Email:           s.email,
			BaseURL:         settings.BaseURL(),
			TLSDescription:  tlsDescription(s.tlsMode),
			DatabaseSummary: summariseDatabase(s.databaseURL),
			DataDir:         w.deps.DataDir,
		}
	case stepToken, stepDone:
		return nil
	default:
		return nil
	}
}

// sentence turns an error from a leaf package into something to show an
// operator: the package prefix off, a capital at the front and a full stop at
// the end.
func sentence(err error, prefix string) string {
	msg := strings.TrimPrefix(err.Error(), prefix)
	if msg == "" {
		return msg
	}
	r := []rune(msg)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	msg = string(r)
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

func tlsDescription(mode config.TLSMode) string {
	if mode == config.TLSAuto {
		return "This server gets its own certificate and serves HTTPS on 443, with 80 kept for the certificate check."
	}
	return "Plain HTTP on this server's port, with your own proxy in front holding the certificate."
}

// summariseDatabase renders a connection string without its password, so the
// confirmation page can show which database is about to be written to without
// putting a credential on a screen someone might photograph.
func summariseDatabase(url string) string {
	if url == "" {
		return "not set"
	}
	cfg, err := db.Describe(url)
	if err != nil {
		return "the database you entered"
	}
	return cfg
}

// --------------------------------------------------------------------------
// The posts
// --------------------------------------------------------------------------

// begin does what every post does before it looks at the form: find the
// session, take its lock, check the CSRF token, and check that it is on the
// right step.
//
// It returns the session and the function that releases it. A nil session means
// the request has already been answered, and the release function is safe to
// call in that case too, so a caller can defer it unconditionally.
func (w *Wizard) begin(rw http.ResponseWriter, r *http.Request, at step) (*wizardSession, func()) {
	noop := func() {}
	if err := httpx.ParseForm(rw, r, httpx.FormLimit); err != nil {
		httpx.Problem(rw, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return nil, noop
	}
	s := w.session(r)
	if s == nil {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return nil, noop
	}
	if !w.checkCSRF(r, s) {
		httpx.Problem(rw, r, http.StatusForbidden,
			"That form is stale. Go back to the wizard and try again.")
		return nil, noop
	}
	s.mu.Lock()
	if at > s.step {
		path := s.step.path()
		s.mu.Unlock()
		http.Redirect(rw, r, path, http.StatusSeeOther)
		return nil, noop
	}
	return s, func() { s.mu.Unlock() }
}

// advance moves the session on if it has not already been further, and sends
// the browser to the next step. The caller holds s.mu.
func (w *Wizard) advance(rw http.ResponseWriter, r *http.Request, s *wizardSession, next step) {
	if next > s.step {
		s.step = next
	}
	http.Redirect(rw, r, next.path(), http.StatusSeeOther)
}

func (w *Wizard) postDatabase(rw http.ResponseWriter, r *http.Request) {
	s, release := w.begin(rw, r, stepDatabase)
	if s == nil {
		return
	}
	defer release()
	url := strings.TrimSpace(r.PostFormValue("database_url"))
	s.databaseURL = url
	if url == "" {
		w.renderStep(rw, r, s, stepDatabase, "Enter a connection string. It looks like postgres://user:password@host:5432/pacenote.")
		return
	}
	if err := w.deps.Probe(r.Context(), url); err != nil {
		msg := "That database could not be used."
		var connErr *db.ConnectError
		if errors.As(err, &connErr) {
			msg = connErr.Message
		}
		// The reason and not the error: a driver error can carry the host, the
		// user and, in the wrong build, the connection string.
		w.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "setup database test failed",
			slog.String("reason", db.ReasonOf(err)))
		w.renderStep(rw, r, s, stepDatabase, msg)
		return
	}
	if done, err := w.deps.AlreadySetUp(r.Context(), url); err != nil {
		w.renderStep(rw, r, s, stepDatabase, describeError(err))
		return
	} else if done {
		// The database, not the filesystem, decides. An operator who lost
		// their data directory lands here, and the answer is to give the
		// server the connection string rather than to run the wizard — which
		// would otherwise hand the existing administrator account to whoever
		// asked first.
		w.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "setup refused: that database is already set up")
		w.render(rw, r, "blocked", pageData{
			Title:  "Database",
			Status: http.StatusConflict,
			Form: blockedForm{
				Heading: "That database has already been set up",
				Body: "It holds an administrator account already, so the wizard will not run against it again. " +
					"Stop this server, set " + config.EnvDatabaseURL + " to that connection string, start it again, " +
					"and sign in with the account you made — or point this at a different, empty database.",
			},
		})
		return
	}
	w.advance(rw, r, s, stepOrganisation)
}

func (w *Wizard) postOrganisation(rw http.ResponseWriter, r *http.Request) {
	s, release := w.begin(rw, r, stepOrganisation)
	if s == nil {
		return
	}
	defer release()
	name := strings.TrimSpace(r.PostFormValue("organisation"))
	s.organisation = name
	switch {
	case name == "":
		w.renderStep(rw, r, s, stepOrganisation, "Enter the name drivers should see.")
		return
	case len([]rune(name)) > 120:
		w.renderStep(rw, r, s, stepOrganisation, "That name is longer than 120 characters. Use something a client can show in a header.")
		return
	}
	w.advance(rw, r, s, stepAccount)
}

func (w *Wizard) postAccount(rw http.ResponseWriter, r *http.Request) {
	s, release := w.begin(rw, r, stepAccount)
	if s == nil {
		return
	}
	defer release()
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	again := r.PostFormValue("password2")
	s.email = email

	if _, err := mail.ParseAddress(email); err != nil {
		w.renderStep(rw, r, s, stepAccount, "That is not an email address this server can send to.")
		return
	}
	if err := auth.CheckPasswordStrength(password); err != nil {
		w.renderStep(rw, r, s, stepAccount, sentence(err, "auth: "))
		return
	}
	if password != again {
		w.renderStep(rw, r, s, stepAccount, "The two passwords are not the same.")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		httpx.Problem(rw, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}
	s.passwordHash = hash
	w.advance(rw, r, s, stepAddress)
}

func (w *Wizard) postAddress(rw http.ResponseWriter, r *http.Request) {
	s, release := w.begin(rw, r, stepAddress)
	if s == nil {
		return
	}
	defer release()
	host := strings.TrimSpace(r.PostFormValue("host"))
	mode := config.TLSMode(r.PostFormValue("tls_mode"))
	s.host = host
	if mode.Valid() {
		s.tlsMode = mode
	}
	if err := config.ValidateHost(host); err != nil {
		w.renderStep(rw, r, s, stepAddress, sentence(err, "config: "))
		return
	}
	if !mode.Valid() {
		w.renderStep(rw, r, s, stepAddress, "Choose how this server is reached.")
		return
	}
	if mode == config.TLSAuto && strings.EqualFold(host, "localhost") {
		w.renderStep(rw, r, s, stepAddress, "A certificate cannot be issued for localhost. Put a proxy in front, or use a host name that resolves from the internet.")
		return
	}
	w.advance(rw, r, s, stepFinish)
}
