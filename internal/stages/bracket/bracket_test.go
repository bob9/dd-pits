package bracket

import (
	"fmt"
	"sort"
	"testing"
)

// playOutTournament drives the bracket to completion by repeatedly picking the
// lowest-round incomplete race that has all its pilots and selecting the top N.
// Returns once no more progress is possible.
func playOutTournament(t *testing.T, b *Bracket) {
	t.Helper()
	safety := 2000
	for safety > 0 {
		safety--
		incomplete := []*Race{}
		for _, r := range b.State().Races {
			if !r.IsComplete && !r.IsVoid && len(r.Pilots) > 0 {
				incomplete = append(incomplete, r)
			}
		}
		if len(incomplete) == 0 {
			break
		}
		sort.Slice(incomplete, func(i, j int) bool {
			if incomplete[i].Round != incomplete[j].Round {
				return incomplete[i].Round < incomplete[j].Round
			}
			// Within a round, order numerically by trailing index so CTA-2
			// comes before CTA-10 (sequential CTA gating needs this).
			ni, nj := raceIndexNum(incomplete[i].ID), raceIndexNum(incomplete[j].ID)
			if ni != nj {
				return ni < nj
			}
			return incomplete[i].ID < incomplete[j].ID
		})
		race := incomplete[0]
		// Use the bracket library's own expected count — it knows about
		// consolidation races' FinalTarget overrides and single-elim finals.
		winnerCount := b.ExpectedWinnersCount(race)
		if winnerCount > len(race.Pilots) {
			winnerCount = len(race.Pilots)
		}
		if winnerCount < 1 {
			winnerCount = 1
		}
		winners := append([]string(nil), race.Pilots[:winnerCount]...)
		if err := b.SelectWinners(race.ID, winners); err != nil {
			t.Fatalf("SelectWinners(%s) failed: %v", race.ID, err)
		}
	}
	if safety == 0 {
		t.Fatalf("play-out exceeded safety limit")
	}
}

func pilotNames(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("P%d", i+1)
	}
	return out
}

func validateInitialBracket(t *testing.T, b *Bracket) {
	t.Helper()
	cfg := b.Config()
	state := b.State()

	// All pilots placed in round 1
	round1 := []*Race{}
	for _, r := range state.Races {
		if r.Round == 1 {
			round1 = append(round1, r)
		}
	}
	placed := 0
	for _, r := range round1 {
		placed += len(r.Pilots)
	}
	// For chase-the-ace with ≤4 pilots, all pilots may land directly in CTA-1, not W1
	if cfg.FinalsFormat == FinalsChaseTheAce && cfg.EliminationType == EliminationSingle && cfg.PilotCount <= 4 {
		// pilots already in CTA-1 — check round 1 has them
		for _, r := range state.Races {
			if r.Round == 1 && r.Bracket == SlotChaseTheAce {
				placed = len(r.Pilots)
			}
		}
	}
	if placed != cfg.PilotCount {
		t.Errorf("placed %d pilots in round 1, want %d", placed, cfg.PilotCount)
	}

	// Round 1 race sizes within ±1 (snake seeding)
	if len(round1) > 1 {
		minS, maxS := len(round1[0].Pilots), len(round1[0].Pilots)
		for _, r := range round1 {
			if len(r.Pilots) < minS {
				minS = len(r.Pilots)
			}
			if len(r.Pilots) > maxS {
				maxS = len(r.Pilots)
			}
		}
		if maxS-minS > 1 {
			t.Errorf("round 1 race sizes unbalanced: min=%d max=%d", minS, maxS)
		}
	}

	// No race exceeds capacity
	for _, r := range state.Races {
		if len(r.Pilots) > cfg.PilotsPerRace {
			t.Errorf("race %s has %d pilots (max %d)", r.ID, len(r.Pilots), cfg.PilotsPerRace)
		}
	}
}

func validateFinalState(t *testing.T, b *Bracket) {
	t.Helper()
	for _, r := range b.State().Races {
		if !r.IsComplete && !r.IsVoid && len(r.Pilots) > 0 {
			t.Errorf("race %s left incomplete with %d pilots", r.ID, len(r.Pilots))
		}
	}
	cfg := b.Config()
	if cfg.FinalsFormat == FinalsChaseTheAce && cfg.PilotCount >= 4 {
		if b.State().ChaseTheAce == nil || b.State().ChaseTheAce.Champion == "" {
			t.Errorf("expected chase-the-ace champion to be set")
		}
	}
}

func TestBracketAllPilotCounts(t *testing.T) {
	configs := []struct {
		label           string
		eliminationType EliminationType
		finalsFormat    FinalsFormat
	}{
		{"double+CTA", EliminationDouble, FinalsChaseTheAce},
		{"single+CTA", EliminationSingle, FinalsChaseTheAce},
		{"double+GF", EliminationDouble, FinalsGrandFinal},
		{"single+final", EliminationSingle, FinalsGrandFinal},
	}

	for _, cfg := range configs {
		cfg := cfg
		t.Run(cfg.label, func(t *testing.T) {
			for pilotCount := 2; pilotCount <= 96; pilotCount++ {
				pilotCount := pilotCount
				t.Run(fmt.Sprintf("%d_pilots", pilotCount), func(t *testing.T) {
					b, err := New(Config{
						PilotCount:      pilotCount,
						PilotNames:      pilotNames(pilotCount),
						EliminationType: cfg.eliminationType,
						PilotsPerRace:   4,
						PilotsToAdvance: 2,
						FinalsFormat:    cfg.finalsFormat,
						AceWinsRequired: 2,
					})
					if err != nil {
						t.Fatalf("New: %v", err)
					}
					validateInitialBracket(t, b)
					playOutTournament(t, b)
					validateFinalState(t, b)
				})
			}
		})
	}
}

