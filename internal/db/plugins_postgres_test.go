//go:build postgres

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

// TestPluginsRoundTrip covers what the plugin host stores: the plugin itself,
// what the operator configured, and what it spent. The interesting parts are
// the ones a migration can get wrong — a credential that is sealed and a plain
// value that is not, and spending that outlives the plugin that did it.
func TestPluginsRoundTrip(t *testing.T) {
	t.Parallel()

	store, _ := migrated(t)
	ctx := context.Background()

	// Each subtest gets a plugin of its own. They share a database, so a
	// subtest that updates a row would otherwise be updating one another
	// subtest is reading.
	record := func(name string) db.PluginRecord {
		return db.PluginRecord{
			Name:             name,
			Version:          "1.0.0",
			Author:           "Pacenote",
			Description:      "Exercises every part of the plugin contract.",
			InterfaceVersion: 1,
			Capabilities:     []byte(`{"events":["lap.completed"],"network":true}`),
			Directory:        name,
			State:            "discovered",
		}
	}

	t.Run("a plugin is stored and read back", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		r.NoError(store.SavePlugin(ctx, record("readback")))

		got, err := store.Plugin(ctx, "readback")
		r.NoError(err)
		r.Equal("1.0.0", got.Version)
		r.Equal(1, got.InterfaceVersion)
		r.JSONEq(`{"events":["lap.completed"],"network":true}`, string(got.Capabilities))
		r.Equal("[]", string(got.DeclaredSettings), "a plugin that has never started has declared nothing")
		r.True(got.Enabled, "a plugin an operator installed is on until they say otherwise")
		r.False(got.FirstSeenAt.IsZero())
	})

	t.Run("a plugin nobody installed is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		_, err := store.Plugin(ctx, "nosuchplugin")
		r.ErrorIs(err, db.ErrNotFound)
	})

	t.Run("saving it again updates rather than duplicates", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		r.NoError(store.SavePlugin(ctx, record("upgraded")))

		upgraded := record("upgraded")
		upgraded.Version = "1.1.0"
		upgraded.State = "running"
		r.NoError(store.SavePlugin(ctx, upgraded))

		got, err := store.Plugin(ctx, "upgraded")
		r.NoError(err)
		r.Equal("1.1.0", got.Version)
		r.Equal("running", got.State)

		all, err := store.Plugins(ctx)
		r.NoError(err)
		seen := 0
		for i := range all {
			if all[i].Name == "upgraded" {
				seen++
			}
		}
		r.Equal(1, seen, "the upsert updated the row rather than adding one")
	})
}

