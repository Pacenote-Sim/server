package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/logging"
)

// planted is the value every test in this file looks for in the output. If it
// ever appears, the redactor let a secret through.
const planted = "s3cr3t-planted-value"

func logLine(t *testing.T, f func(*slog.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	f(logging.New(logging.Options{Level: slog.LevelDebug, Output: &buf}))
	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out), "log line is not JSON: %s", buf.String())
	return out
}

func TestSensitive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"authorization header", "Authorization", true},
		{"token", "token", true},
		{"device token", "device_token", true},
		{"token prefix", "token_prefix", true},
		{"setup token", "setup_token", true},
		{"password", "PASSWORD", true},
		{"api key", "anthropic_api_key", true},
		{"database url", "database_url", true},
		{"connection string", "connection_string", true},
		{"cookie", "Set-Cookie", true},
		{"secret", "secret_key", true},
		{"session id", "session_id", true},
		{"bare key", "key", true},
		{"path is not a secret", "path", false},
		{"status is not a secret", "status", false},
		{"organisation is not a secret", "organisation", false},
		{"remote is not a secret", "remote", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, logging.Sensitive(tc.key))
		})
	}
}

func TestScrub(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		in          string
		wantAbsent  string
		wantPresent []string
	}{
		{
			name:        "password in a postgres url",
			in:          "failed to connect to postgres://pacenote:" + planted + "@db.example.com:5432/pacenote",
			wantAbsent:  planted,
			wantPresent: []string{"db.example.com:5432", "pacenote", logging.Redacted},
		},
		{
			name:        "password in a keyword value connection string",
			in:          "host=db.example.com user=pacenote password=" + planted + " dbname=pacenote",
			wantAbsent:  planted,
			wantPresent: []string{"host=db.example.com", "user=pacenote", logging.Redacted},
		},
		{
			name:        "quoted password in a keyword value connection string",
			in:          "host=db user=pacenote password='" + planted + "'",
			wantAbsent:  planted,
			wantPresent: []string{"user=pacenote"},
		},
		{
			name:        "bearer token in prose",
			in:          "rejected header Bearer " + planted,
			wantAbsent:  planted,
			wantPresent: []string{"Bearer", logging.Redacted},
		},
		{
			name:        "an ordinary sentence is left alone",
			in:          "nothing answered at db.example.com:5432",
			wantAbsent:  logging.Redacted,
			wantPresent: []string{"nothing answered at db.example.com:5432"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			got := logging.Scrub(tc.in)
			r.NotContains(got, tc.wantAbsent)
			for _, want := range tc.wantPresent {
				r.Contains(got, want)
			}
		})
	}
}

func TestHandlerRedacts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		emit func(*slog.Logger)
		at   string
	}{
		{
			name: "an attribute whose key names a secret",
			emit: func(l *slog.Logger) { l.Info("paired", slog.String("token", planted)) },
			at:   "token",
		},
		{
			name: "an attribute bound to a child logger",
			emit: func(l *slog.Logger) { l.With(slog.String("api_key", planted)).Info("called") },
			at:   "api_key",
		},
		{
			name: "a secret nested inside a group",
			emit: func(l *slog.Logger) {
				l.Info("request", slog.Group("headers", slog.String("Authorization", planted)))
			},
			at: "headers",
		},
		{
			name: "a connection string in an ordinary attribute",
			emit: func(l *slog.Logger) {
				l.Info("connect failed", slog.String("detail", "postgres://u:"+planted+"@h:5432/d"))
			},
			at: "detail",
		},
		{
			name: "a connection string inside an error value",
			emit: func(l *slog.Logger) {
				l.Info("connect failed", slog.Any("error", errors.New("dial postgres://u:"+planted+"@h:5432/d")))
			},
			at: "error",
		},
		{
			name: "a secret in the message itself",
			emit: func(l *slog.Logger) { l.Info("using password=" + planted) },
			at:   "msg",
		},
		{
			name: "a group opened with WithGroup",
			emit: func(l *slog.Logger) {
				l.WithGroup("db").Info("opened", slog.String("dsn", planted))
			},
			at: "db",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			var buf bytes.Buffer
			tc.emit(logging.New(logging.Options{Level: slog.LevelDebug, Output: &buf}))
			r.NotContains(buf.String(), planted, "the planted secret reached the log")
			r.Contains(buf.String(), logging.Redacted)
			var out map[string]any
			r.NoError(json.Unmarshal(buf.Bytes(), &out))
			r.Contains(out, tc.at)
		})
	}
}

func TestHandlerKeepsWhatIsNotSecret(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	out := logLine(t, func(l *slog.Logger) {
		l.Info("request",
			slog.String("method", "GET"),
			slog.String("path", "/admin/overview"),
			slog.Int("status", 200))
	})
	r.Equal("request", out["msg"])
	r.Equal("GET", out["method"])
	r.Equal("/admin/overview", out["path"])
	r.InEpsilon(200.0, out["status"], 0.0001)
}

func TestLevelIsHonoured(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	var buf bytes.Buffer
	l := logging.New(logging.Options{Level: slog.LevelWarn, Output: &buf})
	l.Info("not this one")
	l.Warn("this one")
	r.NotContains(buf.String(), "not this one")
	r.Contains(buf.String(), "this one")
	r.Equal(1, strings.Count(strings.TrimSpace(buf.String()), "\n")+1)
}

func TestDiscardWritesNothing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	l := logging.Discard()
	r.NotPanics(func() { l.Info("nothing", slog.String("token", planted)) })
}
