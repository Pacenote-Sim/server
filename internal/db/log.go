package db

import "log/slog"

// The logging helpers exist so that this package can use slog.LogAttrs — which
// costs nothing when the level is disabled — without every call site spelling
// out slog.String.
const levelInfo = slog.LevelInfo

func attrString(k, v string) slog.Attr { return slog.String(k, v) }
func attrInt64(k string, v int64) slog.Attr {
	return slog.Int64(k, v)
}
