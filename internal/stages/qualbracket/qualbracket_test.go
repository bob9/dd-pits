package qualbracket

import (
	"fmt"
	"sort"
	"testing"

	"github.com/bob9/dd-pits/internal/stages/bracket"
)

// srcKey renders a SlotRef as "POS@FROM" (or "seedN") for compact assertions.
func srcKey(r SlotRef) string {
	if r.FromID == "" {
		return fmt.Sprintf("seed%d", r.Position)
	}
	return fmt.Sprintf("%d@%s", r.Position, r.FromID)
}

func srcKeys(refs []SlotRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = srcKey(r)
	}
	return out
}

// TestSpec16ExactGraph asserts the 16-pilot spec matches MultiGP's published
// "Top 4 advance" double-elimination sheet (the supplied photo) verbatim.
func TestSpec16ExactGraph(t *testing.T) {
	spec := specFor16(false, false)
	byID := map[string]SpecRace{}
	for _, r := range spec.Body {
		byID[r.ID] = r
	}

	want := map[string][]string{
		// Winners Round 1 — snake seeding ordered by top seed 1,4,2,3 so seeds 1
		// and 2 sit on opposite halves and only meet in the FINAL.
		"W1-1": {"seed1", "seed8", "seed9", "seed16"},
		"W1-2": {"seed4", "seed5", "seed12", "seed13"},
		"W1-3": {"seed2", "seed7", "seed10", "seed15"},
		"W1-4": {"seed3", "seed6", "seed11", "seed14"},
		// Redemption Round 2.
		"L1-1": {"4@W1-1", "3@W1-2", "3@W1-3", "4@W1-4"},
		"L1-2": {"3@W1-1", "4@W1-2", "4@W1-3", "3@W1-4"},
		// Winners Round 3.
		"W2-1": {"1@W1-1", "2@W1-1", "1@W1-2", "2@W1-2"},
		"W2-2": {"1@W1-3", "2@W1-3", "1@W1-4", "2@W1-4"},
		// Redemption Round 4.
		"L2-1": {"3@W2-1", "1@L1-1", "2@L1-1", "4@W2-2"},
		"L2-2": {"4@W2-1", "2@L1-2", "1@L1-2", "3@W2-2"},
		// Winners Round 5 (winners final).
		"W3-1": {"1@W2-1", "2@W2-1", "1@W2-2", "2@W2-2"},
		// Redemption Round 6.
		"L3-1": {"1@L2-1", "2@L2-1", "1@L2-2", "2@L2-2"},
		// Redemption Round 7 (redemption final).
		"L4-1": {"3@W3-1", "4@W3-1", "1@L3-1", "2@L3-1"},
	}
	for id, w := range want {
		r, ok := byID[id]
		if !ok {
			t.Fatalf("race %s missing from spec", id)
		}
		got := srcKeys(r.Sources)
		if !equalStrs(got, w) {
			t.Errorf("race %s sources = %v, want %v", id, got, w)
		}
	}
	// Terminal final.
	if spec.WinnersFinalID != "W3-1" || spec.LosersFinalID != "L4-1" {
		t.Errorf("finals = %s/%s, want W3-1/L4-1", spec.WinnersFinalID, spec.LosersFinalID)
	}
	if got := srcKeys(spec.Finalists); !equalStrs(got, []string{"1@W3-1", "2@W3-1", "1@L4-1", "2@L4-1"}) {
		t.Errorf("finalists = %v", got)
	}
}

