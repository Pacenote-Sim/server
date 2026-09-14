package packaging_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/pacenote-sim/server/internal/packaging"
)

// root is this repository from the package's own directory, which is where a Go
// test's working directory always is.
const root = "../.."

// flat collapses the runs of whitespace a wrapped markdown paragraph is full
// of, so an assertion about a sentence does not depend on where the line broke.
func flat(tb testing.TB, rel string) string {
	tb.Helper()
	return strings.Join(strings.Fields(read(tb, rel)), " ")
}

func read(tb testing.TB, rel string) string {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoErrorf(tb, err, "%s is part of the release package and has to exist", rel)
	return string(b)
}

// --- the release configuration agrees with the contract ---------------------

type releaseConfig struct {
	Builds []struct {
		Main    string   `yaml:"main"`
		Binary  string   `yaml:"binary"`
		Env     []string `yaml:"env"`
		Goos    []string `yaml:"goos"`
		Goarch  []string `yaml:"goarch"`
		Flags   []string `yaml:"flags"`
		Ldflags []string `yaml:"ldflags"`
		Ignore  []struct {
			Goos   string `yaml:"goos"`
			Goarch string `yaml:"goarch"`
		} `yaml:"ignore"`
	} `yaml:"builds"`
	Archives []struct {
		Formats         []string `yaml:"formats"`
		NameTemplate    string   `yaml:"name_template"`
		WrapInDirectory bool     `yaml:"wrap_in_directory"`
		Files           []struct {
			Src         string `yaml:"src"`
			StripParent bool   `yaml:"strip_parent"`
		} `yaml:"files"`
	} `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
		Algorithm    string `yaml:"algorithm"`
	} `yaml:"checksum"`
}

func releaseCfg(tb testing.TB) releaseConfig {
	tb.Helper()
	var cfg releaseConfig
	require.NoError(tb, yaml.Unmarshal([]byte(read(tb, ".goreleaser.yml")), &cfg))
	require.Len(tb, cfg.Builds, 1, "one build, so there is one set of flags to reason about")
	require.Len(tb, cfg.Archives, 1, "one archive, so every platform's zip holds the same files")
	return cfg
}

func TestReleaseBuildsEveryPlatformThatIsSupported(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	b := releaseCfg(t).Builds[0]

	var built []packaging.Platform
	for _, goos := range b.Goos {
		for _, goarch := range b.Goarch {
			ignored := slices.ContainsFunc(b.Ignore, func(i struct {
				Goos   string `yaml:"goos"`
				Goarch string `yaml:"goarch"`
			},
			) bool {
				return i.Goos == goos && i.Goarch == goarch
			})
			if !ignored {
				built = append(built, packaging.Platform{OS: goos, Arch: goarch})
			}
		}
	}

	want := packaging.Platforms()
	slices.SortFunc(built, func(a, b packaging.Platform) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(want, func(a, b packaging.Platform) int { return strings.Compare(a.String(), b.String()) })
	r.Equal(want, built, "the release config and the supported platform list have drifted apart")
}

func TestReleaseBinaryIsStaticTrimmedAndStamped(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	b := releaseCfg(t).Builds[0]

	r.Equal(packaging.Binary, b.Binary)
	r.Contains(b.Env, "CGO_ENABLED=0", "a dynamic binary is one an operator has to install things for")
	r.Contains(b.Flags, "-trimpath", "without it the archive carries a build machine's paths")
	r.Contains(strings.Join(b.Ldflags, " "), "-X github.com/pacenote-sim/server/internal/buildinfo.Stamp=",
		"the version has to reach -version and the panel, and this is what writes it")
}

func TestArchiveIsNamedWhatAnOperatorDownloads(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	a := releaseCfg(t).Archives[0]

	r.Equal([]string{"zip"}, a.Formats, "one format everywhere, so there is one instruction to give")
	r.False(a.WrapInDirectory, "the binary and the files beside it belong at the top of the zip")

	// Render GoReleaser's template the way GoReleaser would, and check it
	// produces the name the README and the checksums file promise.
	rendered := a.NameTemplate
	for placeholder, value := range map[string]string{
		"{{ .ProjectName }}": packaging.Project,
		"{{ .Version }}":     "1.2.3",
		"{{ .Os }}":          "linux",
		"{{ .Arch }}":        "amd64",
	} {
		rendered = strings.ReplaceAll(rendered, placeholder, value)
	}
	r.Equal(
		packaging.ArchiveName("1.2.3", packaging.Platform{OS: "linux", Arch: "amd64"}),
		rendered+".zip",
	)
}

func TestArchiveCarriesEveryFileAnOperatorReads(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	a := releaseCfg(t).Archives[0]

	srcs := make([]string, 0, len(a.Files))
	for _, f := range a.Files {
		srcs = append(srcs, f.Src)
		// A file taken from packaging/ has to be flattened, or it arrives one
		// directory down from where the README says it is.
		if strings.Contains(f.Src, "/") {
			r.Truef(f.StripParent, "%s needs strip_parent or it lands in a subdirectory", f.Src)
		}
	}
	slices.Sort(srcs)
	r.Equal(packaging.Sources(), srcs, "the archive contents and the packaging contract have drifted apart")

	for _, src := range srcs {
		r.FileExists(filepath.Join(root, filepath.FromSlash(src)))
	}
}

func TestChecksumsAreOneSha256FileForTheWholeRelease(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	c := releaseCfg(t).Checksum

	r.Equal(packaging.Checksums, c.NameTemplate, "one file, named the way the README tells people to check it")
	r.Equal("sha256", c.Algorithm)
}

func TestContentsIsTheBinaryAndTheDocuments(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal(
		[]string{"CHANGELOG.md", "LICENSE", "README.md", "docker-compose.yml", "pacenote-server"},
		packaging.Contents(packaging.Platform{OS: "linux", Arch: "amd64"}),
	)
	r.Equal(
		[]string{"CHANGELOG.md", "LICENSE", "README.md", "docker-compose.yml", "pacenote-server.exe"},
		packaging.Contents(packaging.Platform{OS: "windows", Arch: "amd64"}),
	)
	r.Equal("pacenote-ce_0.1.0_darwin_arm64.zip",
		packaging.ArchiveName("v0.1.0", packaging.Platform{OS: "darwin", Arch: "arm64"}),
		"a leading v is dropped, because GoReleaser's .Version has none")
}

// --- the compose file ------------------------------------------------------

type composeService struct {
	Image     string `yaml:"image"`
	Restart   string `yaml:"restart"`
	DependsOn map[string]struct {
		Condition string `yaml:"condition"`
	} `yaml:"depends_on"`
	Environment map[string]string `yaml:"environment"`
	Ports       []string          `yaml:"ports"`
	Volumes     []string          `yaml:"volumes"`
	ReadOnly    bool              `yaml:"read_only"`
	User        string            `yaml:"user"`
	CapDrop     []string          `yaml:"cap_drop"`
	SecurityOpt []string          `yaml:"security_opt"`
	Healthcheck *struct {
		Test        []string `yaml:"test"`
		Interval    string   `yaml:"interval"`
		Timeout     string   `yaml:"timeout"`
		Retries     int      `yaml:"retries"`
		StartPeriod string   `yaml:"start_period"`
	} `yaml:"healthcheck"`
}

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"volumes"`
}

