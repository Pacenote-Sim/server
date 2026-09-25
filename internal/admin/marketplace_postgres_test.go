//go:build postgres

package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/marketplace"
	"github.com/pacenote-sim/server/internal/plugins"
)

// fakeMarketplace stands in for the client: what the page renders, what it
// refuses and what it calls are the questions, not how an index is fetched.
type fakeMarketplace struct {
	mu        sync.Mutex
	snap      marketplace.Snapshot
	avail     []marketplace.Plugin
	withdrawn map[string]marketplace.Version
	refreshes int
	installed []string
	// failRefresh and failInstall are the two failures the page renders.
	failRefresh error
	failInstall error
	// replaced makes Install report that a version was already there.
	replaced bool
	// block holds Install until closed, for the one-at-a-time rule.
	block chan struct{}
}

func (f *fakeMarketplace) Snapshot() marketplace.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeMarketplace) Refresh(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	if f.failRefresh != nil {
		f.snap.Err = f.failRefresh.Error()
		return f.failRefresh
	}
	f.snap.Err = ""
	f.snap.FetchedAt = time.Now()
	return nil
}

func (f *fakeMarketplace) Available() []marketplace.Plugin {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]marketplace.Plugin(nil), f.avail...)
}

func (f *fakeMarketplace) Install(_ context.Context, name string) (marketplace.Installed, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInstall != nil {
		return marketplace.Installed{}, f.failInstall
	}
	f.installed = append(f.installed, name)
	return marketplace.Installed{Name: name, Tag: "v0.2.0", Replaced: f.replaced}, nil
}

func (f *fakeMarketplace) Withdrawn(name, version string) (marketplace.Version, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.withdrawn[name+"@"+version]
	return v, ok
}

func voicePlugin() marketplace.Plugin {
	return marketplace.Plugin{
		Name: "voice", Kind: marketplace.KindServer, Title: "Voice", Summary: "Speaks the coach's lines.",
		Author: "Pacenote", Licence: "GPL-3.0", Pricing: "free", Calls: []string{"api.cartesia.ai"},
		Versions: []marketplace.Version{{
			Tag: "v0.2.0", Approved: "2026-09-25", InterfaceVersion: 3, Status: marketplace.StatusApproved,
			Artifacts: []marketplace.Artifact{{OS: "linux", Arch: "amd64", URL: "https://example.com/voice.zip", SHA256: "ab"}},
		}},
	}
}

func withMarketplace(f *fakeMarketplace) func(*admin.Deps) {
	return func(d *admin.Deps) { d.Marketplace = f }
}

// turnOn posts the switch and returns the page it answered with.
func (p *panel) turnOn() string {
	p.t.Helper()
	page := p.get(admin.PluginsPath)
	require.Equal(p.t, http.StatusOK, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(p.t, page)}, "action": {"on"}}))
	return p.answered()
}

func TestMarketplaceIsOffUntilTurnedOn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	m := &fakeMarketplace{avail: []marketplace.Plugin{voicePlugin()}}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)

	body := p.get(admin.PluginsPath)
	r.Contains(body, "Turn the marketplace on")
	r.Contains(body, "has never gone online")
	r.NotContains(body, "Speaks the coach", "nothing is listed while it is off")
	r.Zero(m.refreshes, "and nothing was fetched")

	body = p.turnOn()
	r.Contains(body, "The marketplace is on")
	r.Equal(1, m.refreshes, "turning it on reads the list at once")
	settings, err := p.store.Settings(context.Background())
	r.NoError(err)
	r.True(settings.Marketplace, "the switch is a setting, so it survives a restart")

	r.Contains(body, "Voice")
	r.Contains(body, "Speaks the coach")
	r.Contains(body, "api.cartesia.ai")
	r.Contains(body, `value="voice"`, "an install button")
	r.Contains(body, "Turn the marketplace off")

	// Off again: the setting flips and the list is gone.
	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusOK, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"off"}}))
	r.Contains(p.answered(), "The marketplace is off")
	settings, err = p.store.Settings(context.Background())
	r.NoError(err)
	r.False(settings.Marketplace)
	r.NotContains(p.answered(), `value="voice"`)
}

func TestMarketplaceRowsSayWhatIsInstalled(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	installed := coachPlugin()
	installed.Name, installed.Version = "voice", "0.1.0"
	current := coachPlugin()
	current.Name, current.Version = "engineer", "0.2.0"
	f := &fakePlugins{all: []plugins.Status{installed, current}}
	eng := voicePlugin()
	eng.Name, eng.Title = "engineer", "Race engineer"
	m := &fakeMarketplace{
		avail:     []marketplace.Plugin{voicePlugin(), eng},
		withdrawn: map[string]marketplace.Version{"voice@0.1.0": {Tag: "v0.1.0", Status: marketplace.StatusWithdrawn, Notes: "It leaked keys."}},
	}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)

	body := p.turnOn()
	r.Contains(body, "Update from 0.1.0", "an older version installed offers the newer one")
	r.Contains(body, ">installed<", "the current version is marked and has no button")
	r.Contains(body, "The marketplace has withdrawn this version. It leaked keys.")

	one := p.get(admin.PluginsPath + "/voice")
	r.Contains(one, "Withdrawn")
	r.Contains(one, "It leaked keys.")
}

