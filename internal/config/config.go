// Package config is the server's configuration: the small file beside the
// binary, the environment variables that override it, and the organisation
// settings that live in the database.
//
// The split matters and it is deliberate. The file holds only what a machine
// needs in order to find its data and open its ports — a connection string and
// two listen addresses. Everything an operator chose in the wizard, and
// everything drivers see, lives in the database, because the database is the
// thing that survives. A data directory can be deleted, or sit on a container
// volume that does not outlive a restart, and when that happens the server must
// still come up knowing who it is.
//
// That is also why nothing in this package decides whether setup has run. The
// presence of a file proves nothing. See internal/db for the row that does.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pacenote-sim/protocol/wire"
)

// DirName is the data directory created beside the binary on first run.
const DirName = "pacenote-data"

// FileName is the configuration file inside the data directory.
const FileName = "config.json"

// DirMode is the data directory's permission: the operator, and nobody else.
// The directory holds a connection string, so a group-readable bit on it is a
// credential leak on a shared machine.
const DirMode os.FileMode = 0o700

// FileMode is the configuration file's permission, for the same reason.
const FileMode os.FileMode = 0o600

// The environment variables that override the file. They exist for anyone
// automating a deployment, and EnvDatabaseURL is the one that matters: it is
// how a container with an ephemeral volume starts without its file.
const (
	EnvDataDir       = "PACENOTE_DATA_DIR"
	EnvDatabaseURL   = "PACENOTE_DATABASE_URL"
	EnvListen        = "PACENOTE_LISTEN"
	EnvMetricsListen = "PACENOTE_METRICS_LISTEN"
	EnvSecretKey     = "PACENOTE_SECRET_KEY"
	EnvClientBinary  = "PACENOTE_CLIENT_BINARY"
)

// DefaultListen is the address the public server binds when nothing says
// otherwise: every interface, port 8080, because 80 needs a privilege an
// operator should not have to grant to try something out.
const DefaultListen = ":8080"

// DefaultMetricsListen is the private port. It is loopback and stays loopback:
// [Config.Validate] refuses anything else.
const DefaultMetricsListen = "127.0.0.1:9090"

// ErrNotConfigured is returned by [Load] when the data directory holds no
// configuration file. It is not a failure — on a first run it is the expected
// answer — so callers test for it with errors.Is rather than treating it as
// broken.
var ErrNotConfigured = errors.New("config: no configuration file")

// Config is the machine-local wiring: where the data is and which ports to
// open. It is the whole contents of config.json.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string, in either the URL or the
	// keyword/value spelling. It is the one secret in the file, which is why
	// the file is 0600 inside a 0700 directory.
	DatabaseURL string `json:"database_url"`
	// Listen is the public address, "host:port" or ":port".
	Listen string `json:"listen"`
	// MetricsListen is the private address for metrics and pprof. It must
	// resolve to a loopback interface.
	MetricsListen string `json:"metrics_listen"`
	// SecretKey is the base64 data key that encrypts the operator's own API
	// keys before they are stored. It lives here rather than in the database so
	// that a database dump holds ciphertext and nothing that opens it. Losing
	// it costs the stored API key and nothing else: the server says the key is
	// unreadable and asks for it again.
	SecretKey string `json:"secret_key,omitempty"`
	// ClientBinary is the prebuilt Windows client the build page stamps copies
	// of. It is a path rather than a file inside this binary because the
	// client is another repository's build artefact, an order of magnitude
	// larger than this server, and an operator upgrades one without the other:
	// dropping a newer client into the data directory is how they do it.
	//
	// Empty means [ClientBinaryIn] of the data directory, which is where the
	// community edition's zip puts it.
	ClientBinary string `json:"client_binary,omitempty"`
}

// Default is the configuration of a server that has been told nothing.
func Default() Config {
	return Config{Listen: DefaultListen, MetricsListen: DefaultMetricsListen}
}

// Load reads config.json from dir and applies the environment overrides on top.
//
// A missing file is [ErrNotConfigured] and the environment is still applied, so
// a caller can tell "no file, but a database URL in the environment" from "no
// file and nothing to go on" by checking the returned Config.
func Load(dir string) (Config, error) {
	c := Default()
	// The path is the operator's own data directory, named by them on the
	// command line or in the environment. Reading a file they chose is the
	// whole job.
	b, err := os.ReadFile(filepath.Join(dir, FileName)) //nolint:gosec // G304: the data directory is the operator's own choice.
	switch {
	case err == nil:
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if decErr := dec.Decode(&c); decErr != nil {
			return c, fmt.Errorf("config: %s is not readable: %w", FileName, decErr)
		}
		c.applyEnv()
		c.fillDefaults()
		return c, nil
	case errors.Is(err, os.ErrNotExist):
		c.applyEnv()
		c.fillDefaults()
		return c, ErrNotConfigured
	default:
		return c, fmt.Errorf("config: cannot read %s: %w", FileName, err)
	}
}

// Save writes the configuration into dir, creating the directory at [DirMode]
// if it is not there. The write is to a temporary file in the same directory
// followed by a rename, so a crash halfway through leaves the old file intact
// rather than half of the new one.
func (c Config) Save(dir string) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: cannot encode configuration: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("config: cannot write into %s: %w", dir, err)
	}
	name := tmp.Name()
	// A no-op once the rename below has succeeded, and best-effort if it has
	// not: there is nothing useful to do about a temporary file that will not
	// go away.
	defer func() { _ = os.Remove(name) }()

	if err := tmp.Chmod(FileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot set the permissions of %s: %w", FileName, err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot write %s: %w", FileName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot flush %s: %w", FileName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: cannot close %s: %w", FileName, err)
	}
	if err := os.Rename(name, filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("config: cannot install %s: %w", FileName, err)
	}
	return nil
}

