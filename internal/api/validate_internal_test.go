package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"
)

// testLimits are the published limits the lap validator is written against.
var testLimits = wire.Limits{TracePoints: 300, LapsPerRequest: 50}

// validStint is a body that passes, so that each case below changes one thing.
func validStint() wire.Stint {
	return wire.Stint{
		Sim: "iracing", Track: "Circuit de Barcelona-Catalunya", TrackID: "barcelona gp",
		Car: "Ferrari 296 GT3", CarClass: "gt3", SessionType: wire.SessionPractice,
		StartedAt: time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC),
		Sectors:   []float64{0, 0.31, 0.68},
	}
}

// validLap is the same for a lap.
func validLap() wire.Lap {
	return wire.Lap{
		Number: 7, LapMs: 91234, Kind: wire.KindClean,
		StartedAt: time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC),
	}
}

// TestValidateSetup covers the optional car setup of a stint. The first case is
// the one that matters most: absent has to be free, because a simulator that
// publishes no setup and a series that locks it away are the common cases and
// neither is a failure.
func TestValidateSetup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   *wire.CarSetup
		wantErr string
	}{
		{
			name:  "no setup at all is not an error",
			setup: nil,
		},
		{
			name:  "an empty setup is accepted, because a client may know nothing yet",
			setup: &wire.CarSetup{},
		},
		{
			name: "a whole sheet is accepted",
			setup: &wire.CarSetup{
				UpdateCount: 4,
				Tyres: []wire.SetupTyre{
					{Wheel: wire.WheelLF, ColdKpa: 165, HotKpa: 178.5, TempInnerC: 96.5, TreadInnerPct: 94},
					{Wheel: wire.WheelRF},
					{Wheel: wire.WheelLR},
					{Wheel: wire.WheelRR},
				},
				RearWing: &wire.SetupValue{Name: "RearWingSetting", Text: "7 hole", Number: 7, Unit: "hole"},
				Values:   []wire.SetupValue{{Group: "Chassis/Front", Name: "ArbSize", Text: "Medium"}},
			},
		},
		{
			name:    "a wheel this server does not know",
			setup:   &wire.CarSetup{Tyres: []wire.SetupTyre{{Wheel: "spare"}}},
			wantErr: "names a wheel this server does not know",
		},
		{
			name: "the same wheel twice",
			setup: &wire.CarSetup{Tyres: []wire.SetupTyre{
				{Wheel: wire.WheelLF}, {Wheel: wire.WheelLF},
			}},
			wantErr: "carries the same wheel twice",
		},
		{
			name: "more wheels than a car",
			setup: &wire.CarSetup{Tyres: []wire.SetupTyre{
				{Wheel: wire.WheelLF},
				{Wheel: wire.WheelRF},
				{Wheel: wire.WheelLR},
				{Wheel: wire.WheelRR},
				{Wheel: wire.WheelLF},
			}},
			wantErr: "more wheels than a car",
		},
		{
			name:    "a negative pressure is not a measurement",
			setup:   &wire.CarSetup{Tyres: []wire.SetupTyre{{Wheel: wire.WheelLF, ColdKpa: -1}}},
			wantErr: "tyre reading no tyre could produce",
		},
		{
			name:    "a pressure no tyre holds",
			setup:   &wire.CarSetup{Tyres: []wire.SetupTyre{{Wheel: wire.WheelLF, HotKpa: MaxSetupKpa + 1}}},
			wantErr: "tyre reading no tyre could produce",
		},
		{
			name:    "a tread temperature no tyre reaches",
			setup:   &wire.CarSetup{Tyres: []wire.SetupTyre{{Wheel: wire.WheelLF, TempOuterC: MaxSetupTempC + 1}}},
			wantErr: "tyre reading no tyre could produce",
		},
		{
			name:    "a tread depth that is not a percentage",
			setup:   &wire.CarSetup{Tyres: []wire.SetupTyre{{Wheel: wire.WheelLF, TreadMiddlePct: 101}}},
			wantErr: "tyre reading no tyre could produce",
		},
		{
			name:    "a setting with no name",
			setup:   &wire.CarSetup{Values: []wire.SetupValue{{Text: "Medium"}}},
			wantErr: "a setting with no name",
		},
		{
			name: "a rear wing with no name",
			setup: &wire.CarSetup{
				RearWing: &wire.SetupValue{Text: "7 hole"},
			},
			wantErr: "a setting with no name",
		},
		{
			name: "a setting longer than this server stores",
			setup: &wire.CarSetup{Values: []wire.SetupValue{
				{Name: "Camber", Text: strings.Repeat("x", MaxSetupTextLen+1)},
			}},
			wantErr: "longer than this server stores",
		},
		{
			name:    "a revision number no garage reaches",
			setup:   &wire.CarSetup{UpdateCount: MaxSetupUpdateCount + 1},
			wantErr: "revision number no garage could reach",
		},
		{
			name:    "a negative revision number",
			setup:   &wire.CarSetup{UpdateCount: -1},
			wantErr: "revision number no garage could reach",
		},
		{
			name:    "more settings than this server stores",
			setup:   &wire.CarSetup{Values: make([]wire.SetupValue, MaxSetupValues+1)},
			wantErr: "more settings than this server stores",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			body := validStint()
			body.Setup = tt.setup

			f := validateStint(body)
			if tt.wantErr == "" {
				r.Nil(f)
				return
			}
			r.NotNil(f)
			r.Contains(f.Message, tt.wantErr)
			r.Contains(f.Detail, "field", "a client logs the detail and needs to know which field")
		})
	}
}

