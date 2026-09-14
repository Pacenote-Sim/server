//go:build postgres

package admin_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/db"
)

// TestPanelFormsNeedTheirCSRFToken covers every route on the three pages that
// changes something.
//
// A signed-in session is the prerequisite, which is why these are here and not
// beside the tests that need no database: the session check runs first, so a
// form posted by a stranger is a redirect to the sign-in page and never reaches
// the token check at all. The case worth pinning is the other one — a real
// session, a real page, and a form that came from somewhere else.
func TestPanelFormsNeedTheirCSRFToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"revoking a machine, no token", "/admin/devices/revoke", url.Values{"device_id": {"1"}}},
		{"revoking a machine, a token from somewhere else", "/admin/devices/revoke", url.Values{"csrf": {"nope"}, "device_id": {"1"}}},
		{"revoking a driver's machines, no token", "/admin/devices/revoke-all", url.Values{"driver_id": {"1"}}},
		{"revoking a driver's machines, a token from somewhere else", "/admin/devices/revoke-all", url.Values{"csrf": {"nope"}, "driver_id": {"1"}}},
		{"saving retention, no token", "/admin/data/retention", url.Values{"trace_months": {"6"}}},
		{"saving retention, a token from somewhere else", "/admin/data/retention", url.Values{"csrf": {"nope"}, "trace_months": {"6"}}},
		{"pruning, no token", "/admin/data/prune", url.Values{"confirm": {admin.PruneConfirmation}}},
		{"pruning, a token from somewhere else", "/admin/data/prune", url.Values{"csrf": {"nope"}, "confirm": {admin.PruneConfirmation}}},
		{"building a client, no token", admin.ClientsPath + "/build", url.Values{"address": {"https://pacenote.example.com"}}},
		{"building a client, a token from somewhere else", admin.ClientsPath + "/build", url.Values{"csrf": {"nope"}, "address": {"https://pacenote.example.com"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			p := newPanel(t)

			// Something to destroy, so that "nothing changed" is an assertion
			// about behaviour rather than about an empty database.
			driver := p.seedDriver("Marta Ferrer", "")
			token, device := p.seedDevice(driver.ID, "race laptop")
			old := time.Now().AddDate(0, -4, 0)
			stint := p.seedStint(driver.ID, device.ID, "Jerez", "Cup", old, []seedLap{
				{Number: 1, LapMs: 95_000, StartedAt: old, Trace: traceOf(1024)},
			})
			p.setRetention(1)

			r.Equal(http.StatusForbidden, p.post(tc.path, tc.form))
			r.Contains(string(p.lastBody), "stale")

			r.Equal(http.StatusOK, p.apiStatus(token), "no machine was signed out")
			r.Equal(1024, p.traceBytesOf(stint), "no trace was cleared")
			r.Zero(p.auditCount(db.ActionDeviceRevoked))
			r.Zero(p.auditCount(db.ActionDriverDevicesRevoked))
			r.Zero(p.auditCount(db.ActionRetentionChanged))
			r.Zero(p.auditCount(db.ActionTracesPruned))
			r.Zero(p.auditCount(db.ActionClientBuilt))
		})
	}
}
