// Package mainsbracket implements the "Mains progression" race format: a stack
// of "Double Elimination — Top 4 Advance" qualifiers (one per Main, A = top /
// championship down to the lowest letter). Pilots progress upward via "bump-up":
// the lowest main starts with the bottom seeds of the field, and the top-N
// finishers of each main move UP into the next, joining that main's resident
// pilots (positioned by their original competition seed). The A-main winner is
// the champion.
//
// Each main is a *qualbracket.Qual (reused wholesale — generation, propagation,
// chase-the-ace, SVG, ingest, finalise). The only logic here is the cross-main
// bump-up cascade and the edit/reset locking between mains.
package mainsbracket

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bob9/dd-pits/internal/stages/bracket"
	"github.com/bob9/dd-pits/internal/stages/qualbracket"
)

// idSep separates a main label from the inner qualbracket race id, e.g. "A:W1-1".
const idSep = ":"

// Config is the competition-wide setup.
type Config struct {
	PilotCount      int
	PilotNames      []string // FULL field in competition seed order, seed 1 first
	PilotsPerMain   int      // running size of each main, e.g. 16
	BumpUp          int      // top-N that bump up into the next main, e.g. 4
	CTA             bool     // master toggle: run some mains' 4-pilot final as chase-the-ace
	AceWinsRequired int      // CTA only; defaults to 2
	PhotoSeed       bool     // per-main 16-pilot seed style (false snake / true photo)
	// CTAMains selects which mains run their final as chase-the-ace (when CTA is
	// on). Empty = the A Main only (the championship final); a list of labels =
	// exactly those; ["*"] = every main.
	CTAMains []string
	// PilotsPerRace / Advance are each main's heat shape (4-up/top-2 default;
	// 6-up/top-3 for a Nationals-style 48-pilot main). Passed to every main's
	// qualbracket; the bump-up count is independent of it.
	PilotsPerRace int
	Advance       int
}

// CTAForMain reports whether the given main runs its 4-pilot final as chase-the-
// ace. The master CTA toggle gates everything; CTAMains then selects which mains
// (empty = the A Main only by default, "*" = every main).
func CTAForMain(cfg Config, label string) bool {
	if !cfg.CTA {
		return false
	}
	if len(cfg.CTAMains) == 0 {
		return label == "A"
	}
	for _, l := range cfg.CTAMains {
		if l == "*" || l == label {
			return true
		}
	}
	return false
}

// MainState is one main's persisted slice. State is nil until Built.
type MainState struct {
	Label  string         `json:"label"`  // "A".."E"
	Roster []string       `json:"roster"` // seed-ordered lineup; residents-only before Built
	Built  bool           `json:"built"`  // true once the qualbracket has been materialised
	State  *bracket.State `json:"state"`  // the built main's qualbracket state
	// Seeding is the round-1 layout the main was BUILT with ("photo" | "snake"),
	// recorded so reloading a part-raced bracket can never re-wire its round 1
	// under a different layout. "" = built before this was recorded: the main
	// follows cfg.PhotoSeed, which is how it was built.
	Seeding string `json:"seeding,omitempty"`
}

// State is the whole persisted blob.
type State struct {
	Cfg   Config       `json:"cfg"`
	Mains []*MainState `json:"mains"` // ordered A (index 0) … lowest (last)
}

// Mains is the running competition.
type Mains struct {
	cfg   Config
	state *State
	quals map[string]*qualbracket.Qual // label → interpreter (built mains only)
	seed  map[string]int               // pilot → 0-based ORIGINAL competition seed
}

// New generates a fresh competition from cfg. Only the lowest main is built
// immediately (its roster is final — nothing bumps into it); higher mains are
// built as the main below them finishes.
func New(cfg Config) (*Mains, error) {
	cfg = normaliseConfig(cfg)
	if len(cfg.PilotNames) < 2 {
		return nil, errors.New("mainsbracket: need at least 2 pilots")
	}
	if cfg.BumpUp >= cfg.PilotsPerMain {
		return nil, errors.New("mainsbracket: bump-up must be smaller than pilots-per-main")
	}
	residents, labels, err := residentRosters(cfg)
	if err != nil {
		return nil, err
	}
	st := &State{Cfg: cfg}
	for _, lbl := range labels {
		st.Mains = append(st.Mains, &MainState{Label: lbl, Roster: residents[lbl]})
	}
	m := &Mains{cfg: cfg, state: st, quals: map[string]*qualbracket.Qual{}, seed: seedMap(cfg.PilotNames)}
	// Build the lowest main (last index) up front — no incoming bump.
	low := st.Mains[len(st.Mains)-1]
	if err := m.buildMain(len(st.Mains)-1, low.Roster); err != nil {
		return nil, err
	}
	return m, nil
}

