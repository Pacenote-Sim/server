//go:build postgres

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/db/dbtest"
	"github.com/pacenote-sim/server/internal/logging"
)

func open(t *testing.T) (*db.Store, string) {
	t.Helper()
	url := dbtest.URL(t)
	store, err := db.Open(context.Background(), url, logging.Discard())
	require.NoError(t, err)
	t.Cleanup(store.Close)
	return store, url
}

func migrated(t *testing.T) (*db.Store, string) {
	t.Helper()
	store, url := open(t)
	require.NoError(t, store.Migrate(context.Background()))
	return store, url
}

func settings() config.Settings {
	return config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy)
}

func TestProbeAcceptsAGoodDatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	r.NoError(db.Probe(context.Background(), dbtest.URL(t)))
}

func TestProbeDiagnoses(t *testing.T) {
	t.Parallel()
	base := dbtest.AdminURL(t)

	cases := []struct {
		name string
		url  func(t *testing.T) string
		want db.Reason
	}{
		{
			name: "a database that does not exist",
			url:  func(*testing.T) string { return replaceDatabase(base, "no_such_database_here") },
			want: db.ReasonDatabaseMissing,
		},
		{
			name: "nothing listening",
			url:  func(*testing.T) string { return "postgres://pacenote:secret@127.0.0.1:1/pacenote?connect_timeout=2" },
			want: db.ReasonUnreachable,
		},
		{
			name: "not a connection string",
			url:  func(*testing.T) string { return "nonsense" },
			want: db.ReasonBadConnectionString,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			err := db.Probe(context.Background(), tc.url(t))
			r.Error(err)
			var connErr *db.ConnectError
			r.ErrorAs(err, &connErr)
			r.Equal(tc.want, connErr.Reason)
			r.NotEmpty(connErr.Message)
			r.NotContains(connErr.Message, "secret", "a diagnosis must never quote the password")
			r.Equal(string(tc.want), db.ReasonOf(err))
		})
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := open(t)
	ctx := context.Background()

	installed, err := store.SchemaInstalled(ctx)
	r.NoError(err)
	r.False(installed)

	r.NoError(store.Migrate(ctx))
	r.NoError(store.Migrate(ctx), "running the migrations twice changes nothing")

	installed, err = store.SchemaInstalled(ctx)
	r.NoError(err)
	r.True(installed)

	state, err := store.MigrationState(ctx)
	r.NoError(err)
	r.True(state.UpToDate())
	r.Positive(state.Applied)
	r.Zero(state.Pending)
	r.Equal(state.Target, state.Version)
	r.False(state.LastAppliedAt.IsZero())
}

func TestSetupStateBeforeAnything(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := open(t)
	ctx := context.Background()

	state, err := store.SetupState(ctx)
	r.NoError(err)
	r.False(state.Installed, "an empty database has no schema")
	r.False(state.Complete)

	r.NoError(store.Migrate(ctx))

	state, err = store.SetupState(ctx)
	r.NoError(err)
	r.True(state.Installed)
	r.False(state.Complete, "a migrated database with no marker is still a first run")
}

func TestCompleteSetup(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	at, err := store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        "ana@example.com",
		AdminPasswordHash: "$argon2id$v=19$m=8,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZA",
		Settings:          settings(),
		ServerVersion:     "v0.0.0-test",
	})
	r.NoError(err)
	r.WithinDuration(time.Now(), at, time.Minute)

	state, err := store.SetupState(ctx)
	r.NoError(err)
	r.True(state.Complete)
	r.Equal("ana@example.com", state.CompletedBy)
	r.Equal("v0.0.0-test", state.ServerVersion)

	n, err := store.CountAdmins(ctx)
	r.NoError(err)
	r.Equal(int64(1), n)

	got, err := store.Settings(ctx)
	r.NoError(err)
	r.Equal(settings(), got)
}