// Validate reports the first thing wrong with the configuration, phrased for
// the operator who has to fix it.
func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("config: the listen address is empty — set it to something like \":8080\"")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("config: %q is not a listen address — it needs a port, like \":8080\"", c.Listen)
	}
	return c.validateMetrics()
}

// validateMetrics holds the metrics port to loopback. The port carries pprof
// and every internal counter, so binding it to a public interface publishes the
// server's internals; it is easier to make that impossible than to document it.
func (c Config) validateMetrics() error {
	if c.MetricsListen == "" {
		return errors.New("config: the metrics address is empty — set it to something like \"127.0.0.1:9090\"")
	}
	host, _, err := net.SplitHostPort(c.MetricsListen)
	if err != nil {
		return fmt.Errorf("config: %q is not a metrics address — it needs a port, like \"127.0.0.1:9090\"", c.MetricsListen)
	}
	if host == "" {
		return fmt.Errorf("config: %q would put metrics and pprof on every interface — bind them to 127.0.0.1", c.MetricsListen)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if host == "localhost" {
			return nil
		}
		return fmt.Errorf("config: %q is not a loopback address — metrics and pprof must stay on 127.0.0.1", c.MetricsListen)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("config: %q is not a loopback address — metrics and pprof must stay on 127.0.0.1", c.MetricsListen)
	}
	return nil
}

func (c *Config) applyEnv() {
	if v, ok := os.LookupEnv(EnvDatabaseURL); ok && v != "" {
		c.DatabaseURL = v
	}
	if v, ok := os.LookupEnv(EnvListen); ok && v != "" {
		c.Listen = v
	}
	if v, ok := os.LookupEnv(EnvMetricsListen); ok && v != "" {
		c.MetricsListen = v
	}
	if v, ok := os.LookupEnv(EnvSecretKey); ok && v != "" {
		c.SecretKey = v
	}
	if v, ok := os.LookupEnv(EnvClientBinary); ok && v != "" {
		c.ClientBinary = v
	}
}

func (c *Config) fillDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.MetricsListen == "" {
		c.MetricsListen = DefaultMetricsListen
	}
}

// EnsureDir creates the data directory at [DirMode] if it is missing, and
// tightens the permissions of one that already exists but is readable by
// anyone else.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("config: cannot create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("config: cannot set the permissions of %s: %w", dir, err)
	}
	return nil
}

// DefaultDir is the data directory beside the running binary, overridden by
// [EnvDataDir]. Beside the binary rather than in a home directory because the
// operator moves one file to a server and expects its data to be where they put
// it.
func DefaultDir() string {
	if v, ok := os.LookupEnv(EnvDataDir); ok && v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		// Nothing useful to say and nowhere to say it: fall back to the
		// working directory, which is where an operator running ./pacenote-server
		// would have looked anyway.
		return DirName
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), DirName)
}

// ClientDirName is the folder inside the data directory that holds the
// prebuilt Windows client. It is named in the empty state on the build page,
// so it is a constant rather than a string typed twice.
const ClientDirName = "client"

// ClientFileName is what the prebuilt Windows client is called. It is the name
// the client's own build produces, so an operator copies the file across
// without renaming it.
const ClientFileName = "pacenote-telemetry.exe"

// ClientBinaryIn is where this server looks for the prebuilt client when the
// configuration does not say: inside the data directory, beside everything
// else an operator put there.
func ClientBinaryIn(dir string) string {
	return filepath.Join(dir, ClientDirName, ClientFileName)
}

// ClientBinaryPath is the prebuilt client this configuration names, or the
// data directory's own when it names none.
func (c Config) ClientBinaryPath(dir string) string {
	if c.ClientBinary != "" {
		return c.ClientBinary
	}
	return ClientBinaryIn(dir)
}

// ClientBuildsDir is where the clients an operator builds are kept. They are
// in the data directory rather than in a temporary one because a download link
// an operator sent to their team has to keep working after a restart.
func ClientBuildsDir(dir string) string { return filepath.Join(dir, "client-builds") }

// AutocertDir is where an automatic certificate and its account key are cached,
// inside the data directory. Losing it costs a new certificate, not an outage,
// but Let's Encrypt rate-limits issuance, so it is worth keeping.
func AutocertDir(dir string) string { return filepath.Join(dir, "autocert") }

// PluginsDir is where plugins live, inside the data directory: one directory
// per plugin, each holding a manifest and a binary.
//
// It is under the data directory rather than beside the binary because the data
// directory is the thing an operator backs up, moves between machines and
// mounts as a volume, and a plugin that did not travel with it would be a
// server that came up missing an integration nobody removed.
func PluginsDir(dir string) string { return filepath.Join(dir, "plugins") }

// DefaultLimits are the wire limits served in discovery and enforced by the
// server, straight from API-V1.md. One set of constants, so the client's
// intervals and the server's limiter cannot drift apart.
func DefaultLimits() wire.Limits {
	return wire.Limits{
		TracePoints:       300,
		LapsPerRequest:    50,
		LiveIntervalMs:    1000,
		FieldIntervalMs:   2000,
		SummaryIntervalMs: 30000,
		MaxBodyBytes:      2 << 20,
	}
}
