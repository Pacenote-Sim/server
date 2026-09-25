package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/driverauth"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/marketplace"
	"github.com/pacenote-sim/server/internal/metrics"
	"github.com/pacenote-sim/server/internal/plugins"
	"github.com/pacenote-sim/server/internal/pluginweb"
	"github.com/pacenote-sim/server/internal/setup"
)

// The ports an automatic certificate needs. 443 serves drivers; 80 is kept for
// the certificate check and redirects everything else, because a driver who
// types the host name without a scheme should still arrive.
const (
	httpsAddr = ":443"
	httpAddr  = ":80"
)

// poolSampleInterval is how often the pool gauges are refreshed for the metrics
// port. Often enough to see a pool filling up, rarely enough to cost nothing.
const poolSampleInterval = 10 * time.Second

// phase is one run of the server in one mode. Switching modes ends a phase and
// starts another, which is how setup hands over without a restart: the
// listeners are rebuilt, so the ports can change too.
type phase struct {
	opts      Options
	cfg       config.Config
	mode      Mode
	store     *db.Store
	metrics   *metrics.Registry
	startedAt time.Time
	reload    chan struct{}

	// api is the version 1 handler, kept so that the phase can run its
	// housekeeping alongside the listeners.
	api *api.API

	// market reads the plugin index while the marketplace is on. Its refresh
	// loop is the phase's, like the sweeps.
	market *marketplace.Client
	// drivers holds the driver sessions a login plugin mints through, kept so
	// that the housekeeping can clear out what has run out.
	drivers *driverauth.Sessions

	// keyring holds the data key for this phase. The API and the admin panel
	// share one, so an operator who regenerates the key in the panel changes
	// it for the whole process rather than for one handler.
	keyring *auth.Keyring

	// tlsPorts are the two ports the automatic-certificate mode binds, which
	// are [httpsAddr] and [httpAddr] on a real server and are fields only so
	// that the wiring around them can be exercised on a machine where binding
	// 443 needs privileges nothing in a test should have. Empty means the
	// constants.
	tlsPorts struct{ https, challenge string }

	// plugins is the plugin host, in normal mode only. It is owned by the
	// phase so that switching modes stops every plugin process: a plugin
	// outliving the server that started it is an orphan nothing will kill.
	plugins *plugins.Host

	// Filled in while building, for the banner printed to the terminal.
	listenAddr string
	setupToken string
	settings   config.Settings
}

// serving is one listener and the server on it.
type serving struct {
	name string
	srv  *http.Server
	ln   net.Listener
}

// run serves this phase until the context is cancelled, a listener fails, or
// the wizard says setup is finished. It reports whether the last of those
// happened.
func (p *phase) run(ctx context.Context) (bool, error) {
	servers, err := p.build(ctx)
	if err != nil {
		closeAll(servers)
		return false, err
	}

	phaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s serving) {
			defer wg.Done()
			if err := httpx.Serve(phaseCtx, s.srv, s.ln); err != nil {
				errc <- fmt.Errorf("%s listener: %w", s.name, err)
				return
			}
			errc <- nil
		}(s)
	}
	if p.store != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.metrics.SamplePoolEvery(phaseCtx, poolSampleInterval, p.store.PoolStats)
		}()
	}
	if p.api != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.api.Sweep(phaseCtx, api.SweepInterval)
		}()
	}
	if p.market != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.market.Run(phaseCtx, marketplace.DefaultRefresh)
		}()
	}
	if p.drivers != nil {
		// The driver sessions that have run out. Nothing depends on this
		// having run — the lookup refuses an expired session either way — so a
		// failure is a line in the log and the loop carries on.
		wg.Add(1)
		go func() {
			defer wg.Done()
			sweepDriverSessions(phaseCtx, p.drivers, p.opts.Log, api.SweepInterval)
		}()
	}

	if p.opts.OnListen != nil {
		p.opts.OnListen(p.mode, p.listenAddr)
	}

	switched := false
	var first error
	select {
	case <-ctx.Done():
	case <-p.reload:
		switched = true
	case err := <-errc:
		first = err
	}

	cancel()
	wg.Wait()
	if p.plugins != nil {
		p.plugins.Close()
	}
	close(errc)
	for err := range errc {
		if err != nil && first == nil {
			first = err
		}
	}
	return switched, first
}

// build makes the listeners and the servers for this phase. Listening happens
// here rather than inside Serve so that a port already in use is reported
// before anything claims to be running.
func (p *phase) build(ctx context.Context) ([]serving, error) {
	var out []serving

	mln, err := net.Listen("tcp", p.cfg.MetricsListen)
	if err != nil {
		return nil, fmt.Errorf("the metrics port could not be opened: %w", err)
	}
	out = append(out, serving{
		name: "metrics",
		srv:  httpx.NewServer(p.cfg.MetricsListen, p.metrics.Handler()),
		ln:   mln,
	})

	public, err := p.buildPublic(ctx)
	if err != nil {
		closeAll(out)
		return nil, err
	}
	out = append(out, public...)

	p.announce(ctx, mln.Addr().String())
	return out, nil
}

