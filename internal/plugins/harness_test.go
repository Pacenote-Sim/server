package plugins_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/auth"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/plugins"
)

// The host's tests run real plugin processes. They have to: what is being
// tested is that a separate process crashing, hanging or lying does not take
// the server with it, and a fake plugin inside the host's own memory proves
// only that the host agrees with itself.
//
// The plugin they run is github.com/pacenote-sim/plugin/examples/testplugin, built once here
// and copied into a directory per test. It exists for exactly this.

// testPluginBinary is the example plugin, built once for the whole package.
var testPluginBinary string

// TestMain builds the example plugin and fails the package if a test leaves a
// goroutine behind. The host runs one supervisor goroutine per plugin and one
// per dispatched event, and the shutdown path is the thing most likely to
// strand one.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// go-plugin reaps its subprocesses from a goroutine it starts for the
		// life of the process, and gRPC's connection bookkeeping outlives a
		// Close by a few scheduler turns. Both are the libraries' own.
		goleak.IgnoreAnyFunction("github.com/hashicorp/go-plugin.(*Client).logStderr"),
		goleak.IgnoreAnyFunction("google.golang.org/grpc.(*ClientConn).WithStateChangeHandler"),
		goleak.IgnoreAnyFunction("google.golang.org/grpc/internal/grpcsync.(*CallbackSerializer).run"),
		goleak.IgnoreAnyFunction("google.golang.org/grpc/internal/transport.(*controlBuffer).get"),
	)
}

// buildTestPlugin compiles the example plugin once and hands back the path.
func buildTestPlugin(tb testing.TB) string {
	tb.Helper()
	buildOnce.Do(func() { testPluginBinary, buildErr = compileTestPlugin() })
	if buildErr != nil {
		tb.Fatalf("the example plugin could not be built: %v", buildErr)
	}
	return testPluginBinary
}

var (
	buildOnce sync.Once
	buildErr  error
)

func compileTestPlugin() (string, error) {
	dir, err := os.MkdirTemp("", "pacenote-testplugin")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "testplugin")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "github.com/pacenote-sim/plugin/examples/testplugin")
	if combined, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w: %s", err, combined)
	}
	return out, nil
}

// behaviour is the example plugin's misbehaviour file, spelled the way it
// reads it.
type behaviour struct {
	CrashAt    string `json:"crash_at,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Hang       string `json:"hang,omitempty"`
	LeakSecret bool   `json:"leak_secret,omitempty"`
	// LeakDatabase makes it print its own connection string, password and all.
	LeakDatabase bool   `json:"leak_database,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
	Spend        int64  `json:"spend,omitempty"`
}

// manifestFor is a manifest that declares everything, which is what the tests
// want unless they are testing a declaration.
func manifestFor(name string) plugin.Manifest {
	return plugin.Manifest{
		Name:             name,
		Version:          "1.0.0",
		Author:           "Pacenote",
		Description:      "Exercises every part of the plugin contract and does nothing useful.",
		InterfaceVersion: plugin.InterfaceVersion,
		Binary:           "testplugin",
		Capabilities: plugin.Capabilities{
			Events:          []plugin.EventKind{plugin.EventLapCompleted, plugin.EventStintFinished},
			Requests:        kindsOf(name),
			Asks:            asksFor(name),
			ReadsDriverData: true,
		},
	}
}

// install puts the example plugin in a directory of its own under root, with
// the manifest given.
func install(tb testing.TB, root, name string, m plugin.Manifest) string {
	tb.Helper()
	r := require.New(tb)

	dir := filepath.Join(root, name)
	r.NoError(os.MkdirAll(dir, 0o700))

	binary := filepath.Join(dir, "testplugin")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	src, err := os.ReadFile(buildTestPlugin(tb))
	r.NoError(err)
	r.NoError(os.WriteFile(binary, src, 0o700))

	raw, err := json.Marshal(m)
	r.NoError(err)
	r.NoError(os.WriteFile(filepath.Join(dir, plugin.ManifestName), raw, 0o600))
	return dir
}

// installRaw puts an arbitrary manifest in a directory, for the cases where the
// manifest is the thing under test and cannot be built from the struct.
func installRaw(tb testing.TB, root, name, manifest string) {
	tb.Helper()
	r := require.New(tb)

	dir := filepath.Join(root, name)
	r.NoError(os.MkdirAll(dir, 0o700))
	r.NoError(os.WriteFile(filepath.Join(dir, plugin.ManifestName), []byte(manifest), 0o600))
}

// misbehave writes the example plugin's behaviour file.
func misbehave(tb testing.TB, dir string, b behaviour) {
	tb.Helper()
	r := require.New(tb)

	raw, err := json.Marshal(b)
	r.NoError(err)
	r.NoError(os.WriteFile(filepath.Join(dir, "behaviour.json"), raw, 0o600))
}

