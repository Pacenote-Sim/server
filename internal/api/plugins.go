package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
)

// EventSink is where the API publishes the things plugins react to. It is an
// interface rather than the host itself because this package must not depend on
// how plugins are run, and because a server built without plugins passes nil
// and nothing else changes.
type EventSink interface {
	// Notify dispatches an event. It must not block: an upload finishing is
	// not allowed to wait on an integration.
	Notify(ctx context.Context, e plugin.Event)
}

// Asking is a host that can be asked for something with the caller waiting. It
// is separate from [EventSink] because the two are used at different moments —
// an event is fire and forget after a write, a request is a driver holding on —
// and because a server built without plugins satisfies neither.
type Asking interface {
	// Ask puts a request to whichever plugin answers that kind and returns the
	// first usable answer.
	Ask(ctx context.Context, r plugin.Request) (plugin.Response, error)
}

// publish sends the events a request produced, after the write that produced
// them has committed. Nothing is published for a write that failed or for one
// that was replayed from an idempotency key, because neither of those is a lap
// that has just happened.
func (a *API) publish(ctx context.Context, events []plugin.Event) {
	if a.deps.Plugins == nil {
		return
	}
	for i := range events {
		a.deps.Plugins.Notify(ctx, events[i])
	}
}

// eventID is the key a plugin deduplicates on. It is random rather than derived
// from the lap, because two deliveries of the same lap are exactly what it has
// to be able to tell apart, and a derived id would make them identical.
func eventID(log *slog.Logger) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// There is no useful fallback and no reason to fail an upload over it.
		// A plugin loses its ability to deduplicate this one delivery.
		log.LogAttrs(context.Background(), slog.LevelWarn, "an event was dispatched without an id")
		return ""
	}
	return hex.EncodeToString(b)
}

// pluginDriver is the driver as a plugin sees them.
func pluginDriver(d db.Driver) plugin.Driver {
	return plugin.Driver{ID: d.ID, Slug: d.Slug, Name: d.Name}
}

// pluginSession is the stint as a plugin sees it. The display name of the
// circuit and the session type are not on the row the write path holds, so they
// are left empty rather than guessed at: a plugin that says "at Barcelona" when
// the server does not know the name would be inventing one.
func pluginSession(stint db.Stint) plugin.Session {
	return plugin.Session{
		StintID:   stint.ID.String(),
		Sim:       stint.Sim,
		TrackID:   stint.TrackID,
		Car:       stint.Car,
		CarClass:  stint.CarClass,
		StartedAt: stint.StartedAt,
	}
}

// lapEvents is one event per lap this batch actually stored. A lap the stint
// already held produces nothing: it happened once, so it is announced once.
//
// It reads the laps as the client sent them rather than as they were encoded
// for the database. The facts are the client's own numbers, and taking them off
// the encoded row would mean decoding a trace blob and a JSON document to
// recover what is still in hand.
func lapEvents(log *slog.Logger, s session, stint db.Stint, laps []wire.Lap, res db.LapResult) []plugin.Event {
	if len(res.Stored) == 0 {
		return nil
	}
	stored := make(map[int]bool, len(res.Stored))
	for _, n := range res.Stored {
		stored[n] = true
	}
	events := make([]plugin.Event, 0, len(res.Stored))
	for i := range laps {
		if stored[laps[i].Number] {
			events = append(events, lapEvent(log, s, stint, laps[i], res.BestLapMs))
		}
	}
	return events
}

// lapEvent turns one stored lap into the facts a plugin is given.
//
// The delta is against the best lap of this stint, which is the reference the
// write path already holds. It is not the driver's best at this circuit and not
// the class best — those are a query each, on the path a driver waits on, and
// they belong to the step that gives the coaching plugin a reference lap rather
// than to this one. [plugin.LapFacts.Reference] says which it is in words, so a
// plugin is never left guessing what the number means.
//
// Corners are the client's own corner analysis, carried across unchanged. The
// detector is in the client — it is the only thing that holds a lap's trace at
// full resolution while the lap is being driven — and what it measured is
// copied here rather than recomputed, because it is measured against a
// reference lap this server did not choose and could not reproduce.
//
// A lap with no corners is still normal and still common: a lap with no
// reference to compare against has no deficits to report, and a plugin must
// cope with that rather than invent a turn number.
func lapEvent(log *slog.Logger, s session, stint db.Stint, lap wire.Lap, bestLapMs int) plugin.Event {
	facts := plugin.LapFacts{
		Number:    lap.Number,
		LapMs:     lap.LapMs,
		Kind:      plugin.LapKind(lap.Kind),
		StartedAt: lap.StartedAt,
		Corners:   pluginCorners(lap.Corners),
	}
	if bestLapMs > 0 {
		facts.Reference = "your best lap of this stint"
		facts.DeltaMs = lap.LapMs - bestLapMs
		facts.PersonalBest = lap.LapMs <= bestLapMs && lap.Kind == wire.KindClean
	}
	return plugin.Event{
		ID:      eventID(log),
		Kind:    plugin.EventLapCompleted,
		At:      lap.StartedAt,
		Driver:  pluginDriver(s.driver),
		Session: pluginSession(stint),
		Lap:     &facts,
	}
}

