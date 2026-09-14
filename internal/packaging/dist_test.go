//go:build dist

// These tests read what `make dist` produced, so they are behind a build tag:
// `go test ./...` has to pass on a machine that has never run a release build.
//
//	make dist && make test-dist
package packaging_test

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/packaging"
)

// distDir is where GoReleaser writes, relative to this package.
const distDir = root + "/dist"

// startTimeout bounds how long a packaged binary may take to print where it is
// listening. It is generous: the point is to fail rather than hang.
const startTimeout = 30 * time.Second

// version reads the version out of the archives that are actually there, so the
// test works for a snapshot and for a tag without being told which.
func version(tb testing.TB) string {
	tb.Helper()
	matches, err := filepath.Glob(filepath.Join(distDir, packaging.Project+"_*_*_*.zip"))
	require.NoError(tb, err)
	require.NotEmptyf(tb, matches, "no archives in %s — run make dist first", distDir)

	name := strings.TrimSuffix(filepath.Base(matches[0]), ".zip")
	parts := strings.Split(strings.TrimPrefix(name, packaging.Project+"_"), "_")
	require.Lenf(tb, parts, 3, "%s is not <project>_<version>_<os>_<arch>.zip", name)
	return parts[0]
}

func TestDistHasOneArchivePerSupportedPlatform(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	v := version(t)

	want := make([]string, 0, len(packaging.Platforms()))
	for _, p := range packaging.Platforms() {
		want = append(want, packaging.ArchiveName(v, p))
	}
	slices.Sort(want)

	matches, err := filepath.Glob(filepath.Join(distDir, packaging.Project+"_*.zip"))
	r.NoError(err)
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, filepath.Base(m))
	}
	slices.Sort(got)

	r.Equal(want, got, "the release built something other than the supported platforms")
}

func TestEveryArchiveCarriesTheBinaryAndTheDocuments(t *testing.T) {
	t.Parallel()
	v := version(t)

	for _, p := range packaging.Platforms() {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			zr, err := zip.OpenReader(filepath.Join(distDir, packaging.ArchiveName(v, p)))
			r.NoError(err)
			t.Cleanup(func() { _ = zr.Close() })

			names := make([]string, 0, len(zr.File))
			for _, f := range zr.File {
				names = append(names, f.Name)
				r.NotEmptyf(f.UncompressedSize64, "%s is empty", f.Name)
			}
			slices.Sort(names)
			r.Equal(packaging.Contents(p), names)
		})
	}
}

func TestChecksumsCoverEveryArchive(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	raw, err := os.ReadFile(filepath.Join(distDir, packaging.Checksums))
	r.NoError(err, "the README tells an operator to check this file, so it has to be there")

	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		r.Lenf(fields, 2, "%q is not a sha256sum line", line)
		listed[fields[1]] = fields[0]
	}

	v := version(t)
	for _, p := range packaging.Platforms() {
		name := packaging.ArchiveName(v, p)
		sum, ok := listed[name]
		r.Truef(ok, "%s is not in %s", name, packaging.Checksums)

		f, err := os.Open(filepath.Join(distDir, name))
		r.NoError(err)
		h := sha256.New()
		_, err = io.Copy(h, f)
		r.NoError(err)
		r.NoError(f.Close())
		r.Equalf(sum, hex.EncodeToString(h.Sum(nil)), "%s does not match its checksum", name)
	}
}

// TestThePackagedBinaryServesTheWizard is the one that matters: unzip what an
// operator downloads, run it out of the directory it unzipped into, and see the
// setup page. No database — a server that has never been configured and has
// been told nothing opens the wizard on its first question.
func TestThePackagedBinaryServesTheWizard(t *testing.T) {
	t.Parallel()
	base := unpackForThisMachine(t)
	addr := start(t, base, nil)

	body, status := get(t, "http://"+addr+"/setup")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "Set up this server")
	require.Contains(t, body, "Setup token")
}