// behaveNormally removes the behaviour file, so the next call is honest.
func behaveNormally(tb testing.TB, dir string) {
	tb.Helper()
	require.NoError(tb, os.Remove(filepath.Join(dir, "behaviour.json")))
}

// logs is a race-safe place for the host's own lines, so a test can assert what
// did and did not reach them.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// fakeStore is the host's database, in memory. The host's failure paths have to
// be testable on a machine with no PostgreSQL on it, which is what the Store
// interface is for.
type fakeStore struct {
	mu       sync.Mutex
	records  map[string]db.PluginRecord
	settings map[string][]db.PluginSettingRow
	usage    []db.PluginUsageWrite
	spent    int64

	// failSettings makes reading a plugin's settings fail, for the path where
	// the database is unreachable at call time.
	failSettings error
	// The writes that can fail while a plugin is being recorded. Each is its
	// own field so a test names the write it is about.
	failSavePlugin   error
	failSaveEnabled  error
	failPlugins      error
	failReadPlugin   error
	failSaveDeclared error
	failTokens       error

	// The plugin-database half. There is no PostgreSQL here, so what is
	// recorded is the sequence: which plugins were provisioned, with which
	// password, and whether anything dropped them. A test that wants the real
	// statements runs against a real server — see the postgres-tagged tests in
	// internal/db.
	databases     map[string]db.PluginDatabaseRow
	provisioned   []string
	dropped       []string
	dsnFor        map[string]string
	failProvision error
	// The other ways the database half fails, each injectable on its own so a
	// test names the failure it is about.
	failReadDatabase      error
	failRecordProvisioned error
	failDSN               error
	failDrop              error
	failClear             error
	failDelete            error
	// dsnOverride replaces what PluginDSN returns, for the paths that need a
	// connection string that will not connect.
	dsnOverride string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		records:   make(map[string]db.PluginRecord),
		settings:  make(map[string][]db.PluginSettingRow),
		databases: make(map[string]db.PluginDatabaseRow),
		dsnFor:    make(map[string]string),
	}
}

func (f *fakeStore) PluginDatabase(_ context.Context, name string) (db.PluginDatabaseRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failReadDatabase != nil {
		return db.PluginDatabaseRow{}, f.failReadDatabase
	}
	row, ok := f.databases[name]
	if !ok {
		return db.PluginDatabaseRow{}, db.ErrNotFound
	}
	return row, nil
}

func (f *fakeStore) SetPluginProvisioned(_ context.Context, name string, sealed []byte, migration string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRecordProvisioned != nil {
		return f.failRecordProvisioned
	}
	f.databases[name] = db.PluginDatabaseRow{
		PasswordSealed: sealed,
		Provisioned:    true,
		Migration:      migration,
	}
	return nil
}

func (f *fakeStore) DeletePlugin(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete != nil {
		return f.failDelete
	}
	delete(f.records, name)
	delete(f.settings, name)
	return nil
}

func (f *fakeStore) ClearPluginDatabase(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClear != nil {
		return f.failClear
	}
	delete(f.databases, name)
	return nil
}

func (f *fakeStore) ProvisionPluginDatabase(_ context.Context, name, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failProvision != nil {
		return f.failProvision
	}
	f.provisioned = append(f.provisioned, name)
	// A fake DSN that still looks like one, carrying the password, so a test
	// can assert the plugin was handed it and the scrubber removed it.
	f.dsnFor[name] = "postgres://plugin_" + name + ":" + password + "@localhost:5432/pacenote?sslmode=disable"
	return nil
}

func (f *fakeStore) DropPluginDatabase(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDrop != nil {
		return f.failDrop
	}
	f.dropped = append(f.dropped, name)
	delete(f.dsnFor, name)
	return nil
}

func (f *fakeStore) PluginDSN(name, password string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDSN != nil {
		return "", f.failDSN
	}
	if f.dsnOverride != "" {
		return f.dsnOverride, nil
	}
	if dsn, ok := f.dsnFor[name]; ok {
		return dsn, nil
	}
	return "postgres://plugin_" + name + ":" + password + "@localhost:5432/pacenote?sslmode=disable", nil
}

func (f *fakeStore) PluginTokensSinceFor(_ context.Context, name string, _ time.Time) (db.TokenUse, error) {
	if err := f.tokensError(); err != nil {
		return db.TokenUse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out db.TokenUse
	for _, u := range f.usage {
		if u.Plugin != name {
			continue
		}
		out.Calls++
		out.Input += u.Input
		out.Output += u.Output
	}
	// A test that set a figure directly means it for whichever plugin it is
	// asking about.
	if f.spent > 0 {
		out.Input = f.spent
		out.Output = 0
	}
	return out, nil
}

// tokensError is the injected failure for reading what has been spent.
func (f *fakeStore) tokensError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failTokens
}

