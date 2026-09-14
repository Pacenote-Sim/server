package plugins_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/plugins"
)

// restart is what restarting the server is: a new host over the same plugin
// directory, the same store and the same data key. It is not the same as a
// rescan — Discover leaves a plugin that is already supervised alone, so
// anything that happens at install time only happens again on a real restart.
//
// The options are how a test keeps a store this harness cannot hold. A test
// running against real PostgreSQL has to pass its own store back in, because
// harness.store is the in-memory one and handing that to the second host would
// point it at a database that does not exist.
func restart(t *testing.T, h *harness, opts ...func(*plugins.Options)) *harness {
	t.Helper()
	base := make([]func(*plugins.Options), 0, 1+len(opts))
	base = append(base, func(o *plugins.Options) {
		o.Dir = h.dir
		o.Store = h.store
		o.Keyring = auth.NewKeyring(h.key)
	})
	next := newHarness(t, append(base, opts...)...)
	next.key = h.key
	return next
}

// withDatabase is the manifest of a plugin that asked for tables.
func withDatabase(name string) plugin.Manifest {
	m := manifestFor(name)
	m.Capabilities.Database = true
	return m
}

func TestAPluginThatAsksForNoDatabaseGetsNone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	// Nothing was provisioned, which is the common case and the one worth being
	// sure about: most plugins want no database, and creating a role for each of
	// them would be a cluster full of roles nobody asked for.
	r.Empty(h.store.provisionedNames())

	// And the process agrees: it was handed no connection string.
	eventually(t, "the plugin to say it has no database", func() bool {
		return strings.Contains(statusOf(t, h.host, "testplugin").LastOutput, "database no")
	})
}

func TestAPluginThatAsksForADatabaseIsGivenOne(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", withDatabase("testplugin"))

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.Equal([]string{"testplugin"}, h.store.provisionedNames())

	// The connection string reached the process, which is the whole delivery
	// mechanism: an environment variable on a subprocess started with no other
	// environment at all.
	eventually(t, "the plugin to say it has a database", func() bool {
		return strings.Contains(statusOf(t, h.host, "testplugin").LastOutput, "database yes")
	})

	// And the password was sealed, not stored in clear.
	row, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	r.True(row.Provisioned)
	r.NotEmpty(row.PasswordSealed)
	opened, err := h.key.Open(row.PasswordSealed)
	r.NoError(err)
	r.NotEmpty(opened)
	r.NotContains(string(row.PasswordSealed), opened, "the sealed password contains its own plaintext")
}

// A plugin printing its own connection string is not far-fetched — it is what
// an author writes when a connection fails — and last_output is shown in the
// panel. The password must not survive the trip.
func TestAPluginsDatabasePasswordNeverReachesWhatIsStored(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))
	misbehave(t, dir, behaviour{LeakDatabase: true})

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	eventually(t, "the plugin to leak its connection string", func() bool {
		return strings.Contains(statusOf(t, h.host, "testplugin").LastOutput, "leaking database")
	})

	row, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	password, err := h.key.Open(row.PasswordSealed)
	r.NoError(err)
	r.NotEmpty(password)

	out := statusOf(t, h.host, "testplugin").LastOutput
	r.Contains(out, "leaking database", "the test did not actually exercise the leak")
	r.NotContains(out, password, "a plugin's database password reached what the panel shows")
	r.Contains(out, plugin.Redacted)

	// The same applies to the copy in the database, which is what a support
	// bundle would carry.
	r.NotContains(h.store.record("testplugin").LastOutput, password)
	r.NotContains(h.logs.String(), password, "the password reached the host's own log")
}

// A database that cannot be created is this plugin failing, not the server. A
// league whose results plugin has nowhere to write still wants laps uploaded.
func TestAPluginWhoseDatabaseCannotBeMadeFailsAlone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	h.store.failProvision = errors.New("permission denied to create role")

	install(t, h.dir, "needsdb", withDatabase("needsdb"))
	install(t, h.dir, "plainplugin", manifestFor("plainplugin"))

	// Discover reports nothing: a plugin that will not start is a line in the
	// log and a row marked failed, not an error that stops the scan.
	r.NoError(h.host.Discover(t.Context()))

	eventually(t, "the other plugin to start", func() bool {
		return stateOf(h.host, "plainplugin") == plugins.StateRunning
	})
	// A plugin that never started has no instance to ask, so its state is read
	// where the panel reads it: the row. This is the same path a broken manifest
	// takes.
	r.Equal(string(plugins.StateFailed), h.store.record("needsdb").State)
	r.Contains(h.store.record("needsdb").LastError, "permission denied to create role")
}

