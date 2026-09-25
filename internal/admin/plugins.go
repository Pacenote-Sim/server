package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
	"github.com/pacenote-sim/server/internal/plugins"
	"github.com/pacenote-sim/server/internal/pluginweb"
)

// PluginsPath is where the plugins page is mounted.
const PluginsPath = "/admin/plugins"

// The actions one row offers. They are one form with an action field rather
// than four routes because they are four buttons in one row, and because a
// route per verb would have to repeat the same lookup, the same CSRF check and
// the same redirect four times.
const (
	actionEnable  = "enable"
	actionDisable = "disable"
	actionRestart = "restart"
	actionRemove  = "remove"
)

// pluginRow is one plugin as the list renders it.
type pluginRow struct {
	Name        string
	Version     string
	Author      string
	Description string
	// State is what it is doing, in the words the page shows rather than the
	// host's constant: "Running", not "running".
	State string
	// Tone is what the state looks like — "good", "warn" or "bad" — so the
	// template does not decide which states are which.
	Tone string
	// Enabled is the operator's choice, which is not the same as whether it is
	// running: a plugin can be enabled and failed at the same time, and the
	// page has to show both or the buttons make no sense.
	Enabled bool
	// Installed is whether its files are still there.
	Installed bool
	// Declares is the capability list, one sentence each, from the plugin
	// module so that every host words it the same way.
	Declares []string
	// Pages are the addresses this plugin serves, as links.
	//
	// The capability list says a plugin serves them; this is how an operator
	// gets there. Without it the address is knowable only by reading the
	// manifest, which is not a thing an operator should have to do to find the
	// page a plugin installed.
	Pages []pluginPage
	// NeedsConfiguring is a plugin with a required setting nobody has filled
	// in. It is the single most common reason a plugin is installed and does
	// nothing, so it is called out rather than left to the operator to notice.
	NeedsConfiguring bool
	// LastError is why it is not running, already scrubbed of credentials.
	LastError string
	// Restarts is how many times it has come back since it last ran cleanly,
	// shown only when it is more than none.
	Restarts int
	// WrongVersion is a plugin built against another contract, with the two
	// numbers so the message can name both.
	WrongVersion bool
	BuiltFor     int
	HostSpeaks   int
	Href         string
	// Withdrawn is the marketplace's warning about this version, empty when
	// there is none.
	Withdrawn string
}

// pluginsForm is the list page.
type pluginsForm struct {
	Rows []pluginRow
	// Dir is the directory a plugin is dropped into, shown in the empty state
	// because "there are no plugins" without saying where they go is a dead
	// end.
	Dir string
	// HostVersion is the contract this server speaks.
	HostVersion int
	// Unavailable is a server with no plugin host at all.
	Unavailable bool
	// Market is the marketplace card.
	Market marketForm
}

// pluginField is one setting as the form renders it.
type pluginField struct {
	Name        string
	Label       string
	Help        string
	Kind        string
	Required    bool
	Placeholder string
	// Value is what is stored, for everything that is not a credential.
	Value string
	// Choices are the options of a choice setting, with the stored one marked.
	Choices []pluginChoice
	// IsSecret, Stored and Tail describe a credential. The value is never
	// rendered: the field says whether one is set and shows the last few
	// characters so an operator can tell which key they used.
	IsSecret bool
	Stored   bool
	Tail     string
	// Checked is a boolean setting's state, so the template does not have to
	// compare strings.
	Checked bool
}

type pluginChoice struct {
	Value    string
	Label    string
	Note     string
	Selected bool
}

// pluginSpend is what a plugin may spend and what it has, as the page shows it.
type pluginSpend struct {
	// Cap is the daily cap in tokens, and Capped whether there is one at all.
	Cap    int64
	Capped bool
	// Today and Month are what it has cost in each window.
	Today db.TokenUse
	Month db.TokenUse
	// OverCap is a plugin that is installed, running, configured, and quietly
	// not being called because it has spent its allowance.
	OverCap bool
}