// FromState rehydrates a competition from a persisted State.
func FromState(cfg Config, st *State) (*Mains, error) {
	if st == nil {
		return nil, errors.New("mainsbracket: state is nil")
	}
	cfg = normaliseConfig(cfg)
	m := &Mains{cfg: cfg, state: st, quals: map[string]*qualbracket.Qual{}, seed: seedMap(cfg.PilotNames)}
	st.Cfg = cfg
	for _, ms := range st.Mains {
		if ms.Built && ms.State != nil {
			q, err := qualbracket.FromState(m.perMainCfg(ms, ms.Roster), ms.State)
			if err != nil {
				return nil, fmt.Errorf("mainsbracket: rehydrate main %s: %w", ms.Label, err)
			}
			m.quals[ms.Label] = q
		}
	}
	return m, nil
}

func normaliseConfig(cfg Config) Config {
	cfg.PilotNames = trimNames(cfg.PilotNames)
	cfg.PilotCount = len(cfg.PilotNames)
	if cfg.PilotsPerMain < 2 {
		cfg.PilotsPerMain = 16
	}
	if cfg.BumpUp < 1 {
		cfg.BumpUp = 4
	}
	if cfg.AceWinsRequired < 1 {
		cfg.AceWinsRequired = 2
	}
	if cfg.PilotsPerRace < 2 || cfg.PilotsPerRace > 8 {
		cfg.PilotsPerRace = qualbracket.DefaultPilotsPerRace
	}
	if cfg.Advance < 1 {
		cfg.Advance = qualbracket.DefaultAdvance
	}
	if cfg.Advance >= cfg.PilotsPerRace {
		cfg.Advance = cfg.PilotsPerRace - 1
	}
	return cfg
}

// State / Cfg accessors.
func (m *Mains) State() *State { return m.state }
func (m *Mains) Cfg() Config   { return m.cfg }
func (m *Mains) NumMains() int { return len(m.state.Mains) }

// perMainCfg builds a qualbracket config for one main from its (final) roster.
// CTA is decided per main (CTAForMain) so chase-the-ace can be limited to, e.g.,
// just the A Main championship final. The round-1 layout is the one the main
// was built with (ms.Seeding — see photoSeedFor).
func (m *Mains) perMainCfg(ms *MainState, roster []string) qualbracket.Config {
	photo := m.cfg.PhotoSeed // legacy: built before Seeding was recorded
	if ms.Seeding != "" {
		photo = ms.Seeding == "photo"
	}
	return qualbracket.Config{
		PilotNames:      append([]string(nil), roster...),
		CTA:             CTAForMain(m.cfg, ms.Label),
		AceWinsRequired: m.cfg.AceWinsRequired,
		PhotoSeed:       photo && len(roster) == 16,
		PilotsPerRace:   m.cfg.PilotsPerRace,
		Advance:         m.cfg.Advance,
		Seats:           m.seatsFor(ms.Label),
	}
}

// photoSeedFor decides the round-1 layout for a main being built now.
//
// A main that RECEIVES bump-ups always seeds snake. The photo layout ("AUFPV —
// Standard Seed") is a fixed 16-slot sheet that puts seeds 13 and 14 in the
// same heat and the top four two-per-heat (1 with 4, 2 with 3). In a main whose
// bottom four seeds are the pilots bumped up from below, that put two bump-ups
// in one heat and paired the top qualifiers off to eliminate each other — the
// 2026 Nationals Pro Spec A/B Mains (bracket 104), fixed on the night by
// switching the bracket to snake. Snake gives every round-1 heat exactly one
// top-4 seed and one bump-up. A main with nothing coming in (the lowest, or a
// single main) still honours the director's photo choice.
func (m *Mains) photoSeedFor(idx int) bool {
	return m.cfg.PhotoSeed && !m.receivesBump(idx)
}

// MainReceivesBump reports whether the main at idx (0 = A) has bump-ups coming
// in from below — every main except the lowest, when there is more than one.
func MainReceivesBump(idx, numMains, bumpUp int) bool {
	return numMains > 1 && bumpUp > 0 && idx < numMains-1
}

