package plugins_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/plugins"
)

// TestHostRunsAPlugin is the happy path, end to end and through a real process:
// discovered, started, told what happened, asked for something, and metered.
func TestHostRunsAPlugin(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	h.store.setValue("greeting", "Right")
	h.store.setValue("budget", "400")
	h.store.setSecret(t, h.key, "api_key", "sk-ant-thekeynobodymayeversee")

	require.NoError(t, h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	t.Run("it was discovered and recorded", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		got := statusOf(t, h.host, "testplugin")
		r.Equal("1.0.0", got.Version)
		r.Equal("Pacenote", got.Author)
		r.Equal(plugin.InterfaceVersion, got.InterfaceVersion)
		r.True(got.Capabilities.Wants(plugin.EventLapCompleted))

		stored := h.store.record("testplugin")
		r.Equal(string(plugins.StateRunning), stored.State)
		r.Equal(dir, dirOf(h.dir, stored.Directory))
	})

	t.Run("it declared what the operator must configure", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		got := statusOf(t, h.host, "testplugin")
		r.NotEmpty(got.Settings, "the panel needs the declaration to render a form")

		kinds := make(map[string]plugin.Kind, len(got.Settings))
		for _, s := range got.Settings {
			kinds[s.Name] = s.Kind
		}
		r.Equal(plugin.KindSecret, kinds["api_key"])
		r.Equal(plugin.KindChoice, kinds["loudness"])

		// And it was stored, so the form can be rendered for a plugin that is
		// not running — which is the plugin an operator most needs to fix.
		r.Contains(string(h.store.record("testplugin").DeclaredSettings), `"api_key"`)
	})

	t.Run("an event reaches it and what it cost is recorded", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h.host.Notify(t.Context(), lapEvent("ana"))
		eventually(t, "the event to be metered", func() bool {
			for _, u := range h.store.calls() {
				if u.Job == string(plugin.EventLapCompleted) {
					return true
				}
			}
			return false
		})

		for _, u := range h.store.calls() {
			if u.Job != string(plugin.EventLapCompleted) {
				continue
			}
			r.Equal("testplugin", u.Plugin)
			r.Equal("testplugin-fast", u.Model, "a plugin with a credential reports the model it used it on")
			r.Equal(int64(400), u.Input+u.Output)
			r.NotNil(u.DriverID)
			r.Equal(int64(7), *u.DriverID)
		}
	})

	t.Run("a request is answered, out of the facts it was given", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
		r.NoError(err)
		r.Equal(plugin.RequestCueTraining, res.Kind)
		r.Contains(res.Text, "Right", "the operator's setting reached the plugin")
		r.Contains(res.Text, "Turn 4", "the facts reached the plugin")
		r.Equal(int64(400), res.Usage.Total())
		r.Equal("testplugin/1", res.PromptVersion)
	})

	t.Run("the plugins answering a kind can be found without knowing the name", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		r.Equal([]string{"testplugin"}, h.host.Answering(plugin.RequestSetup))
		r.Empty(h.host.Answering("cue.nonsense"))
	})
}

// dirOf rebuilds the absolute directory from what the record stores, which is
// the name relative to the plugin directory.
func dirOf(root, name string) string { return root + "/" + name }