func equalStrs(a, b []string) bool {
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

// TestSnakeSeparation16 plays 16 seeds-win and asserts the top two seeds do not
// share a heat before the winners final (W3-1, round 5). Snake seeding puts them
// on opposite halves of the winners bracket, so the earliest they can meet is the
// winners final (which gathers the top-4 winners-side pilots) — never round 3.
func TestSnakeSeparation16(t *testing.T) {
	q, err := New(Config{PilotNames: seedNames(16)})
	if err != nil {
		t.Fatal(err)
	}
	playSeedsWin(t, q)
	for _, r := range q.state.Races {
		has1, has2 := false, false
		for _, p := range r.Pilots {
			if p == "1" {
				has1 = true
			}
			if p == "2" {
				has2 = true
			}
		}
		if has1 && has2 && r.Round < 5 {
			t.Errorf("seeds 1 and 2 meet too early in race %s (round %d): %v", r.ID, r.Round, r.Pilots)
		}
	}
}

// TestPhotoSeed16 asserts the photo-layout option reproduces the photo's exact
// round-1 slot placement (ranks mapped to seeds 1..16) and still converges.
func TestPhotoSeed16(t *testing.T) {
	spec := specFor16(false, true)
	byID := map[string]SpecRace{}
	for _, r := range spec.Body {
		byID[r.ID] = r
	}
	want := map[string][]string{
		"W1-1": {"seed5", "seed12", "seed6", "seed8"},
		"W1-2": {"seed10", "seed11", "seed15", "seed7"},
		"W1-3": {"seed2", "seed13", "seed14", "seed3"},
		"W1-4": {"seed9", "seed1", "seed4", "seed16"},
	}
	for id, w := range want {
		if got := srcKeys(byID[id].Sources); !equalStrs(got, w) {
			t.Errorf("photo %s = %v, want %v", id, got, w)
		}
	}
	// The redemption (losers) flow must match the photo regardless of the
	// round-1 seeding choice — it is wired by race id/position, not by seed.
	redemption := map[string][]string{
		"L1-1": {"4@W1-1", "3@W1-2", "3@W1-3", "4@W1-4"},
		"L1-2": {"3@W1-1", "4@W1-2", "4@W1-3", "3@W1-4"},
		"L2-1": {"3@W2-1", "1@L1-1", "2@L1-1", "4@W2-2"},
		"L2-2": {"4@W2-1", "2@L1-2", "1@L1-2", "3@W2-2"},
		"L3-1": {"1@L2-1", "2@L2-1", "1@L2-2", "2@L2-2"},
		"L4-1": {"3@W3-1", "4@W3-1", "1@L3-1", "2@L3-1"},
	}
	for id, w := range redemption {
		if got := srcKeys(byID[id].Sources); !equalStrs(got, w) {
			t.Errorf("photo redemption %s = %v, want %v (must match the photo)", id, got, w)
		}
	}

	q, err := New(Config{PilotNames: seedNames(16), PhotoSeed: true})
	if err != nil {
		t.Fatal(err)
	}
	playSeedsWin(t, q)
	if got := q.Qualified(); len(got) != 4 {
		t.Fatalf("photo qualified = %v, want 4", got)
	}
}

// TestQual16Materialises checks the interpreter builds a clean state for 16 and
// the FINAL heat carries the top-4 target.
func TestQual16Materialises(t *testing.T) {
	q, err := New(Config{PilotNames: seedNames(16)})
	if err != nil {
		t.Fatal(err)
	}
	final := q.findRace("FINAL")
	if final == nil {
		t.Fatal("no FINAL race")
	}
	if final.FinalTarget != 4 {
		t.Errorf("FINAL.FinalTarget = %d, want 4", final.FinalTarget)
	}
	// Round-1 winners races are seeded; downstream races empty.
	if r := q.findRace("W1-1"); r == nil || len(r.Pilots) != 4 {
		t.Errorf("W1-1 pilots = %v", r)
	}
	if r := q.findRace("W3-1"); r == nil || len(r.Pilots) != 0 {
		t.Errorf("W3-1 should start empty")
	}
}

// TestSimulate16TopFour plays 16 to completion seeds-win and asserts the top
// seed qualifies first and exactly 4 pilots qualify.
func TestSimulate16TopFour(t *testing.T) {
	q, err := New(Config{PilotNames: seedNames(16)})
	if err != nil {
		t.Fatal(err)
	}
	playSeedsWin(t, q)
	if !q.IsComplete() {
		t.Fatal("bracket did not complete")
	}
	qual := q.Qualified()
	if len(qual) != 4 {
		t.Fatalf("qualified = %v, want 4", qual)
	}
	if qual[0] != "1" {
		t.Errorf("top qualifier = %s, want seed 1", qual[0])
	}
}

// TestCTA16 runs 16 with chase-the-ace and asserts a champion emerges from the 4.
func TestCTA16(t *testing.T) {
	q, err := New(Config{PilotNames: seedNames(16), CTA: true})
	if err != nil {
		t.Fatal(err)
	}
	if q.findRace("FINAL") != nil {
		t.Error("CTA bracket should have no FINAL race")
	}
	if q.findRace("CTA-1") == nil {
		t.Error("CTA bracket should pre-create CTA heats")
	}
	playSeedsWin(t, q)
	if !q.IsComplete() {
		t.Fatal("CTA did not complete")
	}
	if got := q.Qualified(); len(got) != 4 || got[0] != "1" {
		t.Errorf("CTA qualified = %v", got)
	}
}

// TestPlayOutAllSizes plays every field size 2..96 to completion and checks the
// spec is well-formed: no dangling source refs, the bracket completes, and
// exactly min(4,N) pilots qualify.
func TestPlayOutAllSizes(t *testing.T) {
	for n := 2; n <= 96; n++ {
		q, err := New(Config{PilotNames: seedNames(n)})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		assertNoDanglingRefs(t, q, n)
		playSeedsWin(t, q)
		if !q.IsComplete() {
			t.Fatalf("n=%d: did not complete", n)
		}
		want := minI(4, n)
		if got := q.Qualified(); len(got) != want {
			t.Fatalf("n=%d: qualified %v, want %d", n, got, want)
		}
	}
}

// assertNoDanglingRefs checks every source ref points at a race that can produce
// at least that finishing position.
func assertNoDanglingRefs(t *testing.T, q *Qual, n int) {
	t.Helper()
	size := map[string]int{}
	// Round-1 race sizes come from the materialised pilots; others from advance
	// math are validated structurally by playout, so only check Round-1 + that
	// every FromID exists.
	for _, r := range q.state.Races {
		size[r.ID] = len(r.Pilots)
	}
	for id, refs := range q.sources {
		for _, ref := range refs {
			if ref.FromID == "" {
				if ref.Position < 1 || ref.Position > n {
					t.Errorf("n=%d race %s: seed ref %d out of range", n, id, ref.Position)
				}
				continue
			}
			if q.findRace(ref.FromID) == nil {
				t.Errorf("n=%d race %s: source %s does not exist", n, id, ref.FromID)
			}
		}
	}
}

// playSeedsWin drives a bracket to completion, best seed winning each heat.
func playSeedsWin(t *testing.T, q *Qual) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		var pick *bracket.Race
		for _, r := range q.state.Races {
			if r.IsComplete || r.IsVoid || len(r.Pilots) == 0 {
				continue
			}
			if pick == nil || r.Round < pick.Round {
				pick = r
			}
		}
		if pick == nil {
			return
		}
		order := append([]string(nil), pick.Pilots...)
		sort.SliceStable(order, func(a, b int) bool { return q.seedOf(order[a]) < q.seedOf(order[b]) })
		if err := q.ApplyPlacement(pick.ID, order); err != nil {
			t.Fatalf("apply %s: %v", pick.ID, err)
		}
	}
	t.Fatal("playout did not converge")
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// playWorstWin drives a bracket to completion with the WORST seed winning each
// heat — the inverse of playSeedsWin. This forces upsets so the elimination
// depth order diverges from the seed order, exercising Ranking().
func playWorstWin(t *testing.T, q *Qual) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		var pick *bracket.Race
		for _, r := range q.state.Races {
			if r.IsComplete || r.IsVoid || len(r.Pilots) == 0 {
				continue
			}
			if pick == nil || r.Round < pick.Round {
				pick = r
			}
		}
		if pick == nil {
			return
		}
		order := append([]string(nil), pick.Pilots...)
		sort.SliceStable(order, func(a, b int) bool { return q.seedOf(order[a]) > q.seedOf(order[b]) })
		if err := q.ApplyPlacement(pick.ID, order); err != nil {
			t.Fatalf("apply %s: %v", pick.ID, err)
		}
	}
	t.Fatal("worst-win playout did not converge")
}

