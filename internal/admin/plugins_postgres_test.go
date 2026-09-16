//go:build postgres

package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/plugins"
)

// The panel's plugin pages are tested against a fake host rather than real
// plugin processes. Running one per assertion would make this file take a minute
// and would be testing the host, which has its own tests — what is being tested
// here is the page: what it renders, what it refuses, and what it calls.

type fakePlugins struct {
	mu   sync.Mutex
	all  []plugins.Status
	acts []string
	// The failures a page has to render rather than panic on.
	failAll     error
	failStatus  error
	failAction  error
	failRescan  error
	version     int
	rescanCount int
	// failStatusAfter makes Status start failing once it has answered this
	// many times, for the page that has to re-render after an action against a
	// plugin that went away while the form was open.
	failStatusAfter int
	statusCalls     int
	// caps is what each plugin may spend, and the two failures the page has to
	// render rather than break on.
	caps         map[string]int64
	failSpending error
	failSetCap   error
}

func (f *fakePlugins) All(context.Context) ([]plugins.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, f.failAll
	}
	return append([]plugins.Status(nil), f.all...), nil
}

func (f *fakePlugins) Status(_ context.Context, name string) (plugins.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.failStatus != nil {
		return plugins.Status{}, f.failStatus
	}
	if f.failStatusAfter > 0 && f.statusCalls > f.failStatusAfter {
		return plugins.Status{}, db.ErrNotFound
	}
	for _, s := range f.all {
		if s.Name == name {
			return s, nil
		}
	}
	return plugins.Status{}, db.ErrNotFound
}

func (f *fakePlugins) Discover(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescanCount++
	return f.failRescan
}

func (f *fakePlugins) InterfaceVersion() int {
	if f.version == 0 {
		return plugin.InterfaceVersion
	}
	return f.version
}

func (f *fakePlugins) act(verb, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAction != nil {
		return f.failAction
	}
	found := false
	for i := range f.all {
		if f.all[i].Name == name {
			found = true
			switch verb {
			case "enable":
				f.all[i].Enabled = true
				f.all[i].State = plugins.StateRunning
			case "disable":
				f.all[i].Enabled = false
				f.all[i].State = plugins.StateDisabled
			case "remove":
				f.all = append(f.all[:i], f.all[i+1:]...)
			}
			break
		}
	}
	if !found {
		return db.ErrNotFound
	}
	f.acts = append(f.acts, verb+":"+name)
	return nil
}

func (f *fakePlugins) Enable(_ context.Context, n string) error  { return f.act("enable", n) }
func (f *fakePlugins) Disable(_ context.Context, n string) error { return f.act("disable", n) }
func (f *fakePlugins) Restart(_ context.Context, n string) error { return f.act("restart", n) }
func (f *fakePlugins) Remove(_ context.Context, n string) error  { return f.act("remove", n) }

func (f *fakePlugins) Spending(_ context.Context, name string) (plugins.Spending, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSpending != nil {
		return plugins.Spending{}, f.failSpending
	}
	return plugins.Spending{
		Cap:     f.caps[name],
		Today:   db.TokenUse{Calls: 3, Input: 1_200, Output: 300},
		Month:   db.TokenUse{Calls: 40, Input: 18_000, Output: 4_000},
		OverCap: f.caps[name] > 0 && 1_500 >= f.caps[name],
	}, nil
}

func (f *fakePlugins) SetDailyCap(_ context.Context, name string, tokens int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSetCap != nil {
		return f.failSetCap
	}
	found := false
	for _, s := range f.all {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		return db.ErrNotFound
	}
	if f.caps == nil {
		f.caps = map[string]int64{}
	}
	f.caps[name] = tokens
	return nil
}

func (f *fakePlugins) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acts...)
}