// Sealing needs a data key. A server with none cannot store the role's password,
// and storing it in clear to get past that would be worse than refusing.
func TestAPluginDatabaseIsRefusedWithoutADataKey(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A server whose configuration file carries no data key. Sealed values do
	// not open and new ones cannot be made.
	h := newHarness(t, func(o *plugins.Options) { o.Keyring = auth.NewKeyring(nil) })
	install(t, h.dir, "needsdb", withDatabase("needsdb"))

	r.NoError(h.host.Discover(t.Context()))
	r.Equal(string(plugins.StateFailed), h.store.record("needsdb").State)
	r.Contains(h.store.record("needsdb").LastError, "no data key")
	r.Empty(h.store.provisionedNames())
}

// Removing a plugin's files leaves its data alone. A directory deleted by
// accident, or moved during an upgrade, must not destroy a season of results —
// so retiring a plugin stops it and keeps everything it wrote.
func TestRetiringAPluginDoesNotDropItsDatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	r.Equal([]string{"testplugin"}, h.store.provisionedNames())

	r.NoError(os.RemoveAll(dir))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to stop", func() bool { return stateOf(h.host, "testplugin") != plugins.StateRunning })

	r.Empty(h.store.droppedNames(), "removing a plugin's files destroyed its data")
	row, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	r.True(row.Provisioned, "the plugin's database was forgotten when its files went")
}

// Remove is the only path that destroys a plugin's data, and it is the operator
// saying so. It is worth distinguishing sharply from a plugin whose files went
// missing, which keeps everything.
func TestRemovingAPluginDropsItsDatabaseAndItsRow(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", withDatabase("testplugin"))

	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })
	r.Equal([]string{"testplugin"}, h.store.provisionedNames())

	r.NoError(h.host.Remove(t.Context(), "testplugin"))

	r.Equal([]string{"testplugin"}, h.store.droppedNames(), "the database was not dropped")
	r.Empty(h.store.record("testplugin").Name, "the row outlived the plugin")
	_, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.Error(err, "the sealed password outlived the role it opened")

	// The process is gone with it, not left running against a schema that no
	// longer exists.
	r.Empty(stateOf(h.host, "testplugin"))
}

// Removing a plugin that never had a database, or was never there at all, must
// work: most plugins have no database, and an operator pressing remove twice is
// not an error worth showing them.
func TestRemovingAPluginWithoutADatabaseIsFine(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	r.NoError(h.host.Remove(t.Context(), "testplugin"))
	r.NoError(h.host.Remove(t.Context(), "testplugin"))
	r.NoError(h.host.Remove(t.Context(), "neverexisted"))
}

// The password is kept across restarts rather than rotated. A running plugin is
// holding the connection string it was given, and changing it under a plugin
// that is working would break it for nothing.
func TestAPluginsDatabasePasswordIsKeptAcrossRestarts(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", withDatabase("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	first, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	before, err := h.key.Open(first.PasswordSealed)
	r.NoError(err)
	r.NotEmpty(before)

	// A rescan leaves a running plugin alone, so the restart is the case worth
	// testing: a fresh host that finds the plugin already provisioned.
	r.NoError(h.host.Discover(t.Context()))
	again := restart(t, h)
	r.NoError(again.host.Discover(t.Context()))
	eventually(t, "the plugin to start again", func() bool {
		return stateOf(again.host, "testplugin") == plugins.StateRunning
	})

	second, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	after, err := h.key.Open(second.PasswordSealed)
	r.NoError(err)
	r.Equal(before, after, "the password was rotated by a rescan")
}

