package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
)

// The host's own numbers. They are constants rather than settings because an
// operator has no way to choose them well, and because every one of them is a
// promise this server makes to the rest of itself rather than a preference.
const (
	// DefaultCallTimeout is how long a request waits when the caller names no
	// deadline of its own. It is short because the caller is usually a driver
	// at speed: an answer that arrives after the corner is worse than none.
	DefaultCallTimeout = 5 * time.Second

	// DefaultEventTimeout bounds a fire-and-forget delivery. Nothing is
	// waiting on it, so it is generous — but it is not unbounded, because a
	// wedged plugin would otherwise hold a goroutine and a process for ever.
	DefaultEventTimeout = 30 * time.Second

	// DefaultStartTimeout is how long a plugin has to complete its handshake.
	DefaultStartTimeout = 10 * time.Second

	// DefaultMaxRestarts is how many times a plugin is restarted before the
	// host gives up on it. Five is enough to ride out a transient cause — a
	// vendor that was down, a file that was being replaced — and few enough
	// that a genuinely broken plugin is marked failed within a minute rather
	// than restarting for the life of the server.
	DefaultMaxRestarts = 5

	// DefaultRestartBackoff is the first wait after a crash. It doubles each
	// time, up to MaxRestartBackoff.
	DefaultRestartBackoff = 500 * time.Millisecond

	// MaxRestartBackoff is the longest wait between restarts.
	MaxRestartBackoff = 30 * time.Second

	// outputTail is how much of a plugin's standard error is kept to show the
	// operator. Enough for a stack trace, bounded because a plugin in a crash
	// loop can print without limit.
	outputTail = 8 << 10
)

// Options is what the host needs from the rest of the server.
type Options struct {
	// Dir is the plugin directory: one subdirectory per plugin, each with a
	// manifest and a binary. It is created if it is not there, so that an
	// operator dropping a plugin into it has somewhere to drop it.
	Dir string
	// Log receives the host's own lines. A credential is never one of them,
	// which is enforced by the scrubber rather than promised.
	Log *slog.Logger
	// Store is where installed plugins, their settings and their spending
	// live.
	Store Store
	// Keyring holds the data key that opens a plugin's sealed credentials. It
	// is a keyring rather than a key because an operator can regenerate the
	// data key while the server runs. A nil keyring, or one holding no key, is
	// every plugin credential being unreadable, which is reported as the
	// plugin needing configuring rather than as the plugin being broken.
	Keyring *auth.Keyring
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// CallTimeout, EventTimeout and StartTimeout override the defaults above.
	CallTimeout  time.Duration
	EventTimeout time.Duration
	StartTimeout time.Duration
	// MaxRestarts and RestartBackoff override the restart policy.
	MaxRestarts    int
	RestartBackoff time.Duration
}

// Host runs the plugins.
type Host struct {
	opts Options

	// ctx is the host's own lifetime. Every call to a plugin descends from it
	// rather than from the caller's context: an event dispatched while an
	// upload is being answered must not be cancelled when that response is
	// written, and a plugin must not outlive the host.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.RWMutex
	instances map[string]*instance
	closed    bool
}

// New builds a host. It does not start anything: [Host.Discover] does, once the
// caller is ready for plugins to begin receiving events.
func New(opts Options) (*Host, error) {
	if opts.Dir == "" {
		return nil, errors.New("plugins: the host was given no plugin directory")
	}
	if opts.Store == nil {
		return nil, errors.New("plugins: the host was given no store")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.EventTimeout <= 0 {
		opts.EventTimeout = DefaultEventTimeout
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = DefaultStartTimeout
	}
	if opts.MaxRestarts <= 0 {
		opts.MaxRestarts = DefaultMaxRestarts
	}
	if opts.RestartBackoff <= 0 {
		opts.RestartBackoff = DefaultRestartBackoff
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Host{
		opts:      opts,
		ctx:       ctx,
		cancel:    cancel,
		instances: make(map[string]*instance),
	}, nil
}

// InterfaceVersion is the contract this host speaks. It is exposed so the panel
// and the startup log can name it, and so a test cannot assert against a
// version the host does not actually implement.
func (h *Host) InterfaceVersion() int { return plugin.InterfaceVersion }

// Discover reads the plugin directory and reconciles it with what is installed:
// new directories are recorded and started, known ones are updated, and a
// plugin whose files have gone is stopped and marked so.
//
// It is safe to call again — that is what a rescan is.
func (h *Host) Discover(ctx context.Context) error {
	if err := os.MkdirAll(h.opts.Dir, 0o700); err != nil {
		return fmt.Errorf("plugins: cannot create the plugin directory %s: %w", h.opts.Dir, err)
	}
	entries, err := os.ReadDir(h.opts.Dir)
	if err != nil {
		return fmt.Errorf("plugins: cannot read the plugin directory %s: %w", h.opts.Dir, err)
	}

	found := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			// A stray file is not a plugin and saying so on every rescan would
			// be noise. A plugin is a directory; that is the whole rule.
			continue
		}
		name := e.Name()
		found[name] = true
		if err := h.install(ctx, name); err != nil {
			h.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin will not be started",
				slog.String("plugin", name), slog.String("reason", err.Error()))
		}
	}

	h.retire(ctx, found)
	return nil
}

