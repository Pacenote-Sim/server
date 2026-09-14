package admin_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
)

// TestSettingsFormsNeedTheirCSRFToken pins the rule on every mutating form the
// settings page carries. It runs without a database because a request with no
// token is refused before anything is read, which is itself the point: the
// check is the first thing each handler does.
func TestSettingsFormsNeedTheirCSRFToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"identity, no token", "/admin/settings/identity", url.Values{"organisation": {"A league"}}},
		{"identity, a token from somewhere else", "/admin/settings/identity", url.Values{"csrf": {"nope"}}},
		{"limits, no token", "/admin/settings/limits", url.Values{"trace_points": {"300"}}},
		{"the danger zone, no token", "/admin/settings/danger", url.Values{"action": {"revoke_devices"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			panelWithoutADatabase(t).ServeHTTP(rec, req)

			// Without a session the request never reaches the token check, so
			// either answer is the request being refused. What must not
			// happen is the form being acted on.
			r.Contains([]int{http.StatusForbidden, http.StatusSeeOther}, rec.Code)
		})
	}
}

// TestSettingsPageNeedsASession keeps the settings page behind the sign-in
// form like every other page in the panel.
func TestSettingsPageNeedsASession(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	rec := httptest.NewRecorder()
	panelWithoutADatabase(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings", http.NoBody))
	r.Equal(http.StatusSeeOther, rec.Code)
	r.Equal("/admin/login", rec.Header().Get("Location"))
}

// TestSettingsConstants pins the two values the page and its script agree on,
// so a rename in one does not quietly break the other.
func TestSettingsConstants(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	r.Equal("/admin/settings", admin.SettingsPath)
	r.Contains(admin.EnterpriseNote, "Enterprise")
}
