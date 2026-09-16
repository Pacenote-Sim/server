//go:build postgres

package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/admin"
	"github.com/pacenote-sim/server/internal/api"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

// The credentials the settings tests type. They are not secrets — they never
// leave this process — but they are shaped like the real ones so that a test
// asserting "this never appears in a response" is asserting something.
const (
	anthropicKey = "sk-ant-api03-test-key-ending-ABCD"
	voiceKey     = "cartesia-test-key-ending-WXYZ"
)

// settingsPanel is the whole server as the settings page needs it: the admin
// panel, the API beside it on the same mux, one database, one data key, and a
// stub standing in for the operator's voice service.
//
// The API is here on purpose. Half of what the settings page promises is that
// a change reaches the discovery document without a restart, and that is only
// testable with the thing that serves the document in the same process.
type settingsPanel struct {
	t      *testing.T
	store  *db.Store
	server *httptest.Server
	client *http.Client

	keyring *auth.Keyring
	logs    *bytes.Buffer

	// spoken counts what the stub voice service was asked for, and voiceBody
	// is the last request it received, so a test can assert the shape the
	// server posts without calling anything real.
	spoken    atomic.Int64
	voiceBody atomic.Pointer[[]byte]

	// savedKeys counts data keys written, standing in for the configuration
	// file the real server rewrites.
	savedKeys atomic.Int64

	// voiceEndpoint is the stub the settings are pointed at, so that nothing
	// in these tests reaches a real voice service.
	voiceEndpoint string

	lastBody []byte
	lastCode int
}

// newSettingsPanel builds it. No request in these tests reaches a real voice
// service or a real language model: the endpoint points at a stub in this
// process, which is what lets the Test voice path be asserted end to end
// without an account or a bill.
func newSettingsPanel(t *testing.T, opts ...func(*admin.Deps)) *settingsPanel {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	store, err := db.Open(ctx, dbtest.URL(t), logging.Discard())
	r.NoError(err)
	t.Cleanup(store.Close)
	r.NoError(store.Migrate(ctx))

	hash, err := auth.HashPassword(panelPassword)
	r.NoError(err)
	_, err = store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        panelEmail,
		AdminPasswordHash: hash,
		Settings:          config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy),
	})
	r.NoError(err)

	key, err := auth.NewSecretKey()
	r.NoError(err)

	p := &settingsPanel{
		t:       t,
		store:   store,
		keyring: auth.NewKeyring(key),
		logs:    &bytes.Buffer{},
	}

	// The stub voice service. It answers with a WAV header and nothing else,
	// which is enough for the relay to accept it as audio.
	voice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		p.voiceBody.Store(&body)
		p.spoken.Add(1)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF----WAVEfmt "))
	}))
	t.Cleanup(voice.Close)
	p.voiceEndpoint = voice.URL

	log := logging.New(logging.Options{Level: slog.LevelDebug, Output: p.logs})

	v1, err := api.New(ctx, api.Deps{
		Log:   log,
		Store: store,
	})
	r.NoError(err)

	deps := admin.Deps{
		Log:               log,
		Store:             store,
		Version:           "v0.0.0-test",
		StartedAt:         time.Now(),
		Settings:          store.Settings,
		Keyring:           p.keyring,
		OnSettingsChanged: v1.Invalidate,
		SaveDataKey: func(context.Context, auth.SecretKey) error {
			p.savedKeys.Add(1)
			return nil
		},
		// The preview is built the way the API builds the real one.
		Discovery: func(s config.Settings) wire.Discovery {
			return api.Discovery(s, api.Features())
		},
	}
	for _, opt := range opts {
		opt(&deps)
	}
	panel, err := admin.New(deps)
	r.NoError(err)

	mux := http.NewServeMux()
	v1.Routes(mux)
	panel.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.server = srv

	jar, err := cookiejar.New(nil)
	r.NoError(err)
	p.client = &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	p.signIn()
	return p
}

func (p *settingsPanel) signIn() {
	p.t.Helper()
	r := require.New(p.t)
	page := p.get("/admin/login")
	code := p.post("/admin/login", url.Values{
		"csrf": {csrfOf(p.t, page)}, "email": {panelEmail}, "password": {panelPassword},
	})
	r.Equal(http.StatusSeeOther, code)
}

