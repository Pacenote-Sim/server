package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
)

// exitPoll is how often a running plugin is checked for having died. go-plugin
// exposes no channel for it, so this is a poll; a quarter of a second is far
// below anything a person would notice and far above anything that costs.
const exitPoll = 250 * time.Millisecond

// instance is one plugin: its manifest, its process, and the goroutine that
// keeps it alive.
type instance struct {
	host *instanceHost
	dir  string

	// done is closed when the host wants this plugin gone. It is what turns a
	// restart loop into a shutdown.
	done chan struct{}
	// stopOnce guards done, because retire and Close can both reach it.
	stopOnce sync.Once

	// scrub knows the credentials this plugin has been lent, and everything it
	// says passes through it before it is logged or stored.
	scrub *scrubber
	// tail is the last of its standard error, already scrubbed.
	tail *tail

	mu        sync.RWMutex
	manifest  plugin.Manifest
	state     State
	lastError string
	restarts  int
	// dsn is this plugin's own database, empty for one that declared none. It
	// is set once at construction and read on every start, so a restart after a
	// crash reconnects to the same schema.
	dsn    string
	client *goplugin.Client
	impl   plugin.Plugin
	// detach stops serving the host to this plugin, for one that asks. It is
	// called whenever the process is killed, because the channel's goroutine
	// does not go away on its own.
	detach   func()
	declared []plugin.Setting
	// valueRules is declared with the requirement lifted off every secret,
	// because a secret is required of the sealed map rather than of the plain
	// one. It is derived once here rather than per call: configure runs on
	// every lap, every request and every page, and this changes only when a
	// plugin starts and says what it needs.
	valueRules []plugin.Setting
}

// instanceHost is the part of the host an instance uses. It is a named type
// rather than the host itself so that the direction of the dependency is
// visible: an instance asks the host for policy and never reaches into its map.
type instanceHost = Host

// newInstance builds a supervised plugin. It does not start it.
//
// dsn is the plugin's own database, empty for a plugin that declared none, and
// password is the credential inside it. Both are given to the scrubber rather
// than only the password: a plugin that prints its connection string on a
// failure to connect would otherwise write it into last_output, where the panel
// shows it to whoever is watching.
func newInstance(h *Host, m plugin.Manifest, dir, dsn, password string) *instance {
	scrub := &scrubber{}
	scrub.learnValues(password, dsn)
	return &instance{
		host:     h,
		dir:      dir,
		dsn:      dsn,
		done:     make(chan struct{}),
		scrub:    scrub,
		tail:     newTail(outputTail, scrub),
		manifest: m,
		state:    StateDiscovered,
	}
}

func (i *instance) name() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.Name
}

func (i *instance) currentState() State {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state
}

func (i *instance) restartCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.restarts
}

func (i *instance) output() string { return i.tail.String() }

func (i *instance) wants(k plugin.EventKind) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state == StateRunning && i.manifest.Capabilities.Wants(k)
}

func (i *instance) answers(k plugin.RequestKind) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.Capabilities.Answers(k)
}

func (i *instance) mayAsk(target string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.Capabilities.MayAsk(target)
}

// refresh takes a manifest re-read from disk, so that a rescan after an upgrade
// shows the new version without restarting a working process.
func (i *instance) refresh(m plugin.Manifest) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.manifest = m
}

// manifestSnapshot is the manifest as last read, for a caller that describes
// the plugin rather than calls it.
func (i *instance) manifestSnapshot() plugin.Manifest {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest
}

func (i *instance) status() Status {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return Status{
		Name:             i.manifest.Name,
		Version:          i.manifest.Version,
		Author:           i.manifest.Author,
		Description:      i.manifest.Description,
		InterfaceVersion: i.manifest.InterfaceVersion,
		Capabilities:     i.manifest.Capabilities,
		Settings:         i.declared,
		State:            i.state,
		LastError:        i.lastError,
		LastOutput:       i.tail.String(),
		Restarts:         i.restarts,
	}
}

// stop asks the supervisor to give up and kills the process.
func (i *instance) stop() {
	i.stopOnce.Do(func() { close(i.done) })

	i.mu.Lock()
	c, d := i.client, i.detach
	i.client, i.detach = nil, nil
	i.impl = nil
	i.mu.Unlock()

	if c != nil {
		c.Kill()
	}
	if d != nil {
		d()
	}
}

// stopping reports whether this plugin should not be started again.
func (i *instance) stopping() bool {
	select {
	case <-i.done:
		return true
	case <-i.host.ctx.Done():
		return true
	default:
		return false
	}
}

