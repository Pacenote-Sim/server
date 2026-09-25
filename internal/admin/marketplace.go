package admin

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/marketplace"
)

// MarketplacePath is the form on the plugins page that turns the marketplace
// on and off and refreshes it; MarketplaceInstallPath installs one plugin.
const (
	MarketplacePath        = "/admin/plugins/marketplace"
	MarketplaceInstallPath = "/admin/plugins/install"
)

// installRefreshSeconds is how often the plugins page reloads itself while a
// package is being downloaded and unpacked.
const installRefreshSeconds = 2

// Marketplace is what the panel needs from the marketplace client. It is an
// interface for the same reason [Plugins] is: the pages are tested against a
// fake, and this package should not know how an index is fetched.
type Marketplace interface {
	// Snapshot is the last good index and how the last refresh went.
	Snapshot() marketplace.Snapshot
	// Refresh fetches the index now. It is the button, and what turning the
	// marketplace on does first.
	Refresh(ctx context.Context) error
	// Available is every server plugin with a package for this machine.
	Available() []marketplace.Plugin
	// Install downloads, verifies and unpacks one of them.
	Install(ctx context.Context, name string) (marketplace.Installed, error)
	// Withdrawn says whether the index has withdrawn an installed version.
	Withdrawn(name, version string) (marketplace.Version, bool)
}

// marketRow is one available plugin as the page renders it.
type marketRow struct {
	Name    string
	Title   string
	Summary string
	Author  string
	Tag     string
	Licence string
	Paid    bool
	Calls   []string
	// Installed is the version on this server, empty when it is not here.
	Installed string
	// Update is whether the index has a newer approved version than the one
	// installed.
	Update bool
}

// marketForm is the marketplace card on the plugins page.
type marketForm struct {
	// Available is a server with a marketplace client at all.
	Available bool
	// On is the operator's switch.
	On bool
	// Fetched is when the index was last read, and Err why the last attempt
	// failed if it did.
	Fetched moment
	Err     string
	Rows    []marketRow
	// Installing is the install in flight or the last one, for the notice.
	Installing InstallProgress
}

// InstallProgress reports what an install is doing right now, or what the
// last one in this process did.
type InstallProgress struct {
	Running   bool
	Name      string
	StartedAt time.Time
	// Tag is what was installed, once it is done.
	Tag string
	// Replaced is whether a version was already there.
	Replaced bool
	// Err is why it failed, empty when it did not.
	Err string
	// Restarted is whether the running plugin was restarted on the new
	// version, and RestartErr why it was not.
	Restarted  bool
	RestartErr string
}

// installer owns the one install at a time this panel runs. Like the pruner,
// it is a job that outlives a request: a package is tens of megabytes and the
// page follows it rather than holding a connection open.
type installer struct {
	mu    sync.Mutex
	state InstallProgress
}

func (i *installer) begin(name string, now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state.Running {
		return false
	}
	i.state = InstallProgress{Running: true, Name: name, StartedAt: now}
	return true
}

func (i *installer) finish(done marketplace.Installed, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.state.Running = false
	i.state.Tag = done.Tag
	i.state.Replaced = done.Replaced
	if err != nil {
		i.state.Err = err.Error()
	}
}

func (i *installer) restarted(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		i.state.RestartErr = err.Error()
		return
	}
	i.state.Restarted = true
}

func (i *installer) snapshot() InstallProgress {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.state
}

// InstallProgress is the install in flight, for a caller shutting down or a
// test that must not tear down the plugin directory underneath one.
func (p *Panel) InstallProgress() InstallProgress { return p.installs.snapshot() }

// marketFormFor builds the card from the client, the settings and what is
// installed. It never fails: a marketplace that cannot be read is a card that
// says so.
func (p *Panel) marketFormFor(ctx context.Context, rows []pluginRow) marketForm {
	form := marketForm{Installing: p.installs.snapshot()}
	if p.deps.Marketplace == nil {
		return form
	}
	form.Available = true
	if p.deps.Settings != nil {
		if s, err := p.deps.Settings(ctx); err == nil {
			form.On = s.Marketplace
		}
	}
	snap := p.deps.Marketplace.Snapshot()
	form.Fetched = momentAt(p.now(), snap.FetchedAt)
	form.Err = snap.Err

	installed := map[string]string{}
	for _, r := range rows {
		if r.Installed {
			installed[r.Name] = r.Version
		}
	}
	for _, pl := range p.deps.Marketplace.Available() {
		v := pl.Latest()
		row := marketRow{
			Name: pl.Name, Title: pl.Title, Summary: pl.Summary, Author: pl.Author, Tag: v.Tag,
			Licence: pl.Licence, Paid: pl.Pricing == "paid", Calls: pl.Calls, Installed: installed[pl.Name],
		}
		if row.Installed != "" && "v"+strings.TrimPrefix(row.Installed, "v") != v.Tag {
			row.Update = true
		}
		form.Rows = append(form.Rows, row)
	}
	sort.Slice(form.Rows, func(i, j int) bool { return form.Rows[i].Title < form.Rows[j].Title })
	return form
}

