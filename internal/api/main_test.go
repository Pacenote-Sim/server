package api_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if a test leaves a goroutine behind.
//
// This package starts one of its own — the sweeper that clears out expired
// idempotency keys and finished pairings — and it is owned by the context the
// caller passes, so a cancelled phase must not leave it running. The pool and
// the test servers are the other two things that could strand one.
//
// The two ignores are the database driver's and the test client's own
// background workers, which the standard library and pgx park on an idle
// connection; neither is this package's to stop.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}
