package plugins_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/plugins"
)

// The cap is stored beside a plugin's own settings and is not one of them. This
// is what keeps the two apart: a plugin cannot declare a name beginning with an
// underscore, so it cannot collide with the host's however hard it tries.
func TestTheHostsOwnSettingsAreReserved(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.True(plugins.Reserved(plugins.SettingDailyCap))
	r.Equal(byte('_'), plugins.SettingDailyCap[0],
		"a host-owned setting has to start with the character a plugin may not use")

	// Everything a plugin can legally declare is not reserved.
	for _, name := range []string{"api_key", "model", "a", "z9", "with-hyphen", "with_underscore"} {
		r.False(plugins.Reserved(name), "%q was treated as the host's", name)
	}
	r.False(plugins.Reserved(""))
}

func TestTheCapIsPerPluginAndStoredAgainstIt(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	install(t, h.dir, "otherplugin", manifestFor("otherplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "both to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning &&
			stateOf(h.host, "otherplugin") == plugins.StateRunning
	})

	// No cap until one is set, which is the operator saying so rather than a
	// default nobody chose.
	got, err := h.host.DailyCap(t.Context(), "testplugin")
	r.NoError(err)
	r.Zero(got)

	r.NoError(h.host.SetDailyCap(t.Context(), "testplugin", 250_000))
	got, err = h.host.DailyCap(t.Context(), "testplugin")
	r.NoError(err)
	r.EqualValues(250_000, got)

	// One plugin's cap is not another's.
	got, err = h.host.DailyCap(t.Context(), "otherplugin")
	r.NoError(err)
	r.Zero(got, "a cap set on one plugin reached another")

	// It is replaced rather than added to, so saving twice leaves one value.
	r.NoError(h.host.SetDailyCap(t.Context(), "testplugin", 1_000))
	got, err = h.host.DailyCap(t.Context(), "testplugin")
	r.NoError(err)
	r.EqualValues(1_000, got)
}

func TestSettingACapIsRefusedForWhatCannotHaveOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	r.Error(h.host.SetDailyCap(t.Context(), "testplugin", -1), "a negative cap was accepted")
	r.Error(h.host.SetDailyCap(t.Context(), "neverheardof", 1_000),
		"a cap was set on a plugin that does not exist")
}

// What the panel renders: the cap, and what has been spent in the two windows an
// operator asks about.
func TestSpendingReadsBothWindows(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	r.NoError(h.host.SetDailyCap(t.Context(), "testplugin", 1_000))
	h.store.setSpent(400)

	got, err := h.host.Spending(t.Context(), "testplugin")
	r.NoError(err)
	r.EqualValues(1_000, got.Cap)
	r.EqualValues(400, got.Today.Total())
	r.False(got.OverCap)

	// At the cap, which is the state most likely to be reported as a broken
	// coach.
	h.store.setSpent(1_000)
	got, err = h.host.Spending(t.Context(), "testplugin")
	r.NoError(err)
	r.True(got.OverCap)
}
