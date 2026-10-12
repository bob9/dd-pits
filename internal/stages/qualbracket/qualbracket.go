package qualbracket

import (
	"errors"
	"fmt"
	"sort"

	"github.com/bob9/dd-pits/internal/stages/bracket"
)

// Config is the qualifier setup. PilotNames is the qualifying order — seed 1 first.
type Config struct {
	PilotCount      int
	PilotNames      []string
	CTA             bool // run the final as a chase-the-ace series
	AceWinsRequired int  // CTA only; defaults to 2
	// PhotoSeed selects the 16-pilot round-1 seeding: false = MultiGP snake
	// (default), true = the photo's exact slot layout. Ignored for other sizes.
	PhotoSeed bool
	// PilotsPerRace / Advance are the heat shape — 4-up/top-2 (the MultiGP
	// standard, and what a zero value means) or e.g. 6-up/top-3 for a Nationals
	// bracket. The terminal final takes Advance from each side, so it is up to
	// PilotsPerRace pilots and qualifies that many.
	PilotsPerRace int
	Advance       int
	// Seats is the STRUCTURAL field size — the bracket is generated for this
	// many seed positions and the pilots occupy the top ones, the rest are
	// vacant seats (0 or < the roster = the roster size). The lowest main of a
	// mains progression uses it to run the exact same bracket as a full main:
	// a 40-pilot B Main in a 48-per-main competition is 8 heats of 5 with the
	// bottom seed's seat empty in each, not 7 heats of 5–6 with a ragged
	// redemption side. A seat that stays empty simply advances nobody, and an
	// entirely empty heat voids itself so the heats it feeds don't wait on it.
	Seats int
}

// seats returns the structural field size.
func (c Config) seats() int {
	if c.Seats < len(c.PilotNames) {
		return len(c.PilotNames)
	}
	return c.Seats
}

// shape returns the normalised heat shape.
func (c Config) shape() (ppr, adv int) { return normaliseShape(c.PilotsPerRace, c.Advance) }

// Qual is the running qualifier bracket. Its persisted state IS a *bracket.State
// (reusing the bracket package's Race/slot types) so the existing renderer,
// runner, ingest and finalise flow consume it unchanged. The spec is regenerated
// from cfg on load, so only the state blob needs persisting.
type Qual struct {
	cfg     Config
	spec    Spec
	state   *bracket.State
	seed    map[string]int       // pilot name → 0-based qualifying seed
	sources map[string][]SlotRef // race id → its source slot refs (body + finals/CTA)
}

// New generates a fresh qualifier from cfg.
func New(cfg Config) (*Qual, error) {
	names := trimNames(cfg.PilotNames)
	if len(names) < 2 {
		return nil, errors.New("qualbracket: need at least 2 pilots")
	}
	cfg.PilotNames = names
	cfg.PilotCount = len(names)
	if cfg.AceWinsRequired < 1 {
		cfg.AceWinsRequired = 2
	}
	cfg.PilotsPerRace, cfg.Advance = cfg.shape()
	cfg.Seats = cfg.seats()
	q := &Qual{cfg: cfg, spec: buildSpec(cfg.Seats, cfg.CTA, cfg.PhotoSeed, cfg.PilotsPerRace, cfg.Advance), seed: seedMap(names)}
	q.generate()
	return q, nil
}

