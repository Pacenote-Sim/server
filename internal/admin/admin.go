// Package admin is the operator's panel: sign in, sign out, a layout, and one
// page that says what this server is and how it is doing.
//
// It has grown a settings page since: everything an operator would otherwise
// have written into the settings table by hand, grouped the way they think
// about it rather than the way it is stored — who this team is, what it
// sounds like, what the language-model features may spend, what the limits
// are, and the two things that cannot be undone.
//
// Drivers, devices and data followed, through that same seam: the roster and
// what each driver has recorded, every paired machine and the one button that
// signs one out, and what is stored together with the retention that bounds it.
// Build client is the last of them: the page an operator presses once to get
// the Windows client their drivers install, already carrying this server's
// address, and the page where the signing question is put to them honestly
// rather than sold.
//
// It is server-rendered html/template, embedded in the binary. No JavaScript
// framework, no content delivery network, nothing fetched from anywhere: an
// operator's admin panel should work on a machine with no route to the
// internet, and the content security policy that allows nothing is only
// possible because there is nothing to allow. The one exception is the
// marketplace, which the operator turns on, and which the browser never talks
// to: the server fetches the index, and the panel renders what it verified.
package admin

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/clientbuild"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/plugins"
	"github.com/pacenote-sim/server/internal/web"
)

//go:embed templates
var templatesFS embed.FS

// Prefix is where the panel is mounted.
const Prefix = "/admin"

// Deps is everything the panel needs from the rest of the server.
type Deps struct {
	// Log receives the panel's own lines.
	Log *slog.Logger
	// Store is the database.
	Store *db.Store
	// Version is the build, shown in the footer and on the overview.
	Version string
	// StartedAt is when the process started, which is what uptime counts from.
	StartedAt time.Time
	// Settings reads the organisation's current settings. It is a function
	// rather than a value because settings change while the server runs.
	Settings func(context.Context) (config.Settings, error)
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// OnLogin is called after every sign-in attempt with "accepted",
	// "rejected" or "rate_limited".
	OnLogin func(outcome string)
	// Keyring holds the data key that seals the operator's API keys. The
	// panel needs it to seal what an operator types, to show the last four
	// characters of what is stored, and to re-seal everything when the key is
	// regenerated. A nil keyring is a server with no data key, which the page
	// says rather than pretending a key could be stored safely.
	Keyring *auth.Keyring
	// SaveDataKey writes a newly minted data key into the configuration file
	// beside the binary. nil means this server cannot rewrite that file, and
	// the danger zone says so rather than offering a button that does nothing.
	SaveDataKey func(context.Context, auth.SecretKey) error
	// OnSettingsChanged is called after every saved change, so that the parts
	// of the server holding a copy of the settings re-read them. It is what
	// makes a change take effect without a restart. nil means nothing is told.
	OnSettingsChanged func()
	// Discovery renders the document as it would be served with the settings
	// given, for the preview on the settings page. nil means no preview.
	Discovery func(config.Settings) wire.Discovery
	// Clients is the prebuilt Windows client this server stamps copies of and
	// the directory the copies are kept in. The zero value is a server that
	// has not been given one, which the build page says plainly rather than
	// failing: a fresh installation has no client binary yet, and the page's
	// job then is to say what to put where.
	Clients clientbuild.Builder
	// PluginDir is where a plugin is dropped to install it by hand. It is
	// shown in the empty state, because "there are no plugins" without saying
	// where they go is a dead end.
	PluginDir string
	// Plugins is the plugin host. nil is a server running in a mode that has
	// none — setup, or a configuration that switched them off — and the page
	// says so rather than being absent, because a missing menu entry is a
	// thing an operator cannot ask a question about.
	Plugins Plugins
	// Marketplace reads the index of approved plugins and installs from it.
	// nil is a server built without one, and the plugins page says so.
	Marketplace Marketplace
}

