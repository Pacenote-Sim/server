package api

import (
	"fmt"
	"strings"

	"github.com/pacenote-sim/protocol/wire"
)

// The bounds this server puts on the fields the contract leaves open. Each is
// far wider than any honest value and narrow enough that a column cannot be
// used as storage: a text field is a name, not a place to keep a document.
const (
	MaxNameLen   = 200
	MaxShortLen  = 64
	MaxSectors   = 64
	MaxLapNumber = 100_000
	MaxLapMs     = 3_600_000 // one hour, the same bound the trace codec puts on t
	MaxSpeedKmh  = 1_000
	MaxIncidents = 100_000
	// MaxCornersPerLap bounds a lap's corner analysis. No circuit has sixty-four
	// corners, and the client's own widest setting reports ninety-nine, so this
	// is wider than any real lap and narrow enough that the column cannot be
	// used as storage.
	MaxCornersPerLap = 64
	// MaxSetupValues bounds the named values of a setup sheet, and
	// MaxSetupTextLen the length of one setting's group, name, text or unit. A
	// real iRacing sheet is sixty to ninety lines of a few words each.
	MaxSetupValues  = 256
	MaxSetupTextLen = 200
	// MaxSetupUpdateCount bounds the simulator's own revision counter for the
	// sheet. A driver who changed their setup a million times in one sitting
	// did not.
	MaxSetupUpdateCount = 1_000_000
	// MaxSetupKpa, MaxSetupTempC and the tread percentage bound the normalised
	// core. Each is far outside what a tyre does and inside what the column can
	// mean.
	MaxSetupKpa   = 1_000
	MaxSetupTempC = 1_000
)

// fault is one thing wrong with a request: the sentence the driver is shown and
// the context the client logs. It is not an error — a body that fails
// validation is an answer, not a failure — so it does not implement error and
// cannot be returned up a stack by mistake.
type fault struct {
	Message string
	Detail  map[string]any
}

func invalidField(field, message string) *fault {
	return &fault{Message: message, Detail: map[string]any{"field": field}}
}

// validateStint checks the body of PUT /stints/{id}.
func validateStint(s wire.Stint) *fault {
	switch {
	case blank(s.Sim):
		return invalidField("sim", "That stint does not say which simulator it came from — the upload was dropped.")
	case len(s.Sim) > MaxShortLen:
		return invalidField("sim", "That simulator name is longer than this server stores — the upload was dropped.")
	case blank(s.TrackID):
		return invalidField("track_id", "That stint does not say which track it was driven on — the upload was dropped.")
	case len(s.TrackID) > MaxNameLen || len(s.Track) > MaxNameLen:
		return invalidField("track", "That track name is longer than this server stores — the upload was dropped.")
	case blank(s.Car):
		return invalidField("car", "That stint does not say which car was driven — the upload was dropped.")
	case len(s.Car) > MaxNameLen:
		return invalidField("car", "That car name is longer than this server stores — the upload was dropped.")
	case len(s.CarClass) > MaxShortLen:
		return invalidField("car_class", "That car class is longer than this server stores — the upload was dropped.")
	case !validSessionType(s.SessionType):
		return invalidField("session_type", "That stint names a session type this server does not know — the upload was dropped.")
	case s.StartedAt.IsZero():
		return invalidField("started_at", "That stint does not say when it started — the upload was dropped.")
	case len(s.Sectors) > MaxSectors:
		return invalidField("sectors", "That stint has more sectors than any circuit — the upload was dropped.")
	}
	if f := validateSetup(s.Setup); f != nil {
		return f
	}
	prev := -1.0
	for i, sec := range s.Sectors {
		if sec < 0 || sec > 1 || sec <= prev {
			return &fault{
				Message: "Those sector positions do not run from the start of the lap to its end — the upload was dropped.",
				Detail:  map[string]any{"field": "sectors", "index": i, "value": sec},
			}
		}
		prev = sec
	}
	return nil
}