// pluginForm is one plugin's own page.
type pluginForm struct {
	Row    pluginRow
	Fields []pluginField
	// Spend is what this plugin may spend and what it has. It is on this page
	// rather than in the server's settings because it is this plugin's money:
	// an operator running two plugins wants to know which one costs.
	Spend pluginSpend
	// Output is the last of what it printed, for a plugin that will not start.
	// It is scrubbed of credentials before it ever reaches here.
	Output string
	// Database says whether this plugin keeps tables of its own, which is the
	// one capability that leaves something behind when it is removed.
	Database bool
}

// getPlugins lists every plugin, running or not.
func (p *Panel) getPlugins(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	csrf := p.sessions.EnsureCSRF(w, r)
	form := pluginsForm{Dir: p.pluginDir()}

	if p.deps.Plugins == nil {
		form.Unavailable = true
		p.render(w, r, "plugins", pageData{
			Title: "Plugins", CSRF: csrf, SignedIn: true,
			Nav: navFor("Plugins"), Form: form,
		})
		return
	}

	form.HostVersion = p.deps.Plugins.InterfaceVersion()
	all, err := p.deps.Plugins.All(r.Context())
	if err != nil {
		p.renderPluginsError(w, r, csrf, form,
			"The list of plugins could not be read. The database did not answer.")
		return
	}
	for _, s := range all {
		form.Rows = append(form.Rows, p.rowOf(s, form.HostVersion))
	}
	form.Market = p.marketFormFor(r.Context(), form.Rows)
	p.render(w, r, "plugins", pageData{
		Title: "Plugins", CSRF: csrf, SignedIn: true,
		Nav: navFor("Plugins"), Form: form,
		Refresh: p.installRefresh(),
	})
}

// rowOf is [pluginRowOf] with the marketplace's warning attached.
func (p *Panel) rowOf(s plugins.Status, hostVersion int) pluginRow {
	row := pluginRowOf(s, hostVersion)
	row.Withdrawn = p.withdrawnNote(s.Name, s.Version)
	return row
}

// installRefresh is how often the page reloads while an install is running,
// and zero otherwise.
func (p *Panel) installRefresh() int {
	if p.installs.snapshot().Running {
		return installRefreshSeconds
	}
	return 0
}

func (p *Panel) renderPluginsError(w http.ResponseWriter, r *http.Request, csrf string, form pluginsForm, msg string) {
	p.render(w, r, "plugins", pageData{
		Title: "Plugins", CSRF: csrf, SignedIn: true,
		Nav: navFor("Plugins"), Form: form, Error: msg,
		Status: http.StatusInternalServerError,
	})
}

// getPlugin is one plugin's page: what it declared, what it needs configured,
// and what it last printed.
func (p *Panel) getPlugin(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	if p.deps.Plugins == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server is not running plugins.")
		return
	}
	name := r.PathValue("name")
	csrf := p.sessions.EnsureCSRF(w, r)

	status, err := p.deps.Plugins.Status(r.Context(), name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			httpx.Problem(w, r, http.StatusNotFound, "There is no plugin by that name.")
			return
		}
		httpx.Problem(w, r, http.StatusInternalServerError, "That plugin could not be read. Try again.")
		return
	}

	fields, err := p.pluginFields(r.Context(), status)
	if err != nil {
		httpx.Problem(w, r, http.StatusInternalServerError, "What this plugin has configured could not be read.")
		return
	}

	p.render(w, r, "plugin", pageData{
		Title: status.Name, CSRF: csrf, SignedIn: true,
		Nav: navFor("Plugins"),
		Form: pluginForm{
			Row:      p.rowOf(status, p.deps.Plugins.InterfaceVersion()),
			Fields:   fields,
			Spend:    p.pluginSpending(r.Context(), name),
			Output:   status.LastOutput,
			Database: status.Capabilities.Database,
		},
	})
}