// coachPlugin is a plugin declaring one of every kind of setting, which is what
// the form has to render.
func coachPlugin() plugins.Status {
	return plugins.Status{
		Name:             "engineer",
		Version:          "1.0.0",
		Author:           "Pacenote",
		Description:      "Turns a lap into something worth hearing.",
		InterfaceVersion: plugin.InterfaceVersion,
		State:            plugins.StateRunning,
		Enabled:          true,
		Installed:        true,
		Capabilities: plugin.Capabilities{
			Events:          []plugin.EventKind{plugin.EventLapCompleted},
			Requests:        []plugin.RequestKind{"engineer.debrief"},
			Network:         true,
			ReadsDriverData: true,
			Database:        true,
		},
		Settings: []plugin.Setting{
			{
				Name: "api_key", Label: "Anthropic key", Kind: plugin.KindSecret, Required: true,
				Help: "Yours, from console.anthropic.com. It is sealed with this server's data key.",
			},
			{
				Name: "model", Label: "Model", Kind: plugin.KindChoice, Default: "claude-opus-5",
				Choices: []plugin.Choice{
					{Value: "claude-opus-5", Label: "Claude Opus 5", Note: "the most capable"},
					{Value: "claude-sonnet-5", Label: "Claude Sonnet 5"},
				},
			},
			{Name: "greeting", Label: "Greeting", Kind: plugin.KindText, Placeholder: "Right"},
			{Name: "max_words", Label: "Words per cue", Kind: plugin.KindNumber, Default: "20"},
			{Name: "debriefs", Label: "Write a debrief", Kind: plugin.KindBool, Default: "true"},
		},
	}
}

// seedPluginRows writes the plugins table rows the fake host implies. The real
// host writes them when it finds a plugin on disk, and plugin_settings has a
// foreign key to them, so a fake that skipped this would make every save fail
// for a reason that has nothing to do with the page.
func (p *panel) seedPluginRows(all []plugins.Status) {
	p.t.Helper()
	for _, s := range all {
		require.NoError(p.t, p.store.SavePlugin(context.Background(), db.PluginRecord{
			Name:             s.Name,
			Version:          s.Version,
			Author:           s.Author,
			Description:      s.Description,
			InterfaceVersion: s.InterfaceVersion,
			Directory:        s.Name,
			State:            string(s.State),
		}))
	}
}

// answered is what the last post rendered. These pages answer in place rather
// than redirecting, the way every other page in this panel does, so the reply to
// a form is the body of the post itself.
func (p *panel) answered() string { return string(p.lastBody) }

func withPlugins(f *fakePlugins) func(*admin.Deps) {
	return func(d *admin.Deps) {
		d.Plugins = f
		d.PluginDir = "/srv/pacenote/data/plugins"
		// A data key, because a plugin's credential is sealed with one and a
		// server without one refuses to store any. The refusal has a test of
		// its own below.
		key, err := auth.NewSecretKey()
		if err != nil {
			panic(err)
		}
		d.Keyring = auth.NewKeyring(key)
	}
}

// A server whose configuration file carries no data key cannot store a
// credential, and says so rather than writing one in clear to get past it.
func TestASecretIsRefusedWithoutADataKey(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, func(d *admin.Deps) {
		d.Plugins = f
		d.PluginDir = "/srv/pacenote/data/plugins"
		d.Keyring = nil
	})
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "api_key": {"sk-ant-thekeynobodymayeversee"}, "max_words": {"20"},
	}))
	r.Contains(p.answered(), "no data key")

	rows, err := p.store.PluginSettings(context.Background(), "engineer")
	r.NoError(err)
	for _, row := range rows {
		r.NotEqual("api_key", row.Name, "a credential was stored with no key to seal it")
	}
}

func TestPluginsPageListsWhatIsInstalled(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	broken := plugins.Status{
		Name: "fromthefuture", Version: "2.0.0", Author: "Someone",
		Description:      "Built against a contract this server does not speak.",
		InterfaceVersion: plugin.InterfaceVersion + 1,
		State:            plugins.StateFailed, Enabled: true, Installed: true,
		LastError: "it was built for plugin interface 2",
	}
	gone := plugins.Status{
		Name: "vanished", Version: "0.9.0", Author: "Someone",
		Description:      "Its folder is no longer there.",
		InterfaceVersion: plugin.InterfaceVersion,
		State:            plugins.StateStopped, Enabled: true, Installed: false,
	}
	f := &fakePlugins{all: []plugins.Status{coachPlugin(), broken, gone}}

	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	body := p.get(admin.PluginsPath)

	r.Contains(body, "engineer")
	r.Contains(body, "Turns a lap into something worth hearing.")
	r.Contains(body, "Running")
	// The capability sentences come from the plugin module so that every host
	// words them the same way.
	r.Contains(body, "Calls something outside this machine.")
	r.Contains(body, "Keeps tables of its own")

	r.Contains(body, "fromthefuture")
	r.Contains(body, "Failed")
	r.Contains(body, "built for interface")

	r.Contains(body, "vanished")
	r.Contains(body, "its files are gone")

	// The nav carries the page, or an operator cannot find it.
	r.Contains(body, `href="/admin/plugins"`)
}

