package raceformat

import (
	"encoding/json"
	"testing"
)

// f1Points is the classic top-heavy preset (descending: 1st earns the most).
var f1Points = map[int]int{1: 25, 2: 18, 3: 15, 4: 12, 5: 10, 6: 8, 7: 6, 8: 4}

// linearAsc is an ascending preset: 1st = 1 point, lowest total wins.
var linearAsc = map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7, 8: 8}

func mainsConfig(n int, mains []string, rounds int, dir string, pts map[int]int) Config {
	return Config{
		PilotCount:  n,
		PilotNames:  pilotNames(n),
		Mains:       mains,
		Rounds:      rounds,
		Direction:   dir,
		PointsTable: pts,
	}
}

func TestMainsAssignmentBalance(t *testing.T) {
	for n := 3; n <= 40; n++ {
		for _, mCount := range []int{1, 2, 3, 4} {
			if n < mCount {
				continue
			}
			mains := make([]string, mCount)
			for i := range mains {
				mains[i] = string(rune('A' + i))
			}
			mf, err := newMains(mainsConfig(n, mains, 2, mainsDescending, f1Points))
			if err != nil {
				t.Fatalf("n=%d m=%d: %v", n, mCount, err)
			}
			min, max, total := 1<<30, 0, 0
			seen := map[string]bool{}
			for _, main := range mf.state.Mains {
				sz := len(main.Pilots)
				total += sz
				if sz < min {
					min = sz
				}
				if sz > max {
					max = sz
				}
				for _, p := range main.Pilots {
					if seen[p] {
						t.Fatalf("n=%d m=%d: pilot %s in two mains", n, mCount, p)
					}
					seen[p] = true
				}
			}
			if total != n {
				t.Fatalf("n=%d m=%d: assigned %d pilots, want %d", n, mCount, total, n)
			}
			if max-min > 1 {
				t.Fatalf("n=%d m=%d: main sizes unbalanced min=%d max=%d", n, mCount, min, max)
			}
		}
	}
}

// TestMainsDescendingMath checks exact totals with the F1 preset over 2 rounds.
func TestMainsDescendingMath(t *testing.T) {
	// 4 pilots, single A main, 2 rounds.
	mf, err := newMains(mainsConfig(4, []string{"A"}, 2, mainsDescending, f1Points))
	if err != nil {
		t.Fatal(err)
	}
	// Round 1: P1,P2,P3,P4 → 25,18,15,12
	// Round 2: P2,P1,P4,P3 → P2:25 P1:18 P4:15 P3:12
	if err := mf.ApplyPlacement("A-R1", []string{"P1", "P2", "P3", "P4"}); err != nil {
		t.Fatal(err)
	}
	if err := mf.ApplyPlacement("A-R2", []string{"P2", "P1", "P4", "P3"}); err != nil {
		t.Fatal(err)
	}
	// Totals: P1=43 P2=43 P3=27 P4=27
	want := map[string]int{"P1": 43, "P2": 43, "P3": 27, "P4": 27}
	ranked := mf.rankMain(mf.state.Mains[0])
	got := map[string]int{}
	for _, s := range ranked {
		// detail begins with "<total> pts"
		var total int
		_, _ = fscanTotal(s.Detail, &total)
		got[s.Pilot] = total
	}
	for p, w := range want {
		if got[p] != w {
			t.Fatalf("pilot %s total=%d want %d", p, got[p], w)
		}
	}
	// P1 and P2 tie on 43; ties are broken by seeding order, so the higher seed
	// (P1) ranks ahead — no "tied" flag.
	if ranked[0].Pilot != "P1" || ranked[1].Pilot != "P2" {
		t.Fatalf("expected P1 then P2 by seed; got %s, %s", ranked[0].Pilot, ranked[1].Pilot)
	}
	if ranked[0].Tied || ranked[1].Tied {
		t.Fatalf("ties should be resolved by seed, not flagged: %+v %+v", ranked[0], ranked[1])
	}
	if !mf.IsComplete() {
		t.Fatal("should be complete after both rounds")
	}
	if mf.Champion() != "P1" {
		t.Fatalf("champion=%s want P1", mf.Champion())
	}
}