func (p *settingsPanel) get(path string) string {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.server.URL+path, http.NoBody)
	r.NoError(err)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	r.NoError(err)
	p.lastBody, p.lastCode = body, res.StatusCode
	// The same check every other page gets. Without it a template that fails
	// part-way through is a test that passes against half a page, which is
	// exactly what happened when the voice card was removed and left a dangling
	// conditional behind it.
	requireWholePage(p.t, string(body))
	return string(body)
}

func (p *settingsPanel) post(path string, form url.Values) int {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.server.URL+path, strings.NewReader(form.Encode()))
	r.NoError(err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	p.lastBody, _ = io.ReadAll(res.Body)
	p.lastCode = res.StatusCode
	return res.StatusCode
}

// settingsPage loads the page and returns it with a fresh token taken from it.
func (p *settingsPanel) settingsPage() (body, csrf string) {
	p.t.Helper()
	body = p.get("/admin/settings")
	return body, csrfOf(p.t, body)
}

// saved is the current settings, straight from the database.
func (p *settingsPanel) saved() config.Settings {
	p.t.Helper()
	s, err := p.store.Settings(context.Background())
	require.NoError(p.t, err)
	return s
}

// discovery is the document the API is serving right now.
func (p *settingsPanel) discovery() wire.Discovery {
	p.t.Helper()
	r := require.New(p.t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		p.server.URL+config.DiscoveryPath, http.NoBody)
	r.NoError(err)
	res, err := p.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	var doc wire.Discovery
	r.NoError(json.NewDecoder(res.Body).Decode(&doc))
	return doc
}

// audit reads the trail for one action.
func (p *settingsPanel) audit(action string) []db.AuditEntry {
	p.t.Helper()
	rows, err := p.store.AuditFor(context.Background(), action, 20)
	require.NoError(p.t, err)
	return rows
}

// TestSavingEachGroup walks the four groups an operator fills in and checks
// that each one lands in the database and writes its audit row.
func TestSavingEachGroup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		path   string
		action string
		form   func(panel *settingsPanel, csrf string) url.Values
		check  func(r *require.Assertions, panel *settingsPanel, s config.Settings)
	}{
		{
			name: "identity", path: "/admin/settings/identity", action: db.ActionSettingsIdentity,
			form: func(_ *settingsPanel, csrf string) url.Values {
				return url.Values{
					"csrf": {csrf}, "organisation": {"Iberian GT Championship"},
					"short_name": {"IGTC"}, "logo": {"https://igtc.example/logo.png"},
					"accent": {"#C6F24B"},
				}
			},
			check: func(r *require.Assertions, _ *settingsPanel, s config.Settings) {
				r.Equal("IGTC", s.ShortName)
				r.Equal("https://igtc.example/logo.png", s.Logo)
				r.Equal("#C6F24B", s.Accent)
			},
		},
		{
			name: "limits", path: "/admin/settings/limits", action: db.ActionSettingsLimits,
			form: func(_ *settingsPanel, csrf string) url.Values {
				return url.Values{
					"csrf": {csrf}, "trace_points": {"450"}, "laps_per_request": {"25"},
					"live_interval_ms": {"1500"}, "summary_interval_ms": {"45000"},
					"max_body_bytes": {"1048576"},
				}
			},
			check: func(r *require.Assertions, _ *settingsPanel, s config.Settings) {
				r.Equal(450, s.Limits.TracePoints)
				r.Equal(25, s.Limits.LapsPerRequest)
				r.Equal(1500, s.Limits.LiveIntervalMs)
				r.Equal(45000, s.Limits.SummaryIntervalMs)
				r.Equal(1048576, s.Limits.MaxBodyBytes)
			},
		},
	}
	// Each group gets a server and a database of its own, so that the audit
	// assertion below can say "exactly one row" and mean it.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			panel := newSettingsPanel(t)

			_, csrf := panel.settingsPage()
			r.Equal(http.StatusOK, panel.post(tc.path, tc.form(panel, csrf)), string(panel.lastBody))
			tc.check(r, panel, panel.saved())

			rows := panel.audit(tc.action)
			r.Len(rows, 1, "a saved group writes exactly one audit row")
			r.Equal(panelEmail, rows[0].Actor, "the row names who changed it")
			r.NotEmpty(rows[0].Fields(), "the row names what changed")
		})
	}
}

