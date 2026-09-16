package plugins

import "errors"

// What a caller gets when a plugin does not answer. Each one is a different
// thing for the caller to do, which is why they are separate values and not one
// "the plugin did not work".
var (
	// ErrNoPlugin is a name nothing is installed under. It is a mistake in the
	// caller, not a runtime condition.
	ErrNoPlugin = errors.New("plugins: no plugin of that name is installed")

	// ErrUnavailable is a plugin that is installed but not running — starting,
	// stopped, disabled by the operator, or failed. The caller falls back and
	// says nothing to the driver about it; the panel is where a plugin's state
	// is explained.
	ErrUnavailable = errors.New("plugins: the plugin is not running")

	// ErrOverCap is the operator's daily token cap having been reached. The
	// plugin is not called at all — not called and truncated, not called and
	// refused by the vendor — and the caller is told so that it falls back to
	// whatever it does without that plugin, and nothing else breaks.
	ErrOverCap = errors.New("plugins: the daily token cap has been reached")

	// ErrClosed is the host having been shut down.
	ErrClosed = errors.New("plugins: the plugin host is closed")
)
