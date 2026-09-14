//go:build postgres

// Package dbtest gives a test a PostgreSQL database of its own.
//
// It is behind the "postgres" build tag on purpose. Docker is not available
// everywhere — this was written on a machine where it is not — so the rule is
// that `go test ./...` passes with no database at all, and the tests that need
// one are compiled in deliberately:
//
//	go test -tags postgres ./...
//
// with PACENOTE_TEST_DATABASE_URL pointing at a server the test user may create
// databases on. Each test gets a fresh, empty database and drops it afterwards,
// so tests are independent and can run in parallel.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvURL names the variable that points at a PostgreSQL server the tests may
// create databases on. It must be a URL, not the keyword/value spelling.
const EnvURL = "PACENOTE_TEST_DATABASE_URL"

// Timeout bounds every administrative statement this package runs.
const Timeout = 30 * time.Second

// URL creates an empty database and returns a connection string for it. The
// database is dropped when the test finishes.
//
// A missing [EnvURL] skips the test rather than failing it: the build tag says
// "these may run here", and the variable says "here is where".
func URL(tb testing.TB) string {
	tb.Helper()
	admin := adminURL(tb)

	name := "pacenote_test_" + randomSuffix(tb)
	exec(tb, admin, `CREATE DATABASE "`+name+`"`)
	tb.Cleanup(func() {
		// Drop with FORCE so a connection the test forgot to close does not
		// leave a database behind on every run.
		exec(tb, admin, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	u, err := url.Parse(admin)
	if err != nil {
		tb.Fatalf("%s is not a URL: %v", EnvURL, err)
	}
	u.Path = "/" + name
	return u.String()
}

// AdminURL is the server the tests create databases on, for the cases that need
// to talk to it directly — testing what happens when a database is missing, for
// instance.
func AdminURL(tb testing.TB) string {
	tb.Helper()
	return adminURL(tb)
}

func adminURL(tb testing.TB) string {
	tb.Helper()
	v := lookupEnv(EnvURL)
	if v == "" {
		tb.Skipf("%s is not set, so the database tests have nothing to run against", EnvURL)
	}
	return v
}

func exec(tb testing.TB, dsn, sql string) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		tb.Fatalf("cannot reach the test database server: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		tb.Fatalf("%s: %v", sql, err)
	}
}

func randomSuffix(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("no randomness available: %v", err)
	}
	return hex.EncodeToString(b)
}
