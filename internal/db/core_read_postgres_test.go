//go:build postgres

package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// The core_read schema is a contract with plugins, including plugins written by
// people we will never meet against a server version we no longer ship. A column
// may be added to it. A column may not be renamed or removed without a plugin
// interface version, because doing so breaks those plugins silently at whatever
// upgrade the operator happens to run.
//
// So it is written down twice: once in the migration, once here. A change to
// either without the other fails, which is the only way a promise of this shape
// survives a year of refactoring.
var coreReadColumns = map[string][]string{
	"drivers": {
		"avatar_url", "class", "created_at", "external_id", "id", "name", "slug",
	},
	"stints": {
		"car", "car_class", "created_at", "driver_id", "finished_at", "id",
		"sectors", "session_type", "setup", "sim", "started_at", "track", "track_id",
	},
	"laps": {
		"car", "car_class", "corners", "created_at", "driver_id", "id", "kind",
		"lap_ms", "number", "sim", "started_at", "stint_id", "track_id", "trace_bytes",
	},
	"stint_summaries": {
		"avg_lap_ms", "best_lap_id", "best_lap_ms", "car_state", "conditions",
		"consistency_pct", "finished_at", "incidents", "laps", "stint_id",
		"top_speed_kmh", "updated_at",
	},
	"reference_laps": {
		"car", "car_class", "driver_id", "lap_id", "lap_ms", "scope", "sim",
		"track_id", "updated_at",
	},
}

func TestCoreReadPublishesExactlyTheAgreedColumns(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	_, dsn := migrated(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx,
		`SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = 'core_read' ORDER BY table_name, column_name`)
	r.NoError(err)
	defer rows.Close()

	got := map[string][]string{}
	for rows.Next() {
		var view, column string
		r.NoError(rows.Scan(&view, &column))
		got[view] = append(got[view], column)
	}
	r.NoError(rows.Err())

	r.Equal(names(coreReadColumns), names(got),
		"core_read publishes a different set of views than the contract lists")
	for view, want := range coreReadColumns {
		sort.Strings(want)
		r.Equal(want, got[view],
			"core_read.%s: a column was added, renamed or removed. Adding one is fine — "+
				"add it here too. Renaming or removing one breaks every installed plugin "+
				"and needs a plugin interface version.", view)
	}
}

// The reason the views exist at all. A plugin granted the view is not granted
// the table, so a column left out of a view is a column a plugin cannot reach —
// and the ones left out are the ones that would be worst to hand over.
func TestCoreReadOmitsWhatMustNotLeave(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	_, dsn := migrated(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	// Whole tables that are published nowhere.
	for _, table := range []string{
		"admins", "admin_sessions", "settings", "devices", "pairings",
		"plugins", "plugin_settings", "plugin_usage", "audit_log",
		"idempotency_keys", "client_builds", "llm_usage", "setup_state",
	} {
		var n int
		r.NoError(conn.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'core_read' AND table_name = $1`, table).Scan(&n))
		r.Zero(n, "core_read publishes %s, which holds credentials or core bookkeeping", table)
	}

	// Columns dropped from views that are otherwise published.
	for _, c := range []struct{ view, column string }{
		{"laps", "trace"},          // codec-versioned blob; the host decodes it
		{"laps", "trace_codec"},    // meaningless without the trace
		{"laps", "content_sha256"}, // how core deduplicates an upload
		{"stint_summaries", "best_trace"},
		{"stint_summaries", "best_trace_codec"},
	} {
		var n int
		r.NoError(conn.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = 'core_read' AND table_name = $1 AND column_name = $2`,
			c.view, c.column).Scan(&n))
		r.Zero(n, "core_read.%s publishes %s", c.view, c.column)
	}
}