// withdrawnNote is the warning on a plugin row whose installed version the
// index has withdrawn.
func (p *Panel) withdrawnNote(name, version string) string {
	if p.deps.Marketplace == nil || version == "" {
		return ""
	}
	v, gone := p.deps.Marketplace.Withdrawn(name, version)
	if !gone {
		return ""
	}
	note := "The marketplace has withdrawn this version."
	if v.Notes != "" {
		note += " " + v.Notes
	}
	return note
}

// postMarketplace is the card's three buttons: on, off, refresh.
func (p *Panel) postMarketplace(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if p.deps.Marketplace == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server has no marketplace client.")
		return
	}
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()
	switch r.PostFormValue("action") {
	case "on", "off":
		on := r.PostFormValue("action") == "on"
		if p.deps.Store == nil {
			p.answerPlugins(w, r, "", "The setting could not be saved. There is no database.", http.StatusInternalServerError)
			return
		}
		current, err := p.deps.Store.Settings(ctx)
		if err != nil {
			p.answerPlugins(w, r, "", "The settings could not be read. The database did not answer.", http.StatusInternalServerError)
			return
		}
		if current.Marketplace != on {
			current.Marketplace = on
			if err := p.deps.Store.SaveSettings(ctx, current); err != nil {
				p.answerPlugins(w, r, "", "The setting could not be saved. Try again.", http.StatusInternalServerError)
				return
			}
			if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionMarketplaceChanged, "marketplace", []string{r.PostFormValue("action")}); err != nil {
				p.deps.Log.WarnContext(ctx, "the marketplace change could not be audited", "error", err)
			}
			if p.deps.OnSettingsChanged != nil {
				p.deps.OnSettingsChanged()
			}
		}
		if !on {
			p.answerPlugins(w, r, "The marketplace is off. This server will not go online again until it is turned on.", "", http.StatusOK)
			return
		}
		if err := p.deps.Marketplace.Refresh(ctx); err != nil {
			p.answerPlugins(w, r, "The marketplace is on.", "The index could not be read: "+err.Error(), http.StatusOK)
			return
		}
		p.answerPlugins(w, r, "The marketplace is on, and the list of plugins is current.", "", http.StatusOK)
	case "refresh":
		if err := p.deps.Marketplace.Refresh(ctx); err != nil {
			p.answerPlugins(w, r, "", "The index could not be read: "+err.Error(), http.StatusBadGateway)
			return
		}
		p.answerPlugins(w, r, "The list of plugins is current.", "", http.StatusOK)
	default:
		httpx.Problem(w, r, http.StatusBadRequest, "That is not something this page can do.")
	}
}

// postMarketplaceInstall starts an install and answers with the page, which
// follows the job until it is done.
func (p *Panel) postMarketplaceInstall(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if p.deps.Marketplace == nil || p.deps.Plugins == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server has no marketplace client.")
		return
	}
	if !p.beginForm(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	var wanted bool
	for _, pl := range p.deps.Marketplace.Available() {
		if pl.Name == name {
			wanted = true
		}
	}
	if !wanted {
		p.answerPlugins(w, r, "", "That plugin is not in the marketplace for this machine. Refresh the list and try again.", http.StatusBadRequest)
		return
	}
	if !p.installs.begin(name, p.now()) {
		p.answerPlugins(w, r, "", "Another plugin is being installed. Wait for it to finish.", http.StatusConflict)
		return
	}
	go p.runInstall(context.WithoutCancel(r.Context()), sess.Email, name)
	p.answerPlugins(w, r, "Installing "+name+"…", "", http.StatusOK)
}

// runInstall is the job: download and unpack, then tell the host, then restart
// the plugin if a running one was replaced.
func (p *Panel) runInstall(ctx context.Context, actor, name string) {
	done, err := p.deps.Marketplace.Install(ctx, name)
	p.installs.finish(done, err)
	if err != nil {
		p.deps.Log.WarnContext(ctx, "a plugin could not be installed from the marketplace", "plugin", name, "error", err)
		return
	}
	if p.deps.Store != nil {
		if err := p.deps.Store.WriteAudit(ctx, actor, db.ActionPluginInstalled, name, []string{done.Tag}); err != nil {
			p.deps.Log.WarnContext(ctx, "the install could not be audited", "plugin", name, "error", err)
		}
	}
	if err := p.deps.Plugins.Discover(ctx); err != nil {
		p.installs.restarted(err)
		return
	}
	if !done.Replaced {
		return
	}
	// A replaced plugin that was running is still running the old binary:
	// discovery leaves a working plugin alone. Restart it onto the new one.
	st, err := p.deps.Plugins.Status(ctx, name)
	if err != nil || !st.Enabled {
		return
	}
	p.installs.restarted(p.deps.Plugins.Restart(ctx, name))
}

// now is the clock.
func (p *Panel) now() time.Time {
	if p.deps.Now != nil {
		return p.deps.Now()
	}
	return time.Now()
}