// validateSetup checks the optional car setup of a stint. A nil setup is not a
// failure and never will be: a simulator that publishes none and a series that
// locks it away are the normal cases, and this endpoint's whole contract is
// that they cost nothing.
func validateSetup(s *wire.CarSetup) *fault {
	if s == nil {
		return nil
	}
	switch {
	case s.UpdateCount < 0 || s.UpdateCount > MaxSetupUpdateCount:
		return invalidField("setup",
			"That car setup has a revision number no garage could reach — the upload was dropped.")
	case len(s.Tyres) > len(validWheels):
		return invalidField("setup", "That car setup has more wheels than a car — the upload was dropped.")
	case len(s.Values) > MaxSetupValues:
		return &fault{
			Message: "That car setup carries more settings than this server stores — the upload was dropped.",
			Detail:  map[string]any{"field": "setup", "count": len(s.Values), "max": MaxSetupValues},
		}
	}

	seen := make(map[wire.Wheel]struct{}, len(s.Tyres))
	for i, t := range s.Tyres {
		if !validWheel(t.Wheel) {
			return &fault{
				Message: "That car setup names a wheel this server does not know — the upload was dropped.",
				Detail:  map[string]any{"field": "setup.tyres", "index": i, "wheel": string(t.Wheel)},
			}
		}
		if _, dup := seen[t.Wheel]; dup {
			return &fault{
				Message: "That car setup carries the same wheel twice — the upload was dropped, and this is a bug in the client.",
				Detail:  map[string]any{"field": "setup.tyres", "index": i, "wheel": string(t.Wheel)},
			}
		}
		seen[t.Wheel] = struct{}{}
		if f := validateTyre(t, i); f != nil {
			return f
		}
	}
	if f := validateSetupValue(s.RearWing, "setup.rear_wing", 0); f != nil {
		return f
	}
	for i := range s.Values {
		if f := validateSetupValue(&s.Values[i], "setup.values", i); f != nil {
			return f
		}
	}
	return nil
}

// validateTyre checks one wheel of a setup. Every reading is bounded from
// below as well as above: zero means "not published", and a negative pressure,
// temperature or tread depth is a client bug rather than a measurement.
func validateTyre(t wire.SetupTyre, i int) *fault {
	at := func(field string) *fault {
		return &fault{
			Message: "That car setup carries a tyre reading no tyre could produce — the upload was dropped.",
			Detail:  map[string]any{"field": "setup.tyres", "index": i, "wheel": string(t.Wheel), "reading": field},
		}
	}
	for field, kpa := range map[string]float64{"cold_kpa": t.ColdKpa, "hot_kpa": t.HotKpa} {
		if kpa < 0 || kpa > MaxSetupKpa {
			return at(field)
		}
	}
	for field, c := range map[string]float64{
		"temp_inner_c": t.TempInnerC, "temp_middle_c": t.TempMiddleC, "temp_outer_c": t.TempOuterC,
	} {
		if c < 0 || c > MaxSetupTempC {
			return at(field)
		}
	}
	for field, pct := range map[string]float64{
		"tread_inner_pct": t.TreadInnerPct, "tread_middle_pct": t.TreadMiddlePct, "tread_outer_pct": t.TreadOuterPct,
	} {
		if pct < 0 || pct > 100 {
			return at(field)
		}
	}
	return nil
}

// validateSetupValue checks one named setting. A nil value is the rear wing of
// a car that has none.
func validateSetupValue(v *wire.SetupValue, field string, i int) *fault {
	if v == nil {
		return nil
	}
	at := func(message string) *fault {
		return &fault{Message: message, Detail: map[string]any{"field": field, "index": i, "name": v.Name}}
	}
	switch {
	case blank(v.Name):
		return at("That car setup carries a setting with no name — the upload was dropped.")
	case len(v.Name) > MaxSetupTextLen || len(v.Group) > MaxSetupTextLen ||
		len(v.Text) > MaxSetupTextLen || len(v.Unit) > MaxSetupTextLen:
		return at("That car setup carries a setting longer than this server stores — the upload was dropped.")
	}
	return nil
}