// TestBracketCompletesTightConfigs covers double-elim configurations where
// the winners-bracket's adv/ppr ratio is tight enough that convergence
// stalls without the "stop when no progress" guard. The hang the user
// reported (19 pilots × 3-per-race × 2-advance × double+CTA) is in this set.
//
// Scope: realistic tournament sizes (≥ ppr*2 + 1 pilots so there's room
// for some elimination), double-elim only — single-elim with adv close to
// ppr has separate convergence issues that aren't what the user reported.
func TestBracketCompletesTightConfigs(t *testing.T) {
	type matrixCfg struct {
		ppr int
		adv int
	}
	matrix := []matrixCfg{
		{3, 2}, // ← user's case
		{4, 2},
		{5, 2},
		{6, 2},
		{6, 3},
		{8, 4},
	}
	formats := []struct {
		label string
		final FinalsFormat
	}{
		{"double+CTA", FinalsChaseTheAce},
		{"double+GF", FinalsGrandFinal},
	}

	for _, m := range matrix {
		m := m
		for _, f := range formats {
			f := f
			t.Run(fmt.Sprintf("%s_pp%d_adv%d", f.label, m.ppr, m.adv), func(t *testing.T) {
				// Skip tiny brackets where CTA can't be filled
				start := m.ppr*2 + 1
				if start < 8 {
					start = 8
				}
				for pc := start; pc <= 64; pc++ {
					pc := pc
					t.Run(fmt.Sprintf("%d_pilots", pc), func(t *testing.T) {
						b, err := New(Config{
							PilotCount:      pc,
							PilotNames:      pilotNames(pc),
							EliminationType: EliminationDouble,
							PilotsPerRace:   m.ppr,
							PilotsToAdvance: m.adv,
							FinalsFormat:    f.final,
							AceWinsRequired: 2,
						})
						if err != nil {
							t.Fatalf("New: %v", err)
						}
						playOutTournament(t, b)

						// (a) no race left with pilots but incomplete
						for _, r := range b.State().Races {
							if !r.IsComplete && !r.IsVoid && len(r.Pilots) > 0 {
								t.Errorf("stuck race %s round=%d slot=%s pilots=%v expected=%d",
									r.ID, r.Round, r.Bracket, r.Pilots, b.ExpectedWinnersCount(r))
							}
						}
						// (b) finals format actually produces a champion
						if f.final == FinalsChaseTheAce {
							if cta := b.State().ChaseTheAce; cta == nil || !cta.IsComplete {
								t.Errorf("CTA never completed; champion=%q", b.Champion())
							}
						} else if b.Champion() == "" {
							t.Errorf("no champion was crowned")
						}
					})
				}
			})
		}
	}
}

func TestSnakeSeeding(t *testing.T) {
	// 13 pilots / 4 races should snake to 3+3+3+4, top seeds spread out.
	groups := createBalancedSeeding(pilotNames(13), 4)
	if len(groups) != 4 {
		t.Fatalf("expected 4 races, got %d", len(groups))
	}
	sizes := []int{len(groups[0]), len(groups[1]), len(groups[2]), len(groups[3])}
	want := []int{3, 3, 3, 4}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("race %d size = %d, want %d (all sizes: %v)", i, sizes[i], want[i], sizes)
		}
	}
	// Seed 1 (P1) and seed 2 (P2) must be in different races.
	r0, r1 := groups[0][0], groups[1][0]
	if r0 != "P1" || r1 != "P2" {
		t.Errorf("expected seed 1 in race 0 and seed 2 in race 1, got %q and %q", r0, r1)
	}
}

