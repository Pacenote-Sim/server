//go:build postgres

package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

// provisionFixture is a migrated database, a store, and a plugin name nothing
// else in the cluster is using. Roles are cluster-wide rather than per-database,
// so a fixed name would collide with a parallel package.
func provisionFixture(t *testing.T) (*db.Store, string, string) {
	t.Helper()
	store, dsn := migrated(t)
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	name := "t" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		// Deliberately the real removal path rather than raw SQL: a cleanup
		// that works where the product's own uninstall would not is a cleanup
		// that hides a bug.
		_ = store.DropPluginDatabase(context.Background(), name)
	})
	return store, dsn, name
}

// asPlugin connects with the role a plugin was given.
func asPlugin(t *testing.T, store *db.Store, name, password string) *pgx.Conn {
	t.Helper()
	dsn, err := store.PluginDSN(name, password)
	require.NoError(t, err)
	conn, err := pgx.Connect(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestProvisionGivesAPluginItsOwnSchemaAndNothingElse(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _, name := provisionFixture(t)
	ctx := context.Background()

	const password = "not-a-real-credential-0123456789"
	r.NoError(store.ProvisionPluginDatabase(ctx, name, password))

	conn := asPlugin(t, store, name, password)

	// It owns a schema and may write in it, unqualified — the search_path the
	// role carries puts its own schema first.
	_, err := conn.Exec(ctx, `CREATE TABLE debriefs (id int PRIMARY KEY, body text)`)
	r.NoError(err)
	_, err = conn.Exec(ctx, `INSERT INTO debriefs VALUES (1, 'lost 14 km/h at the apex')`)
	r.NoError(err)

	// Its table landed in its own schema and not somewhere shared.
	var schema string
	r.NoError(conn.QueryRow(ctx,
		`SELECT table_schema FROM information_schema.tables WHERE table_name = 'debriefs'`).Scan(&schema))
	role, err := store.PluginRoleName(name)
	r.NoError(err)
	r.Equal(role, schema)

	// It reads core through the views.
	var n int
	r.NoError(conn.QueryRow(ctx, `SELECT count(*) FROM drivers`).Scan(&n))
	r.Zero(n)

	// And is refused the tables behind them, and the public schema entirely.
	for _, q := range []string{
		`SELECT * FROM public.drivers`,
		`SELECT * FROM public.settings`,
		`SELECT * FROM public.devices`,
		`CREATE TABLE public.mine (id int)`,
	} {
		_, err := conn.Exec(ctx, q)
		r.Error(err, "a plugin role was allowed to: %s", q)
		r.Contains(err.Error(), "permission denied", "%s failed for the wrong reason", q)
	}
}

// The whole point of the default-privileges line in ProvisionPluginDatabase: a
// view core adds after a plugin was installed must be readable by it, or every
// installed plugin would have to be re-granted on upgrade and the one nobody
// remembered would break.
func TestAPluginCanReadAViewAddedAfterItWasProvisioned(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, dsn, name := provisionFixture(t)
	ctx := context.Background()

	const password = "not-a-real-credential-0123456789"
	r.NoError(store.ProvisionPluginDatabase(ctx, name, password))

	// Core publishes something new, exactly as a later migration would.
	admin, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = admin.Close(ctx) }()
	_, err = admin.Exec(ctx, `CREATE VIEW core_read.later AS SELECT 1 AS answer`)
	r.NoError(err)

	conn := asPlugin(t, store, name, password)
	var answer int
	r.NoError(conn.QueryRow(ctx, `SELECT answer FROM core_read.later`).Scan(&answer),
		"a view added after the plugin was provisioned was not readable by it")
	r.Equal(1, answer)
}

func TestProvisionIsIdempotentAndResetsThePassword(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _, name := provisionFixture(t)
	ctx := context.Background()

	const first = "not-a-real-credential-first-0000"
	r.NoError(store.ProvisionPluginDatabase(ctx, name, first))

	conn := asPlugin(t, store, name, first)
	_, err := conn.Exec(ctx, `CREATE TABLE keep_me (id int)`)
	r.NoError(err)

	// Running it again is what a restart does, and what a crash halfway through
	// the first one leaves behind.
	r.NoError(store.ProvisionPluginDatabase(ctx, name, first))

	// And with a new password — the data key was regenerated, so the old one
	// could not be read and a new one was set on the same role.
	const second = "not-a-real-credential-second-000"
	r.NoError(store.ProvisionPluginDatabase(ctx, name, second))

	// The new password works and the plugin's data survived, which is the whole
	// reason the role is altered rather than dropped and remade.
	again := asPlugin(t, store, name, second)
	var n int
	r.NoError(again.QueryRow(ctx, `SELECT count(*) FROM keep_me`).Scan(&n))
	r.Zero(n)

	// And the old password does not — on a server that checks passwords at all.
	// A development cluster set to trust accepts any of them, which would make
	// this assertion pass for the wrong reason on a real server and fail here
	// for no reason, so it is skipped where it cannot mean anything.
	if !enforcesPasswords(t, store, name) {
		t.Log("this PostgreSQL trusts local connections, so the old password cannot be shown to stop working")
		return
	}
	old, err := store.PluginDSN(name, first)
	r.NoError(err)
	_, err = pgx.Connect(ctx, old)
	r.Error(err, "the previous password still connects")
}

// enforcesPasswords reports whether this server actually checks them, by
// offering one that is certainly wrong. A cluster configured with trust — which
// is the default for local connections in most development installs — answers
// yes to everything, and a test that asserts a wrong password is refused would
// be asserting nothing there.
func enforcesPasswords(t *testing.T, store *db.Store, name string) bool {
	t.Helper()
	dsn, err := store.PluginDSN(name, "certainly-not-the-password-0000000")
	require.NoError(t, err)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return true
	}
	_ = conn.Close(context.Background())
	return false
}