// pluginSpending reads what a plugin may spend and what it has. A failure is
// rendered as nothing rather than failing the page: an operator whose usage
// table is unreadable still needs the form above it.
func (p *Panel) pluginSpending(ctx context.Context, name string) pluginSpend {
	if p.deps.Plugins == nil {
		return pluginSpend{}
	}
	got, err := p.deps.Plugins.Spending(ctx, name)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "what a plugin has spent could not be read",
			slog.String("plugin", name), slog.String("reason", err.Error()))
		return pluginSpend{}
	}
	return pluginSpend{
		Cap: got.Cap, Capped: got.Cap > 0,
		Today: got.Today, Month: got.Month, OverCap: got.OverCap,
	}
}

// postPluginCap saves what a plugin may spend in a day.
//
// It is its own form rather than a field on the settings above, because the two
// are different kinds of thing: the settings are what the plugin asked for, and
// this is what the operator allows it. A plugin that declares nothing still has
// a cap.
func (p *Panel) postPluginCap(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	if p.deps.Plugins == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server is not running plugins.")
		return
	}
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}

	name := r.PathValue("name")
	tokens, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("daily_token_cap")), 10, 64)
	if err != nil || tokens < 0 {
		p.answerPlugin(w, r, name, "",
			"The daily cap is a number of tokens, and 0 means no cap at all.",
			http.StatusBadRequest)
		return
	}
	if err := p.deps.Plugins.SetDailyCap(r.Context(), name, tokens); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			httpx.Problem(w, r, http.StatusNotFound, "There is no plugin by that name.")
			return
		}
		p.answerPlugin(w, r, name, "", "That could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	p.answerPlugin(w, r, name, "That was saved.", "", http.StatusOK)
}

// postPluginSettings saves what the operator typed.
//
// A credential left blank is left alone rather than cleared: the field is never
// rendered with its value in it, so an empty box means "I did not change this"
// and not "remove it". Clearing one is the remove button beside it.
func (p *Panel) postPluginSettings(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	if p.deps.Plugins == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server is not running plugins.")
		return
	}
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}

	name := r.PathValue("name")
	status, err := p.deps.Plugins.Status(r.Context(), name)
	if err != nil {
		httpx.Problem(w, r, http.StatusNotFound, "There is no plugin by that name.")
		return
	}

	if msg := p.savePluginSettings(r, status); msg != "" {
		p.answerPlugin(w, r, name, "", msg, http.StatusBadRequest)
		return
	}

	// The plugin is restarted so that it is holding what was just saved. A
	// plugin reads its settings on every call, so this is not strictly needed —
	// but a plugin that was failing for want of a credential has to be given
	// the chance to start now that it has one, and an operator who has just
	// filled in a key expects the page to stop saying it is missing.
	if status.Enabled {
		if err := p.deps.Plugins.Restart(r.Context(), name); err != nil {
			p.answerPlugin(w, r, name, "That was saved.",
				"It would not restart with the new settings. What it printed is below.",
				http.StatusOK)
			return
		}
	}
	p.answerPlugin(w, r, name, "That was saved.", "", http.StatusOK)
}

