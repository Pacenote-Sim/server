//go:build postgres

package plugins_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
	"github.com/pacenote-sim/server/internal/plugins"
)

// Everything else in this package runs against a store in memory, because the
// host's failure paths have to be testable on a machine with no PostgreSQL. This
// file is the other half: a real database, a real role, a real plugin process,
// and the migrations it shipped actually applied.
//
// It is the only test that proves the whole chain, and the chain is the product:
// a plugin declares a database, gets one it owns, and joins its own tables to
// core's in one query.

// realStore is a migrated database and a store over it.
func realStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	dsn := dbtest.URL(t)
	store, err := db.Open(context.Background(), dsn, logging.Discard())
	require.NoError(t, err)
	t.Cleanup(store.Close)
	require.NoError(t, store.Migrate(context.Background()))
	return store, dsn
}

// writeMigration puts a .sql file in a plugin's migrations directory.
func writeMigration(t *testing.T, dir, name, body string) {
	t.Helper()
	md := filepath.Join(dir, plugin.MigrationsDir)
	require.NoError(t, os.MkdirAll(md, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(md, name), []byte(body), 0o600))
}

func TestAPluginsMigrationsRunAgainstItsOwnSchema(t *testing.T) {
	r := require.New(t)
	store, serverDSN := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))
	writeMigration(t, dir, "00001_init.sql", `
		CREATE TABLE debriefs (
			id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			lap_id   bigint NOT NULL,
			verdict  text   NOT NULL
		);
		CREATE INDEX debriefs_lap ON debriefs (lap_id);
	`)
	writeMigration(t, dir, "00002_more.sql", `ALTER TABLE debriefs ADD COLUMN confidence int NOT NULL DEFAULT 0;`)

	t.Cleanup(func() { _ = store.DropPluginDatabase(context.Background(), "testplugin") })

	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	// The plugin was told it has a database.
	eventually(t, "the plugin to report its database", func() bool {
		return strings.Contains(statusOf(t, h.host, "testplugin").LastOutput, "database yes")
	})

	row, err := store.PluginDatabase(ctx, "testplugin")
	r.NoError(err)
	r.True(row.Provisioned)
	r.Equal("00002_more.sql", row.Migration, "the last migration applied was not recorded")

	password, err := h.key.Open(row.PasswordSealed)
	r.NoError(err)
	pluginDSN, err := store.PluginDSN("testplugin", password)
	r.NoError(err)

	conn, err := pgx.Connect(ctx, pluginDSN)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	// Both migrations ran, in order, in the plugin's own schema.
	var schema string
	r.NoError(conn.QueryRow(ctx,
		`SELECT table_schema FROM information_schema.tables WHERE table_name = 'debriefs'`).Scan(&schema))
	role, err := store.PluginRoleName("testplugin")
	r.NoError(err)
	r.Equal(role, schema)

	var columns int
	r.NoError(conn.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'debriefs' AND column_name = 'confidence'`).Scan(&columns))
	r.Equal(1, columns, "the second migration did not run")

	// And the thing all of this is for: the plugin's table joined to core's data,
	// one query, one connection.
	admin, err := pgx.Connect(ctx, serverDSN)
	r.NoError(err)
	defer func() { _ = admin.Close(ctx) }()
	_, err = admin.Exec(ctx, `
		INSERT INTO drivers (name, slug) VALUES ('Mihai', 'mihai');
		INSERT INTO stints (id, driver_id, sim, track_id, track, car, car_class, session_type, started_at)
		VALUES ('22222222-2222-2222-2222-222222222222',
		        (SELECT id FROM drivers WHERE slug = 'mihai'),
		        'iracing', 'spa', 'Spa', 'GT3', 'GT3', 'practice', now());
		INSERT INTO laps (stint_id, number, lap_ms, kind, started_at, track_id, car, car_class,
		                  driver_id, content_sha256, trace_codec, trace, sim, corners)
		VALUES ('22222222-2222-2222-2222-222222222222', 1, 138400, 'clean', now(),
		        'spa', 'GT3', 'GT3', (SELECT id FROM drivers WHERE slug = 'mihai'),
		        '\x00', 1, '\xdeadbeef', 'iracing', '[{"turn":1,"deficit_kmh":14}]')`)
	r.NoError(err)

	_, err = conn.Exec(ctx,
		`INSERT INTO debriefs (lap_id, verdict) SELECT id, 'lost 14 km/h at the apex' FROM laps LIMIT 1`)
	r.NoError(err)

	var driver, track, verdict string
	var lapMS int
	r.NoError(conn.QueryRow(ctx, `
		SELECT d.name, s.track, l.lap_ms, db.verdict
		FROM debriefs db
		JOIN laps l    ON l.id = db.lap_id
		JOIN stints s  ON s.id = l.stint_id
		JOIN drivers d ON d.id = l.driver_id`).Scan(&driver, &track, &lapMS, &verdict))
	r.Equal("Mihai", driver)
	r.Equal("Spa", track)
	r.Equal(138400, lapMS)
	r.Equal("lost 14 km/h at the apex", verdict)
}

func TestAPluginsMigrationsRunOnceAndItsDataSurvivesARescan(t *testing.T) {
	r := require.New(t)
	store, _ := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))
	// A migration that would fail if it ran twice, which is the honest way to
	// test that it does not.
	writeMigration(t, dir, "00001_init.sql", `CREATE TABLE notes (id int PRIMARY KEY, body text);`)
	t.Cleanup(func() { _ = store.DropPluginDatabase(context.Background(), "testplugin") })

	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	row, err := store.PluginDatabase(ctx, "testplugin")
	r.NoError(err)
	password, err := h.key.Open(row.PasswordSealed)
	r.NoError(err)
	dsn, err := store.PluginDSN("testplugin", password)
	r.NoError(err)

	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	_, err = conn.Exec(ctx, `INSERT INTO notes VALUES (1, 'written before the rescan')`)
	r.NoError(err)
	_ = conn.Close(ctx)

	// A rescan, which is what pressing the button in the panel will do, and what
	// every restart of the server does.
	r.NoError(h.host.Discover(ctx))
	r.Equal(plugins.StateRunning, stateOf(h.host, "testplugin"))

	// The password did not change, so the connection string a running plugin is
	// holding is still good.
	after, err := store.PluginDatabase(ctx, "testplugin")
	r.NoError(err)
	stillTheSame, err := h.key.Open(after.PasswordSealed)
	r.NoError(err)
	r.Equal(password, stillTheSame, "a rescan rotated the plugin's password")

	// And the row it wrote is still there: the migration did not run again.
	again, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = again.Close(ctx) }()
	var body string
	r.NoError(again.QueryRow(ctx, `SELECT body FROM notes WHERE id = 1`).Scan(&body))
	r.Equal("written before the rescan", body)
}

// A migration that will not apply is this plugin failing. The server comes up,
// the other plugins run, and the panel says what was wrong with this one.
func TestAPluginWithABrokenMigrationFailsAlone(t *testing.T) {
	r := require.New(t)
	store, _ := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "brokenschema", withDatabase("brokenschema"))
	writeMigration(t, dir, "00001_init.sql", `CREATE TABLE fine (id int);`)
	writeMigration(t, dir, "00002_bad.sql", `THIS IS NOT SQL;`)
	install(t, h.dir, "testplugin", manifestFor("testplugin"))
	t.Cleanup(func() { _ = store.DropPluginDatabase(context.Background(), "brokenschema") })

	r.NoError(h.host.Discover(ctx), "a plugin with a broken migration must not fail discovery")

	eventually(t, "the other plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	rec, err := store.Plugin(ctx, "brokenschema")
	r.NoError(err)
	r.Equal(string(plugins.StateFailed), rec.State)
	r.Contains(rec.LastError, "00002_bad.sql")

	// The first migration was applied and committed; the second was rolled back
	// whole. A half-applied migration is the thing this must never leave.
	row, err := store.PluginDatabase(ctx, "brokenschema")
	r.NoError(err)
	r.False(row.Provisioned, "a plugin whose migrations failed was recorded as provisioned")
}

func TestDeprovisioningRemovesEverything(t *testing.T) {
	r := require.New(t)
	store, serverDSN := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))
	writeMigration(t, dir, "00001_init.sql", `CREATE TABLE notes (id int);`)

	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	r.NoError(store.DropPluginDatabase(ctx, "testplugin"))
	r.NoError(store.ClearPluginDatabase(ctx, "testplugin"))

	admin, err := pgx.Connect(ctx, serverDSN)
	r.NoError(err)
	defer func() { _ = admin.Close(ctx) }()

	role, err := store.PluginRoleName("testplugin")
	r.NoError(err)

	var n int
	r.NoError(admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = $1`,
		role).Scan(&n))
	r.Zero(n, "the role outlived the plugin")
	r.NoError(admin.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = $1`,
		role).Scan(&n))
	r.Zero(n, "the schema outlived the plugin")

	row, err := store.PluginDatabase(ctx, "testplugin")
	r.NoError(err)
	r.False(row.Provisioned)
	r.Empty(row.PasswordSealed, "the password outlived the role it opened")
}

// A restart re-runs provisioning, and the migrations already applied are
// skipped. A rescan does not reach this — Discover leaves a running plugin
// alone — so a restart of the server is the case that has to be right.
func TestMigrationsAlreadyAppliedAreSkippedOnARestart(t *testing.T) {
	r := require.New(t)
	store, _ := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "testplugin", withDatabase("testplugin"))
	writeMigration(t, dir, "00001_init.sql", `CREATE TABLE notes (id int PRIMARY KEY, body text);`)
	t.Cleanup(func() { _ = store.DropPluginDatabase(context.Background(), "testplugin") })

	r.NoError(h.host.Discover(ctx))
	eventually(t, "the plugin to start", func() bool {
		return stateOf(h.host, "testplugin") == plugins.StateRunning
	})

	row, err := store.PluginDatabase(ctx, "testplugin")
	r.NoError(err)
	password, err := h.key.Open(row.PasswordSealed)
	r.NoError(err)
	dsn, err := store.PluginDSN("testplugin", password)
	r.NoError(err)
	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	_, err = conn.Exec(ctx, `INSERT INTO notes VALUES (1, 'written before the restart')`)
	r.NoError(err)
	_ = conn.Close(ctx)

	// The server restarts: the old host stops first, the way a restart actually
	// goes, then a new one comes up over the same directory, store and key.
	h.host.Close()
	again := restart(t, h, func(o *plugins.Options) { o.Store = store })
	r.NoError(again.host.Discover(ctx))
	eventually(t, "the plugin to start again", func() bool {
		return stateOf(again.host, "testplugin") == plugins.StateRunning
	})

	after, err := store.PluginDatabase(ctx, "testplugin")
	r.NoError(err)
	r.Equal("00001_init.sql", after.Migration)

	// The migration did not run again — it would have failed on the existing
	// table — and the row it wrote is still there.
	back, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = back.Close(ctx) }()
	var body string
	r.NoError(back.QueryRow(ctx, `SELECT body FROM notes WHERE id = 1`).Scan(&body))
	r.Equal("written before the restart", body)
}

// A migration file the operator's own permissions made unreadable. It is this
// plugin failing with the file named, not a server that will not come up.
func TestAMigrationFileThatCannotBeRead(t *testing.T) {
	r := require.New(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a file with no permission bits set")
	}
	store, _ := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "unreadable", withDatabase("unreadable"))
	writeMigration(t, dir, "00001_init.sql", `CREATE TABLE t (id int);`)
	path := filepath.Join(dir, plugin.MigrationsDir, "00001_init.sql")
	r.NoError(os.Chmod(path, 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(path, 0o600)
		_ = store.DropPluginDatabase(context.Background(), "unreadable")
	})

	r.NoError(h.host.Discover(ctx), "an unreadable migration must not fail the whole scan")

	rec, err := store.Plugin(ctx, "unreadable")
	r.NoError(err)
	r.Equal(string(plugins.StateFailed), rec.State)
	r.Contains(rec.LastError, "00001_init.sql")
}

// A migrations directory that cannot be listed at all.
func TestAMigrationsDirectoryThatCannotBeRead(t *testing.T) {
	r := require.New(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a directory with no permission bits set")
	}
	store, _ := realStore(t)
	ctx := context.Background()

	h := newHarness(t, func(o *plugins.Options) { o.Store = store })
	dir := install(t, h.dir, "shutout", withDatabase("shutout"))
	writeMigration(t, dir, "00001_init.sql", `CREATE TABLE t (id int);`)
	md := filepath.Join(dir, plugin.MigrationsDir)
	r.NoError(os.Chmod(md, 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(md, 0o700)
		_ = store.DropPluginDatabase(context.Background(), "shutout")
	})

	r.NoError(h.host.Discover(ctx))

	rec, err := store.Plugin(ctx, "shutout")
	r.NoError(err)
	r.Equal(string(plugins.StateFailed), rec.State)
	r.Contains(rec.LastError, plugin.MigrationsDir)
}

// Each migration runs inside a transaction with the row that records it. A
// plugin author writing that row themselves is a thing somebody will eventually
// do, and it must fail loudly rather than leave a migration marked as run that
// the next start would then skip.
func TestAMigrationThatBreaksItsOwnTransaction(t *testing.T) {
	t.Run("it writes the migration table itself", func(t *testing.T) {
		r := require.New(t)
		store, _ := realStore(t)
		ctx := context.Background()

		h := newHarness(t, func(o *plugins.Options) { o.Store = store })
		dir := install(t, h.dir, "tampers", withDatabase("tampers"))
		// Claiming to have run is the host's to write, and the primary key on
		// that table is what makes the claim fail rather than silently skip the
		// migration on the next start.
		writeMigration(t, dir, "00001_init.sql",
			"CREATE TABLE t (id int);\nINSERT INTO _pacenote_migrations (name) VALUES ('00001_init.sql');")
		t.Cleanup(func() { _ = store.DropPluginDatabase(context.Background(), "tampers") })

		r.NoError(h.host.Discover(ctx))

		rec, err := store.Plugin(ctx, "tampers")
		r.NoError(err)
		r.Equal(string(plugins.StateFailed), rec.State)
		r.Contains(rec.LastError, "recording 00001_init.sql")

		row, err := store.PluginDatabase(ctx, "tampers")
		r.NoError(err)
		r.False(row.Provisioned)
	})
}
