// Package logging builds the server's one logger: slog, JSON, to stdout, with a
// redaction layer wrapped around it.
//
// The redaction layer is the point of the package. A secret reaches a log line
// by accident, never on purpose — a connection string inside a pgx error, an
// Authorization header swept up by a request dump, an API key logged while
// someone was debugging at two in the morning. So the handler treats redaction
// as its job rather than the caller's: it rewrites attribute values whose key
// names a secret, and it scrubs message and string values for the shapes a
// secret has. A caller cannot opt out, and forgetting to think about it is the
// safe default.
//
// What it cannot do is read minds. A password logged under the key "detail"
// with no recognisable shape goes through. The rule for callers stays: log the
// name of a thing, not the thing.
package logging

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces every value this package removes. It is a fixed string, so
// a log search for it finds every place a secret was on its way out.
const Redacted = "[redacted]"

// sensitiveKeys are the attribute keys whose value is always removed. Matching
// is case-insensitive and on the whole key, with the substring list below
// catching the compound spellings.
var sensitiveKeys = map[string]struct{}{
	"authorization": {}, "cookie": {}, "set-cookie": {}, "credential": {},
	"credentials": {}, "dsn": {}, "key": {}, "passphrase": {}, "password": {},
	"pwd": {}, "secret": {}, "session": {}, "token": {}, "url": {},
}

// sensitiveParts are substrings that make a key sensitive wherever they appear,
// so "database_url", "anthropic_api_key" and "token_prefix" are all covered
// without listing every compound by hand.
var sensitiveParts = []string{
	"api_key", "apikey", "authorization", "connstring", "conn_string",
	"connection_string", "cookie", "credential", "database_url", "db_url",
	"passwd", "password", "private_key", "secret", "session_id", "setup_token",
	"token",
}

// dsnRE matches a URL with a userinfo component, which is how a PostgreSQL
// connection string carries its password. The password is replaced rather than
// the whole string, because "postgres://pacenote@db:5432/pacenote" is useful in a
// log line and the password is the only part that is not.
var dsnRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/\s:@]+):([^/\s@]*)@`)

// kvSecretRE matches a libpq keyword/value connection string's password field,
// the other spelling of the same secret.
var kvSecretRE = regexp.MustCompile(`(?i)\b(password|passfile)\s*=\s*('[^']*'|\S+)`)

// bearerRE matches a bearer credential wherever it is embedded in prose.
var bearerRE = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)

// Options configures [New]. The zero value is valid: info level to stdout.
type Options struct {
	// Level is the minimum level that reaches the output. The zero value is
	// [slog.LevelInfo].
	Level slog.Level
	// Output is where lines are written. nil means os.Stdout.
	Output io.Writer
	// AddSource attaches the file and line of the call site. Off by default:
	// it costs a stack walk per line.
	AddSource bool
}

// New builds the server's logger. Every line is JSON on one line, which is what
// a log shipper wants and what `jq` reads, and every line has passed through
// the redactor.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = stdout()
	}
	h := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:     opts.Level,
		AddSource: opts.AddSource,
	})
	return slog.New(&redactor{inner: h})
}

// Discard is a logger that writes nothing. Tests that do not assert on log
// output use it so a test run stays readable.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// redactor wraps a handler and rewrites every record on its way through.
type redactor struct{ inner slog.Handler }

// Enabled reports whether the wrapped handler wants records at this level.
func (r *redactor) Enabled(ctx context.Context, l slog.Level) bool {
	return r.inner.Enabled(ctx, l)
}

// Handle redacts the record and passes it on.
func (r *redactor) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, Scrub(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return r.inner.Handle(ctx, out)
}

// WithAttrs redacts the attributes before they are bound, so a secret attached
// once to a child logger is removed once rather than on every line.
func (r *redactor) WithAttrs(as []slog.Attr) slog.Handler {
	cleaned := make([]slog.Attr, len(as))
	for i, a := range as {
		cleaned[i] = redactAttr(a)
	}
	return &redactor{inner: r.inner.WithAttrs(cleaned)}
}

// WithGroup opens a group on the wrapped handler.
func (r *redactor) WithGroup(name string) slog.Handler {
	return &redactor{inner: r.inner.WithGroup(name)}
}

// redactAttr removes an attribute's value when its key names a secret, and
// otherwise scrubs the value for the shapes a secret has. Groups are walked, so
// a secret nested three groups deep is found.
func redactAttr(a slog.Attr) slog.Attr {
	if Sensitive(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		in := v.Group()
		out := make([]slog.Attr, len(in))
		for i, g := range in {
			out[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if v.Kind() == slog.KindString {
		return slog.String(a.Key, Scrub(v.String()))
	}
	if v.Kind() == slog.KindAny {
		if err, ok := v.Any().(error); ok && err != nil {
			return slog.String(a.Key, Scrub(err.Error()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// Sensitive reports whether an attribute key names something that must never be
// logged. It is exported because the admin panel's "copy diagnostics" button
// needs the same rule.
func Sensitive(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if _, ok := sensitiveKeys[k]; ok {
		return true
	}
	for _, part := range sensitiveParts {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}

// Scrub removes the secrets it can recognise from free text: the password in a
// connection string, in either spelling, and a bearer credential in prose. The
// rest of the string survives, because a log line that says which host was
// unreachable is worth keeping.
func Scrub(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "://") {
		s = dsnRE.ReplaceAllString(s, "$1$2:"+Redacted+"@")
	}
	if strings.Contains(s, "=") {
		s = kvSecretRE.ReplaceAllString(s, "$1="+Redacted)
	}
	s = bearerRE.ReplaceAllString(s, "$1 "+Redacted)
	return s
}