// put writes a record straight in, for the rows a test needs to be shaped
// differently from anything the host would write — an upgrade from an older
// version, usually.
func (f *fakeStore) put(rec db.PluginRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[rec.Name] = rec
}

// provisionedNames is what was provisioned, in order.
func (f *fakeStore) provisionedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.provisioned...)
}

// droppedNames is what was deprovisioned, in order.
func (f *fakeStore) droppedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dropped...)
}

func (f *fakeStore) SavePlugin(_ context.Context, p db.PluginRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSavePlugin != nil {
		return f.failSavePlugin
	}
	existing, ok := f.records[p.Name]
	if ok {
		// Keep what only state changes write, the way the upsert does.
		p.LastError = existing.LastError
		p.LastOutput = existing.LastOutput
		p.Restarts = existing.Restarts
		p.DeclaredSettings = existing.DeclaredSettings
		// And what only the operator writes. UpsertPlugin does not name
		// enabled in its DO UPDATE, so rediscovering a plugin must not turn a
		// disabled one back on — this fake has to have the same property or
		// every test of it would be testing the fake.
		p.Enabled = existing.Enabled
	} else {
		// The column defaults to true: a plugin found on disk runs unless the
		// operator has said otherwise.
		p.Enabled = true
	}
	f.records[p.Name] = p
	return nil
}

func (f *fakeStore) SavePluginDeclaredSettings(_ context.Context, name string, declared []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSaveDeclared != nil {
		return f.failSaveDeclared
	}
	r := f.records[name]
	r.Name = name
	r.DeclaredSettings = declared
	f.records[name] = r
	return nil
}

func (f *fakeStore) SavePluginState(_ context.Context, name, state, lastError, lastOutput string, restarts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.records[name]
	r.Name = name
	r.State = state
	r.LastError = lastError
	r.LastOutput = lastOutput
	r.Restarts = restarts
	f.records[name] = r
	return nil
}

func (f *fakeStore) Plugins(context.Context) ([]db.PluginRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPlugins != nil {
		return nil, f.failPlugins
	}
	out := make([]db.PluginRecord, 0, len(f.records))
	for name := range f.records {
		out = append(out, f.records[name])
	}
	return out, nil
}

func (f *fakeStore) Plugin(_ context.Context, name string) (db.PluginRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failReadPlugin != nil {
		return db.PluginRecord{}, f.failReadPlugin
	}
	rec, ok := f.records[name]
	if !ok {
		return db.PluginRecord{}, db.ErrNotFound
	}
	return rec, nil
}

func (f *fakeStore) SavePluginEnabled(_ context.Context, name string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSaveEnabled != nil {
		return f.failSaveEnabled
	}
	rec, ok := f.records[name]
	if !ok {
		return db.ErrNotFound
	}
	rec.Enabled = enabled
	f.records[name] = rec
	return nil
}

func (f *fakeStore) PluginSettings(_ context.Context, name string) ([]db.PluginSettingRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSettings != nil {
		return nil, f.failSettings
	}
	return append([]db.PluginSettingRow(nil), f.settings[name]...), nil
}

func (f *fakeStore) RecordPluginUsage(_ context.Context, u db.PluginUsageWrite) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, u)
	f.spent += u.Input + u.Output
	return nil
}

func (f *fakeStore) PluginTokensSince(context.Context, time.Time) (db.TokenUse, error) {
	if err := f.tokensError(); err != nil {
		return db.TokenUse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return db.TokenUse{Input: f.spent}, nil
}

// testPluginName is the plugin every test configures. Only one of them is ever
// configured — the broken ones in these tests never get as far as reading a
// setting — so the helpers below name it rather than taking it as an argument
// nobody varies.
const testPluginName = "testplugin"

// setValue stores a plain setting the way the panel would.
// SavePluginSetting is how the host writes the settings a plugin does not own,
// which today is its daily cap.
func (f *fakeStore) SavePluginSetting(_ context.Context, name, setting, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.settings[name]
	for i := range rows {
		if rows[i].Name == setting {
			rows[i] = db.PluginSettingRow{Name: setting, Value: value}
			f.settings[name] = rows
			return nil
		}
	}
	f.settings[name] = append(rows, db.PluginSettingRow{Name: setting, Value: value})
	return nil
}

func (f *fakeStore) setValue(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[testPluginName] = append(f.settings[testPluginName], db.PluginSettingRow{Name: name, Value: value})
}

// setValueFor stores a plain setting for a plugin installed under another name.
func (f *fakeStore) setValueFor(pluginName, name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[pluginName] = append(f.settings[pluginName], db.PluginSettingRow{Name: name, Value: value})
}

// setSecret stores a sealed credential the way the panel would.
func (f *fakeStore) setSecret(tb testing.TB, key auth.SecretKey, name, value string) {
	tb.Helper()
	sealed, err := key.Seal(value)
	require.NoError(tb, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[testPluginName] = append(f.settings[testPluginName], db.PluginSettingRow{Name: name, Sealed: sealed})
}

func (f *fakeStore) record(name string) db.PluginRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[name]
}

func (f *fakeStore) calls() []db.PluginUsageWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.PluginUsageWrite(nil), f.usage...)
}

