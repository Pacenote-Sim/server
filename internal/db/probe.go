package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// exampleDSN is the shape of a connection string, shown to an operator who has
// typed something that is not one. It is an example and not a credential; the
// words in it are the literal words "user" and "password".
const exampleDSN = "postgres://user:password@host:5432/dbname" //nolint:gosec // G101: an example in an error message, not a credential.

// MinServerVersionNum is the oldest PostgreSQL this server runs against, in the
// server_version_num spelling: 150000 is PostgreSQL 15. It is stated in the
// wizard and checked before the operator is allowed to continue (D-4).
//
// The floor is 15 rather than 14 for a reason that has nothing to do with SQL:
// PostgreSQL 14 leaves support in November 2026, and a server shipped now with a
// floor on a version about to stop receiving security fixes would be telling
// operators to run something nobody is patching.
//
// Two things follow from it that the plugin boundary relies on. PostgreSQL 15
// stopped granting CREATE on the public schema to PUBLIC, so a plugin role
// cannot create tables beside core's; and security_invoker exists, so a view's
// read-as-owner semantics — which is what lets a plugin be granted a view
// without being granted the table behind it — is stated in the migration rather
// than depending on a default.
//
// Not 16. PostgreSQL 15 is supported until November 2027 and is on every managed
// service; 16's stricter CREATEROLE would be welcome but is not worth refusing a
// year of otherwise fine installations.
const MinServerVersionNum = 150000

// MinServerVersion is [MinServerVersionNum] as an operator reads it.
const MinServerVersion = "15"

// Reason says what went wrong with a database, in the terms the setup wizard
// has to explain it in. It exists so that the wizard can name the failure
// rather than showing the driver's error text, and so that a test can assert on
// the diagnosis rather than on a sentence.
type Reason string

// The ways connecting to a database fails.
const (
	// ReasonBadConnectionString means the string is not a connection string at
	// all — nothing was attempted.
	ReasonBadConnectionString Reason = "bad_connection_string"
	// ReasonUnreachable means nothing answered: wrong host, wrong port, a
	// firewall, or PostgreSQL not running.
	ReasonUnreachable Reason = "unreachable"
	// ReasonCredentials means PostgreSQL answered and refused the user name or
	// password.
	ReasonCredentials Reason = "credentials"
	// ReasonDatabaseMissing means the server is there and the credentials are
	// good, but the named database does not exist.
	ReasonDatabaseMissing Reason = "database_missing"
	// ReasonPermission means the user connected but may not do what the server
	// needs — usually create tables.
	ReasonPermission Reason = "permission"
	// ReasonVersionTooOld means the server is older than [MinServerVersion].
	ReasonVersionTooOld Reason = "version_too_old"
	// ReasonBusy means the server refused because it has no connection slots
	// left.
	ReasonBusy Reason = "busy"
	// ReasonUnknown is everything else. The message carries the database's own
	// words, because inventing a diagnosis would be worse.
	ReasonUnknown Reason = "unknown"
)

// ConnectError is a failure to use a database, diagnosed.
//
// Message is a complete sentence written for the operator: it names what failed
// and what to do about it, and it never contains the connection string.
type ConnectError struct {
	Reason  Reason
	Message string
	err     error
}

// Error implements error. The rendering is for a log line; the operator sees
// Message.
func (e *ConnectError) Error() string {
	if e.err == nil {
		return string(e.Reason) + ": " + e.Message
	}
	return string(e.Reason) + ": " + e.Message
}

// Unwrap returns the driver's own error, for a log line that wants it.
func (e *ConnectError) Unwrap() error { return e.err }

