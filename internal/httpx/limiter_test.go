package httpx_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/httpx"
)

func TestLimiterBurstThenRefusal(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// Five immediately, then one a minute — the setup token's limits.
	l := httpx.NewLimiter(1.0/60, 5, 60.0/60, 60)
	for i := range 5 {
		r.True(l.Allow("203.0.113.4"), "attempt %d should be inside the burst", i+1)
	}
	r.False(l.Allow("203.0.113.4"), "the sixth attempt from one address is refused")
}

func TestLimiterIsPerKey(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	l := httpx.NewLimiter(1.0/60, 2, 60.0/60, 60)
	r.True(l.Allow("a"))
	r.True(l.Allow("a"))
	r.False(l.Allow("a"))
	r.True(l.Allow("b"), "one address using up its budget does not lock out another")
}

func TestLimiterGlobalCeiling(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	// A generous per-key allowance under a tight global one: the ceiling is
	// what stops a flood spread across many addresses.
	l := httpx.NewLimiter(10, 10, 0, 3)
	allowed := 0
	for i := range 20 {
		if l.Allow(strconv.Itoa(i)) {
			allowed++
		}
	}
	r.Equal(3, allowed)
}

func TestLimiterRetryIsAdvice(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	l := httpx.NewLimiter(1.0/60, 5, 1, 60)
	r.Positive(l.Retry())
}