func (p *phase) buildPublic(ctx context.Context) ([]serving, error) {
	switch p.mode {
	case ModeSetup:
		h, err := p.setupHandler()
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", p.cfg.Listen)
		if err != nil {
			return nil, fmt.Errorf("the listen address %q could not be opened: %w", p.cfg.Listen, err)
		}
		p.listenAddr = ln.Addr().String()
		return []serving{{name: "setup", srv: httpx.NewServer(p.cfg.Listen, h), ln: ln}}, nil

	case ModeNormal:
		settings, err := p.store.Settings(ctx)
		if err != nil {
			return nil, err
		}
		p.settings = settings
		h, err := p.normalHandler(ctx)
		if err != nil {
			return nil, err
		}
		if settings.TLSMode == config.TLSAuto {
			return p.autocertServers(settings, h)
		}
		ln, err := net.Listen("tcp", p.cfg.Listen)
		if err != nil {
			return nil, fmt.Errorf("the listen address %q could not be opened: %w", p.cfg.Listen, err)
		}
		p.listenAddr = ln.Addr().String()
		return []serving{{name: "public", srv: httpx.NewServer(p.cfg.Listen, h), ln: ln}}, nil

	default:
		return nil, fmt.Errorf("app: %q is not a mode", string(p.mode))
	}
}

