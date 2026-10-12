package raceformat

import (
	"encoding/json"
	"testing"

	"github.com/bob9/dd-pits/internal/stages/bracket"
)

// pilotNames builds n sequential seed names ("P1".."Pn").
func pilotNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "P" + itoaTest(i+1)
	}
	return out
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

type move struct {
	raceID    string
	placement []string
}

// playOut runs every runnable race (seeds finish in their current race order)
// until no progress is made, driving the format purely through the Format
// interface, and records every move applied so it can be replayed verbatim.
func playOut(t *testing.T, f Format) []move {
	t.Helper()
	var moves []move
	for i := 0; i < 500; i++ {
		progressed := false
		for _, rv := range f.Races() {
			if rv.IsComplete || rv.IsVoid || rv.Locked {
				continue
			}
			if len(rv.Pilots) == 0 {
				continue
			}
			placement := append([]string(nil), rv.Pilots...)
			if err := f.ApplyPlacement(rv.ID, placement); err == nil {
				moves = append(moves, move{rv.ID, placement})
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return moves
}

// TestEliminationAdapterParity drives a bracket through both the raw engine and
// the Format adapter with identical placements and asserts identical champion +
// serialized state. This is the de-risking gate: the adapter must be a faithful
// pass-through.
func TestEliminationAdapterParity(t *testing.T) {
	combos := []struct {
		elim   string
		finals string
	}{
		{"single", "double-elimination"},
		{"double", "double-elimination"},
		{"single", "chase-the-ace"},
		{"double", "chase-the-ace"},
	}
	for _, c := range combos {
		for n := 2; n <= 48; n++ {
			cfg := Config{
				PilotCount:      n,
				PilotNames:      pilotNames(n),
				EliminationType: c.elim,
				PilotsPerRace:   4,
				PilotsToAdvance: 2,
				FinalsFormat:    c.finals,
				AceWinsRequired: 2,
			}

			// Drive the adapter and capture the exact move sequence.
			f, err := New(KindElimination, cfg)
			if err != nil {
				t.Fatalf("%s/%s n=%d adapter New: %v", c.elim, c.finals, n, err)
			}
			moves := playOut(t, f)

			// Replay the identical sequence on the raw engine.
			raw, err := bracket.New(eliminationConfig(cfg))
			if err != nil {
				t.Fatalf("%s/%s n=%d raw New: %v", c.elim, c.finals, n, err)
			}
			for _, mv := range moves {
				if err := raw.ApplyPlacement(mv.raceID, mv.placement); err != nil {
					t.Fatalf("%s/%s n=%d raw replay %s: %v", c.elim, c.finals, n, mv.raceID, err)
				}
			}

			if raw.Champion() != f.Champion() {
				t.Fatalf("%s/%s n=%d champion mismatch: raw=%q adapter=%q",
					c.elim, c.finals, n, raw.Champion(), f.Champion())
			}
			rawJSON, _ := json.Marshal(raw.State())
			adpJSON, _ := json.Marshal(f.State())
			if string(rawJSON) != string(adpJSON) {
				t.Fatalf("%s/%s n=%d state mismatch", c.elim, c.finals, n)
			}
		}
	}
}

// TestFromStateRoundTrip confirms a serialized adapter state rehydrates to an
// equivalent format (the path buildFormat takes when loading a persisted row).
func TestFromStateRoundTrip(t *testing.T) {
	cfg := Config{
		PilotCount:      16,
		PilotNames:      pilotNames(16),
		EliminationType: "double",
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    "chase-the-ace",
		AceWinsRequired: 2,
	}
	f, err := New(KindElimination, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Run a couple of races, persist, reload.
	races := f.Races()
	if len(races) > 0 {
		_ = f.ApplyPlacement(races[0].ID, races[0].Pilots)
	}
	blob, err := json.Marshal(f.State())
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := FromState(KindElimination, cfg, blob)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(reloaded.State())
	if string(blob) != string(again) {
		t.Fatalf("round-trip state mismatch")
	}
}