// TestValidateCorners covers the corner analysis of a lap, whose every bound is
// one the wire declares.
func TestValidateCorners(t *testing.T) {
	t.Parallel()

	good := wire.Corner{
		Turn: 3, ApexPct: 600, ApexKmh: 60, RefApexKmh: 80, DeficitKmh: 20,
		BrakeAtApex: 45, ThrottleLag: 40, Pattern: wire.PatternEarlyApex,
	}
	with := func(f func(*wire.Corner)) []wire.Corner {
		c := good
		f(&c)
		return []wire.Corner{c}
	}

	tests := []struct {
		name    string
		corners []wire.Corner
		wantErr string
	}{
		{
			name: "a lap with no corners is normal",
		},
		{
			name:    "a whole corner is accepted",
			corners: []wire.Corner{good},
		},
		{
			name:    "a corner with no pattern is accepted",
			corners: with(func(c *wire.Corner) { c.Pattern = "" }),
		},
		{
			name:    "a corner with no reference speed is accepted",
			corners: with(func(c *wire.Corner) { c.RefApexKmh = 0 }),
		},
		{
			name:    "turn zero is not a turn",
			corners: with(func(c *wire.Corner) { c.Turn = 0 }),
			wantErr: "turn number no lap has",
		},
		{
			name:    "a turn number past what a lap holds",
			corners: with(func(c *wire.Corner) { c.Turn = MaxCornersPerLap + 1 }),
			wantErr: "turn number no lap has",
		},
		{
			name:    "an apex that is not on the lap",
			corners: with(func(c *wire.Corner) { c.ApexPct = perMille + 1 }),
			wantErr: "not on the lap",
		},
		{
			name:    "a throttle lag longer than a lap",
			corners: with(func(c *wire.Corner) { c.ThrottleLag = perMille + 1 }),
			wantErr: "throttle lag longer than a lap",
		},
		{
			name:    "an apex speed no car reaches",
			corners: with(func(c *wire.Corner) { c.ApexKmh = MaxSpeedKmh + 1 }),
			wantErr: "apex speed no car reaches",
		},
		{
			name:    "a deficit of zero is not a corner that cost anything",
			corners: with(func(c *wire.Corner) { c.DeficitKmh = 0 }),
			wantErr: "speed loss no corner produces",
		},
		{
			name:    "a negative deficit is a corner that gained, which is not sent",
			corners: with(func(c *wire.Corner) { c.DeficitKmh = -4 }),
			wantErr: "speed loss no corner produces",
		},
		{
			name:    "a brake reading that is not a percentage",
			corners: with(func(c *wire.Corner) { c.BrakeAtApex = 101 }),
			wantErr: "not a percentage",
		},
		{
			name:    "a pattern this server does not know",
			corners: with(func(c *wire.Corner) { c.Pattern = "understeer" }),
			wantErr: "names a pattern this server does not know",
		},
		{
			name:    "more corners than a lap has",
			corners: make([]wire.Corner, MaxCornersPerLap+1),
			wantErr: "more than the 64 corners this server takes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			lap := validLap()
			lap.Corners = tt.corners

			f := validateLapBatch(wire.LapBatch{Laps: []wire.Lap{lap}}, testLimits)
			if tt.wantErr == "" {
				r.Nil(f)
				return
			}
			r.NotNil(f)
			r.Contains(f.Message, tt.wantErr)
			r.Equal("corners", f.Detail["field"])
			r.Equal(lap.Number, f.Detail["number"], "a client logs which lap it was")
		})
	}
}

