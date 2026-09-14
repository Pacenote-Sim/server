//go:build postgres

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db"
)

// Every call this package offers, made against a database that has gone away.
//
// It is one test rather than eighty because the thing being checked is a
// property of the package and not of any one call: every method answers with an
// error that says "db:" and none of them panics. That matters more than it
// looks. The whole panel is built on being able to render a page with a
// sentence on it when the database is down, and a single method that panicked
// instead of returning would take the process with it — on the one day an
// operator most needs the server to still be answering.
//
// The store is opened against a real database and then closed, because that is
// what the failure actually looks like: a pool whose connections are gone,
// rather than a pool that was never built.
func TestEveryCallSurvivesADatabaseThatStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store, _ := migrated(t)
	store.Close()

	// Each entry is one call. The name is what a failure prints, which is the
	// only reason this is a table and not a very long function.
	// The calls that hand back the driver's own error rather than wrapping it.
	// Each has a caller that reads the error itself — the probe turns a
	// connection failure into a sentence for the setup wizard, and a
	// transaction's failure is the one the caller inside it returned — so a
	// prefix here would be a prefix something downstream has to take off again.
	raw := map[string]bool{
		"Ping": true, "SchemaInstalled": true, "SetupState": true,
		"CompleteSetup": true, "IssueDeviceForPairing": true, "Idempotent": true,
	}

	calls := []struct {
		name string
		call func() error
	}{
		{"Ping", func() error { return store.Ping(ctx) }},
		{"SchemaInstalled", func() error { _, err := store.SchemaInstalled(ctx); return err }},
		{"MigrationState", func() error { _, err := store.MigrationState(ctx); return err }},
		{"SetupState", func() error { _, err := store.SetupState(ctx); return err }},
		{"CompleteSetup", func() error { _, err := store.CompleteSetup(ctx, db.SetupRequest{}); return err }},

		{"AdminByEmail", func() error { _, err := store.AdminByEmail(ctx, "ana@example.com"); return err }},
		{"AdminByID", func() error { _, err := store.AdminByID(ctx, 1); return err }},
		{"CountAdmins", func() error { _, err := store.CountAdmins(ctx); return err }},
		{"TouchAdminLogin", func() error { return store.TouchAdminLogin(ctx, 1) }},
		{"SetAdminPasswordHash", func() error { return store.SetAdminPasswordHash(ctx, 1, "hash") }},
		{"CreateAdminSession", func() error {
			return store.CreateAdminSession(ctx, 1, []byte("sum"), time.Now(), "a browser")
		}},
		{"AdminSessionByToken", func() error { _, err := store.AdminSessionByToken(ctx, []byte("sum")); return err }},
		{"TouchAdminSession", func() error { return store.TouchAdminSession(ctx, []byte("sum"), time.Now()) }},
		{"DeleteAdminSession", func() error { return store.DeleteAdminSession(ctx, []byte("sum")) }},
		{"DeleteAdminSessionsForAdmin", func() error {
			_, err := store.DeleteAdminSessionsForAdmin(ctx, 1)
			return err
		}},
		{"DeleteExpiredAdminSessions", func() error { _, err := store.DeleteExpiredAdminSessions(ctx); return err }},

		{"Settings", func() error { _, err := store.Settings(ctx); return err }},
		{"SaveSettings", func() error { return store.SaveSettings(ctx, config.Settings{}) }},
		{"SettingString", func() error { _, err := store.SettingString(ctx, "organisation"); return err }},

		{"EnsureDriver", func() error { _, err := store.EnsureDriver(ctx, "Ana Ruiz", "gt3"); return err }},
		{"CreateDriver", func() error { _, err := store.CreateDriver(ctx, "x", "Ana", "ana", "gt3", ""); return err }},
		{"DriverByID", func() error { _, err := store.DriverByID(ctx, 1); return err }},
		{"ListDrivers", func() error { _, err := store.ListDrivers(ctx); return err }},
		{"Roster", func() error { _, err := store.Roster(ctx, db.RosterQuery{}); return err }},
		{"RosterEntryByID", func() error { _, err := store.RosterEntryByID(ctx, 1); return err }},
		{"StorageByDriver", func() error { _, err := store.StorageByDriver(ctx, 10); return err }},

		{"CreateDevice", func() error {
			_, err := store.CreateDevice(ctx, 1, []byte("sum"), "prefix", "a sim rig")
			return err
		}},
		{"DeviceByToken", func() error { _, err := store.DeviceByToken(ctx, "prefix", []byte("sum")); return err }},
		{"DeviceCounts", func() error { _, _, err := store.DeviceCounts(ctx); return err }},
		{"TouchDevice", func() error { return store.TouchDevice(ctx, 1) }},
		{"RevokeDevice", func() error { _, err := store.RevokeDevice(ctx, 1); return err }},
		{"RevokeAllDevices", func() error { _, err := store.RevokeAllDevices(ctx); return err }},
		{"RevokeDevicesForDriver", func() error { _, err := store.RevokeDevicesForDriver(ctx, 1); return err }},
		{"ListDevicesForDriver", func() error { _, err := store.ListDevicesForDriver(ctx, 1); return err }},
		{"CountLiveDevicesForDriver", func() error { _, err := store.CountLiveDevicesForDriver(ctx, 1); return err }},
		{"PanelDevices", func() error { _, err := store.PanelDevices(ctx, db.DeviceQuery{}); return err }},
		{"PanelDeviceByID", func() error { _, err := store.PanelDeviceByID(ctx, 1); return err }},
		{"PanelDevicesForDriver", func() error { _, err := store.PanelDevicesForDriver(ctx, 1, 10); return err }},

		{"CreatePairing", func() error {
			_, err := store.CreatePairing(ctx, []byte("sum"), "5DH-EHY", time.Now())
			return err
		}},
		{"PairingByDeviceCode", func() error { _, err := store.PairingByDeviceCode(ctx, []byte("sum")); return err }},
		{"ListPendingPairings", func() error { _, err := store.ListPendingPairings(ctx); return err }},
		{"DecidePairing", func() error {
			_, err := store.DecidePairing(ctx, 1, wire.StatusApproved, nil, "ana@example.com")
			return err
		}},
		{"ExpirePairing", func() error { return store.ExpirePairing(ctx, 1) }},
		{"IssueDeviceForPairing", func() error {
			_, err := store.IssueDeviceForPairing(ctx, 1, []byte("sum"), "prefix", "a sim rig")
			return err
		}},
		{"DeleteFinishedPairings", func() error { _, err := store.DeleteFinishedPairings(ctx, time.Now()); return err }},

		{"Stint", func() error { _, err := store.Stint(ctx, db.UUID{}, 1); return err }},
		{"StintsForDriver", func() error { _, err := store.StintsForDriver(ctx, db.StintQuery{}); return err }},
		{"PanelStintByID", func() error { _, err := store.PanelStintByID(ctx, db.UUID{}); return err }},
		{"CountLapsForStint", func() error { _, err := store.CountLapsForStint(ctx, db.UUID{}); return err }},
		{"LapsForStint", func() error { _, err := store.LapsForStint(ctx, db.UUID{}, 0, 10); return err }},
		{"ReferenceLap", func() error { _, err := store.ReferenceLap(ctx, db.ReferenceQuery{}); return err }},

		{"Stats", func() error { _, err := store.Stats(ctx); return err }},
		{"DataTotals", func() error { _, err := store.DataTotals(ctx); return err }},
		{"TracesOlderThan", func() error { _, err := store.TracesOlderThan(ctx, time.Now()); return err }},
		{"PruneTracesOlderThan", func() error { _, err := store.PruneTracesOlderThan(ctx, time.Now(), 10); return err }},

		{"WriteAudit", func() error { return store.WriteAudit(ctx, "ana@example.com", "x", "y", nil) }},
		{"RecentAudit", func() error { _, err := store.RecentAudit(ctx, 10); return err }},
		{"AuditFor", func() error { _, err := store.AuditFor(ctx, "x", 10); return err }},

		{"RecordClientBuild", func() error { _, err := store.RecordClientBuild(ctx, db.NewClientBuild{}); return err }},
		{"ClientBuilds", func() error { _, err := store.ClientBuilds(ctx, db.ClientBuildQuery{}); return err }},
		{"ClientBuildByReference", func() error { _, err := store.ClientBuildByReference(ctx, "aabb"); return err }},
		{"CountClientBuilds", func() error { _, err := store.CountClientBuilds(ctx); return err }},

		{"SigningCertificate", func() error { _, err := store.SigningCertificate(ctx); return err }},
		{"SaveSigningCertificate", func() error {
			return store.SaveSigningCertificate(ctx, db.NewSigningCertificate{})
		}},
		{"DeleteSigningCertificate", func() error { return store.DeleteSigningCertificate(ctx) }},

		{"Plugins", func() error { _, err := store.Plugins(ctx); return err }},
		{"Plugin", func() error { _, err := store.Plugin(ctx, "engineer"); return err }},
		{"SavePlugin", func() error { return store.SavePlugin(ctx, db.PluginRecord{Name: "engineer"}) }},
		{"DeletePlugin", func() error { return store.DeletePlugin(ctx, "engineer") }},
		{"SavePluginState", func() error { return store.SavePluginState(ctx, "engineer", "running", "", "", 0) }},
		{"SavePluginEnabled", func() error { return store.SavePluginEnabled(ctx, "engineer", true) }},
		{"PluginSettings", func() error { _, err := store.PluginSettings(ctx, "engineer"); return err }},
		{"SavePluginSetting", func() error { return store.SavePluginSetting(ctx, "engineer", "model", "sonnet") }},
		{"SavePluginSecret", func() error { return store.SavePluginSecret(ctx, "engineer", "api_key", []byte{1}) }},
		{"DeletePluginSetting", func() error { return store.DeletePluginSetting(ctx, "engineer", "model") }},
		{"SavePluginDeclaredSettings", func() error {
			return store.SavePluginDeclaredSettings(ctx, "engineer", []byte("[]"))
		}},
		{"PluginDatabase", func() error { _, err := store.PluginDatabase(ctx, "engineer"); return err }},
		{"SetPluginProvisioned", func() error {
			return store.SetPluginProvisioned(ctx, "engineer", []byte{1}, "0001")
		}},
		{"RecordPluginUsage", func() error { return store.RecordPluginUsage(ctx, db.PluginUsageWrite{}) }},
		{"PluginTokensSince", func() error { _, err := store.PluginTokensSince(ctx, time.Now()); return err }},
		{"PluginTokensSinceFor", func() error {
			_, err := store.PluginTokensSinceFor(ctx, "engineer", time.Now())
			return err
		}},

		{"RecordTokenUsage", func() error { return store.RecordTokenUsage(ctx, "debrief", "sonnet", nil, 1, 1) }},
		{"TokensSince", func() error { _, err := store.TokensSince(ctx, time.Now()); return err }},
		{"DeleteExpiredIdempotencyKeys", func() error {
			_, err := store.DeleteExpiredIdempotencyKeys(ctx, time.Now())
			return err
		}},
		{"Idempotent", func() error {
			_, _, err := store.Idempotent(ctx, db.Write{}, func(context.Context, *db.Tx) (db.Response, error) {
				return db.Response{}, nil
			})
			return err
		}},
	}

	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() { err = c.call() },
				"%s panicked instead of answering", c.name)
			require.Error(t, err, "%s reported success against a database that is gone", c.name)
			if !raw[c.name] {
				require.Contains(t, err.Error(), "db:",
					"%s answered with an error that does not say which layer it came from", c.name)
			}
		})
	}
}
