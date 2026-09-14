// Package db is the only package that speaks PostgreSQL.
//
// One engine, supported properly. There is no store interface
// here pretending that two database engines are interchangeable, because that
// abstraction costs more than it saves and hides the differences that matter —
// COPY for bulk lap ingest, partial indexes for the reference lookup, binary
// parameters for a compressed trace blob. pgx speaks the native protocol, sqlc
// turns the plain SQL in queries/ into typed methods, and goose applies the
// migrations in migrations/ at startup from inside the binary, so there is no
// migration command for an operator to remember.
//
// The dependency rule is enforced by the linter: nothing outside this package
// may import pgx, goose or database/sql. A caller that needs a new query adds
// SQL to queries/, regenerates, and calls a typed method.
//
// # Setup is decided here
//
// One row, in setup_state, says whether the wizard has already run. It is
// written in the same transaction as the first administrator, and its primary
// key is a single fixed value so that two concurrent attempts contend and
// exactly one wins. See [Store.CompleteSetup] and [Store.SetupState].
package db
