package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// The parts of the plugin-database code that are pure functions of their input.
// They are the parts that make interpolating a name into DDL safe, so they are
// worth testing directly rather than through a database that would only show
// that one particular name worked.

func TestRoleNameIsScopedToTheDatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	got, err := roleName("engineer", "pacenote")
	r.NoError(err)
	r.True(strings.HasPrefix(got, "plugin_engineer_"), "unexpected role name %q", got)
	r.LessOrEqual(len(got), 63, "PostgreSQL will not take an identifier this long")

	// The same plugin on the same database is the same role, every time.
	again, err := roleName("engineer", "pacenote")
	r.NoError(err)
	r.Equal(got, again)

	// And on a different database it is a different role. This is the whole
	// point: a role is cluster-wide and a database is not, so two servers
	// sharing a PostgreSQL would otherwise share one role for the same plugin —
	// and each could then read the other's data and rotate the other's password.
	staging, err := roleName("engineer", "pacenote_staging")
	r.NoError(err)
	r.NotEqual(got, staging)

	// The longest name a plugin may have still fits.
	long, err := roleName(strings.Repeat("a", 40), "a-database-with-a-very-long-name-indeed")
	r.NoError(err)
	r.LessOrEqual(len(long), 63)

	for _, bad := range []string{
		"", "Capitals", "has space", `quote"inside`, "semi;colon", "back\\slash",
		"-leading", "_leading", strings.Repeat("a", 41), "a'; DROP TABLE drivers; --",
		"tab\there", "new\nline",
	} {
		_, err := roleName(bad, "pacenote")
		r.Error(err, "roleName(%q) was accepted", bad)
		r.Contains(err.Error(), "is not a plugin name")
	}

	// Forty is the limit and not past it.
	_, err = roleName(strings.Repeat("a", 40), "pacenote")
	r.NoError(err)
}

// The password goes into CREATE ROLE as a literal because PostgreSQL will not
// take a parameter there. This is what makes that safe.
func TestQuoteLiteral(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal(`E'abc'`, quoteLiteral("abc"))
	r.Equal(`E''`, quoteLiteral(""))
	r.Equal(`E'it''s'`, quoteLiteral("it's"), "a quote is doubled")
	r.Equal(`E'a\\b'`, quoteLiteral(`a\b`), "a backslash is doubled")
	r.Equal(`E'''; DROP TABLE drivers; --'`, quoteLiteral(`'; DROP TABLE drivers; --`))
	// The generated password's alphabet, which needs no escaping at all and
	// must come back unchanged.
	r.Equal(`E'0123456789abcdef'`, quoteLiteral("0123456789abcdef"))
}

// PostgreSQL does not usually echo a statement back in an error, but "usually"
// is not a property to rely on for a credential.
func TestRedactPassword(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.NoError(redactPassword(nil, "secret"))

	plain := errors.New("syntax error near CREATE ROLE")
	r.Equal(plain, redactPassword(plain, "secret"),
		"an error with no password in it is returned unchanged, wrapper and all")

	echoed := errors.New(`ERROR: near CREATE ROLE "plugin_x" LOGIN PASSWORD E'deadbeef'`)
	got := redactPassword(echoed, "deadbeef")
	r.NotContains(got.Error(), "deadbeef")
	r.Contains(got.Error(), "[redacted]")

	// An empty password would otherwise match everywhere and redact the whole
	// message into nothing.
	r.Equal(plain, redactPassword(plain, ""))
}

// The sslmode a plugin connects with follows the server's own. It is the
// nearest honest answer pgx makes available: it keeps the resolved TLS
// configuration rather than the word that produced it.
func TestSSLModeFollowsTheServersOwnConnection(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("disable", sslModeOf(&pgx.ConnConfig{}))

	cfg, err := pgx.ParseConfig("postgres://u:p@localhost:5432/db?sslmode=require")
	r.NoError(err)
	r.Equal("require", sslModeOf(cfg))

	cfg, err = pgx.ParseConfig("postgres://u:p@localhost:5432/db?sslmode=disable")
	r.NoError(err)
	r.Equal("disable", sslModeOf(cfg))

	// prefer, and the connection string that says nothing — which is prefer.
	// These are the ones worth pinning: they carry a TLS configuration like
	// require does, so reading that alone called them require, and a plugin
	// told to require TLS against a PostgreSQL with ssl off cannot connect at
	// all to the database the server is already using.
	for _, dsn := range []string{
		"postgres://u:p@localhost:5432/db?sslmode=prefer",
		"postgres://u:p@localhost:5432/db",
	} {
		cfg, err = pgx.ParseConfig(dsn)
		r.NoError(err)
		r.NotNil(cfg.TLSConfig, "%s stopped offering TLS, and this test is no longer about anything", dsn)
		r.Equal("prefer", sslModeOf(cfg), "%s", dsn)
	}
}