// autocertServers wires the "obtain a certificate for me" path: HTTPS on 443
// with certificates fetched and renewed in the background, and HTTP on 80 for
// the challenge, redirecting everything else.
//
// The cache lives in the data directory, because losing it means asking a
// certificate authority for a new certificate and those are rate-limited.
func (p *phase) autocertServers(settings config.Settings, h http.Handler) ([]serving, error) {
	cacheDir := config.AutocertDir(p.opts.DataDir)
	if err := config.EnsureDir(cacheDir); err != nil {
		return nil, err
	}
	m := &autocert.Manager{
		Cache:      autocert.DirCache(cacheDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(settings.PublicHost),
	}

	// Both listeners are on every interface deliberately: this is the path
	// where the server itself is the thing on the internet, holding a
	// certificate for a public host name.
	https, challenge := httpsAddr, httpAddr
	if p.tlsPorts.https != "" {
		https, challenge = p.tlsPorts.https, p.tlsPorts.challenge
	}

	httpsLn, err := net.Listen("tcp", https)
	if err != nil {
		return nil, fmt.Errorf("port 443 could not be opened, which an automatic certificate needs: %w", err)
	}
	tlsCfg := m.TLSConfig()
	tlsCfg.MinVersion = tls.VersionTLS12
	httpsSrv := httpx.NewServer(https, h)

	challengeLn, err := net.Listen("tcp", challenge)
	if err != nil {
		_ = httpsLn.Close()
		return nil, fmt.Errorf("port 80 could not be opened, which the certificate check needs: %w", err)
	}
	challengeSrv := httpx.NewServer(challenge, m.HTTPHandler(nil))

	p.listenAddr = httpsLn.Addr().String()
	return []serving{
		{name: "https", srv: httpsSrv, ln: tls.NewListener(httpsLn, tlsCfg)},
		{name: "certificate check", srv: challengeSrv, ln: challengeLn},
	}, nil
}

func (p *phase) setupHandler() (http.Handler, error) {
	token, err := auth.NewSetupToken()
	if err != nil {
		return nil, err
	}
	p.setupToken = token
	wz, err := setup.New(setup.Deps{
		Log:     p.opts.Log,
		Version: p.opts.Version,
		Token:   token,
		DataDir: p.opts.DataDir,
		Base:    p.cfg,
		Done:    p.reload,
		Now:     p.opts.Now,
		OnAttempt: func(outcome string) {
			p.metrics.SetupAttempts.WithLabelValues(outcome).Inc()
		},
	})
	if err != nil {
		return nil, err
	}
	return wz.Handler(), nil
}

func (p *phase) normalHandler(ctx context.Context) (http.Handler, error) {
	p.keyring = auth.NewKeyring(secretKey(p.cfg, p.opts.Log))
	keys := &dataKeyFile{cfg: p.cfg, dir: p.opts.DataDir}

	host, err := p.pluginHost(ctx)
	if err != nil {
		return nil, err
	}
	p.plugins = host

	v1, err := api.New(ctx, api.Deps{
		Log:       p.opts.Log,
		Store:     p.store,
		Now:       p.opts.Now,
		OnRequest: p.metrics.Observe,
		Plugins:   host,
	})
	if err != nil {
		return nil, err
	}
	p.api = v1

	// The marketplace client. It goes online only while the setting says so,
	// which it asks the database on every tick, so turning it off in the
	// panel stops the next fetch without a restart.
	market, err := marketplace.New(marketplace.Options{
		URL:        p.cfg.MarketplaceURL,
		Dir:        config.MarketplaceDir(p.opts.DataDir),
		PluginsDir: config.PluginsDir(p.opts.DataDir),
		Enabled: func(ctx context.Context) bool {
			s, err := p.store.Settings(ctx)
			return err == nil && s.Marketplace
		},
		UserAgent: "pacenote-server/" + p.opts.Version,
		Log:       p.opts.Log,
		Now:       p.opts.Now,
	})
	if err != nil {
		return nil, err
	}
	p.market = market

	panel, err := admin.New(admin.Deps{
		Log:       p.opts.Log,
		Store:     p.store,
		Version:   p.opts.Version,
		StartedAt: p.startedAt,
		Settings:  p.store.Settings,
		Now:       p.opts.Now,
		OnLogin: func(outcome string) {
			p.metrics.AdminLogins.WithLabelValues(outcome).Inc()
		},
		Keyring:     p.keyring,
		SaveDataKey: keys.save,
		// The plugin host, and where a plugin is dropped to install one by
		// hand. The directory is passed as well as the host because the empty
		// state has to name it: "there are no plugins" without saying where
		// they go is a dead end.
		Plugins:     host,
		PluginDir:   config.PluginsDir(p.opts.DataDir),
		Marketplace: market,
		// The prebuilt Windows client, and where copies of it go. Both live in
		// the data directory by default, because that is the one place an
		// operator already knows about and the one place that survives an
		// upgrade of this binary.
		Clients: clientbuild.Builder{
			Source: clientbuild.Source{Path: p.cfg.ClientBinaryPath(p.opts.DataDir)},
			Dir:    config.ClientBuildsDir(p.opts.DataDir),
			Now:    p.opts.Now,
			// The operator's own code-signing certificate, opened at the
			// moment somebody presses build. It is a function rather than a
			// value because a certificate is uploaded and removed while the
			// server runs, and because the private key inside it has no
			// business sitting in memory between builds.
			Certificate: admin.Certificate(p.store, p.keyring),
		},
		// The one line that makes a settings change take effect without a
		// restart: the panel writes, the API drops what it was holding, and
		// the next request rebuilds the discovery document from the database.
		OnSettingsChanged: v1.Invalidate,
		// The preview on the settings page is the document a client would be
		// served right now, built the way the real one is so the two cannot
		// drift.
		Discovery: func(s config.Settings) wire.Discovery {
			return api.Discovery(s, api.Features())
		},
	})
	if err != nil {
		return nil, err
	}

	// The plugins that asked for a route. It is mounted whether or not any
	// plugin has one: a server that grew the route only when a plugin wanted it
	// would answer a different way on two installations, and "there is nothing
	// at that address" is the same answer either way.
	drivers := driverauth.New(p.store, p.opts.Now)
	p.drivers = drivers
	plugs, err := pluginweb.New(pluginweb.Deps{
		Log:      p.opts.Log,
		Plugins:  host,
		Settings: p.store.Settings,
		Who:      callerFor(panel, drivers, v1, p.opts.Log),
		SignIn:   signInWith(drivers),
		SignOut:  signOutWith(drivers),
	})
	if err != nil {
		return nil, err
	}
	return normalHandler(panel, v1, plugs, p.opts.Log), nil
}

// pluginHost builds the plugin host and starts whatever is in the plugin
// directory.
//
// A plugin that will not start does not stop the server: the whole point of
// running them as separate processes is that the server survives them, and a
// team whose chat integration is broken still wants their laps uploaded. So
// discovery failing is a line in the log and a plugin marked failed in the
// panel, never a server that refuses to come up.
func (p *phase) pluginHost(ctx context.Context) (*plugins.Host, error) {
	host, err := plugins.New(plugins.Options{
		Dir:     config.PluginsDir(p.opts.DataDir),
		Log:     p.opts.Log,
		Store:   p.store,
		Keyring: p.keyring,
		Now:     p.opts.Now,
	})
	if err != nil {
		return nil, err
	}
	if err := host.Discover(ctx); err != nil {
		p.opts.Log.LogAttrs(ctx, slog.LevelWarn, "the plugin directory could not be read",
			slog.String("reason", err.Error()))
	}
	p.opts.Log.LogAttrs(ctx, slog.LevelInfo, "plugins started",
		slog.Int("interface_version", host.InterfaceVersion()),
		slog.Int("plugins", len(host.List())))
	return host, nil
}

func closeAll(servers []serving) {
	for _, s := range servers {
		if s.ln != nil {
			_ = s.ln.Close()
		}
	}
}

// announce prints the one thing a person reads on the terminal, and logs the
// same facts minus the token. The token is printed and never logged: that is
// the whole reason these are two separate writers.
func (p *phase) announce(ctx context.Context, metricsAddr string) {
	out := p.opts.Terminal
	switch p.mode {
	case ModeSetup:
		fmt.Fprintf(out, "Pacenote %s — first run\n", p.opts.Version)
		fmt.Fprintf(out, "Open  %s\n", setupURL(p.listenAddr))
		fmt.Fprintf(out, "Token %s        (this terminal only, once)\n", p.setupToken)
		p.logAttrs(ctx, slog.LevelInfo, "setup mode",
			slog.String("listen", p.listenAddr),
			slog.String("metrics", metricsAddr))
	case ModeNormal:
		name := p.settings.Organisation
		if name == "" {
			name = "Pacenote"
		}
		fmt.Fprintf(out, "Pacenote %s — %s\n", p.opts.Version, name)
		fmt.Fprintf(out, "Admin %s/admin\n", p.settings.BaseURL())
		fmt.Fprintf(out, "Bound %s\n", p.listenAddr)
		p.logAttrs(ctx, slog.LevelInfo, "running",
			slog.String("organisation", name),
			slog.String("public_host", p.settings.PublicHost),
			slog.String("tls_mode", string(p.settings.TLSMode)),
			slog.String("listen", p.listenAddr),
			slog.String("metrics", metricsAddr))
	}
}

// setupURL turns a listen address into something an operator can paste. A
// server listening on every interface prints its own host name rather than
// "[::]", because that is what they have to type.
func setupURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/setup"
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = hostName()
	}
	return "http://" + net.JoinHostPort(host, port) + "/setup"
}

