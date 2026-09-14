//go:build postgres

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

//nolint:unparam // there is one plugin in these tests; naming it at the call is the point.
func seedPlugin(t *testing.T, store *db.Store, name string) {
	t.Helper()
	require.NoError(t, store.SavePlugin(context.Background(), db.PluginRecord{
		Name: name, Version: "1.0.0", Author: "Pacenote",
		Description: "Does something.", InterfaceVersion: 1,
		Directory: name, State: "running",
	}))
}

func TestPluginRowsRoundTrip(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	seedPlugin(t, store, "engineer")

	got, err := store.Plugin(ctx, "engineer")
	r.NoError(err)
	r.Equal("1.0.0", got.Version)
	r.True(got.Enabled, "a plugin found on disk runs unless the operator says otherwise")

	// Turning it off sticks, and rediscovering it does not turn it back on —
	// the upsert deliberately does not name enabled in its DO UPDATE.
	r.NoError(store.SavePluginEnabled(ctx, "engineer", false))
	seedPlugin(t, store, "engineer")
	got, err = store.Plugin(ctx, "engineer")
	r.NoError(err)
	r.False(got.Enabled, "rediscovering a plugin turned a disabled one back on")

	r.NoError(store.SavePluginEnabled(ctx, "engineer", true))
	got, err = store.Plugin(ctx, "engineer")
	r.NoError(err)
	r.True(got.Enabled)

	all, err := store.Plugins(ctx)
	r.NoError(err)
	r.Len(all, 1)

	// A plugin nobody has seen is not found rather than an empty row, so a
	// caller can tell the difference.
	_, err = store.Plugin(ctx, "neverheardof")
	r.ErrorIs(err, db.ErrNotFound)
	_, err = store.PluginDatabase(ctx, "neverheardof")
	r.ErrorIs(err, db.ErrNotFound)

	r.NoError(store.DeletePlugin(ctx, "engineer"))
	_, err = store.Plugin(ctx, "engineer")
	r.ErrorIs(err, db.ErrNotFound)
}

func TestPluginDatabaseBookkeeping(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	seedPlugin(t, store, "engineer")

	// A plugin with no database reads back as not provisioned, which is most of
	// them and is not an error.
	row, err := store.PluginDatabase(ctx, "engineer")
	r.NoError(err)
	r.False(row.Provisioned)
	r.Empty(row.PasswordSealed)
	r.Empty(row.Migration)

	sealed := []byte("this stands in for a sealed password")
	r.NoError(store.SetPluginProvisioned(ctx, "engineer", sealed, "00002_more.sql"))

	row, err = store.PluginDatabase(ctx, "engineer")
	r.NoError(err)
	r.True(row.Provisioned)
	r.Equal(sealed, row.PasswordSealed)
	r.Equal("00002_more.sql", row.Migration)

	// Provisioning with no password is refused rather than written: the table's
	// own constraint says a provisioned plugin has a password, and this is the
	// check that gives the caller a sentence rather than a constraint name.
	r.Error(store.SetPluginProvisioned(ctx, "engineer", nil, ""))

	r.NoError(store.ClearPluginDatabase(ctx, "engineer"))
	row, err = store.PluginDatabase(ctx, "engineer")
	r.NoError(err)
	r.False(row.Provisioned)
	r.Empty(row.PasswordSealed, "the password outlived the role it opened")
}

func TestPluginSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	seedPlugin(t, store, "engineer")

	r.NoError(store.SavePluginDeclaredSettings(ctx, "engineer", []byte(`[{"name":"model"}]`)))
	r.NoError(store.SavePluginSetting(ctx, "engineer", "model", "claude-opus-5"))
	r.NoError(store.SavePluginSecret(ctx, "engineer", "api_key", []byte("sealed")))

	rows, err := store.PluginSettings(ctx, "engineer")
	r.NoError(err)
	got := map[string]db.PluginSettingRow{}
	for _, row := range rows {
		got[row.Name] = row
	}
	r.Equal("claude-opus-5", got["model"].Value)
	r.Equal([]byte("sealed"), got["api_key"].Sealed)
	r.Empty(got["api_key"].Value, "a credential was also stored in clear")

	// A setting saved twice is updated rather than duplicated.
	r.NoError(store.SavePluginSetting(ctx, "engineer", "model", "claude-sonnet-5"))
	rows, err = store.PluginSettings(ctx, "engineer")
	r.NoError(err)
	r.Len(rows, 2)

	r.NoError(store.DeletePluginSetting(ctx, "engineer", "api_key"))
	rows, err = store.PluginSettings(ctx, "engineer")
	r.NoError(err)
	r.Len(rows, 1)

	// Removing the plugin takes its settings with it, which is what the
	// foreign key is for.
	r.NoError(store.DeletePlugin(ctx, "engineer"))
	rows, err = store.PluginSettings(ctx, "engineer")
	r.NoError(err)
	r.Empty(rows)
}

func TestPluginUsageIsKeptAfterThePluginGoes(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	seedPlugin(t, store, "engineer")
	r.NoError(store.RecordPluginUsage(ctx, db.PluginUsageWrite{
		Plugin: "engineer", Job: "coach", Model: "claude-opus-5",
		Input: 1_000, Output: 200,
	}))

	since := time.Now().Add(-time.Hour)
	use, err := store.PluginTokensSince(ctx, since)
	r.NoError(err)
	r.EqualValues(1_200, use.Input+use.Output)

	mine, err := store.PluginTokensSinceFor(ctx, "engineer", since)
	r.NoError(err)
	r.EqualValues(1_200, mine.Input+mine.Output)

	// The money was the organisation's and the row is the record of it, so
	// removing the plugin must not erase what it spent.
	r.NoError(store.DeletePlugin(ctx, "engineer"))
	use, err = store.PluginTokensSince(ctx, since)
	r.NoError(err)
	r.EqualValues(1_200, use.Input+use.Output, "removing a plugin erased what it had spent")
}

// Every one of these reports a database that has gone away rather than
// panicking or returning a zero value that reads as success. The pool is closed
// under them, which is what a server whose database restarted sees.
func TestEveryPluginStoreCallReportsAClosedDatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	seedPlugin(t, store, "engineer")
	store.Close()

	r.Error(store.SavePlugin(ctx, db.PluginRecord{Name: "engineer", InterfaceVersion: 1}))
	r.Error(store.SavePluginDeclaredSettings(ctx, "engineer", []byte(`[]`)))
	r.Error(store.SavePluginState(ctx, "engineer", "failed", "", "", 0))
	r.Error(store.SavePluginEnabled(ctx, "engineer", false))
	r.Error(store.SavePluginSetting(ctx, "engineer", "model", "x"))
	r.Error(store.SavePluginSecret(ctx, "engineer", "api_key", []byte("x")))
	r.Error(store.DeletePluginSetting(ctx, "engineer", "model"))
	r.Error(store.DeletePlugin(ctx, "engineer"))
	r.Error(store.RecordPluginUsage(ctx, db.PluginUsageWrite{Plugin: "engineer"}))
	r.Error(store.SetPluginProvisioned(ctx, "engineer", []byte("x"), ""))
	r.Error(store.ClearPluginDatabase(ctx, "engineer"))
	r.Error(store.ProvisionPluginDatabase(ctx, "engineer", "not-a-real-credential"))
	r.Error(store.DropPluginDatabase(ctx, "engineer"))

	_, err := store.Plugins(ctx)
	r.Error(err)
	_, err = store.Plugin(ctx, "engineer")
	r.Error(err)
	r.NotErrorIs(err, db.ErrNotFound, "a database that will not answer is not the same as no such plugin")
	_, err = store.PluginSettings(ctx, "engineer")
	r.Error(err)
	_, err = store.PluginDatabase(ctx, "engineer")
	r.Error(err)
	r.NotErrorIs(err, db.ErrNotFound)
	_, err = store.PluginTokensSince(ctx, time.Now().Add(-time.Hour))
	r.Error(err)
	_, err = store.PluginTokensSinceFor(ctx, "engineer", time.Now().Add(-time.Hour))
	r.Error(err)
}
