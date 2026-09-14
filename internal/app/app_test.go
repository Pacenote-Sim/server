package app_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/pacenote-sim/server/internal/app"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/logging"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// runUntilListening starts the server and returns the mode it settled in and
// the address it bound, then stops it. It is how the startup decision is
// tested: what the server chooses to be is the whole question.
func runUntilListening(t *testing.T, opts app.Options) (app.Mode, string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listening := make(chan struct{})
	var mode app.Mode
	var addr string
	var once bool
	opts.Log = logging.Discard()
	opts.OnListen = func(m app.Mode, a string) {
		if once {
			return
		}
		once = true
		mode, addr = m, a
		close(listening)
	}

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, opts) }()

	select {
	case err := <-done:
		return "", "", err
	case <-listening:
	}
	cancel()
	select {
	case err := <-done:
		return mode, addr, err
	case <-time.After(20 * time.Second):
		require.Fail(t, "the server did not stop")
		return mode, addr, nil
	}
}

// baseOptions is a server told to bind ephemeral ports on loopback, so a test
// cannot collide with a real one or with another test.
func baseOptions(t *testing.T, dataDir string) app.Options {
	t.Helper()
	return app.Options{
		DataDir:       dataDir,
		Version:       "v0.0.0-test",
		Terminal:      &strings.Builder{},
		Listen:        "127.0.0.1:0",
		MetricsListen: "127.0.0.1:0",
	}
}

func writeConfig(t *testing.T, dir, databaseURL string) {
	t.Helper()
	require.NoError(t, config.Config{
		DatabaseURL:   databaseURL,
		Listen:        "127.0.0.1:0",
		MetricsListen: "127.0.0.1:0",
	}.Save(dir))
}

func TestNothingConfiguredRunsTheWizard(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A directory that does not exist yet: a genuine first run.
	dir := filepath.Join(t.TempDir(), "pacenote-data")

	mode, addr, err := runUntilListening(t, baseOptions(t, dir))
	r.NoError(err)
	r.Equal(app.ModeSetup, mode)
	r.NotEmpty(addr)
}

func TestTheTokenIsPrintedToTheTerminalAndNotToTheLog(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := filepath.Join(t.TempDir(), "pacenote-data")
	var terminal strings.Builder
	opts := baseOptions(t, dir)
	opts.Terminal = &terminal

	mode, _, err := runUntilListening(t, opts)
	r.NoError(err)
	r.Equal(app.ModeSetup, mode)

	printed := terminal.String()
	r.Contains(printed, "first run")
	r.Contains(printed, "/setup")
	r.Contains(printed, "Token ")
	r.Contains(printed, "this terminal only, once")
}

func TestADataDirectoryWithNoConnectionStringDoesNotReopenTheWizard(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// The directory is there, so this server was configured once. The
	// connection string has gone, and the wizard must not step in: the
	// database it used may already hold an administrator.
	dir := t.TempDir()
	r.NoError(config.EnsureDir(dir))

	_, _, err := runUntilListening(t, baseOptions(t, dir))
	r.Error(err)
	var notConfigured *app.NotConfiguredError
	r.ErrorAs(err, &notConfigured)
	r.Contains(err.Error(), config.EnvDatabaseURL)
	r.Contains(err.Error(), "will not run again")
}

func TestAnUnreachableDatabaseIsAHardStop(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := filepath.Join(t.TempDir(), "pacenote-data")
	writeConfig(t, dir, "postgres://pacenote:secret@127.0.0.1:1/pacenote?connect_timeout=2")

	_, _, err := runUntilListening(t, baseOptions(t, dir))
	r.Error(err)
	r.Contains(err.Error(), "Nothing answered")
	r.Contains(err.Error(), "will not run instead",
		"a server that cannot reach its database must not offer to be set up again")
	r.NotContains(err.Error(), "secret")
}

func TestAMetricsPortThatIsNotLoopbackIsRefused(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := filepath.Join(t.TempDir(), "pacenote-data")
	r.NoError(config.Config{Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:0"}.Save(dir))
	opts := baseOptions(t, dir)
	opts.MetricsListen = "0.0.0.0:9090"

	_, _, err := runUntilListening(t, opts)
	r.Error(err)
	r.Contains(err.Error(), "127.0.0.1")
}

func TestDataDirIsCreatedTight(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	r.NoError(config.EnsureDir(dir))
	info, err := os.Stat(dir)
	r.NoError(err)
	r.Equal(config.DirMode, info.Mode().Perm())
}

// A server told to bind a port somebody else already has. Both listeners are
// named separately because they are two different things for an operator to go
// and look at.
func TestAPortThatIsAlreadyTaken(t *testing.T) {
	t.Parallel()

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })

	t.Run("the public port", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		opts := baseOptions(t, filepath.Join(t.TempDir(), "pacenote-data"))
		opts.Listen = taken.Addr().String()
		_, _, err := runUntilListening(t, opts)
		r.Error(err)
		r.Contains(err.Error(), "could not be opened")
		r.Contains(err.Error(), opts.Listen, "the sentence does not say which address")
	})

	t.Run("the metrics port", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		opts := baseOptions(t, filepath.Join(t.TempDir(), "pacenote-data"))
		opts.MetricsListen = taken.Addr().String()
		_, _, err := runUntilListening(t, opts)
		r.Error(err)
		r.Contains(err.Error(), "metrics port could not be opened")
	})
}

// A server given nothing but somewhere to listen. Every other option has a
// default, because the supported way to run this is one binary with no
// arguments.
func TestTheDefaultsAreEnoughToStart(t *testing.T) {
	// Not parallel: it sets process environment variables.
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	t.Setenv(config.EnvDataDir, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listening := make(chan struct{})
	var once sync.Once
	var mode app.Mode
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Options{
			// No log, no terminal, no version, no data directory: each one is
			// filled in rather than being a reason not to start.
			Listen:        "127.0.0.1:0",
			MetricsListen: "127.0.0.1:0",
			OnListen: func(m app.Mode, _ string) {
				once.Do(func() { mode = m; close(listening) })
			},
		})
	}()

	select {
	case err := <-done:
		r.NoError(err, "a server given only an address would not start")
	case <-listening:
	}
	cancel()
	select {
	case err := <-done:
		r.NoError(err)
	case <-time.After(20 * time.Second):
		r.Fail("the server did not stop")
	}
	r.Equal(app.ModeSetup, mode, "a server given only an address did not open the wizard")
	_ = dir
}