func (m *Mains) receivesBump(idx int) bool {
	return MainReceivesBump(idx, len(m.state.Mains), m.cfg.BumpUp)
}

// seatsFor is a main's structural size. Every main above the lowest is full by
// construction (residents + bump-up = PilotsPerMain). The lowest main is the
// only one that can be short, and it runs the SAME bracket as a full main —
// PilotsPerMain seats, the missing bottom seeds left empty — so the race
// director's B Main has exactly the A Main's heats (the ask: "leave unoccupied
// seats for the missing seed positions"), not a ragged smaller bracket.
func (m *Mains) seatsFor(label string) int {
	if m.state == nil || len(m.state.Mains) < 2 {
		return 0
	}
	if label == m.state.Mains[len(m.state.Mains)-1].Label {
		return m.cfg.PilotsPerMain
	}
	return 0
}

// buildMain materialises a main's qualbracket from its final roster.
func (m *Mains) buildMain(idx int, roster []string) error {
	ms := m.state.Mains[idx]
	ms.Seeding = "snake"
	if m.photoSeedFor(idx) {
		ms.Seeding = "photo"
	}
	q, err := qualbracket.New(m.perMainCfg(ms, roster))
	if err != nil {
		return fmt.Errorf("mainsbracket: build main %s: %w", ms.Label, err)
	}
	ms.Roster = append([]string(nil), roster...)
	ms.Built = true
	ms.State = q.State()
	m.quals[ms.Label] = q
	return nil
}

// teardownMain un-builds a main back to its residents (used when the main below
// is reset/edited so its bump set is no longer valid).
func (m *Mains) teardownMain(idx int) {
	ms := m.state.Mains[idx]
	ms.Built = false
	ms.State = nil
	ms.Seeding = "" // decided afresh when it's rebuilt
	ms.Roster = residentsOf(m.cfg, idx)
	delete(m.quals, ms.Label)
}

// ── Seeding math ────────────────────────────────────────────────────────────

// mainCount returns the number of mains for the field: every main above the
// lowest holds (PilotsPerMain−BumpUp) residents so it runs a FULL PilotsPerMain
// once the bump-up arrives, and the lowest main holds whatever is left (at most
// PilotsPerMain) — so this is the smallest M whose resident capacity covers the
// field.
func mainCount(pilotCount, pilotsPerMain, bumpUp int) int {
	if pilotCount <= pilotsPerMain {
		return 1
	}
	step := pilotsPerMain - bumpUp // residents added by each higher main
	m := 1
	cap := pilotsPerMain
	for cap < pilotCount {
		m++
		cap += step
	}
	return m
}

// mainLabels returns ["A","B",…] truncated to n (capped at 26).
func mainLabels(n int) []string {
	if n > 26 {
		n = 26
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = string(rune('A' + i))
	}
	return out
}

// residentRosters assigns residents by seed: A (index 0) gets the best residents,
// every main above the lowest gets (PilotsPerMain−BumpUp), and the LOWEST main
// (last index) takes the bottom seeds — however many are left over.
func residentRosters(cfg Config) (map[string][]string, []string, error) {
	n := len(cfg.PilotNames)
	M := mainCount(n, cfg.PilotsPerMain, cfg.BumpUp)
	if M > 26 {
		return nil, nil, errors.New("mainsbracket: too many mains (max 26)")
	}
	labels := mainLabels(M)
	out := map[string][]string{}
	if M == 1 {
		out[labels[0]] = append([]string(nil), cfg.PilotNames...)
		return out, labels, nil
	}
	idx := 0
	for i := 0; i < M; i++ {
		size := residentSize(cfg, i, M)
		if idx+size > n {
			size = n - idx
		}
		out[labels[i]] = append([]string(nil), cfg.PilotNames[idx:idx+size]...)
		idx += size
	}
	return out, labels, nil
}