// The data key was regenerated, or the data directory was lost while the
// database survived. The stored password cannot be opened, so a new one is set
// on the role that already exists — and the plugin's tables are not touched,
// which is the whole reason the role is altered rather than dropped and remade.
func TestAPluginsDatabasePasswordIsReplacedWhenItCannotBeRead(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t)
	install(t, h.dir, "testplugin", withDatabase("testplugin"))
	r.NoError(h.host.Discover(t.Context()))
	eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

	original, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	before, err := h.key.Open(original.PasswordSealed)
	r.NoError(err)

	// Rubbish where the sealed password was: this is what a regenerated data key
	// leaves behind.
	r.NoError(h.store.SetPluginProvisioned(t.Context(), "testplugin",
		[]byte("this was sealed with a key that is gone"), ""))

	again := restart(t, h)
	r.NoError(again.host.Discover(t.Context()))
	eventually(t, "the plugin to start again", func() bool {
		return stateOf(again.host, "testplugin") == plugins.StateRunning
	})

	replaced, err := h.store.PluginDatabase(t.Context(), "testplugin")
	r.NoError(err)
	after, err := h.key.Open(replaced.PasswordSealed)
	r.NoError(err)
	r.NotEmpty(after)
	r.NotEqual(before, after, "the unreadable password was not replaced")

	// The role was re-provisioned rather than dropped, so nothing it owns was
	// destroyed to get a readable password back.
	r.Empty(h.store.droppedNames())
	r.Contains(again.logs.String(), "cannot be read")
}

// Each of these is a way the store fails while a plugin's database is being set
// up. All of them are this plugin failing rather than the server failing, and
// each says which step could not be done.
func TestAPluginsDatabaseFailsOnEveryStoreError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		fail     func(*fakeStore)
		contains string
	}{
		{
			name:     "the plugin's database row cannot be read",
			fail:     func(f *fakeStore) { f.failReadDatabase = errors.New("the connection is gone") },
			contains: "the connection is gone",
		},
		{
			name:     "the role cannot be created",
			fail:     func(f *fakeStore) { f.failProvision = errors.New("permission denied to create role") },
			contains: "permission denied to create role",
		},
		{
			name:     "no connection string can be built",
			fail:     func(f *fakeStore) { f.failDSN = errors.New("the server's own connection is unreadable") },
			contains: "unreadable",
		},
		{
			name:     "the provisioning cannot be recorded",
			fail:     func(f *fakeStore) { f.failRecordProvisioned = errors.New("the row would not write") },
			contains: "the row would not write",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			h := newHarness(t)
			tc.fail(h.store)
			install(t, h.dir, "needsdb", withDatabase("needsdb"))
			install(t, h.dir, "testplugin", manifestFor("testplugin"))

			r.NoError(h.host.Discover(t.Context()), "one plugin's database must not fail discovery")

			eventually(t, "the other plugin to start", func() bool {
				return stateOf(h.host, "testplugin") == plugins.StateRunning
			})
			r.Equal(string(plugins.StateFailed), h.store.record("needsdb").State)
			r.Contains(h.store.record("needsdb").LastError, tc.contains)
		})
	}
}

// Removing a plugin stops at the first thing that will not happen, and says so,
// rather than reporting success over a role that is still there.
func TestRemoveReportsWhatWouldNotHappen(t *testing.T) {
	t.Parallel()

	t.Run("the database will not drop", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		install(t, h.dir, "testplugin", withDatabase("testplugin"))
		r.NoError(h.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

		h.store.failDrop = errors.New("the role is still connected")
		err := h.host.Remove(t.Context(), "testplugin")
		r.Error(err)
		r.Contains(err.Error(), "still connected")
		// The row is kept, so the operator can see what is still there and try
		// again rather than losing track of it.
		r.NotEmpty(h.store.record("testplugin").Name)
	})

	t.Run("the password will not clear", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		install(t, h.dir, "testplugin", withDatabase("testplugin"))
		r.NoError(h.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

		h.store.failClear = errors.New("the row would not write")
		r.Error(h.host.Remove(t.Context(), "testplugin"))
	})

	t.Run("the row will not delete", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		install(t, h.dir, "testplugin", manifestFor("testplugin"))
		r.NoError(h.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool { return stateOf(h.host, "testplugin") == plugins.StateRunning })

		h.store.failDelete = errors.New("the row would not delete")
		err := h.host.Remove(t.Context(), "testplugin")
		r.Error(err)
		r.Contains(err.Error(), "would not delete")
	})
}