func TestPluginsPageEmptyStateSaysWhereToPutOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := newPanel(t, withPlugins(&fakePlugins{}))
	body := p.get(admin.PluginsPath)

	r.Contains(body, "No plugins yet")
	r.Contains(body, "/srv/pacenote/data/plugins",
		"the empty state does not say where a plugin goes")
	r.Contains(body, "Look for new plugins")
}

func TestPluginsPageOnAServerWithNoHost(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// No Plugins dependency at all. The page exists and says so rather than
	// being absent, because a missing menu entry is a thing nobody can ask a
	// question about.
	p := newPanel(t)
	body := p.get(admin.PluginsPath)
	r.Contains(body, "not running plugins")

	status, _ := p.getStatus(admin.PluginsPath + "/engineer")
	r.Equal(http.StatusNotFound, status)
}

func TestPluginsPageSurvivesADatabaseThatWillNotAnswer(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{failAll: errors.New("the connection is gone")}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	status, body := p.getStatus(admin.PluginsPath)
	r.Equal(http.StatusInternalServerError, status)
	r.Contains(body, "could not be read")
	// The operator's own words, not the driver's error.
	r.NotContains(body, "the connection is gone")
}

func TestPluginPageRendersEveryKindOfSetting(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	body := p.get(admin.PluginsPath + "/engineer")

	r.Contains(body, `type="password"`, "the credential is not a password field")
	r.Contains(body, "Anthropic key")
	r.Contains(body, "<select")
	r.Contains(body, "Claude Opus 5")
	r.Contains(body, `type="number"`)
	r.Contains(body, `type="checkbox"`)
	r.Contains(body, "Right", "the placeholder was not rendered")

	// A plugin that keeps tables says so where it matters most: beside the
	// button that destroys them.
	r.Contains(body, "drops the tables it kept")
}

func TestPluginPageIsNotFoundForAPluginThatIsNotThere(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := newPanel(t, withPlugins(&fakePlugins{}))
	status, _ := p.getStatus(admin.PluginsPath + "/neverheardof")
	r.Equal(http.StatusNotFound, status)
}

func TestSavingPluginSettingsStoresWhatWasTyped(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	body := p.get(admin.PluginsPath + "/engineer")

	status := p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf":      {csrfOf(t, body)},
		"api_key":   {"sk-ant-thekeynobodymayeversee"},
		"model":     {"claude-sonnet-5"},
		"greeting":  {"Right"},
		"max_words": {"14"},
		"debriefs":  {"true"},
	})
	r.Equal(http.StatusOK, status, "the page said: %s", p.answered())
	r.Contains(p.answered(), "That was saved.")

	rows, err := p.store.PluginSettings(context.Background(), "engineer")
	r.NoError(err)
	got := map[string]db.PluginSettingRow{}
	for _, row := range rows {
		got[row.Name] = row
	}

	r.Equal("claude-sonnet-5", got["model"].Value)
	r.Equal("Right", got["greeting"].Value)
	r.Equal("14", got["max_words"].Value)
	r.Equal("true", got["debriefs"].Value)

	// The credential is sealed, and its plaintext is nowhere in the row.
	r.NotEmpty(got["api_key"].Sealed)
	r.Empty(got["api_key"].Value, "a credential was stored in clear")
	r.NotContains(string(got["api_key"].Sealed), "sk-ant-")

	// And the page shows that one is stored without showing it.
	after := p.get(admin.PluginsPath + "/engineer")
	r.NotContains(p.answered(), "sk-ant-thekeynobodymayeversee",
		"the credential came back in the reply to the form that set it")
	r.NotContains(after, "sk-ant-thekeynobodymayeversee")
	r.Contains(after, "ending rsee",
		"the last four characters are shown so an operator can tell which key they used")

	// Saving restarts the plugin, so it is holding what was just saved.
	r.Contains(f.actions(), "restart:engineer")
}