// TestThePackagedBinaryRedirectsToTheWizard checks the thing an operator
// actually does, which is open the host and port with nothing after it.
func TestThePackagedBinaryRedirectsToTheWizard(t *testing.T) {
	t.Parallel()
	base := unpackForThisMachine(t)
	addr := start(t, base, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", http.NoBody)
	require.NoError(t, err)
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/setup", resp.Header.Get("Location"))
}

func TestThePackagedBinaryReportsItsVersion(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	base := unpackForThisMachine(t)

	ctx, cancel := context.WithTimeout(t.Context(), startTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(base, packaging.BinaryName(runtime.GOOS)), "-version").Output()
	r.NoError(err)

	line := strings.TrimSpace(string(out))
	r.Contains(line, packaging.Binary)
	r.Contains(line, runtime.GOOS+"/"+runtime.GOARCH)
	r.NotContains(line, "unknown", "a release binary that cannot say which build it is is not a release binary")
	r.Containsf(line, version(t), "%q does not carry the version the archive is named for", line)
}

// --- the harness -----------------------------------------------------------

// unpackForThisMachine extracts the archive for the platform the test is
// running on into a temporary directory, and returns that directory.
func unpackForThisMachine(tb testing.TB) string {
	tb.Helper()
	r := require.New(tb)

	here := packaging.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if !slices.Contains(packaging.Platforms(), here) {
		tb.Skipf("%s is not a platform a release ships, so there is no archive to run", here)
	}

	archive := filepath.Join(distDir, packaging.ArchiveName(version(tb), here))
	zr, err := zip.OpenReader(archive)
	r.NoError(err)
	defer func() { _ = zr.Close() }()

	base := tb.TempDir()
	for _, f := range zr.File {
		// The archive is flat by construction, and a name with a separator in
		// it would be a path-traversal bug rather than a feature.
		r.NotContains(f.Name, "/")

		rc, err := f.Open()
		r.NoError(err)
		out, err := os.OpenFile(
			filepath.Join(base, f.Name),
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
			f.Mode().Perm(),
		)
		r.NoError(err)
		_, err = io.Copy(out, rc)
		r.NoError(err)
		r.NoError(out.Close())
		r.NoError(rc.Close())
	}
	return base
}

var listening = regexp.MustCompile(`http://([^/\s]+)/setup`)

// start runs the packaged binary from base with a data directory that does not
// exist yet, waits for it to say where it is listening, and returns that
// address. The process is stopped when the test finishes.
func start(tb testing.TB, base string, env []string) string {
	tb.Helper()
	r := require.New(tb)

	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, filepath.Join(base, packaging.BinaryName(runtime.GOOS)))
	cmd.Dir = base
	cmd.Env = append(os.Environ(),
		// A data directory that exists but holds no configuration is not a
		// first run, and the server says so rather than opening the wizard. So
		// name one inside the temporary directory that is not there yet.
		"PACENOTE_DATA_DIR="+filepath.Join(tb.TempDir(), "pacenote-data"),
		// Port 0, so tests run in parallel and on a machine already running a
		// server. The banner prints what was bound.
		"PACENOTE_LISTEN=127.0.0.1:0",
		"PACENOTE_METRICS_LISTEN=127.0.0.1:0",
		"PACENOTE_LOG_LEVEL=warn",
	)
	cmd.Env = append(cmd.Env, env...)

	stdout, err := cmd.StdoutPipe()
	r.NoError(err)
	cmd.Stderr = os.Stderr
	r.NoError(cmd.Start())
	tb.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	type found struct {
		addr string
		text string
	}
	ch := make(chan found, 1)
	go func() {
		var (
			seen strings.Builder
			addr string
		)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			seen.WriteString(sc.Text() + "\n")
			if m := listening.FindStringSubmatch(sc.Text()); m != nil {
				addr = m[1]
			}
			// The banner is three lines and the address is on the second, so
			// wait for the third as well rather than reporting half of it.
			if addr != "" && strings.Contains(seen.String(), "Token") {
				ch <- found{addr: addr, text: seen.String()}
				break
			}
		}
		// Drain, so the server never blocks writing to a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
	}()

	select {
	case f := <-ch:
		r.Contains(f.text, "first run", "the banner is what tells an operator this is setup mode")
		r.Contains(f.text, "Token", "the setup token is the whole reason a stranger cannot take the server")
		return f.addr
	case <-time.After(startTimeout):
		tb.Fatalf("the packaged binary did not say where it was listening within %s", startTimeout)
		return ""
	}
}

func get(tb testing.TB, url string) (string, int) {
	tb.Helper()
	r := require.New(tb)

	req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, url, http.NoBody)
	r.NoError(err)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	r.NoError(err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	r.NoError(err)
	return string(body), resp.StatusCode
}