// FromState rehydrates a qualifier from a persisted Config + State.
func FromState(cfg Config, st *bracket.State) (*Qual, error) {
	if st == nil {
		return nil, errors.New("qualbracket: state is nil")
	}
	names := trimNames(cfg.PilotNames)
	if len(names) < 2 {
		return nil, errors.New("qualbracket: need at least 2 pilots")
	}
	cfg.PilotNames = names
	cfg.PilotCount = len(names)
	if cfg.AceWinsRequired < 1 {
		cfg.AceWinsRequired = 2
	}
	cfg.PilotsPerRace, cfg.Advance = cfg.shape()
	cfg.Seats = cfg.seats()
	spec := buildSpec(cfg.Seats, cfg.CTA, cfg.PhotoSeed, cfg.PilotsPerRace, cfg.Advance)
	if !stateMatchesSpec(st, spec) {
		// The persisted state was generated under a different heat shape —
		// every bracket built before the shape was configurable is 4/2, and a
		// bracket whose row already said 6/3 (the columns were stored but
		// ignored) was materialised 4-up. Un-run: regenerate under the shape
		// asked for. Results in: keep the state and interpret it under the
		// shape it was built with, so nothing already raced is lost.
		if !stateHasResults(st) {
			q := &Qual{cfg: cfg, spec: spec, seed: seedMap(names)}
			q.generate()
			return q, nil
		}
		// Legacy shapes, most recent first: the roster-sized bracket at this
		// shape (before seats), then the roster-sized 4/2 bracket.
		for _, try := range []struct{ n, ppr, adv int }{
			{len(names), cfg.PilotsPerRace, cfg.Advance},
			{len(names), DefaultPilotsPerRace, DefaultAdvance},
		} {
			if legacy := buildSpec(try.n, cfg.CTA, cfg.PhotoSeed, try.ppr, try.adv); stateMatchesSpec(st, legacy) {
				cfg.Seats, cfg.PilotsPerRace, cfg.Advance = try.n, try.ppr, try.adv
				spec = legacy
				break
			}
		}
	}
	q := &Qual{cfg: cfg, spec: spec, seed: seedMap(names), state: st}
	q.buildSources()
	return q, nil
}

// stateMatchesSpec reports whether a persisted state carries exactly the spec's
// body races (by id) — the test for "was this state built from this spec".
func stateMatchesSpec(st *bracket.State, spec Spec) bool {
	have := map[string]bool{}
	for _, r := range st.Races {
		have[r.ID] = true
	}
	for _, sr := range spec.Body {
		if !have[sr.ID] {
			return false
		}
	}
	body := 0
	for _, r := range st.Races {
		if r.Bracket != bracket.SlotChaseTheAce && r.ID != "FINAL" {
			body++
		}
	}
	return body == len(spec.Body)
}

// stateHasResults reports whether any race has been run (a voided empty heat
// is not a result).
func stateHasResults(st *bracket.State) bool {
	for _, r := range st.Races {
		if r.IsComplete && !r.IsVoid {
			return true
		}
	}
	return false
}

// Config returns a bracket.Config view so the qualifier state can be wrapped in a
// throwaway *bracket.Bracket for the shared SVG/grid renderer (which only reads
// state + PilotsToAdvance/FinalTarget for expected-winner counts).
func (q *Qual) Config() bracket.Config {
	return bracket.Config{
		PilotCount:      q.cfg.PilotCount,
		PilotNames:      append([]string(nil), q.cfg.PilotNames...),
		EliminationType: bracket.EliminationDouble,
		PilotsPerRace:   q.cfg.PilotsPerRace,
		PilotsToAdvance: q.cfg.Advance,
		FinalsFormat:    bracket.FinalsGrandFinal,
		AceWinsRequired: q.cfg.AceWinsRequired,
	}
}

// State returns the live state.
func (q *Qual) State() *bracket.State { return q.state }

// Cfg returns the qualifier's own config (for RestoreState).
func (q *Qual) Cfg() Config { return q.cfg }

// ExpectedWinners reports how many pilots advance/qualify out of a race.
func (q *Qual) ExpectedWinners(r *bracket.Race) int { return q.expectedWinners(r) }

// Spec returns the resolved bracket spec (for predicted-slot labels).
func (q *Qual) Spec() Spec { return q.spec }

// Sources returns each race's source slot refs (for predicted-slot labels in the
// runner grid). Round-1 races map to seed refs (FromID empty).
func (q *Qual) Sources() map[string][]SlotRef { return q.sources }

// buildSources indexes every race's source refs (body race sources, plus the
// terminal final / CTA heats' finalist refs).
func (q *Qual) buildSources() {
	q.sources = map[string][]SlotRef{}
	for _, sr := range q.spec.Body {
		q.sources[sr.ID] = sr.Sources
	}
	// Terminal final (non-CTA) and every CTA heat are fed by the finalist refs.
	if !q.specHasTerminalInBody() {
		if q.spec.CTA {
			for _, r := range q.state.Races {
				if r.Bracket == bracket.SlotChaseTheAce {
					q.sources[r.ID] = q.spec.Finalists
				}
			}
		} else {
			q.sources["FINAL"] = q.spec.Finalists
		}
	}
}

