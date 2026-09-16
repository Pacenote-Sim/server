package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// SettingsPath is where the settings page is mounted.
const SettingsPath = "/admin/settings"

// EnterpriseNote is the one line beside a setting this build does not have.
// Such settings are shown and disabled rather than hidden: an operator should
// be able to see what exists and decide whether they want it, and a gap where
// a control will be is more confusing than a control that is plainly off.
const EnterpriseNote = "Part of Enterprise — it is not in the community edition."

// identityForm is what discovery publishes about the organisation.
type identityForm struct {
	Organisation string
	ShortName    string
	ShortNameHas bool
	Logo         string
	Accent       string
}

// limitsForm is the numbers discovery publishes and the limiter enforces.
type limitsForm struct {
	TracePoints       int
	LapsPerRequest    int
	LiveIntervalMs    int
	FieldIntervalMs   int
	SummaryIntervalMs int
	MaxBodyBytes      int
}

// settingsForm is the whole page.
type settingsForm struct {
	Identity   identityForm
	Limits     limitsForm
	Discovery  string
	Devices    int64
	HasDataKey bool
}

// getSettings renders the page.
func (p *Panel) getSettings(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.renderSettings(w, r, sess, "", "", http.StatusOK)
}

// renderSettings builds the page from what is stored and writes it.
func (p *Panel) renderSettings(w http.ResponseWriter, r *http.Request, sess db.AdminSession, notice, problem string, status int) {
	ctx := r.Context()
	csrf := p.sessions.EnsureCSRF(w, r)

	settings, err := p.deps.Settings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
		if problem == "" {
			problem = "These settings could not be read — the database did not answer. Nothing has changed."
		}
	}
	form := p.settingsForm(ctx, settings)

	p.render(w, r, "settings", pageData{
		Title:        "Settings",
		Organisation: settings.Organisation,
		SignedIn:     true,
		Email:        sess.Email,
		CSRF:         csrf,
		Nav:          navFor("Settings"),
		Notice:       notice,
		Error:        problem,
		Status:       status,
		Form:         form,
	})
}

// settingsForm turns the stored settings into what the template renders. It
// never fails: a part that cannot be read is shown as the part that could not
// be read, because a page that refuses to load is a page an operator cannot
// use to fix the thing that broke it.
func (p *Panel) settingsForm(ctx context.Context, s config.Settings) settingsForm {
	key := p.deps.Keyring.Key()
	form := settingsForm{
		Identity: identityForm{
			Organisation: s.Organisation,
			ShortName:    s.ShortName,
			ShortNameHas: s.ShortName != "",
			Logo:         s.Logo,
			Accent:       s.Accent,
		},
		Limits: limitsForm{
			TracePoints:       s.Limits.TracePoints,
			LapsPerRequest:    s.Limits.LapsPerRequest,
			LiveIntervalMs:    s.Limits.LiveIntervalMs,
			FieldIntervalMs:   s.Limits.FieldIntervalMs,
			SummaryIntervalMs: s.Limits.SummaryIntervalMs,
			MaxBodyBytes:      s.Limits.MaxBodyBytes,
		},
		HasDataKey: len(key) == auth.SecretKeyBytes && p.deps.SaveDataKey != nil,
	}

	form.Discovery = p.discoveryPreview(s)
	if p.deps.Store != nil {
		if stats, err := p.deps.Store.Stats(ctx); err == nil {
			form.Devices = stats.Devices
		}
	}
	return form
}