// TestPluginSettingsKeepSecretsSealed is the storage half of the promise that a
// plugin never holds a credential: the value in the database is ciphertext, and
// a plain setting beside it is not.
func TestPluginSettingsKeepSecretsSealed(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	store, _ := migrated(t)
	ctx := context.Background()
	r.NoError(store.SavePlugin(ctx, db.PluginRecord{
		Name: "testplugin", Version: "1", Author: "a", Description: "d",
		InterfaceVersion: 1, Directory: "testplugin", State: "discovered",
	}))

	r.NoError(store.SavePluginSetting(ctx, "testplugin", "greeting", "Right"))
	r.NoError(store.SavePluginSecret(ctx, "testplugin", "api_key", []byte{0x01, 0x02, 0x03}))

	rows, err := store.PluginSettings(ctx, "testplugin")
	r.NoError(err)
	r.Len(rows, 2)

	byName := make(map[string]db.PluginSettingRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	r.Equal("Right", byName["greeting"].Value)
	r.Empty(byName["greeting"].Sealed)
	r.Empty(byName["api_key"].Value, "a credential is never readable beside its ciphertext")
	r.Equal([]byte{0x01, 0x02, 0x03}, byName["api_key"].Sealed)

	t.Run("saving a plain value over a credential clears the ciphertext", func(t *testing.T) {
		r := require.New(t)

		r.NoError(store.SavePluginSetting(ctx, "testplugin", "api_key", "notasecretanymore"))
		rows, err := store.PluginSettings(ctx, "testplugin")
		r.NoError(err)
		for _, row := range rows {
			if row.Name == "api_key" {
				r.Empty(row.Sealed)
				r.Equal("notasecretanymore", row.Value)
			}
		}
	})

	t.Run("removing the plugin removes its credentials", func(t *testing.T) {
		r := require.New(t)

		r.NoError(store.DeletePlugin(ctx, "testplugin"))
		rows, err := store.PluginSettings(ctx, "testplugin")
		r.NoError(err)
		r.Empty(rows)
	})
}

// TestPluginUsageOutlivesThePlugin. The cap is measured on these rows, and the
// money was the organisation's: removing a plugin must not erase what it spent.
func TestPluginUsageOutlivesThePlugin(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	store, _ := migrated(t)
	ctx := context.Background()
	r.NoError(store.SavePlugin(ctx, db.PluginRecord{
		Name: "testplugin", Version: "1", Author: "a", Description: "d",
		InterfaceVersion: 1, Directory: "testplugin", State: "running",
	}))

	r.NoError(store.RecordPluginUsage(ctx, db.PluginUsageWrite{
		Plugin: "testplugin", Job: "cue.training", Model: "fast", Input: 300, Output: 40,
	}))
	r.NoError(store.RecordPluginUsage(ctx, db.PluginUsageWrite{
		Plugin: "testplugin", Job: "debrief", Model: "strong", Input: 900, Output: 300, Cached: true,
	}))

	since := time.Now().Add(-time.Hour)

	all, err := store.PluginTokensSince(ctx, since)
	r.NoError(err)
	r.Equal(int64(1540), all.Total())
	r.Equal(int64(2), all.Calls)

	one, err := store.PluginTokensSinceFor(ctx, "testplugin", since)
	r.NoError(err)
	r.Equal(int64(1540), one.Total())

	r.NoError(store.DeletePlugin(ctx, "testplugin"))

	after, err := store.PluginTokensSince(ctx, since)
	r.NoError(err)
	r.Equal(int64(1540), after.Total(), "what it spent is the record of the operator's money, not the plugin's")
}

// TestPluginStateSurvivesARestart is why the state is in the database and not
// only in the host's memory: the plugin an operator goes looking for is the one
// that failed, and an in-memory list has nothing to say about it after a
// restart.
func TestPluginStateSurvivesARestart(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	store, _ := migrated(t)
	ctx := context.Background()
	r.NoError(store.SavePlugin(ctx, db.PluginRecord{
		Name: "testplugin", Version: "1", Author: "a", Description: "d",
		InterfaceVersion: 1, Directory: "testplugin", State: "discovered",
	}))

	r.NoError(store.SavePluginState(ctx, "testplugin", "failed",
		"It has now failed 3 times.", "panic: nil map\n", 3))
	r.NoError(store.SavePluginDeclaredSettings(ctx, "testplugin", []byte(`[{"name":"api_key","label":"Key","kind":"secret"}]`)))

	got, err := store.Plugin(ctx, "testplugin")
	r.NoError(err)
	r.Equal("failed", got.State)
	r.Equal(3, got.Restarts)
	r.Contains(got.LastOutput, "panic")
	r.Contains(string(got.DeclaredSettings), "api_key")

	t.Run("the panel can still render the form for a plugin that will not start", func(t *testing.T) {
		r := require.New(t)

		got, err := store.Plugin(ctx, "testplugin")
		r.NoError(err)
		r.NotEqual("[]", string(got.DeclaredSettings))
	})

	t.Run("a state nothing defines is refused by the database", func(t *testing.T) {
		r := require.New(t)

		err := store.SavePluginState(ctx, "testplugin", "confused", "", "", 0)
		r.Error(err, "the check constraint is what stops a typo becoming a state")
	})
}
