package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// This package holds no opinion about what a plugin is. It stores rows: what
// was found, what the operator configured, and what each one spent. The
// capability declaration and the settings declaration pass through as the JSON
// the manifest carried, because this package must not depend on the plugin
// interface module — the database is the server's, and the contract is
// somebody else's, and keeping them apart is what stops a change to one from
// needing a migration to the other.

// PluginRecord is an installed plugin as the database holds it.
type PluginRecord struct {
	// Name is the key everywhere: the directory, the settings, the metering.
	Name string
	// Version, Author and Description are what the manifest said, kept so the
	// panel can describe a plugin whose files have since gone.
	Version     string
	Author      string
	Description string
	// InterfaceVersion is the contract version the plugin was built against.
	InterfaceVersion int
	// Capabilities and DeclaredSettings are the JSON documents the manifest
	// and the plugin produced, stored whole.
	Capabilities     []byte
	DeclaredSettings []byte
	// Directory is where it was found, relative to the plugin directory.
	Directory string
	// Enabled is whether the operator wants it running. It is separate from
	// State because disabled is a choice and stopped is a consequence.
	Enabled bool
	// State is what it is doing, as internal/plugins spells it.
	State string
	// LastError is why it is not running and LastOutput the last of what it
	// printed. Both are scrubbed of credentials before they get here.
	LastError  string
	LastOutput string
	// Restarts is how many times it has been restarted since it last ran
	// cleanly, so an operator can see a plugin flapping rather than running.
	Restarts int
	// FirstSeenAt and UpdatedAt are when it appeared and when this row last
	// changed.
	FirstSeenAt time.Time
	UpdatedAt   time.Time
}

// PluginSettingRow is one configured value. Exactly one of Value and Sealed is
// set: a credential is sealed with the data key and a plain setting is not, and
// a value that is both would be a credential anyone with the database can read.
type PluginSettingRow struct {
	Name   string
	Value  string
	Sealed []byte
}

// PluginUsageWrite is what one plugin call cost.
type PluginUsageWrite struct {
	// Plugin is the plugin's name. It is not a foreign key: removing a plugin
	// must not erase what it already spent.
	Plugin string
	// Job is what the call was for, Model what it was spent on.
	Job   string
	Model string
	// DriverID is nil for work that was not on any one driver's behalf.
	DriverID *int64
	// Input and Output are tokens, counted the way the vendor counts them.
	Input, Output int64
	// Cached reports that the plugin answered from its own cache.
	Cached bool
}

// SavePlugin records a plugin that was found on disk, or updates what is known
// about one that was already there. It does not touch the settings declaration,
// which arrives later — a plugin has to start before it can say what it needs.
func (s *Store) SavePlugin(ctx context.Context, p PluginRecord) error {
	err := s.q.UpsertPlugin(ctx, gen.UpsertPluginParams{
		Name:             p.Name,
		Version:          p.Version,
		Author:           p.Author,
		Description:      p.Description,
		InterfaceVersion: int32(p.InterfaceVersion), //nolint:gosec // G115: an interface version is a small positive integer.
		Capabilities:     orJSON(p.Capabilities, "{}"),
		DeclaredSettings: orJSON(p.DeclaredSettings, "[]"),
		Directory:        p.Directory,
		State:            p.State,
	})
	if err != nil {
		return fmt.Errorf("db: cannot record the %q plugin: %w", p.Name, err)
	}
	return nil
}

// SavePluginDeclaredSettings stores what a plugin said it needs configured, so
// that the panel can render the form whether or not the plugin is running. That
// matters most for a plugin that will not start until a credential is filled
// in: without this the operator would have no way to fill it in.
func (s *Store) SavePluginDeclaredSettings(ctx context.Context, name string, declared []byte) error {
	err := s.q.SetPluginSettings(ctx, gen.SetPluginSettingsParams{
		Name:             name,
		DeclaredSettings: orJSON(declared, "[]"),
	})
	if err != nil {
		return fmt.Errorf("db: cannot record what the %q plugin needs configured: %w", name, err)
	}
	return nil
}

// SavePluginState records what a plugin is doing and why.
func (s *Store) SavePluginState(ctx context.Context, name, state, lastError, lastOutput string, restarts int) error {
	err := s.q.SetPluginState(ctx, gen.SetPluginStateParams{
		Name:       name,
		State:      state,
		LastError:  lastError,
		LastOutput: lastOutput,
		Restarts:   int32(restarts), //nolint:gosec // G115: a restart count is bounded by the host's own limit.
	})
	if err != nil {
		return fmt.Errorf("db: cannot record what the %q plugin is doing: %w", name, err)
	}
	return nil
}