// validWheels are the four spellings a setup may name, in the order a client
// sends them.
var validWheels = [...]wire.Wheel{wire.WheelLF, wire.WheelRF, wire.WheelLR, wire.WheelRR}

func validWheel(w wire.Wheel) bool {
	for _, known := range validWheels {
		if w == known {
			return true
		}
	}
	return false
}

// validateLapBatch checks the body of POST /stints/{id}/laps against the limits
// this server published, which are the ones the client read before it sent.
func validateLapBatch(b wire.LapBatch, limits wire.Limits) *fault {
	switch {
	case len(b.Laps) == 0:
		return invalidField("laps", "That upload carried no laps — nothing was stored.")
	case len(b.Laps) > limits.LapsPerRequest:
		return &fault{
			Message: fmt.Sprintf("That upload carried more than the %d laps this server takes at once — send it in smaller batches.", limits.LapsPerRequest),
			Detail:  map[string]any{"field": "laps", "count": len(b.Laps), "max": limits.LapsPerRequest},
		}
	}
	seen := make(map[int]struct{}, len(b.Laps))
	for i := range b.Laps {
		lap := &b.Laps[i]
		if f := validateLap(lap, i, limits); f != nil {
			return f
		}
		if _, dup := seen[lap.Number]; dup {
			return &fault{
				Message: "That upload carried the same lap number twice — nothing was stored, and this is a bug in the client.",
				Detail:  map[string]any{"field": "laps", "index": i, "number": lap.Number},
			}
		}
		seen[lap.Number] = struct{}{}
	}
	return nil
}

func validateLap(lap *wire.Lap, i int, limits wire.Limits) *fault {
	at := func(field, message string) *fault {
		return &fault{Message: message, Detail: map[string]any{
			"field": field, "index": i, "number": lap.Number,
		}}
	}
	switch {
	case lap.Number < 0 || lap.Number > MaxLapNumber:
		return at("number", "One of those laps has a lap number no session could reach — nothing was stored.")
	case lap.LapMs <= 0 || lap.LapMs > MaxLapMs:
		return at("lap_ms", "One of those laps has a lap time no lap could take — nothing was stored.")
	case !validKind(lap.Kind):
		return at("kind", "One of those laps is of a kind this server does not know — nothing was stored.")
	case lap.StartedAt.IsZero():
		return at("started_at", "One of those laps does not say when it started — nothing was stored.")
	case len(lap.Trace) > limits.TracePoints:
		return &fault{
			Message: fmt.Sprintf("One of those laps carries more than the %d trace points this server takes — nothing was stored.", limits.TracePoints),
			Detail: map[string]any{
				"field": "trace", "index": i, "number": lap.Number,
				"points": len(lap.Trace), "max": limits.TracePoints,
			},
		}
	case len(lap.Corners) > MaxCornersPerLap:
		return &fault{
			Message: fmt.Sprintf("One of those laps names more than the %d corners this server takes — nothing was stored.", MaxCornersPerLap),
			Detail: map[string]any{
				"field": "corners", "index": i, "number": lap.Number,
				"corners": len(lap.Corners), "max": MaxCornersPerLap,
			},
		}
	}
	for j, c := range lap.Corners {
		if f := validateCorner(c, i, j, lap.Number); f != nil {
			return f
		}
	}
	return nil
}