// The whole boundary, exercised the way the host will build it: a role of its
// own, a schema of its own, USAGE and SELECT on core_read and nothing else.
//
// It asserts both halves, because either one alone is worthless. A plugin that
// cannot read core is useless; a plugin that can read the tables behind the
// views has been handed every sealed credential the server holds.
func TestPluginRoleReadsTheViewsAndNotTheTables(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	_, dsn := migrated(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	role := "plugin_t" + randomRoleSuffix(t)
	const password = "not-a-real-credential"

	// The exact sequence the host will run at install. If the test user may not
	// create roles this is the line that says so, and the test skips rather than
	// failing: it is the environment that is short, not the code.
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, password),
		// Required on PostgreSQL 15: a CREATEROLE role is not automatically a
		// member of the roles it creates, and CREATE SCHEMA ... AUTHORIZATION
		// refuses without membership. PostgreSQL 16 grants it automatically and
		// this is a no-op there.
		fmt.Sprintf(`GRANT %s TO CURRENT_USER`, role),
		fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, role, role),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA core_read TO %s`, role),
		fmt.Sprintf(`GRANT SELECT ON ALL TABLES IN SCHEMA core_read TO %s`, role),
		fmt.Sprintf(`ALTER ROLE %s SET search_path = %s, core_read`, role, role),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "permission denied") ||
				strings.Contains(err.Error(), "must have CREATEROLE") {
				t.Skipf("the test user may not create roles, so the plugin boundary cannot be exercised here: %v", err)
			}
			r.NoError(err, "statement: %s", stmt)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		dropRole(context.Background(), c, role)
	})

	// One driver, one stint, one lap, so the join below has something to return.
	_, err = conn.Exec(ctx, `
		INSERT INTO drivers (name, slug) VALUES ('Mihai', 'mihai');
		INSERT INTO stints (id, driver_id, sim, track_id, track, car, car_class, session_type, started_at)
		VALUES ('11111111-1111-1111-1111-111111111111',
		        (SELECT id FROM drivers WHERE slug = 'mihai'),
		        'iracing', 'spa', 'Spa', 'GT3', 'GT3', 'practice', now());
		INSERT INTO laps (stint_id, number, lap_ms, kind, started_at, track_id, car, car_class,
		                  driver_id, content_sha256, trace_codec, trace, sim, corners)
		VALUES ('11111111-1111-1111-1111-111111111111', 1, 138400, 'clean', now(),
		        'spa', 'GT3', 'GT3', (SELECT id FROM drivers WHERE slug = 'mihai'),
		        '\x00', 1, '\xdeadbeef', 'iracing', '[{"turn":1,"deficit_kmh":14}]')`)
	r.NoError(err)

	asPlugin, err := pgx.Connect(ctx, withCredentials(t, dsn, role, password))
	r.NoError(err)
	defer func() { _ = asPlugin.Close(ctx) }()

	// It owns its schema: it can create and write a table of its own.
	_, err = asPlugin.Exec(ctx, `CREATE TABLE debriefs (lap_id bigint, verdict text)`)
	r.NoError(err, "a plugin must be able to create tables in its own schema")
	_, err = asPlugin.Exec(ctx, `INSERT INTO debriefs VALUES (1, 'lost 14 km/h at the apex')`)
	r.NoError(err)

	// It joins that table to core's data, unqualified, the way core's own SQL
	// reads. This is the thing the whole design is for.
	var driver, track, verdict string
	var lapMS int
	r.NoError(asPlugin.QueryRow(ctx, `
		SELECT d.name, s.track, l.lap_ms, db.verdict
		FROM debriefs db
		JOIN laps l ON l.id = db.lap_id
		JOIN stints s ON s.id = l.stint_id
		JOIN drivers d ON d.id = l.driver_id`).Scan(&driver, &track, &lapMS, &verdict))
	r.Equal("Mihai", driver)
	r.Equal("Spa", track)
	r.Equal(138400, lapMS)
	r.Equal("lost 14 km/h at the apex", verdict)

	// And it is refused every table behind those views.
	for _, table := range []string{"laps", "stints", "drivers", "settings", "devices", "admins", "audit_log"} {
		_, err := asPlugin.Exec(ctx, `SELECT * FROM public.`+table)
		r.Error(err, "a plugin role could read public.%s directly", table)
		r.Contains(err.Error(), "permission denied",
			"reading public.%s failed for the wrong reason", table)
	}

	// The trace is not merely unreadable, it is not there to ask for.
	_, err = asPlugin.Exec(ctx, `SELECT trace FROM laps`)
	r.Error(err)
	r.Contains(err.Error(), "does not exist")
}

// A second plugin's role cannot see the first's tables, or learn that it has
// any. This is why there is a role per plugin rather than one shared role: a
// free plugin must not be able to read a paid one's data.
func TestPluginRolesCannotSeeEachOther(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	_, dsn := migrated(t)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	r.NoError(err)
	defer func() { _ = conn.Close(ctx) }()

	suffix := randomRoleSuffix(t)
	a, b := "plugin_a"+suffix, "plugin_b"+suffix
	const password = "not-a-real-credential"

	for _, role := range []string{a, b} {
		for _, stmt := range []string{
			fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, password),
			fmt.Sprintf(`GRANT %s TO CURRENT_USER`, role),
			fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, role, role),
			fmt.Sprintf(`ALTER ROLE %s SET search_path = %s, core_read`, role, role),
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				if strings.Contains(err.Error(), "permission denied") ||
					strings.Contains(err.Error(), "must have CREATEROLE") {
					t.Skipf("the test user may not create roles: %v", err)
				}
				r.NoError(err, "statement: %s", stmt)
			}
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		for _, role := range []string{a, b} {
			dropRole(context.Background(), c, role)
		}
	})

	asB, err := pgx.Connect(ctx, withCredentials(t, dsn, b, password))
	r.NoError(err)
	defer func() { _ = asB.Close(ctx) }()
	_, err = asB.Exec(ctx, `CREATE TABLE paid_results (id int); INSERT INTO paid_results VALUES (1)`)
	r.NoError(err)

	asA, err := pgx.Connect(ctx, withCredentials(t, dsn, a, password))
	r.NoError(err)
	defer func() { _ = asA.Close(ctx) }()

	_, err = asA.Exec(ctx, `SELECT * FROM `+b+`.paid_results`)
	r.Error(err, "one plugin read another plugin's table")

	// Not even the existence of it.
	var n int
	r.NoError(asA.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1`, b).Scan(&n))
	r.Zero(n, "one plugin can enumerate another plugin's tables")
}