// savePluginSettings writes one form's worth of values, returning the message
// to show when something was refused. Validation is the plugin module's, so a
// value the panel accepts is one the plugin would accept.
func (p *Panel) savePluginSettings(r *http.Request, status plugins.Status) string {
	for _, s := range status.Settings {
		raw := strings.TrimSpace(r.PostFormValue(s.Name))

		if s.Kind == plugin.KindSecret {
			if r.PostFormValue("remove_"+s.Name) == "1" {
				if err := p.deps.Store.DeletePluginSetting(r.Context(), status.Name, s.Name); err != nil {
					return "That credential could not be removed. Try again."
				}
				continue
			}
			if raw == "" {
				// Not "clear it": the field is never rendered with a value in
				// it, so an empty box is an operator who did not touch it.
				continue
			}
			key := p.deps.Keyring.Key()
			if key == nil {
				return "This server has no data key, so a credential cannot be stored. Set one in Settings first."
			}
			sealed, err := key.Seal(raw)
			if err != nil {
				return "That credential could not be stored. Try again."
			}
			if err := p.deps.Store.SavePluginSecret(r.Context(), status.Name, s.Name, sealed); err != nil {
				return "That credential could not be stored. Try again."
			}
			continue
		}

		// An unchecked checkbox is absent from the form rather than false.
		if s.Kind == plugin.KindBool {
			raw = "false"
			if r.PostFormValue(s.Name) != "" {
				raw = "true"
			}
		}

		if raw == "" && s.Required {
			return "“" + s.Label + "” has to be filled in."
		}
		if raw != "" {
			if _, err := plugin.ValidateValues([]plugin.Setting{s}, plugin.Values{s.Name: raw}); err != nil {
				return "“" + s.Label + "” is not something this plugin accepts: " + err.Error()
			}
		}
		if err := p.deps.Store.SavePluginSetting(r.Context(), status.Name, s.Name, raw); err != nil {
			return "That could not be saved. Try again."
		}
	}
	return ""
}

// postPluginAction is the four buttons on a row.
func (p *Panel) postPluginAction(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	if p.deps.Plugins == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server is not running plugins.")
		return
	}
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}

	name := r.PathValue("name")
	ctx := r.Context()

	var err error
	var notice string
	switch r.PostFormValue("action") {
	case actionEnable:
		err, notice = p.deps.Plugins.Enable(ctx, name), name+" is on."
	case actionDisable:
		err, notice = p.deps.Plugins.Disable(ctx, name), name+" is off. Nothing it stored has been removed."
	case actionRestart:
		err, notice = p.deps.Plugins.Restart(ctx, name), name+" was restarted."
	case actionRemove:
		// The one action that destroys anything, so it is the one that asks
		// first. The checkbox is marked required in the form as well, but that
		// is the browser's opinion and this is the server's: a POST that
		// reaches here without it is refused.
		if r.PostFormValue("confirm") != "1" {
			p.answerPlugin(w, r, name, "",
				"Removing "+name+" destroys everything it stored, so the box has to be ticked.",
				http.StatusBadRequest)
			return
		}
		err = p.deps.Plugins.Remove(ctx, name)
		notice = name + " was removed, with everything it had stored."
	default:
		httpx.Problem(w, r, http.StatusBadRequest, "That is not something this page can do.")
		return
	}

	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			httpx.Problem(w, r, http.StatusNotFound, "There is no plugin by that name.")
			return
		}
		p.answerPlugin(w, r, name, "", "That did not work: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Removing leaves no page to go back to.
	if r.PostFormValue("action") == actionRemove {
		p.answerPlugins(w, r, notice, "", http.StatusOK)
		return
	}
	p.answerPlugin(w, r, name, notice, "", http.StatusOK)
}