func TestMarketplaceRefresh(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	m := &fakeMarketplace{avail: []marketplace.Plugin{voicePlugin()}}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)
	p.turnOn()

	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusOK, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"refresh"}}))
	r.Contains(p.answered(), "The list of plugins is current.")
	r.Equal(2, m.refreshes)

	m.mu.Lock()
	m.failRefresh = errors.New("pacenote.tech answered 503")
	m.mu.Unlock()
	r.Equal(http.StatusBadGateway, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"refresh"}}))
	r.Contains(p.answered(), "answered 503")
	r.Contains(p.get(admin.PluginsPath), "The last attempt failed: pacenote.tech answered 503", "the card keeps saying so")

	// Turning it on while the index cannot be read is still turning it on.
	r.Equal(http.StatusOK, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"off"}}))
	r.Equal(http.StatusOK, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"on"}}))
	r.Contains(p.answered(), "The marketplace is on.")
	r.Contains(p.answered(), "answered 503")

	r.Equal(http.StatusBadRequest, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"dance"}}))
	r.Equal(http.StatusForbidden, p.post(admin.MarketplacePath, url.Values{"action": {"refresh"}}), "no token, no action")
}

func TestMarketplaceInstall(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	m := &fakeMarketplace{avail: []marketplace.Plugin{voicePlugin()}}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)
	p.turnOn()

	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusOK, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"voice"}}))
	r.Contains(p.answered(), "Installing voice")
	r.Eventually(func() bool { return !p.object.InstallProgress().Running }, 5*time.Second, 10*time.Millisecond)
	done := p.object.InstallProgress()
	r.Empty(done.Err)
	r.Equal("v0.2.0", done.Tag)
	r.Equal([]string{"voice"}, m.installed)
	r.Equal(1, f.rescanCount, "the host is told to look again")
	r.Empty(f.actions(), "a new plugin is not restarted; discovery starts it")
	body := p.get(admin.PluginsPath)
	r.Contains(body, "voice v0.2.0 is installed")

	// A name the marketplace does not offer.
	r.Equal(http.StatusBadRequest, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"nothing"}}))
	r.Contains(p.answered(), "not in the marketplace for this machine")
}

func TestMarketplaceInstallReplacesAndRestarts(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	running := coachPlugin()
	running.Name, running.Version = "voice", "0.1.0"
	f := &fakePlugins{all: []plugins.Status{running}}
	m := &fakeMarketplace{avail: []marketplace.Plugin{voicePlugin()}, replaced: true}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)
	p.turnOn()

	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusOK, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"voice"}}))
	r.Eventually(func() bool { return !p.object.InstallProgress().Running }, 5*time.Second, 10*time.Millisecond)
	r.Eventually(func() bool { return len(f.actions()) == 1 }, 5*time.Second, 10*time.Millisecond)
	r.Equal([]string{"restart:voice"}, f.actions(), "a replaced running plugin is restarted onto the new version")
	r.Contains(p.get(admin.PluginsPath), "running the new version")
}

func TestMarketplaceInstallFailuresAreShown(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	m := &fakeMarketplace{avail: []marketplace.Plugin{voicePlugin()}, failInstall: errors.New("the package does not match the index")}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)
	p.turnOn()

	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusOK, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"voice"}}))
	r.Eventually(func() bool { return !p.object.InstallProgress().Running }, 5*time.Second, 10*time.Millisecond)
	r.Contains(p.get(admin.PluginsPath), "voice could not be installed: the package does not match the index")
	r.Zero(f.rescanCount, "nothing to look for")
}

func TestMarketplaceInstallsOneAtATime(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	m := &fakeMarketplace{avail: []marketplace.Plugin{voicePlugin()}, block: make(chan struct{})}
	p := newPanel(t, withPlugins(f), withMarketplace(m))
	p.seedPluginRows(f.all)
	p.turnOn()

	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusOK, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"voice"}}))
	r.True(p.object.InstallProgress().Running)
	r.Contains(p.get(admin.PluginsPath), `http-equiv="refresh"`, "the page follows the job")
	r.Equal(http.StatusConflict, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"voice"}}))
	r.Contains(p.answered(), "Another plugin is being installed")
	close(m.block)
	r.Eventually(func() bool { return !p.object.InstallProgress().Running }, 5*time.Second, 10*time.Millisecond)
}

func TestMarketplaceOnAServerWithoutOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	r.Contains(p.get(admin.PluginsPath), "This build has no marketplace client.")
	page := p.get(admin.PluginsPath)
	r.Equal(http.StatusNotFound, p.post(admin.MarketplacePath, url.Values{"csrf": {csrfOf(t, page)}, "action": {"on"}}))
	r.Equal(http.StatusNotFound, p.post(admin.MarketplaceInstallPath, url.Values{"csrf": {csrfOf(t, page)}, "name": {"voice"}}))
}
