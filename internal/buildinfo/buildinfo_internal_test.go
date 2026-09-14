package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithStamp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    Info
		stamp string
		want  string
	}{
		{
			name:  "a container image has no build information to go on",
			in:    Info{Version: "(devel)", Revision: Unknown},
			stamp: "v1.2.3",
			want:  "v1.2.3",
		},
		{
			name:  "an empty stamp leaves the build information alone",
			in:    Info{Version: "v1.2.3", Revision: "abcdef0123456789"},
			stamp: "",
			want:  "v1.2.3",
		},
		{
			name:  "a stamp wins, because only a release build sets one",
			in:    Info{Version: "v1.2.3", Revision: "abcdef0123456789"},
			stamp: "v1.3.0",
			want:  "v1.3.0",
		},
		{
			name:  "a dirty tree still says so",
			in:    Info{Version: "(devel)", Revision: "abcdef0123456789", Modified: true},
			stamp: "v1.2.3",
			want:  "v1.2.3+dirty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, withStamp(tc.in, tc.stamp).Short())
		})
	}
}

// The linker can only write to a package-level string variable, so this is the
// shape -X depends on. A change of type or of name breaks the release build in
// a way nothing else would notice.
func TestStampIsWhatTheLinkerCanWrite(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	r.Empty(Stamp, "a build from source carries no stamp; only a release sets one")
}

// A test binary carries no vcs.revision, no vcs.time and no vcs.modified, so
// the branches that fill in every field an operator actually reads are the ones
// nothing else reaches. This is why fromBuildInfo takes its input.
func TestFromBuildInfoReadsEverythingAReleaseCarries(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	got := fromBuildInfo(&debug.BuildInfo{
		Main:      debug.Module{Version: "v1.4.0"},
		GoVersion: "go1.26.5",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "9f3c1ab"},
			{Key: "vcs.time", Value: "2026-09-14T10:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "GOOS", Value: "windows"},
			{Key: "GOARCH", Value: "arm64"},
			{Key: "somethingelse", Value: "ignored"},
		},
	})

	r.Equal("v1.4.0", got.Version)
	r.Equal("go1.26.5", got.GoVersion)
	r.Equal("9f3c1ab", got.Revision)
	r.Equal("2026-09-14T10:00:00Z", got.Time)
	r.True(got.Modified)
	r.Equal("windows", got.OS)
	r.Equal("arm64", got.Arch)
}

// Every one of those settings can be present and empty, which is not the same
// as absent and must not overwrite a good default with nothing.
func TestFromBuildInfoIgnoresEmptyValues(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	got := fromBuildInfo(&debug.BuildInfo{
		Main:      debug.Module{Version: ""},
		GoVersion: "",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: ""},
			{Key: "vcs.time", Value: ""},
			{Key: "vcs.modified", Value: "false"},
			{Key: "GOOS", Value: ""},
			{Key: "GOARCH", Value: ""},
		},
	})

	r.Equal(Unknown, got.Version)
	r.Equal(Unknown, got.Revision)
	r.Equal(Unknown, got.Time)
	r.False(got.Modified)
	r.Equal(runtime.Version(), got.GoVersion)
	r.Equal(runtime.GOOS, got.OS)
	r.Equal(runtime.GOARCH, got.Arch)
}

// A binary built in a way that carries no build information at all. It is not an
// error: everything is already Unknown or the running toolchain's answer.
func TestFromBuildInfoWithNothingAtAll(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	got := fromBuildInfo(nil)
	r.Equal(Unknown, got.Version)
	r.Equal(Unknown, got.Revision)
	r.Equal(Unknown, got.Time)
	r.False(got.Modified)
	r.Equal(runtime.Version(), got.GoVersion)
}
