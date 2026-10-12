package raceformat

import "testing"

// containsAny reports whether any of names appears in a race (other than
// exceptID) across the format's heats — used to detect downstream propagation.
func appearsInOtherRace(f Format, exceptID string, names []string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for _, rv := range f.Races() {
		if rv.ID == exceptID {
			continue
		}
		for _, p := range rv.Pilots {
			if set[p] {
				return true
			}
		}
	}
	return false
}

func raceByID(f Format, id string) (RaceView, bool) {
	for _, rv := range f.Races() {
		if rv.ID == id {
			return rv, true
		}
	}
	return RaceView{}, false
}

// TestResetRace checks that ResetRace un-runs a completed heat, strips its
// advancers from downstream, is a no-op when not run, and is refused once a
// downstream heat has been run.
func TestResetRace(t *testing.T) {
	mk := func() Format {
		f, err := newElimination(Config{
			PilotCount:      8,
			PilotNames:      pilotNames(8),
			EliminationType: "double",
			PilotsPerRace:   4,
			PilotsToAdvance: 2,
			FinalsFormat:    "double-elimination",
		})
		if err != nil {
			t.Fatalf("new elimination: %v", err)
		}
		return f
	}

	// 1. Run one first-round heat, then reset it.
	f := mk()
	var first RaceView
	for _, rv := range f.Races() {
		if len(rv.Pilots) == 4 && !rv.IsComplete {
			first = rv
			break
		}
	}
	if first.ID == "" {
		t.Fatal("no runnable first-round race")
	}
	order := append([]string(nil), first.Pilots...)
	if err := f.ApplyPlacement(first.ID, order); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rv, _ := raceByID(f, first.ID); !rv.IsComplete {
		t.Fatal("race should be complete after apply")
	}
	if !appearsInOtherRace(f, first.ID, order[:2]) {
		t.Fatal("winners should have propagated downstream after apply")
	}

	if err := f.ResetRace(first.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if rv, _ := raceByID(f, first.ID); rv.IsComplete {
		t.Fatal("race should be pending after reset")
	}
	if appearsInOtherRace(f, first.ID, order[:2]) {
		t.Fatal("winners should be stripped from downstream after reset")
	}

	// 2. Reset on a not-run race is a no-op (no error).
	if err := f.ResetRace(first.ID); err != nil {
		t.Fatalf("reset of not-run race should be a no-op: %v", err)
	}

	// 3. Reset is refused once a downstream heat has been run.
	f2 := mk()
	moves := playOut(t, f2) // play the whole thing
	if len(moves) == 0 {
		t.Fatal("expected the bracket to play out")
	}
	if err := f2.ResetRace(moves[0].raceID); err == nil {
		t.Fatalf("expected reset of %s to be refused once downstream ran", moves[0].raceID)
	}
}
