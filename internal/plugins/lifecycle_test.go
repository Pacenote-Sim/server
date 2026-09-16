package plugins_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/plugins"
)

// statusIn finds one plugin in a list, or fails saying what was there instead.
//
//nolint:unparam // there is one plugin in these tests; naming it at the call is the point.
func statusIn(tb testing.TB, all []plugins.Status, name string) plugins.Status {
	tb.Helper()
	var names []string
	for _, s := range all {
		if s.Name == name {
			return s
		}
		names = append(names, s.Name)
	}
	tb.Fatalf("%s is not in the list; it holds %v", name, names)
	return plugins.Status{}
}

// Disabling stops the process and keeps everything else. An operator turning a
// plugin off for an evening must not lose what they configured, and must not
// find it running again after a restart.
func TestDisableStopsThePluginAndSticks(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.NoError(h.host.Disable(t.Context(), "testplugin"))

	// No process: a disabled plugin that was still running would be a switch
	// that does nothing.
	r.Empty(stateOf(h.host, "testplugin"), "the instance was left supervised")
	_, err := h.host.Ask(t.Context(), "testplugin", echoRequest("testplugin"))
	r.Error(err, "a disabled plugin answered a request")

	all, err := h.host.All(t.Context())
	r.NoError(err)
	got := statusIn(t, all, "testplugin")
	r.False(got.Enabled)
	r.Equal(plugins.StateDisabled, got.State)
	r.True(got.Installed, "disabling is not uninstalling")

	// A rescan does not undo it, and neither does a restart of the server.
	r.NoError(h.host.Discover(t.Context()))
	r.Empty(stateOf(h.host, "testplugin"), "a rescan restarted a disabled plugin")

	again := restart(t, h)
	r.NoError(again.host.Discover(t.Context()))
	r.Empty(stateOf(again.host, "testplugin"), "a restart of the server ignored the operator's choice")

	all, err = again.host.All(t.Context())
	r.NoError(err)
	r.Equal(plugins.StateDisabled, statusIn(t, all, "testplugin").State)
}

func TestEnableStartsAPluginThatWasTurnedOff(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	r.NoError(h.host.Disable(t.Context(), "testplugin"))

	r.NoError(h.host.Enable(t.Context(), "testplugin"))
	eventually(t, "the plugin to start again", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	res, err := h.host.Ask(t.Context(), "testplugin", echoRequest("testplugin"))
	r.NoError(err)
	r.NotEmpty(res.Payload)

	// Pressing it twice is not an error, and does not start a second process.
	r.NoError(h.host.Enable(t.Context(), "testplugin"))
	r.Equal(plugins.StateRunning, stateOf(h.host, "testplugin"))

	all, err := h.host.All(t.Context())
	r.NoError(err)
	r.True(statusIn(t, all, "testplugin").Enabled)
}

func TestRestartBringsAPluginBack(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.NoError(h.host.Restart(t.Context(), "testplugin"))
	eventually(t, "the plugin to answer again", func() bool {
		_, err := h.host.Ask(t.Context(), "testplugin", echoRequest("testplugin"))
		return err == nil
	})

	// Restarting one that is turned off is refused rather than quietly turning
	// it back on: the operator's choice is not something a different button
	// undoes.
	r.NoError(h.host.Disable(t.Context(), "testplugin"))
	err := h.host.Restart(t.Context(), "testplugin")
	r.Error(err)
	r.Contains(err.Error(), "turned off")
}

// A plugin whose directory was deleted keeps its row, its settings and its
// database, so an operator can find it — and the panel has to be able to say
// that its files are gone rather than showing it as merely broken.
func TestAllShowsAPluginWhoseFilesAreGone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.NoError(os.RemoveAll(dir))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to stop", func() bool { return stateOf(h.host, "testplugin") != plugins.StateRunning })

	all, err := h.host.All(t.Context())
	r.NoError(err)
	got := statusIn(t, all, "testplugin")
	r.False(got.Installed, "a plugin with no files was reported as installed")
	r.Equal("1.0.0", got.Version, "what was known about it is still shown")
	r.NotEmpty(got.Description)
}

