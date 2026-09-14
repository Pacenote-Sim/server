package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A plugin's database is a PostgreSQL role and a schema, and this file is the
// only place either is created or destroyed.
//
// None of it can be an sqlc query. Every statement here names a role or a schema
// whose name is not known until a plugin is installed, and an identifier cannot
// be a bind parameter in PostgreSQL — so the names are interpolated, which makes
// validating them the entire security of this file rather than a nicety. They
// are checked twice: the plugin interface module refuses a manifest whose name
// is not a plugin name, and [pluginNamePattern] refuses it again here, on the
// principle that the check nearest the statement is the one that must hold.
//
// Quoting is [pgx.Identifier.Sanitize] rather than fmt, because a plugin name
// may contain a hyphen and plugin_my-plugin is three tokens unquoted.

// PluginRolePrefix is what a plugin's role and schema are called. It is a prefix
// rather than a bare name so that a plugin can never be granted a role the
// operator made for something else by choosing its name carefully.
const PluginRolePrefix = "plugin_"

// pluginNamePattern is what may be interpolated into a statement here. It is
// deliberately narrower than what PostgreSQL would accept: no quotes, no
// backslashes, no spaces, nothing that has to be escaped to be safe, so that a
// name which passes cannot be a statement.
//
// Forty characters, because the identifier limit is sixty-three bytes and the
// prefix and a suffix have to fit inside it.
var pluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// roleName is the PostgreSQL role and schema a plugin owns, unquoted.
//
// It carries a fingerprint of the database because a PostgreSQL role is
// cluster-wide and a database is not. Two Pacenote servers on one cluster —
// staging and production on the same box, which is an ordinary way to run this
// — would otherwise share one role for the same plugin, and that is worse than
// untidy in two ways that were both measured before this was written.
//
// The staging server's plugin could connect to the production database and read
// the production plugin's tables, because it is the same role and the role is
// granted the schema in both. And installing the plugin on one server rotates
// the password, so the other server's plugin stops being able to connect at its
// next restart, with nothing anywhere connecting the two events.
//
// The fingerprint is eight hex characters of the database name. It is not a
// secret and does not need to be: it exists to make two roles different, not to
// make one hard to guess.
func roleName(name, database string) (string, error) {
	if !pluginNamePattern.MatchString(name) {
		return "", fmt.Errorf("db: %q is not a plugin name, so no role may be made for it — lowercase letters, digits, underscores and hyphens, starting with a letter or digit, at most forty characters", name)
	}
	sum := sha256.Sum256([]byte(database))
	// 7 for the prefix, at most 40 for the name, 1 for the separator and 8 for
	// the fingerprint is 56, inside PostgreSQL's limit of 63 bytes.
	return PluginRolePrefix + name + "_" + hex.EncodeToString(sum[:4]), nil
}

// PluginRoleName is the role and schema this store gives a plugin. It is
// exported because a caller that wants to look at a plugin's schema — a support
// query, a test — cannot work it out without knowing which database it is for.
func (s *Store) PluginRoleName(name string) (string, error) {
	return roleName(name, s.pool.Config().ConnConfig.Database)
}

// pluginRole is [roleName] quoted for use in a statement. It quotes with
// [pgx.Identifier] rather than fmt because a plugin name may contain a hyphen
// and plugin_my-plugin is three tokens unquoted.
func (s *Store) pluginRole(name string) (string, error) {
	role, err := roleName(name, s.pool.Config().ConnConfig.Database)
	if err != nil {
		return "", err
	}
	return pgx.Identifier{role}.Sanitize(), nil
}