// Plugins is what the panel needs from the plugin host. It is an interface
// rather than the host itself so that this package does not depend on the
// host's internals, and so the pages can be tested without running a plugin
// process for every assertion about how a table renders.
type Plugins interface {
	// All is every plugin this server knows about, running or not.
	All(ctx context.Context) ([]plugins.Status, error)
	// Status is one of them.
	Status(ctx context.Context, name string) (plugins.Status, error)
	// Discover re-reads the plugin directory. It is the rescan button.
	Discover(ctx context.Context) error
	// Enable, Disable and Restart are the three buttons on a row.
	Enable(ctx context.Context, name string) error
	Disable(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
	// Remove uninstalls: it stops the process, drops the plugin's database and
	// forgets its row. It is the only one of these that destroys anything.
	Remove(ctx context.Context, name string) error
	// Spending is what this plugin may spend in a day and what it has spent.
	Spending(ctx context.Context, name string) (plugins.Spending, error)
	// SetDailyCap records what it may spend. Zero is no cap.
	SetDailyCap(ctx context.Context, name string, tokens int64) error
	// InterfaceVersion is the contract this host speaks, shown beside a plugin
	// that was built against another one.
	InterfaceVersion() int
}

// Panel serves the admin pages.
type Panel struct {
	deps      Deps
	templates map[string]*template.Template
	sessions  *Sessions
	logins    *httpx.Limiter
	// prunes owns the one piece of work in this panel that outlives the
	// request that started it, and installs the other.
	prunes   *pruner
	installs *installer
}

// New builds the panel.
func New(d Deps) (*Panel, error) {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OnLogin == nil {
		d.OnLogin = func(string) {}
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Panel{
		deps:      d,
		templates: tmpl,
		sessions:  NewSessions(d.Store, d.Now),
		prunes:    &pruner{},
		installs:  &installer{},
		// Ten attempts, then one every six seconds, per address. A person who
		// has forgotten which password they used is not locked out; a script
		// working through a list is.
		logins: httpx.NewLimiter(1.0/6, 10, 5, 120),
	}, nil
}

// funcMap is the shared helper set plus the panel's own. The panel's one
// addition is the Enterprise line, which is written once here rather than
// copied beside every setting this build does not have — there will be more of
// them, and they should all say the same thing.
func funcMap() template.FuncMap {
	fm := web.FuncMap()
	fm["enterprise"] = func() string { return EnterpriseNote }
	return fm
}

func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"login", "overview", "pairings", "settings", "drivers", "driver", "stint", "devices", "data", "clients", "plugins", "plugin", "pair"}
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcMap()).
			ParseFS(templatesFS, "templates/layout.tmpl", "templates/"+p+".tmpl")
		if err != nil {
			return nil, fmt.Errorf("admin: the %s page does not parse: %w", p, err)
		}
		out[p] = t
	}
	return out, nil
}

// Routes registers the panel on a mux. It does not own the mux, because the
// same server carries the API and the discovery document.
func (p *Panel) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", p.redirectToOverview)
	mux.HandleFunc("GET /admin/{$}", p.redirectToOverview)
	mux.HandleFunc("GET /admin/overview", p.requireSession(p.overview))
	mux.HandleFunc("GET /admin/pairings", p.requireSession(p.getPairings))
	mux.HandleFunc("POST /admin/pairings", p.requireSession(p.postPairingDecision))
	mux.HandleFunc("GET /admin/drivers", p.requireSession(p.getDrivers))
	mux.HandleFunc("GET /admin/drivers/{id}", p.requireSession(p.getDriver))
	mux.HandleFunc("GET /admin/stints/{id}", p.requireSession(p.getStint))
	mux.HandleFunc("GET /admin/devices", p.requireSession(p.getDevices))
	mux.HandleFunc("POST /admin/devices/revoke", p.requireSession(p.postRevokeDevice))
	mux.HandleFunc("POST /admin/devices/revoke-all", p.requireSession(p.postRevokeDriverDevices))
	mux.HandleFunc("POST "+SessionsRevokePath, p.requireSession(p.postRevokeDriverSession))
	mux.HandleFunc("POST "+SessionsRevokePath+"-all", p.requireSession(p.postRevokeDriverSessions))
	mux.HandleFunc("GET /admin/data", p.requireSession(p.getData))
	mux.HandleFunc("POST /admin/data/retention", p.requireSession(p.postRetention))
	mux.HandleFunc("POST /admin/data/prune", p.requireSession(p.postPrune))
	mux.HandleFunc("GET /admin/clients", p.requireSession(p.getClients))
	mux.HandleFunc("POST /admin/clients/build", p.requireSession(p.postBuildClient))
	mux.HandleFunc("GET /admin/clients/download/{reference}", p.requireSession(p.getClientDownload))
	mux.HandleFunc("POST "+CertificatePath, p.requireSession(p.postCertificate))
	mux.HandleFunc("POST "+CertificateRemovePath, p.requireSession(p.postRemoveCertificate))
	mux.HandleFunc("GET /admin/plugins", p.requireSession(p.getPlugins))
	mux.HandleFunc("POST /admin/plugins/rescan", p.requireSession(p.postPluginRescan))
	mux.HandleFunc("POST "+MarketplacePath, p.requireSession(p.postMarketplace))
	mux.HandleFunc("POST "+MarketplaceInstallPath, p.requireSession(p.postMarketplaceInstall))
	mux.HandleFunc("GET /admin/plugins/{name}", p.requireSession(p.getPlugin))
	mux.HandleFunc("POST /admin/plugins/{name}/settings", p.requireSession(p.postPluginSettings))
	mux.HandleFunc("POST /admin/plugins/{name}/cap", p.requireSession(p.postPluginCap))
	mux.HandleFunc("POST /admin/plugins/{name}/action", p.requireSession(p.postPluginAction))
	mux.HandleFunc("GET /admin/settings", p.requireSession(p.getSettings))
	mux.HandleFunc("POST /admin/settings/identity", p.requireSession(p.postIdentity))
	mux.HandleFunc("POST /admin/settings/limits", p.requireSession(p.postLimits))
	mux.HandleFunc("POST /admin/settings/danger", p.requireSession(p.postDanger))
	// The one page here that needs no session: a driver opens it with the code
	// their client printed, and it tells them who approves it. See [PairPath].
	mux.HandleFunc("GET "+PairPath, p.getPair)
	mux.HandleFunc("GET /admin/login", p.getLogin)
	mux.HandleFunc("POST /admin/login", p.postLogin)
	mux.HandleFunc("POST /admin/logout", p.postLogout)
}