func TestCompleteSetupRefusesASecondTime(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	req := db.SetupRequest{
		AdminEmail:        "ana@example.com",
		AdminPasswordHash: "hash",
		Settings:          settings(),
	}
	_, err := store.CompleteSetup(ctx, req)
	r.NoError(err)

	req.AdminEmail = "someone.else@example.com"
	_, err = store.CompleteSetup(ctx, req)
	r.ErrorIs(err, db.ErrSetupAlreadyComplete)

	n, err := store.CountAdmins(ctx)
	r.NoError(err)
	r.Equal(int64(1), n, "the second attempt must not leave an administrator behind")
}

func TestCompleteSetupIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	const attempts = 8
	var g errgroup.Group
	results := make([]error, attempts)
	start := make(chan struct{})
	for i := range attempts {
		g.Go(func() error {
			<-start
			_, err := store.CompleteSetup(ctx, db.SetupRequest{
				AdminEmail:        "admin" + string(rune('a'+i)) + "@example.com",
				AdminPasswordHash: "hash",
				Settings:          settings(),
			})
			results[i] = err
			return nil
		})
	}
	close(start)
	r.NoError(g.Wait())

	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		r.ErrorIs(err, db.ErrSetupAlreadyComplete,
			"the loser of the race is told setup was already done, not given a database error")
	}
	r.Equal(1, winners, "exactly one of %d concurrent attempts may win", attempts)

	n, err := store.CountAdmins(ctx)
	r.NoError(err)
	r.Equal(int64(1), n, "exactly one administrator exists after a concurrent double submit")
}

func TestInterruptedSetupLeavesNoAdministrator(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	// A setup that fails partway: the settings are unwritable because the
	// transaction is rolled back with it. A cancelled context is the closest
	// honest stand-in for the process being killed mid-commit.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := store.CompleteSetup(cancelled, db.SetupRequest{
		AdminEmail:        "ana@example.com",
		AdminPasswordHash: "hash",
		Settings:          settings(),
	})
	r.Error(err)

	state, err := store.SetupState(ctx)
	r.NoError(err)
	r.False(state.Complete, "an interrupted setup leaves the server in setup mode")

	n, err := store.CountAdmins(ctx)
	r.NoError(err)
	r.Zero(n, "an interrupted setup leaves no administrator")
}

func TestSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	want := settings()
	want.TLSMode = config.TLSAuto
	want.Retention.TraceMonths = 6
	want.Limits.TracePoints = 250
	want.Marketplace = true
	r.NoError(store.SaveSettings(ctx, want))

	got, err := store.Settings(ctx)
	r.NoError(err)
	r.Equal(want, got)

	name, err := store.SettingString(ctx, db.SettingOrganisation)
	r.NoError(err)
	r.Equal(want.Organisation, name)

	missing, err := store.SettingString(ctx, "no-such-setting")
	r.NoError(err)
	r.Empty(missing, "a setting that was never written is empty, not an error")
}

func TestAdminsAndSessions(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	_, err := store.CompleteSetup(ctx, db.SetupRequest{
		AdminEmail:        "Ana@Example.com",
		AdminPasswordHash: "the-hash",
		Settings:          settings(),
	})
	r.NoError(err)

	t.Run("email lookup is case-insensitive", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		admin, err := store.AdminByEmail(ctx, "ana@example.com")
		r.NoError(err)
		r.Equal("the-hash", admin.PasswordHash)
		r.Nil(admin.LastLoginAt)

		again, err := store.AdminByID(ctx, admin.ID)
		r.NoError(err)
		r.Equal(admin.ID, again.ID)
	})

	t.Run("an unknown address is not found", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		_, err := store.AdminByEmail(ctx, "nobody@example.com")
		r.ErrorIs(err, db.ErrNotFound)
	})

	t.Run("a session lives and dies", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		admin, err := store.AdminByEmail(ctx, "ana@example.com")
		r.NoError(err)

		sum := []byte("0123456789abcdef0123456789abcdef")
		r.NoError(store.CreateAdminSession(ctx, admin.ID, sum, time.Now().Add(time.Hour), "test"))

		sess, err := store.AdminSessionByToken(ctx, sum)
		r.NoError(err)
		r.Equal(admin.ID, sess.AdminID)
		r.Equal("Ana@Example.com", sess.Email)

		r.NoError(store.DeleteAdminSession(ctx, sum))
		_, err = store.AdminSessionByToken(ctx, sum)
		r.ErrorIs(err, db.ErrNotFound)
	})

	t.Run("an expired session is simply absent", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		admin, err := store.AdminByEmail(ctx, "ana@example.com")
		r.NoError(err)

		sum := []byte("expired-0123456789abcdef01234567")
		r.NoError(store.CreateAdminSession(ctx, admin.ID, sum, time.Now().Add(-time.Minute), "test"))

		_, err = store.AdminSessionByToken(ctx, sum)
		r.ErrorIs(err, db.ErrNotFound, "expiry is enforced by the lookup, not by a sweep")

		n, err := store.DeleteExpiredAdminSessions(ctx)
		r.NoError(err)
		r.GreaterOrEqual(n, int64(1))
	})
}