func compose(tb testing.TB) composeFile {
	tb.Helper()
	var c composeFile
	require.NoError(tb, yaml.Unmarshal([]byte(read(tb, "packaging/docker-compose.yml")), &c),
		"the compose file in the release package does not parse")
	return c
}

func TestComposeBringsUpTheServerAndADatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	c := compose(t)

	r.NotEmpty(c.Name, "a project name, so two Pacenotes on one host do not share volumes")
	r.Contains(c.Services, "db")
	r.Contains(c.Services, "server")
	r.Len(c.Services, 2, "one command brings up two things and nothing an operator did not ask for")
}

func TestComposePinsThePostgresMajor(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	db := compose(t).Services["db"]

	r.Regexp(`^postgres:\d+`, db.Image,
		"a major upgrade rewrites the data directory, so it must not happen by restarting a container")
	r.NotContains(db.Image, ":latest")
}

func TestComposeNamesItsVolumes(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	c := compose(t)

	r.Len(c.Volumes, 2, "the database and the data directory, both surviving a recreate")
	for key, v := range c.Volumes {
		r.NotEmptyf(v.Name, "volume %q has no name, so Docker invents one from the project", key)
	}
}

func TestComposeGivesTheDatabaseAHealthcheck(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	db := compose(t).Services["db"]

	r.NotNil(db.Healthcheck, "without one there is nothing for the server to wait on")
	r.Contains(strings.Join(db.Healthcheck.Test, " "), "pg_isready")
	r.Positive(db.Healthcheck.Retries)
	r.NotEmpty(db.Healthcheck.StartPeriod, "initdb runs once and it is the slow start")
}

func TestComposeMakesTheServerWaitForTheDatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	server := compose(t).Services["server"]

	dep, ok := server.DependsOn["db"]
	r.True(ok, "the server treats an unreachable database as a hard stop, so it must not start first")
	r.Equal("service_healthy", dep.Condition,
		"service_started only waits for the container, which says nothing about PostgreSQL accepting connections")
}

func TestComposeRunsTheServerWithNoMoreThanItNeeds(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	server := compose(t).Services["server"]

	r.True(server.ReadOnly, "nothing in the server writes outside its data directory")
	r.NotEqual("root", server.User)
	r.NotEqual("0:0", server.User)
	r.NotEmpty(server.User)
	r.Contains(server.CapDrop, "ALL")
	r.Contains(server.SecurityOpt, "no-new-privileges:true")

	r.Contains(server.Environment, "PACENOTE_DATABASE_URL")
	r.Contains(server.Environment, "PACENOTE_DATA_DIR")
	r.Contains(server.Environment["PACENOTE_METRICS_LISTEN"], "127.0.0.1",
		"the metrics port carries pprof, and the server refuses to bind it anywhere else")

	r.Len(server.Volumes, 1, "config.json and the certificate cache, and nothing else")
	r.NotEmpty(server.Ports, "an operator has to be able to reach the wizard")
}

func TestComposeRefusesToStartWithoutADatabasePassword(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	raw := read(t, "packaging/docker-compose.yml")

	// The ":?" form makes Docker refuse rather than default, which is the
	// difference between an operator setting a password and one being chosen
	// for them and published in a file.
	r.Contains(raw, "${POSTGRES_PASSWORD:?")
	r.NotContains(raw, "POSTGRES_PASSWORD:-", "a default password is a published password")
	r.NotContains(raw, "POSTGRES_HOST_AUTH_METHOD: trust")
}

// --- the Dockerfile --------------------------------------------------------

func TestDockerfileIsMultiStageDistrolessAndNonRoot(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	raw := read(t, "Dockerfile")

	froms := regexp.MustCompile(`(?m)^FROM `).FindAllString(raw, -1)
	r.GreaterOrEqual(len(froms), 2, "a single stage ships the Go toolchain to every operator")

	runtime := raw[strings.LastIndex(raw, "\nFROM "):]
	r.NotContains(runtime, "\nRUN ", "the runtime image has no shell to RUN anything with")

	r.Regexp(`RUNTIME_IMAGE=(gcr\.io/distroless/|scratch)`, raw,
		"distroless or scratch: no shell, no package manager, nothing to pivot into")
	r.Regexp(`(?m)^USER 65532:65532$`, runtime,
		"numeric, so a read-only root filesystem and the volume's owner agree")
	r.Contains(raw, "CGO_ENABLED=0")
	r.Contains(raw, "-trimpath")
	r.Contains(raw, "-X github.com/pacenote-sim/server/internal/buildinfo.Stamp=",
		"there is no .git in this build context, so the linker is the only thing that knows the version")
	r.Contains(raw, "PACENOTE_DATA_DIR=/data",
		"the default data directory is beside the binary, and / is read-only here")
}

func TestDockerfileIgnoresWhatMustNotReachTheImage(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	raw := read(t, "Dockerfile.dockerignore")

	for _, pattern := range []string{"**/.git", "**/.env", "**/pacenote-data"} {
		r.Containsf(raw, pattern, "%s would put something private or pointless in the build context", pattern)
	}
}

