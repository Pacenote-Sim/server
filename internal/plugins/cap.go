package plugins

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/pacenote-sim/server/internal/db"
)

// What a plugin may spend, and what it has.
//
// The cap is stored in plugin_settings beside the plugin's own settings, but it
// is not one of them: the plugin does not declare it, is never sent it, and
// cannot read it. It is the operator's number and the host's to enforce.
//
// That distinction is the whole reason this file exists. A plugin holding its
// own spending limit could simply not apply it — and a limit that somebody
// else's code may ignore is a preference, not a limit. The host checks it before
// the plugin is called at all, because calling it and throwing the answer away
// would still have spent the money.
//
// It is per plugin rather than one number for the server. Two reasons: an
// operator running two plugins wants to know which one is costing them, and a
// cap that is reached by one plugin should not silence the other.

// The settings a plugin does not own. They live in the same table as the ones it
// declares, and they are told apart by the leading underscore: the plugin
// interface refuses a setting name that starts with one, so a plugin cannot
// declare a name that collides with these however hard it tries.
const (
	// SettingDailyCap is the most tokens this plugin may spend in a day, as a
	// decimal string. Absent or zero means no cap.
	SettingDailyCap = "_daily_token_cap"
)

// Reserved reports whether a setting name belongs to the host rather than to the
// plugin. The panel uses it to keep these off the form a plugin declares, and it
// is a prefix test rather than a list so that adding one needs no second edit.
func Reserved(name string) bool { return name != "" && name[0] == '_' }

// DailyCap is what this plugin may spend in a day, in tokens. Zero is no cap,
// which is the operator saying so rather than a default nobody chose.
func (h *Host) DailyCap(ctx context.Context, name string) (int64, error) {
	rows, err := h.opts.Store.PluginSettings(ctx, name)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if row.Name != SettingDailyCap {
			continue
		}
		n, err := strconv.ParseInt(row.Value, 10, 64)
		if err != nil {
			// A value that is not a number is not a cap that is off: it is a
			// row somebody edited by hand, and refusing is the safe direction.
			return 0, fmt.Errorf("plugins: the daily cap of %s is %q, which is not a number of tokens", name, row.Value)
		}
		return n, nil
	}
	return 0, nil
}

// SetDailyCap records what a plugin may spend. Zero removes the cap.
func (h *Host) SetDailyCap(ctx context.Context, name string, tokens int64) error {
	if tokens < 0 {
		return fmt.Errorf("plugins: a daily cap cannot be negative")
	}
	if _, err := h.opts.Store.Plugin(ctx, name); err != nil {
		return err
	}
	return h.opts.Store.SavePluginSetting(ctx, name, SettingDailyCap, strconv.FormatInt(tokens, 10))
}

// Spending is what one plugin has cost, over the two windows an operator asks
// about: today, because that is what the cap is measured against, and this
// month, because that is what the bill is.
type Spending struct {
	// Cap is the daily cap in tokens, zero for none.
	Cap int64
	// Today and Month are what has been spent in each window.
	Today db.TokenUse
	Month db.TokenUse
	// OverCap reports that the cap has been reached, which is a plugin that is
	// installed, running, configured and quietly not being called.
	OverCap bool
}

// Spending reads what a plugin has spent and what it may.
func (h *Host) Spending(ctx context.Context, name string) (Spending, error) {
	var out Spending

	capTokens, err := h.DailyCap(ctx, name)
	if err != nil {
		return out, err
	}
	out.Cap = capTokens

	now := h.opts.Now()
	if out.Today, err = h.opts.Store.PluginTokensSinceFor(ctx, name, startOfDay(now)); err != nil {
		return out, err
	}
	if out.Month, err = h.opts.Store.PluginTokensSinceFor(ctx, name, startOfMonth(now)); err != nil {
		return out, err
	}
	out.OverCap = capTokens > 0 && out.Today.Total() >= capTokens
	return out, nil
}

// startOfMonth is the first instant of the operator's own month, in the server's
// time zone. The bill is monthly and the operator reads it in their own time,
// so a month that started at midnight UTC would be the wrong month for most of
// the world for part of every day.
func startOfMonth(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}