// TestMainsAscendingMath checks ascending scoring (lowest total wins).
func TestMainsAscendingMath(t *testing.T) {
	mf, err := newMains(mainsConfig(4, []string{"A"}, 2, mainsAscending, linearAsc))
	if err != nil {
		t.Fatal(err)
	}
	// R1: P3,P4,P1,P2 → P3:1 P4:2 P1:3 P2:4
	// R2: P3,P1,P4,P2 → P3:1 P1:2 P4:3 P2:4
	_ = mf.ApplyPlacement("A-R1", []string{"P3", "P4", "P1", "P2"})
	_ = mf.ApplyPlacement("A-R2", []string{"P3", "P1", "P4", "P2"})
	// Totals: P3=2 P1=5 P4=5 P2=8 → ascending: P3 best, P2 worst.
	ranked := mf.rankMain(mf.state.Mains[0])
	order := []string{ranked[0].Pilot, ranked[3].Pilot}
	if order[0] != "P3" || order[1] != "P2" {
		t.Fatalf("ascending order wrong: best=%s worst=%s", order[0], order[1])
	}
	if mf.Champion() != "P3" {
		t.Fatalf("ascending champion=%s want P3", mf.Champion())
	}
}

// TestMainsSeedTieBreak verifies a points tie is broken purely by seeding order.
func TestMainsSeedTieBreak(t *testing.T) {
	pts := map[int]int{1: 3, 2: 1}
	mf, _ := newMains(mainsConfig(2, []string{"A"}, 2, mainsDescending, pts))
	// R1: P1,P2 → P1:3 P2:1 ; R2: P2,P1 → P2:3 P1:1. Both total 4 → seed → P1.
	_ = mf.ApplyPlacement("A-R1", []string{"P1", "P2"})
	_ = mf.ApplyPlacement("A-R2", []string{"P2", "P1"})
	ranked := mf.rankMain(mf.state.Mains[0])
	if ranked[0].Pilot != "P1" || ranked[1].Pilot != "P2" {
		t.Fatalf("expected seed order P1 then P2; got %s, %s", ranked[0].Pilot, ranked[1].Pilot)
	}
	if ranked[0].Tied {
		t.Fatalf("seed-resolved tie should not be flagged: %+v", ranked[0])
	}
}

