package dronedash

import "testing"

// TestCurrentRacePartial verifies the partial snapshot carries only the running
// heat(s) and exactly the rounds / pilots / results they reference — and
// nothing from finished heats. This is the safety-critical bit: a partial must
// never imply the finished heats are gone (the server leaves absent heats
// untouched, but the partial should also be minimal and self-contained).
func TestCurrentRacePartial(t *testing.T) {
	const (
		running  = "2026-06-27T07:00:00Z"
		finStart = "2026-06-27T06:00:00Z"
		finEnd   = "2026-06-27T06:05:00Z"
	)
	snap := Snapshot{
		Event:    ftEvent{ID: "ev1", Name: "Meeting", Channels: []string{"c1"}, ChannelColors: []string{"#fff"}},
		Channels: []ftChannel{{ID: "c1"}},
		Pilots: []ftPilot{
			{ID: "p1", Name: "Running Only"},
			{ID: "p2", Name: "Finished Only"},
			{ID: "p3", Name: "Both"},
		},
		Rounds: []ftRound{{ID: "rd-run"}, {ID: "rd-fin"}},
		Races: []ftRace{
			{ID: "race-run", Round: "rd-run", Start: running, End: "",
				PilotChannels: []ftPilotChannel{{Pilot: "p1", Channel: "c1"}, {Pilot: "p3", Channel: "c1"}},
				Laps:          []ftLap{{ID: "l1", Detection: "d1", LapNumber: 1}},
				Detections:    []ftDetection{{ID: "d1", Pilot: "p1", Valid: true}}},
			{ID: "race-fin", Round: "rd-fin", Start: finStart, End: finEnd,
				PilotChannels: []ftPilotChannel{{Pilot: "p2", Channel: "c1"}, {Pilot: "p3", Channel: "c1"}},
				Laps:          []ftLap{{ID: "l9", Detection: "d9", LapNumber: 1}},
				Detections:    []ftDetection{{ID: "d9", Pilot: "p2", Valid: true}}},
		},
		Results: []ftResult{
			{ID: "res1", Race: "race-fin", Pilot: "p2", Position: 1},
		},
	}

	part, ok := snap.CurrentRacePartial()
	if !ok {
		t.Fatal("expected a running race, got ok=false")
	}
	if !part.Partial {
		t.Fatal("partial flag not set")
	}
	if len(part.Races) != 1 || part.Races[0].ID != "race-run" {
		t.Fatalf("expected only the running race, got %d races: %+v", len(part.Races), raceIDs(part.Races))
	}
	// Running race must keep its full lap+detection set ("all times").
	if len(part.Races[0].Laps) != 1 || len(part.Races[0].Detections) != 1 {
		t.Fatalf("running race lost its laps/detections: %+v", part.Races[0])
	}
	if got := pilotIDset(part.Pilots); !got["p1"] || !got["p3"] || got["p2"] {
		t.Fatalf("pilots should be exactly the running heat's (p1,p3), got %v", got)
	}
	if len(part.Rounds) != 1 || part.Rounds[0].ID != "rd-run" {
		t.Fatalf("rounds should be only the running heat's, got %+v", part.Rounds)
	}
	if len(part.Results) != 0 {
		t.Fatalf("finished-heat results must not ride in a partial, got %d", len(part.Results))
	}
	// Event header + channels ride along (small), so colours stay fresh.
	if part.Event.ID != "ev1" || len(part.Channels) != 1 {
		t.Fatalf("event header/channels should be carried: %+v", part.Event)
	}
}

func TestCurrentRacePartialNoRunningRace(t *testing.T) {
	snap := Snapshot{
		Races: []ftRace{
			{ID: "race-fin", Start: "2026-06-27T06:00:00Z", End: "2026-06-27T06:05:00Z"},
		},
	}
	if _, ok := snap.CurrentRacePartial(); ok {
		t.Fatal("no race is running — expected ok=false so the caller sends a full")
	}
}

// TestCurrentRacePartialCuedRace verifies that with nothing running but a race
// cued (SelectedRaceID), the partial carries that un-run heat (so a selection /
// re-run pushes a small current-race partial instead of the whole meeting).
func TestCurrentRacePartialCuedRace(t *testing.T) {
	snap := Snapshot{
		Pilots: []ftPilot{{ID: "p1", Name: "Al"}, {ID: "p2", Name: "Bo"}},
		Rounds: []ftRound{{ID: "rd1", RoundNumber: 1, Valid: true}},
		Races: []ftRace{
			{ID: "race-fin", Round: "rd1", Start: "2026-06-27T06:00:00Z", End: "2026-06-27T06:05:00Z"},
			{ID: "race-cued", Round: "rd1", PilotChannels: []ftPilotChannel{{Pilot: "p1"}, {Pilot: "p2"}}},
		},
		SelectedRaceID: "race-cued",
	}
	part, ok := snap.CurrentRacePartial()
	if !ok {
		t.Fatal("a cued race should produce a partial")
	}
	if len(part.Races) != 1 || part.Races[0].ID != "race-cued" {
		t.Fatalf("partial should carry only the cued race, got %+v", part.Races)
	}
	if part.SelectedRaceID != "race-cued" {
		t.Fatalf("partial must carry SelectedRaceID, got %q", part.SelectedRaceID)
	}
	if !part.Partial {
		t.Fatal("partial flag must be set")
	}
	if len(part.Pilots) != 2 {
		t.Fatalf("partial should carry the cued race's pilots, got %d", len(part.Pilots))
	}
}

func TestRunningSignature(t *testing.T) {
	snap := Snapshot{Races: []ftRace{
		{ID: "b", Start: "2026-06-27T07:00:00Z"},
		{ID: "a", Start: "2026-06-27T07:00:00Z"},
		{ID: "done", Start: "2026-06-27T06:00:00Z", End: "2026-06-27T06:05:00Z"},
	}}
	if got := snap.RunningSignature(); got != "a,b" {
		t.Fatalf("RunningSignature = %q, want sorted running ids \"a,b\"", got)
	}
	if got := (Snapshot{}).RunningSignature(); got != "" {
		t.Fatalf("empty snapshot signature = %q, want \"\"", got)
	}
}

func raceIDs(rs []ftRace) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func pilotIDset(ps []ftPilot) map[string]bool {
	m := map[string]bool{}
	for _, p := range ps {
		m[p.ID] = true
	}
	return m
}