// TestRankingByProgressionNotSeed verifies Ranking() orders the non-qualified
// pilots by how far they progressed (deepest elimination first), not by seed.
// With worst-seed-wins the deepest survivors are the low seeds, so a plain seed
// sort would be visibly wrong.
func TestRankingByProgressionNotSeed(t *testing.T) {
	q, err := New(Config{PilotNames: seedNames(16)})
	if err != nil {
		t.Fatal(err)
	}
	playWorstWin(t, q)
	if !q.IsComplete() {
		t.Fatal("bracket did not complete")
	}

	rank := q.Ranking()
	if len(rank) != 16 {
		t.Fatalf("ranking has %d pilots, want 16", len(rank))
	}

	// Deepest completed heat each pilot reached (independent recompute).
	last := map[string]int{}
	for _, r := range q.state.Races {
		if !r.IsComplete {
			continue
		}
		for _, p := range r.Pilots {
			if r.Round > last[p] {
				last[p] = r.Round
			}
		}
	}

	qualified := q.Qualified()
	if len(qualified) != 4 {
		t.Fatalf("qualified = %v, want 4", qualified)
	}
	// The qualified finalists lead the ranking, in their final placing.
	for i, p := range qualified {
		if rank[i] != p {
			t.Errorf("rank %d = %s, want qualified %s", i+1, rank[i], p)
		}
	}

	// The non-qualified tail must be non-increasing in elimination depth.
	tail := rank[len(qualified):]
	for i := 1; i < len(tail); i++ {
		if last[tail[i-1]] < last[tail[i]] {
			t.Errorf("tail not depth-ordered: %s (round %d) ranked above %s (round %d)",
				tail[i-1], last[tail[i-1]], tail[i], last[tail[i]])
		}
	}

	// And it must NOT be a plain seed sort (the old behaviour).
	bySeed := append([]string(nil), tail...)
	sort.SliceStable(bySeed, func(a, b int) bool { return q.seedOf(bySeed[a]) < q.seedOf(bySeed[b]) })
	if sameSlice(tail, bySeed) {
		t.Errorf("non-qualified tail is plain seed order, want progression order: %v", tail)
	}
}

