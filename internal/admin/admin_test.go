package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/logging"
)

// panelWithoutADatabase builds the panel with a nil store. Every path exercised
// below stops before it would reach one: an anonymous visitor has no session
// cookie to look up, and a form with no CSRF token is refused before anything
// is read.
func panelWithoutADatabase(t *testing.T) http.Handler {
	t.Helper()
	p, err := admin.New(admin.Deps{
		Log:     logging.Discard(),
		Version: "v0.0.0-test",
		Settings: func(context.Context) (config.Settings, error) {
			return config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy), nil
		},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	p.Routes(mux)
	return mux
}

func TestTemplatesParse(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	r.NotNil(panelWithoutADatabase(t))
}

func TestSignInPageRenders(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	rec := httptest.NewRecorder()
	panelWithoutADatabase(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login", http.NoBody))

	r.Equal(http.StatusOK, rec.Code)
	r.Contains(rec.Body.String(), "Sign in")
	r.Contains(rec.Body.String(), "Iberian GT Championship")
	r.Contains(rec.Body.String(), `name="csrf"`)
	r.Contains(rec.Body.String(), "Drivers do not sign in here")

	var csrfSet bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == admin.CSRFCookie {
			csrfSet = true
			r.True(c.HttpOnly)
			r.Equal(http.SameSiteLaxMode, c.SameSite)
		}
	}
	r.True(csrfSet, "the sign-in form needs a token before there is a session")
}

func TestPagesNeedASession(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		want string
	}{
		{"the overview", "/admin/overview", "/admin/login"},
		{"the roster", "/admin/drivers", "/admin/login"},
		{"one driver", "/admin/drivers/1", "/admin/login"},
		{"one stint", "/admin/stints/01a099cb-0a7c-7000-a846-2001c572d8fc", "/admin/login"},
		{"the devices list", "/admin/devices", "/admin/login"},
		{"the data page", "/admin/data", "/admin/login"},
		{"the build page", "/admin/clients", "/admin/login"},
		{"a built client", "/admin/clients/download/0011223344556677", "/admin/login"},
		{"the panel root", "/admin", "/admin/overview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			rec := httptest.NewRecorder()
			panelWithoutADatabase(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, http.NoBody))
			r.Equal(http.StatusSeeOther, rec.Code)
			r.Equal(tc.want, rec.Header().Get("Location"))
		})
	}
}

func TestMutatingFormsNeedTheirCSRFToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"sign in, no token", "/admin/login", url.Values{"email": {"ana@example.com"}, "password": {"x"}}},
		{"sign in, a token from somewhere else", "/admin/login", url.Values{"csrf": {"nope"}}},
		{"sign out, no token", "/admin/logout", url.Values{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			panelWithoutADatabase(t).ServeHTTP(rec, req)
			r.Equal(http.StatusForbidden, rec.Code)
			r.Contains(rec.Body.String(), "stale")
		})
	}
}

func TestNavigationNamesThePagesThatFollow(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// The sign-in page carries no navigation; the seam is asserted through the
	// end-to-end test in internal/app, which signs in. Here we only pin that
	// the panel is mounted where the rest of the server expects it, and that
	// the three pages that filled the seam are mounted under it.
	r.Equal("/admin", admin.Prefix)
	r.Equal("pacenote_admin", admin.SessionCookie)
	r.Equal("/admin/drivers", admin.DriversPath)
	r.Equal("/admin/devices", admin.DevicesPath)
	r.Equal("/admin/data", admin.DataPath)
	r.Equal("/admin/stints", admin.StintsPath)
	r.Equal("/admin/clients", admin.ClientsPath)
	r.Equal("/admin/clients/download/", admin.ClientDownloadPath)
}