// validateCorner checks one corner of a lap's analysis.
//
// The bounds are what the wire declares: a turn number from 1, two positions in
// per mille of the lap, whole km/h speeds, a brake percentage, and a deficit
// that is positive because a client sends the corners that cost time and not
// the ones that gained it.
//
// An unknown pattern is refused rather than dropped. Every other named value
// this server stores is checked against the set it knows — a lap kind, a
// session type — and the alternative to refusing is either handing a plugin a
// word its switch has no case for, or quietly editing what a client sent.
// Version skew is what `min_client` is for.
func validateCorner(c wire.Corner, i, j, number int) *fault {
	at := func(message string) *fault {
		return &fault{Message: message, Detail: map[string]any{
			"field": "corners", "index": i, "number": number, "corner": j, "turn": c.Turn,
		}}
	}
	switch {
	case c.Turn < 1 || c.Turn > MaxCornersPerLap:
		return at("One of those laps names a turn number no lap has — nothing was stored.")
	case c.ApexPct < 0 || c.ApexPct > perMille:
		return at("One of those corners is at a position that is not on the lap — nothing was stored.")
	case c.ThrottleLag < 0 || c.ThrottleLag > perMille:
		return at("One of those corners has a throttle lag longer than a lap — nothing was stored.")
	case c.ApexKmh < 0 || c.ApexKmh > MaxSpeedKmh || c.RefApexKmh < 0 || c.RefApexKmh > MaxSpeedKmh:
		return at("One of those corners has an apex speed no car reaches — nothing was stored.")
	case c.DeficitKmh <= 0 || c.DeficitKmh > MaxSpeedKmh:
		return at("One of those corners reports a speed loss no corner produces — nothing was stored.")
	case c.BrakeAtApex < 0 || c.BrakeAtApex > 100:
		return at("One of those corners has a brake reading that is not a percentage — nothing was stored.")
	case !validPattern(c.Pattern):
		return at("One of those corners names a pattern this server does not know — nothing was stored.")
	}
	return nil
}

// perMille is the scale the wire carries a position on the lap in, the same one
// a trace point's distance channel uses.
const perMille = 1000

func validPattern(p wire.CornerPattern) bool {
	switch p {
	case "", wire.PatternEarlyApex, wire.PatternLateBraking, wire.PatternSlowExit:
		return true
	default:
		return false
	}
}

// validateSummary checks the body of PUT /stints/{id}/summary.
func validateSummary(s wire.Summary, limits wire.Limits) *fault {
	switch {
	case s.Laps < 0 || s.Laps > MaxLapNumber:
		return invalidField("laps", "That summary counts more laps than any session holds — it was dropped.")
	case s.Incidents < 0 || s.Incidents > MaxIncidents:
		return invalidField("incidents", "That summary counts more incidents than any session holds — it was dropped.")
	case s.BestLapMs < 0 || s.BestLapMs > MaxLapMs:
		return invalidField("best_lap_ms", "That summary has a best lap no lap could take — it was dropped.")
	case s.AvgLapMs < 0 || s.AvgLapMs > MaxLapMs:
		return invalidField("avg_lap_ms", "That summary has an average lap no lap could take — it was dropped.")
	case s.ConsistencyPct < 0 || s.ConsistencyPct > 100:
		return invalidField("consistency_pct", "Consistency is a percentage and that one is not — the summary was dropped.")
	case s.TopSpeedKmh < 0 || s.TopSpeedKmh > MaxSpeedKmh:
		return invalidField("top_speed_kmh", "That summary has a top speed no car reaches — it was dropped.")
	case len(s.BestTrace) > limits.TracePoints:
		return &fault{
			Message: fmt.Sprintf("That summary's best lap carries more than the %d trace points this server takes — it was dropped.", limits.TracePoints),
			Detail:  map[string]any{"field": "best_trace", "points": len(s.BestTrace), "max": limits.TracePoints},
		}
	case s.FinishedAt != nil && s.FinishedAt.IsZero():
		return invalidField("finished_at", "That summary says it is final but does not say when — it was dropped.")
	}
	return nil
}

func validSessionType(t wire.SessionType) bool {
	switch t {
	case wire.SessionPractice, wire.SessionQualifying, wire.SessionRace, wire.SessionTesting:
		return true
	default:
		return false
	}
}

func validKind(k wire.Kind) bool {
	switch k {
	case wire.KindClean, wire.KindIn, wire.KindOut, wire.KindInvalid:
		return true
	default:
		return false
	}
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