// ProvisionPluginDatabase creates the role and schema a plugin owns, and grants
// it read access to the core_read views and nothing else.
//
// It is idempotent, and has to be: a server that crashed between creating the
// role and recording that it had must be able to run this again and arrive
// somewhere correct. Every statement either tolerates the object already
// existing or is safe to repeat, and the password is set on every call so that a
// role left over from a half-finished install is reachable again rather than
// stranded.
//
// The caller holds the password and is responsible for sealing it. This function
// never logs it, and the statement that carries it is the only place it appears.
func (s *Store) ProvisionPluginDatabase(ctx context.Context, name, password string) error {
	role, err := s.pluginRole(name)
	if err != nil {
		return err
	}
	bare, err := roleName(name, s.pool.Config().ConnConfig.Database)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("db: plugin %s cannot be given a role with no password", name)
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: provisioning plugin %s: %w", name, err)
	}
	defer conn.Release()

	// CREATE ROLE has no IF NOT EXISTS, so the existence check is a query and
	// the create is conditional on it. The race — two servers provisioning the
	// same plugin at once — resolves by one of them failing with a duplicate
	// and retrying into the ALTER path, which is why this is not in a
	// transaction that would have to be rolled back whole.
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		bare).Scan(&exists); err != nil {
		return fmt.Errorf("db: provisioning plugin %s: %w", name, err)
	}

	// The password is a literal because PostgreSQL will not take a parameter in
	// CREATE ROLE. quoteLiteral is what makes that safe, and the callers of this
	// function generate the password from crypto/rand rather than accepting one,
	// so there is no path by which an operator's typing reaches here.
	stmts := []string{}
	if !exists {
		stmts = append(stmts, `CREATE ROLE `+role+` LOGIN PASSWORD `+quoteLiteral(password))
	} else {
		stmts = append(stmts, `ALTER ROLE `+role+` WITH LOGIN PASSWORD `+quoteLiteral(password))
	}
	stmts = append(stmts,
		// Required on PostgreSQL 15: a CREATEROLE role is not automatically a
		// member of the roles it creates, and CREATE SCHEMA ... AUTHORIZATION
		// is refused without membership. PostgreSQL 16 grants it on creation
		// and this is then a statement that changes nothing.
		`GRANT `+role+` TO CURRENT_USER`,
		`CREATE SCHEMA IF NOT EXISTS `+role+` AUTHORIZATION `+role,
		// What it may read of core's data: the views, never the tables.
		`GRANT USAGE ON SCHEMA core_read TO `+role,
		`GRANT SELECT ON ALL TABLES IN SCHEMA core_read TO `+role,
		// A view added by a later migration would otherwise be invisible to
		// every role provisioned before it. This makes the grant apply to what
		// core publishes next as well as what it publishes now.
		`ALTER DEFAULT PRIVILEGES IN SCHEMA core_read GRANT SELECT ON TABLES TO `+role,
		// Its own schema first, then the views, so a plugin writes SQL that
		// reads the way core's own does. The public schema is deliberately not
		// on the path: a plugin has no business resolving a core table by
		// accident, and being refused by name is clearer than being refused by
		// permission.
		`ALTER ROLE `+role+` SET search_path = `+role+`, core_read`,
		// It may not create databases, roles, or anything outside its schema.
		// None of these are granted by default; revoking is belt and braces
		// against a template database that granted them.
		`REVOKE ALL ON SCHEMA public FROM `+role,
	)

	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("db: provisioning plugin %s: %w", name, redactPassword(err, password))
		}
	}
	return nil
}

// DropPluginDatabase removes a plugin's schema, its tables, every grant it holds
// and the role itself.
//
// It is two statements and not one, and the order matters. DROP SCHEMA CASCADE
// alone leaves the role holding USAGE on core_read and SELECT on its views, and
// a role that still holds a grant cannot be dropped — PostgreSQL refuses with
// "cannot be dropped because some objects depend on it". DROP OWNED BY revokes
// what it was granted and drops what it owns in this database, which is the
// schema and everything in it.
//
// It tolerates a role that is not there, because uninstalling a plugin that was
// never provisioned must not be an error.
func (s *Store) DropPluginDatabase(ctx context.Context, name string) error {
	role, err := s.pluginRole(name)
	if err != nil {
		return err
	}
	bare, err := roleName(name, s.pool.Config().ConnConfig.Database)
	if err != nil {
		return err
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: dropping the database of plugin %s: %w", name, err)
	}
	defer conn.Release()

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		bare).Scan(&exists); err != nil {
		return fmt.Errorf("db: dropping the database of plugin %s: %w", name, err)
	}
	if !exists {
		// Still worth dropping the schema: a provisioning that failed between
		// creating the schema and the role would leave one without the other.
		if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+role+` CASCADE`); err != nil {
			return fmt.Errorf("db: dropping the schema of plugin %s: %w", name, err)
		}
		return nil
	}

	for _, stmt := range []string{
		`DROP OWNED BY ` + role + ` CASCADE`,
		`DROP ROLE ` + role,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("db: dropping the database of plugin %s: %w", name, err)
		}
	}
	return nil
}