func TestDeviceTokens(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	driverID, err := store.CreateDriver(ctx, "", "Ana Ruiz", "ana-ruiz", "gt3", "")
	r.NoError(err)

	sum := []byte("0123456789abcdef0123456789abcdef")
	device, err := store.CreateDevice(ctx, driverID, sum, "abcdefgh", "Ana's desktop")
	r.NoError(err)
	r.Equal("abcdefgh", device.TokenPrefix)

	t.Run("the right token finds the device", func(t *testing.T) {
		t.Parallel()
		req := require.New(t)
		got, lookupErr := store.DeviceByToken(ctx, "abcdefgh", sum)
		req.NoError(lookupErr)
		req.Equal(device.ID, got.ID)
		req.False(got.Revoked())
	})

	t.Run("the right prefix with the wrong digest finds nothing", func(t *testing.T) {
		t.Parallel()
		req := require.New(t)
		_, lookupErr := store.DeviceByToken(ctx, "abcdefgh", []byte("wrong-0123456789abcdef0123456789"))
		req.ErrorIs(lookupErr, db.ErrNotFound, "the prefix is a lookup, never a credential")
	})

	// A second device, so that revoking it cannot disturb the subtests above,
	// which run in parallel against the first.
	otherSum := []byte("fedcba9876543210fedcba9876543210")
	other, err := store.CreateDevice(ctx, driverID, otherSum, "zyxwvuts", "Ana's laptop")
	r.NoError(err)

	t.Run("a revoked device stops working", func(t *testing.T) {
		t.Parallel()
		req := require.New(t)
		revoked, revokeErr := store.RevokeDevice(ctx, other.ID)
		req.NoError(revokeErr)
		req.True(revoked)

		again, revokeErr := store.RevokeDevice(ctx, other.ID)
		req.NoError(revokeErr)
		req.False(again, "revoking twice is not an error")

		_, lookupErr := store.DeviceByToken(ctx, "zyxwvuts", otherSum)
		req.ErrorIs(lookupErr, db.ErrNotFound)

		list, listErr := store.ListDevicesForDriver(ctx, driverID)
		req.NoError(listErr)
		req.Len(list, 2)
		req.True(list[0].Revoked(), "the driver still sees the machine, marked revoked")
	})
}

func TestStats(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	ctx := context.Background()

	stats, err := store.Stats(ctx)
	r.NoError(err)
	r.Positive(stats.DatabaseBytes)
	r.Zero(stats.Drivers)
	r.Zero(stats.Laps)
	r.Zero(stats.TraceBytes)

	_, err = store.CreateDriver(ctx, "", "Ana Ruiz", "ana-ruiz", "gt3", "")
	r.NoError(err)

	stats, err = store.Stats(ctx)
	r.NoError(err)
	r.Equal(int64(1), stats.Drivers)
}

func TestPoolAndPing(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	store, _ := migrated(t)
	r.NoError(store.Ping(context.Background()))
	_, _, total := store.PoolStats()
	r.GreaterOrEqual(total, int32(0))
}

func TestMigrationSourcesAreEmbedded(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	sources, err := db.MigrationSources()
	r.NoError(err)
	r.NotEmpty(sources, "the binary must carry its own migrations")
}

// replaceDatabase swaps the database name in a URL-form connection string.
func replaceDatabase(dsn, name string) string {
	u, err := parseURL(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}