// supervise starts the plugin and keeps it started. It is the whole lifecycle:
// start, run, notice a crash, wait, start again, and eventually give up.
//
// Giving up is as important as restarting. A plugin that crashes on every start
// is a plugin with a problem the operator has to fix, and a host that restarts
// it for ever turns that into a process being spawned twice a second for the
// rest of the season, with nothing in the panel to say why.
func (i *instance) supervise() {
	for {
		if i.stopping() {
			return
		}

		startedAt := i.host.opts.Now()
		if err := i.start(); err != nil {
			if i.giveUp(err) {
				return
			}
			continue
		}

		i.setState(StateRunning, "")
		i.watch()

		if i.stopping() {
			i.setState(StateStopped, "")
			return
		}

		// A plugin that ran for a while before dying is a different animal
		// from one that dies on start: a transient cause should not count
		// towards the attempts that mark it failed for good.
		if i.host.opts.Now().Sub(startedAt) >= i.host.opts.StartTimeout {
			i.resetRestarts()
		}
		i.teardown()
		if i.giveUp(errors.New("the plugin stopped without being asked to")) {
			return
		}
	}
}

// giveUp records a failed attempt and reports whether the host is done with
// this plugin — because it has failed too many times, or because the host is
// shutting down. It waits out the backoff when it is not.
func (i *instance) giveUp(cause error) bool {
	i.mu.Lock()
	i.restarts++
	attempts := i.restarts
	i.mu.Unlock()

	reason := i.scrub.cleanError(cause)
	if attempts > i.host.opts.MaxRestarts {
		final := fmt.Sprintf("%s. It has now failed %d times, so it will not be started again until the server is restarted or the plugin is rescanned.",
			reason, attempts)
		i.setState(StateFailed, final)
		i.host.opts.Log.LogAttrs(i.host.ctx, slog.LevelError, "a plugin has failed for good",
			slog.String("plugin", i.name()),
			slog.Int("attempts", attempts),
			slog.String("reason", reason))
		return true
	}

	i.setState(StateStarting, reason)
	i.host.opts.Log.LogAttrs(i.host.ctx, slog.LevelWarn, "a plugin stopped and will be restarted",
		slog.String("plugin", i.name()),
		slog.Int("attempt", attempts),
		slog.String("reason", reason))

	wait := backoffFor(i.host.opts.RestartBackoff, attempts)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-i.done:
		return true
	case <-i.host.ctx.Done():
		return true
	}
}

// backoffFor doubles the wait per attempt, up to [MaxRestartBackoff]. It is
// plain exponential backoff with no jitter, because there is one host and one
// plugin and nothing here to stampede.
func backoffFor(initial time.Duration, attempt int) time.Duration {
	wait := initial
	for range attempt - 1 {
		wait *= 2
		if wait >= MaxRestartBackoff {
			return MaxRestartBackoff
		}
	}
	return wait
}

func (i *instance) resetRestarts() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.restarts = 0
}