// TestHostRefusesToStart covers everything a plugin can get wrong before it
// runs a single line. Each of these is recorded as failed with a reason an
// operator can act on, and none of them stops the server.
func TestHostRefusesToStart(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		install  func(t *testing.T, h *harness)
		plugin   string
		contains []string
	}{
		{
			name:   "a manifest that is not readable",
			plugin: "broken",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				installRaw(t, h.dir, "broken", `{"name": "broken",`)
			},
			contains: []string{"is not readable"},
		},
		{
			name:   "a manifest with a misspelled field",
			plugin: "typo",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				installRaw(t, h.dir, "typo", `{"name":"typo","version":"1","author":"a","description":"d",
					"interface_version":1,"capabilitys":{"events":["lap.completed"]}}`)
			},
			contains: []string{"is not readable"},
		},
		{
			name:   "a manifest naming an event this server does not carry",
			plugin: "curious",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				installRaw(t, h.dir, "curious", `{"name":"curious","version":"1","author":"a","description":"d",
					"interface_version":1,"capabilities":{"events":["driver.paired"]}}`)
			},
			contains: []string{"is not an event this server carries"},
		},
		{
			name:   "a plugin whose name does not match its directory",
			plugin: "elsewhere",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				m := manifestFor("somethingelse")
				install(t, h.dir, "elsewhere", m)
			},
			contains: []string{"calls itself", "must match"},
		},
		{
			name:   "a plugin built against a newer interface version",
			plugin: "fromthefuture",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				m := manifestFor("fromthefuture")
				m.InterfaceVersion = plugin.InterfaceVersion + 1
				install(t, h.dir, "fromthefuture", m)
			},
			contains: []string{
				fmt.Sprintf("version %d", plugin.InterfaceVersion+1),
				fmt.Sprintf("version %d", plugin.InterfaceVersion),
				"newer than the server",
				"upgrade the server",
			},
		},
		{
			name:   "a plugin built against an older interface version",
			plugin: "fromthepast",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				m := manifestFor("fromthepast")
				m.InterfaceVersion = plugin.InterfaceVersion - 1
				if m.InterfaceVersion < 1 {
					m.InterfaceVersion = 0
				}
				install(t, h.dir, "fromthepast", m)
			},
			contains: []string{"interface version"},
		},
		{
			name:   "a manifest with no binary beside it",
			plugin: "empty",
			install: func(t *testing.T, h *harness) {
				t.Helper()
				raw := `{"name":"empty","version":"1","author":"a","description":"d","interface_version":1,
					"binary":"testplugin","capabilities":{"events":["lap.completed"]}}`
				installRaw(t, h.dir, "empty", raw)
			},
			contains: []string{"there is no"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			h := newHarness(t)
			// A plugin that works, installed beside the broken one, so that
			// every case also proves the host survived it.
			install(t, h.dir, "testplugin", manifestFor("testplugin"))
			tc.install(t, h)

			r.NoError(h.host.Discover(t.Context()), "a broken plugin must not fail discovery")

			eventually(t, "the good plugin to start", func() bool {
				return stateOf(h.host, "testplugin") == plugins.StateRunning
			})
			eventually(t, "the broken plugin to be recorded", func() bool {
				return h.store.record(tc.plugin).State != ""
			})

			stored := h.store.record(tc.plugin)
			r.Equal(string(plugins.StateFailed), stored.State)
			for _, want := range tc.contains {
				r.Contains(stored.LastError, want)
			}

			res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
			r.NoError(err, "the host went on working")
			r.NotEmpty(res.Text)
		})
	}
}

// TestPluginCrashesOnStart is the first thing a host has to survive, and the
// first thing it has to stop doing: restart a plugin that will never work, for
// ever, with nothing in the panel to say why.
func TestPluginCrashesOnStart(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	dir := install(t, h.dir, "suicidal", manifestFor("suicidal"))
	misbehave(t, dir, behaviour{CrashAt: "start", ExitCode: 3, Stderr: "testplugin: about to fall over"})

	r.NoError(h.host.Discover(t.Context()))

	eventually(t, "the crashing plugin to be given up on", func() bool {
		return stateOf(h.host, "suicidal") == plugins.StateFailed
	})

	got := statusOf(t, h.host, "suicidal")
	r.Equal(plugins.StateFailed, got.State)
	r.Contains(got.LastError, "will not be started again")
	r.Contains(got.LastOutput, "about to fall over", "the operator is shown what it printed")
	r.Greater(got.Restarts, 1, "it was retried before being given up on")

	stored := h.store.record("suicidal")
	r.Equal(string(plugins.StateFailed), stored.State)
	r.Contains(stored.LastOutput, "about to fall over")

	// And the server is fine.
	eventually(t, "the good plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})
	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)

	_, err = h.host.Ask(t.Context(), "suicidal", cueRequest())
	r.ErrorIs(err, plugins.ErrUnavailable)
}

// TestPluginCrashesMidCall is the case the whole separate-process design exists
// for: a plugin dies with the server's request in its hands, the caller is told,
// and the plugin comes back.
func TestPluginCrashesMidCall(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	misbehave(t, dir, behaviour{CrashAt: "answer", ExitCode: 2})
	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.Error(err, "the caller is told there was no answer rather than waiting")

	// The host notices and brings it back, and the caller's next attempt works.
	behaveNormally(t, dir)
	eventually(t, "the plugin to be restarted", func() bool {
		return statusOf(t, h.host, "testplugin").Restarts > 0 &&
			stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)
}

// TestPluginMissesItsDeadline. A cue that arrives after the corner is worse
// than no cue, so the caller stops waiting, is told plainly, and the plugin is
// left running because being slow once is not being broken.
//
// The steps are sequential rather than subtests: each one changes what the
// plugin does next, and a parallel subtest would be changing it underneath
// another one.
func TestPluginMissesItsDeadline(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	misbehave(t, dir, behaviour{Hang: "30s"})

	// The caller stops waiting at the deadline it set.
	req := cueRequest()
	req.Deadline = time.Now().Add(200 * time.Millisecond)

	start := time.Now()
	_, err := h.host.Ask(t.Context(), "testplugin", req)
	r.ErrorIs(err, context.DeadlineExceeded)
	r.ErrorContains(err, "did not answer in time")
	r.Less(time.Since(start), 5*time.Second)

	// It was reported, because a plugin that misses deadlines is something an
	// operator has to be able to find out about.
	r.Contains(h.logs.String(), "missed its deadline")

	// And the plugin is still there: being slow once is not being broken.
	behaveNormally(t, dir)
	r.Equal(plugins.StateRunning, stateOf(h.host, "testplugin"))

	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)
}

