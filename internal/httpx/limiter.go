package httpx

import (
	"math"
	"sync"
	"time"
)

// Limiter is a token bucket per key, plus a ceiling over all keys.
//
// It is built for the setup token endpoint, which needs it first: an
// unauthenticated form that compares a secret has to cost an attacker
// something. The same type carries the per-device limits, which is why the
// limits are values rather than constants.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	global  bucket

	rate        float64 // tokens per second, per key
	burst       float64
	globalRate  float64
	globalBurst float64
	now         func() time.Time
	last        time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// NewLimiter builds a limiter allowing burst attempts immediately and then one
// every 1/rate seconds, per key, with globalBurst and globalRate the same over
// every key at once.
func NewLimiter(rate, burst, globalRate, globalBurst float64) *Limiter {
	l := &Limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
	l.global = bucket{tokens: globalBurst}
	l.globalRate, l.globalBurst = globalRate, globalBurst
	return l
}

// Allow reports whether one attempt from key may proceed, and takes a token if
// it may.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	if !l.take(&l.global, l.globalRate, l.globalBurst, now) {
		return false
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	return l.take(b, l.rate, l.burst, now)
}

// Retry is how long a caller should wait before the next attempt would be
// allowed. It is an estimate for a message, not a promise.
func (l *Limiter) Retry() time.Duration {
	if l.rate <= 0 {
		return time.Minute
	}
	return time.Duration(math.Ceil(1/l.rate)) * time.Second
}

func (l *Limiter) take(b *bucket, rate, burst float64, now time.Time) bool {
	if !b.seen.IsZero() {
		b.tokens = math.Min(burst, b.tokens+now.Sub(b.seen).Seconds()*rate)
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have been idle long enough to have refilled, so a
// flood of one-off addresses cannot grow the map without bound.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.last) < time.Minute {
		return
	}
	l.last = now
	if l.rate <= 0 {
		return
	}
	idle := time.Duration(l.burst/l.rate) * time.Second
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idle {
			delete(l.buckets, k)
		}
	}
}
