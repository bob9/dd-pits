package dronedash

import (
	"reflect"
	"strconv"
	"testing"
)

// TestBracketResultsFallsBackToLapsOnDegeneratePositions guards the wrong-CTA-
// winner bug: an unfinalised/mis-reported heat can carry degenerate positions
// (e.g. every pilot "4th"). Trusting those verbatim handed the final to whoever
// was first in snapshot order. BracketResults must instead rank by laps so the
// pilot who actually flew the most/fastest laps is 1st.
func TestBracketResultsFallsBackToLapsOnDegeneratePositions(t *testing.T) {
	snap := Snapshot{
		Pilots: []ftPilot{
			{ID: "p1", Name: "MirellaFPV", ExternalID: 11}, // first in snapshot order, fewest laps
			{ID: "p2", Name: "ActualWinner", ExternalID: 22},
		},
		Races: []ftRace{{
			ID:         "r1",
			ExternalID: 99,
			End:        "2026-06-04T11:04:46",
			Detections: []ftDetection{
				{ID: "d1", Pilot: "p1", Valid: true},
				{ID: "d2", Pilot: "p2", Valid: true},
				{ID: "d3", Pilot: "p2", Valid: true},
			},
			Laps: []ftLap{
				{ID: "l1", Detection: "d1", LapNumber: 1, LengthSeconds: 20},
				{ID: "l2", Detection: "d2", LapNumber: 1, LengthSeconds: 18},
				{ID: "l3", Detection: "d3", LapNumber: 2, LengthSeconds: 19}, // p2 has 2 laps → winner
			},
		}},
		Results: []ftResult{
			// Degenerate: both reported "4th" — must NOT decide the order.
			{ID: "res1", Race: "r1", Pilot: "p1", Position: 4},
			{ID: "res2", Race: "r1", Pilot: "p2", Position: 4},
		},
	}

	out := snap.BracketResults()
	if len(out) != 1 {
		t.Fatalf("expected 1 bracket result, got %d", len(out))
	}
	fin := out[0].Finishers
	if len(fin) != 2 {
		t.Fatalf("expected 2 finishers, got %d", len(fin))
	}
	if fin[0].PilotName != "ActualWinner" {
		t.Errorf("1st place: got %q want ActualWinner (most laps), not the first-in-snapshot pilot", fin[0].PilotName)
	}
	if fin[0].Position != 1 || fin[1].Position != 2 {
		t.Errorf("positions not reassigned 1,2: got %d,%d", fin[0].Position, fin[1].Position)
	}
}

// TestBracketResultsKeepsCleanReportedPositions confirms a clean distinct
// reported ranking is still honoured verbatim (the common, correct case).
func TestBracketResultsKeepsCleanReportedPositions(t *testing.T) {
	snap := Snapshot{
		Pilots: []ftPilot{
			{ID: "p1", Name: "Winner", ExternalID: 1},
			{ID: "p2", Name: "Runner", ExternalID: 2},
		},
		Races: []ftRace{{ID: "r1", ExternalID: 5, End: "2026-06-04T11:04:46"}},
		Results: []ftResult{
			{ID: "a", Race: "r1", Pilot: "p2", Position: 2},
			{ID: "b", Race: "r1", Pilot: "p1", Position: 1},
		},
	}
	out := snap.BracketResults()
	if len(out) != 1 || len(out[0].Finishers) != 2 {
		t.Fatalf("unexpected results: %+v", out)
	}
	if out[0].Finishers[0].PilotName != "Winner" || out[0].Finishers[1].PilotName != "Runner" {
		t.Errorf("clean reported order not honoured: %+v", out[0].Finishers)
	}
}

// TestBracketResultsUsesFinishLineNotTimeTrialPositions guards the chase-the-ace
// final bug: FPVTrackside reported a clean, distinct — but TIME-TRIAL-ranked —
// Position (ordered by fastest laps), which put the pilot with the fastest laps
// but a slow holeshot 1st, even though she crossed the line LAST. The bracket
// result must use the real finish-line order: racing laps capped at the target
// (an extra lap past the finish doesn't promote a pilot) and total time that
// INCLUDES the holeshot (a slow start costs you the race).
func TestBracketResultsUsesFinishLineNotTimeTrialPositions(t *testing.T) {
	mkLaps := func(det string, lens ...float64) []ftLap {
		laps := make([]ftLap, len(lens))
		for i, s := range lens {
			laps[i] = ftLap{ID: det + "-l" + strconv.Itoa(i), Detection: det, LapNumber: i, LengthSeconds: s}
		}
		return laps
	}
	var laps []ftLap
	laps = append(laps, mkLaps("dW", 2, 12, 12, 12)...)    // Willman: holeshot+3 laps → 38s
	laps = append(laps, mkLaps("dL", 2, 13, 13, 13, 5)...) // Lucid: +extra lap 4 (past target, ignored) → 41s
	laps = append(laps, mkLaps("dM", 14, 10, 13, 10)...)   // Mirella: slow holeshot → 47s, but fastest racing laps

	snap := Snapshot{
		Pilots: []ftPilot{
			{ID: "pW", Name: "Willman", ExternalID: 1},
			{ID: "pL", Name: "Lucid", ExternalID: 2},
			{ID: "pM", Name: "MirellaFPV", ExternalID: 3},
		},
		Races: []ftRace{{
			ID: "r1", ExternalID: 7, End: "2026-06-20T05:03:55", TargetLaps: 3,
			Detections: []ftDetection{
				{ID: "dW", Pilot: "pW", Valid: true},
				{ID: "dL", Pilot: "pL", Valid: true},
				{ID: "dM", Pilot: "pM", Valid: true},
			},
			Laps: laps,
		}},
		Results: []ftResult{
			// Clean, distinct, but time-trial-ranked positions — must be ignored.
			{ID: "rM", Race: "r1", Pilot: "pM", Position: 1},
			{ID: "rL", Race: "r1", Pilot: "pL", Position: 2},
			{ID: "rW", Race: "r1", Pilot: "pW", Position: 3},
		},
	}

	out := snap.BracketResults()
	if len(out) != 1 {
		t.Fatalf("expected 1 bracket result, got %d", len(out))
	}
	var got []string
	for _, f := range out[0].Finishers {
		got = append(got, f.PilotName)
	}
	want := []string{"Willman", "Lucid", "MirellaFPV"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("finish-line order: got %v want %v (positions are TT-ranked and must be ignored)", got, want)
	}
}