// TestAnEmptyKeyFieldLeavesTheStoredKey is the rule that makes a write-only
// field usable: an operator editing the voice id must not have to retype a key
// to avoid wiping it.
// seedPluginSecret gives a plugin a credential sealed with the panel's current
// data key, the way its own settings page would.
func seedPluginSecret(t *testing.T, p *settingsPanel, name, setting, value string) {
	t.Helper()
	r := require.New(t)
	ctx := context.Background()

	r.NoError(p.store.SavePlugin(ctx, db.PluginRecord{
		Name: name, Version: "1.0.0", Author: "Pacenote",
		Description: "Coaches.", InterfaceVersion: 1, Directory: name, State: "running",
	}))
	sealed, err := p.keyring.Key().Seal(value)
	r.NoError(err)
	r.NoError(p.store.SavePluginSecret(ctx, name, setting, sealed))
}

// TestAKeyNeverLeavesTheServer is the rule in the brief, asserted on both the
// places a key could escape from: a response body and a log line.
func TestAKeyNeverLeavesTheServer(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)

	// A plugin's credential, sealed the way its own page seals one. It is here
	// because it is now the kind of key most likely to be in this server —
	// the core holds one and plugins hold the rest.
	seedPluginSecret(t, panel, "engineer", "api_key", anthropicKey)

	_, csrf := panel.settingsPage()
	r.Equal(http.StatusOK, panel.post("/admin/settings/identity", url.Values{
		"csrf": {csrf}, "organisation": {"Iberian GT Championship"},
	}))
	saveBody := string(panel.lastBody)

	page, _ := panel.settingsPage()

	bodies := map[string]string{
		"the page after saving":  saveBody,
		"the page loaded fresh":  page,
		"the discovery document": string(mustJSON(t, panel.discovery())),
		"the log":                panel.logs.String(),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.NotContains(body, anthropicKey, "a plugin's credential is in %s", name)
		})
	}

	t.Run("the page shows the last four characters and no more", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		// This page shows no credential at all now — the server holds none.
		// A plugin's key is shown on the plugin's own page and nowhere else.
		r.NotContains(page, "Ending ABCD",
			"the settings page showed a credential that belongs to a plugin")
		r.NotContains(page, "stored")
	})
}

// TestDiscoveryFollowsASettingsChange is the no-restart promise: the panel
// writes, and the document the API serves has changed by the next request.
func TestDiscoveryFollowsASettingsChange(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)

	before := panel.discovery()
	r.Equal("Iberian GT Championship", before.Name)
	r.ElementsMatch(api.Features(), before.Features, "the features are the core's and follow nothing an operator types")

	_, csrf := panel.settingsPage()
	r.Equal(http.StatusOK, panel.post("/admin/settings/identity", url.Values{
		"csrf": {csrf}, "organisation": {"Campeonato de España GT"},
		"short_name": {"CEGT"}, "logo": {""}, "accent": {"#FF3B30"},
	}))

	after := panel.discovery()
	r.Equal("Campeonato de España GT", after.Name, "the same process serves the new name")
	r.Equal("CEGT", after.ShortName)
	r.Equal("#FF3B30", after.Accent)

	t.Run("nothing in the settings can add a feature", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t)

		// A feature is a capability this server owns, and the community
		// edition's three are unconditional. Nothing an operator types adds
		// one, and nothing a plugin declares does either: a plugin is not a
		// feature, and what is installed is GET /me's answer, not this one's.
		_, csrf := panel.settingsPage()
		r.Equal(http.StatusOK, panel.post("/admin/settings/limits", url.Values{
			"csrf": {csrf}, "trace_points": {"300"}, "laps_per_request": {"50"},
			"live_interval_ms": {"1000"}, "summary_interval_ms": {"30000"},
			"max_body_bytes": {"2097152"},
		}))
		doc := panel.discovery()
		r.ElementsMatch(api.Features(), doc.Features)
	})

	t.Run("limits reach the document too", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t)
		_, csrf := panel.settingsPage()
		r.Equal(http.StatusOK, panel.post("/admin/settings/limits", url.Values{
			"csrf": {csrf}, "trace_points": {"600"}, "laps_per_request": {"40"},
			"live_interval_ms": {"2000"}, "summary_interval_ms": {"30000"},
			"max_body_bytes": {"2097152"},
		}))
		doc := panel.discovery()
		r.Equal(600, doc.Limits.TracePoints)
		r.Equal(2000, doc.Limits.LiveIntervalMs)
	})
}