// start runs the plugin's binary, completes the handshake and asks it what it
// needs configured.
func (i *instance) start() error {
	i.mu.RLock()
	m := i.manifest
	i.mu.RUnlock()

	i.setState(StateStarting, "")

	exe := m.Executable(i.dir)
	if err := executable(exe); err != nil {
		return err
	}

	// The plugin is started with a clean environment on purpose — SkipHostEnv
	// below — because this server's own environment holds its database
	// connection string and its data key, and a plugin has no business with
	// either. What it is allowed to know arrives as settings, as credentials it
	// declared, and as the one variable set here.
	//
	// That one is its own database, and it is an environment variable rather
	// than a value lent per call because a plugin needs a connection pool for
	// its lifetime and would stash the string on first use anyway. The DSN it
	// carries reaches a role that owns one schema and may read the core_read
	// views: handing it over grants the plugin exactly what the plugin already
	// is.
	cmd := exec.Command(exe) //nolint:gosec // G204: the binary is the one named by the manifest in the operator's own plugin directory.
	cmd.Dir = i.dir
	if i.dsn != "" {
		cmd.Env = []string{plugin.EnvDatabaseURL + "=" + i.dsn}
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  plugin.Handshake,
		Plugins:          plugin.ClientSet(),
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		// go-plugin's own logger would write the plugin's every line to
		// standard error at trace level. The host logs what matters itself,
		// and captures the plugin's output into a scrubbed buffer.
		Logger: hclog.NewNullLogger(),
		// Two writers, and both are needed. Stderr is the process's real
		// standard error, which carries everything a plugin prints before it
		// starts serving — the output that explains a plugin that will not
		// start at all. Sync* are the streams go-plugin substitutes for the
		// plugin's own os.Stdout and os.Stderr once it is serving, which carry
		// everything it prints afterwards. Capturing only one of them would
		// give an operator half the story and never say which half.
		Stderr:       i.tail,
		SyncStdout:   i.tail,
		SyncStderr:   i.tail,
		StartTimeout: i.host.opts.StartTimeout,
		SkipHostEnv:  true,
	})

	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return fmt.Errorf("it would not start: %w", err)
	}
	raw, err := rpc.Dispense(plugin.DispenseKey)
	if err != nil {
		client.Kill()
		return fmt.Errorf("it started but would not answer: %w", err)
	}
	impl, ok := raw.(plugin.Plugin)
	if !ok {
		client.Kill()
		return fmt.Errorf("%w: it serves something that is not a Pacenote plugin", plugin.ErrInvalid)
	}

	// A plugin whose manifest names other plugins it asks is handed this host
	// to ask through, before its settings are read: the plugin's Connected
	// runs inside that first call. One that names nobody is offered nothing,
	// so a question from it has nowhere to go — the manifest is the
	// operator's view of what a plugin does, and this is where it is kept
	// true.
	detach := func() {}
	if len(m.Capabilities.Asks) > 0 {
		detach, _ = plugin.Attach(impl, &asker{host: i.host, from: m.Name})
	}

	ctx, cancel := context.WithTimeout(i.host.ctx, i.host.opts.StartTimeout)
	defer cancel()
	declared, err := impl.Settings(ctx)
	if err != nil {
		detach()
		client.Kill()
		return fmt.Errorf("it would not say what it needs configured: %w", err)
	}

	i.mu.Lock()
	i.client = client
	i.impl = impl
	i.detach = detach
	i.declared = declared
	i.valueRules = valueRulesFor(declared)
	i.mu.Unlock()

	i.saveDeclared(declared)
	return nil
}

// saveDeclared stores the settings declaration, so the panel can render the
// form for a plugin that is not running. That matters most for the plugin that
// will not start until a credential is filled in: without it the operator would
// have no way to fill it in.
// valueRulesFor is declared with the requirement lifted off every secret.
//
// A secret is never in the plain settings map — it is sealed, and lives in the
// other one — so requiring it of the plain map is a check that can never pass.
// The requirement is real and is enforced against the sealed map instead.
func valueRulesFor(declared []plugin.Setting) []plugin.Setting {
	out := make([]plugin.Setting, len(declared))
	copy(out, declared)
	for i := range out {
		if out[i].Kind == plugin.KindSecret {
			out[i].Required = false
		}
	}
	return out
}

func (i *instance) saveDeclared(declared []plugin.Setting) {
	raw, err := json.Marshal(declared)
	if err != nil {
		raw = []byte("[]")
	}
	if err := i.host.opts.Store.SavePluginDeclaredSettings(i.host.ctx, i.name(), raw); err != nil {
		i.host.opts.Log.LogAttrs(i.host.ctx, slog.LevelWarn, "what a plugin needs configured could not be recorded",
			slog.String("plugin", i.name()), slog.String("reason", err.Error()))
	}
}

// teardown drops the connection to a process that is already gone.
func (i *instance) teardown() {
	i.mu.Lock()
	c, d := i.client, i.detach
	i.client, i.detach = nil, nil
	i.impl = nil
	i.mu.Unlock()
	if c != nil {
		c.Kill()
	}
	if d != nil {
		d()
	}
}

// watch blocks until the process exits or the host asks for it to stop.
func (i *instance) watch() {
	t := time.NewTicker(exitPoll)
	defer t.Stop()

	for {
		select {
		case <-i.done:
			return
		case <-i.host.ctx.Done():
			return
		case <-t.C:
			i.mu.RLock()
			c := i.client
			i.mu.RUnlock()
			if c == nil || c.Exited() {
				return
			}
		}
	}
}

// setState records what the plugin is doing, here and in the database.
func (i *instance) setState(state State, lastError string) {
	i.mu.Lock()
	i.state = state
	i.lastError = lastError
	restarts := i.restarts
	name := i.manifest.Name
	i.mu.Unlock()

	i.host.setState(i.host.ctx, name, state, lastError, i.tail.String(), restarts)
}