// postPluginRescan re-reads the plugin directory without a restart.
func (p *Panel) postPluginRescan(w http.ResponseWriter, r *http.Request, _ db.AdminSession) {
	if p.deps.Plugins == nil {
		httpx.Problem(w, r, http.StatusNotFound, "This server is not running plugins.")
		return
	}
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return
	}
	if err := p.deps.Plugins.Discover(r.Context()); err != nil {
		p.answerPlugins(w, r, "", "The plugin directory could not be read: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	p.answerPlugins(w, r, "The plugin directory was read again.", "", http.StatusOK)
}

// The answer to a form on these pages is the page itself, re-rendered with what
// happened at the top. That is what every other page in this panel does, and it
// is worth being consistent about for a reason beyond consistency: the
// alternative is a redirect carrying the message in its query string, which puts
// the operator's error text into their browser history, their proxy logs and
// every screenshot they send when asking what went wrong.

// answerPlugins re-renders the list with a message.
func (p *Panel) answerPlugins(w http.ResponseWriter, r *http.Request, notice, msg string, status int) {
	csrf := p.sessions.EnsureCSRF(w, r)
	form := pluginsForm{Dir: p.pluginDir()}
	if p.deps.Plugins != nil {
		form.HostVersion = p.deps.Plugins.InterfaceVersion()
		if all, err := p.deps.Plugins.All(r.Context()); err == nil {
			for _, s := range all {
				form.Rows = append(form.Rows, p.rowOf(s, form.HostVersion))
			}
		} else if msg == "" {
			msg = "The list of plugins could not be read. The database did not answer."
			status = http.StatusInternalServerError
		}
	} else {
		form.Unavailable = true
	}
	form.Market = p.marketFormFor(r.Context(), form.Rows)
	p.render(w, r, "plugins", pageData{
		Title: "Plugins", CSRF: csrf, SignedIn: true,
		Nav: navFor("Plugins"), Form: form,
		Notice: notice, Error: msg, Status: status,
		Refresh: p.installRefresh(),
	})
}

// answerPlugin re-renders one plugin's page with a message. A plugin that has
// gone — removed while the form was open — falls back to the list rather than
// to a page about nothing.
func (p *Panel) answerPlugin(w http.ResponseWriter, r *http.Request, name, notice, msg string, status int) {
	if p.deps.Plugins == nil {
		p.answerPlugins(w, r, notice, msg, status)
		return
	}
	st, err := p.deps.Plugins.Status(r.Context(), name)
	if err != nil {
		p.answerPlugins(w, r, notice, msg, status)
		return
	}
	csrf := p.sessions.EnsureCSRF(w, r)
	fields, err := p.pluginFields(r.Context(), st)
	if err != nil && msg == "" {
		msg = "What this plugin has configured could not be read."
		status = http.StatusInternalServerError
	}
	p.render(w, r, "plugin", pageData{
		Title: st.Name, CSRF: csrf, SignedIn: true,
		Nav: navFor("Plugins"),
		Form: pluginForm{
			Row:      p.rowOf(st, p.deps.Plugins.InterfaceVersion()),
			Fields:   fields,
			Spend:    p.pluginSpending(r.Context(), name),
			Output:   st.LastOutput,
			Database: st.Capabilities.Database,
		},
		Notice: notice, Error: msg, Status: status,
	})
}

// pluginFields turns what a plugin declared, plus what is stored, into the form.
func (p *Panel) pluginFields(ctx context.Context, status plugins.Status) ([]pluginField, error) {
	rows, err := p.deps.Store.PluginSettings(ctx, status.Name)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]db.PluginSettingRow, len(rows))
	for _, row := range rows {
		stored[row.Name] = row
	}

	out := make([]pluginField, 0, len(status.Settings))
	for _, s := range status.Settings {
		f := pluginField{
			Name:        s.Name,
			Label:       s.Label,
			Help:        s.Help,
			Kind:        string(s.Kind),
			Required:    s.Required,
			Placeholder: s.Placeholder,
			Value:       s.Default,
		}
		row, have := stored[s.Name]

		if s.Kind == plugin.KindSecret {
			f.IsSecret = true
			f.Stored = have && len(row.Sealed) > 0
			if f.Stored {
				f.Tail = p.secretTail(row.Sealed)
			}
			f.Value = ""
			out = append(out, f)
			continue
		}

		if have && row.Value != "" {
			f.Value = row.Value
		}
		switch s.Kind {
		case plugin.KindBool:
			f.Checked = f.Value == "true"
		case plugin.KindChoice:
			for _, c := range s.Choices {
				f.Choices = append(f.Choices, pluginChoice{
					Value: c.Value, Label: c.Label, Note: c.Note,
					Selected: c.Value == f.Value,
				})
			}
		case plugin.KindText, plugin.KindNumber, plugin.KindSecret:
		}
		out = append(out, f)
	}
	return out, nil
}

