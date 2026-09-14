package clientbuild_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/clientbuild"
)

// What the build page says about a real Windows binary.
//
// Every other test in this package uses a fixture, which is right for the
// stamping and the signing: those are about bytes at offsets, and a fixture is
// deterministic where eleven megabytes of another repository's build output is
// not. But the version, the commit and the platform on the build page come from
// the build information the Go toolchain writes inside a binary, and no fixture
// carries any — so that half of the page has never been read from an actual one.
//
// This builds one. It is the only test in this repository that shells out to
// the toolchain, and it skips rather than fails where it cannot: a machine with
// no Go on its path is not a machine where this is the interesting failure.
func TestWhatThePageSaysAboutARealWindowsClient(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	path := buildTinyWindowsClient(t)
	got := clientbuild.Source{Path: path}.Inspect()

	r.True(got.Present)
	r.Equal("x86-64", got.Machine, "a windows/amd64 build was read as something else")
	r.Equal("windows", got.GOOS)
	r.Equal("amd64", got.GOARCH)
	r.NotEmpty(got.Module, "the module the client was built from is not on the page")
	r.Len(got.SHA256, 64)
	r.True(got.Describes(), "the page has nothing to show under Version")

	// A real Go binary carries no reserved region, so this server cannot stamp
	// one — and the page has to say which of the two problems it is rather than
	// "that file is wrong".
	r.False(got.Ready())
	r.Contains(got.Problem, "no reserved region")
}

// buildTinyWindowsClient compiles a do-nothing program for windows/amd64 and
// returns where it landed.
func buildTinyWindowsClient(tb testing.TB) string {
	tb.Helper()
	r := require.New(tb)

	if _, err := exec.LookPath("go"); err != nil {
		tb.Skip("no Go toolchain on this machine, so there is nothing to build a client with")
	}

	dir := tb.TempDir()
	r.NoError(os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module pacenote.example/tinyclient\n\ngo "+goLine()+"\n"), 0o600))
	r.NoError(os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o600))

	out := filepath.Join(dir, "tinyclient.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "GOFLAGS=", "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		tb.Skipf("a windows/amd64 client could not be built here: %v: %s", err, combined)
	}
	return out
}

// goLine is the language version to put in the throwaway module, taken from the
// toolchain doing the building so that a newer one does not refuse it.
func goLine() string {
	v := runtime.Version() // "go1.26.1"
	if len(v) > 2 && v[:2] == "go" {
		v = v[2:]
	}
	// Only the major and minor belong in a go directive.
	dots := 0
	for i := range len(v) {
		if v[i] == '.' {
			dots++
			if dots == 2 {
				return v[:i]
			}
		}
	}
	return v
}