// residentSize is main i's resident count: every main above the lowest gets
// (PilotsPerMain−BumpUp), and the LOWEST main absorbs the remainder.
//
// A short field has to land somewhere, and it belongs at the BOTTOM. Every main
// above the lowest races its residents PLUS the bump-up from below, so giving
// them all (PilotsPerMain−BumpUp) makes each of them run a full PilotsPerMain —
// the A Main championship included. Putting the remainder in the TOP main (what
// this used to do) starved the championship instead: a 37-pilot / 16-per-main /
// bump-4 field gave A nine residents, so the A Main decided the competition over
// 13 pilots while the C Main — the one nobody's title depends on — ran a full 16.
// It could also OVERFILL the top main, since remainder + BumpUp has no reason to
// land on PilotsPerMain.
//
// The lowest main is the only one with nothing feeding it, so it is the only one
// that can be short without unbalancing anything. mainCount's minimality makes
// the remainder strictly greater than BumpUp and at most PilotsPerMain, so the
// lowest main always has more pilots than it bumps up.
func residentSize(cfg Config, i, M int) int {
	if M == 1 {
		return len(cfg.PilotNames)
	}
	step := cfg.PilotsPerMain - cfg.BumpUp
	if i == M-1 { // lowest main takes whatever is left
		return len(cfg.PilotNames) - (M-1)*step
	}
	return step
}

// residentsOf recomputes a main's resident slice (deterministic from cfg).
func residentsOf(cfg Config, idx int) []string {
	residents, labels, err := residentRosters(cfg)
	if err != nil || idx < 0 || idx >= len(labels) {
		return nil
	}
	return residents[labels[idx]]
}

// ── Race routing + bump-up cascade ──────────────────────────────────────────

// SplitRaceID splits a prefixed id "A:W1-1" into ("A", "W1-1").
func SplitRaceID(id string) (label, inner string) {
	if i := strings.Index(id, idSep); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}

func (m *Mains) indexOf(label string) int {
	for i, ms := range m.state.Mains {
		if ms.Label == label {
			return i
		}
	}
	return -1
}

// ApplyPlacement records a result on the owning main and runs the bump-up
// cascade (when that main completes, build the main above it).
func (m *Mains) ApplyPlacement(prefixedID string, placement []string) error {
	label, inner := SplitRaceID(prefixedID)
	q := m.quals[label]
	if q == nil {
		return fmt.Errorf("mainsbracket: main %q not in play", label)
	}
	wasComplete := q.IsComplete()
	if err := q.ApplyPlacement(inner, placement); err != nil {
		return err
	}
	m.state.Mains[m.indexOf(label)].State = q.State()
	if !wasComplete && q.IsComplete() {
		m.bumpInto(m.indexOf(label))
	}
	return nil
}

// bumpInto builds the main ABOVE main idx (idx-1) from idx's top-BumpUp finishers,
// re-seeded by original competition seed.
func (m *Mains) bumpInto(idx int) {
	if idx <= 0 { // A (index 0) is the top — nobody above
		return
	}
	above := idx - 1
	if m.state.Mains[above].Built {
		return // already built (handled by the edit/reset paths)
	}
	bumped := m.bumpSet(idx)
	roster := append(append([]string(nil), residentsOf(m.cfg, above)...), bumped...)
	_ = m.buildMain(above, roster)
}

// bumpSet returns main idx's top-BumpUp finishers sorted by ORIGINAL competition
// seed (they always seed below the higher main's residents).
func (m *Mains) bumpSet(idx int) []string {
	q := m.quals[m.state.Mains[idx].Label]
	if q == nil {
		return nil
	}
	bumped := q.Qualified()
	if len(bumped) > m.cfg.BumpUp {
		bumped = bumped[:m.cfg.BumpUp]
	}
	out := append([]string(nil), bumped...)
	sort.SliceStable(out, func(a, b int) bool { return m.seed[out[a]] < m.seed[out[b]] })
	return out
}

// CanEditRace gates an edit/reset: the intra-main qualbracket rule, plus a
// conservative cross-main lock — a completed lower main can't be edited once the
// main above it has started racing (reset that one first).
func (m *Mains) CanEditRace(prefixedID string) (bool, string) {
	label, inner := SplitRaceID(prefixedID)
	q := m.quals[label]
	if q == nil {
		return false, "main not in play"
	}
	if ok, reason := q.CanEditRace(inner); !ok {
		return false, reason
	}
	idx := m.indexOf(label)
	if idx > 0 && q.IsComplete() {
		above := m.quals[m.state.Mains[idx-1].Label]
		if above != nil && hasStarted(above.State()) {
			return false, fmt.Sprintf("the %s Main has already started — reset it first", m.state.Mains[idx-1].Label)
		}
	}
	return true, ""
}

