package plugins_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/plugins"
)

// A plugin is recorded in the database at several points, and every one of those
// writes can fail. None of them may take the server down, and each has to leave
// a line saying which write it was: an operator whose database went away while
// plugins were starting should be able to tell that from a plugin that is
// broken.

func TestAPluginThatCannotBeRecordedDoesNotStopTheHost(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	h.store.failSavePlugin = errors.New("the plugins table is not there")
	install(t, h.dir, "testplugin", manifestFor("testplugin"))

	// Discover reports the failure but does not panic, and the host stays
	// usable.
	err := h.host.Discover(t.Context())
	r.NoError(err, "a row that will not write must not fail the whole scan")
	r.Empty(h.store.record("testplugin").Name)
}

func TestAManifestThatWillNotParseAndWillNotRecordIsLogged(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	h.store.failSavePlugin = errors.New("the plugins table is not there")
	installRaw(t, h.dir, "broken", `{"name": "broken",`)

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the host to say it could not record the plugin", func() bool {
		return strings.Contains(h.logs.String(), "could not be recorded")
	})
}

func TestADeclarationThatWillNotStoreIsLoggedAndThePluginRuns(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	h.store.failSaveDeclared = errors.New("the plugin_settings table is not there")
	install(t, h.dir, "testplugin", manifestFor("testplugin"))

	r.NoError(h.host.Discover(t.Context()))
	// The plugin still runs: what it declared is for the panel's form, and a
	// form that cannot be rendered is not a reason to refuse a plugin that
	// works.
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})
	eventually(t, "the host to say the declaration could not be stored", func() bool {
		return strings.Contains(h.logs.String(), "plugin_settings table is not there")
	})
}

// The cap is enforced against what has been spent, and reading that can fail.
// It refuses the call rather than letting it through: a database that will not
// answer must not be a way past a spending limit.
func TestSpendingThatCannotBeReadRefusesTheCall(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})
	r.NoError(h.host.SetDailyCap(t.Context(), "testplugin", 1_000))

	h.store.failTokens = errors.New("the usage table is not there")
	_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.Error(err, "a call went through while what had been spent could not be read")
	r.Contains(err.Error(), "usage table is not there")
}

// A plugin that ran for a while before dying has its restart count reset, so a
// transient cause does not accumulate towards the count that marks it failed for
// good. Without that reset a plugin which crashes once a month would be given up
// on in its fifth month, with nothing in between to connect the two.
//
// The assertion is that it is never given up on, rather than that the count is
// zero: the reset happens before the failed attempt is counted, so the count
// after a crash is one. What the reset buys is that it never climbs.
func TestAPluginThatRanForAWhileIsNeverGivenUpOn(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A clock that jumps a minute every time it is read, so that any run at all
	// looks longer than the start timeout. The alternative is a test that waits
	// out a real twenty seconds, several times over.
	var mu sync.Mutex
	now := time.Now()
	h := newHarness(t, func(o *plugins.Options) {
		o.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(time.Minute)
			return now
		}
	})

	dir := install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	// Four crashes, against a harness that gives up after two. Ask is used
	// rather than Notify because it waits: the crash has happened by the time it
	// returns, so restoring the behaviour afterwards cannot race it.
	for range 4 {
		misbehave(t, dir, behaviour{CrashAt: "answer", ExitCode: 2})
		_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
		r.Error(err, "the caller is told there was no answer")

		behaveNormally(t, dir)
		// "Came back" means it answers again, not that the state says running:
		// the state is still running for the moment between the process dying
		// and the supervisor noticing, and asking in that window reaches a
		// connection that is already gone.
		eventually(t, "the plugin to answer again", func() bool {
			_, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
			return err == nil
		})
	}

	r.Equal(plugins.StateRunning, stateOf(h.host, "testplugin"),
		"a plugin that kept coming back was given up on")
	r.LessOrEqual(statusOf(t, h.host, "testplugin").Restarts, 1,
		"the restart count climbed across crashes that were each preceded by a real run")

	// And it still works.
	res, err := h.host.Ask(t.Context(), "testplugin", cueRequest())
	r.NoError(err)
	r.NotEmpty(res.Text)
}