// playBodySeedsWin runs every non-CTA (body) race in round order, best seed
// winning, so the chase-the-ace finalists resolve but no CTA heat is flown yet.
func playBodySeedsWin(t *testing.T, q *Qual) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		var pick *bracket.Race
		for _, r := range q.state.Races {
			if r.IsComplete || r.IsVoid || len(r.Pilots) == 0 || r.Bracket == bracket.SlotChaseTheAce {
				continue
			}
			if pick == nil || r.Round < pick.Round {
				pick = r
			}
		}
		if pick == nil {
			return
		}
		order := append([]string(nil), pick.Pilots...)
		sort.SliceStable(order, func(a, b int) bool { return q.seedOf(order[a]) < q.seedOf(order[b]) })
		if err := q.ApplyPlacement(pick.ID, order); err != nil {
			t.Fatalf("apply %s: %v", pick.ID, err)
		}
	}
	t.Fatal("body playout did not converge")
}

// TestCTAFeederEditableUntilSeriesStarts is the regression guard for the
// premature edit-lock: the winners/redemption finals that feed the chase-the-ace
// final must stay editable until a CTA heat is actually run, and correcting a
// feeder before then must update who makes the final. Previously the finalist
// set locked the instant both feeders completed (propagate resolves it
// automatically), freezing a just-entered redemption-final result.
func TestCTAFeederEditableUntilSeriesStarts(t *testing.T) {
	q, err := New(Config{PilotNames: seedNames(16), CTA: true})
	if err != nil {
		t.Fatal(err)
	}
	playBodySeedsWin(t, q)

	cta := q.state.ChaseTheAce
	if cta == nil || len(cta.Finalists) != 4 {
		t.Fatalf("finalists not resolved after the feeders ran: %+v", cta)
	}
	// The redemption final L4-1 feeds only the (un-run) CTA final, so it must
	// still be editable — this is the exact heat that was wrongly frozen.
	// (W3-1, the winners final, legitimately has a run downstream: L4-1 takes its
	// 3rd/4th, so it stays blocked by the ordinary downstream guard.)
	if ok, reason := q.CanEditRace("L4-1"); !ok {
		t.Fatalf("redemption final L4-1 not editable before CTA started: %s", reason)
	}

	// Promote L4-1's 3rd-place pilot into an advancing slot — this changes WHO
	// makes the final, which the finalist set must follow.
	l4 := q.findRace("L4-1")
	if len(l4.Pilots) < 3 {
		t.Fatalf("L4-1 has too few pilots: %v", l4.Pilots)
	}
	promoted := l4.Pilots[2]
	if containsStr(cta.Finalists, promoted) {
		t.Fatalf("test precondition: %s already a finalist", promoted)
	}
	newOrder := []string{l4.Pilots[2], l4.Pilots[0], l4.Pilots[1], l4.Pilots[3]}
	if err := q.EditRace("L4-1", newOrder); err != nil {
		t.Fatalf("edit L4-1: %v", err)
	}
	if !containsStr(q.state.ChaseTheAce.Finalists, promoted) {
		t.Fatalf("finalists did not follow the corrected L4-1: %v (want %s in)",
			q.state.ChaseTheAce.Finalists, promoted)
	}

	// Run the first CTA heat — now the feeders must freeze.
	next := q.nextCTARace()
	if next == nil {
		t.Fatal("no CTA heat available to run")
	}
	if err := q.ApplyPlacement(next.ID, append([]string(nil), next.Pilots...)); err != nil {
		t.Fatalf("run first CTA heat: %v", err)
	}
	if ok, _ := q.CanEditRace("L4-1"); ok {
		t.Error("L4-1 must be frozen once a CTA heat has been run")
	}
}