// EditRace re-runs a completed race; if it changes the bump set and the main
// above is built-but-not-started, that main is rebuilt from the new bump.
func (m *Mains) EditRace(prefixedID string, placement []string) error {
	if ok, reason := m.CanEditRace(prefixedID); !ok {
		return fmt.Errorf("mainsbracket: %s", reason)
	}
	label, inner := SplitRaceID(prefixedID)
	q := m.quals[label]
	idx := m.indexOf(label)
	var prevBump []string
	if q.IsComplete() {
		prevBump = m.bumpSet(idx)
	}
	if err := q.EditRace(inner, placement); err != nil {
		return err
	}
	m.state.Mains[idx].State = q.State()
	m.reconcileAbove(idx, prevBump)
	return nil
}

// ResetRace clears a completed race; if the main is no longer complete, the main
// above (which depended on its bump) is torn down.
func (m *Mains) ResetRace(prefixedID string) error {
	if ok, reason := m.CanEditRace(prefixedID); !ok {
		return fmt.Errorf("mainsbracket: %s", reason)
	}
	label, inner := SplitRaceID(prefixedID)
	q := m.quals[label]
	idx := m.indexOf(label)
	var prevBump []string
	if q.IsComplete() {
		prevBump = m.bumpSet(idx)
	}
	if err := q.ResetRace(inner); err != nil {
		return err
	}
	m.state.Mains[idx].State = q.State()
	m.reconcileAbove(idx, prevBump)
	return nil
}

// reconcileAbove fixes the main above idx after idx's result changed: rebuild it
// from the new bump if idx is still complete and the bump changed; tear it down
// if idx is no longer complete. Recurses (a rebuilt/torn-down higher main was, by
// the edit lock, not yet started, so the cascade is shallow).
func (m *Mains) reconcileAbove(idx int, prevBump []string) {
	if idx <= 0 {
		return
	}
	above := idx - 1
	q := m.quals[m.state.Mains[idx].Label]
	if q == nil { // idx isn't built → it never built anything above it
		return
	}
	if q.IsComplete() {
		newBump := m.bumpSet(idx)
		if m.state.Mains[above].Built && sameSlice(prevBump, newBump) {
			return // bump unchanged — nothing to do
		}
		roster := append(append([]string(nil), residentsOf(m.cfg, above)...), newBump...)
		_ = m.buildMain(above, roster)
	} else if m.state.Mains[above].Built {
		m.teardownMain(above)
	}
	m.reconcileAbove(above, nil) // a freshly (re)built/torn-down main isn't complete → shallow
}

// ── Status + ranking ────────────────────────────────────────────────────────

// IsComplete reports whether every main is built and complete.
func (m *Mains) IsComplete() bool {
	for _, ms := range m.state.Mains {
		q := m.quals[ms.Label]
		if q == nil || !q.IsComplete() {
			return false
		}
	}
	return true
}

// Champion returns the A-main winner once A is complete, else "".
func (m *Mains) Champion() string {
	if len(m.state.Mains) == 0 {
		return ""
	}
	q := m.quals[m.state.Mains[0].Label]
	if q == nil || !q.IsComplete() {
		return ""
	}
	if order := q.Qualified(); len(order) > 0 {
		return order[0]
	}
	return ""
}

// RankRow is one pilot's place in the overall competition order.
type RankRow struct {
	Pilot    string
	Main     string
	MainRank int // 1-based place within the main
}

// Ranking returns the overall competition order: Main A's full ranking first,
// then B, then C … For a complete main, the qualified top-N lead, then the
// remaining pilots by original seed. Pilots in unbuilt/incomplete mains are
// ranked by seed within their main.
func (m *Mains) Ranking() []RankRow {
	// A pilot belongs to their HIGHEST main (the one they bumped up to); they're
	// ranked there once, not in every lower main they passed through.
	home := map[string]int{}
	for i, ms := range m.state.Mains {
		for _, p := range ms.Roster {
			if _, ok := home[p]; !ok {
				home[p] = i
			}
		}
	}
	var out []RankRow
	for i, ms := range m.state.Mains {
		rank := 0
		for _, p := range m.mainRanking(ms) {
			if home[p] != i {
				continue // ranked in a higher main
			}
			rank++
			out = append(out, RankRow{Pilot: p, Main: ms.Label, MainRank: rank})
		}
	}
	return out
}