// TestMainsDropWorst verifies the worst N rounds are discounted from each total.
func TestMainsDropWorst(t *testing.T) {
	pts := map[int]int{1: 10, 2: 5, 3: 1}
	cfg := mainsConfig(3, []string{"A"}, 3, mainsDescending, pts)
	cfg.DropWorst = 1
	mf, err := newMains(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// P1 finishes 1,1,3 → 10,10,1 ; drop worst (1) → 20.
	// P2 finishes 2,2,1 → 5,5,10 ; drop worst (5) → 15.
	// P3 finishes 3,3,2 → 1,1,5  ; drop worst (1) → 6.
	_ = mf.ApplyPlacement("A-R1", []string{"P1", "P2", "P3"})
	_ = mf.ApplyPlacement("A-R2", []string{"P1", "P2", "P3"})
	_ = mf.ApplyPlacement("A-R3", []string{"P2", "P3", "P1"})
	want := map[string]int{"P1": 20, "P2": 15, "P3": 6}
	for _, s := range mf.rankMain(mf.state.Mains[0]) {
		var total int
		fscanTotal(s.Detail, &total)
		if total != want[s.Pilot] {
			t.Fatalf("%s counted total=%d want %d (detail %q)", s.Pilot, total, want[s.Pilot], s.Detail)
		}
	}
	if mf.Champion() != "P1" {
		t.Fatalf("champion=%s want P1", mf.Champion())
	}
}

func TestMainsCompletionGating(t *testing.T) {
	mf, _ := newMains(mainsConfig(6, []string{"A", "B"}, 2, mainsDescending, f1Points))
	if mf.IsComplete() {
		t.Fatal("should not be complete before any rounds")
	}
	if mf.Champion() != "" {
		t.Fatal("no champion before A main complete")
	}
	// Complete A main only.
	a := mf.state.Mains[0]
	for _, r := range a.Races {
		_ = mf.ApplyPlacement(r.ID, r.Pilots)
	}
	if mf.Champion() == "" {
		t.Fatal("A-main champion should be set once A main complete")
	}
	if mf.IsComplete() {
		t.Fatal("not complete until B main done too")
	}
	b := mf.state.Mains[1]
	for _, r := range b.Races {
		_ = mf.ApplyPlacement(r.ID, r.Pilots)
	}
	if !mf.IsComplete() {
		t.Fatal("should be complete after all mains")
	}
}

// TestMainsExplicitRosters checks that a manual pilot→main assignment is honoured
// (not seed-chunked) and that points ties still break by global seed order.
func TestMainsExplicitRosters(t *testing.T) {
	cfg := mainsConfig(4, []string{"A", "B"}, 2, mainsDescending, map[int]int{1: 10, 2: 5})
	// Manually put seeds 1 & 4 in A, seeds 2 & 3 in B (not the seed-chunk default).
	cfg.MainsRosters = map[string][]string{"A": {"P1", "P4"}, "B": {"P2", "P3"}}
	mf, err := newMains(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := mf.state.Mains[0]
	if len(a.Pilots) != 2 || a.Pilots[0] != "P1" || a.Pilots[1] != "P4" {
		t.Fatalf("A main roster = %v, want [P1 P4] (seed-ordered)", a.Pilots)
	}
	if mf.state.Mains[1].Pilots[0] != "P2" {
		t.Fatalf("B main roster = %v, want P2 first", mf.state.Mains[1].Pilots)
	}
	// A main, both rounds: P1 and P4 alternate 1st/2nd → both total 15. Tie must
	// break by global seed: P1 (seed 0) ahead of P4 (seed 3).
	_ = mf.ApplyPlacement("A-R1", []string{"P1", "P4"})
	_ = mf.ApplyPlacement("A-R2", []string{"P4", "P1"})
	ranked := mf.rankMain(a)
	if ranked[0].Pilot != "P1" || ranked[1].Pilot != "P4" {
		t.Fatalf("tie should break by global seed P1>P4; got %s, %s", ranked[0].Pilot, ranked[1].Pilot)
	}
}

// TestMainsRosterValidation rejects assignments that don't cover every pilot once.
func TestMainsRosterValidation(t *testing.T) {
	cfg := mainsConfig(4, []string{"A", "B"}, 2, mainsDescending, map[int]int{1: 10, 2: 5})
	cfg.MainsRosters = map[string][]string{"A": {"P1", "P2"}, "B": {"P3"}} // P4 missing
	if _, err := newMains(cfg); err == nil {
		t.Fatal("expected error for incomplete assignment")
	}
	cfg.MainsRosters = map[string][]string{"A": {"P1", "P2"}, "B": {"P2", "P3", "P4"}} // P2 twice
	if _, err := newMains(cfg); err == nil {
		t.Fatal("expected error for duplicate assignment")
	}
}

func TestMainsStateRoundTrip(t *testing.T) {
	cfg := mainsConfig(9, []string{"A", "B", "C"}, 3, mainsDescending, f1Points)
	mf, _ := newMains(cfg)
	_ = mf.ApplyPlacement("A-R1", mf.state.Mains[0].Races[0].Pilots)
	blob, _ := json.Marshal(mf.State())
	reloaded, err := FromState(KindMains, cfg, blob)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(reloaded.State())
	if string(blob) != string(again) {
		t.Fatal("mains state round-trip mismatch")
	}
}

func TestMainsValidation(t *testing.T) {
	// No points table.
	if _, err := newMains(mainsConfig(4, []string{"A"}, 2, mainsDescending, nil)); err == nil {
		t.Fatal("expected error for empty points table")
	}
	// Bad direction.
	if _, err := newMains(mainsConfig(4, []string{"A"}, 2, "sideways", f1Points)); err == nil {
		t.Fatal("expected error for bad direction")
	}
	// More mains than pilots.
	if _, err := newMains(mainsConfig(1, []string{"A", "B"}, 1, mainsDescending, f1Points)); err == nil {
		t.Fatal("expected error for more mains than pilots")
	}
	// Drop-worst must be fewer than the round count.
	dw := mainsConfig(4, []string{"A"}, 3, mainsDescending, f1Points)
	dw.DropWorst = 3
	if _, err := newMains(dw); err == nil {
		t.Fatal("expected error for drop-worst >= rounds")
	}
}

// TestMainsDivisions verifies that grouping mains into divisions gives each
// division its own champion (winner of its top main) and that there is no single
// overall champion when more than one division exists.
func TestMainsDivisions(t *testing.T) {
	cfg := mainsConfig(8, []string{"A", "B", "C", "D"}, 1, mainsDescending, f1Points)
	cfg.MainsDivisions = []MainsDivision{
		{Name: "Sport", Mains: []string{"A", "B"}},
		{Name: "Open", Mains: []string{"C", "D"}},
	}
	mf, err := newMains(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Default seed-chunk: A=[P1,P2] B=[P3,P4] C=[P5,P6] D=[P7,P8].
	for _, r := range []struct {
		id    string
		order []string
	}{
		{"A-R1", []string{"P2", "P1"}}, // P2 wins Sport's top main
		{"B-R1", []string{"P3", "P4"}},
		{"C-R1", []string{"P6", "P5"}}, // P6 wins Open's top main
		{"D-R1", []string{"P7", "P8"}},
	} {
		if err := mf.ApplyPlacement(r.id, r.order); err != nil {
			t.Fatalf("%s: %v", r.id, err)
		}
	}
	// No single overall champion with multiple divisions.
	if c := mf.Champion(); c != "" {
		t.Fatalf("overall champion should be empty with divisions, got %q", c)
	}
	ds := mf.DivisionStandings()
	if len(ds) != 2 {
		t.Fatalf("want 2 divisions, got %d", len(ds))
	}
	if ds[0].Name != "Sport" || ds[0].Champion != "P2" {
		t.Fatalf("division 0 = %q champ %q, want Sport/P2", ds[0].Name, ds[0].Champion)
	}
	if ds[1].Name != "Open" || ds[1].Champion != "P6" {
		t.Fatalf("division 1 = %q champ %q, want Open/P6", ds[1].Name, ds[1].Champion)
	}
	if !ds[0].Complete || !ds[1].Complete {
		t.Fatal("both divisions should be complete")
	}
	// Global standings follow division order: Sport (A then B) before Open (C then D).
	st := mf.Standings()
	if len(st) != 8 || st[0].Pilot != "P2" || st[4].Pilot != "P6" {
		t.Fatalf("standings order wrong: first=%s fifth=%s (len %d)", st[0].Pilot, st[4].Pilot, len(st))
	}
}

// TestMainsDivisionValidation rejects groupings that don't cover the mains exactly.
func TestMainsDivisionValidation(t *testing.T) {
	base := func() Config {
		return mainsConfig(8, []string{"A", "B", "C", "D"}, 1, mainsDescending, f1Points)
	}
	// A main left out of every division.
	c := base()
	c.MainsDivisions = []MainsDivision{{Name: "X", Mains: []string{"A", "B"}}, {Name: "Y", Mains: []string{"C"}}}
	if _, err := newMains(c); err == nil {
		t.Fatal("expected error when a main is uncovered")
	}
	// A main in two divisions.
	c = base()
	c.MainsDivisions = []MainsDivision{{Name: "X", Mains: []string{"A", "B"}}, {Name: "Y", Mains: []string{"B", "C", "D"}}}
	if _, err := newMains(c); err == nil {
		t.Fatal("expected error when a main is in two divisions")
	}
	// Unknown main referenced.
	c = base()
	c.MainsDivisions = []MainsDivision{{Name: "X", Mains: []string{"A", "B", "C", "D", "Z"}}}
	if _, err := newMains(c); err == nil {
		t.Fatal("expected error for unknown main")
	}
	// A single division of everything keeps an overall champion.
	c = base()
	c.MainsDivisions = []MainsDivision{{Name: "All", Mains: []string{"A", "B", "C", "D"}}}
	mf, err := newMains(c)
	if err != nil {
		t.Fatalf("single division should be valid: %v", err)
	}
	for _, id := range []string{"A-R1", "B-R1", "C-R1", "D-R1"} {
		r, _ := mf.findRace(id)
		_ = mf.ApplyPlacement(id, r.Pilots)
	}
	if mf.Champion() == "" {
		t.Fatal("a single division should still produce an overall champion")
	}
}

// TestMainsDivisionSkills verifies a division's optional skill tags are normalised
// (trimmed, blanks dropped) and survive a state round-trip. The skill filter itself
// is applied in the setup form; the engine just carries the tags for reference.
func TestMainsDivisionSkills(t *testing.T) {
	cfg := mainsConfig(8, []string{"A", "B", "C", "D"}, 1, mainsDescending, f1Points)
	cfg.MainsDivisions = []MainsDivision{
		{Name: "Sport", Mains: []string{"A", "B"}, Skills: []string{" Pro ", ""}},
		{Name: "Open", Mains: []string{"C", "D"}, Skills: []string{"Intermediate", "Novice"}},
	}
	mf, err := newMains(cfg)
	if err != nil {
		t.Fatal(err)
	}
	divs := mf.state.Cfg.Divisions
	if len(divs) != 2 {
		t.Fatalf("want 2 divisions, got %d", len(divs))
	}
	if len(divs[0].Skills) != 1 || divs[0].Skills[0] != "Pro" {
		t.Fatalf("Sport skills not normalised: %#v", divs[0].Skills)
	}
	if len(divs[1].Skills) != 2 || divs[1].Skills[0] != "Intermediate" || divs[1].Skills[1] != "Novice" {
		t.Fatalf("Open skills wrong: %#v", divs[1].Skills)
	}
	// Round-trip through the persisted state blob.
	blob, _ := json.Marshal(mf.State())
	reloaded, err := FromState(KindMains, cfg, blob)
	if err != nil {
		t.Fatal(err)
	}
	rs := reloaded.(*MainsFormat).state.Cfg.Divisions
	if len(rs) != 2 || rs[1].Skills[1] != "Novice" {
		t.Fatalf("skills lost on round-trip: %#v", rs)
	}
}

// TestMainScoreboard verifies the structured per-main scoreboard (used by the OBS
// overlay): row order matches rankMain, per-round point cells, the dropped flag,
// and blank cells for rounds not yet run.
func TestMainScoreboard(t *testing.T) {
	pts := map[int]int{1: 10, 2: 5, 3: 1}
	cfg := mainsConfig(3, []string{"A"}, 3, mainsDescending, pts)
	cfg.DropWorst = 1
	mf, err := newMains(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first two rounds are run; round 3 stays pending (blank cells).
	// P1 finishes 1,1 → 10,10. Drop is capped to len(rounds)-1 = 1, so P1's worst
	// of the two 10s is dropped → counted total 10.
	_ = mf.ApplyPlacement("A-R1", []string{"P1", "P2", "P3"})
	_ = mf.ApplyPlacement("A-R2", []string{"P1", "P2", "P3"})

	if _, ok := mf.MainScoreboard("Z"); ok {
		t.Fatal(`MainScoreboard("Z") should not resolve a nonexistent main`)
	}
	sb, ok := mf.MainScoreboard("") // empty selects the first main
	if !ok {
		t.Fatal(`MainScoreboard("") not ok`)
	}
	if sb.Label != "A" || sb.Rounds != 3 || len(sb.Rows) != 3 {
		t.Fatalf("scoreboard label=%q rounds=%d rows=%d want A/3/3", sb.Label, sb.Rounds, len(sb.Rows))
	}
	if sb.Rows[0].Pilot != "P1" { // finishing order, winner first
		t.Fatalf("row0=%s want P1", sb.Rows[0].Pilot)
	}
	p1 := sb.Rows[0]
	if len(p1.Cells) != 3 {
		t.Fatalf("P1 cells=%d want 3", len(p1.Cells))
	}
	if !p1.Cells[0].HasResult || !p1.Cells[1].HasResult {
		t.Fatal("P1 rounds 1+2 should have results")
	}
	if p1.Cells[0].Points != 10 || p1.Cells[1].Points != 10 {
		t.Fatalf("P1 round points=%d,%d want 10,10", p1.Cells[0].Points, p1.Cells[1].Points)
	}
	if p1.Cells[0].Position != 1 || p1.Cells[1].Position != 1 { // P1 won both run rounds
		t.Fatalf("P1 round positions=%d,%d want 1,1", p1.Cells[0].Position, p1.Cells[1].Position)
	}
	if p1.Cells[2].HasResult {
		t.Fatal("P1 round 3 should be blank (not run)")
	}
	dropped := 0
	for _, c := range p1.Cells {
		if c.Dropped {
			dropped++
		}
	}
	if dropped != 1 {
		t.Fatalf("P1 dropped cells=%d want 1", dropped)
	}
	if p1.Total != 10 {
		t.Fatalf("P1 total=%d want 10 (20 minus one dropped 10)", p1.Total)
	}
}

// fscanTotal parses the leading integer of a detail string like "43 pts (…)".
func fscanTotal(s string, out *int) (int, error) {
	n := 0
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	*out = n
	return i, nil
}
