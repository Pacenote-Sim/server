package buildinfo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/buildinfo"
)

func TestShort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		info buildinfo.Info
		want string
	}{
		{
			name: "a released build",
			info: buildinfo.Info{Version: "v1.2.3", Revision: "abcdef0123456789"},
			want: "v1.2.3",
		},
		{
			name: "a build from a checkout",
			info: buildinfo.Info{Version: "(devel)", Revision: "abcdef0123456789"},
			want: "abcdef012345",
		},
		{
			name: "a dirty working tree says so",
			info: buildinfo.Info{Version: "v1.2.3", Revision: "abcdef0123456789", Modified: true},
			want: "v1.2.3+dirty",
		},
		{
			name: "nothing to go on",
			info: buildinfo.Info{Version: buildinfo.Unknown, Revision: buildinfo.Unknown},
			want: buildinfo.Unknown,
		},
		{
			name: "a short revision is not sliced past its end",
			info: buildinfo.Info{Version: "(devel)", Revision: "abc"},
			want: "abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			r.Equal(tc.want, tc.info.Short())
		})
	}
}

func TestLongCarriesTheTarget(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	info := buildinfo.Info{
		Version: "v1.0.0", Revision: "abc", GoVersion: "go1.26.5",
		OS: "linux", Arch: "arm64",
	}
	r.Equal("v1.0.0 (go1.26.5, linux/arm64)", info.Long())
}

func TestReadAnswersSomething(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	info := buildinfo.Read()
	r.NotEmpty(info.GoVersion)
	r.NotEmpty(info.OS)
	r.NotEmpty(info.Arch)
	r.NotEmpty(info.Short())
	r.Contains(info.Long(), info.OS)
	r.Equal(info, buildinfo.Read(), "the answer is cached, so it cannot change under a caller")
}
