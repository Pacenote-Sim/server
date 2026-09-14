package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/httpx"
)

// DiscoveryMaxAge is how long a client may keep the discovery document. Five
// minutes is what the contract says, and it is the right answer: the document
// changes when an operator changes a setting, and a driver who is mid-session
// when that happens should not be re-fetching it every lap to find out.
const DiscoveryMaxAge = 5 * 60

// getDiscovery serves the one unauthenticated document everything else follows
// from. It is built from what the operator configured and what this
// installation actually has running, so a server with no coaching plugin has no
// coaching features in it and a client reading it turns those buttons off.
func (a *API) getDiscovery(w http.ResponseWriter, r *http.Request) {
	if err := a.current(r.Context()); err != nil {
		a.deps.Log.LogAttrs(r.Context(), slog.LevelWarn,
			"the settings could not be read, so discovery may be stale", slog.Any("error", err))
	}
	settings, features, _, _ := a.snapshot()

	doc := settings.Discovery(features, MinClient)
	doc.PrivacyURI = settings.BaseURL() + "/privacy"

	// Public rather than private: the document names no driver and holds no
	// credential, so a team's own proxy caching one copy for everybody is
	// exactly right.
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(DiscoveryMaxAge))
	httpx.JSON(w, http.StatusOK, doc)
}

// Discovery renders the document without serving it, for the admin panel and
// for the tests that assert the contract against a golden fixture.
func Discovery(settings config.Settings, features []wire.Feature) wire.Discovery {
	doc := settings.Discovery(features, MinClient)
	doc.PrivacyURI = settings.BaseURL() + "/privacy"
	return doc
}
