package plugins

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
)

// The parts of this package that are worth testing directly rather than through
// a plugin process: the option validation, the backoff arithmetic, the check
// that a binary is runnable, and the scrubber. Every one of them is a pure
// function of its input, and running a subprocess to reach it would say less
// about it and take a thousand times longer.

// nullStore is a Store that does nothing, for the cases where a host has to
// exist and will never be asked anything.
type nullStore struct{}

func (nullStore) SavePlugin(context.Context, db.PluginRecord) error                { return nil }
func (nullStore) SavePluginDeclaredSettings(context.Context, string, []byte) error { return nil }
func (nullStore) SavePluginState(context.Context, string, string, string, string, int) error {
	return nil
}
func (nullStore) Plugins(context.Context) ([]db.PluginRecord, error) { return nil, nil }
func (nullStore) Plugin(context.Context, string) (db.PluginRecord, error) {
	return db.PluginRecord{}, db.ErrNotFound
}
func (nullStore) SavePluginEnabled(context.Context, string, bool) error { return nil }
func (nullStore) PluginSettings(context.Context, string) ([]db.PluginSettingRow, error) {
	return nil, nil
}
func (nullStore) RecordPluginUsage(context.Context, db.PluginUsageWrite) error { return nil }
func (nullStore) PluginTokensSince(context.Context, time.Time) (db.TokenUse, error) {
	return db.TokenUse{}, nil
}

func (nullStore) PluginTokensSinceFor(context.Context, string, time.Time) (db.TokenUse, error) {
	return db.TokenUse{}, nil
}
func (nullStore) SavePluginSetting(context.Context, string, string, string) error { return nil }
func (nullStore) PluginDatabase(context.Context, string) (db.PluginDatabaseRow, error) {
	return db.PluginDatabaseRow{}, db.ErrNotFound
}
func (nullStore) SetPluginProvisioned(context.Context, string, []byte, string) error { return nil }
func (nullStore) DeletePlugin(context.Context, string) error                         { return nil }
func (nullStore) ClearPluginDatabase(context.Context, string) error                  { return nil }
func (nullStore) ProvisionPluginDatabase(context.Context, string, string) error      { return nil }
func (nullStore) DropPluginDatabase(context.Context, string) error                   { return nil }
func (nullStore) PluginDSN(string, string) (string, error)                           { return "", nil }

func TestNewRefusesWhatItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	_, err := New(Options{Store: nullStore{}})
	r.Error(err)
	r.Contains(err.Error(), "no plugin directory")

	_, err = New(Options{Dir: t.TempDir()})
	r.Error(err)
	r.Contains(err.Error(), "no store")
}

// Every default exists so that a caller may give only what it cares about. A
// default that silently became zero would be a timeout of zero, which is a
// plugin that is never given a chance to answer.
func TestNewFillsInEveryDefault(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h, err := New(Options{Dir: t.TempDir(), Store: nullStore{}})
	r.NoError(err)
	t.Cleanup(h.Close)

	r.NotNil(h.opts.Log)
	r.NotNil(h.opts.Now)
	r.Equal(DefaultCallTimeout, h.opts.CallTimeout)
	r.Equal(DefaultEventTimeout, h.opts.EventTimeout)
	r.Equal(DefaultStartTimeout, h.opts.StartTimeout)
	r.Equal(DefaultMaxRestarts, h.opts.MaxRestarts)
	r.Equal(DefaultRestartBackoff, h.opts.RestartBackoff)

	// Negative durations are as wrong as zero and take the same path.
	h2, err := New(Options{
		Dir: t.TempDir(), Store: nullStore{},
		CallTimeout: -time.Second, EventTimeout: -time.Second, StartTimeout: -time.Second,
		MaxRestarts: -1, RestartBackoff: -time.Second,
	})
	r.NoError(err)
	t.Cleanup(h2.Close)
	r.Equal(DefaultCallTimeout, h2.opts.CallTimeout)
	r.Equal(DefaultMaxRestarts, h2.opts.MaxRestarts)
}

// The host's interface version is the plugin module's, not a number copied into
// a second place that can drift from it.
func TestInterfaceVersionIsTheContracts(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Dir: t.TempDir(), Store: nullStore{}})
	require.NoError(t, err)
	t.Cleanup(h.Close)
	require.Equal(t, plugin.InterfaceVersion, h.InterfaceVersion())
}

// The backoff doubles and then stops. The ceiling matters more than the
// doubling: without it a plugin that has been failing all night would be
// retried at intervals measured in days.
func TestBackoffDoublesUpToTheCeiling(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	const initial = 500 * time.Millisecond
	r.Equal(initial, backoffFor(initial, 1))
	r.Equal(2*initial, backoffFor(initial, 2))
	r.Equal(4*initial, backoffFor(initial, 3))
	r.Equal(MaxRestartBackoff, backoffFor(initial, 20))

	// An initial wait already past the ceiling is capped rather than returned.
	r.Equal(MaxRestartBackoff, backoffFor(2*MaxRestartBackoff, 2))
}