func (p *Panel) redirectToOverview(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/overview", http.StatusSeeOther)
}

// navItem is one entry in the panel's navigation. An item with no Href is a
// page that is not in this build: shown, named, and plainly not a link, which
// is more honest than leaving a gap where it will be.
type navItem struct {
	Name    string
	Href    string
	Current bool
}

func navFor(current string) []navItem {
	items := []navItem{
		{Name: "Overview", Href: "/admin/overview"},
		{Name: "Pairing", Href: "/admin/pairings"},
		{Name: "Drivers", Href: DriversPath},
		{Name: "Devices", Href: DevicesPath},
		{Name: "Data", Href: DataPath},
		{Name: "Plugins", Href: PluginsPath},
		{Name: "Settings", Href: SettingsPath},
		{Name: "Build client", Href: ClientsPath},
	}
	for i := range items {
		items[i].Current = items[i].Href != "" && items[i].Name == current
	}
	return items
}

// pageData is what every admin template is given.
type pageData struct {
	Title        string
	Organisation string
	Version      string
	SignedIn     bool
	Email        string
	CSRF         string
	Nav          []navItem
	Error        string
	Notice       string
	Status       int
	Form         any
	// Refresh, when positive, is how many seconds the browser waits before
	// loading this page again. It is set only while a background job is
	// running, so that its progress moves without the operator pressing
	// anything — and it is a meta refresh rather than a script, because the
	// panel's content security policy allows no script and should not have to.
	Refresh int
}

func (p *Panel) render(w http.ResponseWriter, r *http.Request, page string, data pageData) {
	t, ok := p.templates[page]
	if !ok {
		httpx.Problem(w, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}
	data.Version = p.deps.Version
	if data.Organisation == "" {
		data.Organisation = p.organisationName(r.Context())
	}
	if data.Status == 0 {
		data.Status = http.StatusOK
	}
	// Rendered into a buffer and only then written, so that a template which
	// fails part-way through is a clean 500 rather than half a page with 200 on
	// it. Writing the status first — which is what this used to do — meant a
	// broken template produced a page that stopped mid-sentence and told both
	// the browser and every test that it had worked. A page is a few kilobytes;
	// holding one is not a cost worth that.
	var out bytes.Buffer
	if err := t.ExecuteTemplate(&out, "layout", data); err != nil {
		p.deps.Log.LogAttrs(r.Context(), slog.LevelError, "admin page did not render",
			slog.String("page", page), slog.Any("error", err))
		httpx.Problem(w, r, http.StatusInternalServerError, "Something went wrong on the server. Try again.")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(data.Status)
	_, _ = out.WriteTo(w)
}

// organisationName is the name in the header. A database that will not answer
// must not take the sign-in page with it, so a failure is a fallback rather
// than an error page.
func (p *Panel) organisationName(ctx context.Context) string {
	s, err := p.deps.Settings(ctx)
	if err != nil || s.Organisation == "" {
		return "Pacenote"
	}
	return s.Organisation
}