func TestDropRemovesTheRoleTheSchemaAndTheGrants(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, dsn, name := provisionFixture(t)
	ctx := context.Background()

	const password = "not-a-real-credential-0123456789"
	r.NoError(store.ProvisionPluginDatabase(ctx, name, password))
	conn := asPlugin(t, store, name, password)
	_, err := conn.Exec(ctx, `CREATE TABLE data (id int)`)
	r.NoError(err)
	_ = conn.Close(ctx)

	// This is the call that fails if it is written as DROP SCHEMA ... CASCADE
	// followed by DROP ROLE: the role still holds USAGE on core_read and SELECT
	// on its views, and PostgreSQL refuses to drop a role anything depends on.
	r.NoError(store.DropPluginDatabase(ctx, name))

	admin, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = admin.Close(ctx) }()

	role, err := store.PluginRoleName(name)
	r.NoError(err)

	var roles, schemas int
	r.NoError(admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = $1`,
		role).Scan(&roles))
	r.Zero(roles, "the role outlived the plugin")
	r.NoError(admin.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = $1`,
		role).Scan(&schemas))
	r.Zero(schemas, "the schema outlived the plugin")
}

func TestDropIsSafeWhenThereIsNothingToDrop(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _, name := provisionFixture(t)
	// Uninstalling a plugin that never asked for a database must not be an
	// error, or removing one would fail for most plugins.
	r.NoError(store.DropPluginDatabase(context.Background(), name))
	r.NoError(store.DropPluginDatabase(context.Background(), name))
}

func TestPluginDSNPointsAtTheSameDatabaseAsTheServer(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, dsn, name := provisionFixture(t)

	got, err := store.PluginDSN(name, "not-a-real-credential")
	r.NoError(err)

	server, err := url.Parse(dsn)
	r.NoError(err)
	forPlugin, err := url.Parse(got)
	r.NoError(err)

	// Same database, or the join a plugin is for cannot be written.
	r.Equal(server.Path, forPlugin.Path)
	r.Equal(server.Host, forPlugin.Host)
	// Different credentials, and the role is the prefixed one.
	role, err := store.PluginRoleName(name)
	r.NoError(err)
	r.Equal(role, forPlugin.User.Username())
	pw, ok := forPlugin.User.Password()
	r.True(ok)
	r.Equal("not-a-real-credential", pw)
}