// install records and starts the plugin in one directory. A manifest that does
// not parse, or one built against another interface version, is recorded as
// failed with the reason and is not started: there is no degraded mode, because
// a plugin quietly missing a fact is worse than one that is plainly off.
func (h *Host) install(ctx context.Context, dirName string) error {
	dir := filepath.Join(h.opts.Dir, dirName)

	m, err := plugin.LoadManifest(dir)
	if err != nil {
		h.recordBroken(ctx, dirName, dir, err)
		return err
	}
	if m.Name != dirName {
		err := fmt.Errorf("%w: the plugin in %s calls itself %q — a plugin's directory and its name must match",
			plugin.ErrInvalid, dirName, m.Name)
		h.recordBroken(ctx, dirName, dir, err)
		return err
	}
	if err := plugin.CheckVersion(m.Name, m.InterfaceVersion, plugin.InterfaceVersion); err != nil {
		if recErr := h.record(ctx, m, dirName, StateFailed, err.Error()); recErr != nil {
			h.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin refused for its interface version could not be recorded",
				slog.String("plugin", m.Name), slog.String("reason", recErr.Error()))
		}
		return err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	existing, ok := h.instances[m.Name]
	h.mu.Unlock()

	if ok {
		// Already supervised. The manifest is refreshed so the panel shows the
		// version on disk, and the process is left alone: restarting a working
		// plugin because somebody pressed rescan is not what rescan means.
		existing.refresh(m)
		return h.record(ctx, m, dirName, existing.currentState(), "")
	}

	// A plugin the operator turned off is recorded and left alone. The check is
	// here rather than in Discover so that every path which starts a plugin
	// goes through it: a rescan, a restart of the server, and Enable itself all
	// arrive at this line.
	if !h.enabled(ctx, m.Name) {
		return h.record(ctx, m, dirName, StateDisabled, "")
	}

	if err := h.record(ctx, m, dirName, StateDiscovered, ""); err != nil {
		return err
	}

	// A plugin that asked for tables gets them before it is started, so that it
	// can open its pool in main and fail loudly if it cannot. Failing here is
	// this plugin failing, not the server: a team whose results plugin has no
	// database still wants their laps uploaded.
	dsn, password, err := h.provision(ctx, m, dir)
	if err != nil {
		if recErr := h.record(ctx, m, dirName, StateFailed, err.Error()); recErr != nil {
			h.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin that could not be given a database could not be recorded",
				slog.String("plugin", m.Name), slog.String("reason", recErr.Error()))
		}
		return err
	}

	inst := newInstance(h, m, dir, dsn, password)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	h.instances[m.Name] = inst
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		inst.supervise()
	}()
	return nil
}

// retire stops what is no longer on disk. The row stays: an operator who
// deletes a directory should find the plugin listed as gone rather than find
// nothing and wonder whether they imagined it.
func (h *Host) retire(ctx context.Context, found map[string]bool) {
	h.mu.Lock()
	var gone []*instance
	for name, inst := range h.instances {
		if !found[name] {
			gone = append(gone, inst)
			delete(h.instances, name)
		}
	}
	h.mu.Unlock()

	for _, inst := range gone {
		inst.stop()
		h.setState(ctx, inst.name(), StateStopped,
			"Its files are no longer in the plugin directory.", inst.output(), inst.restartCount())
	}
}

