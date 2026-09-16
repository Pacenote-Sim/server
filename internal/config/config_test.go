package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/config"
)

func TestLoadWithNoFile(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	cfg, err := config.Load(t.TempDir())
	r.ErrorIs(err, config.ErrNotConfigured)
	r.Empty(cfg.DatabaseURL)
	r.Equal(config.DefaultListen, cfg.Listen)
	r.Equal(config.DefaultMetricsListen, cfg.MetricsListen)
}

func TestSaveThenLoad(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")

	want := config.Config{
		DatabaseURL:   "postgres://pacenote:hunter2@db.example.com:5432/pacenote",
		Listen:        ":9000",
		MetricsListen: "127.0.0.1:9111",
		SecretKey:     "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	r.NoError(want.Save(dir))

	got, err := config.Load(dir)
	r.NoError(err)
	r.Equal(want, got)
}

func TestSaveSetsTightPermissions(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	r.NoError(config.Config{Listen: ":8080", MetricsListen: "127.0.0.1:9090"}.Save(dir))

	di, err := os.Stat(dir)
	r.NoError(err)
	r.Equal(config.DirMode, di.Mode().Perm(), "the data directory holds a connection string")

	fi, err := os.Stat(filepath.Join(dir, config.FileName))
	r.NoError(err)
	r.Equal(config.FileMode, fi.Mode().Perm())
}

func TestSaveLeavesNoTemporaryFileBehind(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	cfg := config.Config{Listen: ":8080", MetricsListen: "127.0.0.1:9090"}
	r.NoError(cfg.Save(dir))
	r.NoError(cfg.Save(dir))

	entries, err := os.ReadDir(dir)
	r.NoError(err)
	r.Len(entries, 1)
	r.Equal(config.FileName, entries[0].Name())
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()
	r.NoError(os.WriteFile(filepath.Join(dir, config.FileName),
		[]byte(`{"listen":":8080","nonsense":true}`), 0o600))

	_, err := config.Load(dir)
	r.Error(err)
	r.NotErrorIs(err, config.ErrNotConfigured)
}

func TestEnvironmentOverridesTheFile(t *testing.T) {
	// Not parallel: it sets process environment variables.
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	r.NoError(config.Config{
		DatabaseURL:   "postgres://from-file/db",
		Listen:        ":1111",
		MetricsListen: "127.0.0.1:1112",
	}.Save(dir))

	t.Setenv(config.EnvDatabaseURL, "postgres://from-env/db")
	t.Setenv(config.EnvListen, ":2222")
	t.Setenv(config.EnvMetricsListen, "127.0.0.1:2223")
	t.Setenv(config.EnvSecretKey, "from-env-key")

	cfg, err := config.Load(dir)
	r.NoError(err)
	r.Equal("postgres://from-env/db", cfg.DatabaseURL)
	r.Equal(":2222", cfg.Listen)
	r.Equal("127.0.0.1:2223", cfg.MetricsListen)
	r.Equal("from-env-key", cfg.SecretKey)
}

func TestEnvironmentAloneIsEnough(t *testing.T) {
	// Not parallel: it sets process environment variables.
	r := require.New(t)
	t.Setenv(config.EnvDatabaseURL, "postgres://only-from-env/db")

	cfg, err := config.Load(t.TempDir())
	r.ErrorIs(err, config.ErrNotConfigured)
	r.Equal("postgres://only-from-env/db", cfg.DatabaseURL,
		"a container with an ephemeral volume starts from the environment alone")
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{"the defaults", config.Default(), false},
		{"loopback by name", config.Config{Listen: ":8080", MetricsListen: "localhost:9090"}, false},
		{"loopback v6", config.Config{Listen: ":8080", MetricsListen: "[::1]:9090"}, false},
		{"no listen address", config.Config{MetricsListen: "127.0.0.1:9090"}, true},
		{"listen without a port", config.Config{Listen: "8080", MetricsListen: "127.0.0.1:9090"}, true},
		{"no metrics address", config.Config{Listen: ":8080"}, true},
		{"metrics on every interface", config.Config{Listen: ":8080", MetricsListen: ":9090"}, true},
		{"metrics on a public address", config.Config{Listen: ":8080", MetricsListen: "0.0.0.0:9090"}, true},
		{"metrics on a routable address", config.Config{Listen: ":8080", MetricsListen: "10.0.0.5:9090"}, true},
		{"metrics on a host name", config.Config{Listen: ":8080", MetricsListen: "metrics.example.com:9090"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			err := tc.cfg.Validate()
			if tc.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
		})
	}
}

func TestValidateHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		host    string
		wantErr string
	}{
		{"an ordinary host name", "pacenote.example.com", ""},
		{"a subdomain with hyphens", "my-league.pacenote.example.com", ""},
		{"localhost, for a development run", "localhost", ""},
		{"empty", "", "empty"},
		{"with a scheme", "https://pacenote.example.com", "scheme"},
		{"with a port", "pacenote.example.com:8443", "port"},
		{"with a path", "pacenote.example.com/telemetry", "host name on its own"},
		{"an IPv4 address", "203.0.113.10", "host name rather than an IP"},
		{"a bare name with no domain", "pacenote", "needs a domain"},
		{"a label ending in a hyphen", "pacenote-.example.com", "hyphen"},
		{"an underscore", "pit_wall.example.com", "letters, digits, hyphens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			err := config.ValidateHost(tc.host)
			if tc.wantErr == "" {
				r.NoError(err)
				return
			}
			r.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestSettings(t *testing.T) {
	t.Parallel()

	t.Run("a public host gets https", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		s := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSAuto)
		r.Equal("https", s.Scheme())
		r.Equal("https://pacenote.example.com", s.BaseURL())
	})

	t.Run("localhost gets plain http", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		s := config.DefaultSettings("Test", "localhost", config.TLSProxy)
		r.Equal("http://localhost", s.BaseURL())
	})

	t.Run("the limits are the ones in the contract", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		l := config.DefaultLimits()
		r.Equal(300, l.TracePoints)
		r.Equal(50, l.LapsPerRequest)
		r.Equal(1000, l.LiveIntervalMs)
		r.Equal(2000, l.FieldIntervalMs)
		r.Equal(30000, l.SummaryIntervalMs)
		r.Equal(2097152, l.MaxBodyBytes)
	})

	t.Run("discovery carries the operator's identity and limits", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		s := config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSAuto)
		d := s.Discovery([]wire.Feature{wire.FeatureTelemetry}, "1.0.0")
		r.Equal(config.APIBase, d.API)
		r.Equal("Iberian GT Championship", d.Name)
		r.Equal("IGTC", d.ShortName)
		r.Equal("https://pacenote.example.com/pair", d.PairURI)
		r.Equal(config.DefaultLimits(), d.Limits)
		r.True(d.Has(wire.FeatureTelemetry))
		r.False(d.Has(wire.FeatureField), "a feature the list does not carry is absent")
	})

	t.Run("validate", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			set  config.Settings
			ok   bool
		}{
			{"complete", config.DefaultSettings("A league", "pacenote.example.com", config.TLSProxy), true},
			{"no organisation", config.DefaultSettings("", "pacenote.example.com", config.TLSProxy), false},
			{"no host", config.DefaultSettings("A league", "", config.TLSProxy), false},
			{"no mode", config.DefaultSettings("A league", "pacenote.example.com", ""), false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				err := tc.set.Validate()
				if tc.ok {
					r.NoError(err)
					return
				}
				r.Error(err)
			})
		}
	})
}

func TestDefaultDir(t *testing.T) {
	// Not parallel: it sets a process environment variable.
	r := require.New(t)
	t.Setenv(config.EnvDataDir, "/tmp/somewhere-else")
	r.Equal("/tmp/somewhere-else", config.DefaultDir())
}

func TestAutocertDirIsInsideTheDataDir(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()
	r.Equal(filepath.Join(dir, "autocert"), config.AutocertDir(dir))
}

func TestRetention(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	t.Run("keeping everything is the default and deletes nothing", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		var zero config.Retention
		r.False(zero.Keeps())
		_, ok := zero.Cutoff(now)
		r.False(ok, "a zero-month setting must never read as a cutoff of now")
	})

	t.Run("a horizon becomes a cutoff that far back", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			months int
			want   time.Time
		}{
			{"one month", 1, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)},
			{"six months", 6, time.Date(2026, 3, 13, 12, 0, 0, 0, time.UTC)},
			{"a year", 12, time.Date(2025, 9, 13, 12, 0, 0, 0, time.UTC)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				got, ok := config.Retention{TraceMonths: tc.months}.Cutoff(now)
				r.True(ok)
				r.True(tc.want.Equal(got), "want %s, got %s", tc.want, got)
			})
		}
	})

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			months  int
			wantErr bool
		}{
			{"keeping everything", 0, false},
			{"one month", 1, false},
			{"the longest this server sets", config.MaxRetentionMonths, false},
			{"longer than that", config.MaxRetentionMonths + 1, true},
			{"a negative length of time", -1, true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				r := require.New(t)
				err := config.ValidateRetention(config.Retention{TraceMonths: tc.months})
				if tc.wantErr {
					r.Error(err)
					return
				}
				r.NoError(err)
			})
		}
	})
}