// A plugin name is interpolated into DDL because PostgreSQL will not take an
// identifier as a parameter. This is the check that makes that safe, so it is
// worth testing the refusals rather than only the happy name.
func TestProvisionRefusesANameThatIsNotAPluginName(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, dsn := migrated(t)
	ctx := context.Background()

	for _, name := range []string{
		"",
		"Capitals",
		"has space",
		`quote"inside`,
		"semi;colon",
		"back\\slash",
		"-leading-hyphen",
		strings.Repeat("a", 41),
		"drop table drivers",
		"a'; DROP TABLE drivers; --",
	} {
		err := store.ProvisionPluginDatabase(ctx, name, "not-a-real-credential")
		r.Error(err, "provisioning accepted the name %q", name)
		r.Contains(err.Error(), "is not a plugin name")

		err = store.DropPluginDatabase(ctx, name)
		r.Error(err, "dropping accepted the name %q", name)

		_, err = store.PluginDSN(name, "not-a-real-credential")
		r.Error(err, "a DSN was built for the name %q", name)
	}

	// And the tables those names were reaching for are still there. The role
	// list is deliberately not asserted on: roles are cluster-wide, other tests
	// run in parallel against the same server, and a count here would be
	// measuring them rather than this.
	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()
	var n int
	r.NoError(conn.QueryRow(ctx, `SELECT count(*) FROM drivers`).Scan(&n))
	r.Zero(n)
}

func TestProvisionRefusesAnEmptyPassword(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _, name := provisionFixture(t)
	err := store.ProvisionPluginDatabase(context.Background(), name, "")
	r.Error(err)
	r.Contains(err.Error(), "no password")
}

// Two Pacenote servers sharing one PostgreSQL — staging and production on the
// same box, which is an ordinary way to run this — must not share a plugin's
// role. A role is cluster-wide and a database is not.
//
// Before the role name carried the database, installing the same plugin on both
// gave them one role: each server's plugin could connect to the other's database
// and read its tables, and installing on either rotated the password the other
// was using. Both were measured; this is the test that keeps them closed.
func TestTwoServersOnOneClusterDoNotShareAPluginRole(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	prod, prodDSN := migrated(t)
	staging, stagingDSN := migrated(t)
	r.NotEqual(prodDSN, stagingDSN, "these are meant to be two databases")

	const name = "engineer"
	const prodPassword = "not-a-real-credential-prod-0000"
	const stagingPassword = "not-a-real-credential-stage-000"

	r.NoError(prod.ProvisionPluginDatabase(ctx, name, prodPassword))
	t.Cleanup(func() { _ = prod.DropPluginDatabase(context.Background(), name) })
	r.NoError(staging.ProvisionPluginDatabase(ctx, name, stagingPassword))
	t.Cleanup(func() { _ = staging.DropPluginDatabase(context.Background(), name) })

	prodRole, err := prod.PluginRoleName(name)
	r.NoError(err)
	stagingRole, err := staging.PluginRoleName(name)
	r.NoError(err)
	r.NotEqual(prodRole, stagingRole, "one plugin on two servers got one role")

	// Production's plugin writes something.
	onProd := asPlugin(t, prod, name, prodPassword)
	_, err = onProd.Exec(ctx, `CREATE TABLE debriefs (body text)`)
	r.NoError(err)
	_, err = onProd.Exec(ctx, `INSERT INTO debriefs VALUES ('production coaching')`)
	r.NoError(err)

	// The staging server's plugin points at the production database with its
	// own credentials. It must not get in — and if it does, it must not be able
	// to read production's schema.
	crossed, err := url.Parse(prodDSN)
	r.NoError(err)
	crossed.User = url.UserPassword(stagingRole, stagingPassword)

	conn, err := pgx.Connect(ctx, crossed.String())
	if err != nil {
		return // refused at the door, which is the best outcome
	}
	defer func() { _ = conn.Close(ctx) }()
	var body string
	err = conn.QueryRow(ctx, `SELECT body FROM `+prodRole+`.debriefs`).Scan(&body)
	r.Error(err, "one server's plugin read another server's plugin data")

	// And installing on staging did not rotate the password production is
	// holding, which would have broken it at its next restart.
	stillWorks := asPlugin(t, prod, name, prodPassword)
	r.NoError(stillWorks.QueryRow(ctx, `SELECT body FROM debriefs`).Scan(&body))
	r.Equal("production coaching", body)
}
