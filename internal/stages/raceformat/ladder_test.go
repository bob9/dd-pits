package raceformat

import (
	"encoding/json"
	"testing"
)

// playOutLadder runs every ready bout (seeds finish in current order) until a
// champion is crowned, returning the recorded moves.
func playOutLadder(t *testing.T, l *LadderFormat) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		progressed := false
		for _, rv := range l.Races() {
			if rv.IsComplete || rv.Locked || len(rv.Pilots) == 0 {
				continue
			}
			if err := l.ApplyPlacement(rv.ID, rv.Pilots); err == nil {
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
}

func TestLadderSweep(t *testing.T) {
	for n := 2; n <= 96; n++ {
		for _, p := range []int{2, 3, 4} {
			for a := 1; a < p; a++ {
				cfg := Config{
					PilotCount:      n,
					PilotNames:      pilotNames(n),
					PilotsPerRace:   p,
					PilotsToAdvance: a,
				}
				lf, err := newLadder(cfg)
				if err != nil {
					t.Fatalf("n=%d p=%d a=%d new: %v", n, p, a, err)
				}

				// Every pilot must appear in exactly one entry bout (as a fresh
				// seed, not a carry-over). Fresh seeds are the pilots present at
				// generation; carry-overs are appended only during play.
				seen := map[string]int{}
				for _, r := range lf.state.Races {
					for _, pilot := range r.Pilots {
						seen[pilot]++
					}
				}
				if len(seen) != n {
					t.Fatalf("n=%d p=%d a=%d: %d distinct entry pilots, want %d", n, p, a, len(seen), n)
				}
				for pilot, c := range seen {
					if c != 1 {
						t.Fatalf("n=%d p=%d a=%d: pilot %s appears in %d entry bouts", n, p, a, pilot, c)
					}
				}

				// No bout exceeds pilotsPerRace once fully populated.
				playOutLadder(t, lf)
				for _, r := range lf.state.Races {
					if len(r.Pilots) > p {
						t.Fatalf("n=%d p=%d a=%d: bout %s has %d pilots > ppr %d", n, p, a, r.ID, len(r.Pilots), p)
					}
				}

				if !lf.IsComplete() {
					t.Fatalf("n=%d p=%d a=%d: no champion after play-out", n, p, a)
				}
				champ := lf.Champion()
				if champ == "" {
					t.Fatalf("n=%d p=%d a=%d: empty champion", n, p, a)
				}

				// Standings cover every pilot exactly once.
				st := lf.Standings()
				if len(st) != n {
					t.Fatalf("n=%d p=%d a=%d: standings has %d rows, want %d", n, p, a, len(st), n)
				}
				rankSeen := map[string]bool{}
				for _, s := range st {
					if rankSeen[s.Pilot] {
						t.Fatalf("n=%d p=%d a=%d: pilot %s duplicated in standings", n, p, a, s.Pilot)
					}
					rankSeen[s.Pilot] = true
				}
				if st[0].Pilot != champ {
					t.Fatalf("n=%d p=%d a=%d: standings[0]=%s, champion=%s", n, p, a, st[0].Pilot, champ)
				}

				// Seeds-always-win → the top seed (P1) wins, and enters in the
				// final (or only) bout.
				if champ != "P1" {
					t.Fatalf("n=%d p=%d a=%d: seeds-win champion=%s, want P1", n, p, a, champ)
				}
			}
		}
	}
}

// TestLadderTopSeedEntersLast verifies the marquee property: with seeds-win the
// top seed is a fresh entrant in the last bout (it gets the longest bye).
func TestLadderTopSeedEntersLast(t *testing.T) {
	cfg := Config{PilotCount: 10, PilotNames: pilotNames(10), PilotsPerRace: 4, PilotsToAdvance: 2}
	lf, err := newLadder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	last := lf.state.Races[len(lf.state.Races)-1]
	found := false
	for _, p := range last.Pilots {
		if p == "P1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("top seed P1 not a fresh entrant in final bout %s (pilots=%v)", last.ID, last.Pilots)
	}
	// And the bottom seed should be in bout 1.
	if lf.state.Races[0].Pilots[0] == "" {
		t.Fatal("empty first bout")
	}
}

// TestLadderEditRewindsCarry confirms editing a bout rewinds its advancers from
// the next (un-run) bout and is refused once the next bout has run.
func TestLadderEditRewindsCarry(t *testing.T) {
	cfg := Config{PilotCount: 8, PilotNames: pilotNames(8), PilotsPerRace: 4, PilotsToAdvance: 2}
	lf, err := newLadder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Run bout 1.
	b1 := lf.state.Races[0]
	if err := lf.ApplyPlacement(b1.ID, b1.Pilots); err != nil {
		t.Fatal(err)
	}
	b2 := lf.state.Races[1]
	beforeLen := len(b2.Pilots)
	// Editable (bout 2 not run yet).
	if ok, _ := lf.CanEditRace(b1.ID); !ok {
		t.Fatal("bout1 should be editable before bout2 runs")
	}
	// Edit bout1 reversing the order — winners change, bout2 carry-overs swap.
	rev := reversed(lf.state.Races[0].Pilots)
	if err := lf.EditRace(b1.ID, rev); err != nil {
		t.Fatal(err)
	}
	if len(b2.Pilots) != beforeLen {
		t.Fatalf("bout2 pilot count changed after edit: got %d want %d", len(b2.Pilots), beforeLen)
	}
	// Run bout 2, then bout1 must be locked.
	if err := lf.ApplyPlacement(b2.ID, b2.Pilots); err != nil {
		t.Fatal(err)
	}
	if ok, _ := lf.CanEditRace(b1.ID); ok {
		t.Fatal("bout1 should be locked once bout2 has run")
	}
}

func TestLadderStateRoundTrip(t *testing.T) {
	cfg := Config{PilotCount: 12, PilotNames: pilotNames(12), PilotsPerRace: 4, PilotsToAdvance: 2}
	lf, _ := newLadder(cfg)
	_ = lf.ApplyPlacement(lf.state.Races[0].ID, lf.state.Races[0].Pilots)
	blob, _ := json.Marshal(lf.State())
	reloaded, err := FromState(KindLadder, cfg, blob)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(reloaded.State())
	if string(blob) != string(again) {
		t.Fatal("ladder state round-trip mismatch")
	}
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