// The other half of the same file: what it must not ignore.
//
// **/name matches at any depth, so a rule meant for a built binary at the top
// of the repository also matches a directory of that name further down. That is
// not hypothetical — **/pacenote-server was written for the binary beside the
// Makefile and silently excluded server/cmd/pacenote-server, which is the
// package the image is built from. The image build failed on a path that is
// plainly in the checkout, and nothing else in this suite noticed.
// The release notes have to survive the path from CHANGELOG.md to the release.
//
// Two files have to agree for that: the workflow passes --release-notes, and
// GoReleaser's changelog step is what reads it. The flag replaces generation
// rather than adding to it, so disabling the step to avoid a generated commit
// list silently drops the notes and the release is published with an empty
// body. That is what happened to v0.1.0, and nothing here noticed.
func TestTheReleaseNotesReachTheRelease(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	workflow := read(t, ".github/workflows/release.yml")
	r.Contains(workflow, "--release-notes=", "the release does not pass the notes to GoReleaser")
	r.Contains(workflow, "release-notes.sh", "nothing takes the notes out of CHANGELOG.md")

	var cfg struct {
		Changelog struct {
			Disable bool `yaml:"disable"`
		} `yaml:"changelog"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(read(t, ".goreleaser.yml")), &cfg))
	r.False(cfg.Changelog.Disable,
		"the changelog step is what reads --release-notes; disabled, the release body is empty")
}

func TestDockerfileDoesNotIgnoreWhatItBuilds(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	raw := read(t, "Dockerfile.dockerignore")

	// Paths the Dockerfile copies and the build needs, relative to the context.
	needed := []string{
		"server/cmd/pacenote-server",
		"server/internal",
		"protocol/wire",
		"plugin/examples",
	}

	for _, line := range strings.Split(raw, "\n") {
		pattern := strings.TrimSpace(line)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		name, found := strings.CutPrefix(pattern, "**/")
		if !found || strings.ContainsAny(name, "*?[") {
			continue
		}
		for _, want := range needed {
			for _, segment := range strings.Split(want, "/") {
				r.NotEqualf(name, segment,
					"%q matches at any depth, so it also excludes %s — anchor it as */%s",
					pattern, want, name)
			}
		}
	}
}

// --- the words an operator reads -------------------------------------------

func TestReadmeAnswersTheQuestionsInTheOrderTheyAreAsked(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	readme := read(t, "packaging/README.md")

	// Every heading the brief for this package promises, in order. A reader
	// meeting them out of order has to skip ahead to know whether they can
	// start at all.
	want := []string{
		"# Pacenote server",
		"## What you need before you start",
		"## Starting it",
		"### The binary",
		"### Docker compose",
		"## What the first run looks like",
		"### Why there is a token",
		"## Where your data is",
		"## Backing it up",
		"## Upgrading",
		"## Getting the Windows client to your drivers",
	}
	at := 0
	for _, heading := range want {
		i := strings.Index(readme[at:], heading)
		r.GreaterOrEqualf(i, 0, "the README has no %q, or it comes before the section above it", heading)
		at += i + len(heading)
	}
}

func TestReadmeSaysWhatIsNeededAndWhatIsNotHereYet(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	readme := flat(t, "packaging/README.md")

	r.Contains(readme, "PostgreSQL 15 or newer")
	r.Contains(readme, "the server creates its own tables on first start")
	r.Contains(readme, "pg_dump", "a backup instruction that is not a command is not an instruction")
	r.Contains(readme, "pg_restore")
	r.Contains(readme, "checksums.txt")
	// Building clients from the panel ships, so the README has to say so, say
	// what a driver will see if the build is unsigned, and say what to upload
	// to stop them seeing it. An operator who discovers SmartScreen from a
	// driver rather than from this page has been let down by it.
	r.Contains(readme, "The panel builds it for you")
	r.Contains(readme, "SmartScreen")
	r.Contains(readme, "code-signing certificate")
	r.Contains(readme, ".pfx")
	for _, env := range []string{"PACENOTE_DATABASE_URL", "PACENOTE_DATA_DIR", "PACENOTE_LISTEN", "PACENOTE_METRICS_LISTEN"} {
		r.Contains(readme, env)
	}
}

// The licence is the GNU GPL version 3, verbatim.
//
// Verbatim is the whole assertion. The GPL is only the GPL if it is unaltered —
// a licence with a paragraph edited into it is a different licence that looks
// like this one, and somebody who reads the heading and stops has been misled
// about what they may do. So this checks the shape of the real document rather
// than a phrase in it: the heading, the version, the thirty numbered terms,
// both ends of the terms, and the fact that nothing has been appended after the
// closing section.
func TestTheLicenceIsTheGPLVerbatim(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	licence := read(t, "LICENSE")
	lines := strings.Split(licence, "\n")

	head := strings.Join(lines[:8], " ")
	r.Contains(head, "GNU GENERAL PUBLIC LICENSE")
	r.Contains(head, "Version 3, 29 June 2007")
	r.Contains(head, "Free Software Foundation")

	r.Contains(licence, "TERMS AND CONDITIONS")
	r.Contains(licence, "END OF TERMS AND CONDITIONS")
	r.Contains(licence, "How to Apply These Terms to Your New Programs")

	// The GPL has sixteen numbered terms plus a preamble of definitions. They
	// are counted rather than spot-checked because a licence missing one of
	// them is the failure this test exists for.
	for _, term := range []string{
		"  0. Definitions.",
		"  1. Source Code.",
		"  4. Conveying Verbatim Copies.",
		"  5. Conveying Modified Source Versions.",
		"  6. Conveying Non-Source Forms.",
		"  11. Patents.",
		"  15. Disclaimer of Warranty.",
		"  16. Limitation of Liability.",
	} {
		r.Contains(licence, term, "the licence is missing %q", strings.TrimSpace(term))
	}

	// Nothing of ours after the FSF's closing words. An addition here is the
	// commonest way a GPL text stops being the GPL.
	tail := strings.TrimSpace(strings.Join(lines[len(lines)-4:], " "))
	r.Contains(tail, "why-not-lgpl",
		"something has been appended after the end of the licence")

	r.NotContains(strings.ToUpper(licence), "MIT LICENSE")
	r.NotContains(strings.ToUpper(licence), "APACHE LICENSE")
	r.NotContains(licence, "Community Edition",
		"the licence still carries the terms it replaced")
}

func TestChangelogHasASectionAReleaseCanQuote(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	changelog := read(t, "CHANGELOG.md")

	r.Contains(changelog, "## [Unreleased]", "somewhere to write the next change down as it lands")
	r.Regexp(`(?m)^## \[\d+\.\d+\.\d+\]`, changelog,
		"the release notes are this file's sections, so there has to be at least one")
}