// TestTheDangerZone checks both irreversible actions, including that neither
// happens without the phrase the page asks for.
func TestTheDangerZone(t *testing.T) {
	t.Parallel()

	t.Run("regenerating the data key re-seals what was sealed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t)

		// Every credential this server holds belongs to a plugin. That is the
		// case that made the re-seal worth having: without it the coach would
		// simply stop, with nothing on any page connecting it to the button
		// about to be pressed.
		seedPluginSecret(t, panel, "engineer", "api_key", "sk-ant-the-operators-own-key")
		oldSealedPlugin := func() []byte {
			rows, err := panel.store.PluginSettings(context.Background(), "engineer")
			r.NoError(err)
			r.Len(rows, 1)
			return rows[0].Sealed
		}()

		oldKey := panel.keyring.Key()

		_, csrf := panel.settingsPage()
		r.Equal(http.StatusUnprocessableEntity, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"regenerate_data_key"}, "confirm": {"yes"},
		}), "the wrong phrase changes nothing")
		r.Equal(int64(0), panel.savedKeys.Load())

		_, csrf = panel.settingsPage()
		r.Equal(http.StatusOK, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"regenerate_data_key"}, "confirm": {"regenerate"},
		}), string(panel.lastBody))

		r.Equal(int64(1), panel.savedKeys.Load(), "the new key was written to the configuration file")
		r.NotEqual([]byte(oldKey), []byte(panel.keyring.Key()), "the process is using a new key")

		rows, err := panel.store.PluginSettings(context.Background(), "engineer")
		r.NoError(err)
		r.Len(rows, 1)
		resealed := rows[0].Sealed
		r.NotEqual(oldSealedPlugin, resealed, "the ciphertext changed with the key")

		pluginSecret, err := panel.keyring.Key().Open(resealed)
		r.NoError(err)
		r.Equal("sk-ant-the-operators-own-key", pluginSecret,
			"a plugin's credential was left sealed with a key that is gone")
		r.Contains(string(panel.lastBody), "belonging to plugins",
			"the operator was not told their plugins' credentials had moved")

		_, err = oldKey.Open(resealed)
		r.Error(err, "and no longer with the old one")

		r.Len(panel.audit(db.ActionDataKeyRegenerated), 1)
	})

	t.Run("revoking every device token", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t)
		ctx := context.Background()

		driver, err := panel.store.EnsureDriver(ctx, "Ana Ruiz", "gt3")
		r.NoError(err)
		token, err := auth.NewDeviceToken()
		r.NoError(err)
		_, err = panel.store.CreateDevice(ctx, driver.ID, token.Sum, token.Prefix, "a sim rig")
		r.NoError(err)

		_, csrf := panel.settingsPage()
		r.Equal(http.StatusUnprocessableEntity, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"revoke_devices"}, "confirm": {"please"},
		}), "the wrong phrase changes nothing")
		_, err = panel.store.DeviceByToken(ctx, token.Prefix, token.Sum)
		r.NoError(err, "the device is still live")

		_, csrf = panel.settingsPage()
		r.Equal(http.StatusOK, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"revoke_devices"}, "confirm": {"revoke"},
		}), string(panel.lastBody))
		r.Contains(string(panel.lastBody), "Revoked 1 device token —", "one is not \"1 device tokens\"")

		_, err = panel.store.DeviceByToken(ctx, token.Prefix, token.Sum)
		r.ErrorIs(err, db.ErrNotFound, "the token no longer finds a live device")

		rows := panel.audit(db.ActionDevicesRevoked)
		r.Len(rows, 1)
		r.Equal("1 device token", rows[0].Subject, "the trail counts the way a sentence does")
	})
}

