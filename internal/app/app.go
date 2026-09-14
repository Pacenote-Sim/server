// Package app is the server's runtime: it decides which mode to start in,
// builds the listeners for that mode, and switches between them without the
// operator having to restart anything.
//
// # The startup decision
//
// The database decides whether setup has run. Not the data directory — that is
// a cache of a connection string, and on a container it can live on a volume
// that does not survive a restart. The order is:
//
//  1. Read the configuration file, then let the environment override it.
//  2. If no connection string is known from either, there is nothing to ask, so
//     the wizard runs and its first question is the connection string.
//  3. If one is known, connect. A database that cannot be reached is a hard
//     stop, not a wizard: a server that cannot reach its data has nothing to
//     serve, and opening setup on an unreachable database is how an
//     administrator account gets taken over.
//  4. A reachable database whose setup_state row says complete closes setup
//     permanently, whatever is on disk.
//  5. A reachable database with no such row is a first run, or a run that was
//     interrupted before its transaction committed. Either way the wizard runs.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/buildinfo"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/metrics"
	"github.com/pacenote-sim/server/internal/pluginweb"
	"github.com/pacenote-sim/server/internal/web"
)

// Mode is which half of the server is running.
type Mode string

// The two modes. There is no third: a server is either being configured or
// serving.
const (
	// ModeSetup serves the wizard and nothing else.
	ModeSetup Mode = "setup"
	// ModeNormal serves the admin panel, and in a later chunk the API.
	ModeNormal Mode = "normal"
)

// Options is what [Run] needs from the command line and the environment.
type Options struct {
	// DataDir is the data directory. Empty means beside the binary.
	DataDir string
	// Log is where structured lines go. nil means a discarding logger.
	Log *slog.Logger
	// Terminal is where the first-run banner and the setup token are printed.
	// It is deliberately not the logger: the token must never be in a log.
	Terminal io.Writer
	// Version is the build, for pages and for the setup record.
	Version string
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Listen and MetricsListen override the addresses in the configuration
	// file and the environment. Empty means use what is configured. They exist
	// for a supervisor that allocates ports, and for tests, which ask for
	// "127.0.0.1:0" and read back what they were given.
	Listen, MetricsListen string
	// OnListen, when set, is called each time the server begins listening,
	// with the mode it is in and the address its public listener bound. It is
	// how a test finds the port after asking for one, and how a supervisor
	// could learn the same thing.
	OnListen func(mode Mode, addr string)
}

// Run starts the server and blocks until ctx is cancelled or something fails.
// A clean shutdown returns nil.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Terminal == nil {
		opts.Terminal = io.Discard
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Version == "" {
		opts.Version = buildinfo.Read().Short()
	}
	if opts.DataDir == "" {
		opts.DataDir = config.DefaultDir()
	}
	startedAt := opts.Now()

	mx := metrics.New()

	for {
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		mode, store, err := decide(ctx, opts, cfg)
		if err != nil {
			return err
		}

		reload := make(chan struct{}, 1)
		phase := &phase{
			opts:      opts,
			cfg:       cfg,
			mode:      mode,
			store:     store,
			metrics:   mx,
			startedAt: startedAt,
			reload:    reload,
		}

		switched, err := phase.run(ctx)
		if store != nil {
			store.Close()
		}
		if err != nil {
			return err
		}
		if !switched {
			return nil
		}
		opts.Log.LogAttrs(ctx, slog.LevelInfo, "setup finished, switching to normal mode")
	}
}

