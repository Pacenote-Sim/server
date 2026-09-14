package api

import (
	"math"

	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/httpx"
)

// Class is one kind of call, for rate limiting. A class has its own bucket per
// device and its own ceiling over every device at once, because the intervals
// the contract gives them differ by three orders of magnitude: a live sample is
// a second apart and a summary is half a minute.
type Class int

// The rate-limited classes.
const (
	// ClassRead is GET /me and GET /reference.
	ClassRead Class = iota
	// ClassWrite is PUT /stints/{id} and POST /stints/{id}/laps.
	ClassWrite
	// ClassLive is POST /live.
	ClassLive
	// ClassField is POST /field.
	ClassField
	// ClassSummary is PUT /stints/{id}/summary.
	ClassSummary
	// ClassTTS is POST /tts.
	ClassTTS
	// ClassPair is POST /pair/start and POST /pair/poll, which are
	// unauthenticated and so are limited by address instead of by device.
	ClassPair
	classCount
)

// The rates this server sets for itself, for the classes the contract does not
// give an interval to. They are per device and per second.
//
// Writes are generous on purpose: the intervals in discovery govern a client
// that is connected, and a client that has been offline for a session drains
// its whole queue the moment it reconnects. Refusing that drain would be
// refusing the case the offline queue exists for.
const (
	ReadsPerSecond  = 5.0
	ReadBurst       = 30.0
	WritesPerSecond = 5.0
	WriteBurst      = 60.0
	TTSPerSecond    = 2.0
	TTSBurst        = 10.0
)

// PairIntervalS is the floor a client must not poll faster than, published as
// [wire.PairStart.IntervalS] and enforced by the limiter, so the number a
// client is given and the number it is held to are the same number.
const PairIntervalS = 2

// PairBurst is how many pairing calls one address may make at once. It covers a
// client that starts a pairing and polls immediately, several times over, and
// it is small enough that walking the user-code space is not worth beginning.
const PairBurst = 10.0

// IntervalBurst is the slack on the three classes whose rate comes from
// discovery. A client that sends at exactly the published interval must never
// be refused, so the bucket has to absorb the jitter of a timer and a network;
// four calls is that and no more.
const IntervalBurst = 4.0

// The ceilings over every device at once, per second and in burst. They are
// what stops a flood spread across many tokens, which a per-device bucket
// cannot see.
const (
	GlobalReadsPerSecond  = 500.0
	GlobalWritesPerSecond = 200.0
	GlobalLivePerSecond   = 500.0
	GlobalFieldPerSecond  = 100.0
	GlobalOtherPerSecond  = 50.0
	globalBurstFactor     = 2.0
)

// Limiter is the whole server's rate limiting: a token bucket per device per
// class, each under a ceiling shared by every device.
//
// It is built from the [wire.Limits] discovery serves, so the intervals a
// client is told to obey and the intervals this refuses are the same constants.
// A client that honours the document is never refused.
type Limiter struct {
	classes [classCount]*httpx.Limiter
}

// NewLimiter builds the limiter for one set of published limits.
func NewLimiter(l wire.Limits) *Limiter {
	rate := func(intervalMs int) float64 {
		if intervalMs <= 0 {
			return 0
		}
		return 1000 / float64(intervalMs)
	}
	var lim Limiter
	lim.classes[ClassRead] = httpx.NewLimiter(ReadsPerSecond, ReadBurst,
		GlobalReadsPerSecond, GlobalReadsPerSecond*globalBurstFactor)
	lim.classes[ClassWrite] = httpx.NewLimiter(WritesPerSecond, WriteBurst,
		GlobalWritesPerSecond, GlobalWritesPerSecond*globalBurstFactor)
	lim.classes[ClassLive] = httpx.NewLimiter(rate(l.LiveIntervalMs), IntervalBurst,
		GlobalLivePerSecond, GlobalLivePerSecond*globalBurstFactor)
	lim.classes[ClassField] = httpx.NewLimiter(rate(l.FieldIntervalMs), IntervalBurst,
		GlobalFieldPerSecond, GlobalFieldPerSecond*globalBurstFactor)
	lim.classes[ClassSummary] = httpx.NewLimiter(rate(l.SummaryIntervalMs), IntervalBurst,
		GlobalOtherPerSecond, GlobalOtherPerSecond*globalBurstFactor)
	lim.classes[ClassTTS] = httpx.NewLimiter(TTSPerSecond, TTSBurst,
		GlobalOtherPerSecond, GlobalOtherPerSecond*globalBurstFactor)
	lim.classes[ClassPair] = httpx.NewLimiter(1.0/PairIntervalS, PairBurst,
		GlobalOtherPerSecond, GlobalOtherPerSecond*globalBurstFactor)
	return &lim
}

// Allow reports whether one call of this class from this key may proceed, and
// when to try again if it may not. The key is the device's token prefix for an
// authenticated call and the caller's address for pairing.
func (l *Limiter) Allow(c Class, key string) (ok bool, retryAfterS int) {
	if c < 0 || c >= classCount || l.classes[c] == nil {
		return true, 0
	}
	if l.classes[c].Allow(key) {
		return true, 0
	}
	return false, int(math.Ceil(l.classes[c].Retry().Seconds()))
}