func TestPlaceBalanced(t *testing.T) {
	// 6 W1 winners feeding 2 W2 races should produce 3+3, not 4+2,
	// regardless of which W1 race completes first.
	b, err := New(Config{
		PilotCount:      9,
		PilotNames:      pilotNames(9),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Find W1 races and complete each with the first 2 pilots as winners.
	w1 := []*Race{}
	for _, r := range b.State().Races {
		if r.Round == 1 {
			w1 = append(w1, r)
		}
	}
	sort.Slice(w1, func(i, j int) bool { return w1[i].ID < w1[j].ID })
	for _, r := range w1 {
		winners := append([]string(nil), r.Pilots[:2]...)
		if err := b.SelectWinners(r.ID, winners); err != nil {
			t.Fatalf("SelectWinners(%s): %v", r.ID, err)
		}
	}
	// Check W2-1 and W2-2 sizes
	var w2 []*Race
	for _, r := range b.State().Races {
		if r.Round == 2 && r.Bracket == SlotWinners {
			w2 = append(w2, r)
		}
	}
	if len(w2) != 2 {
		t.Fatalf("expected 2 W2 races, got %d", len(w2))
	}
	sort.Slice(w2, func(i, j int) bool { return w2[i].ID < w2[j].ID })
	if len(w2[0].Pilots) != 3 || len(w2[1].Pilots) != 3 {
		t.Errorf("expected W2-1=3, W2-2=3, got %d and %d", len(w2[0].Pilots), len(w2[1].Pilots))
	}
}

// TestSelectFullPlacement verifies that recording the complete finish order
// of a race (a) reorders race.Pilots to placement order so the loser ranks
// are preserved for display, and (b) extracts the right N as winners.
func TestSelectFullPlacement(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// W1-1 has 4 pilots after snake seeding. Pick an order that's NOT the
	// initial pilot order so we know reordering happened.
	var w1 *Race
	for _, r := range b.State().Races {
		if r.ID == "W1-1" {
			w1 = r
			break
		}
	}
	if w1 == nil {
		t.Fatalf("W1-1 missing")
	}
	if len(w1.Pilots) != 4 {
		t.Fatalf("expected 4 pilots in W1-1, got %d", len(w1.Pilots))
	}
	original := append([]string(nil), w1.Pilots...)
	// Reverse the order
	placement := []string{original[3], original[2], original[1], original[0]}
	if err := b.SelectFullPlacement("W1-1", placement); err != nil {
		t.Fatalf("SelectFullPlacement: %v", err)
	}
	if !equal(w1.Pilots, placement) {
		t.Errorf("race.Pilots not reordered to placement order: got %v want %v", w1.Pilots, placement)
	}
	wantWinners := placement[:2]
	if !equal(w1.Winners, wantWinners) {
		t.Errorf("Winners = %v, want %v (top 2 from placement)", w1.Winners, wantWinners)
	}
	if !equal(w1.Losers, placement[2:]) {
		t.Errorf("Losers = %v, want %v (placement[2:])", w1.Losers, placement[2:])
	}
}

// TestEditRaceWhileDownstreamClean covers the happy path: edit a race whose
// winners landed in a still-incomplete next-round race. The old winners get
// stripped from the downstream race; the new winners get placed.
func TestEditRaceWhileDownstreamClean(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var w1 *Race
	for _, r := range b.State().Races {
		if r.ID == "W1-1" {
			w1 = r
			break
		}
	}
	original := append([]string(nil), w1.Pilots...)
	// Initial result — accept the first 2 as winners.
	if err := b.SelectFullPlacement("W1-1", original); err != nil {
		t.Fatalf("initial place: %v", err)
	}
	firstWinners := append([]string(nil), w1.Winners...)

	// Find a downstream race that received one of the winners; it should be
	// incomplete at this point.
	if !w1.IsComplete {
		t.Fatalf("W1-1 should be complete after initial submit")
	}
	canEdit, reason := b.CanEditRace("W1-1")
	if !canEdit {
		t.Fatalf("expected CanEdit true; got false: %s", reason)
	}

	// Edit with reversed order so the previous losers are now winners.
	newPlacement := []string{original[3], original[2], original[1], original[0]}
	if err := b.EditRace("W1-1", newPlacement); err != nil {
		t.Fatalf("EditRace: %v", err)
	}
	if !w1.IsComplete {
		t.Fatalf("W1-1 should still be complete after edit")
	}
	if equal(w1.Winners, firstWinners) {
		t.Errorf("winners should have changed after edit; still %v", w1.Winners)
	}
	if !equal(w1.Winners, newPlacement[:2]) {
		t.Errorf("Winners = %v, want %v", w1.Winners, newPlacement[:2])
	}

	// The new winners must now appear in a W2 race (where the old winners
	// used to be). The old winners (now losers) must NOT remain in any
	// winners-bracket downstream race — they should be in the losers
	// bracket if anything.
	newWinSet := stringSet(w1.Winners)
	oldWinSet := stringSet(firstWinners)
	foundInW2 := 0
	for _, r := range b.State().Races {
		if r.ID == "W1-1" || r.Bracket != SlotWinners || r.Round <= 1 {
			continue
		}
		for _, p := range r.Pilots {
			if _, ok := newWinSet[p]; ok {
				foundInW2++
			}
			if _, ok := oldWinSet[p]; ok {
				if _, stillNew := newWinSet[p]; !stillNew {
					t.Errorf("old winner %q still in winners-bracket race %s after edit", p, r.ID)
				}
			}
		}
	}
	if foundInW2 != len(w1.Winners) {
		t.Errorf("expected %d new winners in W2 races, found %d", len(w1.Winners), foundInW2)
	}
}

// TestEditRaceBlockedByCompletedDownstream verifies that once any downstream
// race has been run, the edit is refused — editing would invalidate work
// already recorded.
func TestEditRaceBlockedByCompletedDownstream(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Play out W1 entirely.
	for _, r := range b.State().Races {
		if r.Round == 1 && r.Bracket == SlotWinners {
			if err := b.SelectFullPlacement(r.ID, r.Pilots); err != nil {
				t.Fatalf("W1 place: %v", err)
			}
		}
	}
	// Find any W2 race that exists and has pilots — complete it.
	var w2 *Race
	for _, r := range b.State().Races {
		if r.Round == 2 && r.Bracket == SlotWinners && len(r.Pilots) > 0 && !r.IsComplete {
			w2 = r
			break
		}
	}
	if w2 == nil {
		t.Skip("no W2 race available to complete")
	}
	if err := b.SelectFullPlacement(w2.ID, w2.Pilots); err != nil {
		t.Fatalf("W2 place: %v", err)
	}

	// Now find a W1 race whose winners contributed to the completed W2 and
	// confirm the edit is refused.
	for _, r := range b.State().Races {
		if r.Round != 1 || r.Bracket != SlotWinners {
			continue
		}
		// Is any winner from r now in w2.Pilots?
		for _, p := range r.Winners {
			for _, q := range w2.Pilots {
				if p == q {
					ok, reason := b.CanEditRace(r.ID)
					if ok {
						t.Errorf("CanEdit(%s) = true; expected false (downstream %s is complete)", r.ID, w2.ID)
					}
					if reason == "" {
						t.Errorf("CanEdit(%s) returned empty reason", r.ID)
					}
					if err := b.EditRace(r.ID, r.Pilots); err == nil {
						t.Errorf("EditRace(%s) returned nil error; expected refusal", r.ID)
					}
					return
				}
			}
		}
	}
	t.Skip("no W1 race feeds the completed W2 race")
}

// ---------- chase-the-ace: pre-created max races ----------

// ctaRaceIDsSorted returns every CTA race ID (incl. voided) in numeric order.
func ctaRaceIDsSorted(b *Bracket) []string {
	ids := []string{}
	for _, r := range b.State().Races {
		if r.Bracket == SlotChaseTheAce {
			ids = append(ids, r.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return raceIndexNum(ids[i]) < raceIndexNum(ids[j]) })
	return ids
}

// driveNonCTASides plays out every winners/losers race (seeds win) so the CTA
// finalists get populated, without touching the CTA finals themselves.
func driveNonCTASides(t *testing.T, b *Bracket) {
	t.Helper()
	safety := 2000
	for safety > 0 {
		safety--
		ran := false
		for _, r := range b.State().Races {
			if r.Bracket == SlotChaseTheAce || r.IsComplete || r.IsVoid || len(r.Pilots) == 0 {
				continue
			}
			n := b.ExpectedWinnersCount(r)
			if n > len(r.Pilots) {
				n = len(r.Pilots)
			}
			if n < 1 {
				n = 1
			}
			if err := b.SelectWinners(r.ID, r.Pilots[:n]); err != nil {
				t.Fatalf("SelectWinners(%s): %v", r.ID, err)
			}
			ran = true
			break
		}
		if !ran {
			break
		}
	}
	if safety == 0 {
		t.Fatalf("driveNonCTASides exceeded safety limit")
	}
}

// TestChaseTheAceMaxRacesPreCreated asserts the number of CTA finals races
// created up front equals (RequiredWins-1)*finalistCount + 1.
func TestChaseTheAceMaxRacesPreCreated(t *testing.T) {
	cases := []struct {
		name         string
		pilotCount   int
		requiredWins int
		wantRaces    int
	}{
		{"8_double_reqWins2", 8, 2, 5}, // (2-1)*4 + 1 = 5
		{"8_double_reqWins3", 8, 3, 9}, // (3-1)*4 + 1 = 9
		{"16_double_reqWins2", 16, 2, 5},
		{"8_double_reqWins4", 8, 4, 13}, // (4-1)*4 + 1 = 13
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			b, err := New(Config{
				PilotCount:      c.pilotCount,
				PilotNames:      pilotNames(c.pilotCount),
				EliminationType: EliminationDouble,
				PilotsPerRace:   4,
				PilotsToAdvance: 2,
				FinalsFormat:    FinalsChaseTheAce,
				AceWinsRequired: c.requiredWins,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ids := ctaRaceIDsSorted(b)
			if len(ids) != c.wantRaces {
				t.Errorf("got %d CTA races %v, want %d", len(ids), ids, c.wantRaces)
			}
			// IDs must be the contiguous CTA-1 … CTA-max sequence.
			for i, id := range ids {
				want := fmt.Sprintf("CTA-%d", i+1)
				if id != want {
					t.Errorf("CTA race %d id = %q, want %q", i, id, want)
				}
			}
		})
	}
}

// TestChaseTheAceMaxFormulaHelper checks the pure max-race formula directly.
func TestChaseTheAceMaxFormulaHelper(t *testing.T) {
	cases := []struct{ finalists, reqWins, want int }{
		{4, 2, 5},
		{4, 3, 9},
		{4, 4, 13},
		{2, 2, 3},
		{3, 2, 4},
		{4, 1, 1}, // single win required → one race only
	}
	for _, c := range cases {
		if got := maxChaseTheAceRaces(c.finalists, c.reqWins); got != c.want {
			t.Errorf("maxChaseTheAceRaces(%d,%d) = %d, want %d", c.finalists, c.reqWins, got, c.want)
		}
	}
}

// TestChaseTheAceAllRacesCarryFinalists verifies that once the W/L sides are
// done, EVERY pre-created CTA race holds the 4 finalists (so they can all be
// exported to the timing system up front).
func TestChaseTheAceAllRacesCarryFinalists(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)
	cta := b.State().ChaseTheAce
	if cta == nil || len(cta.Finalists) != 4 {
		t.Fatalf("expected 4 finalists, got %v", cta)
	}
	for _, r := range b.State().Races {
		if r.Bracket != SlotChaseTheAce {
			continue
		}
		if !equal(r.Pilots, cta.Finalists) {
			t.Errorf("CTA race %s pilots = %v, want finalists %v", r.ID, r.Pilots, cta.Finalists)
		}
	}
}

// findEditableCTAFeeder returns a completed non-CTA race that feeds the chase-
// the-ace finalists, still has a non-advancing pilot to promote, and is editable
// (its only run downstream is the un-run CTA) — i.e. the redemption final.
func findEditableCTAFeeder(b *Bracket) *Race {
	cta := b.State().ChaseTheAce
	for _, r := range b.State().Races {
		if r.Bracket == SlotChaseTheAce || !r.IsComplete || len(r.Pilots) <= len(r.Winners) {
			continue
		}
		feeds := false
		for _, w := range r.Winners {
			if stringInSlice(w, cta.Finalists) {
				feeds = true
				break
			}
		}
		if !feeds {
			continue
		}
		if ok, _ := b.CanEditRace(r.ID); ok {
			return r
		}
	}
	return nil
}

// TestChaseTheAceFeederEditableUntilSeriesStarts is the regression guard for the
// premature edit-lock: a winners/redemption final that feeds the chase-the-ace
// final must stay editable until a CTA heat is actually run, and correcting it
// before then must update who makes the final. Previously the finalist set locked
// the instant both sides completed, freezing a just-entered redemption result.
func TestChaseTheAceFeederEditableUntilSeriesStarts(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)

	cta := b.State().ChaseTheAce
	if cta == nil || len(cta.Finalists) != 4 {
		t.Fatalf("expected 4 finalists after the sides ran, got %v", cta)
	}

	feeder := findEditableCTAFeeder(b)
	if feeder == nil {
		t.Fatal("no editable CTA feeder found (redemption final should be editable pre-series)")
	}

	// Promote a non-advancing pilot of the feeder to 1st — this changes WHO makes
	// the final, which the finalist set must follow.
	var promoted string
	for _, p := range feeder.Pilots {
		if !stringInSlice(p, feeder.Winners) {
			promoted = p
			break
		}
	}
	if promoted == "" {
		t.Fatalf("feeder %s has no non-advancer to promote: %v", feeder.ID, feeder.Pilots)
	}
	if stringInSlice(promoted, cta.Finalists) {
		t.Fatalf("precondition: %s already a finalist", promoted)
	}
	newOrder := []string{promoted}
	for _, p := range feeder.Pilots {
		if p != promoted {
			newOrder = append(newOrder, p)
		}
	}
	if err := b.EditRace(feeder.ID, newOrder); err != nil {
		t.Fatalf("edit feeder %s: %v", feeder.ID, err)
	}
	if !stringInSlice(promoted, b.State().ChaseTheAce.Finalists) {
		t.Fatalf("finalists did not follow the corrected feeder: %v (want %s in)",
			b.State().ChaseTheAce.Finalists, promoted)
	}

	// Run the first CTA heat — now the feeder must freeze.
	firstCTA := ctaRaceIDsSorted(b)[0]
	cr := b.findRace(firstCTA)
	if err := b.SelectFullPlacement(firstCTA, append([]string(nil), cr.Pilots...)); err != nil {
		t.Fatalf("run first CTA heat %s: %v", firstCTA, err)
	}
	if ok, _ := b.CanEditRace(feeder.ID); ok {
		t.Errorf("feeder %s must be frozen once a CTA heat has been run", feeder.ID)
	}
}

// TestChaseTheAcePlayThroughVoidsRemainder plays the CTA out to a champion and
// asserts the right number of races ran and the remainder were voided (not the
// on-demand append of the old behaviour).
func TestChaseTheAcePlayThroughVoidsRemainder(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)
	cta := b.State().ChaseTheAce
	finalists := append([]string(nil), cta.Finalists...)

	// Two different pilots win CTA-1 and CTA-2 (no clinch), then the first
	// wins CTA-3 to reach 2 wins.
	if err := b.SelectWinners("CTA-1", []string{finalists[0]}); err != nil {
		t.Fatalf("CTA-1: %v", err)
	}
	if err := b.SelectWinners("CTA-2", []string{finalists[1]}); err != nil {
		t.Fatalf("CTA-2: %v", err)
	}
	if err := b.SelectWinners("CTA-3", []string{finalists[0]}); err != nil {
		t.Fatalf("CTA-3: %v", err)
	}
	if cta.Champion != finalists[0] {
		t.Errorf("champion = %q, want %q", cta.Champion, finalists[0])
	}
	if !cta.IsComplete || !b.IsComplete() {
		t.Errorf("expected CTA + bracket complete")
	}
	// All 5 pre-created heats remain in state; the 3 that ran are complete, the
	// 2 surplus are voided (kept so an un-crowning edit can reopen them).
	ctaCount, ranCount, voidCount := 0, 0, 0
	for _, r := range b.State().Races {
		if r.Bracket != SlotChaseTheAce {
			continue
		}
		ctaCount++
		switch {
		case r.IsComplete:
			ranCount++
			if r.IsVoid {
				t.Errorf("CTA race %s is complete but also voided", r.ID)
			}
		case r.IsVoid:
			voidCount++
		default:
			t.Errorf("CTA race %s is neither complete nor voided after champion", r.ID)
		}
	}
	if ctaCount != 5 {
		t.Errorf("CTA races present = %d, want 5 (none deleted)", ctaCount)
	}
	if ranCount != 3 {
		t.Errorf("completed CTA races = %d, want 3", ranCount)
	}
	if voidCount != 2 {
		t.Errorf("voided surplus CTA races = %d, want 2", voidCount)
	}
}