// The credential field is never rendered with a value in it, so an empty box is
// an operator who did not touch it — not one asking for it to be cleared.
func TestAnEmptySecretFieldLeavesTheStoredOneAlone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	body := p.get(admin.PluginsPath + "/engineer")
	csrf := csrfOf(t, body)

	p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "api_key": {"sk-ant-thekeynobodymayeversee"},
		"model": {"claude-opus-5"}, "max_words": {"20"},
	})

	// A second save that changes only the model, with the credential box empty
	// the way the form renders it.
	p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "api_key": {""},
		"model": {"claude-sonnet-5"}, "max_words": {"20"},
	})

	rows, err := p.store.PluginSettings(context.Background(), "engineer")
	r.NoError(err)
	var sealed []byte
	var model string
	for _, row := range rows {
		switch row.Name {
		case "api_key":
			sealed = row.Sealed
		case "model":
			model = row.Value
		}
	}
	r.NotEmpty(sealed, "an empty box cleared a stored credential")
	r.Equal("claude-sonnet-5", model)
}

func TestRemovingAStoredSecret(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "api_key": {"sk-ant-thekeynobodymayeversee"}, "max_words": {"20"},
	})
	p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "remove_api_key": {"1"}, "max_words": {"20"},
	})

	rows, err := p.store.PluginSettings(context.Background(), "engineer")
	r.NoError(err)
	for _, row := range rows {
		r.NotEqual("api_key", row.Name, "the credential was not removed")
	}
}

// Validation is the plugin module's, so a value the panel accepts is one the
// plugin would accept. A number field given a word has to be refused here
// rather than by the plugin at three in the morning.
func TestSavingRefusesAValueThePluginWouldNot(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "max_words": {"lots"},
	}))
	r.Contains(p.answered(), "not something this plugin accepts")

	// And a choice that is not one of the choices.
	r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "model": {"gpt-9"}, "max_words": {"20"},
	}))
	r.Contains(p.answered(), "not something this plugin accepts")
}

func TestPluginActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		action string
		want   string
	}{
		{"disable", "disable:engineer"},
		{"restart", "restart:engineer"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
			p := newPanel(t, withPlugins(f))
			p.seedPluginRows(f.all)
			csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

			r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/action", url.Values{
				"csrf": {csrf}, "action": {tc.action},
			}))
			r.Contains(f.actions(), tc.want)
		})
	}

	t.Run("an action this page does not do", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
		p := newPanel(t, withPlugins(f))
		p.seedPluginRows(f.all)
		csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

		r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/action", url.Values{
			"csrf": {csrf}, "action": {"setfire"},
		}))
		r.Empty(f.actions())
	})
}

// Remove is the only action that destroys anything, so it is the only one that
// asks first — and the checkbox is checked on the server, not just in the form.
func TestRemoveNeedsTheBoxTicked(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/action", url.Values{
		"csrf": {csrf}, "action": {"remove"},
	}))
	r.Empty(f.actions(), "a plugin was removed without the box being ticked")
	r.Contains(p.answered(), "has to be ticked")

	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/action", url.Values{
		"csrf": {csrf}, "action": {"remove"}, "confirm": {"1"},
	}))
	r.Contains(f.actions(), "remove:engineer")
	// The reply is the list, not a page about a plugin that is no longer there.
	r.Contains(p.answered(), "was removed")
	r.NotContains(p.get(admin.PluginsPath), "engineer")
}

func TestRescanReadsTheDirectoryAgain(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath))

	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/rescan", url.Values{"csrf": {csrf}}))
	r.Equal(1, f.rescanCount)
	r.Contains(p.answered(), "read again")

	f.failRescan = errors.New("the plugin directory is not readable")
	r.Equal(http.StatusInternalServerError,
		p.post(admin.PluginsPath+"/rescan", url.Values{"csrf": {csrf}}))
	r.Contains(p.answered(), "could not be read")
}