// Probe opens a connection to url, checks that the server is new enough and
// that the user may create the schema, and closes again. It is what the setup
// wizard calls before it lets the operator move on, and what the admin panel
// calls to test a changed connection string.
//
// Everything it can diagnose comes back as a [*ConnectError] with a [Reason].
func Probe(ctx context.Context, url string) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return &ConnectError{
			Reason:  ReasonBadConnectionString,
			Message: "That is not a PostgreSQL connection string. It looks like \"" + exampleDSN + "\".",
			err:     err,
		}
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = AcquireTimeout
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return classify(err, cfg)
	}
	defer func() { _ = conn.Close(ctx) }()

	versionNum, canCreate := 0, false
	row := conn.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int,
		        has_database_privilege(current_user, current_database(), 'CREATE')`)
	if err := row.Scan(&versionNum, &canCreate); err != nil {
		return classify(err, cfg)
	}
	if versionNum < MinServerVersionNum {
		return &ConnectError{
			Reason: ReasonVersionTooOld,
			Message: fmt.Sprintf(
				"That server is PostgreSQL %s. This needs %s or newer — upgrade it, or point this at a newer server.",
				humanVersion(versionNum), MinServerVersion),
		}
	}
	if !canCreate {
		return &ConnectError{
			Reason: ReasonPermission,
			Message: fmt.Sprintf(
				"The user %q connected, but it may not create tables in %q. Grant it CREATE on that database, or use a user that owns it.",
				cfg.User, cfg.Database),
		}
	}
	return nil
}

// classify turns a driver error into a [*ConnectError] with the right reason
// and a sentence naming it. cfg is used only for the host, port, user and
// database name — never for the password, which is why the message is built
// here rather than from the driver's text.
func classify(err error, cfg *pgx.ConnConfig) *ConnectError {
	where := "the database server"
	user, database := "the configured user", "the configured database"
	if cfg != nil {
		if cfg.Host != "" {
			where = net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
		}
		if cfg.User != "" {
			user = strconv.Quote(cfg.User)
		}
		if cfg.Database != "" {
			database = strconv.Quote(cfg.Database)
		}
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return &ConnectError{
				Reason:  ReasonCredentials,
				Message: fmt.Sprintf("%s answered, but it refused the user name or password for %s.", where, user),
				err:     err,
			}
		case "3D000":
			return &ConnectError{
				Reason:  ReasonDatabaseMissing,
				Message: fmt.Sprintf("%s is running, but it has no database called %s. Create it first, or point this at one that exists.", where, database),
				err:     err,
			}
		case "42501":
			return &ConnectError{
				Reason:  ReasonPermission,
				Message: fmt.Sprintf("The user %s connected, but it is not allowed to do that on %s.", user, database),
				err:     err,
			}
		case "53300":
			return &ConnectError{
				Reason:  ReasonBusy,
				Message: fmt.Sprintf("%s has no connection slots left. Raise max_connections, or wait for something else to let go.", where),
				err:     err,
			}
		}
		return &ConnectError{
			Reason:  ReasonUnknown,
			Message: fmt.Sprintf("%s refused the connection: %s.", where, strings.TrimSuffix(pgErr.Message, ".")),
			err:     err,
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &ConnectError{
			Reason:  ReasonUnreachable,
			Message: fmt.Sprintf("Nothing answered at %s before the attempt timed out. Check the host name and port, and that PostgreSQL is reachable from this machine.", where),
			err:     err,
		}
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &ConnectError{
			Reason:  ReasonUnreachable,
			Message: fmt.Sprintf("The host name in that connection string does not resolve: %s.", dnsErr.Name),
			err:     err,
		}
	}

	var netErr net.Error
	var opErr *net.OpError
	if errors.As(err, &netErr) || errors.As(err, &opErr) {
		return &ConnectError{
			Reason:  ReasonUnreachable,
			Message: fmt.Sprintf("Nothing answered at %s. Check the host name and port, and that PostgreSQL is accepting connections from this machine.", where),
			err:     err,
		}
	}

	return &ConnectError{
		Reason:  ReasonUnknown,
		Message: fmt.Sprintf("The connection to %s failed: %s.", where, strings.TrimSuffix(scrubbed(err), ".")),
		err:     err,
	}
}

// scrubbed renders an error without whatever a connection string may have left
// in it. The logging package redacts on the way out too; this is the belt to
// that pair of braces, because this text is shown to the operator in a browser
// and never reaches a log handler at all.
func scrubbed(err error) string {
	s := err.Error()
	if i := strings.Index(s, "://"); i >= 0 {
		if j := strings.Index(s[i:], "@"); j >= 0 {
			s = s[:i+3] + "[redacted]" + s[i+j:]
		}
	}
	return s
}

func humanVersion(num int) string {
	if num >= 100000 {
		return strconv.Itoa(num / 10000)
	}
	return fmt.Sprintf("%d.%d", num/10000, (num%10000)/100)
}

// Describe renders a connection string as a place, with no credential in it:
// "pacenote on db.example.com:5432, as the user pacenote". The setup wizard shows
// it on the confirmation page, where the password must not be.
func Describe(url string) (string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return "", &ConnectError{
			Reason:  ReasonBadConnectionString,
			Message: "That is not a PostgreSQL connection string.",
			err:     err,
		}
	}
	database := cfg.Database
	if database == "" {
		database = "the default database"
	}
	return fmt.Sprintf("%s on %s, as %s", database,
		net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), cfg.User), nil
}

// ReasonOf is a label for a failure that is safe to put in a log line: the
// diagnosis and nothing else. An error this package did not produce is
// "unknown", because its text has not been checked for secrets.
func ReasonOf(err error) string {
	var connErr *ConnectError
	if errors.As(err, &connErr) {
		return string(connErr.Reason)
	}
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrSetupAlreadyComplete):
		return "setup_already_complete"
	case errors.Is(err, ErrAdminEmailTaken):
		return "admin_email_taken"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	default:
		return string(ReasonUnknown)
	}
}