// TestChaseTheAceEarlyChampionVoidsRest verifies a straight-sets champion (wins
// RequiredWins back-to-back) voids every later CTA race immediately.
func TestChaseTheAceEarlyChampionVoidsRest(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)
	cta := b.State().ChaseTheAce
	champ := cta.Finalists[0]

	if err := b.SelectWinners("CTA-1", []string{champ}); err != nil {
		t.Fatalf("CTA-1: %v", err)
	}
	if err := b.SelectWinners("CTA-2", []string{champ}); err != nil {
		t.Fatalf("CTA-2: %v", err)
	}
	if cta.Champion != champ {
		t.Fatalf("champion = %q, want %q", cta.Champion, champ)
	}
	for _, id := range []string{"CTA-3", "CTA-4", "CTA-5"} {
		r := b.findRace(id)
		if r == nil {
			t.Errorf("%s should remain (voided, not deleted) after early champion", id)
			continue
		}
		if !r.IsVoid {
			t.Errorf("%s should be voided after early champion", id)
		}
	}
	for _, id := range []string{"CTA-1", "CTA-2"} {
		r := b.findRace(id)
		if r == nil {
			t.Errorf("%s should remain (it ran)", id)
			continue
		}
		if r.IsVoid {
			t.Errorf("%s ran and must not be voided", id)
		}
	}
}