// TestOverTheCapThePluginIsNotCalled. The cap belongs to the host and not to the
// plugin, so a plugin past it is not called at all — not called and truncated,
// not called and refused by the vendor — and the caller is told so that it falls
// back to the client's own cue.
//
// Sequential, because each step moves the same counter the next one reads.
func TestOverTheCapThePluginIsNotCalled(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	const capTokens = 1000

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	// The cap is set the way the panel sets it: against the plugin, by the
	// host. The plugin is never told it and cannot read it.
	r.NoError(h.host.SetDailyCap(t.Context(), "testplugin", capTokens))

	// Under the cap it is called.
	h.store.setSpent(capTokens - 1)
	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)

	// At the cap it is not, and nothing is spent finding that out.
	h.store.setSpent(capTokens)
	before := len(h.store.calls())

	_, err = h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.ErrorIs(err, plugins.ErrOverCap)
	r.ErrorContains(err, "1000")
	r.Len(h.store.calls(), before, "nothing was spent, because nothing was called")
}

// TestACapThatCannotBeReadRefuses. "The setting would not load" must not be a
// way past a spending limit, so the safe direction is to refuse: the driver
// hears the deterministic cue and the operator's bill does not depend on
// whether a query succeeded.
func TestACapThatCannotBeReadRefuses(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	// The settings this plugin's cap lives in cannot be read.
	h.store.failSettings = errors.New("the database is not answering")

	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.ErrorIs(err, plugins.ErrOverCap)
}

// A cap somebody edited by hand into something that is not a number is not a cap
// that is off. Refusing is the safe direction, for the same reason.
func TestACapThatIsNotANumberRefuses(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.NoError(h.store.SavePluginSetting(t.Context(), "testplugin", plugins.SettingDailyCap, "lots"))

	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.ErrorIs(err, plugins.ErrOverCap)
	r.ErrorContains(err, "not a number")
}

// The cap is one plugin's, not the server's. A plugin that has spent its
// allowance must not silence one that has not.
func TestOnePluginsCapDoesNotSilenceAnother(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	install(t, h.dir, "otherplugin", manifestFor("otherplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "both plugins to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning &&
			stateOf(h.host, "otherplugin") == plugins.StateRunning
	})

	r.NoError(h.host.SetDailyCap(t.Context(), "testplugin", 1))
	h.store.setSpent(1_000)

	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.ErrorIs(err, plugins.ErrOverCap)

	// The other has no cap and is called.
	res, err := h.host.Ask(t.Context(), "otherplugin", cueRequest())
	r.NoError(err, "one plugin at its cap silenced another that had none")
	r.NotEmpty(res.Text)
}

// TestASecretNeverReachesALogLine is the promise the sealed-credentials
// capability rests on, tested against a plugin that prints the key it was lent
// straight to standard error. The contract's own type cannot stop that — it is
// another process — so the host scrubs everything coming back, and this is
// where that is proved.
func TestASecretNeverReachesALogLine(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	const secret = "sk-ant-thisisthekeynobodymayeversee"

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	h.store.setSecret(t, h.key, "api_key", secret)
	h.store.setValue("budget", "40")
	h.store.setValue("loudness", "loud")
	misbehave(t, dir, behaviour{LeakSecret: true})

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.Contains(res.Text, "api_key", "the plugin was handed the credential")
	r.NotContains(res.Text, secret, "and did not send it back")

	eventually(t, "the plugin's leak to be captured", func() bool {
		return strings.Contains(statusOf(t, h.host, "testplugin").LastOutput, "leaking api_key")
	})

	t.Run("what the plugin printed is redacted", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		out := statusOf(t, h.host, "testplugin").LastOutput
		r.Contains(out, "leaking api_key", "the line is kept, so an operator can see what happened")
		r.NotContains(out, secret, "but not the credential in it")
		r.Contains(out, plugin.Redacted)
	})

	t.Run("nothing in the host's own log carries it", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		r.NotContains(h.logs.String(), secret)
	})

	t.Run("nothing written to the database carries it", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		stored := h.store.record("testplugin")
		r.NotContains(stored.LastOutput, secret)
		r.NotContains(stored.LastError, secret)
	})
}