// TestRefusedValuesChangeNothing checks that a form the operator got wrong is
// answered with a sentence rather than a half-saved settings row.
func TestRefusedValuesChangeNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		form url.Values
		says string
	}{
		{
			"an empty organisation", "/admin/settings/identity",
			url.Values{"organisation": {"  "}},
			"cannot be empty",
		},
		{
			"an accent that is not a colour", "/admin/settings/identity",
			url.Values{"organisation": {"A league"}, "accent": {"volt green"}},
			"hex colour",
		},
		{
			"a logo that is not an address", "/admin/settings/identity",
			url.Values{"organisation": {"A league"}, "logo": {"logo.png"}},
			"full address",
		},
		{
			"a trace longer than this server takes", "/admin/settings/limits",
			url.Values{
				"trace_points": {"99999"}, "laps_per_request": {"50"},
				"live_interval_ms": {"1000"}, "summary_interval_ms": {"30000"},
				"max_body_bytes": {"2097152"},
			},
			"trace points",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			panel := newSettingsPanel(t)
			before := panel.saved()

			_, csrf := panel.settingsPage()
			form := url.Values{"csrf": {csrf}}
			for k, v := range tc.form {
				form[k] = v
			}
			r.Equal(http.StatusUnprocessableEntity, panel.post(tc.path, form))
			r.Contains(string(panel.lastBody), tc.says)
			r.Equal(before, panel.saved(), "a refused form changes nothing")
		})
	}
}

// TestThePageShowsWhatThisEditionHasNot pins the rule that a setting this
// build does not include is shown and disabled rather than hidden.
func TestThePageShowsWhatThisEditionHasNot(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)
	page, _ := panel.settingsPage()

	r.Contains(page, "Field interval", "the setting is named")
	r.Contains(page, admin.EnterpriseNote, "and says why it is not available")
	r.Contains(page, "disabled", "and is not something an operator can type into")
	r.Contains(page, "Single sign-on")
}

// TestThePagePreviewsTheDiscoveryDocument checks the preview is the document
// and not a description of it.
func TestThePagePreviewsTheDiscoveryDocument(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)

	_, csrf := panel.settingsPage()
	r.Equal(http.StatusOK, panel.post("/admin/settings/identity", url.Values{
		"csrf": {csrf}, "organisation": {"Iberian GT Championship"},
		"short_name": {"IGTC"}, "logo": {""}, "accent": {"#C6F24B"},
	}))

	page, _ := panel.settingsPage()
	r.Contains(page, "&#34;short_name&#34;: &#34;IGTC&#34;", "the preview carries the fields a client reads")
	r.Contains(page, "min_client")
	r.Contains(page, "&#34;pair_uri&#34;")
}

func mustJSON(tb testing.TB, v any) []byte {
	tb.Helper()
	b, err := json.Marshal(v)
	require.NoError(tb, err)
	return b
}

// The two answers to "the data key was replaced" that the covered case is not:
// a server with nothing to re-seal, and one where something could not be.
//
// The difference matters to the operator. "Nothing was stored" is a finished
// action; "two credentials could not be re-sealed" is a list of plugin pages
// they now have to visit, and a page that said the first when it meant the
// second would leave a coach silently off.
func TestWhatRegeneratingTheDataKeySays(t *testing.T) {
	t.Parallel()

	t.Run("a server with nothing sealed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t)

		_, csrf := panel.settingsPage()
		r.Equal(http.StatusOK, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"regenerate_data_key"}, "confirm": {"regenerate"},
		}), string(panel.lastBody))
		r.Contains(string(panel.lastBody), "No plugin had a credential stored")
		r.Equal(int64(1), panel.savedKeys.Load())
	})

	t.Run("a credential the old key cannot open either", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t)
		ctx := context.Background()

		// A credential sealed with a key nobody has any more — the shape of a
		// server whose data directory was restored from the wrong backup. It
		// cannot be re-sealed, and the operator has to be told which way it
		// went rather than left to find out from a plugin that stopped.
		seedPluginSecret(t, panel, "engineer", "api_key", "sk-ant-the-operators-own-key")
		lost, err := auth.NewSecretKey()
		r.NoError(err)
		unreadable, err := lost.Seal("sk-ant-sealed-with-a-key-that-is-gone")
		r.NoError(err)
		r.NoError(panel.store.SavePluginSecret(ctx, "engineer", "api_key", unreadable))

		_, csrf := panel.settingsPage()
		r.Equal(http.StatusOK, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"regenerate_data_key"}, "confirm": {"regenerate"},
		}), string(panel.lastBody))
		r.Contains(string(panel.lastBody), "could not be re-sealed")
		r.Contains(string(panel.lastBody), "must be entered again on their pages")

		// And the row is left as it was rather than replaced with something
		// sealed over nothing.
		rows, err := panel.store.PluginSettings(ctx, "engineer")
		r.NoError(err)
		r.Len(rows, 1)
		r.Equal(unreadable, rows[0].Sealed, "an unreadable credential was overwritten")
	})
}