// executable checks that there is something to run before go-plugin is asked to
// run it, so that "there is no binary in that directory" is the error rather
// than a handshake that times out.
func executable(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: there is no %s to run", plugin.ErrInvalid, path)
	case err != nil:
		return fmt.Errorf("%w: %s cannot be read: %w", plugin.ErrInvalid, path, err)
	case info.IsDir():
		return fmt.Errorf("%w: %s is a directory, not a program", plugin.ErrInvalid, path)
	}
	// Windows decides what is runnable by extension rather than by a bit, so
	// the check is Unix only. The manifest already put the .exe on the name.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable — it needs the execute bit set", plugin.ErrInvalid, path)
	}
	return nil
}

// marshalCapabilities renders the declaration for the database.
func marshalCapabilities(c plugin.Capabilities) ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("plugins: the capability declaration cannot be stored: %w", err)
	}
	return b, nil
}

// deliver sends one event, on the host's own clock rather than the caller's.
func (i *instance) deliver(e plugin.Event) {
	impl, ok := i.live()
	if !ok {
		return
	}

	values, secrets, err := i.configure(i.host.ctx)
	if err != nil {
		i.host.opts.Log.LogAttrs(i.host.ctx, slog.LevelWarn, "an event was not delivered",
			slog.String("plugin", i.name()),
			slog.String("event", string(e.Kind)),
			slog.String("reason", i.scrub.cleanError(err)))
		return
	}
	e.Settings = values
	e.Secrets = secrets

	ctx, cancel := context.WithTimeout(i.host.ctx, i.host.opts.EventTimeout)
	defer cancel()

	use, err := impl.Notify(ctx, e)
	if err != nil {
		// An event is never retried. One that matters enough to retry is a
		// request, and should be one.
		i.host.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin did not handle an event",
			slog.String("plugin", i.name()),
			slog.String("event", string(e.Kind)),
			slog.String("reason", i.scrub.cleanError(err)))
		return
	}
	i.meter(ctx, use, driverOf(e.Driver.ID))
}

// ask puts a request and waits for it.
func (i *instance) ask(ctx context.Context, r plugin.Request) (plugin.Response, error) {
	impl, ok := i.live()
	if !ok {
		return plugin.Response{}, fmt.Errorf("%w: %s stopped while it was being asked", ErrUnavailable, i.name())
	}

	values, secrets, err := i.configure(ctx)
	if err != nil {
		return plugin.Response{}, err
	}
	r.Settings = values
	r.Secrets = secrets

	answerer, ok := impl.(plugin.Answerer)
	if !ok {
		// The host's side of every connection answers; this is a fake in a
		// test standing in for one, and it does not.
		return plugin.Response{}, fmt.Errorf("%w: %s does not answer requests", plugin.ErrUnsupported, i.name())
	}

	callCtx, cancel := context.WithDeadline(ctx, r.Deadline)
	defer cancel()

	res, err := answerer.Answer(callCtx, r)
	if err != nil {
		reason := i.scrub.cleanError(err)
		if errors.Is(err, context.DeadlineExceeded) {
			// Reported, and nothing more. The plugin is not killed for being
			// slow once — it may be perfectly healthy and the vendor may have
			// been slow — but the caller is told plainly so it can fall back.
			i.host.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin missed its deadline and was skipped",
				slog.String("plugin", i.name()),
				slog.String("request", string(r.Kind)),
				slog.Duration("allowed", time.Until(r.Deadline)))
			return plugin.Response{}, fmt.Errorf("%s did not answer in time: %w", i.name(), err)
		}
		if !errors.Is(err, plugin.ErrNoAnswer) {
			i.host.opts.Log.LogAttrs(ctx, slog.LevelWarn, "a plugin did not answer",
				slog.String("plugin", i.name()),
				slog.String("request", string(r.Kind)),
				slog.String("reason", reason))
		}
		return plugin.Response{}, err
	}

	// A question is not about a driver — its payload is another plugin's and
	// this host does not read it — so what it cost is recorded against the
	// plugin that answered and no driver.
	i.meter(ctx, res.Usage, nil)
	return res, nil
}

// live is the plugin's client, if it is running.
func (i *instance) live() (plugin.Plugin, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.state != StateRunning || i.impl == nil {
		return nil, false
	}
	return i.impl, true
}

