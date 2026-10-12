package raceformat

import (
	"encoding/json"
	"testing"
)

// oldDogs2026State is the stored bracket_state of the 2026 Nationals Old Dogs
// bracket (race_brackets.id 110): single elimination, 6-up, top 3, with a
// full finishing order recorded for the final (R3-1).
const oldDogs2026State = `{"races": [{"id": "race-1", "round": 1, "losers": ["halcyon", "Spanna"], "pilots": ["Foxur", "Clog wog", "Croz", "halcyon", "Spanna"], "bracket": "winners", "winners": ["Foxur", "Clog wog", "Croz"], "isComplete": true, "fullPlacement": true}, {"id": "race-3", "round": 1, "losers": ["Tumblebee", "Has0"], "pilots": ["tilted", "tex", "JohnnyGMachine", "Tumblebee", "Has0"], "bracket": "winners", "winners": ["tilted", "tex", "JohnnyGMachine"], "isComplete": true, "fullPlacement": true}, {"id": "race-2", "round": 1, "losers": ["Nacho", "A440"], "pilots": ["Voland", "bob9", "timmytron", "Nacho", "A440"], "bracket": "winners", "winners": ["Voland", "bob9", "timmytron"], "isComplete": true, "fullPlacement": true}, {"id": "race-4", "round": 1, "losers": ["Shane O", "Karade"], "pilots": ["CGO", "MacDaddy", "Manic Tardis", "Shane O", "Karade"], "bracket": "winners", "winners": ["CGO", "MacDaddy", "Manic Tardis"], "isComplete": true, "fullPlacement": true}, {"id": "R2-1", "round": 2, "losers": ["tex", "Croz", "Manic Tardis"], "pilots": ["Foxur", "MacDaddy", "bob9", "tex", "Croz", "Manic Tardis"], "bracket": "winners", "winners": ["Foxur", "MacDaddy", "bob9"], "isComplete": true, "fullPlacement": true}, {"id": "R2-2", "round": 2, "losers": ["timmytron", "JohnnyGMachine", "tilted"], "pilots": ["Voland", "CGO", "Clog wog", "timmytron", "JohnnyGMachine", "tilted"], "bracket": "winners", "winners": ["Voland", "CGO", "Clog wog"], "isComplete": true, "fullPlacement": true}, {"id": "R3-1", "round": 3, "losers": ["bob9", "Voland", "Clog wog", "MacDaddy", "CGO"], "pilots": ["Foxur", "bob9", "Voland", "Clog wog", "MacDaddy", "CGO"], "bracket": "winners", "winners": ["Foxur"], "isComplete": true, "fullPlacement": true}], "rounds": 3, "currentRound": 1}`

// TestEliminationStandingsFollowTheFinal pins that the final's recorded
// finishing order is the podium. Before the fix only the champion came from
// the bracket and FinalOrdering ranked the rest of the final by seed, which
// published Foxur, Voland, MacDaddy for a final run Foxur, bob9, Voland,
// Clog wog.
func TestEliminationStandingsFollowTheFinal(t *testing.T) {
	names := []string{"Foxur", "tilted", "Voland", "MacDaddy", "CGO", "bob9", "Tumblebee", "Clog wog", "halcyon", "tex",
		"timmytron", "Shane O", "Karade", "Nacho", "JohnnyGMachine", "Spanna", "Croz", "Has0", "A440", "Manic Tardis"}
	cfg := Config{PilotCount: len(names), PilotNames: names, EliminationType: "single",
		PilotsPerRace: 6, PilotsToAdvance: 3, FinalsFormat: "double-elimination", AceWinsRequired: 1}
	f, err := eliminationFromState(cfg, json.RawMessage(oldDogs2026State))
	if err != nil {
		t.Fatal(err)
	}
	seeds := map[string]int{}
	for i, n := range names {
		seeds[n] = i + 1
	}
	got := FinalOrdering(f, seeds)
	want := []string{"Foxur", "bob9", "Voland", "Clog wog", "MacDaddy", "CGO"}
	if len(got) < len(want) {
		t.Fatalf("got %d standings, want at least %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Pilot != w || got[i].Rank != i+1 {
			t.Errorf("rank %d = %q (rank %d), want %q", i+1, got[i].Pilot, got[i].Rank, w)
		}
	}
	if len(got) != len(names) {
		t.Errorf("FinalOrdering listed %d pilots, want all %d", len(got), len(names))
	}
}
