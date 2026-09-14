package api

import (
	"strconv"
	"strings"
)

// olderThan reports whether the version a client claims is older than the
// minimum this server accepts.
//
// The comparison is major, minor, patch, each as a number, which is what the
// contract's "1.0.0" spelling is. A pre-release or build suffix — "1.2.0-rc1",
// "1.2.0+deadbeef" — is dropped before comparing, so a release candidate counts
// as its release: refusing someone testing a build against a server is the
// opposite of useful, and the check exists to stop uploads from clients that
// predate a wire change, not to police version strings.
//
// Anything unparseable is treated as new enough. A client that sends nonsense
// in the header has not claimed to be old, and a false refusal here stops a
// driver uploading laps that were fine.
func olderThan(got, minimum string) bool {
	g, ok := parseVersion(got)
	if !ok {
		return false
	}
	m, ok := parseVersion(minimum)
	if !ok {
		return false
	}
	for i := range g {
		if g[i] != m[i] {
			return g[i] < m[i]
		}
	}
	return false
}

// parseVersion reads "1.2.3", with or without a leading v and with or without
// the minor and patch. A missing part is zero, so "1" is 1.0.0.
func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return out, false
	}
	parts := strings.SplitN(s, ".", 4)
	if len(parts) > len(out) {
		return out, false
	}
	for i := range parts {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