// configure reads the operator's settings for this plugin and opens its sealed
// credentials.
//
// The credentials are opened here, at call time, and handed over for the length
// of the call. Nothing about them is cached on the instance: an operator who
// changes a key in the panel changes it for the next call, and a plugin that
// has been stopped is holding nothing.
//
// It returns [plugin.ErrNotConfigured] alongside usable values when a required
// setting is empty, rather than instead of them. A caller that must refuse an
// unconfigured plugin checks the error; one that serves a page uses the values
// and lets the plugin decide.
func (i *instance) configure(ctx context.Context) (plugin.Values, plugin.Secrets, error) {
	i.mu.RLock()
	declared := i.declared
	valueRules := i.valueRules
	name := i.manifest.Name
	i.mu.RUnlock()

	rows, err := i.host.opts.Store.PluginSettings(ctx, name)
	if err != nil {
		return nil, nil, err
	}

	stored := make(plugin.Values, len(rows))
	sealed := make(map[string][]byte, len(rows))
	for _, row := range rows {
		if len(row.Sealed) > 0 {
			sealed[row.Name] = row.Sealed
			continue
		}
		stored[row.Name] = row.Value
	}

	// A plugin that is not finished being set up still gets what is set.
	//
	// The error is returned either way, and Answer and Notify still refuse on
	// it — a cue cannot be written without a key. But serving a page is a
	// weaker promise, and the caller that tolerates this needs the settings
	// that are present: the page an operator uses to finish the setup is
	// usually one that has to render what is already there.
	values, err := plugin.ValidateValues(valueRules, stored)
	notConfigured := errors.Is(err, plugin.ErrNotConfigured)
	if err != nil && !notConfigured {
		return nil, nil, err
	}
	if notConfigured {
		optional := make([]plugin.Setting, len(valueRules))
		copy(optional, valueRules)
		for k := range optional {
			optional[k].Required = false
		}
		// It validated a moment ago bar the required check, so this cannot
		// fail for a new reason; if it somehow does, the caller has the first
		// error and no values, which is what it had before.
		values, _ = plugin.ValidateValues(optional, stored)
	}

	secrets := make(plugin.Secrets)
	key := i.host.opts.Keyring.Key()
	for i := range declared {
		s := &declared[i]
		if s.Kind != plugin.KindSecret {
			continue
		}
		raw, ok := sealed[s.Name]
		if !ok || len(raw) == 0 {
			// Recorded rather than returned, so that the settings resolved
			// above survive. A caller that must refuse checks the error; the
			// one that serves a page needs what is set, and a page rendering
			// nothing is how an operator loses what they had already entered.
			if s.Required && err == nil {
				err = fmt.Errorf("%w: %s has to be filled in", plugin.ErrNotConfigured, s.Label)
			}
			continue
		}
		opened, openErr := key.Open(raw)
		if openErr != nil {
			// The data directory was lost while the database survived, or the
			// operator regenerated the data key. The feature is off until they
			// enter the credential again, and saying so is better than a call
			// that fails at the vendor.
			if err == nil {
				err = fmt.Errorf("%w: %s is stored but cannot be read — enter it again", plugin.ErrNotConfigured, s.Label)
			}
			continue
		}
		secrets[s.Name] = plugin.NewSecret(opened)
	}

	// Everything the plugin is about to be lent is learned by the scrubber
	// first, so that whatever it says next cannot carry one back.
	i.scrub.learn(secrets)
	return values, secrets, err
}

// meter records what a call cost and nothing else. The decision about whether
// there is budget left was taken before the call; this is the bookkeeping that
// makes the next such decision true.
func (i *instance) meter(ctx context.Context, use plugin.Usage, driverID *int64) {
	if !use.Spent() {
		return
	}
	err := i.host.opts.Store.RecordPluginUsage(ctx, db.PluginUsageWrite{
		Plugin:   i.name(),
		Job:      use.Job,
		Model:    use.Model,
		DriverID: driverID,
		Input:    use.InputTokens,
		Output:   use.OutputTokens,
		Cached:   use.Cached,
	})
	if err != nil {
		// A call that is not recorded is a call the operator cannot see and
		// the cap cannot count, so this is worth a line even though there is
		// nothing to do about it here.
		i.host.opts.Log.LogAttrs(ctx, slog.LevelError, "what a plugin spent could not be recorded",
			slog.String("plugin", i.name()),
			slog.String("job", use.Job),
			slog.Int64("tokens", use.Total()),
			slog.String("reason", err.Error()))
	}
}

// driverOf is the driver a call was on behalf of, or nil for work that was not
// on any one driver's.
func driverOf(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	return &id
}

// serves reports whether this plugin asked for a route.
func (i *instance) serves() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.Capabilities.Serves()
}

// access is what this plugin requires of a caller at this path, and whether it
// serves the path at all.
func (i *instance) access(path string) (plugin.Access, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.manifest.Capabilities.HTTP == nil {
		return "", false
	}
	return i.manifest.Capabilities.HTTP.For(path)
}