// stintEvent turns a final summary into the facts a debrief is written from,
// and the car it was driven on into the facts setup advice is written from.
func stintEvent(log *slog.Logger, s session, stint db.Stint, body wire.Summary) plugin.Event {
	car := body.CarState
	facts := plugin.StintFacts{
		Laps:           body.Laps,
		Incidents:      body.Incidents,
		BestLapMs:      body.BestLapMs,
		AvgLapMs:       body.AvgLapMs,
		ConsistencyPct: body.ConsistencyPct,
		TopSpeedKmh:    body.TopSpeedKmh,
		Fuel: plugin.FuelSummary{
			UsedL:      car.FuelUsedL,
			RemainingL: car.FuelLevelL,
			PerLapL:    perLap(car.FuelUsedL, body.Laps),
		},
		Tyres: plugin.TyreSummary{
			LF:      car.TyreTempC.LF,
			RF:      car.TyreTempC.RF,
			LR:      car.TyreTempC.LR,
			RR:      car.TyreTempC.RR,
			SpreadC: spread(car.TyreTempC),
		},
		Setup: pluginSetup(log, stint),
		Conditions: plugin.Conditions{
			Skies:      body.Conditions.Skies,
			Wetness:    body.Conditions.Wetness,
			WindKmh:    body.Conditions.WindKmh,
			Humidity:   body.Conditions.Humidity,
			TrackTempC: body.Conditions.TrackTempC,
			AirTempC:   body.Conditions.AirTempC,
		},
	}
	// CleanLaps is not in the summary document, so it is left at zero rather
	// than set to Laps. A plugin reading zero knows it was not told; a plugin
	// reading the total would be told something false.
	if body.FinishedAt != nil {
		facts.FinishedAt = *body.FinishedAt
	}
	return plugin.Event{
		ID:      eventID(log),
		Kind:    plugin.EventStintFinished,
		At:      facts.FinishedAt,
		Driver:  pluginDriver(s.driver),
		Session: pluginSession(stint),
		Stint:   &facts,
	}
}

// pluginCorners maps a lap's corner analysis onto the facts. It is a copy field
// for field: the two types carry the same measurements in the same units, and
// the wire is where the units are defined.
//
// A lap with no corners yields nil rather than an empty slice, so that the
// field is omitted from the JSON a plugin decodes and "nothing to say about the
// corners" has one spelling.
func pluginCorners(corners []wire.Corner) []plugin.Corner {
	if len(corners) == 0 {
		return nil
	}
	out := make([]plugin.Corner, 0, len(corners))
	for _, c := range corners {
		out = append(out, plugin.Corner{
			Turn:             c.Turn,
			ApexPct:          c.ApexPct,
			ApexKmh:          c.ApexKmh,
			ReferenceApexKmh: c.RefApexKmh,
			DeficitKmh:       c.DeficitKmh,
			BrakeAtApex:      c.BrakeAtApex,
			ThrottleLag:      c.ThrottleLag,
			Pattern:          plugin.CornerPattern(c.Pattern),
		})
	}
	return out
}

// pluginSetup decodes the setup the stint was stored with and maps it onto the
// facts, or returns nil.
//
// Nil is the answer to every kind of absence there is: a client that sent no
// setup, a simulator that publishes none, a series that locks it away, and a
// stored document this build cannot read. The last of those is the reason this
// swallows the decode error rather than failing the event: a stint whose setup
// column holds something unexpected still finished, and a debrief about it is
// worth more than no debrief at all. What a plugin must never be handed is half
// a setup presented as a whole one.
func pluginSetup(log *slog.Logger, stint db.Stint) *plugin.CarSetup {
	if len(stint.Setup) == 0 {
		return nil
	}
	var stored wire.CarSetup
	if err := json.Unmarshal(stint.Setup, &stored); err != nil {
		log.LogAttrs(context.Background(), slog.LevelWarn, "a stored car setup could not be read",
			slog.String("stint_id", stint.ID.String()), slog.String("error", err.Error()))
		return nil
	}
	out := plugin.CarSetup{
		UpdateCount: stored.UpdateCount,
		RearWing:    pluginSetupValue(stored.RearWing),
	}
	for _, t := range stored.Tyres {
		out.Tyres = append(out.Tyres, plugin.SetupTyre{
			Wheel:          plugin.Wheel(t.Wheel),
			ColdKpa:        t.ColdKpa,
			HotKpa:         t.HotKpa,
			TempInnerC:     t.TempInnerC,
			TempMiddleC:    t.TempMiddleC,
			TempOuterC:     t.TempOuterC,
			TreadInnerPct:  t.TreadInnerPct,
			TreadMiddlePct: t.TreadMiddlePct,
			TreadOuterPct:  t.TreadOuterPct,
		})
	}
	for _, v := range stored.Values {
		out.Values = append(out.Values, *pluginSetupValue(&v))
	}
	if out.UpdateCount == 0 && len(out.Tyres) == 0 && out.RearWing == nil && len(out.Values) == 0 {
		// A document that decoded to nothing is not a setup, whatever the
		// column held.
		return nil
	}
	return &out
}

// pluginSetupValue maps one named setting, passing nil through: the rear wing
// is a setting a car may not have.
func pluginSetupValue(v *wire.SetupValue) *plugin.SetupValue {
	if v == nil {
		return nil
	}
	return &plugin.SetupValue{
		Group:  v.Group,
		Name:   v.Name,
		Text:   v.Text,
		Number: v.Number,
		Unit:   v.Unit,
	}
}

// perLap is the fuel figure a strategy is built on. Zero laps is zero rather
// than a division by zero.
func perLap(used float64, laps int) float64 {
	if laps <= 0 {
		return 0
	}
	return used / float64(laps)
}

// spread is the hottest tyre minus the coldest, which is the number that says
// whether one corner of the car is working alone.
func spread(t wire.TyreTemps) float64 {
	temps := [4]float64{t.LF, t.RF, t.LR, t.RR}
	lo, hi := temps[0], temps[0]
	for _, v := range temps[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return hi - lo
}