// Every check [config.Settings.Validate] makes, each one reached by settings
// that are wrong in exactly one way.
//
// The order matters as much as the checks. An operator gets one sentence back,
// so it has to be about the field they are most likely to have got wrong, and a
// settings document that is wrong in two ways has to name the first rather than
// the one whose check happens to run first.
func TestSettingsValidate(t *testing.T) {
	t.Parallel()

	good := func() config.Settings {
		return config.DefaultSettings("Iberian GT Championship", "pacenote.example.com", config.TLSProxy)
	}
	r := require.New(t)
	r.NoError(good().Validate())

	cases := []struct {
		name string
		set  func(s *config.Settings)
		want string
	}{
		{
			name: "no organisation", want: "organisation name is empty",
			set: func(s *config.Settings) { s.Organisation = "   " },
		},
		{
			name: "no public host", want: "public host name is empty",
			set: func(s *config.Settings) { s.PublicHost = "" },
		},
		{
			name: "a way of serving nobody has heard of", want: "not a way of serving this host",
			set: func(s *config.Settings) { s.TLSMode = config.TLSMode("carrier pigeon") },
		},
		{
			// A host that is a URL rather than a host. It is the commonest
			// mistake on that field, because every other address on the page is
			// written with a scheme.
			name: "a host with a scheme on it", want: "scheme",
			set: func(s *config.Settings) { s.PublicHost = "https://pacenote.example.com" },
		},
		{
			name: "a limit nobody could meet", want: "trace points",
			set: func(s *config.Settings) { s.Limits.TracePoints = -1 },
		},
		{
			name: "an accent that is not a colour", want: "accent",
			set: func(s *config.Settings) { s.Accent = "racing green" },
		},
		{
			name: "a logo that is not an address", want: "logo",
			set: func(s *config.Settings) { s.Logo = "logo.png" },
		},
		{
			name: "traces kept for a length of time nobody chose", want: "",
			set: func(s *config.Settings) { s.Retention.TraceMonths = -5 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			s := good()
			tc.set(&s)
			err := s.Validate()
			r.Error(err, "settings that are wrong were accepted")
			if tc.want != "" {
				r.Contains(err.Error(), tc.want)
			}
		})
	}
}

// A metrics address with no port at all. It is worth its own case because the
// two neighbouring failures — an empty address and one bound to every interface
// — are different mistakes with different fixes, and the sentence has to say
// which one the operator made.
func TestAMetricsAddressWithNoPort(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	err := config.Config{
		DatabaseURL:   "postgres://localhost/pacenote",
		Listen:        config.DefaultListen,
		MetricsListen: "not an address at all",
	}.Validate()
	r.Error(err)
	r.Contains(err.Error(), "needs a port")
}

// The prebuilt client can be pointed somewhere else entirely, for the operator
// who keeps it outside the data directory.
func TestTheClientBinaryCanComeFromTheEnvironment(t *testing.T) {
	// Not parallel: it sets process environment variables.
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "pacenote-data")
	r.NoError(config.Config{DatabaseURL: "postgres://localhost/pacenote"}.Save(dir))
	t.Setenv(config.EnvClientBinary, "/srv/clients/pacenote-telemetry.exe")

	cfg, err := config.Load(dir)
	r.NoError(err)
	r.Equal("/srv/clients/pacenote-telemetry.exe", cfg.ClientBinaryPath(dir),
		"the environment did not override where the client is kept")
}

// An abbreviation is made of the words that are there. A name padded with
// whitespace is the same name.
func TestTheShortNameIgnoresEmptyWords(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	s := config.DefaultSettings("  Iberian   GT \t Championship  ", "pacenote.example.com", config.TLSProxy)
	r.Equal("IGTC", s.ShortName)
}