// TestNewMessagesFollowTheHouseStyle holds the sentences the two new validators
// show a driver to the same rules as every other one. They are built inline
// rather than declared as constants, which is why they are not in
// TestMessagesFollowTheHouseStyle and are here instead.
func TestNewMessagesFollowTheHouseStyle(t *testing.T) {
	t.Parallel()

	setups := []*wire.CarSetup{
		{UpdateCount: -1},
		{Tyres: []wire.SetupTyre{{Wheel: "spare"}}},
		{Tyres: []wire.SetupTyre{{Wheel: wire.WheelLF}, {Wheel: wire.WheelLF}}},
		{Tyres: []wire.SetupTyre{{Wheel: wire.WheelLF, ColdKpa: -1}}},
		{Values: []wire.SetupValue{{Text: "x"}}},
		{Values: []wire.SetupValue{{Name: strings.Repeat("x", MaxSetupTextLen+1)}}},
		{Values: make([]wire.SetupValue, MaxSetupValues+1)},
	}
	corners := [][]wire.Corner{
		{{Turn: 0}},
		{{Turn: 1, ApexPct: -1}},
		{{Turn: 1, DeficitKmh: 0}},
		{{Turn: 1, DeficitKmh: 4, BrakeAtApex: 200}},
		{{Turn: 1, DeficitKmh: 4, Pattern: "understeer"}},
		make([]wire.Corner, MaxCornersPerLap+1),
	}

	messages := make([]string, 0, len(setups)+len(corners))
	for _, s := range setups {
		body := validStint()
		body.Setup = s
		f := validateStint(body)
		require.NotNil(t, f)
		messages = append(messages, f.Message)
	}
	for _, c := range corners {
		lap := validLap()
		lap.Corners = c
		f := validateLapBatch(wire.LapBatch{Laps: []wire.Lap{lap}}, testLimits)
		require.NotNil(t, f)
		messages = append(messages, f.Message)
	}

	for _, message := range messages {
		t.Run(message, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			r.True(strings.HasSuffix(message, "."), "a complete sentence ends in a full stop")
			r.NotContains(message, "!", "nothing this server says to a driver is exclaimed")
			r.NotContains(message, "  ", "one space between words")
			r.Equal(strings.ToUpper(message[:1]), message[:1], "sentence case")
			for _, jargon := range []string{"HTTP", "SQL", "nil", "JSON", "jsonb", "goroutine", "422", "500"} {
				r.NotContains(message, jargon, "a driver is not shown %q", jargon)
			}
		})
	}
}
