package raceformat

import "testing"

func seedsFor(n int) map[string]int {
	m := map[string]int{}
	for i, name := range pilotNames(n) {
		m[name] = i + 1
	}
	return m
}

// TestFinalOrderingEliminationCoversAllPilots drives each elimination/finals combo
// to completion (seeds win) and checks FinalOrdering fills a contiguous, complete,
// duplicate-free ranking with the real champion at rank 1 — even though the
// engine's own Standings() only knows the champion / CTA finalists.
func TestFinalOrderingEliminationCoversAllPilots(t *testing.T) {
	combos := []struct{ elim, finals string }{
		{"single", "double-elimination"},
		{"double", "double-elimination"},
		{"single", "chase-the-ace"},
		{"double", "chase-the-ace"},
	}
	for _, c := range combos {
		for _, n := range []int{4, 5, 8, 12, 16, 32} {
			cfg := Config{
				PilotCount:      n,
				PilotNames:      pilotNames(n),
				EliminationType: c.elim,
				PilotsPerRace:   4,
				PilotsToAdvance: 2,
				FinalsFormat:    c.finals,
				AceWinsRequired: 2,
			}
			f, err := New(KindElimination, cfg)
			if err != nil {
				t.Fatalf("%s/%s n=%d New: %v", c.elim, c.finals, n, err)
			}
			playOut(t, f)
			if !f.IsComplete() {
				t.Fatalf("%s/%s n=%d: playOut left bracket incomplete", c.elim, c.finals, n)
			}

			order := FinalOrdering(f, seedsFor(n))
			if len(order) != n {
				t.Fatalf("%s/%s n=%d: ordering has %d rows, want %d", c.elim, c.finals, n, len(order), n)
			}
			if order[0].Pilot != f.Champion() {
				t.Fatalf("%s/%s n=%d: rank 1 = %q, want champion %q", c.elim, c.finals, n, order[0].Pilot, f.Champion())
			}
			seen := map[string]bool{}
			for i, s := range order {
				if s.Rank != i+1 {
					t.Fatalf("%s/%s n=%d: row %d has rank %d", c.elim, c.finals, n, i, s.Rank)
				}
				if seen[s.Pilot] {
					t.Fatalf("%s/%s n=%d: pilot %q appears twice", c.elim, c.finals, n, s.Pilot)
				}
				seen[s.Pilot] = true
			}
			for _, p := range pilotNames(n) {
				if !seen[p] {
					t.Fatalf("%s/%s n=%d: pilot %q missing from ordering", c.elim, c.finals, n, p)
				}
			}
		}
	}
}

// TestFinalOrderingMatchesStandingsWhenComplete confirms that for formats whose
// Standings() already ranks every pilot (mains, ladder), FinalOrdering is a
// pass-through — same pilots, same order, nothing appended.
func TestFinalOrderingMatchesStandingsWhenComplete(t *testing.T) {
	// Mains: 8 pilots, A/B mains, 2 rounds, F1 points.
	mf, err := newMains(mainsConfig(8, []string{"A", "B"}, 2, mainsDescending, f1Points))
	if err != nil {
		t.Fatal(err)
	}
	playOut(t, mf)
	if !mf.IsComplete() {
		t.Fatal("mains: playOut left format incomplete")
	}
	assertOrderingEqualsStandings(t, "mains", mf, seedsFor(8))

	// Ladder: 9 pilots, 4 per race, top 2 advance.
	lf, err := newLadder(Config{
		PilotCount:      9,
		PilotNames:      pilotNames(9),
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	playOut(t, lf)
	if !lf.IsComplete() {
		t.Fatal("ladder: playOut left format incomplete")
	}
	assertOrderingEqualsStandings(t, "ladder", lf, seedsFor(9))
}

func assertOrderingEqualsStandings(t *testing.T, label string, f Format, seeds map[string]int) {
	t.Helper()
	std := f.Standings()
	order := FinalOrdering(f, seeds)
	if len(order) != len(std) {
		t.Fatalf("%s: ordering has %d rows, Standings has %d", label, len(order), len(std))
	}
	for i := range std {
		if order[i].Pilot != std[i].Pilot {
			t.Fatalf("%s: row %d = %q, want %q", label, i, order[i].Pilot, std[i].Pilot)
		}
	}
}