// discoveryPreview renders the document as it would be served right now. It is
// on the page because every field above it ends up in this one document, and
// an operator who can see it does not have to guess which field a client reads.
func (p *Panel) discoveryPreview(s config.Settings) string {
	if p.deps.Discovery == nil {
		return ""
	}
	b, err := json.MarshalIndent(p.deps.Discovery(s), "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// postIdentity saves what drivers see.
func (p *Panel) postIdentity(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.saveGroup(w, r, sess, db.ActionSettingsIdentity, "identity",
		func(s *config.Settings) ([]string, string) {
			organisation := strings.TrimSpace(r.PostFormValue("organisation"))
			if organisation == "" {
				return nil, "The organisation name is what a driver sees when their client connects, so it cannot be empty."
			}
			logo := strings.TrimSpace(r.PostFormValue("logo"))
			if err := config.ValidateURL("logo address", logo); err != nil {
				return nil, "The logo has to be a full address starting with https://, or empty for no logo."
			}
			accent := strings.TrimSpace(r.PostFormValue("accent"))
			if err := config.ValidateAccent(accent); err != nil {
				return nil, "The accent colour is a hex colour like #C6F24B, or empty to leave the client's own."
			}

			var changed []string
			changed = note(changed, "organisation", s.Organisation != organisation)
			changed = note(changed, "short_name", s.ShortName != strings.TrimSpace(r.PostFormValue("short_name")))
			changed = note(changed, "logo", s.Logo != logo)
			changed = note(changed, "accent", s.Accent != accent)

			s.Organisation = organisation
			s.ShortName = strings.TrimSpace(r.PostFormValue("short_name"))
			s.Logo = logo
			s.Accent = accent
			return changed, ""
		})
}

// postLimits saves the numbers discovery publishes.
//
// They are one source of truth and not two: the limiter is built from the same
// values a moment later, so a client that obeys the document it was given is
// never refused by the server that gave it.
func (p *Panel) postLimits(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	p.saveGroup(w, r, sess, db.ActionSettingsLimits, "limits",
		func(s *config.Settings) ([]string, string) {
			next := s.Limits
			fields := []struct {
				name  string
				field string
				into  *int
			}{
				{"trace_points", "trace_points", &next.TracePoints},
				{"laps_per_request", "laps_per_request", &next.LapsPerRequest},
				{"live_interval_ms", "live_interval_ms", &next.LiveIntervalMs},
				{"summary_interval_ms", "summary_interval_ms", &next.SummaryIntervalMs},
				{"max_body_bytes", "max_body_bytes", &next.MaxBodyBytes},
			}
			for _, f := range fields {
				v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(f.field)))
				if err != nil {
					return nil, "Every limit is a whole number — one of them is not."
				}
				*f.into = v
			}
			if err := config.ValidateLimits(next); err != nil {
				return nil, operatorMessage(err)
			}

			var changed []string
			for _, f := range fields {
				before := limitValue(s.Limits, f.name)
				changed = note(changed, f.name, before != *f.into)
			}
			s.Limits = next
			return changed, ""
		})
}

// saveGroup is the shape every settings form shares: check the token, read
// what is stored, let the caller change it, write it back, tell the API to
// re-read, and record what changed.
//
// The audit row names the fields and never their values. A settings change can
// be a change to an API key, and a trail that recorded the value would be the
// one place the key was readable.
func (p *Panel) saveGroup(
	w http.ResponseWriter, r *http.Request, sess db.AdminSession,
	action, subject string,
	apply func(*config.Settings) (changed []string, problem string),
) {
	if !p.beginForm(w, r) {
		return
	}
	ctx := r.Context()

	settings, err := p.deps.Settings(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "settings unreadable", slog.Any("error", err))
		p.renderSettings(w, r, sess, "",
			"These settings could not be read, so nothing was changed — the database did not answer.",
			http.StatusInternalServerError)
		return
	}

	next := settings
	changed, problem := apply(&next)
	if problem != "" {
		p.renderSettings(w, r, sess, "", problem, http.StatusUnprocessableEntity)
		return
	}
	if len(changed) == 0 {
		p.renderSettings(w, r, sess, "Nothing to save — those values are already what is stored.", "", http.StatusOK)
		return
	}
	if err := p.deps.Store.SaveSettings(ctx, next); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the settings could not be saved", slog.Any("error", err))
		p.renderSettings(w, r, sess, "",
			"That change was not saved — the database did not answer.", http.StatusInternalServerError)
		return
	}
	p.settingsChanged(ctx, sess, action, subject, changed)
	p.renderSettings(w, r, sess, savedNotice(subject), "", http.StatusOK)
}

// settingsChanged is what follows every successful write: the API re-reads, the
// change is recorded, and a line goes into the log naming the fields.
func (p *Panel) settingsChanged(ctx context.Context, sess db.AdminSession, action, subject string, changed []string) {
	if p.deps.OnSettingsChanged != nil {
		p.deps.OnSettingsChanged()
	}
	if err := p.deps.Store.WriteAudit(ctx, sess.Email, action, subject, changed); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the change could not be recorded", slog.Any("error", err))
	}
	// The field names travel; no value does, and the two credential fields are
	// named here exactly as they are named in the audit row.
	p.deps.Log.LogAttrs(ctx, slog.LevelInfo, "settings changed",
		slog.String("action", action),
		slog.String("fields", strings.Join(changed, ",")))
}

