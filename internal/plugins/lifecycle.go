package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
)

// What an operator does to a plugin from the panel: see all of them, turn one
// off, turn it back on, and restart one that is stuck.
//
// The distinction running through this file is between what the operator chose
// and what is happening. Enabled is a choice and survives a restart of the
// server; the state is a consequence and does not. A plugin that is disabled has
// no process at all, which is the point — a disabled plugin that was still
// running would be a switch that does nothing.

// All is every plugin this server knows about: the ones it is supervising, and
// the ones it has a row for but no process — disabled, or with its files gone.
//
// [Host.List] is the live view and is what the request path uses. This is the
// panel's view, and it is a different question: an operator looking for the
// plugin they installed last week needs to find it whether or not it is running,
// and a plugin that has vanished from the directory has to be visible or its
// settings and its database are unreachable.
func (h *Host) All(ctx context.Context) ([]Status, error) {
	rows, err := h.opts.Store.Plugins(ctx)
	if err != nil {
		return nil, err
	}

	h.mu.RLock()
	live := make(map[string]*instance, len(h.instances))
	for name, inst := range h.instances {
		live[name] = inst
	}
	h.mu.RUnlock()

	out := make([]Status, 0, len(rows))
	for _, row := range rows {
		if inst, ok := live[row.Name]; ok {
			s := inst.status()
			s.Enabled = row.Enabled
			s.Installed = true
			out = append(out, s)
			continue
		}
		out = append(out, statusOfRow(row, h.installed(row)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Status is one plugin, for the panel. A plugin with no row is not found, which
// is what an operator following a stale link gets.
func (h *Host) Status(ctx context.Context, name string) (Status, error) {
	row, err := h.opts.Store.Plugin(ctx, name)
	if err != nil {
		return Status{}, err
	}
	h.mu.RLock()
	inst, live := h.instances[name]
	h.mu.RUnlock()

	if live {
		s := inst.status()
		s.Enabled = row.Enabled
		s.Installed = true
		return s, nil
	}
	return statusOfRow(row, h.installed(row)), nil
}

// statusOfRow renders a plugin that has no process from what was stored the last
// time it had one. Everything on it is as of then, which is why the panel says
// when a plugin is not running rather than showing a stale state as current.
func statusOfRow(row db.PluginRecord, installed bool) Status {
	s := Status{
		Name:             row.Name,
		Version:          row.Version,
		Author:           row.Author,
		Description:      row.Description,
		InterfaceVersion: row.InterfaceVersion,
		State:            State(row.State),
		LastError:        row.LastError,
		LastOutput:       row.LastOutput,
		Restarts:         row.Restarts,
		Enabled:          row.Enabled,
		Installed:        installed,
	}
	// Both declarations are stored as the JSON they arrived as, so that this
	// package's own types can change without a migration. A document that will
	// not parse is left empty rather than failing the page: an operator looking
	// at a plugin with a corrupt row still needs the row.
	if len(row.Capabilities) > 0 {
		_ = json.Unmarshal(row.Capabilities, &s.Capabilities)
	}
	if len(row.DeclaredSettings) > 0 {
		_ = json.Unmarshal(row.DeclaredSettings, &s.Settings)
	}
	return s
}

// installed reports whether a plugin's files are still where it was found.
func (h *Host) installed(row db.PluginRecord) bool {
	dir := row.Directory
	if dir == "" {
		dir = row.Name
	}
	_, err := plugin.LoadManifest(filepath.Join(h.opts.Dir, dir))
	return err == nil
}

// Enable turns a plugin on and starts it. It is safe on one that is already
// enabled, which is an operator pressing a button twice.
func (h *Host) Enable(ctx context.Context, name string) error {
	row, err := h.opts.Store.Plugin(ctx, name)
	if err != nil {
		return err
	}
	if err := h.opts.Store.SavePluginEnabled(ctx, name, true); err != nil {
		return err
	}

	h.mu.RLock()
	_, live := h.instances[name]
	closed := h.closed
	h.mu.RUnlock()
	if live {
		return nil
	}
	if closed {
		return ErrClosed
	}

	dir := row.Directory
	if dir == "" {
		dir = name
	}
	// install is the whole start path — manifest, version check, database,
	// supervision — so enabling takes it rather than reimplementing half of it
	// and diverging in a year.
	if err := h.install(ctx, dir); err != nil {
		return fmt.Errorf("plugins: %s was enabled but will not start: %w", name, err)
	}
	return nil
}

// Disable stops a plugin and records that the operator wants it stopped. Its
// settings, its row and its database are all left alone: disabling is not
// removing, and an operator turning something off for an evening must not lose
// what they configured.
func (h *Host) Disable(ctx context.Context, name string) error {
	if _, err := h.opts.Store.Plugin(ctx, name); err != nil {
		return err
	}
	if err := h.opts.Store.SavePluginEnabled(ctx, name, false); err != nil {
		return err
	}

	h.mu.Lock()
	inst, live := h.instances[name]
	delete(h.instances, name)
	h.mu.Unlock()

	if live {
		inst.stop()
	}
	h.setState(ctx, name, StateDisabled, "", "", 0)
	return nil
}

// Restart stops a plugin and starts it again. It is what an operator presses
// when a plugin has wedged against something outside it — a vendor that was
// down, a credential that has just been corrected — and it is the one action
// here that changes nothing about what was chosen or stored.
func (h *Host) Restart(ctx context.Context, name string) error {
	row, err := h.opts.Store.Plugin(ctx, name)
	if err != nil {
		return err
	}
	if !row.Enabled {
		return fmt.Errorf("%w: %s is turned off, so there is nothing to restart", ErrUnavailable, name)
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	inst, live := h.instances[name]
	delete(h.instances, name)
	h.mu.Unlock()

	if live {
		inst.stop()
	}

	dir := row.Directory
	if dir == "" {
		dir = name
	}
	if err := h.install(ctx, dir); err != nil {
		return fmt.Errorf("plugins: %s would not restart: %w", name, err)
	}
	return nil
}

// enabled reports whether the operator wants this plugin running. A row that
// cannot be read is treated as enabled and logged: refusing to start every
// plugin because one query failed would turn a database hiccup into a server
// with no plugins, and the failure is already visible.
func (h *Host) enabled(ctx context.Context, name string) bool {
	row, err := h.opts.Store.Plugin(ctx, name)
	switch {
	case errors.Is(err, db.ErrNotFound):
		// Never seen before, which is a plugin installed by dropping it in the
		// directory. New plugins start.
		return true
	case err != nil:
		h.opts.Log.LogAttrs(ctx, slog.LevelWarn,
			"whether a plugin is turned on could not be read, so it is being started",
			slog.String("plugin", name), slog.String("reason", err.Error()))
		return true
	}
	return row.Enabled
}