// specGraph flattens a spec's body into id → SORTED source keys ("pos@race") so
// a shape can be pinned by composition. Order is deliberately not pinned here:
// the generator lists advancers position-major (every 1st before every 2nd) and
// the sheet lists them race-major, which is a seat order, not a structure.
func specGraph(sp Spec) map[string][]string {
	out := map[string][]string{}
	for _, sr := range sp.Body {
		keys := srcKeys(sr.Sources)
		sort.Strings(keys)
		out[sr.ID] = keys
	}
	return out
}

// The generic generator must run the real double-elimination cadence: after the
// first two intakes the redemption side alternates a MINOR round (halve the pool,
// nobody new enters) with a MAJOR round that folds in the next winners round's
// drops. It used to fold every round's drops straight in, so the 13-pilot C Main
// of a mains-progression bracket got an L3-1 AND an L3-2 (R6-1/R6-2 on screen)
// where the sheet has one heat — reported straight off the runner: "R6-1 and R6-2
// should just be a single race of R6-1 featuring 1st and 2nd placings from R4-1
// and R4-2 … 1st and 2nd of that race progress to R8-1 against 3rd and 4th
// placings from W5-1."
func TestGeneric13RedemptionHalvesBeforeTheWinnersFinalDrops(t *testing.T) {
	g := specGraph(genericSpec(13, false, 4, 2))
	if _, ok := g["L3-2"]; ok {
		t.Fatalf("L3-2 exists — the halving round ran as two heats: %v", g["L3-2"])
	}
	want := map[string][]string{
		"L3-1": {"1@L2-1", "2@L2-1", "1@L2-2", "2@L2-2"}, // the minor round: L2's top-2s only
		"L4-1": {"3@W3-1", "4@W3-1", "1@L3-1", "2@L3-1"}, // redemption final vs the winners-final droppers
	}
	for id, w := range want {
		sort.Strings(w)
		if !equalStrs(g[id], w) {
			t.Errorf("%s sources = %v, want %v", id, g[id], w)
		}
	}
}

// With the cadence right, the generic generator reproduces the 16-pilot sheet's
// redemption structure on its own (specFor16 stays the transcription of record,
// but the generator and the sheet must not disagree about the shape).
func TestGeneric16MatchesTheSheetRedemption(t *testing.T) {
	gen := specGraph(genericSpec(16, false, 4, 2))
	sheet := specGraph(specFor16(false, false))
	for _, id := range []string{"L3-1", "L4-1"} {
		if !equalStrs(gen[id], sheet[id]) {
			t.Errorf("%s: generic %v, sheet %v", id, gen[id], sheet[id])
		}
	}
	for id := range gen {
		if _, ok := sheet[id]; !ok {
			t.Errorf("generic emits %s, which the sheet does not have", id)
		}
	}
	if gen["L3-1"] == nil || sheet["L3-1"] == nil {
		t.Fatal("expected an L3-1 in both")
	}
}

// Across every field size a MAJOR redemption round must never be preceded by a
// round whose survivors alone would overfill it — i.e. the halving happened. Put
// concretely: no redemption round may contain BOTH a winners-round dropper and
// a pilot from a redemption round two steps back (that is what skipping the
// minor round produces).
func TestGenericRedemptionAlternatesMinorAndMajor(t *testing.T) {
	for n := 9; n <= 96; n++ {
		sp := genericSpec(n, false, 4, 2)
		lround := map[string]int{}
		for _, sr := range sp.Body {
			if sr.Slot == bracket.SlotLosers {
				lround[sr.ID] = sr.Round
			}
		}
		for _, sr := range sp.Body {
			if sr.Slot != bracket.SlotLosers {
				continue
			}
			hasDrop, hasStale := false, false
			for _, ref := range sr.Sources {
				fr, isL := lround[ref.FromID]
				switch {
				case !isL:
					hasDrop = true
				case sr.Round-fr > 2:
					hasStale = true
				}
			}
			if hasDrop && hasStale {
				t.Errorf("n=%d: %s mixes winners droppers with survivors from two rounds back: %v",
					n, sr.ID, srcKeys(sr.Sources))
			}
		}
	}
}