// The migration runner's own failures. They are separated from a migration that
// is bad SQL — which is tested against real PostgreSQL — because these are the
// ways the step fails before any SQL is read.
func TestMigrationsFailBeforeAnySQLRuns(t *testing.T) {
	t.Parallel()

	t.Run("the plugin's own role cannot connect", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		// A connection string that parses and will not connect. The port is one
		// nothing listens on rather than a malformed string, so the failure is
		// the one a real unreachable database gives.
		h.store.dsnOverride = "postgres://nobody:nothing@127.0.0.1:1/pacenote?sslmode=disable&connect_timeout=1"
		dir := install(t, h.dir, "needsdb", withDatabase("needsdb"))
		writeMigrationFile(t, dir, "00001_init.sql", "CREATE TABLE t (id int);")
		install(t, h.dir, "testplugin", manifestFor("testplugin"))

		r.NoError(h.host.Discover(t.Context()))
		eventually(t, "the other plugin to start", func() bool {
			return stateOf(h.host, "testplugin") == plugins.StateRunning
		})
		r.Equal(string(plugins.StateFailed), h.store.record("needsdb").State)
		r.Contains(h.store.record("needsdb").LastError, "migrating needsdb")
	})

	t.Run("a migration file cannot be read", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		dir := install(t, h.dir, "needsdb", withDatabase("needsdb"))
		writeMigrationFile(t, dir, "00001_init.sql", "CREATE TABLE t (id int);")
		// Unreadable, which is a file the operator's own permissions broke.
		r.NoError(os.Chmod(filepath.Join(dir, plugin.MigrationsDir, "00001_init.sql"), 0o000))
		t.Cleanup(func() {
			_ = os.Chmod(filepath.Join(dir, plugin.MigrationsDir, "00001_init.sql"), 0o600)
		})
		// It never gets as far as reading: the fake store's DSN does not
		// connect either. What matters is that both are this plugin failing.
		h.store.dsnOverride = "postgres://nobody:nothing@127.0.0.1:1/pacenote?sslmode=disable&connect_timeout=1"

		r.NoError(h.host.Discover(t.Context()))
		r.Equal(string(plugins.StateFailed), h.store.record("needsdb").State)
	})

	t.Run("a plugin with a database and no migrations is fine", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		// No migrations directory at all: a plugin may declare a database and
		// keep its schema entirely in its own code.
		install(t, h.dir, "testplugin", withDatabase("testplugin"))

		r.NoError(h.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool {
			return stateOf(h.host, "testplugin") == plugins.StateRunning
		})
		row, err := h.store.PluginDatabase(t.Context(), "testplugin")
		r.NoError(err)
		r.True(row.Provisioned)
		r.Empty(row.Migration, "a plugin with no migrations recorded one")
	})

	t.Run("everything that is not a .sql file is ignored", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		h := newHarness(t)
		// Unreachable, so the run would fail if it found a migration to apply.
		// It finds none, so it never connects and the plugin starts.
		h.store.dsnOverride = "postgres://nobody:nothing@127.0.0.1:1/pacenote?sslmode=disable&connect_timeout=1"
		dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))
		writeMigrationFile(t, dir, "README.md", "these are the tables")
		writeMigrationFile(t, dir, "notes.txt", "and these are notes")
		r.NoError(os.MkdirAll(filepath.Join(dir, plugin.MigrationsDir, "olddir.sql"), 0o700))

		r.NoError(h.host.Discover(t.Context()))
		eventually(t, "the plugin to start", func() bool {
			return stateOf(h.host, "testplugin") == plugins.StateRunning
		})
	})
}

// writeMigrationFile puts any file in a plugin's migrations directory.
func writeMigrationFile(t *testing.T, dir, name, body string) {
	t.Helper()
	md := filepath.Join(dir, plugin.MigrationsDir)
	require.NoError(t, os.MkdirAll(md, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(md, name), []byte(body), 0o600))
}

// A data key that is the wrong length cannot seal. It cannot arrive through the
// configuration file — ParseSecretKey refuses one — so this is defence against a
// keyring built in code, and the point of the test is that it is refused rather
// than storing the role's password in clear to get past it.
func TestAPluginDatabaseIsRefusedWhenSealingFails(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h := newHarness(t, func(o *plugins.Options) {
		o.Keyring = auth.NewKeyring(auth.SecretKey([]byte("not thirty-two bytes")))
	})
	install(t, h.dir, "needsdb", withDatabase("needsdb"))
	install(t, h.dir, "testplugin", manifestFor("testplugin"))

	r.NoError(h.host.Discover(t.Context()))

	eventually(t, "the other plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})
	r.Equal(string(plugins.StateFailed), h.store.record("needsdb").State)
	r.Contains(h.store.record("needsdb").LastError, "sealing the database password")

	// The role was made — that happens before the seal — but it is not recorded
	// as provisioned, so the next start provisions again rather than believing
	// in a password nobody can read.
	row, err := h.store.PluginDatabase(t.Context(), "needsdb")
	r.ErrorIs(err, db.ErrNotFound)
	r.False(row.Provisioned)
}