// loadConfig reads the configuration file and applies the environment over it.
// A missing file is not a failure: the environment may carry everything, and if
// it does not the wizard asks.
func loadConfig(opts Options) (config.Config, error) {
	cfg, err := config.Load(opts.DataDir)
	switch {
	case errors.Is(err, config.ErrNotConfigured):
		if cfg.DatabaseURL != "" {
			opts.Log.LogAttrs(context.Background(), slog.LevelInfo,
				"no configuration file — the database connection string came from the environment",
				slog.String("data_dir", opts.DataDir),
				slog.String("variable", config.EnvDatabaseURL))
		}
	case err != nil:
		return cfg, err
	}
	if opts.Listen != "" {
		cfg.Listen = opts.Listen
	}
	if opts.MetricsListen != "" {
		cfg.MetricsListen = opts.MetricsListen
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// decide answers the one question at startup: setup, or normal?
func decide(ctx context.Context, opts Options, cfg config.Config) (Mode, *db.Store, error) {
	if cfg.DatabaseURL == "" {
		// A data directory that exists but names no database is not a first
		// run: something was configured here and the file has since gone. The
		// wizard must not reopen on that, because the database it used may
		// already hold an administrator, so say what is needed and stop.
		if dirExists(opts.DataDir) {
			return "", nil, &NotConfiguredError{DataDir: opts.DataDir}
		}
		// Otherwise there is genuinely nothing to ask a database. The wizard's
		// first question is the connection string, and its answer is checked
		// against the same rule before anything is written.
		return ModeSetup, nil, nil
	}

	store, err := db.Open(ctx, cfg.DatabaseURL, opts.Log)
	if err != nil {
		return "", nil, startupDatabaseError(err)
	}

	state, err := store.SetupState(ctx)
	if err != nil {
		store.Close()
		return "", nil, startupDatabaseError(err)
	}
	if !state.Complete {
		// Setup has not finished on this database. Close the pool: the wizard
		// opens its own once the operator has confirmed which database to use,
		// which may not be this one.
		store.Close()
		return ModeSetup, nil, nil
	}

	if err := store.Migrate(ctx); err != nil {
		store.Close()
		return "", nil, err
	}
	return ModeNormal, store, nil
}

// startupDatabaseError turns a failure to reach the database at startup into
// the message an operator needs, and makes the important half of it explicit:
// this is not a reason to run setup again.
func startupDatabaseError(err error) error {
	var connErr *db.ConnectError
	if errors.As(err, &connErr) {
		return fmt.Errorf("%s\nThe setup wizard will not run instead: if this database has already been set up it holds an administrator account, and reopening setup would hand that account to whoever asked first", connErr.Message)
	}
	return err
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// NotConfiguredError is what [Run] returns when there is no way to reach a
// database and no way to ask for one. The command prints it and exits.
type NotConfiguredError struct{ DataDir string }

// Error implements error, and says what to do about it.
func (e *NotConfiguredError) Error() string {
	return fmt.Sprintf(
		"There is no configuration in %s and %s is not set, so this server does not know which database to use.\n"+
			"Set %s to the connection string and start it again.\n"+
			"If that database has already been set up, the wizard will not run again — it already holds an administrator account.",
		e.DataDir, config.EnvDatabaseURL, config.EnvDatabaseURL)
}

// normalHandler is what a configured server serves: the telemetry API, the
// discovery document, the admin panel, the one stylesheet, and a plain answer
// everywhere else.
//
// One mux carries both halves because they are one server to everyone who uses
// it: a driver's client resolves the host to the discovery document and an
// operator opens the same host in a browser. The middleware below is worn by
// both, which is what keeps the hardening from being something an endpoint can
// be written without.
func normalHandler(panel *admin.Panel, v1 *api.API, plugs *pluginweb.Router, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+web.AssetPrefix, web.Assets())
	v1.Routes(mux)
	panel.Routes(mux)
	plugs.Routes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, admin.Prefix, http.StatusSeeOther)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Problem(w, r, http.StatusNotFound, "There is nothing at that address on this server.")
	})
	return httpx.Chain(mux,
		httpx.Recover(log),
		httpx.LogRequests(log),
		httpx.SecureHeaders,
		httpx.NoStore,
	)
}

// secretKey reads the data key from the configuration, or reports that there
// isn't one. A missing key is not fatal: it means no plugin's stored credential
// can be read, and the operator is told so on the plugin's own page rather than
// on every call.
func secretKey(cfg config.Config, log *slog.Logger) auth.SecretKey {
	key, err := auth.ParseSecretKey(cfg.SecretKey)
	if err != nil {
		log.LogAttrs(context.Background(), slog.LevelWarn,
			"the secret key in the configuration file is not readable, so a stored API key cannot be used")
		return nil
	}
	return key
}

// dataKeyFile writes a replacement data key into config.json.
//
// It exists so the admin panel can regenerate the key without knowing where
// the file is or what else is in it: the panel mints a key and re-seals what
// was sealed, and this puts the key where the next startup will find it.
//
// The mutex is not decoration. The save happens on a request goroutine while
// the rest of the phase holds its own copy of the configuration, so the copy
// this type carries is the one that must not be written by two requests at
// once.
type dataKeyFile struct {
	mu  sync.Mutex
	cfg config.Config
	dir string
}

// save writes cfg with the new key into the data directory. The write is
// atomic — a temporary file and a rename — so a crash halfway through leaves
// the old key in place rather than half of the new one.
func (w *dataKeyFile) save(_ context.Context, key auth.SecretKey) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := w.cfg
	next.SecretKey = key.String()
	if err := next.Save(w.dir); err != nil {
		return err
	}
	w.cfg = next
	return nil
}