// postDanger runs one of the two irreversible actions, each behind a phrase the
// operator types.
func (p *Panel) postDanger(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !p.beginForm(w, r) {
		return
	}
	switch r.PostFormValue("action") {
	case "regenerate_data_key":
		p.regenerateDataKey(w, r, sess)
	case "revoke_devices":
		p.revokeDevices(w, r, sess)
	default:
		p.renderSettings(w, r, sess, "", "That is not something this page does.", http.StatusBadRequest)
	}
}

// regenerateDataKey mints a new data key, re-seals the stored credentials with
// it, and writes it beside the binary.
//
// The order is deliberate. The file is written first, because it is the half
// that can fail for reasons outside this server — a read-only volume, a full
// disk — and a failure there must leave the database holding ciphertext the
// old key still opens. If the second half fails the operator is told plainly
// that the keys have to be entered again, which is recoverable; the reverse
// would be a server that cannot read its own credentials and does not say so.
func (p *Panel) regenerateDataKey(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !auth.EqualString("regenerate", strings.TrimSpace(r.PostFormValue("confirm"))) {
		p.renderSettings(w, r, sess, "",
			"Type regenerate to confirm — the current data key stays until you do.", http.StatusUnprocessableEntity)
		return
	}
	if p.deps.SaveDataKey == nil {
		p.renderSettings(w, r, sess, "",
			"This server cannot write its configuration file, so the data key cannot be replaced here.",
			http.StatusConflict)
		return
	}
	ctx := r.Context()

	if _, err := p.deps.Settings(ctx); err != nil {
		p.renderSettings(w, r, sess, "",
			"The settings could not be read, so nothing was changed — the database did not answer.",
			http.StatusInternalServerError)
		return
	}
	fresh, err := auth.NewSecretKey()
	if err != nil {
		p.renderSettings(w, r, sess, "", "A new data key could not be made. Nothing has changed.",
			http.StatusInternalServerError)
		return
	}

	old := p.deps.Keyring.Key()
	if err := p.deps.SaveDataKey(ctx, fresh); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the new data key could not be stored", slog.Any("error", err))
		p.renderSettings(w, r, sess, "",
			"The new data key could not be written to the configuration file, so nothing has changed.",
			http.StatusInternalServerError)
		return
	}
	p.deps.Keyring.Replace(fresh)

	// Every credential this server holds belongs to a plugin, so this is the
	// whole of the re-sealing. An operator who regenerated the key and then
	// found a plugin silently off would have no way to connect the two.
	resealed, failed := p.resealPluginSecrets(ctx, old, fresh)
	var notice string
	switch {
	case failed > 0:
		notice = "The data key has been replaced. " +
			plural(int64(failed), "credential") +
			" stored by plugins could not be re-sealed and must be entered again on their pages."
	case resealed > 0:
		notice = "The data key has been replaced, and " +
			plural(int64(resealed), "credential") +
			" belonging to plugins was re-sealed with it. A copy of the database taken before now can no longer be opened with it."
	default:
		notice = "The data key has been replaced. No plugin had a credential stored, so there was nothing to re-seal."
	}
	p.settingsChanged(ctx, sess, db.ActionDataKeyRegenerated, "data key", []string{"secret_key"})
	p.renderSettings(w, r, sess, notice, "", http.StatusOK)
}

// revokeDevices revokes every live device token at once.
func (p *Panel) revokeDevices(w http.ResponseWriter, r *http.Request, sess db.AdminSession) {
	if !auth.EqualString("revoke", strings.TrimSpace(r.PostFormValue("confirm"))) {
		p.renderSettings(w, r, sess, "",
			"Type revoke to confirm — every paired machine keeps working until you do.",
			http.StatusUnprocessableEntity)
		return
	}
	ctx := r.Context()
	n, err := p.deps.Store.RevokeAllDevices(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the device tokens could not be revoked", slog.Any("error", err))
		p.renderSettings(w, r, sess, "",
			"Nothing was revoked — the database did not answer.", http.StatusInternalServerError)
		return
	}
	if err := p.deps.Store.WriteAudit(ctx, sess.Email, db.ActionDevicesRevoked,
		plural(n, "device token"), []string{"devices"}); err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError, "the revocation could not be recorded", slog.Any("error", err))
	}
	p.deps.Log.LogAttrs(ctx, slog.LevelWarn, "every device token revoked", slog.Int64("devices", n))
	p.renderSettings(w, r, sess, fmt.Sprintf(
		"Revoked %s — every driver pairs again before their next upload.",
		plural(n, "device token")), "", http.StatusOK)
}