// TestHostRefusesWhatItShouldNotAsk covers the answers a caller gets that are
// not "here is your cue", each of which it has to handle differently.
func TestHostRefusesWhatItShouldNotAsk(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	require.NoError(t, h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	t.Run("a plugin nobody installed", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		_, err := h.host.Ask(t.Context(), "nosuchplugin", cueRequest())
		r.ErrorIs(err, plugins.ErrNoPlugin)
	})

	t.Run("a request the plugin never said it answers", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		limited := newHarness(t)
		m := manifestFor("testplugin")
		m.Capabilities.Requests = []plugin.RequestKind{plugin.RequestSetup}
		install(t, limited.dir, "testplugin", m)
		r.NoError(limited.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool {
			return stateOf(limited.host, "testplugin") == plugins.StateRunning
		})

		_, err := limited.host.Ask(t.Context(), "testplugin", cueRequest())
		r.ErrorIs(err, plugin.ErrUnsupported)
	})

	t.Run("a plugin with nothing worth saying", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		quiet := newHarness(t)
		install(t, quiet.dir, "testplugin", manifestFor("testplugin"))
		quiet.store.setValue("enabled", "false")
		r.NoError(quiet.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool {
			return stateOf(quiet.host, "testplugin") == plugins.StateRunning
		})

		_, err := quiet.host.Ask(t.Context(), "testplugin", cueRequest())
		r.ErrorIs(err, plugin.ErrNoAnswer, "silence is an answer a caller falls back from, not a failure")
	})

	t.Run("an event nothing can act on is not dispatched", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h.host.Notify(t.Context(), plugin.Event{ID: "e", Kind: plugin.EventLapCompleted})
		r.Contains(h.logs.String(), "was not dispatched")
	})
}

// TestDiscoverIsSafeToRepeat, because rescan is a button an operator presses.
func TestDiscoverIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	before := statusOf(t, h.host, "testplugin")
	r.NoError(h.host.Discover(t.Context()))
	r.NoError(h.host.Discover(t.Context()))

	after := statusOf(t, h.host, "testplugin")
	r.Equal(plugins.StateRunning, after.State)
	r.Equal(before.Restarts, after.Restarts, "a working plugin is not restarted because somebody pressed rescan")

	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)
}

// TestClosedHostAnswersNothing, because a shutdown must not leave a caller
// hanging on a process that is already gone.
func TestClosedHostAnswersNothing(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	h.host.Close()
	h.host.Close() // twice, because a shutdown path gets called twice.

	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.ErrorIs(err, plugins.ErrClosed)

	// And an event dispatched after the door closed goes nowhere rather than
	// starting a goroutine nothing will wait for.
	h.host.Notify(t.Context(), lapEvent("ana"))
}

// TestSettingsRefusedAtTheBoundary covers the operator's half of the settings
// capability: a value the plugin cannot use never reaches it.
func TestSettingsRefusedAtTheBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		set     func(*fakeStore)
		wantErr error
		text    string
	}{
		{
			name:    "a number that is not one",
			set:     func(f *fakeStore) { f.setValue("budget", "plenty") },
			wantErr: plugin.ErrInvalid,
			text:    "is not a whole number",
		},
		{
			name:    "a choice nobody offered",
			set:     func(f *fakeStore) { f.setValue("loudness", "deafening") },
			wantErr: plugin.ErrInvalid,
			text:    "is not one of the choices",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			h := newHarness(t)
			install(t, h.dir, "testplugin", manifestFor("testplugin"))
			tc.set(h.store)

			r.NoError(h.host.Discover(t.Context()))
			eventually(t, "the plugin to start", func() bool {
				return stateOf(h.host, "testplugin") == plugins.StateRunning
			})

			_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
			r.ErrorIs(err, tc.wantErr)
			r.ErrorContains(err, tc.text)
		})
	}
}

// TestACredentialThatWillNotOpen is the data directory having been lost while
// the database survived. The feature is off and the operator is told to enter
// the key again, rather than a call failing at the vendor an hour later.
func TestACredentialThatWillNotOpen(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	m := manifestFor("testplugin")
	install(t, h.dir, "testplugin", m)

	// Sealed with a key this host does not have.
	other := newHarness(t)
	h.store.setSecret(t, other.key, "api_key", "sk-ant-sealed-with-another-key")

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.ErrorIs(err, plugin.ErrNotConfigured)
	r.ErrorContains(err, "enter it again")
}

// TestAPluginWhoseFilesHaveGone. An operator who deletes a directory should
// find the plugin listed as gone rather than find nothing and wonder whether
// they imagined it.
func TestAPluginWhoseFilesHaveGone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	install(t, h.dir, "temporary", manifestFor("temporary"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "both plugins to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning &&
			stateOf(h.host, "temporary") == plugins.StateRunning
	})

	r.NoError(removeAll(h.dir, "temporary"))
	r.NoError(h.host.Discover(t.Context()))

	r.Empty(stateOf(h.host, "temporary"), "it is no longer supervised")
	stored := h.store.record("temporary")
	r.Equal(string(plugins.StateStopped), stored.State)
	r.Contains(stored.LastError, "no longer in the plugin directory")

	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)
}