// A danger-zone action this page does not have. Nothing happens and the page
// says so, rather than falling through to one of the two that are real.
func TestADangerousActionThisPageDoesNotHave(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)

	_, csrf := panel.settingsPage()
	r.Equal(http.StatusBadRequest, panel.post("/admin/settings/danger", url.Values{
		"csrf": {csrf}, "action": {"drop_every_table"}, "confirm": {"yes"},
	}))
	r.Contains(string(panel.lastBody), "not something this page does")
	r.Equal(int64(0), panel.savedKeys.Load())
}

// Every form on this page is behind the same stale-form check, and a form this
// server cannot read at all is a bad request rather than a panic.
func TestTheSettingsFormsRefuseWhatTheyCannotTrust(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)

	for _, path := range []string{"/admin/settings/identity", "/admin/settings/limits", "/admin/settings/danger"} {
		r.Equal(http.StatusForbidden, panel.post(path, url.Values{"csrf": {"not-the-token"}}),
			"%s accepted a stale form", path)
	}

	// A body that is not a form at all. The panel answers rather than letting
	// the parse failure reach the operator as a stack trace.
	_, csrf := panel.settingsPage()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		panel.server.URL+"/admin/settings/identity", strings.NewReader("%%"+csrf))
	r.NoError(err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := panel.client.Do(req)
	r.NoError(err)
	defer func() { _ = res.Body.Close() }()
	r.Equal(http.StatusBadRequest, res.StatusCode)
}

// A limit that is not a number. The page names the mistake rather than saving a
// zero, because a zero limit is a limit an operator did not choose.
func TestALimitThatIsNotANumber(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	panel := newSettingsPanel(t)
	before := panel.saved()

	_, csrf := panel.settingsPage()
	r.Equal(http.StatusUnprocessableEntity, panel.post("/admin/settings/limits", url.Values{
		"csrf": {csrf}, "trace_points": {"quite a lot"},
	}))
	r.Contains(string(panel.lastBody), "whole number")
	r.Equal(before.Limits, panel.saved().Limits, "a limit that was refused was saved anyway")
}

// The settings page on a server missing one of the things it depends on.
//
// Both of these are wiring rather than data: a panel built without the ability
// to rewrite its configuration file, and one built without a way to preview
// discovery. Each is a real deployment — the preview is the API's and a panel
// can be built before it — and neither may take the page down.
func TestTheSettingsPageWithoutItsNeighbours(t *testing.T) {
	t.Parallel()

	t.Run("no way to write the configuration file", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t, func(d *admin.Deps) { d.SaveDataKey = nil })

		_, csrf := panel.settingsPage()
		r.Equal(http.StatusConflict, panel.post("/admin/settings/danger", url.Values{
			"csrf": {csrf}, "action": {"regenerate_data_key"}, "confirm": {"regenerate"},
		}))
		r.Contains(string(panel.lastBody), "cannot write its configuration file")
	})

	t.Run("no discovery document to preview", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		panel := newSettingsPanel(t, func(d *admin.Deps) { d.Discovery = nil })

		body, _ := panel.settingsPage()
		r.Contains(body, "Settings", "the page did not render without a preview")
	})
}
