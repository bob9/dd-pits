package mainsbracket

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/stages/bracket"
)

func seedNames(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = "P" + strconv.Itoa(i+1)
	}
	return out
}

func mustNew(t *testing.T, n, perMain, bump int) *Mains {
	t.Helper()
	m, err := New(Config{PilotNames: seedNames(n), PilotsPerMain: perMain, BumpUp: bump})
	if err != nil {
		t.Fatalf("New(%d/%d/%d): %v", n, perMain, bump, err)
	}
	return m
}

// playMainSeedsWin runs every runnable race of one main, best original seed
// winning, until the main is complete. Routes through the public ApplyPlacement
// so the bump-up cascade fires.
func playMainSeedsWin(t *testing.T, m *Mains, label string) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		q := m.quals[label]
		if q == nil {
			t.Fatalf("main %s not built", label)
		}
		var pick *bracket.Race
		for _, r := range q.State().Races {
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
		sort.SliceStable(order, func(a, b int) bool { return m.seed[order[a]] < m.seed[order[b]] })
		if err := m.ApplyPlacement(label+":"+pick.ID, order); err != nil {
			t.Fatalf("apply %s:%s: %v", label, pick.ID, err)
		}
	}
}

func TestResidentSeeding(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	if got := m.NumMains(); got != 5 {
		t.Fatalf("mains = %d, want 5", got)
	}
	want := map[string][]string{
		"A": seedRange(1, 12), "B": seedRange(13, 24), "C": seedRange(25, 36),
		"D": seedRange(37, 48), "E": seedRange(49, 64),
	}
	for _, ms := range m.state.Mains {
		got := residentsOf(m.cfg, m.indexOf(ms.Label))
		if !sameSlice(got, want[ms.Label]) {
			t.Errorf("main %s residents = %v, want %v", ms.Label, got, want[ms.Label])
		}
	}
	// Only the lowest main (E) is built up front.
	if !m.state.Mains[4].Built || m.state.Mains[0].Built {
		t.Errorf("expected only the lowest main built initially: E=%v A=%v", m.state.Mains[4].Built, m.state.Mains[0].Built)
	}
}

func seedRange(lo, hi int) []string {
	out := make([]string, 0, hi-lo+1)
	for s := lo; s <= hi; s++ {
		out = append(out, "P"+strconv.Itoa(s))
	}
	return out
}

func TestBumpUpByOriginalSeedBelowResidents(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	playMainSeedsWin(t, m, "E") // E's best 4 seeds (P49-P52) win and bump to D

	d := m.state.Mains[3]
	if !d.Built {
		t.Fatal("D should be built after E completes")
	}
	if len(d.Roster) != 16 {
		t.Fatalf("D roster len = %d, want 16", len(d.Roster))
	}
	// Residents P37-P48 first, then bumped P49-P52 (sorted by original seed).
	wantResidents := seedRange(37, 48)
	wantBumped := seedRange(49, 52)
	if !sameSlice(d.Roster[:12], wantResidents) {
		t.Errorf("D residents = %v, want %v", d.Roster[:12], wantResidents)
	}
	if !sameSlice(d.Roster[12:], wantBumped) {
		t.Errorf("D bumped = %v, want %v", d.Roster[12:], wantBumped)
	}
	// Every bumped pilot seeds strictly below every resident.
	for _, b := range d.Roster[12:] {
		for _, r := range d.Roster[:12] {
			if m.seed[b] < m.seed[r] {
				t.Errorf("bumped %s (seed %d) seeded above resident %s (seed %d)", b, m.seed[b], r, m.seed[r])
			}
		}
	}
}