func (f *fakeStore) setSpent(n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spent = n
}

// harness is a host with a plugin directory, a fake database and a log a test
// can read.
type harness struct {
	host  *plugins.Host
	dir   string
	store *fakeStore
	logs  *logs
	key   auth.SecretKey
}

// newHarness builds a host over an empty plugin directory. The timings are
// small on purpose: a test that waits out the production backoff is a test
// nobody runs.
func newHarness(tb testing.TB, opts ...func(*plugins.Options)) *harness {
	tb.Helper()
	r := require.New(tb)

	key, err := auth.NewSecretKey()
	r.NoError(err)

	out := &logs{}
	store := newFakeStore()
	dir := filepath.Join(tb.TempDir(), "plugins")

	options := plugins.Options{
		Dir:            dir,
		Log:            slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Store:          store,
		Keyring:        auth.NewKeyring(key),
		CallTimeout:    3 * time.Second,
		EventTimeout:   3 * time.Second,
		StartTimeout:   20 * time.Second,
		MaxRestarts:    2,
		RestartBackoff: 10 * time.Millisecond,
	}
	for _, o := range opts {
		o(&options)
	}

	host, err := plugins.New(options)
	r.NoError(err)
	tb.Cleanup(host.Close)

	return &harness{host: host, dir: dir, store: store, logs: out, key: key}
}

// eventually waits for a condition, because a plugin is a process and nothing
// about it is synchronous.
func eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for %s", what)
}

// stateOf is one plugin's state as the host sees it, or empty when the host
// does not have it.
func stateOf(h *plugins.Host, name string) plugins.State {
	list := h.List()
	for i := range list {
		if list[i].Name == name {
			return list[i].State
		}
	}
	return ""
}

// statusOf is one plugin's whole status.
func statusOf(tb testing.TB, h *plugins.Host, name string) plugins.Status {
	tb.Helper()
	list := h.List()
	for i := range list {
		if list[i].Name == name {
			return list[i]
		}
	}
	tb.Fatalf("the host is not supervising %q", name)
	return plugins.Status{}
}

// kindsOf is what the example plugin answers when installed under a name: its
// kinds are spelled with that name, as the contract requires.
func kindsOf(name string) []plugin.RequestKind {
	return []plugin.RequestKind{
		plugin.RequestKind(name + ".echo"),
		plugin.RequestKind(name + ".ask"),
		plugin.RequestKind(name + ".chain"),
	}
}

// asksFor is the plugins a test installation may ask: every name these tests
// install under, minus its own, because a plugin may not declare that it asks
// itself.
func asksFor(name string) []string {
	var out []string
	for _, other := range []string{"testplugin", "otherplugin", "alpha", "beta"} {
		if other != name {
			out = append(out, other)
		}
	}
	return out
}

// lapEvent is a plausible lap.completed, so that no test has to build one.
func lapEvent(driver string) plugin.Event {
	return plugin.Event{
		ID:      "e-" + driver,
		Kind:    plugin.EventLapCompleted,
		At:      time.Now(),
		Driver:  plugin.Driver{ID: 7, Slug: driver, Name: "Ana"},
		Session: plugin.Session{StintID: "s-1", Sim: "iracing", TrackID: "barcelona gp", Car: "296", Type: plugin.SessionRace},
		Lap: &plugin.LapFacts{
			Number: 14, LapMs: 91240, Kind: plugin.LapClean, DeltaMs: 840,
			Reference: "your best lap of this stint",
			Corners: json.RawMessage(`[{"turn":4,"apex_pct":312,"apex_kmh":112,"ref_apex_kmh":121,` +
				`"deficit_kmh":9,"brake_at_apex":31,"throttle_lag":14,"pattern":"early_apex"}]`),
			SpokenLap: "one minute 31.2 seconds",
		},
	}
}

// echoRequest is a question the example plugin answers by echoing it, put to a
// plugin installed under name. From is the test's own word for itself: this
// is the host asking directly, the way the broker does on a plugin's behalf.
func echoRequest(name string) plugin.Request {
	return plugin.Request{
		ID:      "r-1",
		Kind:    plugin.RequestKind(name + ".echo"),
		From:    "test",
		Payload: json.RawMessage(`{"turn":4}`),
	}
}

// removeAll deletes a plugin's directory, for the case where an operator does.
func removeAll(root, name string) error { return os.RemoveAll(filepath.Join(root, name)) }
