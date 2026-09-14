package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// What an operator is told when a database will not have them.
//
// This is the first thing anybody sees of this software: the wizard asks for a
// connection string and this is the answer when it does not work. Every case
// below is a different thing to go and do — start the server, create the
// database, fix the password, raise max_connections — and a single "connection
// failed" would leave all four of them to guess between.
//
// None of these messages may carry the password, which is why [scrubbed] exists
// and why it is checked here rather than trusted.

func TestWhatAFailedConnectionTellsTheOperator(t *testing.T) {
	t.Parallel()

	cfg, err := pgx.ParseConfig("postgres://ana:hunter2@db.example.com:5432/pacenote")
	require.NoError(t, err)

	cases := []struct {
		name   string
		err    error
		reason Reason
		want   string
	}{
		{
			name: "the wrong password", err: &pgconn.PgError{Code: "28P01"},
			reason: ReasonCredentials, want: "refused the user name or password",
		},
		{
			name: "no such database", err: &pgconn.PgError{Code: "3D000"},
			reason: ReasonDatabaseMissing, want: "has no database called",
		},
		{
			name: "a user who may not do that", err: &pgconn.PgError{Code: "42501"},
			reason: ReasonPermission, want: "not allowed to do that",
		},
		{
			name: "a server with no room left", err: &pgconn.PgError{Code: "53300"},
			reason: ReasonBusy, want: "no connection slots left",
		},
		{
			name:   "something PostgreSQL said and this package has not met",
			err:    &pgconn.PgError{Code: "XX000", Message: "the planner gave up."},
			reason: ReasonUnknown, want: "the planner gave up",
		},
		{
			name: "a connection that ran out of time", err: context.DeadlineExceeded,
			reason: ReasonUnreachable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			var connErr *ConnectError
			r.ErrorAs(classify(tc.err, cfg), &connErr)
			r.Equal(tc.reason, connErr.Reason)
			if tc.want != "" {
				r.Contains(connErr.Message, tc.want)
			}
			// The user name is quoted into the sentence; the password is in the
			// same connection string and must not be anywhere near it.
			r.NotContains(connErr.Message, "hunter2")
			r.Contains(connErr.Error(), string(tc.reason))
			r.ErrorIs(connErr, tc.err)
		})
	}
}

// A ConnectError with nothing underneath it still renders, and unwraps to
// nothing rather than to a nil that a caller would dereference.
func TestAConnectErrorWithNothingUnderIt(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	e := &ConnectError{Reason: ReasonUnknown, Message: "Something went wrong."}
	r.Equal("unknown: Something went wrong.", e.Error())
	r.NoError(e.Unwrap())
}

// A connection string is never logged whole. The password sits between the
// scheme and the host, and it is the one part of the string that must not
// appear in a message an operator might paste into a bug report.
func TestAPasswordIsTakenOutOfAnythingShown(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("dial postgres://[redacted]@db.example.com:5432: refused",
		scrubbed(errors.New("dial postgres://ana:hunter2@db.example.com:5432: refused")))

	// Nothing that looks like a connection string is left alone.
	r.Equal("the disk is full", scrubbed(errors.New("the disk is full")))
	r.Equal("postgres://db.example.com/pacenote is unreachable",
		scrubbed(errors.New("postgres://db.example.com/pacenote is unreachable")))
}

// The PostgreSQL version, as a person says it. Ten and later are one number;
// everything before that is two, and this server refuses those anyway — the
// case is here so the refusal names the version correctly.
func TestTheVersionAsAPersonSaysIt(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("15", humanVersion(150004))
	r.Equal("17", humanVersion(170001))
	r.Equal("9.6", humanVersion(90600))
	r.Equal("9.4", humanVersion(90405))
}

// The confirmation page's one-line description of a database, which is shown
// where the password must not be.
func TestDescribingADatabaseForTheConfirmationPage(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	got, err := Describe("postgres://ana:hunter2@db.example.com:5432/pacenote")
	r.NoError(err)
	r.Equal("pacenote on db.example.com:5432, as ana", got)
	r.NotContains(got, "hunter2")

	// A connection string that names no database still describes something.
	got, err = Describe("postgres://ana@db.example.com:5432/")
	r.NoError(err)
	r.Contains(got, "the default database")

	_, err = Describe("this is not a connection string")
	r.Error(err)
	r.Equal(string(ReasonBadConnectionString), ReasonOf(err))
}