func TestCascadeToChampion(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	st := m.Simulate()
	sim, err := FromState(m.cfg, st)
	if err != nil {
		t.Fatalf("FromState(sim): %v", err)
	}
	if !sim.IsComplete() {
		t.Fatal("simulated competition should be complete")
	}
	if champ := sim.Champion(); champ != "P1" {
		t.Errorf("champion = %q, want P1 (seed 1 wins every heat)", champ)
	}
	rank := sim.Ranking()
	if len(rank) != 64 {
		t.Fatalf("ranking covers %d pilots, want 64", len(rank))
	}
	if rank[0].Pilot != "P1" || rank[0].Main != "A" {
		t.Errorf("rank[0] = %+v, want P1 in A", rank[0])
	}
	// A-main pilots all rank ahead of B-main pilots (A first).
	seenB := false
	for _, r := range rank {
		if r.Main == "B" {
			seenB = true
		}
		if r.Main == "A" && seenB {
			t.Errorf("A-main pilot %s ranked after a B-main pilot", r.Pilot)
		}
	}
	// No dupes.
	seen := map[string]bool{}
	for _, r := range rank {
		if seen[r.Pilot] {
			t.Errorf("duplicate pilot in ranking: %s", r.Pilot)
		}
		seen[r.Pilot] = true
	}
}

func TestEditLockWhenHigherStarted(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	playMainSeedsWin(t, m, "E") // E complete, D built
	// Run one D race so D has started.
	dq := m.quals["D"]
	var first *bracket.Race
	for _, r := range dq.State().Races {
		if !r.IsComplete && len(r.Pilots) > 0 {
			first = r
			break
		}
	}
	if first == nil {
		t.Fatal("no runnable D race")
	}
	order := append([]string(nil), first.Pilots...)
	if err := m.ApplyPlacement("D:"+first.ID, order); err != nil {
		t.Fatalf("apply D race: %v", err)
	}
	// Now editing E's terminal must be refused (D started).
	eFinal := terminalID(t, m, "E")
	if ok, reason := m.CanEditRace("E:" + eFinal); ok {
		t.Errorf("expected E edit to be locked once D started; got ok")
	} else if !strings.Contains(reason, "D Main") {
		t.Errorf("lock reason = %q, want mention of D Main", reason)
	}
}