func TestExecutableSaysWhyABinaryWillNotRun(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()

	err := executable(filepath.Join(dir, "missing"))
	r.ErrorIs(err, plugin.ErrInvalid)
	r.Contains(err.Error(), "there is no")

	sub := filepath.Join(dir, "adirectory")
	r.NoError(os.MkdirAll(sub, 0o700))
	err = executable(sub)
	r.ErrorIs(err, plugin.ErrInvalid)
	r.Contains(err.Error(), "is a directory")

	runnable := filepath.Join(dir, "runnable")
	r.NoError(os.WriteFile(runnable, []byte("#!/bin/sh\n"), 0o700))
	r.NoError(executable(runnable))

	if runtime.GOOS == "windows" {
		t.Skip("Windows decides what is runnable by extension, so there is no execute bit to clear")
	}
	plain := filepath.Join(dir, "notrunnable")
	r.NoError(os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o600))
	err = executable(plain)
	r.ErrorIs(err, plugin.ErrInvalid)
	r.Contains(err.Error(), "not executable")
}

func TestScrubberReplacesEveryCredentialItHasLearned(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := &scrubber{}
	r.Empty(s.clean(""), "an empty string is left alone rather than scanned")
	r.Equal("nothing to hide", s.clean("nothing to hide"))
	r.Empty(s.cleanError(nil))

	s.learnValues("sk-ant-thekeynobodymayeversee", "another-long-credential-here")
	// Too short to be a credential, and scrubbing it would redact ordinary
	// words out of everything a plugin ever prints.
	s.learnValues("abc")

	r.Equal("key "+plugin.Redacted+" and "+plugin.Redacted,
		s.clean("key sk-ant-thekeynobodymayeversee and another-long-credential-here"))
	r.Equal("abc is left", s.clean("abc is left"))
	r.Equal("failed with "+plugin.Redacted,
		s.cleanError(errors.New("failed with sk-ant-thekeynobodymayeversee")))

	// Learning the same value twice does not add it twice, and learning nothing
	// is not an error.
	s.learnValues("sk-ant-thekeynobodymayeversee")
	s.learnValues()
	s.learn(nil)
	r.Len(s.values, 2)
}

// A secret learned through the settings path is scrubbed the same as one the
// host generated.
func TestScrubberLearnsFromSecrets(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := &scrubber{}
	s.learn(plugin.Secrets{"api_key": plugin.NewSecret("sk-ant-thekeynobodymayeversee")})
	r.Equal("it said "+plugin.Redacted, s.clean("it said sk-ant-thekeynobodymayeversee"))
}

func TestMigrationFilesIgnoresEverythingThatIsNotSQL(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A directory that is not there is a plugin that ships no schema, which is
	// allowed and is not an error.
	files, err := migrationFiles(filepath.Join(t.TempDir(), "nothinghere"))
	r.NoError(err)
	r.Empty(files)

	dir := t.TempDir()
	for _, name := range []string{"00002_second.sql", "00001_first.sql", "README.md", "notes.txt"} {
		r.NoError(os.WriteFile(filepath.Join(dir, name), []byte("-- x"), 0o600))
	}
	r.NoError(os.MkdirAll(filepath.Join(dir, "adirectory.sql"), 0o700))

	files, err = migrationFiles(dir)
	r.NoError(err)
	r.Equal([]string{"00001_first.sql", "00002_second.sql"}, files,
		"migrations are the .sql files, in the order their names sort")
}

func TestNewPasswordIsLongAndDifferentEveryTime(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	seen := make(map[string]bool, 64)
	for range 64 {
		p, err := newPassword()
		r.NoError(err)
		r.Len(p, passwordBytes*2, "a hex password is two characters a byte")
		r.False(seen[p], "the same password was generated twice")
		seen[p] = true
	}
}

// A host with no keyring at all, rather than one holding no key. Both are a
// server that cannot seal, and neither may be a panic.
func TestProvisionWithoutAKeyringIsRefusedRatherThanFatal(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	h, err := New(Options{
		Dir:   t.TempDir(),
		Store: nullStore{},
		Log:   slog.New(slog.DiscardHandler),
	})
	r.NoError(err)
	t.Cleanup(h.Close)

	m := plugin.Manifest{Name: "needsdb", Capabilities: plugin.Capabilities{Database: true}}
	_, _, err = h.provision(t.Context(), m, t.TempDir())
	r.ErrorIs(err, ErrNoDataKey)

	// And a plugin that asked for nothing gets nothing, with no keyring needed.
	dsn, password, err := h.provision(t.Context(), plugin.Manifest{Name: "plain"}, t.TempDir())
	r.NoError(err)
	r.Empty(dsn)
	r.Empty(password)
}