// Every form on these pages carries a token, and every post checks it. A page
// that changes what the server runs is exactly the page worth being sure about.
func TestPluginFormsRefuseAStaleToken(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	_ = p.get(admin.PluginsPath + "/engineer")

	for _, path := range []string{
		admin.PluginsPath + "/rescan",
		admin.PluginsPath + "/engineer/settings",
		admin.PluginsPath + "/engineer/action",
	} {
		r.Equal(http.StatusForbidden,
			p.post(path, url.Values{"csrf": {"not-the-token"}, "action": {"disable"}}),
			"%s accepted a stale token", path)
	}
	r.Empty(f.actions())
	r.Zero(f.rescanCount)
}

func TestPluginActionOnAPluginThatIsNotThere(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusNotFound, p.post(admin.PluginsPath+"/ghost/action", url.Values{
		"csrf": {csrf}, "action": {"restart"},
	}))
	r.Equal(http.StatusNotFound, p.post(admin.PluginsPath+"/ghost/settings", url.Values{
		"csrf": {csrf},
	}))
}

// A plugin with a required setting nobody has filled in is the commonest reason
// one is installed and does nothing, so the list calls it out.
func TestARequiredSettingIsCalledOut(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	body := p.get(admin.PluginsPath + "/engineer")
	r.Contains(strings.ToLower(body), "required")
}

// postRaw sends a body the form parser will refuse, which is the one request
// shape every handler here has to answer rather than panic on.
func (p *panel) postRaw(path, body string) int {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.server.URL+path, strings.NewReader(body))
	r.NoError(err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	p.lastBody, _ = io.ReadAll(res.Body)
	return res.StatusCode
}

// Every state a plugin can be in has a word and a colour on the page. A state
// added to the host without one here would render as "Unknown", which is worse
// than useless: it tells an operator nothing and looks like a bug in the panel.
func TestEveryPluginStateHasAWord(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	states := []plugins.State{
		plugins.StateRunning, plugins.StateStarting, plugins.StateStopped,
		plugins.StateDisabled, plugins.StateFailed, plugins.StateDiscovered,
	}
	f := &fakePlugins{}
	for i, st := range states {
		s := coachPlugin()
		s.Name = "plugin" + string(rune('a'+i))
		s.State = st
		s.Enabled = st != plugins.StateDisabled
		f.all = append(f.all, s)
	}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	body := p.get(admin.PluginsPath)
	for _, want := range []string{"Running", "Starting", "Stopped", "Off", "Failed", "Found"} {
		r.Contains(body, ">"+want+"<", "no row rendered %q", want)
	}
	r.NotContains(body, "Unknown", "a state reached the page with no word for it")
}

// The tail of a stored credential is shown so an operator can tell which key
// they used. A value that will not open — the data key was regenerated since —
// shows nothing rather than a tail of ciphertext.
func TestASecretThatWillNotOpenShowsNoTail(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	// Sealed with a key this server does not have, which is exactly what a
	// regenerated data key leaves behind.
	other, err := auth.NewSecretKey()
	r.NoError(err)
	sealed, err := other.Seal("sk-ant-thekeynobodymayeversee")
	r.NoError(err)
	r.NoError(p.store.SavePluginSecret(context.Background(), "engineer", "api_key", sealed))

	body := p.get(admin.PluginsPath + "/engineer")
	r.NotContains(body, "ending ", "a credential that cannot be read was described as though it could")
	r.NotContains(body, "sk-ant-")
	// The field says a value is there and that it cannot be opened, so the
	// operator replaces it rather than wondering why the plugin will not start.
	r.Contains(body, "stored, but unreadable")
	r.Contains(body, "cannot be opened with this server's data key")
	r.Contains(body, "Remove what is stored")
}