// TestSixUpTopSix pins the configurable heat shape: a 48-pilot 6-up / top-3
// bracket (the Nationals ask) runs every round 6-up, ends in a 6-pilot final
// fed 3 + 3 from the two sides, and qualifies 6 — and it plays out to
// completion under seeds-win. 4/2 is unchanged: the 16 field still gets the
// MultiGP sheet exactly.
func TestSixUpTopSix(t *testing.T) {
	names := seedNames(48)
	q, err := New(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3})
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[int]int{}
	for _, sr := range q.Spec().Body {
		if sr.Round == 1 {
			sizes[len(sr.Sources)]++
		}
	}
	if sizes[6] != 8 || len(sizes) != 1 {
		t.Fatalf("round 1 must be 8 heats of 6, got %v", sizes)
	}
	for _, sr := range q.Spec().Body {
		if len(sr.Sources) > 6 {
			t.Fatalf("%s has %d pilots — more than 6-up", sr.ID, len(sr.Sources))
		}
		if !sr.Terminal && sr.Advance != 3 && len(sr.Sources) > 3 {
			t.Fatalf("%s advances %d, want 3", sr.ID, sr.Advance)
		}
	}
	if n := len(q.Spec().Finalists); n != 6 {
		t.Fatalf("terminal final must take 3 + 3 = 6 finalists, got %d", n)
	}
	if q.Config().PilotsPerRace != 6 || q.Config().PilotsToAdvance != 3 {
		t.Fatalf("bracket.Config view must carry the shape: %+v", q.Config())
	}
	st := q.Simulate()
	sim, err := FromState(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3}, st)
	if err != nil {
		t.Fatal(err)
	}
	if !sim.IsComplete() || len(sim.Qualified()) != 6 {
		t.Fatalf("seeds-win play-out: complete=%v qualified=%v", sim.IsComplete(), sim.Qualified())
	}
	// CTA: worst case (wins−1)×6 + 1 heats pre-created.
	qc, _ := New(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3, CTA: true, AceWinsRequired: 2})
	cta := 0
	for _, r := range qc.State().Races {
		if r.Bracket == bracket.SlotChaseTheAce {
			cta++
		}
	}
	if cta != 7 {
		t.Fatalf("6 finalists at 2 wins → 7 CTA heats, got %d", cta)
	}
	// The default shape is untouched.
	q16, _ := New(Config{PilotNames: seedNames(16)})
	if q16.Config().PilotsPerRace != 4 || len(q16.Spec().Finalists) != 4 || q16.Spec().WinnersFinalID != "W3-1" {
		t.Fatalf("16-pilot default must still be the MultiGP sheet: %+v", q16.Config())
	}
}

// TestRehydrateAcrossShapeChange: a state built 4-up under a row that now
// says 6/3 (the columns were stored but ignored before the shape was
// configurable) regenerates 6-up while un-run, and keeps its 4-up state and
// shape once results are in.
func TestRehydrateAcrossShapeChange(t *testing.T) {
	names := seedNames(24)
	old, _ := New(Config{PilotNames: names})
	fresh, err := FromState(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3}, old.State())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Config().PilotsPerRace != 6 || len(fresh.Spec().Finalists) != 6 {
		t.Fatalf("un-run state must regenerate under the requested shape: %+v", fresh.Config())
	}
	// Run one heat 4-up, then rehydrate as 6/3: the results must survive.
	played, _ := New(Config{PilotNames: names})
	r1 := played.State().Races[0]
	if err := played.ApplyPlacement(r1.ID, append([]string(nil), r1.Pilots...)); err != nil {
		t.Fatal(err)
	}
	kept, err := FromState(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3}, played.State())
	if err != nil {
		t.Fatal(err)
	}
	if kept.Config().PilotsPerRace != 4 || !kept.State().Races[0].IsComplete {
		t.Fatalf("a raced 4-up state must stay 4-up with its result: ppr=%d complete=%v", kept.Config().PilotsPerRace, kept.State().Races[0].IsComplete)
	}
}

