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
// It reads the rows as they were written, so that what a plugin is handed is
// what the database holds and not a second rendering of the same upload.
func lapEvents(log *slog.Logger, s session, stint db.Stint, rows []db.LapRow, res db.LapResult) []plugin.Event {
	if len(res.Stored) == 0 {
		return nil
	}
	stored := make(map[int]bool, len(res.Stored))
	for _, n := range res.Stored {
		stored[n] = true
	}
	events := make([]plugin.Event, 0, len(res.Stored))
	for i := range rows {
		if stored[rows[i].Number] {
			events = append(events, lapEvent(log, s, stint, rows[i], res.BestLapMs))
		}
	}
	return events
}

// lapEvent turns one stored lap into the facts a plugin is given.
//
// The delta is against the best lap of this stint, which is the reference the
// write path already holds. It is not the driver's best at this circuit and not
// the class best — those are a query each, on the path a driver waits on, and
// they belong to the step that gives a plugin a reference lap rather than to
// this one. [plugin.LapFacts.Reference] says which it is in words, so a plugin
// is never left guessing what the number means.
//
// The corner analysis is the client's own document, handed over as it was
// stored. This server validated its bounds on the way in and has no other
// opinion about it: what is in a corner is between the client that measured it
// and the plugin that reads it, and a client that starts measuring something
// new reaches every plugin without a change here.
func lapEvent(log *slog.Logger, s session, stint db.Stint, row db.LapRow, bestLapMs int) plugin.Event {
	facts := plugin.LapFacts{
		Number:    row.Number,
		LapMs:     row.LapMs,
		Kind:      plugin.LapKind(row.Kind),
		StartedAt: row.StartedAt,
		Corners:   cornersDocument(row.Corners),
	}
	if bestLapMs > 0 {
		facts.Reference = "your best lap of this stint"
		facts.DeltaMs = row.LapMs - bestLapMs
		facts.PersonalBest = row.LapMs <= bestLapMs && row.Kind == string(wire.KindClean)
	}
	return plugin.Event{
		ID:      eventID(log),
		Kind:    plugin.EventLapCompleted,
		At:      row.StartedAt,
		Driver:  pluginDriver(s.driver),
		Session: pluginSession(stint),
		Lap:     &facts,
	}
}

// stintEvent turns a final summary into the facts a plugin is given about the
// whole stint.
//
// Every number here is one the client reported. Nothing is worked out on its
// behalf — not the fuel per lap, not the spread across the tyres — because a
// figure this server computes for one plugin is a figure it has to keep
// computing for every plugin, and the plugin that wants it has the numbers.
//
// The setup is the client's own document, handed over as it was stored, on the
// same terms as the corner analysis.
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
		},
		Tyres: plugin.TyreSummary{
			LF: car.TyreTempC.LF,
			RF: car.TyreTempC.RF,
			LR: car.TyreTempC.LR,
			RR: car.TyreTempC.RR,
		},
		Setup: setupDocument(stint.Setup),
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

// cornersDocument is the corner analysis as a plugin is handed it: the stored
// document, or nothing.
//
// The column holds an empty array for a lap that arrived with no corners, so
// that every row reads the same way. A plugin is told nothing instead — the key
// is absent — because "no corners" is the normal case and an empty list where
// a plugin expected an absent key is a thing it has to have an opinion about.
// That is the one reading of the document this server does, and it is reading
// its own storage convention, not the client's schema.
func cornersDocument(stored []byte) json.RawMessage {
	if isEmptyDocument(stored) || string(stored) == "[]" {
		return nil
	}
	return json.RawMessage(stored)
}

// setupDocument is the car setup as a plugin is handed it: the stored document,
// or nothing when the client sent none.
//
// It is not decoded here. A document this server cannot read is still the
// document the client sent, and whether it is a setup is the plugin's to
// decide; a server that dropped it because it did not understand it would be
// deciding that for every plugin at once.
func setupDocument(stored []byte) json.RawMessage {
	if isEmptyDocument(stored) {
		return nil
	}
	return json.RawMessage(stored)
}

// isEmptyDocument is a column holding nothing: no bytes, or a JSON null.
func isEmptyDocument(stored []byte) bool {
	return len(stored) == 0 || string(stored) == "null"
}