// beginForm reads a form and checks its token, and reports whether the caller
// may carry on. A request that fails either has already been answered.
func (p *Panel) beginForm(w http.ResponseWriter, r *http.Request) bool {
	if err := httpx.ParseForm(w, r, httpx.FormLimit); err != nil {
		httpx.Problem(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return false
	}
	if !p.sessions.CheckCSRF(r) {
		httpx.Problem(w, r, http.StatusForbidden, "That form is stale. Load the page again and try once more.")
		return false
	}
	return true
}

// resealPluginSecrets moves every credential a plugin holds onto the new data
// key, and reports how many moved and how many could not.
//
// A plugin's credentials are sealed with the data key, so regenerating it
// without this would leave every plugin holding ciphertext nothing can open —
// each would simply stop, with nothing on any page connecting that to the
// button that was pressed.
//
// One that will not open is left exactly as it is rather than deleted. The core
// drops its own unreadable credentials, because the page can then say plainly
// that there is none stored; a plugin's settings page says the same thing from
// the row's presence, and deleting somebody else's data to tidy up a failure is
// not this function's to do.
func (p *Panel) resealPluginSecrets(ctx context.Context, old, fresh auth.SecretKey) (resealed, failed int) {
	if p.deps.Store == nil || old == nil || fresh == nil {
		return 0, 0
	}
	installed, err := p.deps.Store.Plugins(ctx)
	if err != nil {
		p.deps.Log.LogAttrs(ctx, slog.LevelError,
			"the plugins holding credentials could not be listed, so none were re-sealed",
			slog.Any("error", err))
		return 0, 0
	}
	for _, row := range installed {
		values, err := p.deps.Store.PluginSettings(ctx, row.Name)
		if err != nil {
			failed++
			continue
		}
		for _, v := range values {
			if len(v.Sealed) == 0 {
				continue
			}
			opened, err := old.Open(v.Sealed)
			if err != nil {
				failed++
				continue
			}
			next, err := fresh.Seal(opened)
			if err != nil {
				failed++
				continue
			}
			if err := p.deps.Store.SavePluginSecret(ctx, row.Name, v.Name, next); err != nil {
				failed++
				continue
			}
			resealed++
		}
	}
	if failed > 0 {
		p.deps.Log.LogAttrs(ctx, slog.LevelWarn,
			"some plugin credentials could not be re-sealed with the new data key",
			slog.Int("resealed", resealed), slog.Int("failed", failed))
	}
	return resealed, failed
}

// note appends a field name when it changed, which is what the audit row and
// the log line both carry.
func note(changed []string, name string, differs bool) []string {
	if differs {
		return append(changed, name)
	}
	return changed
}

// limitValue reads one published limit by the name the audit row uses.
func limitValue(l wire.Limits, name string) int {
	switch name {
	case "trace_points":
		return l.TracePoints
	case "laps_per_request":
		return l.LapsPerRequest
	case "live_interval_ms":
		return l.LiveIntervalMs
	case "summary_interval_ms":
		return l.SummaryIntervalMs
	case "max_body_bytes":
		return l.MaxBodyBytes
	default:
		return 0
	}
}

// plural counts a thing the way a sentence needs it, so the panel never says
// "1 device tokens".
func plural(n int64, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.FormatInt(n, 10) + " " + thing + "s"
}

// savedNotice is what the page says after a group is saved. Each names what
// happens next, because "saved" answers a question nobody asked.
func savedNotice(subject string) string {
	switch subject {
	case "identity":
		return "Saved — a client picks this up the next time it reads the discovery document, within five minutes."
	case "limits":
		return "Saved — clients pick these up with the discovery document, and this server is already holding them."
	default:
		return "Saved."
	}
}

// operatorMessage strips the package prefix off one of config's errors, which
// are written for an operator but spelled for a log.
func operatorMessage(err error) string {
	msg := err.Error()
	if after, ok := strings.CutPrefix(msg, "config: "); ok {
		msg = after
	}
	if msg == "" {
		return "That value is not one this server can use."
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}