// secretTail is the last four characters of a stored credential, so an operator
// can tell which key they used without the panel ever showing one. A value that
// will not open — the data key was regenerated — shows nothing rather than a
// tail of ciphertext.
func (p *Panel) secretTail(sealed []byte) string {
	key := p.deps.Keyring.Key()
	if key == nil {
		return ""
	}
	opened, err := key.Open(sealed)
	if err != nil || len(opened) < 4 {
		return ""
	}
	return opened[len(opened)-4:]
}

// pluginRowOf is one status as a page shows it.
// pluginPage is one address a plugin serves.
type pluginPage struct {
	// Href is where it is, and Path what the plugin calls it.
	Href string
	Path string
	// Reach is who may open it, in words.
	Reach string
	// Yours reports that an administrator can follow this link. A driver's
	// page is listed so the operator knows it exists and can say where it is,
	// but following it as an administrator would only produce a refusal.
	Yours bool
	// Reason is why a plugin checks a caller itself, when it does.
	Reason string
}

// pagesOf is the plugin's declared routes as links an operator can follow.
func pagesOf(s plugins.Status) []pluginPage {
	if s.Capabilities.HTTP == nil {
		return nil
	}
	out := make([]pluginPage, 0, len(s.Capabilities.HTTP.Routes))
	for _, route := range s.Capabilities.HTTP.Routes {
		page := pluginPage{
			Href:   pluginweb.Prefix + url.PathEscape(s.Name) + route.Path,
			Path:   route.Path,
			Reason: route.Reason,
		}
		switch route.Access {
		case plugin.AccessPublic:
			page.Reach, page.Yours = "anyone", true
		case plugin.AccessDriver:
			page.Reach = "a signed-in driver"
		case plugin.AccessAdmin:
			page.Reach, page.Yours = "you", true
		case plugin.AccessCustom:
			page.Reach, page.Yours = "whoever the plugin decides", true
		default:
			page.Reach = "nobody, because this server does not understand who it is for"
		}
		out = append(out, page)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func pluginRowOf(s plugins.Status, hostVersion int) pluginRow {
	row := pluginRow{
		Name:         s.Name,
		Version:      s.Version,
		Author:       s.Author,
		Description:  s.Description,
		State:        stateWord(s.State),
		Tone:         stateTone(s.State),
		Enabled:      s.Enabled,
		Installed:    s.Installed,
		Declares:     s.Capabilities.Describe(),
		Pages:        pagesOf(s),
		LastError:    s.LastError,
		Restarts:     s.Restarts,
		BuiltFor:     s.InterfaceVersion,
		HostSpeaks:   hostVersion,
		WrongVersion: hostVersion != 0 && s.InterfaceVersion != 0 && s.InterfaceVersion != hostVersion,
		Href:         PluginsPath + "/" + url.PathEscape(s.Name),
	}
	for _, set := range s.Settings {
		if set.Required {
			row.NeedsConfiguring = true
			break
		}
	}
	sort.Strings(row.Declares)
	return row
}

// stateWord is a state in the words the page shows.
func stateWord(s plugins.State) string {
	switch s {
	case plugins.StateRunning:
		return "Running"
	case plugins.StateStarting:
		return "Starting"
	case plugins.StateStopped:
		return "Stopped"
	case plugins.StateDisabled:
		return "Off"
	case plugins.StateFailed:
		return "Failed"
	case plugins.StateDiscovered:
		return "Found"
	}
	return "Unknown"
}

// stateTone is how a state looks, so the template holds no opinion about which
// states are bad.
func stateTone(s plugins.State) string {
	switch s {
	case plugins.StateRunning:
		return "good"
	case plugins.StateFailed:
		return "bad"
	case plugins.StateStarting, plugins.StateDiscovered:
		return "warn"
	case plugins.StateStopped, plugins.StateDisabled:
		return "muted"
	}
	return "muted"
}

// pluginDir is where a plugin is dropped, for the empty state.
func (p *Panel) pluginDir() string {
	if p.deps.PluginDir == "" {
		return "the plugins directory beside the server"
	}
	return p.deps.PluginDir
}