// A required setting left empty is refused here rather than by the plugin at
// three in the morning.
func TestARequiredSettingCannotBeClearedToNothing(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := coachPlugin()
	s.Settings = append(s.Settings, plugin.Setting{
		Name: "series", Label: "Series", Kind: plugin.KindText, Required: true,
	})
	f := &fakePlugins{all: []plugins.Status{s}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "series": {""}, "max_words": {"20"},
	}))
	r.Contains(p.answered(), "has to be filled in")
	r.Contains(p.answered(), "Series")
}

// A form body that will not parse is a 400 from every handler on these pages,
// not a panic and not a half-applied change.
func TestPluginFormsRefuseABodyThatWillNotParse(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	_ = p.get(admin.PluginsPath + "/engineer")

	for _, path := range []string{
		admin.PluginsPath + "/rescan",
		admin.PluginsPath + "/engineer/settings",
		admin.PluginsPath + "/engineer/action",
	} {
		r.Equal(http.StatusBadRequest, p.postRaw(path, "%zz=broken"),
			"%s accepted a body that will not parse", path)
	}
	r.Empty(f.actions())
	r.Zero(f.rescanCount)
}

// The host failing for a reason that is not "no such plugin" is a server error
// with the operator's words, not the host's.
func TestAPluginPageWhenTheHostWillNotAnswer(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{
		all:        []plugins.Status{coachPlugin()},
		failStatus: errors.New("the connection is gone"),
	}
	p := newPanel(t, withPlugins(f))

	status, body := p.getStatus(admin.PluginsPath + "/engineer")
	r.Equal(http.StatusInternalServerError, status)
	r.Contains(body, "could not be read")
	r.NotContains(body, "the connection is gone")
}

// An action that fails is reported on the page rather than swallowed, and the
// plugin is left as it was.
func TestAnActionThatTheHostRefuses(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	f.failAction = errors.New("it is turned off, so there is nothing to restart")
	r.Equal(http.StatusInternalServerError, p.post(admin.PluginsPath+"/engineer/action", url.Values{
		"csrf": {csrf}, "action": {"restart"},
	}))
	r.Contains(p.answered(), "That did not work")
	r.Contains(p.answered(), "nothing to restart")
}

// Saving when the plugin has just been removed by somebody else. The write
// fails on the foreign key, and the page says so rather than claiming a save
// that did not happen.
func TestSavingAPluginThatHasJustGone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	// The row goes while the form is open. The host still knows the plugin, so
	// this is the database refusing rather than the page.
	r.NoError(p.store.DeletePlugin(context.Background(), "engineer"))

	r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "greeting": {"Right"}, "max_words": {"20"},
	}))
	r.Contains(p.answered(), "could not be saved")
}

// Removing the last plugin leaves the list, and the list is the empty state
// rather than a page about something that is no longer there.
func TestRemovingTheLastPluginLeavesTheEmptyState(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/action", url.Values{
		"csrf": {csrf}, "action": {"remove"}, "confirm": {"1"},
	}))
	r.Contains(p.answered(), "No plugins yet")
	r.Contains(p.answered(), "/srv/pacenote/data/plugins")
}

// A plugin with nothing to configure says so, rather than showing an empty form
// with a Save button that does nothing.
func TestAPluginWithNoSettings(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := coachPlugin()
	s.Settings = nil
	f := &fakePlugins{all: []plugins.Status{s}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	body := p.get(admin.PluginsPath + "/engineer")
	r.Contains(body, "asks for nothing to be configured")
	r.NotContains(body, "/engineer/settings",
		"a plugin with nothing to configure was given a settings form")
	// It still has a cap, because the cap is the operator's and every plugin
	// has one whether or not it declares anything.
	r.Contains(body, "/engineer/cap")
}

// What a plugin printed is shown on its page, because it is the only thing an
// operator has to go on when one will not start.
func TestWhatAPluginPrintedIsShown(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := coachPlugin()
	s.State = plugins.StateFailed
	s.LastOutput = "engineer: the vendor answered 401"
	s.LastError = "it stopped without being asked to"
	f := &fakePlugins{all: []plugins.Status{s}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	body := p.get(admin.PluginsPath + "/engineer")
	r.Contains(body, "the vendor answered 401")
	r.Contains(body, "it stopped without being asked to")
}

// Every form on these pages is a 404 on a server with no plugin host, rather
// than a panic on a nil dependency.
func TestPluginFormsOnAServerWithNoHost(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := newPanel(t)
	csrf := csrfOf(t, p.get(admin.PluginsPath))

	for _, path := range []string{
		admin.PluginsPath + "/rescan",
		admin.PluginsPath + "/engineer/settings",
		admin.PluginsPath + "/engineer/action",
	} {
		r.Equal(http.StatusNotFound,
			p.post(path, url.Values{"csrf": {csrf}, "action": {"disable"}}),
			"%s did something on a server with no plugin host", path)
	}
}

// An action against a plugin that goes away while the form is open answers with
// the list rather than a page about something that is no longer there.
func TestAnActionOnAPluginThatVanishesMidRequest(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	// It answers the action's own lookup and then stops existing, which is what
	// a second administrator removing it looks like from here.
	f.mu.Lock()
	f.failStatusAfter = f.statusCalls + 1
	f.mu.Unlock()

	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/action", url.Values{
		"csrf": {csrf}, "action": {"disable"},
	}))
	r.Contains(p.answered(), "is off")
	r.Contains(p.answered(), "Plugins", "the reply was not the list")
}