func (q *Qual) specHasTerminalInBody() bool {
	for _, sr := range q.spec.Body {
		if sr.Terminal {
			return true
		}
	}
	return false
}

// generate materialises the initial state from the spec.
func (q *Qual) generate() {
	st := &bracket.State{}
	for _, sr := range q.spec.Body {
		r := &bracket.Race{ID: sr.ID, Round: sr.Round, Bracket: sr.Slot, FinalTarget: sr.FinalTarget}
		st.Races = append(st.Races, r)
	}
	rounds := q.spec.FinalRound
	if !q.specHasTerminalInBody() {
		if q.spec.CTA {
			max := maxCTARaces(q.finalistCount(), q.cfg.AceWinsRequired)
			for n := 1; n <= max; n++ {
				st.Races = append(st.Races, &bracket.Race{
					ID: fmt.Sprintf("CTA-%d", n), Round: q.spec.FinalRound, Bracket: bracket.SlotChaseTheAce})
			}
			st.ChaseTheAce = &bracket.ChaseTheAceState{
				Wins: map[string]int{}, Points: map[string]int{}, RequiredWins: q.cfg.AceWinsRequired}
		} else {
			st.Races = append(st.Races, &bracket.Race{
				ID: "FINAL", Round: q.spec.FinalRound, Bracket: bracket.SlotGrandFinal,
				FinalTarget: q.finalistCount()})
		}
	}
	for _, r := range st.Races {
		if r.Round > rounds {
			rounds = r.Round
		}
	}
	st.Rounds = rounds
	q.state = st
	q.buildSources()
	q.propagate()
}

// finalistCount is how many pilots reach the terminal final (≤ pilots per race).
func (q *Qual) finalistCount() int {
	if n := len(q.spec.Finalists); n > 0 && n < q.cfg.PilotsPerRace {
		return n
	}
	return minI(q.cfg.PilotsPerRace, q.cfg.PilotCount)
}