// Remove uninstalls a plugin: it stops the process, drops the database it owned,
// and forgets the row.
//
// It is not what happens when a plugin's files disappear — that is [Host.retire],
// which stops the process and keeps everything, because a directory deleted by
// accident or moved during an upgrade must not destroy a season of results.
// Remove is the operator saying so, and it is the only path that destroys data.
//
// The order is deliberate. The process is stopped first, so nothing is holding a
// connection to the schema about to be dropped; the database goes next; the row
// last, so that a failure anywhere leaves a plugin the next call can finish
// removing rather than a row pointing at a role that is already gone.
//
// Removing the files is the caller's: this host owns processes and rows, and a
// panel that deletes a directory should do it where it can report what happened.
func (h *Host) Remove(ctx context.Context, name string) error {
	h.mu.Lock()
	inst, running := h.instances[name]
	delete(h.instances, name)
	h.mu.Unlock()

	if running {
		inst.stop()
	}
	if err := h.deprovision(ctx, name); err != nil {
		return err
	}
	return h.opts.Store.DeletePlugin(ctx, name)
}

// recordBroken stores a plugin that could not even be read. The directory name
// stands in for the plugin's own, because a manifest that will not parse has no
// name to offer, and an operator looking for what went wrong is looking at the
// directory.
func (h *Host) recordBroken(ctx context.Context, dirName, dir string, cause error) {
	m := plugin.Manifest{
		Name:        dirName,
		Version:     "unknown",
		Author:      "unknown",
		Description: "This plugin's manifest could not be read.",
	}
	if err := h.record(ctx, m, dirName, StateFailed, cause.Error()); err != nil {
		h.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a broken plugin could not be recorded",
			slog.String("directory", dir), slog.String("reason", err.Error()))
	}
}

// record writes what is known about a plugin.
func (h *Host) record(ctx context.Context, m plugin.Manifest, dirName string, state State, lastError string) error {
	capabilities, err := marshalCapabilities(m.Capabilities)
	if err != nil {
		return err
	}
	err = h.opts.Store.SavePlugin(ctx, db.PluginRecord{
		Name:             m.Name,
		Version:          m.Version,
		Author:           m.Author,
		Description:      m.Description,
		InterfaceVersion: m.InterfaceVersion,
		Capabilities:     capabilities,
		Directory:        dirName,
		State:            string(state),
	})
	if err != nil {
		return err
	}
	if lastError != "" {
		return h.opts.Store.SavePluginState(ctx, m.Name, string(state), lastError, "", 0)
	}
	return nil
}

// setState records a change of state, and says so in the log when it is one an
// operator would want to know about.
func (h *Host) setState(ctx context.Context, name string, state State, lastError, lastOutput string, restarts int) {
	if err := h.opts.Store.SavePluginState(ctx, name, string(state), lastError, lastOutput, restarts); err != nil {
		h.opts.Log.LogAttrs(ctx, slog.LevelWarn, "the state of a plugin could not be recorded",
			slog.String("plugin", name), slog.String("state", string(state)), slog.String("reason", err.Error()))
	}
}

// Close stops every plugin and waits for them. A host that is closed does not
// start anything again.
func (h *Host) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	instances := make([]*instance, 0, len(h.instances))
	for _, inst := range h.instances {
		instances = append(instances, inst)
	}
	h.mu.Unlock()

	h.cancel()
	for _, inst := range instances {
		inst.stop()
	}
	h.wg.Wait()
}

// Notify tells every plugin that asked for this kind of event that it happened,
// and returns immediately.
//
// The caller's context is deliberately not the event's. An upload finishing
// must not cancel the delivery it triggered, and a plugin that is slow, wedged
// or dead must not delay the upload — which together are the whole reason this
// is fire and forget rather than a call.
func (h *Host) Notify(ctx context.Context, e plugin.Event) {
	if err := e.Validate(); err != nil {
		h.opts.Log.LogAttrs(ctx, slog.LevelWarn, "an event was not dispatched",
			slog.String("event", string(e.Kind)), slog.String("reason", err.Error()))
		return
	}

	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	var targets []*instance
	for _, inst := range h.instances {
		if inst.wants(e.Kind) {
			targets = append(targets, inst)
		}
	}
	h.mu.RUnlock()

	for _, inst := range targets {
		h.wg.Add(1)
		go func(inst *instance) {
			defer h.wg.Done()
			inst.deliver(e)
		}(inst)
	}
}