// All renders a plugin that has no process from its row, including the two
// declarations, which are stored as the JSON they arrived as.
func TestAllRendersADisabledPluginFromItsRow(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to declare its settings", func() bool {
		return len(statusOf(t, h.host, "testplugin").Settings) > 0
	})
	r.NoError(h.host.Disable(t.Context(), "testplugin"))

	got, err := h.host.Status(t.Context(), "testplugin")
	r.NoError(err)
	r.Equal("testplugin", got.Name)
	r.Equal("Pacenote", got.Author)
	r.True(got.Capabilities.Wants(plugin.EventLapCompleted),
		"the capability declaration did not survive being read back from the row")
	r.NotEmpty(got.Settings, "the settings declaration did not survive, so the form would be empty")
	r.False(got.Enabled)
}

func TestLifecycleRefusesAPluginItHasNeverSeen(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	for _, act := range []struct {
		name string
		call func() error
	}{
		{"enable", func() error { return h.host.Enable(t.Context(), "neverheardof") }},
		{"disable", func() error { return h.host.Disable(t.Context(), "neverheardof") }},
		{"restart", func() error { return h.host.Restart(t.Context(), "neverheardof") }},
	} {
		r.ErrorIs(act.call(), db.ErrNotFound, "%s accepted a plugin that does not exist", act.name)
	}

	_, err := h.host.Status(t.Context(), "neverheardof")
	r.ErrorIs(err, db.ErrNotFound)
}

func TestLifecycleReportsAStoreThatWillNotWrite(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	h.store.failSaveEnabled = errors.New("the plugins table is not there")
	r.Error(h.host.Disable(t.Context(), "testplugin"))
	r.Error(h.host.Enable(t.Context(), "testplugin"))

	// It is still running: a choice that could not be recorded must not be
	// half-applied, or a restart would disagree with what the operator saw.
	r.Equal(plugins.StateRunning, stateOf(h.host, "testplugin"))
}

func TestAllReportsAStoreThatWillNotRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	h.store.failPlugins = errors.New("the connection is gone")
	_, err := h.host.All(t.Context())
	r.Error(err)
	r.Contains(err.Error(), "connection is gone")
}

// A row that cannot be read is treated as enabled and logged. Refusing to start
// every plugin because one query failed would turn a hiccup into a server with
// no coaching, and the failure is already visible elsewhere.
func TestAPluginStartsWhenWhetherItIsOnCannotBeRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	h.store.failReadPlugin = errors.New("the connection is gone")

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start anyway", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})
	r.Contains(h.logs.String(), "could not be read")
}

// A host that is shutting down refuses to start anything, rather than racing the
// shutdown and leaving a process behind.
func TestLifecycleRefusesOnAClosedHost(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	r.NoError(h.host.Disable(t.Context(), "testplugin"))

	h.host.Close()

	r.ErrorIs(h.host.Enable(t.Context(), "testplugin"), plugins.ErrClosed)
	r.ErrorIs(h.host.Restart(t.Context(), "testplugin"), plugins.ErrClosed)
}

// Enabling or restarting a plugin whose files have gone is refused with the
// reason, rather than leaving the panel saying it is on when nothing is running.
func TestEnableAndRestartReportAPluginThatWillNotStart(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.NoError(h.host.Disable(t.Context(), "testplugin"))
	r.NoError(os.RemoveAll(dir))

	err := h.host.Enable(t.Context(), "testplugin")
	r.Error(err)
	r.Contains(err.Error(), "will not start")

	// It is on as far as the operator's choice goes — that was recorded before
	// the start was attempted — so the panel shows it enabled and failed, which
	// is the truth rather than a switch that silently flipped back.
	all, err2 := h.host.All(t.Context())
	r.NoError(err2)
	r.True(statusIn(t, all, "testplugin").Enabled)
	r.False(statusIn(t, all, "testplugin").Installed)

	err = h.host.Restart(t.Context(), "testplugin")
	r.Error(err)
	r.Contains(err.Error(), "would not restart")
}