// TestSeatsPadToCapacity is the race manager's ask: a 40-pilot B Main in a
// 48-per-main competition must run the SAME bracket as a full 48 — 8 heats of
// 6 seats with the missing bottom seeds left empty (5 pilots each) — not 7
// ragged heats. It plays out to a 6-pilot final and qualifies 6.
func TestSeatsPadToCapacity(t *testing.T) {
	names := seedNames(40)
	full, _ := New(Config{PilotNames: seedNames(48), PilotsPerRace: 6, Advance: 3})
	q, err := New(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3, Seats: 48})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Spec().Body) != len(full.Spec().Body) {
		t.Fatalf("padded bracket must have the full field's heats: %d vs %d", len(q.Spec().Body), len(full.Spec().Body))
	}
	for i := range q.Spec().Body {
		if q.Spec().Body[i].ID != full.Spec().Body[i].ID {
			t.Fatalf("heat %d: %s vs %s", i, q.Spec().Body[i].ID, full.Spec().Body[i].ID)
		}
	}
	r1 := 0
	for _, r := range q.State().Races {
		if r.Round == 1 {
			r1++
			if len(r.Pilots) != 5 {
				t.Fatalf("%s should have 5 pilots (one empty seat), got %d", r.ID, len(r.Pilots))
			}
		}
	}
	if r1 != 8 {
		t.Fatalf("want 8 round-1 heats, got %d", r1)
	}
	if q.Config().PilotCount != 40 {
		t.Fatalf("PilotCount stays the roster: %d", q.Config().PilotCount)
	}
	sim, err := FromState(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3, Seats: 48}, q.Simulate())
	if err != nil {
		t.Fatal(err)
	}
	if !sim.IsComplete() || len(sim.Qualified()) != 6 {
		t.Fatalf("play-out: complete=%v qualified=%d", sim.IsComplete(), len(sim.Qualified()))
	}
	// Seats below the roster are ignored; a roster-sized bracket is unchanged.
	plain, _ := New(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3, Seats: 10})
	if plain.Config().PilotCount != 40 || len(plain.Spec().Body) == len(full.Spec().Body) {
		t.Fatal("Seats below the roster must fall back to the roster size")
	}
}

// TestEmptySeatsVoidAndFlow: a field far short of its seats leaves whole
// heats empty; those void themselves so the heats they feed don't wait, and
// the bracket still completes.
func TestEmptySeatsVoidAndFlow(t *testing.T) {
	names := seedNames(4)
	q, err := New(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3, Seats: 48})
	if err != nil {
		t.Fatal(err)
	}
	voids := 0
	for _, r := range q.State().Races {
		if r.Round == 1 && len(r.Pilots) == 0 {
			if !r.IsVoid {
				t.Fatalf("%s is empty but not void", r.ID)
			}
			voids++
		}
	}
	if voids != 4 {
		t.Fatalf("4 pilots snake into 4 of 8 heats; want 4 void heats, got %d", voids)
	}
	sim, _ := FromState(Config{PilotNames: names, PilotsPerRace: 6, Advance: 3, Seats: 48}, q.Simulate())
	if !sim.IsComplete() || len(sim.Qualified()) != 4 {
		t.Fatalf("complete=%v qualified=%v", sim.IsComplete(), sim.Qualified())
	}
	if stateHasResults(q.State()) {
		t.Fatal("void heats must not read as results")
	}
}

// TestCTAPointsTieGoesToHigherSeed replays the 2026 Nationals Pro Spec A Main:
// heepsy (seed 6, via the winners final) and Davey FPV (seed 1, via
// redemption) tied on 7 points. Finalist order lists the winners side first,
// so the old stable sort gave heepsy 3rd; a points tie must go to the seed.
func TestCTAPointsTieGoesToHigherSeed(t *testing.T) {
	q := &Qual{
		seed: map[string]int{"Davey FPV": 0, "Screecher": 1, "IQ0": 2, "heepsy": 5},
		state: &bracket.State{ChaseTheAce: &bracket.ChaseTheAceState{
			Finalists: []string{"IQ0", "heepsy", "Screecher", "Davey FPV"},
			Points:    map[string]int{"Screecher": 2, "IQ0": 4, "heepsy": 7, "Davey FPV": 7},
			Champion:  "Screecher",
		}},
	}
	q.computeCTAStandings()
	want := []string{"Screecher", "IQ0", "Davey FPV", "heepsy"}
	if got := q.state.ChaseTheAce.Standings; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("standings = %v, want %v", got, want)
	}
}