// A credential too short to have a tail shows none rather than a slice of the
// whole thing.
func TestAVeryShortSecretShowsNoTail(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/settings", url.Values{
		"csrf": {csrf}, "api_key": {"abc"}, "max_words": {"20"},
	}))

	body := p.get(admin.PluginsPath + "/engineer")
	r.NotContains(body, "ending abc")
	r.NotContains(body, ">abc<")
	r.Contains(body, "stored, but unreadable",
		"a credential with no tail to show is described as unreadable, which is what the operator can act on")
}

// A state the host grows that this page has no word for renders as Unknown
// rather than as nothing at all.
func TestAStateWithNoWordRendersAsUnknown(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := coachPlugin()
	s.State = plugins.State("reticulating")
	f := &fakePlugins{all: []plugins.Status{s}}
	p := newPanel(t, withPlugins(f))
	// Deliberately not seeded: the plugins table has a check constraint listing
	// every state, so a row in this one could not exist. What this covers is an
	// older binary reading a row a newer one wrote — the page names it rather
	// than rendering an empty pill.

	r.Contains(p.get(admin.PluginsPath), "Unknown")
}

// The cap and the meter live on the plugin's page because it is the plugin's
// money: an operator running a coach and a voice wants to know which one costs.
func TestThePluginPageShowsWhatItMaySpendAndWhatItHas(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}, caps: map[string]int64{"engineer": 250_000}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	body := p.get(admin.PluginsPath + "/engineer")
	r.Contains(body, "What it may spend")
	r.Contains(body, `name="daily_token_cap"`)
	r.Contains(body, "250000")
	// The meter, in both windows an operator asks about.
	r.Contains(body, "Today")
	r.Contains(body, "This month")
	// And the sentence that makes the cap trustworthy.
	r.Contains(body, "the plugin is not told what it is and cannot go past it")
}

func TestSavingAPluginsCap(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/cap", url.Values{
		"csrf": {csrf}, "daily_token_cap": {"120000"},
	}))
	r.Contains(p.answered(), "That was saved.")
	r.EqualValues(120_000, f.caps["engineer"])

	// Zero is no cap, which is the operator saying so rather than a default.
	r.Equal(http.StatusOK, p.post(admin.PluginsPath+"/engineer/cap", url.Values{
		"csrf": {csrf}, "daily_token_cap": {"0"},
	}))
	r.EqualValues(0, f.caps["engineer"])
	r.Contains(p.get(admin.PluginsPath+"/engineer"), "no cap")
}

func TestACapThatIsNotANumber(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	for _, bad := range []string{"lots", "-1", ""} {
		r.Equal(http.StatusBadRequest, p.post(admin.PluginsPath+"/engineer/cap", url.Values{
			"csrf": {csrf}, "daily_token_cap": {bad},
		}), "the cap accepted %q", bad)
		r.Contains(p.answered(), "number of tokens")
	}
	r.Empty(f.caps["engineer"])
}

