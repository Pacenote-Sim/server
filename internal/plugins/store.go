package plugins

import (
	"context"
	"time"

	"github.com/pacenote-sim/server/internal/db"
)

// State is what a plugin is doing. The spellings are the ones the plugins table
// accepts, in one place so the constant and the check constraint cannot drift.
type State string

// The states a plugin can be in.
const (
	// StateDiscovered is a plugin found on disk and not yet started.
	StateDiscovered State = "discovered"
	// StateStarting is a process started and not yet answering.
	StateStarting State = "starting"
	// StateRunning is a plugin that answered, which is the only state it can
	// be called in.
	StateRunning State = "running"
	// StateStopped is a plugin the host stopped: a shutdown, or files that are
	// no longer there.
	StateStopped State = "stopped"
	// StateDisabled is a plugin the operator turned off. It is separate from
	// stopped because it is a choice rather than a consequence, and because a
	// restart must not undo it.
	StateDisabled State = "disabled"
	// StateFailed is a plugin that will not be started again: a version
	// mismatch, a manifest that does not parse, or one crash too many. The
	// panel shows it with its last error and its last output, which is the
	// only thing an operator can act on.
	StateFailed State = "failed"
)

// Store is what the host needs from the database. It is an interface rather
// than the concrete store so that the host's failure paths — a plugin that
// crashes, one that hangs, one built against the wrong version — can be tested
// on a machine with no PostgreSQL on it. [*db.Store] implements it.
type Store interface {
	// SavePlugin records a plugin found on disk.
	SavePlugin(ctx context.Context, p db.PluginRecord) error
	// SavePluginDeclaredSettings records what it said it needs configured.
	SavePluginDeclaredSettings(ctx context.Context, name string, declared []byte) error
	// SavePluginState records what it is doing and why.
	SavePluginState(ctx context.Context, name, state, lastError, lastOutput string, restarts int) error
	// Plugins is every plugin this server has seen.
	Plugins(ctx context.Context) ([]db.PluginRecord, error)
	// Plugin is one of them, by name.
	Plugin(ctx context.Context, name string) (db.PluginRecord, error)
	// SavePluginEnabled records whether the operator wants it running. It is
	// separate from the state: disabled is a choice and stopped is a
	// consequence, and a plugin the operator turned off must stay off across a
	// restart.
	SavePluginEnabled(ctx context.Context, name string, enabled bool) error
	// PluginSettings is what the operator configured for one.
	PluginSettings(ctx context.Context, name string) ([]db.PluginSettingRow, error)
	// RecordPluginUsage stores what a call cost.
	RecordPluginUsage(ctx context.Context, u db.PluginUsageWrite) error
	// PluginTokensSince is what every plugin has spent since an instant, for
	// the whole-server figures.
	PluginTokensSince(ctx context.Context, since time.Time) (db.TokenUse, error)
	// PluginTokensSinceFor is the same for one plugin, which is what its own
	// daily cap is measured against.
	PluginTokensSinceFor(ctx context.Context, name string, since time.Time) (db.TokenUse, error)
	// SavePluginSetting stores one plain value. The host writes the settings a
	// plugin does not own — its daily cap — through this as well.
	SavePluginSetting(ctx context.Context, plugin, name, value string) error

	// The rest is a plugin's own database: a PostgreSQL role and a schema it
	// owns. They are on this interface rather than an optional one because a
	// host that cannot make a plugin a database should fail to compile rather
	// than fail at install, on a machine, in a year.

	// PluginDatabase is what is known about a plugin's database: the sealed
	// password for its role, whether it exists, and how far it is migrated.
	PluginDatabase(ctx context.Context, name string) (db.PluginDatabaseRow, error)
	// SetPluginProvisioned records that the role and schema exist and stores
	// the sealed password that reaches them.
	SetPluginProvisioned(ctx context.Context, name string, sealed []byte, migration string) error
	// DeletePlugin forgets a plugin entirely. It is the last step of removing
	// one, after its process is stopped and its database dropped.
	DeletePlugin(ctx context.Context, name string) error
	// ClearPluginDatabase forgets a password whose role has been dropped.
	ClearPluginDatabase(ctx context.Context, name string) error
	// ProvisionPluginDatabase creates the role, the schema and the grants, or
	// brings an existing one back to that state.
	ProvisionPluginDatabase(ctx context.Context, name, password string) error
	// DropPluginDatabase removes the role, the schema and everything in it.
	DropPluginDatabase(ctx context.Context, name string) error
	// PluginDSN is the connection string for a plugin's role.
	PluginDSN(name, password string) (string, error)
}
