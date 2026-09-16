package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/trace"
	"github.com/pacenote-sim/protocol/wire"
)

// The write side of the two documents. A plugin is handed the corner analysis
// and the setup exactly as they were stored, so what gets stored is the other
// half of that promise: the client's document, encoded once, fingerprinted with
// the rest of the lap so that a repeat that disagrees about its corners is a
// conflict and not a silent overwrite.

// aTrace is a short, valid trace: offsets that increase, channels in range.
func aTrace() []wire.TracePoint {
	return []wire.TracePoint{
		{OffsetMs: 0, SpeedKmh: 180, Throttle: 100, Gear: 5, RPM: 7000, DistPct: 0},
		{OffsetMs: 250, SpeedKmh: 120, Brake: 80, Gear: 3, RPM: 6000, DistPct: 5},
		{OffsetMs: 500, SpeedKmh: 96, Throttle: 40, Gear: 3, RPM: 5500, DistPct: 10},
	}
}

func TestEncodeLapsStoresTheCornerDocumentAsSent(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 13, 18, 10, 0, 0, time.UTC)
	laps := []wire.Lap{
		{
			Number: 7, LapMs: 91234, Kind: wire.KindClean, StartedAt: started, Trace: aTrace(),
			Corners: []wire.Corner{worstCorner()},
		},
		{Number: 8, LapMs: 92000, Kind: wire.KindIn, StartedAt: started.Add(92 * time.Second), Trace: aTrace()},
	}

	r := require.New(t)
	rows, fault := encodeLaps(laps)
	r.Nil(fault)
	r.Len(rows, 2)

	t.Run("the document is the client's, encoded once", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		want, err := json.Marshal(laps[0].Corners)
		r.NoError(err)
		r.Equal(string(want), string(rows[0].Corners))
		r.Equal(7, rows[0].Number)
		r.Equal(string(wire.KindClean), rows[0].Kind)
		r.Equal(trace.CodecVersion, rows[0].TraceCodec)
		r.NotEmpty(rows[0].Trace)
	})

	t.Run("a lap with no corners stores an empty list, never a null", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		r.Equal("[]", string(rows[1].Corners),
			"every row reads the same way, and a plugin is handed nothing rather than the list")
		r.Nil(cornersDocument(rows[1].Corners))
	})

	t.Run("the fingerprint covers the corners", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		r.Len(rows[0].ContentSum, 32)
		r.NotEqual(rows[0].ContentSum, rows[1].ContentSum, "two laps, two fingerprints")

		// The same lap, sent again, is the same lap.
		again, fault := encodeLaps(laps[:1])
		r.Nil(fault)
		r.Equal(rows[0].ContentSum, again[0].ContentSum, "encoding is deterministic, or every repeat would be a conflict")

		// The same lap with a different corner analysis is a different lap:
		// it is measured against a reference this server did not choose, so
		// it cannot be recomputed and the second upload is not the first.
		changed := laps[0]
		changed.Corners = []wire.Corner{{Turn: 3, ApexPct: 600, ApexKmh: 61, RefApexKmh: 80, DeficitKmh: 19}}
		other, fault := encodeLaps([]wire.Lap{changed})
		r.Nil(fault)
		r.NotEqual(rows[0].ContentSum, other[0].ContentSum)
	})
}

func TestEncodeLapsRefusesATraceTheCodecWouldNot(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// An offset that goes backwards would not survive the round trip, and the
	// codec says so. The driver is told which lap, and the field.
	bad := aTrace()
	bad[2].OffsetMs = 100
	_, fault := encodeLaps([]wire.Lap{
		{Number: 1, LapMs: 90000, Kind: wire.KindClean, Trace: aTrace()},
		{Number: 2, LapMs: 90000, Kind: wire.KindClean, Trace: bad},
	})
	r.NotNil(fault)
	r.Contains(fault.Message, "trace")
	r.Equal("trace", fault.Detail["field"])
	r.Equal(1, fault.Detail["index"])
	r.Equal(2, fault.Detail["number"])
}

func TestEncodeSetupIsNilForNone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Nil and not an empty document: the column is nullable so that "the
	// simulator published no setup" and "the setup is empty" stay different
	// answers, and setupDocument reads the difference back.
	none, err := encodeSetup(nil)
	r.NoError(err)
	r.Nil(none)
	r.Nil(setupDocument(none))

	some, err := encodeSetup(&wire.CarSetup{UpdateCount: 2, Values: []wire.SetupValue{{Name: "Camber", Text: "-2.5 deg"}}})
	r.NoError(err)
	r.JSONEq(`{"update_count":2,"values":[{"name":"Camber","text":"-2.5 deg"}]}`, string(some))
	r.Equal(string(some), string(setupDocument(some)), "stored and handed over as the same bytes")
}

func TestSummaryDocumentsAreStoredWhole(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	conditions, carState, err := summaryDocuments(wire.Summary{
		Conditions: wire.Conditions{Skies: 1, TrackTempC: 38.5},
		CarState:   wire.CarState{FuelUsedL: 36, TyreTempC: wire.TyreTemps{LF: 92}},
	})
	r.NoError(err)
	r.Contains(string(conditions), `"track_temp_c":38.5`)
	r.Contains(string(carState), `"fuel_used_l":36`)
}

// recordingSink is a plugin host that remembers what it was told.
type recordingSink struct{ events []plugin.Event }

func (s *recordingSink) Notify(_ context.Context, e plugin.Event) { s.events = append(s.events, e) }

func TestPublishHandsEveryEventToTheHost(t *testing.T) {
	t.Parallel()

	t.Run("one call per event, in order", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		sink := &recordingSink{}
		a := &API{deps: Deps{Plugins: sink}}
		a.publish(t.Context(), []plugin.Event{{ID: "a"}, {ID: "b"}, {ID: "c"}})
		r.Equal([]string{"a", "b", "c"}, []string{sink.events[0].ID, sink.events[1].ID, sink.events[2].ID})
	})

	t.Run("a server built without plugins publishes to nobody and does not mind", func(t *testing.T) {
		t.Parallel()
		a := &API{}
		a.publish(t.Context(), []plugin.Event{{ID: "a"}})
	})
}