func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return h
}

func (p *phase) logAttrs(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	p.opts.Log.LogAttrs(ctx, level, msg, attrs...)
}

// sweepDriverSessions clears out expired driver sessions on a tick, and returns
// when the phase does.
func sweepDriverSessions(ctx context.Context, s *driverauth.Sessions, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.Sweep(ctx)
			switch {
			case err != nil:
				log.LogAttrs(ctx, slog.LevelWarn, "the expired driver sessions could not be cleared out",
					slog.Any("error", err))
			case n > 0:
				log.LogAttrs(ctx, slog.LevelInfo, "swept", slog.Int64("driver_sessions", n))
			}
		}
	}
}

// callerFor is how the plugin router learns who is asking.
//
// Both halves come from this server's own session tables and neither from a
// header, which is the whole rule: a plugin is told who the caller is and never
// asked, so nothing a browser can set becomes an identity.
func callerFor(panel *admin.Panel, drivers *driverauth.Sessions, tokens *api.API, log *slog.Logger) func(*http.Request) pluginweb.Caller {
	return func(r *http.Request) pluginweb.Caller {
		out := pluginweb.Caller{AdminEmail: panel.SignedInAdmin(r)}
		if sess, ok := drivers.Current(r.Context(), r); ok {
			out.DriverSlug, out.DriverName = sess.Slug, sess.Name
			return out
		}
		// A client with no browser has no session. It has the device token it
		// uploads with, and that is the same driver: the token is checked the
		// way the API checks it, the plugin is told who, and the token itself
		// is kept from the plugin on the way through. A request with no token
		// is nobody, which is what a browser that is not signed in is too.
		if tokens == nil {
			return out
		}
		driver, err := tokens.DriverByBearer(r.Context(), r)
		switch {
		case err == nil:
			out.DriverSlug, out.DriverName = driver.Slug, driver.Name
		case errors.Is(err, api.ErrNoToken), errors.Is(err, api.ErrBadToken), errors.Is(err, api.ErrRevokedToken):
			// The caller's, and answered by the route's own access rule.
		default:
			log.LogAttrs(r.Context(), slog.LevelWarn, "a client's token could not be checked for a plugin route",
				slog.Any("error", err))
		}
		return out
	}
}

// signInWith turns a plugin's word for who somebody is into a session on this
// server. The plugin says who; this mints the session and sets the cookie, and
// the plugin never sees a session token.
func signInWith(drivers *driverauth.Sessions) func(http.ResponseWriter, *http.Request, string, string) error {
	return func(w http.ResponseWriter, r *http.Request, slug, by string) error {
		_, err := drivers.Start(r.Context(), w, r, slug, by)
		return err
	}
}

// signOutWith ends whatever driver session a browser has.
func signOutWith(drivers *driverauth.Sessions) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		return drivers.End(r.Context(), w, r)
	}
}