// Ask puts a request to one plugin and waits for the answer.
//
// The deadline is the request's own, or [Options.CallTimeout] when it carries
// none. A plugin that misses it is skipped and reported, and the caller gets an
// error rather than a late answer, so that it can use its own fallback: the
// deterministic cue this product generates with no model at all.
//
// The caller is told plainly which of the things that can go wrong did.
// [ErrNoPlugin], [ErrUnavailable] and [ErrOverCap] are the host's; the plugin's
// own [plugin.ErrNoAnswer] and [plugin.ErrNotConfigured] pass through.
func (h *Host) Ask(ctx context.Context, name string, r plugin.Request) (plugin.Response, error) {
	inst, err := h.running(name)
	if err != nil {
		return plugin.Response{}, err
	}
	if !inst.answers(r.Kind) {
		return plugin.Response{}, fmt.Errorf("%w: %s does not answer %s", plugin.ErrUnsupported, name, r.Kind)
	}
	if err := h.checkCap(ctx, name); err != nil {
		return plugin.Response{}, err
	}
	if r.Deadline.IsZero() {
		r.Deadline = h.opts.Now().Add(h.opts.CallTimeout)
	}
	return inst.ask(ctx, r)
}

// Answering names the plugins that say they answer this kind of request, so a
// caller can find one without knowing what the operator installed. The order is
// stable.
func (h *Host) Answering(kind plugin.RequestKind) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []string
	for name, inst := range h.instances {
		if inst.answers(kind) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Status is one plugin as the host sees it right now.
type Status struct {
	// Name, Version, Author and Description come from the manifest.
	Name        string
	Version     string
	Author      string
	Description string
	// InterfaceVersion is what it was built against.
	InterfaceVersion int
	// Capabilities is what it declared, and Settings what it asked to have
	// configured. Settings is empty for a plugin that has never started.
	Capabilities plugin.Capabilities
	Settings     []plugin.Setting
	// State is what it is doing, LastError why it is not running, and
	// LastOutput the last of what it printed — scrubbed of credentials.
	State      State
	LastError  string
	LastOutput string
	// Restarts is how many times it has been restarted since it last ran
	// cleanly.
	Restarts int
	// Enabled is whether the operator wants it running at all. A disabled
	// plugin has no process, so everything above it comes from the row rather
	// than from anything live.
	Enabled bool
	// Installed is whether its files are still in the plugin directory. A
	// plugin whose directory was deleted keeps its row, its settings and its
	// database, and this is how the panel says so rather than showing it as
	// simply broken.
	Installed bool
}

// List is every plugin the host is supervising, by name. It is what a panel
// page will render; nothing renders it yet.
func (h *Host) List() []Status {
	h.mu.RLock()
	instances := make([]*instance, 0, len(h.instances))
	for _, inst := range h.instances {
		instances = append(instances, inst)
	}
	h.mu.RUnlock()

	out := make([]Status, 0, len(instances))
	for _, inst := range instances {
		out = append(out, inst.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// running is the plugin of that name, if it is in a state that can be called.
func (h *Host) running(name string) (*instance, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.closed {
		return nil, ErrClosed
	}
	inst, ok := h.instances[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoPlugin, name)
	}
	if state := inst.currentState(); state != StateRunning {
		return nil, fmt.Errorf("%w: %s is %s", ErrUnavailable, name, state)
	}
	return inst, nil
}

// checkCap refuses a call that would spend past this plugin's daily cap.
//
// The cap is the operator's and the host's: it is stored against the plugin, it
// is shown on the plugin's own page, and the plugin is neither told it nor asked
// about it. That is the whole point. A plugin that held its own spending limit
// could simply not apply it, and a limit somebody else's code may ignore is not
// a limit — it is a preference.
//
// The plugin is not called at all when it is reached. Calling it and discarding
// the answer would still have spent the money.
func (h *Host) checkCap(ctx context.Context, name string) error {
	capTokens, err := h.DailyCap(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: the cap could not be read: %w", ErrOverCap, err)
	}
	if capTokens <= 0 {
		return nil
	}
	spent, err := h.spentToday(ctx, name)
	if err != nil {
		// A cap that cannot be read is not a cap that is off. Refusing is the
		// safe direction: the driver hears the deterministic cue, and the
		// operator's bill does not depend on whether a query succeeded.
		return fmt.Errorf("%w: what has been spent today could not be read: %w", ErrOverCap, err)
	}
	if spent >= capTokens {
		return fmt.Errorf("%w: %d of %d tokens", ErrOverCap, spent, capTokens)
	}
	return nil
}

// spentToday is what one plugin has spent since local midnight.
func (h *Host) spentToday(ctx context.Context, name string) (int64, error) {
	since := startOfDay(h.opts.Now())
	use, err := h.opts.Store.PluginTokensSinceFor(ctx, name, since)
	if err != nil {
		return 0, err
	}
	return use.Total(), nil
}

// startOfDay is local midnight, which is the day an operator means when they
// set a daily cap. It is the server's clock and not the driver's, because it is
// the operator's bill.
func startOfDay(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}