// A row written before the directory was recorded — an upgrade from a version
// that did not store it — falls back to the plugin's own name, which is what
// the directory has to be called anyway.
func TestAPluginRowWithNoDirectoryFallsBackToItsName(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	r.NoError(h.host.Disable(t.Context(), "testplugin"))

	// Blank the directory the way an older row would have it.
	rec := h.store.record("testplugin")
	rec.Directory = ""
	h.store.put(rec)

	r.NoError(h.host.Enable(t.Context(), "testplugin"))
	eventually(t, "the plugin to start from a row with no directory", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	all, err := h.host.All(t.Context())
	r.NoError(err)
	r.True(statusIn(t, all, "testplugin").Installed,
		"a row with no directory was reported as having no files")
}

// The panel's two views of a plugin that is actually running.
//
// [Host.All] and [Host.Status] each have two paths: one for a plugin the host
// is supervising right now, and one built from the stored row for a plugin it
// is not. The running path is the one an operator sees on every visit to a
// working server, and it is the one that carries the live state, the restart
// count and the output — none of which is in the row.
func TestWhatThePanelSeesOfARunningPlugin(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)

	install(t, h.dir, "coach", manifestFor("coach"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to be running", func() bool {
		return stateOf(h.host, "coach") == plugins.StateRunning
	})

	one, err := h.host.Status(t.Context(), "coach")
	r.NoError(err)
	r.Equal("coach", one.Name)
	r.Equal(plugins.StateRunning, one.State, "a running plugin was described from its stored row")
	r.True(one.Installed)
	r.True(one.Enabled)
	r.NotEmpty(one.Capabilities.Events, "the live status carries nothing the row does not")

	all, err := h.host.All(t.Context())
	r.NoError(err)
	r.Len(all, 1)
	r.Equal(plugins.StateRunning, all[0].State)
	r.True(all[0].Installed)
	r.True(all[0].Enabled)
}

// The list is in name order however the plugins were found. The directory is
// read in whatever order the file system hands it over, and a panel whose list
// moved between visits would be a panel an operator cannot scan.
func TestThePluginListIsInNameOrder(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)

	for _, name := range []string{"voice", "coach", "results"} {
		install(t, h.dir, name, manifestFor(name))
	}
	r.NoError(h.host.Discover(t.Context()))

	all, err := h.host.All(t.Context())
	r.NoError(err)
	r.Len(all, 3)
	r.Equal([]string{"coach", "results", "voice"},
		[]string{all[0].Name, all[1].Name, all[2].Name})
}

// Restarting a plugin whose row predates this server recording which directory
// it came from. The name is the fallback, because that is what the directory
// was called before there was anywhere to record it — and a restart that could
// not find the files would leave an operator's button doing nothing.
func TestRestartingAPluginWhoseRowHasNoDirectory(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)
	ctx := t.Context()

	install(t, h.dir, "coach", manifestFor("coach"))
	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to be running", func() bool {
		return stateOf(h.host, "coach") == plugins.StateRunning
	})

	rec := h.store.record("coach")
	rec.Directory = ""
	h.store.put(rec)

	r.NoError(h.host.Restart(ctx, "coach"))
	eventually(t, "the plugin to come back", func() bool {
		return stateOf(h.host, "coach") == plugins.StateRunning
	})
}

// Everything in the plugin directory that is not a plugin.
//
// The rule is that a plugin is a directory. A file beside them — a downloaded
// archive an operator has not unpacked, a note to themselves — is passed over
// silently, because saying so on every rescan would be noise on the one page
// that must stay readable.
func TestWhatIsNotAPluginIsPassedOver(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)

	install(t, h.dir, "coach", manifestFor("coach"))
	r.NoError(os.WriteFile(filepath.Join(h.dir, "coach-1.2.0.zip"), []byte("PK\x03\x04"), 0o600))
	r.NoError(os.WriteFile(filepath.Join(h.dir, "notes.txt"), []byte("remember to install this"), 0o600))

	r.NoError(h.host.Discover(t.Context()))
	all, err := h.host.All(t.Context())
	r.NoError(err)
	r.Len(all, 1, "a file beside the plugins was taken for one")
	r.Equal("coach", all[0].Name)
	r.NotContains(h.logs.String(), "notes.txt", "a stray file was complained about")
}

