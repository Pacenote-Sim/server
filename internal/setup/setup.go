// Package setup is the first-run wizard: the only thing this server serves
// until an operator has told it what it is.
//
// # Why there is a token
//
// An operator runs this on a remote machine, so the port is reachable by
// strangers before an administrator account exists. Without a token, whoever
// loads the page first becomes the administrator. It is printed to the terminal
// only, never to a log, it is at least 128 bits from crypto/rand, it is
// compared in constant time, it is single-use, and a fresh one is minted on
// every restart so that a token left in an old scrollback is already dead.
//
// # Why the database decides whether this runs
//
// The rule is not "no data directory means setup mode". A data directory is a
// cache of a connection string: it can be deleted, and on a container it can
// live on a volume that does not survive a restart. If its absence reopened the
// wizard, anyone who could delete it would be handed the administrator account
// of a database that already has one.
//
// So a single row in the database marks completion, written in the same
// transaction that creates the first administrator, and the server asks the
// database at every startup. See [github.com/pacenote-sim/server/internal/db.Store.CompleteSetup].
package setup

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/web"
)

//go:embed templates
var templatesFS embed.FS

// SessionCookie carries the wizard session. It is not a credential to anything
// but the wizard, and it dies with the process.
const SessionCookie = "pacenote_setup"

// CSRFCookie is the other half of the double-submit check on every form.
const CSRFCookie = "pacenote_setup_csrf"

// SessionTTL is how long a half-finished wizard stays open.
const SessionTTL = 2 * time.Hour

// TokenAttemptsBurst and TokenAttemptsPerMinute rate-limit the one endpoint
// that compares a secret. Five attempts, then one a minute: enough that an
// operator who mistypes is not locked out, and far too few for guessing to be
// worth starting.
const (
	TokenAttemptsBurst     = 5.0
	TokenAttemptsPerMinute = 1.0
)

// Deps is everything the wizard needs from the rest of the server.
type Deps struct {
	// Log receives the wizard's own lines. The token is never one of them.
	Log *slog.Logger
	// Version is the build, shown in the page header and recorded against the
	// completed setup.
	Version string
	// Token is the one-time setup token, already printed to the terminal.
	Token string
	// DataDir is where config.json will be written.
	DataDir string
	// Base is the configuration the server is running with: the listen
	// addresses, and a database URL if one came from the environment.
	Base config.Config
	// Done is signalled once, after the wizard has committed everything, to
	// tell the server to switch to normal mode. It must be buffered, so that
	// signalling never blocks the request that is still being written.
	Done chan<- struct{}
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// OnAttempt is called after every setup token attempt with "accepted",
	// "rejected" or "rate_limited". It is how the metrics port sees that
	// someone is guessing.
	OnAttempt func(outcome string)
	// Probe tests a connection string and diagnoses what is wrong with it.
	// nil means [github.com/pacenote-sim/server/internal/db.Probe], which is what the server
	// uses; it is a field so that a test can drive the wizard without a
	// database, and so that the admin panel's "test connection" button can
	// reuse the same seam later.
	Probe func(ctx context.Context, url string) error
	// AlreadySetUp reports whether that database has already been through
	// setup. nil means ask the database, which is the rule: the database
	// decides, not the filesystem.
	AlreadySetUp func(ctx context.Context, url string) (bool, error)
}

// Wizard serves the first-run pages.
type Wizard struct {
	deps      Deps
	templates map[string]*template.Template
	limiter   *httpx.Limiter

	mu        sync.Mutex
	sessions  map[string]*wizardSession
	tokenUsed bool

	completed atomic.Bool
	signalled atomic.Bool
}

// New builds the wizard. It returns an error only if the embedded templates do
// not parse, which is a build fault rather than a runtime one.
func New(d Deps) (*Wizard, error) {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OnAttempt == nil {
		d.OnAttempt = func(string) {}
	}
	if d.Probe == nil {
		d.Probe = db.Probe
	}
	if d.AlreadySetUp == nil {
		d.AlreadySetUp = func(ctx context.Context, url string) (bool, error) {
			store, err := db.Open(ctx, url, d.Log)
			if err != nil {
				return false, err
			}
			defer store.Close()
			state, err := store.SetupState(ctx)
			if err != nil {
				return false, err
			}
			return state.Complete, nil
		}
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Wizard{
		deps:      d,
		templates: tmpl,
		limiter:   httpx.NewLimiter(TokenAttemptsPerMinute/60, TokenAttemptsBurst, 60.0/60, 60),
		sessions:  make(map[string]*wizardSession),
	}, nil
}

// Completed reports whether setup has finished in this process.
func (w *Wizard) Completed() bool { return w.completed.Load() }

// Handler is the whole of what a server in setup mode serves: the wizard, the
// one stylesheet, and a plain page for everything else saying why there is
// nothing there.
func (w *Wizard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+web.AssetPrefix, web.Assets())

	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /setup", w.getToken)
	mux.HandleFunc("POST /setup", w.postToken)
	mux.HandleFunc("GET /setup/database", w.page(stepDatabase))
	mux.HandleFunc("POST /setup/database", w.postDatabase)
	mux.HandleFunc("GET /setup/organisation", w.page(stepOrganisation))
	mux.HandleFunc("POST /setup/organisation", w.postOrganisation)
	mux.HandleFunc("GET /setup/account", w.page(stepAccount))
	mux.HandleFunc("POST /setup/account", w.postAccount)
	mux.HandleFunc("GET /setup/address", w.page(stepAddress))
	mux.HandleFunc("POST /setup/address", w.postAddress)
	mux.HandleFunc("GET /setup/finish", w.page(stepFinish))
	mux.HandleFunc("POST /setup/finish", w.postFinish)
	mux.HandleFunc("GET /setup/done", w.getDone)
	mux.HandleFunc("/", w.notHere)

	return httpx.Chain(mux,
		httpx.Recover(w.deps.Log),
		httpx.LogRequests(w.deps.Log),
		httpx.SecureHeaders,
		httpx.NoStore,
	)
}

