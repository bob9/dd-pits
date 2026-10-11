package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRenderLocalHomeForCompare renders the dd-pits local dashboard "home" page
// from the same ladder scenario as the cloud compare test
// (internal/platform/handlers/dronedash_compare_test.go), writing self-contained
// HTML to $DD_COMPARE_DIR/local.html so the two can be screenshot side-by-side
// to prove visual parity. Opt-in: skipped unless DD_COMPARE_DIR is set.
func TestRenderLocalHomeForCompare(t *testing.T) {
	outDir := os.Getenv("DD_COMPARE_DIR")
	if outDir == "" {
		t.Skip("set DD_COMPARE_DIR to render the local compare page")
	}

	s := newLiveServer(brand{Name: "FPV Rebels", Accent: "#ff4d4d"})
	mv := compareLocalMeeting()
	fillDispLaps(mv)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(outDir, "local.html"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := map[string]any{"M": mv, "Brand": s.brand, "Accent": s.brand.Accent}
	if err := s.tmpl.ExecuteTemplate(f, "home", data); err != nil {
		t.Fatalf("render local home: %v", err)
	}
	t.Logf("wrote %s/local.html", outDir)
}

func compareLocalMeeting() *meetingView {
	current := &vmRace{
		ID: "101", Round: 6, Number: 1, RoundName: "Round 6", Label: "Round 6 — Race 1",
		IsTT: true, ConsecWindow: 3, State: "running", StartMs: 1700000000000, TargetLaps: 3,
		Pilots: []vmPilot{
			{Name: "Clog wog", ChannelLabel: "R2", ChannelColor: "#1e6fff", Holeshot: 16.117,
				Laps: []float64{9.667, 7.584, 1.933, 2.200, 3.367, 1.050, 1.417}, Total: 43.334, CountedLaps: 8, Position: 1,
				BestLap: 1.050, BestConsec: 3.517, PBConsec: 1.750},
			{Name: "Drop Bear", ChannelLabel: "F2", ChannelColor: "#f5d20a", Holeshot: 22.017,
				Laps: []float64{2.267, 8.067, 3.883, 14.300, 0.983, 1.383}, Total: 52.900, CountedLaps: 7, Position: 2,
				BestLap: 0.983, BestConsec: 3.349, PBConsec: 0.517},
			{Name: "Red2rotor", ChannelLabel: "R1", ChannelColor: "#e8392b", Holeshot: 9.867,
				Laps: []float64{8.950, 8.633, 6.800}, Total: 34.250, CountedLaps: 4, Position: 3,
				BestLap: 6.800, BestConsec: 22.233, PBConsec: 3.450},
			{Name: "Enjenir", ChannelLabel: "F4", ChannelColor: "#e23bd0", Holeshot: 28.767,
				Laps: []float64{1.933, 23.100}, Total: 53.800, CountedLaps: 3, Position: 4,
				BestLap: 1.933, BestConsec: 48.133, PBConsec: 1.300},
		},
	}
	previous := vmRace{
		ID: "100", Round: 5, Number: 2, RoundName: "Round 5", Label: "Round 5 — Race 2",
		IsTT: true, ConsecWindow: 3, State: "ended", TargetLaps: 3,
		Pilots: []vmPilot{
			{Name: "tex", ChannelLabel: "R2", ChannelColor: "#1e6fff", Holeshot: 20.595,
				Laps: []float64{0.400, 5.983, 1.500}, Total: 28.478, CountedLaps: 4, Position: 1, BestLap: 0.400, BestConsec: 7.883},
			{Name: "Enjenir", ChannelLabel: "F2", ChannelColor: "#f5d20a", Holeshot: 9.012,
				Laps: []float64{9.566, 1.633, 6.267}, Total: 26.478, CountedLaps: 4, Position: 2, BestLap: 1.633, BestConsec: 17.466},
			{Name: "Drop Bear", ChannelLabel: "R1", ChannelColor: "#e8392b", Holeshot: 3.662,
				Laps: []float64{3.217, 9.116}, Total: 15.995, CountedLaps: 3, Position: 3, BestLap: 3.217},
			{Name: "Willman", ChannelLabel: "F4", ChannelColor: "#e23bd0", Holeshot: 19.528,
				Laps: []float64{3.983, 2.650}, Total: 26.161, CountedLaps: 3, Position: 4, BestLap: 2.650},
		},
	}
	upcoming := []vmRace{{
		ID: "102", Round: 6, Number: 2, RoundName: "Round 6", Label: "Round 6 — Race 2",
		Pilots: []vmPilot{
			{Name: "Drop Bear", ChannelLabel: "R1", ChannelColor: "#e8392b"},
			{Name: "tex", ChannelLabel: "R2", ChannelColor: "#1e6fff"},
			{Name: "Enjenir", ChannelLabel: "F2", ChannelColor: "#f5d20a"},
			{Name: "Willman", ChannelLabel: "F4", ChannelColor: "#e23bd0"},
		},
	}}
	board := []vmStanding{
		{Pos: 1, Name: "Drop Bear", ChannelLabel: "R1", ChannelColor: "#e8392b", Laps: 55, Holeshot: 1.223, BestLap: 0.167, BestConsec: 0.517, FastestRace: 6.100, NextStatus: "Staging"},
		{Pos: 2, Name: "Red2rotor", ChannelLabel: "R1", ChannelColor: "#e8392b", Laps: 25, Holeshot: 2.842, BestLap: 0.250, BestConsec: 3.450, FastestRace: 7.578, NextStatus: ""},
		{Pos: 3, Name: "Willman", ChannelLabel: "F4", ChannelColor: "#e23bd0", Laps: 17, Holeshot: 2.439, BestLap: 0.300, BestConsec: 1.517, FastestRace: 5.389, NextStatus: "Staging"},
		{Pos: 4, Name: "Clog wog", ChannelLabel: "R2", ChannelColor: "#1e6fff", Laps: 31, Holeshot: 1.222, BestLap: 0.333, BestConsec: 1.750, FastestRace: 9.175, NextStatus: ""},
		{Pos: 5, Name: "Enjenir", ChannelLabel: "F2", ChannelColor: "#f5d20a", Laps: 46, Holeshot: 1.839, BestLap: 0.367, BestConsec: 1.300, FastestRace: 5.622, NextStatus: "Staging"},
		{Pos: 6, Name: "tex", ChannelLabel: "R2", ChannelColor: "#1e6fff", Laps: 16, Holeshot: 1.506, BestLap: 0.400, BestConsec: 3.317, FastestRace: 5.973, NextStatus: "Staging"},
	}
	return &meetingView{
		Name: "Ladder", IsTT: true, Window: 3,
		Current: current, Recent: []vmRace{previous}, Upcoming: upcoming, Board: board,
	}
}