// A plugin directory this server cannot use. Both are the operator's own file
// system and both stop the host rather than leaving it looking empty, because a
// server that quietly ran no plugins would be one nobody thinks to check.
func TestAPluginDirectoryThatCannotBeRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	r.NoError(os.WriteFile(blocked, []byte("this is a file"), 0o600))

	h := newHarness(t, func(o *plugins.Options) { o.Dir = filepath.Join(blocked, "plugins") })
	err := h.host.Discover(t.Context())
	r.Error(err)
	r.Contains(err.Error(), "cannot create the plugin directory")

	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a directory it may not")
	}
	unreadable := filepath.Join(t.TempDir(), "plugins")
	r.NoError(os.MkdirAll(unreadable, 0o700))
	r.NoError(os.Chmod(unreadable, 0o200))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })

	h2 := newHarness(t, func(o *plugins.Options) { o.Dir = unreadable })
	err = h2.host.Discover(t.Context())
	r.Error(err)
	r.Contains(err.Error(), "cannot read the plugin directory")
}

// A host that has been closed does nothing more.
//
// It is the shutdown race: a phase ends while an operator's last click is still
// in flight. Starting a plugin after Close would leave a subprocess nothing owns
// — the host is what reaps them — so the answer is a refusal rather than a
// plugin that outlives the server.
func TestAClosedHostStartsNothing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)
	ctx := t.Context()

	install(t, h.dir, "coach", manifestFor("coach"))
	install(t, h.dir, "voice", manifestFor("voice"))
	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugins to be running", func() bool {
		return stateOf(h.host, "coach") == plugins.StateRunning &&
			stateOf(h.host, "voice") == plugins.StateRunning
	})
	// Turned off before the shutdown, so the host is not supervising it and
	// turning it back on has to go all the way to starting a process.
	r.NoError(h.host.Disable(ctx, "voice"))

	h.host.Close()

	r.ErrorIs(h.host.Enable(ctx, "voice"), plugins.ErrClosed)
	r.ErrorIs(h.host.Restart(ctx, "coach"), plugins.ErrClosed)
	// A rescan is a sweep and one plugin failing never fails it, so this is a
	// line in the log rather than an error — but nothing starts.
	r.NoError(h.host.Discover(ctx))
	r.Contains(h.logs.String(), "will not be started")

	// A plugin the host is still holding is already on, and saying so is not
	// an error: the operator pressed a button for something that is true.
	r.NoError(h.host.Enable(ctx, "coach"))

	// And closing twice is what a cleanup does after a shutdown.
	r.NotPanics(h.host.Close)
}

// A plugin serving a request made of its own route.
//
// The host half of that: resolving the name, checking the plugin asked for a
// route, attaching its configuration, and taking back what it said. What the
// server does with the answer is pluginweb's; what is tested here is that a
// real plugin in a real process is handed a real request.
func TestAPluginServesARequestMadeOfIt(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)
	ctx := t.Context()

	m := manifestFor("results")
	m.Capabilities.HTTP = &plugin.HTTPCapability{Title: "Standings", Routes: []plugin.Route{
		{Path: "/", Access: plugin.AccessPublic},
	}}
	dir := install(t, h.dir, "results", m)
	// Told to report a cost, so that serving can be shown to be metered like
	// every other call into a plugin.
	misbehave(t, dir, behaviour{Spend: 40})
	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to be running", func() bool {
		return stateOf(h.host, "results") == plugins.StateRunning
	})

	access, ok := h.host.Access("results", "/standings/gt3")
	r.True(ok, "a plugin that asked for a route was not given one")
	r.Equal(plugin.AccessPublic, access)

	res, err := h.host.Serve(ctx, "results", plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/standings/gt3",
		Query:  "season=2026",
		Body:   []byte("a body"),
		Prefix: "/plugin/results",
		Caller: plugin.Caller{DriverSlug: "ana-ruiz", DriverName: "Ana Ruiz"},
	})
	r.NoError(err)
	r.Equal(http.StatusOK, res.Status)
	r.Equal("application/json", res.Header.Get("Content-Type"))

	var echo struct {
		Method, Path, Query, Body, Prefix string
		Caller                            plugin.Caller
	}
	r.NoError(json.Unmarshal(res.Body, &echo))
	r.Equal(http.MethodPost, echo.Method)
	r.Equal("/standings/gt3", echo.Path)
	r.Equal("season=2026", echo.Query)
	r.Equal("a body", echo.Body)
	r.Equal("/plugin/results", echo.Prefix)
	r.Equal("ana-ruiz", echo.Caller.DriverSlug, "the plugin was not told who was asking")

	// Serving is metered like every other call, because a plugin that spends
	// the operator's tokens rendering a page must be counted like one that
	// spends them on a cue.
	eventually(t, "the request to be metered", func() bool {
		use, err := h.store.PluginTokensSinceFor(ctx, "results", time.Now().Add(-time.Hour))
		return err == nil && use.Total() > 0
	})
}