// dropRole is the uninstall sequence, and it is two statements for a reason.
// DROP SCHEMA CASCADE removes the plugin's tables but not the grants it holds on
// core_read, and a role that still holds a grant cannot be dropped — PostgreSQL
// refuses with "cannot be dropped because some objects depend on it". DROP OWNED
// BY revokes those and drops what the role owns in this database, which is the
// schema and its contents. A role left behind is not a tidiness problem: roles
// are cluster-wide, so it would collide with the next run.
func dropRole(ctx context.Context, c *pgx.Conn, role string) {
	_, _ = c.Exec(ctx, `DROP OWNED BY `+role+` CASCADE`)
	_, _ = c.Exec(ctx, `DROP ROLE IF EXISTS `+role)
}

// randomRoleSuffix keeps role names unique. Roles are cluster-wide, not
// per-database, so two packages running in parallel against the same server
// would otherwise collide on a name.
func randomRoleSuffix(tb testing.TB) string {
	tb.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(tb, err)
	return hex.EncodeToString(b[:])
}

func names[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// withCredentials rewrites a connection string to use another role. The password
// is a literal in the test, so there is nothing here worth protecting; what
// matters is that the host and port come from the same place the test's own
// connection does.
func withCredentials(tb testing.TB, dsn, user, password string) string {
	tb.Helper()
	u, err := url.Parse(dsn)
	require.NoError(tb, err)
	u.User = url.UserPassword(user, password)
	return u.String()
}
