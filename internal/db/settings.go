package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db/gen"
)

// The keys of the settings table. They are spelled out rather than derived from
// the struct so that renaming a Go field cannot silently orphan a stored row.
const (
	SettingOrganisation = "organisation"
	SettingPublicHost   = "public_host"
	SettingTLSMode      = "tls_mode"
	SettingShortName    = "short_name"
	SettingLogo         = "logo"
	SettingAccent       = "accent"
	SettingLimits       = "limits"
	SettingRetention    = "retention"
	SettingMarketplace  = "marketplace"
)

// Settings reads the organisation's own settings. They live in the database
// rather than beside the binary because they are what makes this deployment
// itself: an operator who loses the data directory keeps their organisation
// name, their public address and how they are reached.
func (s *Store) Settings(ctx context.Context) (config.Settings, error) {
	rows, err := s.q.ListSettings(ctx)
	if err != nil {
		return config.Settings{}, fmt.Errorf("db: cannot read the settings: %w", err)
	}
	// The defaults a settings table can be missing a row for. There is no
	// vendor setting among them any more: a plugin holds its own, and this
	// server stores none.
	out := config.Settings{
		Limits: config.DefaultLimits(),
	}
	for _, r := range rows {
		if err := applySetting(&out, r.Key, r.Value); err != nil {
			return config.Settings{}, err
		}
	}
	return out, nil
}

// SaveSettings replaces the organisation's settings.
func (s *Store) SaveSettings(ctx context.Context, set config.Settings) error {
	return saveSettings(ctx, s.q, set)
}

// SettingString reads one string setting, for the places that want a single
// value without the whole document.
func (s *Store) SettingString(ctx context.Context, key string) (string, error) {
	raw, err := s.q.GetSetting(ctx, key)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("db: cannot read the %q setting: %w", key, err)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("db: the %q setting is not readable: %w", key, err)
	}
	return v, nil
}

func applySetting(out *config.Settings, key string, raw []byte) error {
	var err error
	switch key {
	case SettingOrganisation:
		err = json.Unmarshal(raw, &out.Organisation)
	case SettingPublicHost:
		err = json.Unmarshal(raw, &out.PublicHost)
	case SettingTLSMode:
		err = json.Unmarshal(raw, &out.TLSMode)
	case SettingShortName:
		err = json.Unmarshal(raw, &out.ShortName)
	case SettingLogo:
		err = json.Unmarshal(raw, &out.Logo)
	case SettingAccent:
		err = json.Unmarshal(raw, &out.Accent)
	case SettingLimits:
		err = json.Unmarshal(raw, &out.Limits)
	case SettingRetention:
		err = json.Unmarshal(raw, &out.Retention)
	case SettingMarketplace:
		err = json.Unmarshal(raw, &out.Marketplace)
	default:
		// A setting this build does not know about belongs to a newer one.
		// Leaving it alone is what lets a downgrade be survivable.
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: the %q setting is not readable: %w", key, err)
	}
	return nil
}

func saveSettings(ctx context.Context, q *gen.Queries, set config.Settings) error {
	pairs := []struct {
		key   string
		value any
	}{
		{SettingOrganisation, set.Organisation},
		{SettingPublicHost, set.PublicHost},
		{SettingTLSMode, set.TLSMode},
		{SettingShortName, set.ShortName},
		{SettingLogo, set.Logo},
		{SettingAccent, set.Accent},
		{SettingLimits, set.Limits},
		{SettingRetention, set.Retention},
		{SettingMarketplace, set.Marketplace},
	}
	for _, p := range pairs {
		raw, err := json.Marshal(p.value)
		if err != nil {
			return fmt.Errorf("db: cannot encode the %q setting: %w", p.key, err)
		}
		if err := q.UpsertSetting(ctx, gen.UpsertSettingParams{Key: p.key, Value: raw}); err != nil {
			return fmt.Errorf("db: cannot store the %q setting: %w", p.key, err)
		}
	}
	return nil
}