// notHere answers everything the wizard does not serve. It is deliberately the
// same answer for an API path and a typo: a server in setup mode has no
// drivers, no laps and no opinions, and saying so once is enough.
func (w *Wizard) notHere(rw http.ResponseWriter, r *http.Request) {
	httpx.Problem(rw, r, http.StatusNotFound,
		"This server has not been set up yet. Open /setup with the token from the terminal.")
}

func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"token", "database", "organisation", "account", "address", "finish", "done", "blocked"}
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New(p).Funcs(web.FuncMap()).
			ParseFS(templatesFS, "templates/layout.tmpl", "templates/"+p+".tmpl")
		if err != nil {
			return nil, fmt.Errorf("setup: the %s page does not parse: %w", p, err)
		}
		out[p] = t
	}
	return out, nil
}

// render writes one page. A template that fails halfway has already written a
// partial response, so there is nothing useful to send instead; it is logged
// and the connection carries what it carries.
func (w *Wizard) render(rw http.ResponseWriter, r *http.Request, page string, data pageData) {
	t, ok := w.templates[page]
	if !ok {
		httpx.Problem(rw, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}
	data.Version = w.deps.Version
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data.Status == 0 {
		data.Status = http.StatusOK
	}
	rw.WriteHeader(data.Status)
	if err := t.ExecuteTemplate(rw, "layout", data); err != nil {
		w.deps.Log.LogAttrs(r.Context(), slog.LevelError, "setup page did not render",
			slog.String("page", page), slog.Any("error", err))
	}
}

// finished marks the wizard done and wakes the server so it can switch to
// normal mode. It is idempotent, and the signal is non-blocking, so the request
// that triggered it finishes writing its page first — the graceful shutdown
// that follows waits for exactly that.
func (w *Wizard) finished() {
	w.completed.Store(true)
	if w.signalled.Swap(true) {
		return
	}
	if w.deps.Done == nil {
		return
	}
	select {
	case w.deps.Done <- struct{}{}:
	default:
	}
}

// commit does the irreversible half of setup: migrate, then write the marker,
// the administrator and the settings in one transaction, then write the
// configuration file.
//
// The order matters. Migrations first, because the marker lives in the schema
// they create. The transaction next, because it is the thing that must be
// all-or-nothing. The configuration file last, because it is only a cache: if
// writing it fails the server still has an administrator in the database, and
// the operator is told to supply the connection string in the environment
// rather than being offered a wizard that would overwrite what is there.
func (w *Wizard) commit(ctx context.Context, s *wizardSession) (config.Config, config.Settings, error) {
	cfg := w.deps.Base
	cfg.DatabaseURL = s.databaseURL

	settings := config.DefaultSettings(s.organisation, s.host, s.tlsMode)

	store, err := db.Open(ctx, s.databaseURL, w.deps.Log)
	if err != nil {
		return cfg, settings, err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return cfg, settings, err
	}

	if cfg.SecretKey == "" {
		key, err := auth.NewSecretKey()
		if err != nil {
			return cfg, settings, err
		}
		cfg.SecretKey = key.String()
	}
	if _, err := store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        s.email,
		AdminPasswordHash: s.passwordHash,
		Settings:          settings,
		ServerVersion:     w.deps.Version,
	}); err != nil {
		return cfg, settings, err
	}

	if err := cfg.Save(w.deps.DataDir); err != nil {
		return cfg, settings, err
	}
	return cfg, settings, nil
}

// describeError turns a failure during commit into something an operator can
// act on. Anything with a diagnosis keeps it; anything else says what stage it
// failed at, because "it failed" is not a useful sentence.
func describeError(err error) string {
	var connErr *db.ConnectError
	if errors.As(err, &connErr) {
		return connErr.Message
	}
	if errors.Is(err, db.ErrSetupAlreadyComplete) {
		return "Setup has already been completed on this server. Sign in to the admin panel instead."
	}
	if errors.Is(err, db.ErrAdminEmailTaken) {
		return "That email address already has an account on this server."
	}
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && i < 24 {
		msg = msg[i+2:]
	}
	return "Setup could not finish: " + msg + "."
}
