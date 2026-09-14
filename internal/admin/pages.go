package admin

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

type loginForm struct{ Email string }

type overviewForm struct {
	Organisation   string
	BaseURL        string
	TLSDescription string
	Version        string
	StartedAt      time.Time
	Uptime         time.Duration

	Stats      db.Stats
	Migrations db.MigrationState
	// The three counts below are int64 so the count helper can format them;
	// html/template will not convert an int for a func that wants int64.
	MigrationsApplied int64
	MigrationsPending int64

	SetupCompletedAt time.Time
	SetupCompletedBy string
}

// requireSession refuses a page to anyone who is not signed in, and sends them
// to the sign-in form rather than showing an error.
func (p *Panel) requireSession(next func(http.ResponseWriter, *http.Request, db.AdminSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := p.sessions.Current(r.Context(), r)
		if !ok {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r, sess)
	}
}

func (p *Panel) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.sessions.Current(r.Context(), r); ok {
		http.Redirect(w, r, "/admin/overview", http.StatusSeeOther)
		return
	}
	csrf := p.sessions.EnsureCSRF(w, r)
	p.render(w, r, "login", pageData{Title: "Sign in", CSRF: csrf, Form: loginForm{}})
}

func (p *Panel) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	csrf := p.sessions.EnsureCSRF(w, r)
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the sign-in page again and try once more.")
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	host := httpx.ClientHost(r)

	if !p.logins.Allow(host) {
		p.deps.OnLogin("rate_limited")
		p.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "sign-in refused",
			slog.String("reason", "rate_limited"), slog.String("remote", host))
		p.render(w, r, "login", pageData{
			Title: "Sign in", CSRF: csrf, Status: http.StatusTooManyRequests,
			Error: "Too many attempts from this address. Wait a minute and try again.",
			Form:  loginForm{Email: email},
		})
		return
	}

	admin, ok, err := Authenticate(r.Context(), p.deps.Store, email, password)
	if err != nil {
		p.deps.Log.LogAttrs(r.Context(), slog.LevelError, "sign-in failed", slog.Any("error", err))
		p.render(w, r, "login", pageData{
			Title: "Sign in", CSRF: csrf, Status: http.StatusInternalServerError,
			Error: "The database did not answer, so nobody can sign in right now.",
			Form:  loginForm{Email: email},
		})
		return
	}
	if !ok {
		p.deps.OnLogin("rejected")
		// The address is not logged either: it is a piece of the credential
		// pair somebody just tried.
		p.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "sign-in refused",
			slog.String("reason", "bad_credentials"), slog.String("remote", host))
		p.render(w, r, "login", pageData{
			Title: "Sign in", CSRF: csrf, Status: http.StatusUnauthorized,
			Error: "That email address and password do not match an account here.",
			Form:  loginForm{Email: email},
		})
		return
	}

	if _, err := p.sessions.Start(r.Context(), w, r, admin.ID); err != nil {
		p.deps.Log.LogAttrs(r.Context(), slog.LevelError, "session could not be started", slog.Any("error", err))
		httpx.Problem(w, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}
	_ = p.deps.Store.TouchAdminLogin(r.Context(), admin.ID)

	// A hash made with weaker parameters than today's is rewritten now, while
	// the password is in hand. It is the only moment it can be done.
	if auth.NeedsRehash(admin.PasswordHash) {
		if hash, err := auth.HashPassword(password); err == nil {
			_ = p.deps.Store.SetAdminPasswordHash(r.Context(), admin.ID, hash)
		}
	}

	p.deps.OnLogin("accepted")
	p.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "signed in", slog.Int64("admin_id", admin.ID))
	http.Redirect(w, r, "/admin/overview", http.StatusSeeOther)
}

func (p *Panel) postLogout(w http.ResponseWriter, r *http.Request) {
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}
	if err := p.sessions.End(r.Context(), w, r); err != nil {
		p.deps.Log.LogAttrs(r.Context(), slog.LevelError, "sign-out failed", slog.Any("error", err))
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)

	var problems []string
	settings, err := p.deps.Settings(ctx)
	if err != nil {
		// Named rather than only logged. Without it the page renders with a
		// blank organisation and a blank address and says nothing about why,
		// which reads as a server that has forgotten which team it is.
		problems = append(problems, "the settings for this organisation could not be read")
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
	}
	form := overviewForm{
		Organisation:   settings.Organisation,
		BaseURL:        settings.BaseURL(),
		TLSDescription: tlsDescription(settings.TLSMode),
		Version:        p.deps.Version,
		StartedAt:      p.deps.StartedAt,
		Uptime:         p.deps.Now().Sub(p.deps.StartedAt),
	}

	if form.Stats, err = p.deps.Store.Stats(ctx); err != nil {
		problems = append(problems, "the size and counts could not be read")
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "stats unreadable", slog.Any("error", err))
	}
	if form.Migrations, err = p.deps.Store.MigrationState(ctx); err != nil {
		problems = append(problems, "the schema state could not be read")
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "migration state unreadable", slog.Any("error", err))
	}
	form.MigrationsApplied = int64(form.Migrations.Applied)
	form.MigrationsPending = int64(form.Migrations.Pending)

	if st, err := p.deps.Store.SetupState(ctx); err == nil {
		form.SetupCompletedAt, form.SetupCompletedBy = st.CompletedAt, st.CompletedBy
	}

	data := pageData{
		Title:        "Overview",
		Organisation: settings.Organisation,
		SignedIn:     true,
		Email:        sess.Email,
		CSRF:         csrf,
		Nav:          navFor("Overview"),
		Form:         form,
	}
	if len(problems) > 0 {
		data.Error = "Some of this page is missing: " + strings.Join(problems, ", ") + "."
	}
	p.render(w, r, "overview", data)
}

func tlsDescription(mode config.TLSMode) string {
	if mode == config.TLSAuto {
		return "This server holds its own certificate, on 443, with 80 kept for the certificate check."
	}
	return "Plain HTTP on this server's port, with your own proxy in front holding the certificate."
}