// TestChaseTheAceOutOfOrderRejected asserts a CTA result can only be recorded
// for the next un-run heat, and that voided/post-champion entries are refused.
func TestChaseTheAceOutOfOrderRejected(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)
	cta := b.State().ChaseTheAce
	finalists := append([]string(nil), cta.Finalists...)

	// CTA-2 before CTA-1 must be rejected.
	if err := b.SelectWinners("CTA-2", []string{finalists[0]}); err == nil {
		t.Errorf("expected out-of-order CTA-2 to be rejected before CTA-1 runs")
	}
	// Full-placement entry path is gated too.
	if err := b.SelectFullPlacement("CTA-3", finalists); err == nil {
		t.Errorf("expected out-of-order CTA-3 full placement to be rejected")
	}
	// CTA-1 then CTA-2 are accepted in order; champion is crowned.
	if err := b.SelectWinners("CTA-1", []string{finalists[0]}); err != nil {
		t.Fatalf("CTA-1: %v", err)
	}
	if err := b.SelectWinners("CTA-2", []string{finalists[0]}); err != nil {
		t.Fatalf("CTA-2: %v", err)
	}
	// Post-champion entry on a voided race is refused.
	if err := b.SelectWinners("CTA-3", []string{finalists[0]}); err == nil {
		t.Errorf("expected CTA-3 to be rejected after champion crowned")
	}
}

// TestChaseTheAceEditDoesNotDoubleCount is the regression for the bug where
// editing (or re-ingesting) a CTA heat's result inflated the winner's win tally
// — a single heat counted twice, prematurely crowning a champion and voiding the
// remaining heats. EditRace routes through ResetRace + a fresh apply, so the win
// must stay at exactly 1 no matter how many times the same result is re-applied.
func TestChaseTheAceEditDoesNotDoubleCount(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)
	cta := b.State().ChaseTheAce
	champ := cta.Finalists[0]

	// Run CTA-1 (one legitimate win), then edit/re-ingest it several times with
	// the same winner — the kind of repeated apply FPVTrackside does on re-sends.
	if err := b.SelectWinners("CTA-1", []string{champ}); err != nil {
		t.Fatalf("CTA-1: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.EditRace("CTA-1", []string{champ}); err != nil {
			t.Fatalf("edit CTA-1 #%d: %v", i, err)
		}
	}
	if got := cta.Wins[champ]; got != 1 {
		t.Fatalf("after 1 win + 3 re-applies, wins = %d, want 1 (double-count bug)", got)
	}
	if cta.Champion != "" || cta.IsComplete {
		t.Fatalf("champion crowned off a single CTA win (champion=%q complete=%v)", cta.Champion, cta.IsComplete)
	}
	// The remaining heats must still be live (not voided away by a false crown).
	if next := b.nextChaseTheAceRace(); next == nil || next.ID != "CTA-2" {
		t.Fatalf("next runnable CTA = %v, want CTA-2", next)
	}
	// A second genuine win legitimately crowns.
	if err := b.SelectWinners("CTA-2", []string{champ}); err != nil {
		t.Fatalf("CTA-2: %v", err)
	}
	if cta.Champion != champ || !cta.IsComplete {
		t.Fatalf("champion not crowned after 2 real wins (champion=%q)", cta.Champion)
	}
}

