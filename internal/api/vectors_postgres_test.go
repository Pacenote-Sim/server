//go:build postgres

package api_test

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
)

// update rewrites the fixtures in [goldenDir] from what the server actually
// produced. It needs a database, so it only does anything under the postgres
// build tag, and it is not something to reach for: a fixture that changes is a
// change to the contract two programs are built against.
var update = flag.Bool("update", false, "rewrite the request and response fixtures")

// protocolGolden reads one of the trace codec's own golden vectors, so the
// traces in these fixtures are the same bytes the client's tests use rather
// than something invented here.
//
// The module is located the way testdata/README.md in that module says to
// locate it, which is the same thing a consumer in another repository does.
func protocolGolden(tb testing.TB, name string) []wire.TracePoint {
	tb.Helper()
	path := filepath.Join(protocolDir(tb), "trace", "testdata", "golden", name+".json")
	raw, err := os.ReadFile(path)
	require.NoError(tb, err, "the protocol module's golden vectors must be readable")
	var pts []wire.TracePoint
	require.NoError(tb, json.Unmarshal(raw, &pts))
	return pts
}

var protocolDirOnce = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/pacenote-sim/protocol").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
})

func protocolDir(tb testing.TB) string {
	tb.Helper()
	dir, err := protocolDirOnce()
	require.NoError(tb, err, "go list must be able to find github.com/pacenote-sim/protocol")
	return dir
}

// result is one measured call: what it returned and how long it took.
type result[T any] struct {
	value T
	took  time.Duration
}

// timed runs fn once and reports how long it took, for the assertions that
// stand in for a benchmark on a path with a budget.
func timed[T any](fn func() T) result[T] {
	at := time.Now()
	v := fn()
	return result[T]{value: v, took: time.Since(at)}
}

// sampleLap is one lap carrying a real trace: the protocol module's own
// 300-point golden vector, which is the shape every budget here is
// quoted against and the same bytes the client's tests use.
func sampleLap(tb testing.TB, number, lapMs int) wire.Lap {
	tb.Helper()
	return wire.Lap{
		Number:    number,
		LapMs:     lapMs,
		Kind:      wire.KindClean,
		StartedAt: lapStartedAt.Add(time.Duration(number) * 90 * time.Second),
		Trace:     protocolGolden(tb, "lap-300"),
	}
}

// lapStartedAt is the instant the laps of a fixture begin, so a committed
// request document is the same bytes on every run.
var lapStartedAt = time.Date(2026, 9, 12, 14, 3, 11, 0, time.FixedZone("CEST", 2*60*60))

// uuidV7 builds a time-ordered identifier the way a client does: the first
// forty-eight bits are the millisecond it was made, so the identifiers of one
// session sort into the order they were created in. The rest is the tail,
// given rather than taken so a fixture is the same every run.
func uuidV7(at time.Time, tail []byte) string {
	var u [16]byte
	binary.BigEndian.PutUint64(u[:8], uint64(at.UnixMilli())<<16)
	for i := range 10 {
		u[6+i] = tail[i%len(tail)]
	}
	u[6] = 0x70 | (u[6] & 0x0f) // version 7
	u[8] = 0x80 | (u[8] & 0x3f) // the RFC 4122 variant
	return hexUUID(u)
}

// randomUUIDv7 is [uuidV7] with a random tail, for the tests that need a fresh
// identifier rather than a reproducible one. The tail is the full ten bytes the
// format leaves free, because a test that makes thousands of them must not
// collide with itself.
func randomUUIDv7(tb testing.TB, at time.Time) string {
	tb.Helper()
	tail := make([]byte, 10)
	_, err := rand.Read(tail)
	require.NoError(tb, err)
	return uuidV7(at, tail)
}

func hexUUID(u [16]byte) string {
	const hexDigits = "0123456789abcdef"
	var b [36]byte
	at := 0
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b[at] = '-'
			at++
		}
		b[at] = hexDigits[c>>4]
		b[at+1] = hexDigits[c&0x0f]
		at += 2
	}
	return string(b[:])
}
