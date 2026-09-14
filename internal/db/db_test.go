package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

func TestMinimumVersionIsStated(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// The wizard prints this, and the probe enforces it. They must be the same
	// number, which is only true if there is one of them.
	r.Equal(150000, db.MinServerVersionNum)
	r.Equal("15", db.MinServerVersion)
}

func TestProbeRejectsRubbishWithoutDialling(t *testing.T) {
	t.Parallel()
	// An empty string is not in this table: libpq's rules make it mean "the
	// local server with the default database", so it is a connection string,
	// just not one anybody typed. The wizard refuses it before it gets here.
	cases := []struct {
		name string
		dsn  string
	}{
		{"a sentence", "this is not a connection string"},
		{"a url with the wrong scheme", "http://db.example.com/pacenote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			err := db.Probe(context.Background(), tc.dsn)
			r.Error(err)
			var connErr *db.ConnectError
			r.ErrorAs(err, &connErr)
			r.Equal(db.ReasonBadConnectionString, connErr.Reason)
			r.Contains(connErr.Message, "postgres://")
		})
	}
}

func TestDescribeLeavesThePasswordOut(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dsn     string
		want    []string
		wantErr bool
	}{
		{
			name: "a url",
			dsn:  "postgres://pacenote:hunter2@db.example.com:5432/pacenote",
			want: []string{"pacenote", "db.example.com:5432"},
		},
		{
			name: "the keyword value spelling",
			dsn:  "host=db.example.com port=5433 user=pacenote password=hunter2 dbname=laps",
			want: []string{"laps", "db.example.com:5433", "pacenote"},
		},
		{"not a connection string", "!!!", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			got, err := db.Describe(tc.dsn)
			if tc.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			for _, want := range tc.want {
				r.Contains(got, want)
			}
			r.NotContains(got, "hunter2", "the confirmation page must not carry the password")
		})
	}
}

func TestReasonOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nothing went wrong", nil, "none"},
		{"setup already complete", db.ErrSetupAlreadyComplete, "setup_already_complete"},
		{"the address is taken", db.ErrAdminEmailTaken, "admin_email_taken"},
		{"no such row", db.ErrNotFound, "not_found"},
		{"wrapped", errors.Join(errors.New("context"), db.ErrNotFound), "not_found"},
		{"something else", errors.New("who knows"), string(db.ReasonUnknown)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, db.ReasonOf(tc.err))
		})
	}
}

func TestMigrationsAreEmbedded(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	sources, err := db.MigrationSources()
	r.NoError(err)
	r.NotEmpty(sources, "the binary carries its own schema; there is no directory to copy")
	for _, s := range sources {
		r.True(strings.HasSuffix(s, ".sql"))
	}
}

func TestMigrationStateUpToDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		state db.MigrationState
		want  bool
	}{
		{"nothing pending", db.MigrationState{Version: 1, Target: 1, Applied: 1}, true},
		{"one pending", db.MigrationState{Version: 1, Target: 2, Applied: 1, Pending: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, tc.state.UpToDate())
		})
	}
}

func TestDeviceRevoked(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	r.False(db.Device{}.Revoked())
}
