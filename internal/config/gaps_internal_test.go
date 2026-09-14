package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Save writes through a temporary file and a rename so that a crash halfway
// through leaves the previous configuration rather than half of the new one.
// Every step of that has a failure worth naming, and the ones reachable without
// breaking the filesystem are here.
func TestSaveWritesAtomicallyAndTightly(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := filepath.Join(t.TempDir(), "data")
	c := Config{Listen: DefaultListen, MetricsListen: DefaultMetricsListen}
	r.NoError(c.Save(dir))

	// The directory was created, and with the permissions a data directory
	// holding a data key has to have.
	di, err := os.Stat(dir)
	r.NoError(err)
	r.True(di.IsDir())

	fi, err := os.Stat(filepath.Join(dir, FileName))
	r.NoError(err)
	if runtime.GOOS != "windows" {
		r.Equal(DirMode, di.Mode().Perm(), "the data directory is readable by others")
		r.Equal(FileMode, fi.Mode().Perm(), "the configuration file is readable by others")
	}

	// Nothing is left behind: a temporary file in the data directory would be
	// a second copy of the data key sitting beside the first.
	entries, err := os.ReadDir(dir)
	r.NoError(err)
	r.Len(entries, 1)
	r.Equal(FileName, entries[0].Name())

	// Saving again over an existing file works, which is what every settings
	// change does.
	c.Listen = "127.0.0.1:9999"
	r.NoError(c.Save(dir))
	loaded, err := Load(dir)
	r.NoError(err)
	r.Equal("127.0.0.1:9999", loaded.Listen)
}

func TestSaveReportsADirectoryItCannotWriteInto(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A file where the directory should be. MkdirAll fails on it, which is the
	// operator having put something in the way.
	base := t.TempDir()
	inTheWay := filepath.Join(base, "data")
	r.NoError(os.WriteFile(inTheWay, []byte("not a directory"), 0o600))

	err := Config{}.Save(inTheWay)
	r.Error(err)
	r.Contains(err.Error(), "cannot create")
}

// EnsureDir tightens a directory that already exists and is readable by
// everyone, because an upgrade must fix what an earlier version allowed.
func TestEnsureDirTightensADirectoryThatIsTooOpen(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry these permission bits")
	}
	r := require.New(t)

	dir := filepath.Join(t.TempDir(), "data")
	r.NoError(os.MkdirAll(dir, 0o755))
	r.NoError(EnsureDir(dir))

	fi, err := os.Stat(dir)
	r.NoError(err)
	r.Equal(DirMode, fi.Mode().Perm())
}

// The data directory is beside the binary rather than in a home directory: an
// operator copies one file to a server and expects its data where they put it.
// The environment variable overrides that, which is what a container does.
func TestDefaultDirPrefersTheEnvironmentThenTheBinary(t *testing.T) {
	r := require.New(t)

	t.Setenv(EnvDataDir, "/var/lib/pacenote")
	r.Equal("/var/lib/pacenote", DefaultDir())

	// Set but empty is not set: an unset variable and one cleared to nothing
	// are the same intent.
	t.Setenv(EnvDataDir, "")
	beside := DefaultDir()
	r.Equal(DirName, filepath.Base(beside))
	exe, err := os.Executable()
	r.NoError(err)
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	r.Equal(filepath.Join(filepath.Dir(exe), DirName), beside)
}

// A configuration naming its own client binary uses it; one that names none
// falls back to the data directory, which is where an operator drops the file.
func TestClientBinaryPathPrefersWhatTheConfigurationNames(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	dir := "/srv/pacenote"
	r.Equal(ClientBinaryIn(dir), Config{}.ClientBinaryPath(dir))
	r.Equal("/opt/builds/telemetry.exe",
		Config{ClientBinary: "/opt/builds/telemetry.exe"}.ClientBinaryPath(dir))
}

// fillDefaults is what makes a configuration file with only the fields the
// operator cared about into a complete one.
func TestFillDefaultsOnlyFillsWhatIsMissing(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var empty Config
	empty.fillDefaults()
	r.Equal(DefaultListen, empty.Listen)
	r.Equal(DefaultMetricsListen, empty.MetricsListen)

	chosen := Config{Listen: "0.0.0.0:80", MetricsListen: "127.0.0.1:1234"}
	chosen.fillDefaults()
	r.Equal("0.0.0.0:80", chosen.Listen)
	r.Equal("127.0.0.1:1234", chosen.MetricsListen)
}

// The short name is the starting value in settings, not a rule, but it has to
// be a reasonable one: a word already written in capitals is an abbreviation
// already and is kept whole.
func TestShortNameKeepsAbbreviationsWhole(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"Iberian GT Championship", "IGTC"},
		{"GT", "GT"},
		{"SRO Motorsports Group", "SROMG"},
		{"pacenote racing", "PR"},
		{"  spaced   out  words ", "SOW"},
		// Long enough to stop at five characters rather than run on.
		{"One Two Three Four Five Six Seven", "OTTFF"},
		// Nothing to take an initial from: the name is returned as it stands.
		{"", ""},
		{"   ", "   "},
		// Punctuation is not an abbreviation — isAllCaps counts letters — so
		// each word contributes its first character and nothing more.
		{"!!! ???", "!?"},
		// Capitals longer than three letters are a word, not an abbreviation.
		{"RACING league", "RL"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, shortName(tc.in), "shortName(%q)", tc.in)
	}
}

func TestIsAllCapsAndUpper(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.True(isAllCaps([]rune("GT")))
	r.False(isAllCaps([]rune("Gt")), "one lowercase letter and it is a word")
	r.False(isAllCaps([]rune("123")), "digits alone are not an abbreviation")
	r.True(isAllCaps([]rune("G3")))

	r.Equal('A', upper('a'))
	r.Equal('A', upper('A'))
	r.Equal('3', upper('3'))
	r.Equal('é', upper('é'), "only ASCII is folded, deliberately")
}

func TestValidateAccentRefusesWhatIsNotAColour(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.NoError(ValidateAccent("#ff0000"))
	r.NoError(ValidateAccent("#FF0000"))
	for _, bad := range []string{"ff0000", "#ff00", "#gggggg", "#ff00000", "red", "#"} {
		r.Error(ValidateAccent(bad), "ValidateAccent(%q) was accepted", bad)
	}
	// An empty accent is the operator not having chosen one, which is allowed:
	// the client falls back to its own.
	r.NoError(ValidateAccent(""))
}

// validLabel is one dot-separated part of the public host name. It is the check
// that stops a settings field from becoming a host name nothing will resolve.
func TestValidLabelHoldsTheHostNameRules(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.NoError(validLabel("pacenote"))
	r.NoError(validLabel("my-league"))
	r.NoError(validLabel("a"))
	r.NoError(validLabel("123"))

	r.Error(validLabel(""), "an empty part is two dots in a row")
	r.Error(validLabel(strings.Repeat("a", 64)), "a part longer than 63 bytes")
	r.Error(validLabel("under_score"))
	r.Error(validLabel("has space"))
	r.Error(validLabel("-leading"))
	r.Error(validLabel("trailing-"))
	r.NoError(validLabel(strings.Repeat("a", 63)), "63 is the limit, not past it")
}