// operatorCopy is every file whose words an operator reads. The Dockerfile and
// the workflows are not here: those are read by whoever maintains this.
func operatorCopy() []string {
	return []string{"packaging/README.md", "LICENSE", "CHANGELOG.md", "packaging/docker-compose.yml"}
}

func TestOperatorCopyHasNoExclamationMarksAndNoEmoji(t *testing.T) {
	t.Parallel()
	for _, name := range operatorCopy() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			text := read(t, name)
			for i, line := range strings.Split(text, "\n") {
				// "#!" is a shebang and "!=" is an operator; neither is a
				// raised voice.
				stripped := strings.NewReplacer("#!", "", "!=", "", "[!", "").Replace(line)
				r.NotContainsf(stripped, "!", "%s line %d raises its voice", name, i+1)
				for _, ch := range line {
					r.Falsef(isEmoji(ch), "%s line %d has an emoji: %q", name, i+1, string(ch))
				}
			}
		})
	}
}

func TestOperatorCopyUsesSentenceCaseHeadings(t *testing.T) {
	t.Parallel()
	// Names that are capitalised wherever they appear. Everything else in a
	// heading after the first word is lower case, because title case reads as
	// marketing.
	proper := map[string]bool{
		"Pacenote": true, "PostgreSQL": true, "Docker": true, "Windows": true,
		"GitHub": true, "Anthropic": true, "Prometheus": true, "SmartScreen": true,
		"Linux": true, "Unreleased": true, "Added": true, "Changed": true,
		"Fixed": true, "Removed": true, "Security": true, "Deprecated": true,
		"Upgrade": true, "Known": true, "Licence": true, "Ports": true,
	}
	heading := regexp.MustCompile(`(?m)^#{1,6} +(.*)$`)
	word := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

	for _, name := range []string{"packaging/README.md", "CHANGELOG.md"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			for _, m := range heading.FindAllStringSubmatch(read(t, name), -1) {
				words := strings.Fields(m[1])
				for i, w := range words {
					w = strings.Trim(w, "[]().,:`")
					if i == 0 || !word.MatchString(w) || proper[w] {
						continue
					}
					r.Falsef(w[0] >= 'A' && w[0] <= 'Z',
						"%q is title case — %q should be lower", m[1], w)
				}
			}
		})
	}
}

// isEmoji covers the blocks an emoji actually comes from. It is deliberately
// not "anything above ASCII": the copy is full of em dashes and they belong.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // pictographs, transport, supplemental
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols and dingbats
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r == 0x2B50 || r == 0x2B55 || r == 0x203C || r == 0x2049:
		return true
	default:
		return false
	}
}