// A plugin signing a driver in, through the host. The host carries the answer
// back whole; turning it into a cookie is the server's.
func TestAPluginAsksTheHostToSignADriverIn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)
	ctx := t.Context()

	m := manifestFor("driver-login")
	m.Capabilities.HTTP = &plugin.HTTPCapability{Routes: []plugin.Route{
		{Path: "/", Access: plugin.AccessCustom, Reason: "It is what decides who anybody is."},
	}}
	install(t, h.dir, "driver-login", m)
	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to be running", func() bool {
		return stateOf(h.host, "driver-login") == plugins.StateRunning
	})

	res, err := h.host.Serve(ctx, "driver-login", plugin.HTTPRequest{
		Method: http.MethodGet, Path: "/sign-in", Query: "ana-ruiz",
	})
	r.NoError(err)
	r.Equal("ana-ruiz", res.SignIn, "the host did not carry back who the plugin signed in")
	r.Equal(http.StatusSeeOther, res.Status)

	out, err := h.host.Serve(ctx, "driver-login", plugin.HTTPRequest{
		Method: http.MethodPost, Path: "/sign-out",
	})
	r.NoError(err)
	r.True(out.SignOut)
}

// Plugins that have no route, are not running, or are not there at all.
func TestServingWhatHasNoRoute(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)
	ctx := t.Context()

	// A plugin that never asked for one.
	install(t, h.dir, "coach", manifestFor("coach"))
	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to be running", func() bool {
		return stateOf(h.host, "coach") == plugins.StateRunning
	})

	running := h.host.Running()
	r.Len(running, 1, "one plugin is running, and a client is told about that one")
	r.Equal("coach", running[0].Name)

	_, ok := h.host.Access("coach", "/")
	r.False(ok, "a plugin that asked for no route was given one")
	_, err := h.host.Serve(ctx, "coach", plugin.HTTPRequest{Method: http.MethodGet, Path: "/"})
	r.ErrorIs(err, plugins.ErrNoRoute)

	// One that is not installed at all.
	_, ok = h.host.Access("nothing-of-that-name", "/")
	r.False(ok)
	_, err = h.host.Serve(ctx, "nothing-of-that-name", plugin.HTTPRequest{Method: http.MethodGet, Path: "/"})
	r.Error(err)

	// And one the operator turned off. It reads as nothing being there, which
	// is what the operator asked for: a plugin they switched off should not
	// leave an address that answers differently from one that was never
	// installed.
	r.NoError(h.host.Disable(ctx, "coach"))
	_, err = h.host.Serve(ctx, "coach", plugin.HTTPRequest{Method: http.MethodGet, Path: "/"})
	r.ErrorIs(err, plugins.ErrNoPlugin)
	r.Empty(h.host.Running(), "a plugin the operator switched off is not one a client is told about")
}

// A plugin that crashes while serving is reported and restarted, and the
// request it was serving is a failure rather than a hang.
func TestAPluginThatCrashesWhileServing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	h := newHarness(t)
	ctx := t.Context()

	m := manifestFor("results")
	m.Capabilities.HTTP = &plugin.HTTPCapability{Routes: []plugin.Route{
		{Path: "/", Access: plugin.AccessPublic},
	}}
	install(t, h.dir, "results", m)
	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to be running", func() bool {
		return stateOf(h.host, "results") == plugins.StateRunning
	})

	_, err := h.host.Serve(ctx, "results", plugin.HTTPRequest{Method: http.MethodGet, Path: "/crash"})
	r.Error(err, "a plugin that killed itself mid-request answered anyway")

	// And it comes back, because a crash is what the supervisor is for. The
	// wait is on a request being answered rather than on the state, because
	// the state turns over a moment before the new process is on the other end
	// of a connection — and what a caller cares about is the answer.
	eventually(t, "the plugin to answer again", func() bool {
		res, err := h.host.Serve(ctx, "results", plugin.HTTPRequest{Method: http.MethodGet, Path: "/"})
		return err == nil && res.Status == http.StatusOK
	})
}
