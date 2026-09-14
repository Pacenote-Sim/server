// Package buildinfo reports which build of the server is running.
//
// Almost everything here comes from [runtime/debug.ReadBuildInfo], so there is
// no version constant that someone forgets to bump. A binary built by `go
// build` from a git checkout carries its revision, and since Go 1.24 it carries
// the tag it was built from as well; one installed by `go install
// module@v1.2.3` carries that version; one built from a plain directory carries
// neither and says so.
//
// # The one exception
//
// A container image builds from a source tree with no .git in it — copying a
// repository's history into a build context to learn one string is not a trade
// worth making — so ReadBuildInfo has nothing to report there. For that case
// and that case only, [Stamp] is set by the linker:
//
//	go build -ldflags "-X github.com/pacenote-sim/server/internal/buildinfo.Stamp=v1.2.3"
//
// It wins when it is set, because the only thing that sets it is a release
// build that already knows its tag. Everywhere else it is empty and the build
// information answers, which is why there is no version constant in this
// repository to keep in step with a tag.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Unknown is what every accessor returns when the build carries no such
// information. It is a word rather than an empty string so an operator reading
// a diagnostics page sees an answer instead of a gap.
const Unknown = "unknown"

// Stamp is the version a release build writes in with -ldflags -X, for the
// builds that carry no version control information of their own. It is empty in
// every other build, and an empty Stamp changes nothing.
//
// It is a variable rather than a constant because the linker can only write to
// a variable, and it is exported for the same reason: -X names a symbol.
var Stamp string

// Info is one build, described.
type Info struct {
	// Version is the module version, "v1.2.3", or [Unknown] for a build that
	// was not installed from a module proxy.
	Version string
	// Revision is the full git commit hash, or [Unknown].
	Revision string
	// Time is the commit time in RFC 3339, or [Unknown].
	Time string
	// Modified reports whether the working tree had uncommitted changes.
	Modified bool
	// GoVersion is the toolchain that built the binary.
	GoVersion string
	// OS and Arch are the target this binary was built for.
	OS, Arch string
}

// Short is the one-line version an operator sees: the module version when there
// is one, otherwise the first twelve characters of the revision, with "+dirty"
// appended when the working tree was not clean.
func (i Info) Short() string {
	v := i.Version
	if v == Unknown || v == "(devel)" {
		if i.Revision == Unknown {
			v = Unknown
		} else {
			v = i.Revision[:min(12, len(i.Revision))]
		}
	}
	if i.Modified {
		v += "+dirty"
	}
	return v
}

// Long is the diagnostics line: the short version, the toolchain and the
// target, as one string.
func (i Info) Long() string {
	var b strings.Builder
	b.WriteString(i.Short())
	b.WriteString(" (")
	b.WriteString(i.GoVersion)
	b.WriteString(", ")
	b.WriteString(i.OS)
	b.WriteString("/")
	b.WriteString(i.Arch)
	b.WriteString(")")
	return b.String()
}

var read = sync.OnceValue(func() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return withStamp(fromBuildInfo(bi), Stamp)
})

// fromBuildInfo is everything [read] does except reading, so that it is a
// function of its input and can be tested with a build that has the settings a
// test binary never carries — a test binary has no vcs.revision, so the branches
// that matter most in production are exactly the ones nothing would otherwise
// reach.
//
// A nil build info is a binary built in a way that carries none, which is not an
// error: every field already holds [Unknown] or the running toolchain's answer.
func fromBuildInfo(bi *debug.BuildInfo) Info {
	i := Info{
		Version:   Unknown,
		Revision:  Unknown,
		Time:      Unknown,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if bi == nil {
		return i
	}
	if bi.Main.Version != "" {
		i.Version = bi.Main.Version
	}
	if bi.GoVersion != "" {
		i.GoVersion = bi.GoVersion
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				i.Revision = s.Value
			}
		case "vcs.time":
			if s.Value != "" {
				i.Time = s.Value
			}
		case "vcs.modified":
			i.Modified = s.Value == "true"
		case "GOOS":
			if s.Value != "" {
				i.OS = s.Value
			}
		case "GOARCH":
			if s.Value != "" {
				i.Arch = s.Value
			}
		}
	}
	return i
}

// withStamp applies a linker-written version over whatever the build
// information had, which for a release container image is the only version
// there is. An empty stamp changes nothing.
//
// The stamp is a parameter rather than read from [Stamp] so that this stays a
// function of its inputs, and so a test does not have to write to a package
// variable to reach it.
func withStamp(i Info, stamp string) Info {
	if stamp != "" {
		i.Version = stamp
	}
	return i
}

// Read returns the build information of the running binary. It is read once and
// cached, so calling it on a request path costs nothing.
func Read() Info { return read() }
