package httpx_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if a test leaves a goroutine behind. Serve starts
// one per listener and the shutdown path is the thing most likely to strand it,
// so this is the assertion that matters most in this package.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
