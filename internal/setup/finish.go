package setup

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
)

// postFinish is the only irreversible request in the wizard. It reads the
// confirmation, commits everything in one transaction, writes the configuration
// file and then wakes the server.
//
// It used to ask for an Anthropic key here. It does not any more: the coaching
// is a plugin, the plugin holds its own key, and asking for a vendor credential
// during installation was asking an operator to make a decision before they had
// seen the thing it was for.
func (w *Wizard) postFinish(rw http.ResponseWriter, r *http.Request) {
	s, release := w.begin(rw, r, stepFinish)
	if s == nil {
		return
	}
	defer release()
	cfg, settings, err := w.commit(r.Context(), s)
	if err != nil {
		// Setup has already been completed by somebody else — the loser of a
		// race, or an operator who opened two tabs. Say so plainly and switch
		// the server over, because the database is finished either way.
		if errors.Is(err, db.ErrSetupAlreadyComplete) {
			w.deps.Log.LogAttrs(r.Context(), slog.LevelWarn, "setup was already completed",
				slog.String("stage", "commit"))
			w.finished()
			w.clearCookies(rw)
			w.render(rw, r, "blocked", pageData{
				Title:  "Setup",
				Status: http.StatusConflict,
				Form: blockedForm{
					Heading:  "Setup has already been completed",
					Body:     "Another request finished setup first, so this one changed nothing. Sign in to the admin panel with the account that was created.",
					AdminURL: "/admin",
				},
			})
			return
		}
		w.deps.Log.LogAttrs(r.Context(), slog.LevelError, "setup could not finish",
			slog.String("reason", db.ReasonOf(err)), slog.Any("error", err))
		w.renderStep(rw, r, s, stepFinish, describeError(err))
		return
	}

	s.step = stepDone

	w.deps.Log.LogAttrs(r.Context(), slog.LevelInfo, "setup completed",
		slog.String("organisation", settings.Organisation),
		slog.String("public_host", settings.PublicHost),
		slog.String("tls_mode", string(settings.TLSMode)))

	w.renderDone(rw, r, s, settings, cfg)
	w.finished()
}

// getDone re-renders the final page for anyone who refreshes it.
func (w *Wizard) getDone(rw http.ResponseWriter, r *http.Request) {
	s := w.session(r)
	if s == nil {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step != stepDone {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return
	}
	w.renderDone(rw, r, s, config.DefaultSettings(s.organisation, s.host, s.tlsMode), w.deps.Base)
}

// renderDone writes the final page. The caller holds s.mu.
func (w *Wizard) renderDone(rw http.ResponseWriter, r *http.Request, s *wizardSession, settings config.Settings, _ config.Config) {
	form := doneForm{Email: s.email, AdminURL: "/admin"}
	if settings.TLSMode == config.TLSAuto {
		form.AdminURL = settings.BaseURL() + "/admin"
		form.PortChange = "This server now listens on 443 for drivers and on 80 for the certificate check, so the port you used for setup is no longer the one to use."
	}
	w.render(rw, r, "done", pageData{
		Title: "Done",
		Steps: stepViews(stepDone),
		Form:  form,
	})
}