// PluginDSN is the connection string a plugin is given.
//
// It is built from this server's own connection rather than by rewriting a
// string, so that it is right whichever spelling the operator configured — a URL
// or the keyword/value form — and so that a change to how the server connects,
// to TLS in particular, reaches plugins without anybody remembering to make it.
//
// Only the credentials differ. Same host, same port, same database: a plugin
// joins its tables to the core_read views in one query, which is only possible
// because it is the same database.
func (s *Store) PluginDSN(name, password string) (string, error) {
	cfg := s.pool.Config().ConnConfig
	role, err := roleName(name, cfg.Database)
	if err != nil {
		return "", err
	}

	host := cfg.Host
	// A Unix socket directory is a path, and a path is not a URL host. libpq
	// spells it as the host query parameter instead.
	socket := host != "" && host[0] == '/'

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(role, password),
		Path:   "/" + cfg.Database,
	}
	q := url.Values{}
	if socket {
		q.Set("host", host)
	} else {
		u.Host = net.JoinHostPort(host, strconv.Itoa(int(cfg.Port)))
	}
	if mode := sslModeOf(cfg); mode != "" {
		q.Set("sslmode", mode)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// sslModeOf recovers the sslmode a connection was made with. pgx resolves the
// setting into a TLS configuration and does not keep the word, so this is the
// nearest honest answer. The verify- modes are not distinguishable here and are
// not reproduced; a plugin connecting to the same server over the same network
// with require is not weaker than the server's own connection in any way that
// matters, because the server's certificate policy is the server's.
//
// prefer has to be told apart from require, and a TLS configuration is not what
// tells them apart — prefer has one too. It is the mode that offers TLS and
// accepts a server that refuses it, and it is the only one pgx leaves a
// fallback with no TLS on. It is also the default when a connection string says
// nothing about SSL, which is the case that made this worth getting right: a
// PostgreSQL with ssl off and a connection string that never mentioned it runs
// the server perfectly well, and answering "require" here would leave every
// plugin unable to reach the database the server is already using.
func sslModeOf(cfg *pgx.ConnConfig) string {
	if cfg.TLSConfig == nil {
		return "disable"
	}
	for _, f := range cfg.Fallbacks {
		if f.TLSConfig == nil {
			return "prefer"
		}
	}
	return "require"
}

// quoteLiteral is PostgreSQL's quote_literal, done here because the value goes
// into a statement that cannot take parameters. Doubling the quote is the whole
// rule; the E-prefix and doubled backslash cover a server with
// standard_conforming_strings off, which is not the default and is not worth
// being wrong about.
func quoteLiteral(s string) string {
	out := make([]byte, 0, len(s)+8)
	out = append(out, 'E', '\'')
	for i := range len(s) {
		switch s[i] {
		case '\'':
			out = append(out, '\'', '\'')
		case '\\':
			out = append(out, '\\', '\\')
		default:
			out = append(out, s[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}

// redactPassword keeps a generated password out of an error that quotes the
// statement it came from. PostgreSQL does not usually echo the statement, but
// "usually" is not a property to rely on for a credential.
func redactPassword(err error, password string) error {
	if err == nil || password == "" {
		return err
	}
	msg := err.Error()
	replaced := strings.ReplaceAll(msg, password, "[redacted]")
	if replaced == msg {
		return err
	}
	return errors.New(replaced)
}