// mainRanking returns a main's full pilot order. While the main is still running
// (or unbuilt) we can only show the roster by seed. Once it's complete the
// qualifier ranks every pilot — the qualified leaders first, then the rest by how
// far they progressed in the bracket (latest elimination first), not by seed — so
// a pilot who raced deep into the redemption bracket places above a higher seed
// knocked out early.
func (m *Mains) mainRanking(ms *MainState) []string {
	roster := ms.Roster
	q := m.quals[ms.Label]
	if q == nil || !q.IsComplete() {
		out := append([]string(nil), roster...)
		sort.SliceStable(out, func(a, b int) bool { return m.seed[out[a]] < m.seed[out[b]] })
		return out
	}
	// q.Ranking() is keyed on the main's roster and returns it in finishing order.
	order := q.Ranking()
	seen := make(map[string]bool, len(roster))
	out := make([]string, 0, len(roster))
	for _, p := range order {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	// Defensive: append any roster pilot the qualifier didn't rank (shouldn't
	// happen for a complete main) so nobody silently drops out of the standings.
	for _, p := range roster {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

// ── Rendering accessors ─────────────────────────────────────────────────────

// MainView is one main's render info (A first). Qual is nil for unbuilt mains.
type MainView struct {
	Label     string
	Built     bool
	Complete  bool
	Roster    []string
	Residents int    // resident count (excludes bumped)
	BumpFrom  string // label of the main below (which bumps into this), "" for lowest
	Qual      *qualbracket.Qual
}

// MainViews returns the mains A-first for the tabbed runner.
func (m *Mains) MainViews() []MainView {
	out := make([]MainView, 0, len(m.state.Mains))
	for i, ms := range m.state.Mains {
		below := ""
		if i < len(m.state.Mains)-1 {
			below = m.state.Mains[i+1].Label
		}
		q := m.quals[ms.Label]
		complete := q != nil && q.IsComplete()
		out = append(out, MainView{
			Label:     ms.Label,
			Built:     ms.Built,
			Complete:  complete,
			Roster:    append([]string(nil), ms.Roster...),
			Residents: len(residentsOf(m.cfg, i)),
			BumpFrom:  below,
			Qual:      q,
		})
	}
	return out
}

// Qual returns a built main's interpreter (nil if unbuilt) for rendering/lookup.
func (m *Mains) Qual(label string) *qualbracket.Qual { return m.quals[label] }

// ── Simulate (for the setup preview) ────────────────────────────────────────

// Simulate plays a fresh copy of the whole competition seeds-win to completion,
// returning the resulting State so the preview shows every main filled.
func (m *Mains) Simulate() *State {
	sim, err := New(m.cfg)
	if err != nil {
		return m.state
	}
	for i := 0; i < 20000; i++ {
		label, inner, order, ok := sim.nextSeedsWinRace()
		if !ok {
			break
		}
		if err := sim.ApplyPlacement(label+idSep+inner, order); err != nil {
			break
		}
	}
	return sim.state
}

// nextSeedsWinRace picks the lowest-round runnable race across all built mains
// and returns its seeds-win finishing order (best original seed first).
func (m *Mains) nextSeedsWinRace() (label, inner string, order []string, ok bool) {
	var pickLabel string
	var pick *bracket.Race
	for _, ms := range m.state.Mains {
		q := m.quals[ms.Label]
		if q == nil {
			continue
		}
		for _, r := range q.State().Races {
			if r.IsComplete || r.IsVoid || len(r.Pilots) == 0 {
				continue
			}
			if pick == nil || r.Round < pick.Round {
				pick = r
				pickLabel = ms.Label
			}
		}
	}
	if pick == nil {
		return "", "", nil, false
	}
	order = append([]string(nil), pick.Pilots...)
	sort.SliceStable(order, func(a, b int) bool { return m.seed[order[a]] < m.seed[order[b]] })
	return pickLabel, pick.ID, order, true
}

// ── small helpers ───────────────────────────────────────────────────────────

func hasStarted(st *bracket.State) bool {
	if st == nil {
		return false
	}
	for _, r := range st.Races {
		if r.IsComplete {
			return true
		}
	}
	return false
}

func trimNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) != "" {
			out = append(out, n)
		}
	}
	return out
}

func seedMap(names []string) map[string]int {
	out := make(map[string]int, len(names))
	for i, n := range names {
		out[n] = i
	}
	return out
}

func sameSlice(a, b []string) bool {
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