// A plugin at its cap is installed, running, configured and quietly not being
// called. That is the state most likely to be reported as "the coach broke", so
// the page says it plainly.
func TestAPluginAtItsCapSaysSo(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The fake reports 1 500 spent today, so a cap below that is reached.
	f := &fakePlugins{all: []plugins.Status{coachPlugin()}, caps: map[string]int64{"engineer": 1_000}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	body := p.get(admin.PluginsPath + "/engineer")
	r.Contains(body, "at the cap")
	r.Contains(body, "starts again at midnight")
}

// The spend figures failing to read must not take the page with them: the form
// above them is what an operator came for.
func TestAPageWhoseSpendingCannotBeRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{
		all:          []plugins.Status{coachPlugin()},
		failSpending: errors.New("the usage table is not there"),
	}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)

	status, body := p.getStatus(admin.PluginsPath + "/engineer")
	r.Equal(http.StatusOK, status, "a broken meter took the whole page down")
	r.Contains(body, "Anthropic key", "the settings form is still there")
	r.NotContains(body, "the usage table is not there")
}

func TestSavingACapOnAPluginThatIsNotThere(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	f := &fakePlugins{all: []plugins.Status{coachPlugin()}}
	p := newPanel(t, withPlugins(f))
	p.seedPluginRows(f.all)
	csrf := csrfOf(t, p.get(admin.PluginsPath+"/engineer"))

	r.Equal(http.StatusNotFound, p.post(admin.PluginsPath+"/ghost/cap", url.Values{
		"csrf": {csrf}, "daily_token_cap": {"1000"},
	}))

	// And a stale token, and a body that will not parse.
	r.Equal(http.StatusForbidden, p.post(admin.PluginsPath+"/engineer/cap", url.Values{
		"csrf": {"not-the-token"}, "daily_token_cap": {"1000"},
	}))
	r.Equal(http.StatusBadRequest, p.postRaw(admin.PluginsPath+"/engineer/cap", "%zz=broken"))
	r.Empty(f.caps["engineer"])
}

// The pair page is the one address in this product a person is told out loud:
// the client prints it on the machine being paired, and the discovery document
// publishes it. It answered 404 for a while, which meant the first thing a new
// driver was asked to do failed on a server that was working perfectly.
func TestThePairPageExistsAndNeedsNoSession(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := newPanel(t)
	p.signOut()

	status, body := p.getStatus(admin.PairPath)
	r.Equal(http.StatusOK, status, "the address a driver is given does not answer")
	r.Contains(body, "Pair a machine")
	r.Contains(body, "Iberian GT Championship", "a driver pairing with two leagues cannot tell which is which")
	r.Contains(body, "/admin/pairings", "the page does not say who approves it")

	// The code is echoed so a driver can check it against the other screen.
	status, body = p.getStatus(admin.PairPath + "?code=5DH-EHY")
	r.Equal(http.StatusOK, status)
	r.Contains(body, "5DH-EHY")

	// Lowercase is the same code: it is read off a screen and typed by a person.
	_, body = p.getStatus(admin.PairPath + "?code=5dh-ehy")
	r.Contains(body, "5DH-EHY")

	// Something that is not a code is not reflected back.
	_, body = p.getStatus(admin.PairPath + "?code=" + strings.Repeat("A", 64))
	r.NotContains(body, strings.Repeat("A", 64))
}

// What the discovery document tells a client to print has to be the address the
// server actually serves. They drifted apart once and nothing noticed.
func TestTheAdvertisedPairAddressIsTheOneThatIsServed(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	p := newPanel(t)

	// Straight from the document a client reads before anything else.
	status, body := p.getStatus(config.DiscoveryPath)
	r.Equal(http.StatusOK, status)
	var doc wire.Discovery
	r.NoError(json.Unmarshal([]byte(body), &doc))
	r.NotEmpty(doc.PairURI)

	u, err := url.Parse(doc.PairURI)
	r.NoError(err)
	r.Equal(admin.PairPath, u.Path,
		"the document sends drivers to %q and this server serves %q", u.Path, admin.PairPath)

	served, _ := p.getStatus(u.Path)
	r.Equal(http.StatusOK, served)
}