// TestChaseTheAceUncrownReopensVoids verifies that editing the clinching heat to
// a non-clinching result un-crowns the champion and brings the voided surplus
// heats back into play (they are voided, not deleted).
func TestChaseTheAceUncrownReopensVoids(t *testing.T) {
	b, err := New(Config{
		PilotCount:      8,
		PilotNames:      pilotNames(8),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsChaseTheAce,
		AceWinsRequired: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	driveNonCTASides(t, b)
	cta := b.State().ChaseTheAce
	a, c := cta.Finalists[0], cta.Finalists[1]

	// a wins CTA-1 and CTA-2 → champion, surplus (CTA-3..5) voided.
	if err := b.SelectWinners("CTA-1", []string{a}); err != nil {
		t.Fatalf("CTA-1: %v", err)
	}
	if err := b.SelectWinners("CTA-2", []string{a}); err != nil {
		t.Fatalf("CTA-2: %v", err)
	}
	if !cta.IsComplete {
		t.Fatalf("expected champion after two wins for %q", a)
	}
	if r := b.findRace("CTA-3"); r == nil || !r.IsVoid {
		t.Fatalf("CTA-3 should be voided while champion stands")
	}

	// Correct CTA-2 to a different winner — no one has 2 wins now.
	if err := b.EditRace("CTA-2", []string{c}); err != nil {
		t.Fatalf("edit CTA-2: %v", err)
	}
	if cta.Champion != "" || cta.IsComplete {
		t.Fatalf("champion should be un-crowned after edit (champion=%q)", cta.Champion)
	}
	if cta.Wins[a] != 1 || cta.Wins[c] != 1 {
		t.Fatalf("wins after un-crown: %q=%d %q=%d, want 1 and 1", a, cta.Wins[a], c, cta.Wins[c])
	}
	// CTA-3 must be live again so the series can continue to a decision.
	r3 := b.findRace("CTA-3")
	if r3 == nil || r3.IsVoid {
		t.Fatalf("CTA-3 should be reopened (live) after un-crown")
	}
	if next := b.nextChaseTheAceRace(); next == nil || next.ID != "CTA-3" {
		t.Fatalf("next runnable CTA = %v, want CTA-3", next)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPlaceSeededAcrossPilotCounts checks that, after all W1 races finish with
// top-N pilots advancing (seeds win), the seeded placement keeps top seeds apart
// and prevents same-heat rematches in W2 — across a wide range of pilot counts
// and pilots-per-race. Skips counts where W2 collapses to a single race (forced).
//
// For exactly-saturated configs (numAdvancers == numTargets*pilotsPerRace), the
// no-rematch guarantee is best-effort: zero capacity slack can force a single
// overflow back to the source's other target. The top-2 seed split is still
// guaranteed (it's the user-visible MultiGP property).
func TestPlaceSeededAcrossPilotCounts(t *testing.T) {
	cases := []struct {
		pilotsPerRace int
		pilotsAdvance int
	}{
		{4, 2},
		{4, 3},
		{3, 2},
		{6, 2},
		{6, 3},
		{8, 4},
	}

	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("pp%d_adv%d", c.pilotsPerRace, c.pilotsAdvance), func(t *testing.T) {
			for pc := c.pilotsPerRace + 1; pc <= 96; pc++ {
				b, err := New(Config{
					PilotCount:      pc,
					PilotNames:      pilotNames(pc),
					EliminationType: EliminationDouble,
					PilotsPerRace:   c.pilotsPerRace,
					PilotsToAdvance: c.pilotsAdvance,
					FinalsFormat:    FinalsGrandFinal,
				})
				if err != nil {
					t.Fatalf("pc=%d: New: %v", pc, err)
				}

				// Collect W1 races and the pilots in each (in seed order).
				w1 := []*Race{}
				for _, r := range b.State().Races {
					if r.Round == 1 && r.Bracket == SlotWinners {
						w1 = append(w1, r)
					}
				}
				sort.Slice(w1, func(i, j int) bool { return w1[i].ID < w1[j].ID })

				// Seeds win their heats: advance the top N pilots from each race.
				// Skip auto-advanced races (e.g. 2-pilot heat with advance=2 → auto-complete).
				heatTops := map[string][]string{}
				for _, r := range w1 {
					if r.IsComplete {
						heatTops[r.ID] = append([]string(nil), r.Winners...)
						continue
					}
					n := c.pilotsAdvance
					if n > len(r.Pilots) {
						n = len(r.Pilots)
					}
					winners := append([]string(nil), r.Pilots[:n]...)
					heatTops[r.ID] = winners
					if err := b.SelectWinners(r.ID, winners); err != nil {
						t.Fatalf("pc=%d: SelectWinners(%s): %v", pc, r.ID, err)
					}
				}

				w2 := []*Race{}
				for _, r := range b.State().Races {
					if r.Round == 2 && r.Bracket == SlotWinners {
						w2 = append(w2, r)
					}
				}
				if len(w2) < 2 {
					continue // forced into single W2 race / final — no split possible
				}

				raceOf := map[string]string{}
				for _, r := range w2 {
					for _, p := range r.Pilots {
						raceOf[p] = r.ID
					}
				}

				// (1) seeds 1 and 2 must be in different W2 races (always)
				if raceOf["P1"] != "" && raceOf["P2"] != "" && raceOf["P1"] == raceOf["P2"] {
					t.Errorf("pc=%d: seeds 1 and 2 share W2 race %s", pc, raceOf["P1"])
				}

				// (2) no W1 heat's top-N advancers should all land in the same W2 race
				// (best-effort: only enforced when there's some capacity slack — when
				// the next round is exactly saturated, overflow can force a rematch)
				totalAdvancers := 0
				for _, tops := range heatTops {
					totalAdvancers += len(tops)
				}
				capacity := len(w2) * c.pilotsPerRace
				saturated := totalAdvancers >= capacity
				if !saturated {
					for heatID, tops := range heatTops {
						if len(tops) < 2 {
							continue
						}
						first := raceOf[tops[0]]
						same := true
						for _, p := range tops[1:] {
							if raceOf[p] != first {
								same = false
								break
							}
						}
						if same && first != "" {
							t.Errorf("pc=%d: all of heat %s's advancers ended up in W2 race %s",
								pc, heatID, first)
						}
					}
				}

				// (3) no W2 race may exceed PilotsPerRace
				for _, r := range w2 {
					if len(r.Pilots) > c.pilotsPerRace {
						t.Errorf("pc=%d: W2 race %s has %d pilots (max %d)",
							pc, r.ID, len(r.Pilots), c.pilotsPerRace)
					}
				}
			}
		})
	}
}

// TestPlaceSeededSplitsTopSeeds pins MultiGP-style placement: when 3 W1 heats each
// advance their top 2 into 2 W2 races, the heats' 1st-placers must split across
// the two W2 races (so seeds 1 and 2 don't meet in W2), and each heat's 1st and
// 2nd advancer must land in different W2 races.
func TestPlaceSeededSplitsTopSeeds(t *testing.T) {
	b, err := New(Config{
		PilotCount:      9,
		PilotNames:      pilotNames(9),
		EliminationType: EliminationSingle,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsGrandFinal,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w1 := []*Race{}
	for _, r := range b.State().Races {
		if r.Round == 1 {
			w1 = append(w1, r)
		}
	}
	sort.Slice(w1, func(i, j int) bool { return w1[i].ID < w1[j].ID })
	if len(w1) != 3 {
		t.Fatalf("expected 3 W1 races, got %d", len(w1))
	}

	// Top 2 in each heat advance — i.e. seed order is preserved.
	heatTops := map[string][2]string{}
	for _, r := range w1 {
		winners := []string{r.Pilots[0], r.Pilots[1]}
		heatTops[r.ID] = [2]string{winners[0], winners[1]}
		if err := b.SelectWinners(r.ID, winners); err != nil {
			t.Fatalf("SelectWinners(%s): %v", r.ID, err)
		}
	}

	var w2 []*Race
	for _, r := range b.State().Races {
		if r.Round == 2 && r.Bracket == SlotWinners {
			w2 = append(w2, r)
		}
	}
	if len(w2) != 2 {
		t.Fatalf("expected 2 W2 races, got %d", len(w2))
	}
	sort.Slice(w2, func(i, j int) bool { return w2[i].ID < w2[j].ID })

	raceOf := map[string]string{}
	for _, r := range w2 {
		for _, p := range r.Pilots {
			raceOf[p] = r.ID
		}
	}

	// (1) Seeds 1 and 2 must be in different W2 races.
	if raceOf["P1"] == "" || raceOf["P2"] == "" {
		t.Fatalf("expected seeds 1 and 2 placed in W2, got map %v", raceOf)
	}
	if raceOf["P1"] == raceOf["P2"] {
		t.Errorf("seeds 1 and 2 ended up in same W2 race %q (top seeds should split)", raceOf["P1"])
	}

	// (2) Each heat's 1st and 2nd must go to different W2 races (no rematch in W2).
	for heatID, tops := range heatTops {
		if raceOf[tops[0]] == raceOf[tops[1]] {
			t.Errorf("heat %s's 1st (%s) and 2nd (%s) both in %s — should split",
				heatID, tops[0], tops[1], raceOf[tops[0]])
		}
	}

	// (3) Distribution should still be 3+3.
	if len(w2[0].Pilots) != 3 || len(w2[1].Pilots) != 3 {
		t.Errorf("expected W2 sizes 3+3, got %d and %d", len(w2[0].Pilots), len(w2[1].Pilots))
	}
}

// TestMultiPilotHeatNotAutoResulted verifies that a heat where everyone
// advances (not enough pilots to eliminate anyone) is NOT auto-resulted when an
// upstream result cascades through checkAutoAdvanceAll — the pilots still race
// it, so it must stay open for the director to record the finishing order. Only
// a genuine solo bye auto-advances.
func TestMultiPilotHeatNotAutoResulted(t *testing.T) {
	// 5 pilots, 4/race, 2 advance → snake seeding makes a 3-pilot heat and a
	// 2-pilot heat in round 1; the 2-pilot heat has advance=2 (everyone through).
	b, err := New(Config{
		PilotCount:      5,
		PilotNames:      pilotNames(5),
		EliminationType: EliminationDouble,
		PilotsPerRace:   4,
		PilotsToAdvance: 2,
		FinalsFormat:    FinalsGrandFinal,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var twoPilotHeat, biggerHeat *Race
	for _, r := range b.State().Races {
		if r.Round == 1 && r.Bracket == SlotWinners {
			switch {
			case len(r.Pilots) == 2:
				twoPilotHeat = r
			case len(r.Pilots) > 2:
				biggerHeat = r
			}
		}
	}
	if twoPilotHeat == nil || biggerHeat == nil {
		t.Fatalf("expected a 2-pilot and a >2-pilot W1 heat")
	}
	if twoPilotHeat.IsComplete {
		t.Fatalf("2-pilot heat %s auto-resulted at generation", twoPilotHeat.ID)
	}

	// Recording the bigger heat triggers the auto-advance cascade.
	if err := b.SelectFullPlacement(biggerHeat.ID, append([]string(nil), biggerHeat.Pilots...)); err != nil {
		t.Fatalf("SelectFullPlacement(%s): %v", biggerHeat.ID, err)
	}

	if twoPilotHeat.IsComplete {
		t.Errorf("2-pilot heat %s was auto-resulted before the pilots raced it", twoPilotHeat.ID)
	}
}

// TestChaseTheAceFinalistsFollowPilotsPerRace pins the chase-the-ace final at a
// full heat rather than a fixed four pilots: a club racing 6-up finishes with
// six finalists, a 3-up club with three (four would not fit the heat). The
// classic 4-up config is unchanged.
func TestChaseTheAceFinalistsFollowPilotsPerRace(t *testing.T) {
	cases := []struct {
		elim      EliminationType
		ppr, adv  int
		pilots    int
		finalists int
	}{
		{EliminationDouble, 4, 2, 24, 4},
		{EliminationDouble, 6, 2, 24, 6},
		{EliminationDouble, 3, 1, 24, 3},
		{EliminationSingle, 4, 2, 16, 4},
		{EliminationSingle, 6, 2, 24, 6},
		// Field smaller than a heat: every pilot is a finalist.
		{EliminationDouble, 6, 2, 5, 5},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%s_pp%d_adv%d_%dpilots", c.elim, c.ppr, c.adv, c.pilots), func(t *testing.T) {
			b, err := New(Config{
				PilotCount:      c.pilots,
				PilotNames:      pilotNames(c.pilots),
				EliminationType: c.elim,
				PilotsPerRace:   c.ppr,
				PilotsToAdvance: c.adv,
				FinalsFormat:    FinalsChaseTheAce,
				AceWinsRequired: 2,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			playOutTournament(t, b)

			cta := b.State().ChaseTheAce
			if cta == nil {
				t.Fatal("no chase-the-ace state")
			}
			if len(cta.Finalists) != c.finalists {
				t.Errorf("finalists = %d (%v), want %d", len(cta.Finalists), cta.Finalists, c.finalists)
			}
			if !cta.IsComplete || cta.Champion == "" {
				t.Errorf("CTA never completed; champion=%q", cta.Champion)
			}
			// Every CTA heat that actually ran carried the full finalist field.
			for _, r := range b.State().Races {
				if r.Bracket != SlotChaseTheAce || r.IsVoid || !r.IsComplete {
					continue
				}
				if len(r.Pilots) != c.finalists {
					t.Errorf("%s has %d pilots, want %d", r.ID, len(r.Pilots), c.finalists)
				}
			}
		})
	}
}

// losersShape returns the redemption bracket as one entry per L round:
// "L{n}:{heats}" plus "<W{r}" when that round takes winners-round r's droppers.
func losersShape(b *Bracket) []string {
	rounds := map[int]int{}
	from := map[int]int{}
	for _, r := range b.State().Races {
		if r.Bracket != SlotLosers {
			continue
		}
		rounds[r.Round]++
		if r.FromWinnersRound > 0 {
			from[r.Round] = r.FromWinnersRound
		}
	}
	var keys []int
	for k := range rounds {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]string, 0, len(keys))
	for i, k := range keys {
		s := fmt.Sprintf("L%d:%d", i+1, rounds[k])
		if w := from[k]; w > 0 {
			s += fmt.Sprintf("<W%d", w)
		}
		out = append(out, s)
	}
	return out
}

// The redemption bracket runs the real double-elimination cadence: after the
// first two intakes it alternates a MINOR round (halves the redemption pool,
// nobody new enters) with a MAJOR round (survivors + the next winners round's
// droppers). Feeding winners round N straight into losers round N instead let
// the pool run ahead of the field it merged with — a 48-pilot / 6-up / top-3
// bracket got a 3-heat redemption round 3 (12 survivors + 6 fresh droppers)
// where the sheet halves 12 into 2 heats first and takes the droppers in round 4.
func TestLosersBracketAlternatesMinorAndMajorRounds(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			// The 16-pilot MultiGP double-elimination sheet, transcribed by hand
			// in the qualbracket package: L1×2, L2×2, L3×1 (halving), L4×1.
			name: "16 pilots, 4-up, top 2 (MultiGP sheet)",
			cfg:  Config{PilotCount: 16, PilotsPerRace: 4, PilotsToAdvance: 2},
			want: []string{"L1:2<W1", "L2:2<W2", "L3:1", "L4:1<W3"},
		},
		{
			name: "48 pilots, 6-up, top 3",
			cfg:  Config{PilotCount: 48, PilotsPerRace: 6, PilotsToAdvance: 3},
			want: []string{"L1:4<W1", "L2:4<W2", "L3:2", "L4:2<W3", "L5:1", "L6:1<W4"},
		},
		{
			name: "24 pilots, 6-up, top 3",
			cfg:  Config{PilotCount: 24, PilotsPerRace: 6, PilotsToAdvance: 3},
			want: []string{"L1:2<W1", "L2:2<W2", "L3:1", "L4:1<W3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.EliminationType = EliminationDouble
			cfg.FinalsFormat = FinalsChaseTheAce
			cfg.AceWinsRequired = 2
			for i := 0; i < cfg.PilotCount; i++ {
				cfg.PilotNames = append(cfg.PilotNames, fmt.Sprintf("P%02d", i+1))
			}
			b, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := losersShape(b)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("redemption shape\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// A winners round's droppers must land in the redemption round stamped for them,
// not the round that happens to sit one column across — with minor rounds in the
// mix those are no longer the same thing.
func TestWinnersDroppersLandInTheirStampedRedemptionRound(t *testing.T) {
	cfg := Config{
		PilotCount: 48, PilotsPerRace: 6, PilotsToAdvance: 3,
		EliminationType: EliminationDouble, FinalsFormat: FinalsChaseTheAce, AceWinsRequired: 2,
	}
	for i := 0; i < cfg.PilotCount; i++ {
		cfg.PilotNames = append(cfg.PilotNames, fmt.Sprintf("P%02d", i+1))
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// W3 drops into L4 (round 5) here, NOT L3 (round 4) — L3 is the halving round.
	if got := b.losersDropRound(3); got != 5 {
		t.Fatalf("losersDropRound(3) = %d, want 5 (L4)", got)
	}
	// A bracket saved before FromWinnersRound existed keeps the old mapping.
	for _, r := range b.State().Races {
		r.FromWinnersRound = 0
	}
	if got := b.losersDropRound(3); got != 4 {
		t.Fatalf("legacy fallback losersDropRound(3) = %d, want 4", got)
	}
}