// SavePluginEnabled records whether the operator wants it running.
func (s *Store) SavePluginEnabled(ctx context.Context, name string, enabled bool) error {
	if err := s.q.SetPluginEnabled(ctx, gen.SetPluginEnabledParams{Name: name, Enabled: enabled}); err != nil {
		return fmt.Errorf("db: cannot record whether the %q plugin runs: %w", name, err)
	}
	return nil
}

// Plugins is every plugin this server has seen, by name.
func (s *Store) Plugins(ctx context.Context) ([]PluginRecord, error) {
	rows, err := s.q.ListPlugins(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the plugins: %w", err)
	}
	out := make([]PluginRecord, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, PluginRecord{
			Name:             r.Name,
			Version:          r.Version,
			Author:           r.Author,
			Description:      r.Description,
			InterfaceVersion: int(r.InterfaceVersion),
			Capabilities:     r.Capabilities,
			DeclaredSettings: r.DeclaredSettings,
			Directory:        r.Directory,
			Enabled:          r.Enabled,
			State:            r.State,
			LastError:        r.LastError,
			LastOutput:       r.LastOutput,
			Restarts:         int(r.Restarts),
			FirstSeenAt:      r.FirstSeenAt.Time,
			UpdatedAt:        r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// Plugin is one of them. A plugin that was never installed is [ErrNotFound].
func (s *Store) Plugin(ctx context.Context, name string) (PluginRecord, error) {
	r, err := s.q.GetPlugin(ctx, name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PluginRecord{}, ErrNotFound
	case err != nil:
		return PluginRecord{}, fmt.Errorf("db: cannot read the %q plugin: %w", name, err)
	}
	return PluginRecord{
		Name:             r.Name,
		Version:          r.Version,
		Author:           r.Author,
		Description:      r.Description,
		InterfaceVersion: int(r.InterfaceVersion),
		Capabilities:     r.Capabilities,
		DeclaredSettings: r.DeclaredSettings,
		Directory:        r.Directory,
		Enabled:          r.Enabled,
		State:            r.State,
		LastError:        r.LastError,
		LastOutput:       r.LastOutput,
		Restarts:         int(r.Restarts),
		FirstSeenAt:      r.FirstSeenAt.Time,
		UpdatedAt:        r.UpdatedAt.Time,
	}, nil
}

// DeletePlugin forgets a plugin, its settings and the credentials they hold.
// What it spent stays, because the money was the organisation's.
func (s *Store) DeletePlugin(ctx context.Context, name string) error {
	if err := s.q.DeletePlugin(ctx, name); err != nil {
		return fmt.Errorf("db: cannot remove the %q plugin: %w", name, err)
	}
	return nil
}

// PluginSettings is what the operator configured for one plugin.
func (s *Store) PluginSettings(ctx context.Context, name string) ([]PluginSettingRow, error) {
	rows, err := s.q.ListPluginSettings(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the settings of the %q plugin: %w", name, err)
	}
	out := make([]PluginSettingRow, 0, len(rows))
	for _, r := range rows {
		row := PluginSettingRow{Name: r.Name, Sealed: r.Sealed}
		if r.Value != nil {
			row.Value = *r.Value
		}
		out = append(out, row)
	}
	return out, nil
}

// SavePluginSetting stores one plain value. It clears any sealed value under
// the same name, because a setting is one thing or the other.
func (s *Store) SavePluginSetting(ctx context.Context, plugin, name, value string) error {
	err := s.q.UpsertPluginSetting(ctx, gen.UpsertPluginSettingParams{
		PluginName: plugin,
		Name:       name,
		Value:      &value,
	})
	if err != nil {
		return fmt.Errorf("db: cannot store the %q setting of the %q plugin: %w", name, plugin, err)
	}
	return nil
}

// SavePluginSecret stores one sealed credential. The caller seals it: this
// package never sees a plaintext key and never holds the data key.
func (s *Store) SavePluginSecret(ctx context.Context, plugin, name string, sealed []byte) error {
	err := s.q.UpsertPluginSetting(ctx, gen.UpsertPluginSettingParams{
		PluginName: plugin,
		Name:       name,
		Sealed:     sealed,
	})
	if err != nil {
		return fmt.Errorf("db: cannot store the %q credential of the %q plugin: %w", name, plugin, err)
	}
	return nil
}

// DeletePluginSetting removes one value.
func (s *Store) DeletePluginSetting(ctx context.Context, plugin, name string) error {
	err := s.q.DeletePluginSetting(ctx, gen.DeletePluginSettingParams{PluginName: plugin, Name: name})
	if err != nil {
		return fmt.Errorf("db: cannot remove the %q setting of the %q plugin: %w", name, plugin, err)
	}
	return nil
}

// RecordPluginUsage stores what one plugin call cost. A call that is not
// recorded is a call the operator cannot see and the cap cannot count.
func (s *Store) RecordPluginUsage(ctx context.Context, u PluginUsageWrite) error {
	err := s.q.RecordPluginUsage(ctx, gen.RecordPluginUsageParams{
		PluginName:   u.Plugin,
		Job:          u.Job,
		Model:        u.Model,
		DriverID:     u.DriverID,
		InputTokens:  u.Input,
		OutputTokens: u.Output,
		Cached:       u.Cached,
	})
	if err != nil {
		return fmt.Errorf("db: cannot record what the %q plugin spent: %w", u.Plugin, err)
	}
	return nil
}

// PluginTokensSince is what every plugin has spent since an instant. It is what
// the daily cap is measured against.
func (s *Store) PluginTokensSince(ctx context.Context, since time.Time) (TokenUse, error) {
	row, err := s.q.PluginTokensSince(ctx, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return TokenUse{}, fmt.Errorf("db: cannot read what the plugins have spent: %w", err)
	}
	return TokenUse{Input: row.InputTokens, Output: row.OutputTokens, Calls: row.Calls}, nil
}

// PluginTokensSinceFor is the same for one plugin, which is what the panel
// shows per plugin.
func (s *Store) PluginTokensSinceFor(ctx context.Context, name string, since time.Time) (TokenUse, error) {
	row, err := s.q.PluginTokensSinceFor(ctx, gen.PluginTokensSinceForParams{
		PluginName: name,
		At:         pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return TokenUse{}, fmt.Errorf("db: cannot read what the %q plugin has spent: %w", name, err)
	}
	return TokenUse{Input: row.InputTokens, Output: row.OutputTokens, Calls: row.Calls}, nil
}

// orJSON substitutes an empty document for nothing, so that a NOT NULL jsonb
// column cannot be handed a nil by a caller that had nothing to say.
func orJSON(b []byte, empty string) []byte {
	if len(b) == 0 {
		return []byte(empty)
	}
	return b
}

// PluginDatabaseRow is what the server remembers about a plugin's own database:
// the sealed password for its role, whether the role and schema exist, and the
// last migration file it applied.
type PluginDatabaseRow struct {
	PasswordSealed []byte
	Provisioned    bool
	Migration      string
}

// PluginDatabase reads what is known about a plugin's database. A plugin that
// has none reads back zero, which is not an error: most plugins have none.
func (s *Store) PluginDatabase(ctx context.Context, name string) (PluginDatabaseRow, error) {
	row, err := s.q.GetPluginDatabase(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PluginDatabaseRow{}, ErrNotFound
		}
		return PluginDatabaseRow{}, fmt.Errorf("db: reading the database of plugin %s: %w", name, err)
	}
	return PluginDatabaseRow{
		PasswordSealed: row.DbPasswordSealed,
		Provisioned:    row.DbProvisioned,
		Migration:      row.DbMigration,
	}, nil
}

// SetPluginProvisioned records that a plugin's role and schema exist and stores
// the sealed password that reaches them. The caller has already created both:
// this is the bookkeeping, and it is written last so that a crash between the
// two leaves a server that will provision again rather than one that believes
// in a role it never made.
func (s *Store) SetPluginProvisioned(ctx context.Context, name string, sealed []byte, migration string) error {
	if len(sealed) == 0 {
		return fmt.Errorf("db: plugin %s cannot be provisioned with no password", name)
	}
	if err := s.q.SetPluginProvisioned(ctx, gen.SetPluginProvisionedParams{
		Name:             name,
		DbPasswordSealed: sealed,
		DbMigration:      migration,
	}); err != nil {
		return fmt.Errorf("db: recording the database of plugin %s: %w", name, err)
	}
	return nil
}

// ClearPluginDatabase forgets a plugin's database. The caller has already
// dropped the role and the schema; this removes the password that reached them,
// which must not outlive them.
func (s *Store) ClearPluginDatabase(ctx context.Context, name string) error {
	if err := s.q.ClearPluginDatabase(ctx, name); err != nil {
		return fmt.Errorf("db: clearing the database of plugin %s: %w", name, err)
	}
	return nil
}