func TestEditRebuildsHigherWhenNotStarted(t *testing.T) {
	// bump=2 so the top-2 of E's 4-pilot final actually changes when reordered
	// (with bump=4 the whole final IS the bump set, so reordering can't change it).
	m := mustNew(t, 64, 16, 2)
	playMainSeedsWin(t, m, "E") // E complete, D built (not started)
	dResidents := len(residentsOf(m.cfg, 3))
	beforeBump := append([]string(nil), m.state.Mains[3].Roster[dResidents:]...)

	// Re-run E's terminal final with a different winner among the finalists so the
	// bump set changes, and assert D is rebuilt.
	eFinal := terminalID(t, m, "E")
	q := m.quals["E"]
	fr := q.State()
	var finalRace *bracket.Race
	for _, r := range fr.Races {
		if r.ID == eFinal {
			finalRace = r
		}
	}
	if finalRace == nil || len(finalRace.Pilots) < 2 {
		t.Skip("no multi-pilot terminal final to permute")
	}
	rev := append([]string(nil), finalRace.Pilots...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if err := m.EditRace("E:"+eFinal, rev); err != nil {
		t.Fatalf("edit E final: %v", err)
	}
	afterBump := m.state.Mains[3].Roster[dResidents:]
	if sameSlice(beforeBump, afterBump) {
		t.Errorf("expected D's bumped set to change after re-ranking E final; still %v", afterBump)
	}
	if !m.state.Mains[3].Built {
		t.Error("D should still be built after the edit")
	}
}

func TestResetUnbuildsHigher(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	playMainSeedsWin(t, m, "E")
	if !m.state.Mains[3].Built {
		t.Fatal("D should be built after E completes")
	}
	eFinal := terminalID(t, m, "E")
	if err := m.ResetRace("E:" + eFinal); err != nil {
		t.Fatalf("reset E final: %v", err)
	}
	if m.state.Mains[3].Built {
		t.Error("D should be un-built after E's final is reset")
	}
	if !sameSlice(m.state.Mains[3].Roster, residentsOf(m.cfg, 3)) {
		t.Error("D roster should revert to residents after reset")
	}
}

func TestCTAForMain(t *testing.T) {
	cases := []struct {
		name  string
		cfg   Config
		label string
		want  bool
	}{
		{"master off", Config{CTA: false}, "A", false},
		{"default A only — A", Config{CTA: true}, "A", true},
		{"default A only — B", Config{CTA: true}, "B", false},
		{"explicit list — in", Config{CTA: true, CTAMains: []string{"A", "B"}}, "B", true},
		{"explicit list — out", Config{CTA: true, CTAMains: []string{"A", "B"}}, "C", false},
		{"all sentinel", Config{CTA: true, CTAMains: []string{"*"}}, "E", true},
		{"all sentinel but master off", Config{CTA: false, CTAMains: []string{"*"}}, "A", false},
	}
	for _, c := range cases {
		if got := CTAForMain(c.cfg, c.label); got != c.want {
			t.Errorf("%s: CTAForMain(%q) = %v, want %v", c.name, c.label, got, c.want)
		}
	}
}

func TestPerMainCTAApplied(t *testing.T) {
	// Default (CTA on, no CTAMains) → only the A Main runs chase-the-ace.
	m, err := New(Config{PilotNames: seedNames(64), PilotsPerMain: 16, BumpUp: 4, CTA: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sim, err := FromState(m.Cfg(), m.Simulate())
	if err != nil {
		t.Fatalf("FromState(sim): %v", err)
	}
	hasCTA := func(label string) bool {
		q := sim.quals[label]
		if q == nil {
			return false
		}
		for _, r := range q.State().Races {
			if r.Bracket == bracket.SlotChaseTheAce {
				return true
			}
		}
		return false
	}
	if !hasCTA("A") {
		t.Error("A Main should run chase-the-ace by default")
	}
	if hasCTA("E") {
		t.Error("E Main should NOT run chase-the-ace by default")
	}

	// CTAMains ["*"] → every main runs chase-the-ace.
	all, _ := New(Config{PilotNames: seedNames(64), PilotsPerMain: 16, BumpUp: 4, CTA: true, CTAMains: []string{"*"}})
	simAll, _ := FromState(all.Cfg(), all.Simulate())
	for _, lbl := range []string{"A", "C", "E"} {
		found := false
		for _, r := range simAll.quals[lbl].State().Races {
			if r.Bracket == bracket.SlotChaseTheAce {
				found = true
			}
		}
		if !found {
			t.Errorf("with CTAMains=[*], %s Main should run chase-the-ace", lbl)
		}
	}
}

func TestRaceIDNamespacing(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	q := m.quals["E"]
	for _, r := range q.State().Races {
		prefixed := "E:" + r.ID
		lbl, inner := SplitRaceID(prefixed)
		if lbl != "E" || inner != r.ID {
			t.Errorf("SplitRaceID(%q) = (%q,%q), want (E,%q)", prefixed, lbl, inner, r.ID)
		}
	}
}

func TestUnevenField(t *testing.T) {
	m := mustNew(t, 50, 16, 4) // → 4 mains; A,B,C=12 residents, D(lowest)=14
	if m.NumMains() != 4 {
		t.Fatalf("mains = %d, want 4", m.NumMains())
	}
	if got := len(residentsOf(m.cfg, 0)); got != 12 {
		t.Errorf("A residents = %d, want 12 (a full main once the bump arrives)", got)
	}
	if got := len(residentsOf(m.cfg, 3)); got != 14 {
		t.Errorf("lowest residents = %d, want 14 (the remainder lands at the bottom)", got)
	}
	// Whole thing simulates to completion without panicking.
	if sim, err := FromState(m.cfg, m.Simulate()); err != nil || !sim.IsComplete() {
		t.Fatalf("uneven field did not complete: err=%v", err)
	}
}

// A short field belongs at the BOTTOM of the stack. Every main above the lowest
// races its residents plus the bump-up from below, so they all get
// (PilotsPerMain−BumpUp) residents and run a FULL main — the A Main championship
// included. The remainder used to land in the top main, which starved the
// championship: 37 pilots at 16-per-main / bump-4 gave the A Main nine residents,
// so the title was decided over 13 pilots while the C Main ran a full 16.
func TestRemainderLandsInTheLowestMainNotTheChampionship(t *testing.T) {
	for _, tc := range []struct{ field, perMain, bump int }{
		{37, 16, 4}, {50, 16, 4}, {64, 16, 4}, {29, 16, 4}, {17, 16, 4},
		{100, 12, 3}, {43, 8, 2}, {9, 4, 1},
	} {
		m := mustNew(t, tc.field, tc.perMain, tc.bump)
		M := m.NumMains()
		step := tc.perMain - tc.bump
		total := 0
		for i := 0; i < M; i++ {
			n := len(residentsOf(m.cfg, i))
			total += n
			if i == M-1 {
				// The lowest main races its residents alone — short is fine, but it
				// must still have more pilots than it sends up, and never overfill.
				if n <= tc.bump || n > tc.perMain {
					t.Errorf("%d@%d/%d: lowest main has %d residents, want in (%d, %d]",
						tc.field, tc.perMain, tc.bump, n, tc.bump, tc.perMain)
				}
				continue
			}
			if n != step {
				t.Errorf("%d@%d/%d: main %d has %d residents, want %d so it runs a full %d",
					tc.field, tc.perMain, tc.bump, i, n, step, tc.perMain)
			}
		}
		if total != tc.field {
			t.Errorf("%d@%d/%d: residents total %d, want the whole field",
				tc.field, tc.perMain, tc.bump, total)
		}
		// And it still plays out.
		if sim, err := FromState(m.cfg, m.Simulate()); err != nil || !sim.IsComplete() {
			t.Errorf("%d@%d/%d: did not complete: err=%v", tc.field, tc.perMain, tc.bump, err)
		}
	}
}

// An exactly-divisible field is unaffected by where the remainder goes — the
// pin that says the change can't disturb a bracket already running one.
func TestExactFieldSplitIsUnchanged(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	want := map[string][]string{
		"A": seedRange(1, 12), "B": seedRange(13, 24), "C": seedRange(25, 36),
		"D": seedRange(37, 48), "E": seedRange(49, 64),
	}
	for _, ms := range m.state.Mains {
		if got := residentsOf(m.cfg, m.indexOf(ms.Label)); !sameSlice(got, want[ms.Label]) {
			t.Errorf("main %s residents = %v, want %v", ms.Label, got, want[ms.Label])
		}
	}
}

func TestPersistRoundTrip(t *testing.T) {
	m := mustNew(t, 64, 16, 4)
	playMainSeedsWin(t, m, "E")
	blob, err := json.Marshal(m.State())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var st State
	if err := json.Unmarshal(blob, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m2, err := FromState(m.cfg, &st)
	if err != nil {
		t.Fatalf("FromState: %v", err)
	}
	if m2.NumMains() != m.NumMains() || !m2.state.Mains[3].Built {
		t.Error("round-trip lost the built D main")
	}
	if !sameSlice(m.state.Mains[3].Roster, m2.state.Mains[3].Roster) {
		t.Error("round-trip changed D's roster")
	}
}

// terminalID returns a main's terminal final race id (the SlotGrandFinal heat),
// or the only race for a tiny field.
func terminalID(t *testing.T, m *Mains, label string) string {
	t.Helper()
	q := m.quals[label]
	for _, r := range q.State().Races {
		if r.Bracket == bracket.SlotGrandFinal {
			return r.ID
		}
	}
	// Fallback: last race.
	races := q.State().Races
	if len(races) > 0 {
		return races[len(races)-1].ID
	}
	t.Fatalf("no terminal race in main %s", label)
	return ""
}

// TestSixUpMains is the Nationals ask: 90 pilots, 48 per main, 6 bump up,
// 6-up / top-3 heats. B Main (43rd–90th) runs 6-up double elimination to a
// single 6-pilot final; its 6 qualifiers bump into A Main (1st–42nd + 6).
func TestSixUpMains(t *testing.T) {
	names := make([]string, 90)
	for i := range names {
		names[i] = fmt.Sprintf("P%d", i+1)
	}
	m, err := New(Config{PilotNames: names, PilotsPerMain: 48, BumpUp: 6, PilotsPerRace: 6, Advance: 3, CTA: true, CTAMains: []string{"A"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.NumMains() != 2 {
		t.Fatalf("want A + B, got %d mains", m.NumMains())
	}
	b := m.Qual("B")
	if b == nil {
		t.Fatal("B Main must be built up front")
	}
	if len(m.State().Mains[1].Roster) != 48 {
		t.Fatalf("B roster = %d, want 48", len(m.State().Mains[1].Roster))
	}
	for _, sr := range b.Spec().Body {
		if len(sr.Sources) > 6 {
			t.Fatalf("B %s is %d-up", sr.ID, len(sr.Sources))
		}
	}
	if len(b.Spec().Finalists) != 6 || b.Spec().CTA {
		t.Fatalf("B must end in a single 6-pilot final (no CTA): finalists=%d cta=%v", len(b.Spec().Finalists), b.Spec().CTA)
	}
	// Play B out seeds-win; the 6 qualifiers bump into A.
	for {
		label, inner, order, ok := m.nextSeedsWinRace()
		if !ok || label != "B" {
			break
		}
		if err := m.ApplyPlacement("B:"+inner, order); err != nil {
			t.Fatalf("B %s: %v", inner, err)
		}
	}
	if !m.Qual("B").IsComplete() {
		t.Fatal("B did not complete")
	}
	a := m.Qual("A")
	if a == nil || len(m.State().Mains[0].Roster) != 48 {
		t.Fatalf("A must be built with 42 residents + 6 bumped: built=%v roster=%d", a != nil, len(m.State().Mains[0].Roster))
	}
	if !a.Spec().CTA || len(a.Spec().Finalists) != 6 {
		t.Fatalf("A must run a 6-pilot chase-the-ace final: cta=%v finalists=%d", a.Spec().CTA, len(a.Spec().Finalists))
	}
}

// TestLowestMainPaddedToCapacity: 81 pilots / 48 per main / 6 bump → A has 42
// residents and B 39. B must run the full 48-seat bracket (8 heats of 6 seats,
// the 9 missing bottom seeds empty), while A — full by construction — is
// untouched. This is the "B Main must have exactly A Main's races" ask.
func TestLowestMainPaddedToCapacity(t *testing.T) {
	names := make([]string, 81)
	for i := range names {
		names[i] = fmt.Sprintf("P%d", i+1)
	}
	m, err := New(Config{PilotNames: names, PilotsPerMain: 48, BumpUp: 6, PilotsPerRace: 6, Advance: 3})
	if err != nil {
		t.Fatal(err)
	}
	b := m.Qual("B")
	if b == nil || len(m.State().Mains[1].Roster) != 39 {
		t.Fatalf("B roster: %d", len(m.State().Mains[1].Roster))
	}
	r1, seats := 0, 0
	for _, r := range b.State().Races {
		if r.Round == 1 {
			r1++
			seats += len(r.Pilots)
		}
	}
	if r1 != 8 || seats != 39 {
		t.Fatalf("B round 1: %d heats seating %d pilots; want 8 heats / 39", r1, seats)
	}
	if b.Config().PilotsPerRace != 6 || len(b.Spec().Finalists) != 6 {
		t.Fatalf("B shape: %+v finalists=%d", b.Config(), len(b.Spec().Finalists))
	}
	// Play B; A then runs full: 42 + 6 = 48, no padding needed.
	for {
		label, inner, order, ok := m.nextSeedsWinRace()
		if !ok || label != "B" {
			break
		}
		if err := m.ApplyPlacement("B:"+inner, order); err != nil {
			t.Fatalf("B %s: %v", inner, err)
		}
	}
	a := m.Qual("A")
	if a == nil || len(m.State().Mains[0].Roster) != 48 {
		t.Fatalf("A: built=%v roster=%d", a != nil, len(m.State().Mains[0].Roster))
	}
	ar1 := 0
	for _, r := range a.State().Races {
		if r.Round == 1 {
			ar1++
		}
	}
	if ar1 != 8 {
		t.Fatalf("A round 1 heats = %d, want 8 — B mirrors A", ar1)
	}
}

// TestBumpedMainSeedsSnakeEvenWhenPhotoChosen is the 2026 Nationals Pro Spec
// bracket (104): photo ("AUFPV — Standard Seed") seeding on a mains
// progression. The photo sheet puts seeds 13 and 14 in one heat and the top
// four two-per-heat, so the main above got two bumped-up pilots in W1-3 and
// its top qualifiers paired off. A main that RECEIVES bump-ups must seed snake:
// every round-1 heat gets exactly one top-4 seed and exactly one bump-up. The
// lowest main (nothing coming in) still honours the photo choice.
func TestBumpedMainSeedsSnakeEvenWhenPhotoChosen(t *testing.T) {
	// 40 pilots / 16 per main / bump 4 → A 12 + B 12 residents, C (lowest) 16.
	m, err := New(Config{PilotNames: seedNames(40), PilotsPerMain: 16, BumpUp: 4, PhotoSeed: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.state.Mains[2].Seeding; got != "photo" {
		t.Errorf("lowest main seeding = %q, want photo (nothing bumps into it)", got)
	}
	playMainSeedsWin(t, m, "C") // C done → B built with C's top four
	playMainSeedsWin(t, m, "B") // B done → A built with B's top four
	for _, label := range []string{"B", "A"} {
		idx := m.indexOf(label)
		ms := m.state.Mains[idx]
		if ms.Seeding != "snake" {
			t.Errorf("%s seeding = %q, want snake (it receives bump-ups)", label, ms.Seeding)
		}
		top4 := map[string]bool{}
		for _, p := range ms.Roster[:4] {
			top4[p] = true
		}
		bumped := map[string]bool{}
		for _, p := range ms.Roster[12:] {
			bumped[p] = true
		}
		for _, r := range m.quals[label].State().Races {
			if r.Round != 1 || r.Bracket != bracket.SlotWinners {
				continue
			}
			nt, nb := 0, 0
			for _, p := range r.Pilots {
				if top4[p] {
					nt++
				}
				if bumped[p] {
					nb++
				}
			}
			if nt != 1 || nb != 1 {
				t.Errorf("%s:%s %v has %d top-4 seeds and %d bump-ups, want 1 and 1", label, r.ID, r.Pilots, nt, nb)
			}
		}
	}
}

// TestRecordedSeedingSurvivesConfigChange pins that a built main reloads
// under the layout it was BUILT with, whatever the bracket's seeding setting
// says now — so changing the setting mid-event (as bracket 104 did on the
// night) can never re-wire a main's round 1 under the pilots who raced it.
// A main from before the layout was recorded ("") follows the setting.
func TestRecordedSeedingSurvivesConfigChange(t *testing.T) {
	m, err := New(Config{PilotNames: seedNames(16), PilotsPerMain: 16, BumpUp: 4, PhotoSeed: true})
	if err != nil {
		t.Fatal(err)
	}
	ms := m.state.Mains[0]
	if ms.Seeding != "photo" {
		t.Fatalf("single main seeding = %q, want photo", ms.Seeding)
	}
	if !m.perMainCfg(ms, ms.Roster).PhotoSeed {
		t.Fatal("built-photo main didn't load as photo")
	}
	cfg := m.cfg
	cfg.PhotoSeed = false
	m2, err := FromState(cfg, m.state)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.perMainCfg(ms, ms.Roster).PhotoSeed {
		t.Error("switching the bracket to snake re-seeded a main built as photo")
	}
	ms.Seeding = ""
	if m2.perMainCfg(ms, ms.Roster).PhotoSeed {
		t.Error("a legacy main (no recorded seeding) should follow the current setting")
	}
}