func (q *Qual) findRace(id string) *bracket.Race {
	for _, r := range q.state.Races {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// finishOrder returns a completed race's finishing order (best first). Full
// placements carry the exact order in Pilots; partial entries rank the recorded
// winners then the losers by seed (matching the seeds-win convention).
func (q *Qual) finishOrder(r *bracket.Race) []string {
	if !r.IsComplete {
		return nil
	}
	if r.FullPlacement {
		return r.Pilots
	}
	order := append([]string(nil), r.Winners...)
	losers := append([]string(nil), r.Losers...)
	sort.SliceStable(losers, func(i, j int) bool { return q.seedOf(losers[i]) < q.seedOf(losers[j]) })
	return append(order, losers...)
}

func (q *Qual) seedOf(name string) int {
	if s, ok := q.seed[name]; ok {
		return s
	}
	return 1 << 30
}

// slotState is what a source slot resolves to.
type slotState int

const (
	slotPending slotState = iota // the source race hasn't run yet
	slotReady                    // a pilot
	slotVacant                   // settled, and nobody: an unfilled seed seat, a
	// finishing position a short heat never produced, or a voided empty heat
)

// resolve returns the pilot occupying a source slot and its state.
func (q *Qual) resolve(ref SlotRef) (string, slotState) {
	if ref.FromID == "" { // Round-1 seed entry
		if ref.Position >= 1 && ref.Position <= len(q.cfg.PilotNames) {
			return q.cfg.PilotNames[ref.Position-1], slotReady
		}
		return "", slotVacant
	}
	src := q.findRace(ref.FromID)
	if src == nil {
		return "", slotVacant
	}
	if src.IsVoid && !src.IsComplete {
		return "", slotVacant // an empty heat that will never run
	}
	if !src.IsComplete {
		return "", slotPending
	}
	order := q.finishOrder(src)
	if ref.Position >= 1 && ref.Position <= len(order) {
		return order[ref.Position-1], slotReady
	}
	return "", slotVacant
}

// propagate (re)derives the pilot lineup of every incomplete race from its source
// refs. A race whose sources all resolve is filled; otherwise it's cleared. This
// is idempotent, so editing an upstream result cascades correctly. Repeats until
// the state stops changing. The chase-the-ace finalists are kept in sync with
// the winners/redemption finals until the series actually starts (see below).
func (q *Qual) propagate() {
	for pass := 0; pass < len(q.state.Races)+2; pass++ {
		changed := false
		for _, r := range q.state.Races {
			if r.IsComplete {
				continue
			}
			refs, ok := q.sources[r.ID]
			if !ok {
				continue
			}
			filled := make([]string, 0, len(refs))
			pending := false
			for _, ref := range refs {
				p, st := q.resolve(ref)
				if st == slotPending {
					pending = true
					break
				}
				if st == slotReady {
					filled = append(filled, p)
				}
			}
			var want []string
			if !pending {
				want = filled
			}
			if !sameSlice(r.Pilots, want) {
				r.Pilots = want
				changed = true
			}
			// A body heat whose every seat has settled EMPTY can never be run:
			// void it so the heats it feeds see its slots as vacant instead of
			// waiting on it forever (a short field padded to its main's seats).
			// It un-voids if an upstream edit sends someone its way.
			if r.Bracket != bracket.SlotChaseTheAce {
				void := !pending && len(refs) > 0 && len(want) == 0
				if r.IsVoid != void {
					r.IsVoid = void
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	// Keep the chase-the-ace finalists in sync with the winners/redemption
	// finals UNTIL the series has actually started. The set resolves
	// automatically the moment both feeders complete, but a race director may
	// still need to correct a feeder result before any CTA heat is flown — so we
	// re-derive it on every propagate until the first CTA heat runs, then freeze
	// it (from then on editing a feeder is blocked because a completed CTA heat
	// carrying the finalists is a run downstream race). resolvedFinalists returns
	// nil while either feeder is incomplete, which clears a stale set when a
	// feeder is un-run.
	if q.spec.CTA && q.state.ChaseTheAce != nil && !q.ctaStarted() {
		q.state.ChaseTheAce.Finalists = q.resolvedFinalists()
	}
}

// ctaStarted reports whether the chase-the-ace series has begun — at least one
// finals heat has been run (or the champion is already decided). Until then the
// finalist set is provisional and the winners/redemption finals stay editable.
func (q *Qual) ctaStarted() bool {
	cta := q.state.ChaseTheAce
	if cta == nil {
		return false
	}
	if cta.IsComplete {
		return true
	}
	for _, r := range q.state.Races {
		if r.Bracket == bracket.SlotChaseTheAce && r.IsComplete {
			return true
		}
	}
	return false
}

// resolvedFinalists returns the terminal finalists once no finalist ref is
// still pending, else nil. A vacant slot (a feeder that ran short) is skipped.
func (q *Qual) resolvedFinalists() []string {
	out := make([]string, 0, len(q.spec.Finalists))
	for _, ref := range q.spec.Finalists {
		p, st := q.resolve(ref)
		switch st {
		case slotPending:
			return nil
		case slotReady:
			out = append(out, p)
		}
	}
	return out
}

// expectedWinners mirrors bracket.expectedWinnersCount for the qualifier's races.
func (q *Qual) expectedWinners(r *bracket.Race) int {
	if r.FinalTarget > 0 {
		t := r.FinalTarget
		if len(r.Pilots) > 0 && t > len(r.Pilots) {
			t = len(r.Pilots)
		}
		return t
	}
	if r.Bracket == bracket.SlotChaseTheAce {
		return 1
	}
	if len(r.Pilots) == 0 {
		return q.cfg.Advance
	}
	return minI(q.cfg.Advance, len(r.Pilots))
}

// ApplyPlacement records a race result (full finishing order) and advances state.
func (q *Qual) ApplyPlacement(raceID string, placement []string) error {
	r := q.findRace(raceID)
	if r == nil {
		return fmt.Errorf("qualbracket: race %q not found", raceID)
	}
	if r.IsComplete {
		return fmt.Errorf("qualbracket: race %q already complete", raceID)
	}
	if r.Bracket == bracket.SlotChaseTheAce {
		if err := q.checkCTAOrder(r); err != nil {
			return err
		}
	}
	if err := q.record(r, placement); err != nil {
		return err
	}
	if r.Bracket == bracket.SlotChaseTheAce {
		q.advanceCTA(r)
	}
	q.propagate()
	return nil
}

// record validates and writes a finishing order onto a race.
func (q *Qual) record(r *bracket.Race, placement []string) error {
	expected := q.expectedWinners(r)
	if len(placement) < expected {
		return fmt.Errorf("qualbracket: race %q needs at least %d positions, got %d", r.ID, expected, len(placement))
	}
	pset := map[string]bool{}
	for _, p := range r.Pilots {
		pset[p] = true
	}
	full := len(placement) == len(r.Pilots)
	seen := map[string]bool{}
	for _, p := range placement {
		if !pset[p] {
			return fmt.Errorf("qualbracket: %q not in race %q", p, r.ID)
		}
		if seen[p] {
			return fmt.Errorf("qualbracket: duplicate %q", p)
		}
		seen[p] = true
	}
	if full {
		r.Pilots = append([]string(nil), placement...)
	}
	if expected > len(placement) {
		expected = len(placement)
	}
	r.Winners = append([]string(nil), placement[:expected]...)
	wset := map[string]bool{}
	for _, w := range r.Winners {
		wset[w] = true
	}
	r.Losers = r.Losers[:0]
	for _, p := range r.Pilots {
		if !wset[p] {
			r.Losers = append(r.Losers, p)
		}
	}
	r.FullPlacement = full
	r.IsComplete = true
	return nil
}

// checkCTAOrder enforces sequential chase-the-ace heats.
func (q *Qual) checkCTAOrder(r *bracket.Race) error {
	if r.IsVoid {
		return fmt.Errorf("qualbracket: chase-the-ace heat %q is void", r.ID)
	}
	cta := q.state.ChaseTheAce
	if cta != nil && cta.IsComplete {
		return fmt.Errorf("qualbracket: chase-the-ace already complete (champion %q)", cta.Champion)
	}
	if next := q.nextCTARace(); next != nil && next.ID != r.ID {
		return fmt.Errorf("qualbracket: chase-the-ace heats run in order; %q is next, not %q", next.ID, r.ID)
	}
	return nil
}

func (q *Qual) nextCTARace() *bracket.Race {
	var ctas []*bracket.Race
	for _, r := range q.state.Races {
		if r.Bracket == bracket.SlotChaseTheAce {
			ctas = append(ctas, r)
		}
	}
	sort.Slice(ctas, func(i, j int) bool { return ctaIndex(ctas[i].ID) < ctaIndex(ctas[j].ID) })
	for _, r := range ctas {
		if !r.IsVoid && !r.IsComplete {
			return r
		}
	}
	return nil
}

// advanceCTA accumulates wins/points and crowns the CTA champion (mirrors the
// bracket engine's chase-the-ace scoring).
func (q *Qual) advanceCTA(done *bracket.Race) {
	cta := q.state.ChaseTheAce
	if cta == nil || len(done.Winners) == 0 {
		return
	}
	if done.FullPlacement {
		for i, p := range done.Pilots {
			cta.Points[p] += i + 1
		}
	} else {
		cta.Points[done.Winners[0]]++
	}
	winner := done.Winners[0]
	cta.Wins[winner]++
	if cta.Wins[winner] >= cta.RequiredWins {
		cta.Champion = winner
		cta.IsComplete = true
		q.computeCTAStandings()
		q.removeUnrunCTA()
	}
}

func (q *Qual) computeCTAStandings() {
	cta := q.state.ChaseTheAce
	others := make([]string, 0, len(cta.Finalists))
	for _, p := range cta.Finalists {
		if p != cta.Champion {
			others = append(others, p)
		}
	}
	// Fewest points first; a points tie goes to the higher qualifying seed
	// (the rule points-mains uses). Finalist order lists the winners side
	// first, so a stable sort alone handed every tie to the winners-bracket
	// pilot regardless of seed or the heats themselves.
	sort.SliceStable(others, func(i, j int) bool {
		pi, pj := cta.Points[others[i]], cta.Points[others[j]]
		if pi != pj {
			return pi < pj
		}
		return q.seedOf(others[i]) < q.seedOf(others[j])
	})
	cta.Standings = append([]string{cta.Champion}, others...)
}

func (q *Qual) removeUnrunCTA() {
	kept := q.state.Races[:0]
	for _, r := range q.state.Races {
		if r.Bracket == bracket.SlotChaseTheAce && !r.IsComplete {
			continue
		}
		kept = append(kept, r)
	}
	q.state.Races = kept
}

// CanEditRace reports whether a completed race may still be re-run.
func (q *Qual) CanEditRace(raceID string) (bool, string) {
	r := q.findRace(raceID)
	if r == nil {
		return false, "race not found"
	}
	if !r.IsComplete {
		return true, ""
	}
	placed := map[string]bool{}
	for _, p := range r.Winners {
		placed[p] = true
	}
	for _, p := range r.Losers {
		placed[p] = true
	}
	for _, other := range q.state.Races {
		if other.ID == raceID || other.Round <= r.Round || !other.IsComplete {
			continue
		}
		for _, p := range other.Pilots {
			if placed[p] {
				return false, fmt.Sprintf("downstream race %s has already been run", other.ID)
			}
		}
	}
	// No separate chase-the-ace finalists guard: once the series has started, the
	// completed CTA heats (which carry the finalist roster) are caught by the
	// downstream-IsComplete loop above, so a feeder is correctly frozen. Before
	// the series starts the finalists are provisional (propagate keeps them in
	// sync with the feeders), so a feeder may be corrected freely.
	return true, ""
}

// EditRace re-runs a completed race with a new placement, re-deriving downstream.
func (q *Qual) EditRace(raceID string, placement []string) error {
	if ok, reason := q.CanEditRace(raceID); !ok {
		return fmt.Errorf("qualbracket: cannot edit %q: %s", raceID, reason)
	}
	r := q.findRace(raceID)
	if r == nil {
		return fmt.Errorf("qualbracket: race %q not found", raceID)
	}
	if !r.IsComplete {
		return q.ApplyPlacement(raceID, placement)
	}
	r.Winners = nil
	r.Losers = nil
	r.IsComplete = false
	r.FullPlacement = false
	return q.ApplyPlacement(raceID, placement)
}

// ResetRace un-runs a completed race: clears its winners/losers/complete state
// and re-propagates, so downstream heats that depended on this result are
// recomputed (their pilots cleared until it's re-run). Refused when a downstream
// heat that received this result has already been run. No-op if not yet run.
func (q *Qual) ResetRace(raceID string) error {
	if ok, reason := q.CanEditRace(raceID); !ok {
		return fmt.Errorf("qualbracket: cannot reset %q: %s", raceID, reason)
	}
	r := q.findRace(raceID)
	if r == nil {
		return fmt.Errorf("qualbracket: race %q not found", raceID)
	}
	if !r.IsComplete {
		return nil
	}
	r.Winners = nil
	r.Losers = nil
	r.IsComplete = false
	r.FullPlacement = false
	q.propagate()
	return nil
}

// IsComplete reports whether the terminal final (or CTA series) is decided.
func (q *Qual) IsComplete() bool {
	if q.spec.CTA && !q.specHasTerminalInBody() {
		return q.state.ChaseTheAce != nil && q.state.ChaseTheAce.IsComplete
	}
	t := q.terminalRace()
	return t != nil && t.IsComplete
}

// terminalRace returns the FINAL heat (or the tiny-field single race).
func (q *Qual) terminalRace() *bracket.Race {
	if q.specHasTerminalInBody() {
		for _, sr := range q.spec.Body {
			if sr.Terminal {
				return q.findRace(sr.ID)
			}
		}
	}
	return q.findRace("FINAL")
}

// Qualified returns the ranked top-N pilots who advance ("continue into the next
// bracket"). Empty until the final is decided.
func (q *Qual) Qualified() []string {
	if q.spec.CTA && !q.specHasTerminalInBody() {
		if cta := q.state.ChaseTheAce; cta != nil && cta.IsComplete {
			return append([]string(nil), cta.Standings...)
		}
		return nil
	}
	t := q.terminalRace()
	if t == nil || !t.IsComplete {
		return nil
	}
	return q.finishOrder(t)
}

// Ranking returns every pilot in competition finishing order: the qualified
// finalists first (in their final placing), then the remaining pilots ordered by
// how far they progressed — latest elimination first (a pilot who survived to a
// deeper round finishes higher), breaking ties by finishing position in their
// final heat, then by seed. Unlike a plain seed sort, this rewards a pilot who
// raced deep into the redemption bracket over a higher seed knocked out early.
func (q *Qual) Ranking() []string {
	out := make([]string, 0, len(q.cfg.PilotNames))
	seen := make(map[string]bool, len(q.cfg.PilotNames))
	for _, p := range q.Qualified() {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}

	// For everyone else, find the deepest (highest-round) completed heat they
	// appeared in — for a finished qualifier that's the heat that eliminated them —
	// and their finishing position within it.
	type exit struct {
		name  string
		round int // deepest completed-heat round; -1 if they never raced one
		pos   int // 0-based finishing position in that heat; large if unknown
	}
	rest := make([]exit, 0, len(q.cfg.PilotNames))
	for _, name := range q.cfg.PilotNames {
		if seen[name] {
			continue
		}
		e := exit{name: name, round: -1, pos: 1 << 30}
		for _, r := range q.state.Races {
			if !r.IsComplete {
				continue
			}
			idx := indexOf(q.finishOrder(r), name)
			if idx >= 0 && r.Round > e.round {
				e.round = r.Round
				e.pos = idx
			}
		}
		rest = append(rest, e)
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].round != rest[j].round {
			return rest[i].round > rest[j].round // deeper round ranks higher
		}
		if rest[i].pos != rest[j].pos {
			return rest[i].pos < rest[j].pos // better finish in the final heat
		}
		return q.seedOf(rest[i].name) < q.seedOf(rest[j].name)
	})
	for _, e := range rest {
		out = append(out, e.name)
	}
	return out
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// Simulate plays a fresh copy of this qualifier to completion with the
// best-seeded pilots winning every heat, returning the resulting state. Used by
// the structure preview so empty downstream races show the seed flow.
func (q *Qual) Simulate() *bracket.State {
	sim, err := New(q.cfg)
	if err != nil {
		return q.state
	}
	for i := 0; i < 4000; i++ {
		var pick *bracket.Race
		for _, r := range sim.state.Races {
			if r.IsComplete || r.IsVoid || len(r.Pilots) == 0 {
				continue
			}
			if pick == nil || r.Round < pick.Round {
				pick = r
			}
		}
		if pick == nil {
			break
		}
		order := append([]string(nil), pick.Pilots...)
		sort.SliceStable(order, func(a, b int) bool { return sim.seedOf(order[a]) < sim.seedOf(order[b]) })
		if err := sim.ApplyPlacement(pick.ID, order); err != nil {
			break
		}
	}
	return sim.state
}

// ---- helpers ----

func trimNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

func seedMap(names []string) map[string]int {
	m := make(map[string]int, len(names))
	for i, n := range names {
		m[n] = i
	}
	return m
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

func ctaIndex(id string) int {
	n := 0
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' {
			fmt.Sscanf(id[i+1:], "%d", &n)
			break
		}
	}
	return n
}

// maxCTARaces is the worst-case number of chase-the-ace heats: every finalist
// but one wins requiredWins-1 heats, then one decides it.
func maxCTARaces(finalistCount, requiredWins int) int {
	if finalistCount < 1 {
		finalistCount = 1
	}
	if requiredWins < 1 {
		requiredWins = 1
	}
	return (requiredWins-1)*finalistCount + 1
}
