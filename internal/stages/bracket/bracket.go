// Package bracket implements MultiGP-style drone-racing tournament brackets.
//
// Ported from the drone-racing Nuxt project (composables/useBracket.ts). Supports
// single + double elimination, traditional grand-final or chase-the-ace finals,
// 2-4 pilots per race, configurable advancement.
//
// The bracket is generated up-front from a config; SelectWinners drives progression
// race-by-race. Winners and losers are placed into subsequent races using snake
// seeding (round 1) and least-filled balanced placement (later rounds).
package bracket

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EliminationType determines whether losers get a second chance.
type EliminationType string

const (
	EliminationSingle EliminationType = "single"
	EliminationDouble EliminationType = "double"
)

// FinalsFormat picks the championship round style.
type FinalsFormat string

const (
	FinalsGrandFinal  FinalsFormat = "double-elimination" // legacy name kept for parity with TS source
	FinalsChaseTheAce FinalsFormat = "chase-the-ace"
)

// BracketSlot identifies which sub-bracket a race belongs to.
type BracketSlot string

const (
	SlotWinners     BracketSlot = "winners"
	SlotLosers      BracketSlot = "losers"
	SlotGrandFinal  BracketSlot = "grand-final"
	SlotChaseTheAce BracketSlot = "chase-the-ace"
	// SlotFiller marks a non-competitive "filler heat" for knocked-out pilots.
	// The bracket engine never generates, advances, scores, or reads these —
	// they're carried in State.FillerRaces purely as data, populated by handler
	// code so eliminated pilots keep getting heats. See race_bracket_filler.go.
	SlotFiller BracketSlot = "filler"
)

// Config is the tournament setup. PilotNames is the qualifying order — seed 1 first.
type Config struct {
	PilotCount      int
	PilotNames      []string
	EliminationType EliminationType
	PilotsPerRace   int
	PilotsToAdvance int
	FinalsFormat    FinalsFormat
	AceWinsRequired int // chase-the-ace only; defaults to 2
}

// Race is a single heat in the bracket.
type Race struct {
	ID         string      `json:"id"`
	Round      int         `json:"round"`
	Pilots     []string    `json:"pilots"`
	Winners    []string    `json:"winners"`
	Losers     []string    `json:"losers"`
	IsComplete bool        `json:"isComplete"`
	Bracket    BracketSlot `json:"bracket"`
	// FinalTarget is set for "consolidation" races added when a side of
	// the bracket can't naturally converge to the finals-format target
	// (e.g. CTA needs 2 pilots from each side, but 3-ppr/2-advance ends
	// with 4 W-bracket winners). When > 0 it overrides the normal
	// per-race advance count and the race may exceed PilotsPerRace.
	FinalTarget int `json:"finalTarget,omitempty"`
	// FullPlacement is true when SelectFullPlacement recorded the
	// complete finish order. When false, only Winners (the top N
	// advancers) were ranked — the remaining pilots in race.Pilots
	// are eliminated but their relative finishing positions weren't
	// captured, so the UI should leave their position labels blank
	// rather than guessing.
	FullPlacement bool `json:"fullPlacement,omitempty"`
	// IsVoid is set on chase-the-ace finals races that were pre-created
	// up front (so all CTA heats can be exported to the timing system in
	// advance) but turned out not to be needed because a champion was
	// crowned earlier. A voided race is never runnable, is excluded from
	// completion checks, and refuses results. Its ID stays stable so any
	// already-exported timing heat that maps to it is harmless.
	IsVoid bool `json:"isVoid,omitempty"`
	// FromWinnersRound is set on the losers-bracket rounds that absorb a
	// winners round's droppers ("major" rounds). 0 means the round takes no
	// new droppers — either a "minor" (halving) round that just reduces the
	// redemption pool, or a bracket generated before this field existed, in
	// which case the runtime falls back to the old winners-round-N → losers-
	// round-N mapping. See losersRoundPlan.
	FromWinnersRound int `json:"fromWinnersRound,omitempty"`
}

// ChaseTheAceState tracks running CTA-finals progress.
type ChaseTheAceState struct {
	Wins map[string]int `json:"wins"`
	// Points accumulates per-pilot finishing-position totals across every
	// CTA race: 1pt for 1st, 2pt for 2nd, 3pt for 3rd, 4pt for 4th, etc.
	// Lower total = better non-champion finish.
	Points     map[string]int `json:"points"`
	Finalists  []string       `json:"finalists"`
	IsComplete bool           `json:"isComplete"`
	Champion   string         `json:"champion"`
	// Standings is the final non-champion ranking sorted by Points (asc)
	// once a Champion has been crowned. Set by computeChaseTheAceStandings.
	Standings []string `json:"standings,omitempty"`
	// TieBreakNeeded lists groups of pilots tied on points in the final
	// standings; each group needs its own race-off to determine final
	// placement. Empty when no ties remain.
	TieBreakNeeded [][]string `json:"tieBreakNeeded,omitempty"`
	RequiredWins   int        `json:"requiredWins"`
}

// State is the full mutable tournament state.
type State struct {
	Races        []*Race           `json:"races"`
	Rounds       int               `json:"rounds"`
	CurrentRound int               `json:"currentRound"`
	ChaseTheAce  *ChaseTheAceState `json:"chaseTheAce,omitempty"`
	// FillerRaces holds non-competitive "filler heats" (SlotFiller) for pilots
	// who have been knocked out of the bracket, so they keep flying. These are
	// generated and replaced wholesale by handler code (regenerateFillerHeats)
	// — the bracket engine never touches them. Their IDs ("F2-1", …) live in a
	// separate namespace from competitive race IDs.
	FillerRaces []*Race `json:"fillerRaces,omitempty"`
}

// Bracket couples a Config with its mutable State.
type Bracket struct {
	cfg     Config
	state   *State
	seedMap map[string]int // pilot name → absolute qualifying seed (0-indexed)
}

// New creates a Bracket from cfg and generates the initial race structure.
// Returns an error if the config is invalid.
func New(cfg Config) (*Bracket, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	b := &Bracket{cfg: cfg, state: &State{}, seedMap: buildSeedMap(cfg.PilotNames)}
	b.generate()
	return b, nil
}

// FromState rebuilds a Bracket from a persisted Config + State (e.g. loaded from
// the database). No regeneration happens — the state is used as-is.
func FromState(cfg Config, state *State) (*Bracket, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	if state == nil {
		return nil, errors.New("bracket: state is nil")
	}
	return &Bracket{cfg: cfg, state: state, seedMap: buildSeedMap(cfg.PilotNames)}, nil
}

// buildSeedMap derives the pilot-name → absolute-seed lookup from the
// qualifying order in cfg.PilotNames. Seed 0 = pilot 1 (the top qualifier).
func buildSeedMap(pilots []string) map[string]int {
	m := make(map[string]int, len(pilots))
	for i, p := range pilots {
		m[p] = i
	}
	return m
}

// seedOf returns the absolute qualifying seed for a pilot name, or a large
// sentinel if the pilot isn't known (defensive — keeps unknown pilots last
// in placement-order sorting rather than panicking).
func (b *Bracket) seedOf(pilot string) int {
	if s, ok := b.seedMap[pilot]; ok {
		return s
	}
	return 1 << 30
}

// Config returns a copy of the config.
func (b *Bracket) Config() Config { return b.cfg }

// State returns the live state. Mutating the returned value mutates the bracket.
func (b *Bracket) State() *State { return b.state }

func validateConfig(cfg *Config) error {
	if cfg.PilotCount < 2 {
		return errors.New("bracket: need at least 2 pilots")
	}
	if cfg.PilotsPerRace < 2 || cfg.PilotsPerRace > 8 {
		return errors.New("bracket: pilotsPerRace must be between 2 and 8")
	}
	if cfg.PilotsToAdvance < 1 || cfg.PilotsToAdvance >= cfg.PilotsPerRace {
		return errors.New("bracket: pilotsToAdvance must be ≥1 and < pilotsPerRace")
	}
	// Trim/validate pilot names
	names := make([]string, 0, len(cfg.PilotNames))
	for _, n := range cfg.PilotNames {
		t := strings.TrimSpace(n)
		if t != "" {
			names = append(names, t)
		}
	}
	if len(names) < 2 {
		return errors.New("bracket: need at least 2 non-empty pilot names")
	}
	if len(names) > cfg.PilotCount {
		names = names[:cfg.PilotCount]
	}
	cfg.PilotCount = len(names)
	cfg.PilotNames = names
	if cfg.EliminationType == "" {
		cfg.EliminationType = EliminationDouble
	}
	if cfg.FinalsFormat == "" {
		cfg.FinalsFormat = FinalsGrandFinal
	}
	if cfg.FinalsFormat == FinalsChaseTheAce && cfg.AceWinsRequired < 1 {
		cfg.AceWinsRequired = 2
	}
	return nil
}

// ---------- structural calculations ----------

type roundInfo struct {
	round     int
	pilots    int
	races     int
	advancing int
	losers    int
}

// calculateBracketStructure walks the winners bracket until pilots ≤ target. Each
// entry is one round with the real pilot count, race count, advancing and losers.
func calculateBracketStructure(totalPilots, pilotsPerRace, pilotsToAdvance, targetFinalists int) []roundInfo {
	if targetFinalists < 1 {
		targetFinalists = pilotsToAdvance
	}
	out := []roundInfo{}
	current := totalPilots
	round := 0
	for current > targetFinalists {
		round++
		races := ceilDiv(current, pilotsPerRace)
		advancing := races * pilotsToAdvance
		if advancing > current {
			advancing = current
		}
		out = append(out, roundInfo{round, current, races, advancing, current - advancing})
		if advancing >= current {
			break // no progress
		}
		current = advancing
		if round > 20 {
			break
		}
	}
	return out
}

// calculateRoundsNeeded returns how many winners-bracket rounds it takes to
// reduce startingPilots down to targetPilots, stopping when the bracket math
// can't make further progress (next round would have the same pilot count as
// the current one — e.g. a 2-race round of 2-pilot heats with advance=2 just
// passes everyone forward).
func calculateRoundsNeeded(startingPilots, targetPilots, pilotsPerRace, pilotsToAdvance int) int {
	if startingPilots <= targetPilots {
		return 0
	}
	rounds := 0
	current := startingPilots
	for current > targetPilots {
		races := ceilDiv(current, pilotsPerRace)
		next := races * pilotsToAdvance
		if next >= current {
			// No further reduction possible at this ratio (e.g. 2-pilot
			// races advancing 2). The bracket can't keep shrinking — stop
			// here and let the finals format (CTA / grand-final) handle
			// the remaining pilots.
			break
		}
		rounds++
		current = next
		if rounds > 20 {
			break
		}
	}
	return rounds
}

// losersRoundSpec is one round of the redemption (losers) bracket as planned by
// losersRoundPlan.
type losersRoundSpec struct {
	counter int // L{counter}
	races   int
	// fromWinnersRound is the winners round whose droppers enter here, or 0 for
	// a "minor" round that only halves the redemption pool.
	fromWinnersRound int
	// finalTarget > 0 marks the redemption decider — a single race run for
	// exactly that many finals spots (see the consolidation note below).
	finalTarget int
}

// losersRoundPlan lays out the redemption bracket in the real double-elimination
// cadence: after the first two rounds it alternates a MINOR round (halves the
// redemption pool, nobody new enters) with a MAJOR round (the survivors plus the
// next winners round's droppers).
//
// It used to feed winners round N straight into losers round N, with no minor
// rounds at all, which let the redemption pool run ahead of the field it was
// merging with and produced lopsided round sizes — a 48-pilot / 6-up / top-3
// bracket got a 3-heat redemption round 3 (12 survivors + 6 fresh droppers)
// where the correct sheet halves 12 → 2 heats first and takes the droppers in
// round 4. The 16-pilot MultiGP sheet hand-transcribed in the qualbracket
// package (L1×2, L2×2, L3×1, L4×1) is the reference this reproduces.
//
// wbExtraLosers is the number of pilots dropped by the winners consolidation
// race (see buildWinnersBracket) and wbExtraRound the round it sits in; they are
// folded in as one more winners-round drop. losersTarget is how many pilots the
// redemption side owes the finals format.
func losersRoundPlan(totalPilots, pilotsPerRace, pilotsToAdvance, winnersRounds, wbExtraLosers, wbExtraRound, losersTarget int) []losersRoundSpec {
	if pilotsPerRace < 1 || pilotsToAdvance < 1 {
		return nil
	}
	winners := calculateBracketStructure(totalPilots, pilotsPerRace, pilotsToAdvance, pilotsToAdvance)

	// drops[i] = the droppers entering from one winners round, in running order.
	type drop struct{ round, count int }
	var drops []drop
	for wr := 0; wr < winnersRounds; wr++ {
		count := 0
		if wr < len(winners) {
			count = winners[wr].losers
		}
		drops = append(drops, drop{round: wr + 1, count: count})
	}
	if wbExtraLosers > 0 {
		merged := false
		for i := range drops {
			if drops[i].round == wbExtraRound {
				drops[i].count += wbExtraLosers
				merged = true
				break
			}
		}
		if !merged {
			drops = append(drops, drop{round: wbExtraRound, count: wbExtraLosers})
		}
	}

	var plan []losersRoundSpec
	counter, pool, majors := 0, 0, 0
	for _, d := range drops {
		// Minor (halving) round: from the third intake on, the pool is reduced
		// before the next winners round's droppers join it.
		if majors >= 2 && pool > pilotsToAdvance {
			races := ceilDiv(pool, pilotsPerRace)
			if next := races * pilotsToAdvance; next < pool {
				counter++
				plan = append(plan, losersRoundSpec{counter: counter, races: races})
				pool = next
			}
		}
		total := pool + d.count
		if total == 0 {
			continue
		}
		counter++
		races := ceilDiv(total, pilotsPerRace)
		plan = append(plan, losersRoundSpec{counter: counter, races: races, fromWinnersRound: d.round})
		pool = minInt(total, races*pilotsToAdvance)
		majors++
	}

	// Tail: keep halving until the redemption side is down to one race's worth.
	for pool > pilotsToAdvance {
		races := ceilDiv(pool, pilotsPerRace)
		next := races * pilotsToAdvance
		if next >= pool {
			break
		}
		counter++
		plan = append(plan, losersRoundSpec{counter: counter, races: races})
		pool = next
		if counter > (winnersRounds+1)*4 {
			break
		}
	}

	// Redemption decider: same idea as the winners side — if the bracket stops
	// with more pilots than the finals format can take from this side, add a
	// single race so those pilots actually race for the spot instead of getting
	// silently truncated when finalists are picked.
	if losersTarget > 0 && pool > losersTarget && pool <= 8 {
		counter++
		plan = append(plan, losersRoundSpec{counter: counter, races: 1, finalTarget: losersTarget})
	}
	return plan
}

// BalancedSeeding snake-seeds pilots across the minimum number of races needed
// at pilotsPerRace, evening out the per-race counts (max−min ≤ 1) and spreading
// top seeds into different races. Exported for reuse by filler-heat generation;
// see createBalancedSeeding.
func BalancedSeeding(pilots []string, pilotsPerRace int) [][]string {
	return createBalancedSeeding(pilots, pilotsPerRace)
}

// createBalancedSeeding snake-seeds pilots across races. For 13 pilots / 4 races
// → 3+3+3+4 (max−min ≤ 1) with top seeds in different races.
func createBalancedSeeding(pilots []string, pilotsPerRace int) [][]string {
	total := len(pilots)
	races := ceilDiv(total, pilotsPerRace)
	if races == 0 {
		return nil
	}
	out := make([][]string, races)
	for i := range out {
		out[i] = []string{}
	}
	for i := 0; i < total; i++ {
		row := i / races
		pos := i % races
		target := pos
		if row%2 == 1 {
			target = races - 1 - pos
		}
		out[target] = append(out[target], pilots[i])
	}
	// Drop any empty trailing races (shouldn't happen with snake but parity with TS).
	cleaned := out[:0]
	for _, r := range out {
		if len(r) > 0 {
			cleaned = append(cleaned, r)
		}
	}
	return cleaned
}

// calculateInterleavedHeatOrder returns a run order that splits the race list
// in half and interleaves: H1, H(half+1), H2, H(half+2), … Gives pilots more
// rest between back-to-back heats.
func calculateInterleavedHeatOrder(numHeats int) []int {
	if numHeats <= 1 {
		return []int{1}
	}
	half := (numHeats + 1) / 2
	first := make([]int, half)
	for i := 0; i < half; i++ {
		first[i] = i + 1
	}
	second := make([]int, numHeats-half)
	for i := 0; i < numHeats-half; i++ {
		second[i] = half + 1 + i
	}
	out := make([]int, 0, numHeats)
	for i := 0; i < half; i++ {
		out = append(out, first[i])
		if i < len(second) {
			out = append(out, second[i])
		}
	}
	return out
}

// ---------- generation ----------

func (b *Bracket) generate() {
	b.state.Races = nil
	b.state.Rounds = 0
	b.state.CurrentRound = 1
	b.state.ChaseTheAce = nil

	cfg := b.cfg
	seeded := append([]string(nil), cfg.PilotNames...)

	if cfg.EliminationType == EliminationSingle {
		b.generateSingleElim(seeded)
		return
	}

	if cfg.FinalsFormat == FinalsChaseTheAce {
		b.generateChaseTheAce(seeded)
		return
	}

	b.generateDoubleElim(seeded)
}

func (b *Bracket) generateSingleElim(seeded []string) {
	cfg := b.cfg

	var roundsToFinalists int
	if cfg.FinalsFormat == FinalsChaseTheAce {
		targetFinalists := b.ctaFinalistCount()
		roundsToFinalists = calculateRoundsNeeded(cfg.PilotCount, targetFinalists, cfg.PilotsPerRace, cfg.PilotsToAdvance)
		if roundsToFinalists < 1 && cfg.PilotCount > targetFinalists {
			// A field that needs no reduction round by the round maths (e.g. 9
			// pilots, 4-up, 3 advancing) still can't put everyone in the final —
			// keep one qualifying round, or the CTA heats land in round 1
			// alongside it and nothing ever feeds them.
			roundsToFinalists = 1
		}
		b.state.Rounds = roundsToFinalists + 1 // CTA race
	} else {
		target := cfg.PilotsPerRace
		if cfg.PilotCount < target {
			target = cfg.PilotCount
		}
		roundsToFinalists = calculateRoundsNeeded(cfg.PilotCount, target, cfg.PilotsPerRace, cfg.PilotsToAdvance)
		final := 0
		if cfg.PilotCount > 1 {
			final = 1
		}
		b.state.Rounds = roundsToFinalists + final
	}

	// Skip W1 entirely when every pilot is already a chase-the-ace finalist.
	ctaSkipsWinners := cfg.FinalsFormat == FinalsChaseTheAce && cfg.PilotCount <= b.ctaFinalistCount()

	if !ctaSkipsWinners {
		groups := createBalancedSeeding(seeded, cfg.PilotsPerRace)
		order := calculateInterleavedHeatOrder(len(groups))
		for i, g := range groups {
			b.state.Races = append(b.state.Races, &Race{
				ID:      fmt.Sprintf("race-%d", order[i]),
				Round:   1,
				Pilots:  append([]string(nil), g...),
				Bracket: SlotWinners,
			})
		}
	}

	pilotsRemaining := cfg.PilotCount
	for round := 2; round <= b.state.Rounds; round++ {
		prev := countRacesInRound(b.state.Races, round-1)
		newCount := prev * cfg.PilotsToAdvance
		if newCount >= pilotsRemaining && round < b.state.Rounds {
			newCount = pilotsRemaining * 3 / 4
			if newCount < 1 {
				newCount = 1
			}
		}
		pilotsRemaining = newCount

		if round == b.state.Rounds {
			// For chase-the-ace the CTA finals races are created in one
			// batch after the loop (so the finalist-count + required-wins
			// max-race math is centralised). For grand-final, add the
			// single championship race here.
			if cfg.FinalsFormat != FinalsChaseTheAce {
				b.state.Races = append(b.state.Races, &Race{
					ID:      fmt.Sprintf("R%d-1", round),
					Round:   round,
					Bracket: SlotWinners,
				})
			}
			continue
		}

		if pilotsRemaining <= cfg.PilotsToAdvance {
			continue // direct advancement, no race
		}
		races := ceilDiv(pilotsRemaining, cfg.PilotsPerRace)
		for i := 0; i < races; i++ {
			b.state.Races = append(b.state.Races, &Race{
				ID:      fmt.Sprintf("R%d-%d", round, i+1),
				Round:   round,
				Bracket: SlotWinners,
			})
		}
	}

	if cfg.FinalsFormat == FinalsChaseTheAce {
		// The round feeding the final must collectively advance exactly the
		// finalist count — with a wider-than-4 final, two semis advancing the
		// ordinary 2 each can't fill six seats.
		b.setSemiFinalTargets(b.ctaFinalistCount())
		b.initChaseTheAceState(seeded)
		// Pre-create the full set of CTA finals races up front. When every
		// pilot is already a finalist (≤4-pilot field that skips the winners
		// bracket), seed each race with all of them now.
		var ctaPilots []string
		if ctaSkipsWinners {
			ctaPilots = append([]string(nil), seeded...)
		}
		if !hasChaseTheAceRace(b.state.Races) {
			b.appendChaseTheAceRaces(b.state.Rounds, ctaPilots)
		}
		b.populateChaseTheAceFinalists()
	}
}

func (b *Bracket) generateChaseTheAce(seeded []string) {
	cfg := b.cfg
	winnersTarget, losersTarget := finalSideTargets(cfg)
	winnersRounds := calculateRoundsNeeded(cfg.PilotCount, winnersTarget, cfg.PilotsPerRace, cfg.PilotsToAdvance)

	wbExtraLosers, wbExtraRound := b.buildWinnersBracket(seeded, winnersRounds)
	b.buildLosersBracket(winnersRounds, wbExtraLosers, wbExtraRound)

	// A side whose last race advances the ordinary PilotsToAdvance can't fill
	// its share of a wider-than-4 chase-the-ace final (a 6-up final wants 3 a
	// side, but a winners final advancing 2 supplies 2). Widen that last race so
	// it races for every finals spot its side owns.
	b.widenFinalRace(SlotWinners, winnersTarget)
	b.widenFinalRace(SlotLosers, losersTarget)

	finalRound := b.maxBuiltRound() + 1
	b.initChaseTheAceState(seeded)
	b.appendChaseTheAceRaces(finalRound, nil)
	b.state.Rounds = finalRound
}

// appendChaseTheAceRaces pre-creates the maximum possible CTA finals races
// (CTA-1 … CTA-max) all in finalRound. `pilots` is set on every race when the
// finalists are already known at generation time (e.g. ≤4-pilot single-elim
// where everyone is a finalist); pass nil to leave them empty until
// populateChaseTheAceFinalists fills them. Safe to call only once — it assumes
// no CTA races exist yet.
func (b *Bracket) appendChaseTheAceRaces(finalRound int, pilots []string) {
	requiredWins := b.cfg.AceWinsRequired
	if requiredWins < 1 {
		requiredWins = 2
	}
	finalistCount := b.ctaFinalistCount()
	max := maxChaseTheAceRaces(finalistCount, requiredWins)
	for n := 1; n <= max; n++ {
		var p []string
		if len(pilots) > 0 {
			p = append([]string(nil), pilots...)
		}
		b.state.Races = append(b.state.Races, &Race{
			ID:      fmt.Sprintf("CTA-%d", n),
			Round:   finalRound,
			Pilots:  p,
			Bracket: SlotChaseTheAce,
		})
	}
}

func (b *Bracket) generateDoubleElim(seeded []string) {
	cfg := b.cfg
	winnersFinalists := minInt(cfg.PilotsToAdvance, cfg.PilotCount/2)
	if winnersFinalists < 1 {
		winnersFinalists = 1
	}
	winnersRounds := calculateRoundsNeeded(cfg.PilotCount, winnersFinalists, cfg.PilotsPerRace, cfg.PilotsToAdvance)

	wbExtraLosers, wbExtraRound := b.buildWinnersBracket(seeded, winnersRounds)
	b.buildLosersBracket(winnersRounds, wbExtraLosers, wbExtraRound)

	// Place the GF after whatever's actually been built — buildWinnersBracket
	// and buildLosersBracket may have added consolidation rounds beyond the
	// theoretical winnersRounds / losersRounds counts.
	gfRound := b.maxBuiltRound() + 1
	b.state.Rounds = gfRound + 1
	b.state.Races = append(b.state.Races,
		&Race{ID: "GF-1", Round: gfRound, Bracket: SlotGrandFinal},
		&Race{ID: "GF-2", Round: gfRound + 1, Bracket: SlotGrandFinal},
	)
}

// maxBuiltRound returns the highest Round value currently in state.Races,
// used to compute where to place finals after the W/L brackets (including
// any consolidation rounds) are built.
func (b *Bracket) maxBuiltRound() int {
	maxR := 0
	for _, r := range b.state.Races {
		if r.Round > maxR {
			maxR = r.Round
		}
	}
	return maxR
}

// buildWinnersBracket builds the winners side and returns the number of losers
// produced by the winners consolidation/final round (the race that narrows the
// last 2+ winners down to the finals target) plus the round that race sits in.
// Those losers drop into the losers bracket and must be counted when sizing it —
// see buildLosersBracket. (0, 0) when no consolidation race was needed.
func (b *Bracket) buildWinnersBracket(seeded []string, winnersRounds int) (extraLosers, extraRound int) {
	cfg := b.cfg
	target, _ := finalSideTargets(cfg)
	groups := createBalancedSeeding(seeded, cfg.PilotsPerRace)
	for i, g := range groups {
		b.state.Races = append(b.state.Races, &Race{
			ID:      fmt.Sprintf("W1-%d", i+1),
			Round:   1,
			Pilots:  append([]string(nil), g...),
			Bracket: SlotWinners,
		})
	}
	lastRound := 1
	for round := 2; round <= winnersRounds; round++ {
		prev := countRacesInBracketRound(b.state.Races, round-1, SlotWinners)
		pilots := prev * cfg.PilotsToAdvance
		if pilots <= cfg.PilotsToAdvance {
			continue
		}
		races := ceilDiv(pilots, cfg.PilotsPerRace)
		if races >= prev {
			break
		}
		for i := 0; i < races; i++ {
			b.state.Races = append(b.state.Races, &Race{
				ID:      fmt.Sprintf("W%d-%d", round, i+1),
				Round:   round,
				Bracket: SlotWinners,
			})
		}
		lastRound = round
	}

	// Consolidation race: if the loop stopped with more winners than the
	// finals format can take from this side (CTA wants 2, GF wants 1), add a
	// single explicit decider so the race director picks the actual
	// advancers rather than having the library silently truncate.
	prev := countRacesInBracketRound(b.state.Races, lastRound, SlotWinners)
	remaining := prev * cfg.PilotsToAdvance
	if remaining > target && remaining <= 8 {
		b.state.Races = append(b.state.Races, &Race{
			ID:          fmt.Sprintf("W%d-1", lastRound+1),
			Round:       lastRound + 1,
			Bracket:     SlotWinners,
			FinalTarget: target,
		})
		// This decider races `remaining` pilots for `target` finals spots; the
		// rest are eliminated into the losers bracket's tail.
		return remaining - target, lastRound + 1
	}
	return 0, 0
}

// finalSideTargets is how many pilots each bracket side (winners and losers in
// double-elim) should converge to so the finals format can run cleanly: the
// chase-the-ace finalists split across the two sides (4 finalists → 2 + 2, 6 →
// 3 + 3, 3 → 1 + 2), 1 + 1 for a grand final. The split matches
// deriveChaseTheAceFinalists exactly, so no pilot races a decider for a spot
// that is then truncated away. Single-elim always converges to 1.
func finalSideTargets(cfg Config) (winners, losers int) {
	if cfg.EliminationType == EliminationSingle || cfg.FinalsFormat != FinalsChaseTheAce {
		return 1, 1
	}
	want := ctaFinalists(cfg)
	winners = want / 2
	if winners < 1 {
		winners = 1
	}
	losers = want - winners
	if losers < 1 {
		losers = 1
	}
	return winners, losers
}

// buildLosersBracket builds the redemption side from losersRoundPlan. wbExtraLosers
// is the number of pilots dropped by the winners consolidation/final race and
// wbExtraRound the round that race sits in; they enter as one more winners-round
// intake.
func (b *Bracket) buildLosersBracket(winnersRounds, wbExtraLosers, wbExtraRound int) {
	cfg := b.cfg
	_, losersTarget := finalSideTargets(cfg)
	plan := losersRoundPlan(cfg.PilotCount, cfg.PilotsPerRace, cfg.PilotsToAdvance,
		winnersRounds, wbExtraLosers, wbExtraRound, losersTarget)
	// L_n races sit at round counter+1 so they interleave with W rounds — L1 runs
	// alongside W2, L2 alongside W3, etc. (parity with the original drone-racing UI).
	for _, spec := range plan {
		for i := 0; i < spec.races; i++ {
			b.state.Races = append(b.state.Races, &Race{
				ID:               fmt.Sprintf("L%d-%d", spec.counter, i+1),
				Round:            spec.counter + 1,
				Bracket:          SlotLosers,
				FinalTarget:      spec.finalTarget,
				FromWinnersRound: spec.fromWinnersRound,
			})
		}
	}
}

// setSemiFinalTargets spreads `want` finals spots across the single-elimination
// round that feeds the final, so those heats race for exactly the number of
// seats the final holds. A round that already advances exactly `want` (the
// classic two semis × two advancing = four finalists) is left alone, as is a
// round with more heats than seats — there the finalists are taken off the top
// as before.
func (b *Bracket) setSemiFinalTargets(want int) {
	semiRound := b.state.Rounds - 1
	if semiRound < 1 || want < 1 {
		return
	}
	var semis []*Race
	for _, r := range b.state.Races {
		if r.Round == semiRound && r.Bracket == SlotWinners {
			semis = append(semis, r)
		}
	}
	n := len(semis)
	if n == 0 || n > want || n*b.cfg.PilotsToAdvance == want {
		return
	}
	base, extra := want/n, want%n
	for i, r := range semis {
		if r.FinalTarget > 0 {
			continue
		}
		t := base
		if i < extra {
			t++
		}
		r.FinalTarget = t
	}
}

// widenFinalRace makes the single last race of a bracket side advance `target`
// pilots instead of the config's PilotsToAdvance. It is a no-op when the side
// ends in several races (they collectively already supply enough), when the
// side already ends in an explicit consolidation race (FinalTarget set), or
// when target isn't larger than the normal advance count — so the classic
// 4-finalist chase-the-ace is untouched.
func (b *Bracket) widenFinalRace(slot BracketSlot, target int) {
	if target <= b.cfg.PilotsToAdvance {
		return
	}
	maxRound := 0
	for _, r := range b.state.Races {
		if r.Bracket == slot && r.Round > maxRound {
			maxRound = r.Round
		}
	}
	if maxRound == 0 {
		return
	}
	var last []*Race
	for _, r := range b.state.Races {
		if r.Bracket == slot && r.Round == maxRound {
			last = append(last, r)
		}
	}
	if len(last) != 1 || last[0].FinalTarget > 0 {
		return
	}
	last[0].FinalTarget = target
}

func (b *Bracket) initChaseTheAceState(seeded []string) {
	wins := map[string]int{}
	points := map[string]int{}
	for _, p := range seeded {
		wins[p] = 0
		points[p] = 0
	}
	requiredWins := b.cfg.AceWinsRequired
	if requiredWins < 1 {
		requiredWins = 2
	}
	b.state.ChaseTheAce = &ChaseTheAceState{
		Wins:         wins,
		Points:       points,
		Finalists:    []string{},
		RequiredWins: requiredWins,
	}
}

// ---------- advancement ----------

// SelectWinners records the result of a race and advances pilots to the next races.
// `winners` must be in placement order; the rest of race.Pilots become losers.
func (b *Bracket) SelectWinners(raceID string, winners []string) error {
	race := b.findRace(raceID)
	if race == nil {
		return fmt.Errorf("bracket: race %q not found", raceID)
	}
	if race.IsComplete {
		return fmt.Errorf("bracket: race %q already complete", raceID)
	}
	if err := b.checkChaseTheAceEntry(race); err != nil {
		return err
	}

	expected := b.expectedWinnersCount(race)
	if len(winners) > expected {
		return fmt.Errorf("bracket: too many winners (%d > %d) for race %q", len(winners), expected, raceID)
	}

	// Validate every winner is actually in the race
	pilotSet := stringSet(race.Pilots)
	for _, w := range winners {
		if _, ok := pilotSet[w]; !ok {
			return fmt.Errorf("bracket: winner %q not in race %q", w, raceID)
		}
	}

	race.Winners = append([]string(nil), winners...)
	race.Losers = race.Losers[:0]
	winSet := stringSet(winners)
	for _, p := range race.Pilots {
		if _, ok := winSet[p]; !ok {
			race.Losers = append(race.Losers, p)
		}
	}
	race.IsComplete = len(winners) == expected
	// Partial entry: the user only ranked the top N. Losers' positions
	// weren't captured, so make sure FullPlacement is cleared (e.g. if
	// the race was previously edited as full and the new edit is partial).
	race.FullPlacement = false

	if race.IsComplete {
		b.advance(race)
	}
	return nil
}

// SelectFullPlacement records the complete finishing order for a race. The
// first ExpectedWinnersCount entries become winners (advance to the next
// round); the rest stay in race.Pilots in placement order so the loser
// finish order is preserved for display ("3rd: P9, 4th: P12 …").
//
// `placement` must contain every pilot currently in the race, with no
// duplicates and no extras.
func (b *Bracket) SelectFullPlacement(raceID string, placement []string) error {
	race := b.findRace(raceID)
	if race == nil {
		return fmt.Errorf("bracket: race %q not found", raceID)
	}
	if race.IsComplete {
		return fmt.Errorf("bracket: race %q already complete", raceID)
	}
	if err := b.checkChaseTheAceEntry(race); err != nil {
		return err
	}
	if len(placement) != len(race.Pilots) {
		return fmt.Errorf("bracket: placement has %d pilots, race has %d", len(placement), len(race.Pilots))
	}
	pilotSet := stringSet(race.Pilots)
	seen := map[string]bool{}
	for _, p := range placement {
		if _, ok := pilotSet[p]; !ok {
			return fmt.Errorf("bracket: %q not in race %q", p, raceID)
		}
		if seen[p] {
			return fmt.Errorf("bracket: duplicate %q in placement", p)
		}
		seen[p] = true
	}
	// Reorder the race's pilots so the displayed order matches the finish.
	race.Pilots = append([]string(nil), placement...)
	expected := b.expectedWinnersCount(race)
	if expected > len(placement) {
		expected = len(placement)
	}
	winners := placement[:expected]
	race.Winners = append([]string(nil), winners...)
	race.Losers = race.Losers[:0]
	winSet := stringSet(winners)
	for _, p := range race.Pilots {
		if _, ok := winSet[p]; !ok {
			race.Losers = append(race.Losers, p)
		}
	}
	race.IsComplete = len(winners) == expected
	// Mark as full placement BEFORE advance() runs — advanceChaseTheAce
	// reads FullPlacement to decide whether to award position points to
	// every pilot (1pt 1st, 2pt 2nd, …) or just 1pt to the winner.
	race.FullPlacement = true
	if race.IsComplete {
		b.advance(race)
	}
	return nil
}

// CanEditRace reports whether a completed race's result can still be revised.
// Edits are refused once any downstream race that received this race's pilots
// has itself been run (or once the chase-the-ace finalists have been locked
// in), because re-running this race would invalidate work already recorded.
//
// Returns ok=true with reason="" for incomplete races (no edit needed —
// submit normally) and for completed races whose downstream is still clean.
func (b *Bracket) CanEditRace(raceID string) (ok bool, reason string) {
	race := b.findRace(raceID)
	if race == nil {
		return false, "race not found"
	}
	if !race.IsComplete {
		return true, ""
	}

	placed := stringSet(race.Winners)
	for _, l := range race.Losers {
		placed[l] = struct{}{}
	}

	for _, r := range b.state.Races {
		if r.ID == raceID {
			continue
		}
		if r.Round <= race.Round {
			continue
		}
		if !r.IsComplete {
			continue
		}
		for _, p := range r.Pilots {
			if _, ok := placed[p]; ok {
				return false, fmt.Sprintf("downstream race %s has already been run", r.ID)
			}
		}
	}

	// No separate chase-the-ace finalists guard: once the series has started the
	// completed CTA heats (which carry the finalist roster) are caught by the
	// downstream-IsComplete loop above, so a feeder is correctly frozen. Before
	// the series starts the finalists are provisional (re-synced with the feeders
	// by populateChaseTheAceFinalists), so a feeder may be corrected freely.
	return true, ""
}

// EditRace replaces a completed race's result with a new full placement.
// Removes the race's prior outputs (its winners + losers) from any incomplete
// downstream races, clears the race's complete/winners/losers state, then
// records the new placement via SelectFullPlacement.
//
// Refuses (returning an error) if CanEditRace says the downstream has already
// progressed past the point of safe editing.
func (b *Bracket) EditRace(raceID string, newPlacement []string) error {
	// Rewind this race (and any clean downstream state) first, then record the
	// new result. ResetRace is a no-op on a not-yet-run race, so a fresh edit
	// falls straight through to ApplyPlacement.
	if err := b.ResetRace(raceID); err != nil {
		return err
	}
	return b.ApplyPlacement(raceID, newPlacement)
}

// ResetRace un-runs a completed race: it strips the pilots this race advanced
// or dropped from any still-incomplete downstream race, then clears the race's
// own winners/losers/complete state so it becomes pending again. Refuses (via
// CanEditRace) when a downstream race that received these pilots has already
// been run. A no-op on a race that hasn't been run.
func (b *Bracket) ResetRace(raceID string) error {
	ok, reason := b.CanEditRace(raceID)
	if !ok {
		return fmt.Errorf("bracket: cannot edit %q: %s", raceID, reason)
	}
	race := b.findRace(raceID)
	if race == nil {
		return fmt.Errorf("bracket: race %q not found", raceID)
	}
	if !race.IsComplete {
		return nil // nothing to undo
	}

	// Build the set of pilots previously advanced/dropped out of this race.
	placed := stringSet(race.Winners)
	for _, l := range race.Losers {
		placed[l] = struct{}{}
	}

	// Strip those pilots from any downstream race that's still incomplete.
	for _, r := range b.state.Races {
		if r.ID == raceID {
			continue
		}
		if r.Round <= race.Round {
			continue
		}
		if r.IsComplete {
			continue
		}
		if len(r.Pilots) == 0 {
			continue
		}
		kept := r.Pilots[:0]
		for _, p := range r.Pilots {
			if _, drop := placed[p]; !drop {
				kept = append(kept, p)
			}
		}
		if len(kept) < len(r.Pilots) {
			r.Pilots = append([]string(nil), kept...)
		}
	}

	// Reset this race's completion state.
	race.Winners = nil
	race.Losers = nil
	race.IsComplete = false
	race.FullPlacement = false

	// Un-running a CTA finals heat has to reverse its contribution to the
	// running tally, or a later re-run/edit would count the win twice (and
	// could falsely crown a champion). The recompute is derived purely from the
	// still-completed heats, so clearing this one's IsComplete above is enough —
	// it also un-crowns a champion and reopens any voided surplus heats when the
	// clinching heat is the one being reset.
	if race.Bracket == SlotChaseTheAce {
		b.recomputeChaseTheAceProgress()
	}

	// If this reset left a chase-the-ace feeder incomplete (and the series hasn't
	// started), drop the now-stale provisional finalists so they re-derive when
	// the feeder is re-run. No-op for non-CTA brackets / once the series began.
	b.populateChaseTheAceFinalists()
	return nil
}

// ApplyPlacement is the shared "record a race result" core used by both fresh
// submission (after EditRace's reset, or first run via the handler) and edit
// flows. Accepts either a full placement (every pilot in race.Pilots, in
// finish order) or the top-N winners only (anything else stays a loser in
// race.Pilots order). Refuses fewer than the expected-winners count.
func (b *Bracket) ApplyPlacement(raceID string, placement []string) error {
	race := b.findRace(raceID)
	if race == nil {
		return fmt.Errorf("bracket: race %q not found", raceID)
	}
	expected := b.expectedWinnersCount(race)
	if len(placement) == len(race.Pilots) {
		return b.SelectFullPlacement(raceID, placement)
	}
	if len(placement) >= expected {
		return b.SelectWinners(raceID, placement[:expected])
	}
	return fmt.Errorf("bracket: race %q needs at least %d positions, got %d",
		raceID, expected, len(placement))
}

// ExpectedWinnersCount returns how many winners a race expects — 1 for finals
// (CTA / grand final / single-elim championship round), otherwise PilotsToAdvance
// (capped at the actual pilot count).
func (b *Bracket) ExpectedWinnersCount(race *Race) int {
	return b.expectedWinnersCount(race)
}

func (b *Bracket) expectedWinnersCount(race *Race) int {
	// Consolidation races have a hard-coded target (e.g. 2 for CTA, 1 for
	// grand-final) regardless of how many pilots ended up in them. Cap to
	// the actual pilot count so an empty consolidation race doesn't
	// demand more winners than it has pilots.
	if race.FinalTarget > 0 {
		t := race.FinalTarget
		if len(race.Pilots) > 0 && t > len(race.Pilots) {
			t = len(race.Pilots)
		}
		return t
	}
	if race.Bracket == SlotChaseTheAce ||
		race.Bracket == SlotGrandFinal ||
		(b.cfg.EliminationType == EliminationSingle && race.Round == b.state.Rounds) {
		return 1
	}
	if len(race.Pilots) == 0 {
		return b.cfg.PilotsToAdvance
	}
	return minInt(b.cfg.PilotsToAdvance, len(race.Pilots))
}

func (b *Bracket) advance(completed *Race) {
	cfg := b.cfg

	// Single elimination
	if cfg.EliminationType == EliminationSingle {
		if completed.Bracket == SlotChaseTheAce {
			b.advanceChaseTheAce(completed)
			return
		}

		nextRound := completed.Round + 1
		nextRaces := b.racesInRound(nextRound)
		if cfg.FinalsFormat == FinalsChaseTheAce && hasChaseTheAceRace(nextRaces) {
			// The next round IS the chase-the-ace final. Its line-up is the
			// derived finalist set — never a seeding of this heat's winners
			// across the pre-created CTA heats, which scattered one or two
			// pilots into CTA-1, CTA-2, … and burned them before the series
			// even started.
			b.populateChaseTheAceFinalists()
			return
		}
		if len(nextRaces) == 0 {
			// Either tournament is complete, or we need to create races/send to finals.
			currentWinners := b.totalWinnersInRound(completed.Round)
			if currentWinners == 1 {
				return // champion
			}
			if cfg.FinalsFormat == FinalsChaseTheAce {
				b.populateChaseTheAceFinalists()
				return
			}
			// Create the next round's races based on total expected winners.
			expected := b.estimatedNextRoundPilots(completed.Round)
			if expected > 1 {
				races := ceilDiv(expected, cfg.PilotsPerRace)
				for i := 0; i < races; i++ {
					b.state.Races = append(b.state.Races, &Race{
						ID:      fmt.Sprintf("R%d-%d", nextRound, i+1),
						Round:   nextRound,
						Bracket: SlotWinners,
					})
				}
				nextRaces = b.racesInRound(nextRound)
			}
		}
		if len(nextRaces) > 0 {
			b.placeSeeded(completed, completed.Winners, nextRaces)
		}
		if cfg.FinalsFormat == FinalsChaseTheAce {
			b.populateChaseTheAceFinalists()
		}
		b.checkAutoAdvanceAll()
		return
	}

	// Double elimination
	switch completed.Bracket {
	case SlotWinners:
		nextRound := completed.Round + 1
		nextRaces := b.racesInBracketRound(nextRound, SlotWinners)
		if len(nextRaces) > 0 {
			b.placeSeeded(completed, completed.Winners, nextRaces)
		} else if len(completed.Winners) > 0 {
			// Last winners round — send to finals.
			if cfg.FinalsFormat == FinalsChaseTheAce {
				b.populateChaseTheAceFinalists()
			} else {
				if gf := b.findRace("GF-1"); gf != nil {
					gf.Pilots = appendUnique(gf.Pilots, completed.Winners...)
				}
			}
		}
		if len(completed.Losers) > 0 {
			// MultiGP-style seed-aware drop: each W race's losers go to a
			// deterministic L race based on the W race's snake position in
			// its W round, so two top-seed-droppers from different W heats
			// don't end up in the same L1 race. Falls back to the old
			// least-filled logic only when no matching L round exists at the
			// expected round number (irregular bracket shape).
			lTargets := b.racesInBracketRound(b.losersDropRound(completed.Round), SlotLosers)
			if len(lTargets) > 0 {
				remaining := b.placeSeeded(completed, completed.Losers, lTargets)
				if len(remaining) > 0 {
					b.placeLosersInBracket(remaining, completed.Round)
				}
			} else {
				b.placeLosersInBracket(completed.Losers, completed.Round)
			}
		}

	case SlotLosers:
		nextRound := completed.Round + 1
		nextRaces := b.racesInBracketRound(nextRound, SlotLosers)
		if len(nextRaces) > 0 {
			// Same snake-shift as the winners side, applied to losers→losers
			// progression — top L1 advancers don't immediately rematch in L2.
			remaining := b.placeSeeded(completed, completed.Winners, nextRaces)
			if len(remaining) > 0 {
				// Create another race in the next losers round for overflow.
				// L_n is at round n+1, so counter = round - 1.
				counter := nextRound - 1
				existing := len(nextRaces)
				newRace := &Race{
					ID:      fmt.Sprintf("L%d-%d", counter, existing+1),
					Round:   nextRound,
					Pilots:  append([]string(nil), remaining...),
					Bracket: SlotLosers,
				}
				b.state.Races = append(b.state.Races, newRace)
			}
		} else if len(completed.Winners) > 0 {
			// Final losers round — to finals.
			if cfg.FinalsFormat == FinalsChaseTheAce {
				b.populateChaseTheAceFinalists()
			} else {
				if gf := b.findRace("GF-1"); gf != nil {
					gf.Pilots = appendUnique(gf.Pilots, completed.Winners...)
				}
			}
		}

	case SlotGrandFinal:
		if completed.ID == "GF-1" {
			winner := ""
			if len(completed.Winners) > 0 {
				winner = completed.Winners[0]
			}
			losersFinalists := b.getLosersFinalists()
			if stringInSlice(winner, losersFinalists) {
				if gf2 := b.findRace("GF-2"); gf2 != nil {
					gf2.Pilots = append([]string(nil), completed.Pilots...)
				}
			}
		}

	case SlotChaseTheAce:
		b.advanceChaseTheAce(completed)
		return
	}

	if cfg.FinalsFormat == FinalsChaseTheAce {
		b.populateChaseTheAceFinalists()
	}
	b.checkAutoAdvanceAll()
}

// checkChaseTheAceEntry gates results on chase-the-ace finals races. CTA races
// are all pre-created up front, so a result must only be accepted for the
// lowest-numbered un-run heat, never out of order, never for a voided heat, and
// never once a champion has already been crowned. Non-CTA races are unaffected.
func (b *Bracket) checkChaseTheAceEntry(race *Race) error {
	if race.Bracket != SlotChaseTheAce {
		return nil
	}
	if race.IsVoid {
		return fmt.Errorf("bracket: chase-the-ace race %q is void (champion already decided)", race.ID)
	}
	cta := b.state.ChaseTheAce
	if cta != nil && cta.IsComplete {
		return fmt.Errorf("bracket: chase-the-ace already complete (champion %q)", cta.Champion)
	}
	next := b.nextChaseTheAceRace()
	if next != nil && next.ID != race.ID {
		return fmt.Errorf("bracket: chase-the-ace races must be run in order; %q is next, not %q",
			next.ID, race.ID)
	}
	return nil
}

func (b *Bracket) advanceChaseTheAce(completed *Race) {
	// The tally is derived wholesale from the completed CTA heats rather than
	// incremented per-call, so re-running, editing, or re-ingesting a result
	// (all of which route through ResetRace + a fresh apply) can never
	// double-count a heat. `completed` is unused — it's already recorded on the
	// race by the caller; the recompute picks it up from state.
	b.recomputeChaseTheAceProgress()
}

// recomputeChaseTheAceProgress derives the entire chase-the-ace tally — per-
// pilot wins and position points, the champion, and the final standings —
// purely from the completed CTA heats, in race order. Because it starts from a
// clean slate every time, it is idempotent: running a heat, editing its result,
// resetting it, or re-ingesting the same result from the timing system all
// converge on the same state, so a win is counted exactly once. When a champion
// emerges the surplus un-run heats are voided (not deleted) so an edit that
// removes the clinching win can reopen them.
func (b *Bracket) recomputeChaseTheAceProgress() {
	cta := b.state.ChaseTheAce
	if cta == nil {
		return
	}

	cta.Wins = make(map[string]int, len(cta.Finalists))
	cta.Points = make(map[string]int, len(cta.Finalists))
	for _, f := range cta.Finalists {
		cta.Wins[f] = 0
		cta.Points[f] = 0
	}
	cta.Champion = ""
	cta.IsComplete = false
	cta.Standings = nil
	cta.TieBreakNeeded = nil

	// Replay completed heats in CTA-1, CTA-2, … order. Voided placeholders and
	// un-run heats contribute nothing.
	for _, r := range b.chaseTheAceRaces() {
		if r.IsVoid || !r.IsComplete || len(r.Winners) == 0 {
			continue
		}
		if r.FullPlacement {
			// SelectFullPlacement reorders race.Pilots to finish order, so
			// race.Pilots[i] is the (i+1)th-place finisher. 1pt for 1st, 2 for
			// 2nd, … (lower total = better non-champion finish).
			for i, p := range r.Pilots {
				cta.Points[p] += i + 1
			}
		} else {
			// Partial entry — only the winner is known. 1 point to them.
			cta.Points[r.Winners[0]]++
		}
		winner := r.Winners[0]
		cta.Wins[winner]++
		if cta.Wins[winner] >= cta.RequiredWins {
			cta.Champion = winner
			cta.IsComplete = true
			b.computeChaseTheAceStandings()
			break
		}
	}

	b.reconcileChaseTheAceVoids()
}

// reconcileChaseTheAceVoids marks the un-run CTA heats void once a champion is
// crowned (all heats are pre-created up front so they can be exported to the
// timing system in advance; the surplus is no longer needed) and un-voids every
// heat while the series is still undecided — so an edit that removes a clinching
// win brings the remaining heats back into play. Completed heats are never void.
func (b *Bracket) reconcileChaseTheAceVoids() {
	cta := b.state.ChaseTheAce
	decided := cta != nil && cta.IsComplete
	for _, r := range b.chaseTheAceRaces() {
		if r.IsComplete {
			r.IsVoid = false
			continue
		}
		r.IsVoid = decided
	}
}

// computeChaseTheAceStandings ranks the non-champion finalists by total
// Points (ascending). Pilots tied on points are surfaced in TieBreakNeeded
// so the UI can prompt the race director for a tie-break race.
func (b *Bracket) computeChaseTheAceStandings() {
	cta := b.state.ChaseTheAce
	if cta == nil || cta.Champion == "" {
		return
	}
	others := make([]string, 0, len(cta.Finalists))
	for _, p := range cta.Finalists {
		if p != cta.Champion {
			others = append(others, p)
		}
	}
	// Fewest points first; a points tie goes to the higher qualifying seed
	// (the rule points-mains uses) — finalist order would otherwise hand
	// every tie to the winners-side pilot. TieBreakNeeded still flags the
	// tie so the director can run a decider and override on Finalise.
	sort.SliceStable(others, func(i, j int) bool {
		pi, pj := cta.Points[others[i]], cta.Points[others[j]]
		if pi != pj {
			return pi < pj
		}
		return b.seedOf(others[i]) < b.seedOf(others[j])
	})
	cta.Standings = append([]string{cta.Champion}, others...)

	// Detect ties — adjacent non-champion pilots with equal points.
	cta.TieBreakNeeded = nil
	i := 0
	for i < len(others) {
		j := i
		for j+1 < len(others) && cta.Points[others[j+1]] == cta.Points[others[i]] {
			j++
		}
		if j > i {
			group := append([]string(nil), others[i:j+1]...)
			cta.TieBreakNeeded = append(cta.TieBreakNeeded, group)
		}
		i = j + 1
	}
}

// placeBalanced drops each pilot into the next-round race with the fewest pilots
// that still has room. Returns any pilots that couldn't be placed.
func (b *Bracket) placeBalanced(pilots []string, targets []*Race) []string {
	affected := map[*Race]bool{}
	remaining := []string{}
	for _, p := range pilots {
		var pick *Race
		for _, r := range targets {
			if r.FinalTarget == 0 && len(r.Pilots) >= b.cfg.PilotsPerRace {
				continue
			}
			if pick == nil || len(r.Pilots) < len(pick.Pilots) {
				pick = r
			}
		}
		if pick == nil {
			remaining = append(remaining, p)
			continue
		}
		pick.Pilots = append(pick.Pilots, p)
		affected[pick] = true
	}
	for r := range affected {
		b.checkAutoAdvance(r)
	}
	return remaining
}

// placeSeeded distributes winners from a completed winners-bracket race into the
// next round's target races using MultiGP-style snake placement.
//
// Conceptually, all advancers from a round form one sequence — every source's
// rank-0 winner first (in source order), then every source's rank-1, etc. — and
// that sequence is snake-distributed across the targets:
//
//	advancerIdx = rank*numSources + srcIdx
//	target      = snake(advancerIdx, numTargets)
//
// Each race only places its own winners, but because the formula is deterministic
// per (srcIdx, rank), the same overall layout is built up regardless of which
// race completes when. This guarantees:
//
//   - top seeds (lowest srcIdx, rank 0) land in different next-round races
//   - a heat's 1st and 2nd advancer land in different next-round races
//   - per-target load matches what the bracket math sized the next round for
//     (ceilDiv(totalAdvancers, pilotsPerRace) targets), so capacity is respected
//
// Returns any winners that could not be placed (every target full).
func (b *Bracket) placeSeeded(completed *Race, winners []string, targets []*Race) []string {
	if len(winners) == 0 || len(targets) == 0 {
		return append([]string(nil), winners...)
	}

	siblings := []*Race{}
	for _, r := range b.state.Races {
		if r.Round == completed.Round && r.Bracket == completed.Bracket {
			siblings = append(siblings, r)
		}
	}
	sort.Slice(siblings, func(i, j int) bool { return raceIndexNum(siblings[i].ID) < raceIndexNum(siblings[j].ID) })
	srcIdx := -1
	for i, r := range siblings {
		if r.ID == completed.ID {
			srcIdx = i
			break
		}
	}
	if srcIdx < 0 {
		return b.placeBalanced(winners, targets)
	}

	sortedTargets := append([]*Race(nil), targets...)
	sort.Slice(sortedTargets, func(i, j int) bool { return raceIndexNum(sortedTargets[i].ID) < raceIndexNum(sortedTargets[j].ID) })
	numTargets := len(sortedTargets)

	// snakePos = this source's snake-mapped target index (top seeds first).
	row := srcIdx / numTargets
	pos := srcIdx % numTargets
	snakePos := pos
	if row%2 == 1 {
		snakePos = numTargets - 1 - pos
	}

	// Re-rank the inbound pilots by their ABSOLUTE qualifying seed so the
	// snake-shift carries the bracket-wide seed structure forward. Without
	// this, an upset (e.g. seed 16 beats seed 2 in a heat) makes seed 2
	// look like a "rank 1" pilot and lands them in the same next-round
	// race as the actual rank-0 top seed — letting seeds 1 and 2 collide
	// rounds earlier than the MultiGP-style pre-charted bracket would.
	winners = append([]string(nil), winners...)
	sort.SliceStable(winners, func(i, j int) bool {
		return b.seedOf(winners[i]) < b.seedOf(winners[j])
	})

	affected := map[*Race]bool{}
	remaining := []string{}
	usedByThisRace := map[int]bool{}
	for rank, winner := range winners {
		// Rank 0 goes to the snake target; each subsequent rank shifts one over.
		// This puts a heat's 1st and 2nd in different next-round races.
		targetIdx := (snakePos + rank) % numTargets
		target := sortedTargets[targetIdx]

		full := target.FinalTarget == 0 && len(target.Pilots) >= b.cfg.PilotsPerRace
		clash := usedByThisRace[targetIdx]
		if full || clash {
			// Walk forward looking for a target that is (a) not full and
			// (b) not already used by this source race. This avoids both
			// capacity overflow and same-source rematches under overflow.
			placed := false
			for offset := 1; offset < numTargets; offset++ {
				altIdx := (targetIdx + offset) % numTargets
				alt := sortedTargets[altIdx]
				if len(alt.Pilots) >= b.cfg.PilotsPerRace {
					continue
				}
				if usedByThisRace[altIdx] {
					continue
				}
				alt.Pilots = append(alt.Pilots, winner)
				affected[alt] = true
				usedByThisRace[altIdx] = true
				placed = true
				break
			}
			if placed {
				continue
			}
			// Second pass: relax the same-source constraint if nothing else fits.
			for offset := 0; offset < numTargets; offset++ {
				altIdx := (targetIdx + offset) % numTargets
				alt := sortedTargets[altIdx]
				if len(alt.Pilots) >= b.cfg.PilotsPerRace {
					continue
				}
				alt.Pilots = append(alt.Pilots, winner)
				affected[alt] = true
				usedByThisRace[altIdx] = true
				placed = true
				break
			}
			if !placed {
				remaining = append(remaining, winner)
			}
			continue
		}
		target.Pilots = append(target.Pilots, winner)
		affected[target] = true
		usedByThisRace[targetIdx] = true
	}
	for r := range affected {
		b.checkAutoAdvance(r)
	}
	return remaining
}

// raceIndexNum extracts the trailing integer from a race ID like "W2-13".
// Falls back to 0 for IDs without a numeric suffix (e.g. "L1-extra").
func raceIndexNum(id string) int {
	dash := strings.LastIndex(id, "-")
	if dash < 0 || dash == len(id)-1 {
		return 0
	}
	n, err := strconv.Atoi(id[dash+1:])
	if err != nil {
		return 0
	}
	return n
}

// losersDropRound is the absolute round of the redemption races that take
// winners-round wRound's droppers, read off the FromWinnersRound stamped at
// generation. Brackets generated before that field existed (already-saved state)
// carry 0 everywhere and fall back to the old winners-round-N → L_N mapping, so
// an in-progress bracket keeps advancing exactly as it did.
func (b *Bracket) losersDropRound(wRound int) int {
	best := 0
	for _, r := range b.state.Races {
		if r.Bracket == SlotLosers && r.FromWinnersRound == wRound {
			if best == 0 || r.Round < best {
				best = r.Round
			}
		}
	}
	if best > 0 {
		return best
	}
	return wRound + 1
}

// placeLosersInBracket puts losers from winners-round N into losers-round N.
// Uses round-robin distribution for separation. Creates a fallback race if no
// matching losers race has room.
func (b *Bracket) placeLosersInBracket(losers []string, winnersRoundLost int) {
	if len(losers) == 0 {
		return
	}
	absoluteRound := b.losersDropRound(winnersRoundLost)
	maxL := 0
	for _, r := range b.state.Races {
		if r.Bracket == SlotLosers && r.Round > maxL {
			maxL = r.Round
		}
	}
	if maxL == 0 {
		return
	}
	if absoluteRound > maxL {
		absoluteRound = maxL
	}

	candidates := []*Race{}
	for _, r := range b.state.Races {
		if r.Bracket == SlotLosers && r.Round == absoluteRound && !r.IsComplete && len(r.Pilots) < b.cfg.PilotsPerRace {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		// Fallback: any incomplete losers race with room.
		for _, r := range b.state.Races {
			if r.Bracket == SlotLosers && !r.IsComplete && len(r.Pilots) < b.cfg.PilotsPerRace {
				candidates = append(candidates, r)
			}
		}
	}
	if len(candidates) == 0 {
		// Dynamically create a new race for them.
		counter := absoluteRound - 1
		newRace := &Race{
			ID:      fmt.Sprintf("L%d-extra", counter),
			Round:   absoluteRound,
			Bracket: SlotLosers,
		}
		b.state.Races = append(b.state.Races, newRace)
		candidates = []*Race{newRace}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })

	affected := map[*Race]bool{}
	// Drop into the LEAST-FILLED candidate first. placeLosersInBracket is
	// called once per source W-race completion, so a fixed round-robin
	// across calls always picks candidates[0] first — that's how W2-1's
	// dropper AND W2-2's dropper both ended up in L2-1, giving L2-1 four
	// pilots and L2-2 two (auto-advance). Least-filled balances across
	// calls automatically.
	for _, l := range losers {
		var pick *Race
		for _, r := range candidates {
			if r.FinalTarget == 0 && len(r.Pilots) >= b.cfg.PilotsPerRace {
				continue
			}
			if pick == nil || len(r.Pilots) < len(pick.Pilots) {
				pick = r
			}
		}
		if pick == nil {
			break
		}
		pick.Pilots = append(pick.Pilots, l)
		affected[pick] = true
	}
	for r := range affected {
		b.checkAutoAdvance(r)
	}
}

// populateChaseTheAceFinalists fills every pre-created CTA race with the
// finalists once both bracket sides are done. All CTA races carry the same
// finalist roster so any of them can be exported to the timing system ahead
// of time.
func (b *Bracket) populateChaseTheAceFinalists() {
	cta := b.state.ChaseTheAce
	if cta == nil || b.ctaStarted() {
		// No CTA, or the series has already started → the finalist set is frozen
		// (editing a feeder is then blocked by the ordinary downstream check).
		return
	}

	want := b.ctaFinalistCount()
	finalists := b.deriveChaseTheAceFinalists(want)
	if len(finalists) < want {
		// A feeder is still incomplete — e.g. a winners/redemption final was just
		// reset for re-entry. Drop any provisional set so the final lineup can't
		// go stale; it re-derives when the feeder is re-run.
		b.clearProvisionalFinalists()
		return
	}
	finalists = finalists[:want]
	cta.Finalists = finalists
	if cta.Points == nil {
		cta.Points = map[string]int{}
	}
	for _, p := range finalists {
		cta.Wins[p] = 0
		cta.Points[p] = 0
	}
	// Seed the finalists into EVERY pre-created CTA race (not just the first)
	// so all heats can be exported up front. Skip voided/completed races.
	for _, r := range b.chaseTheAceRaces() {
		if r.IsVoid || r.IsComplete {
			continue
		}
		r.Pilots = append([]string(nil), finalists...)
	}
}

// ctaStarted reports whether the chase-the-ace series has actually begun — a real
// finals heat has been RUN (recording a win) or the champion is decided. Keyed on
// wins, NOT "any CTA heat complete": single elimination spreads winners across the
// pre-created CTA heats and then auto-completes any left holding a lone pilot
// (checkAutoAdvanceAll), and those placeholders never run through advanceChaseTheAce
// so they leave wins at zero. Until a real heat runs the finalist set is provisional
// and the winners/redemption finals stay editable.
func (b *Bracket) ctaStarted() bool {
	cta := b.state.ChaseTheAce
	if cta == nil {
		return false
	}
	if cta.IsComplete || cta.Champion != "" {
		return true
	}
	for _, w := range cta.Wins {
		if w > 0 {
			return true
		}
	}
	return false
}

// clearProvisionalFinalists drops a not-yet-locked finalist set so a feeder reset
// before the series starts doesn't leave a stale final lineup; it re-derives once
// the feeder is re-run. It deliberately leaves the CTA heats' Pilots alone: under
// single elimination those are an accumulator advance() fills incrementally as
// semifinals complete, and under double elimination they're overwritten when the
// finalists re-resolve. The all-pilots-zero Wins/Points maps from
// initChaseTheAceState are also left intact (advanceChaseTheAce only reads finalist
// entries; standings only iterate the current finalists).
func (b *Bracket) clearProvisionalFinalists() {
	cta := b.state.ChaseTheAce
	if cta == nil {
		return
	}
	cta.Finalists = nil
}

// deriveChaseTheAceFinalists computes the CTA finalists from the resolved
// winners/redemption finals (or the semifinal for single elimination), or returns
// fewer than `want` (caller treats that as "not ready") while a side is incomplete.
func (b *Bracket) deriveChaseTheAceFinalists(want int) []string {
	finalists := []string{}
	if b.cfg.EliminationType == EliminationSingle {
		// Get the finalists from the semifinal round (round before CTA).
		ctaRace := b.findChaseTheAceRace()
		if ctaRace == nil {
			return nil
		}
		if len(ctaRace.Pilots) >= want {
			finalists = append(finalists, ctaRace.Pilots[:want]...)
		} else {
			semiRound := ctaRace.Round - 1
			for _, r := range b.state.Races {
				if r.Round == semiRound && r.Bracket == SlotWinners && r.IsComplete {
					finalists = append(finalists, r.Winners...)
				}
			}
		}
	} else {
		w := b.getWinnersFinalists()
		l := b.getLosersFinalists()
		if w == nil || l == nil {
			return nil // sides not both complete yet
		}
		// Target `want` CTA finalists (usually 4): ideally split evenly
		// across the two sides. When either side has more (tight
		// ppr/advance ratios converge the bracket to 4-pilot rounds
		// instead of 2-pilot ones), take the top of each. When a side has
		// fewer, top up from the other.
		target := want
		wTake := target / 2
		lTake := target - wTake
		if wTake > len(w) {
			lTake += wTake - len(w)
			wTake = len(w)
		}
		if lTake > len(l) {
			extra := lTake - len(l)
			lTake = len(l)
			if wTake+extra <= len(w) {
				wTake += extra
			}
		}
		finalists = append(finalists, w[:wTake]...)
		finalists = append(finalists, l[:lTake]...)
	}
	return finalists
}

// checkAutoAdvance marks a race complete when it has the right number of pilots
// and no more can arrive. E.g. a 2-pilot race with pilotsToAdvance=2 needs no run.
func (b *Bracket) checkAutoAdvance(r *Race) {
	if r.IsComplete || r.IsVoid {
		return
	}
	if b.canReceiveMorePilots(r) {
		return
	}
	isFinal := r.Round == b.state.Rounds || r.Bracket == SlotGrandFinal || r.Bracket == SlotChaseTheAce
	if isFinal {
		if len(r.Pilots) == 1 {
			r.Winners = append([]string(nil), r.Pilots...)
			r.IsComplete = true
		}
		return
	}
	if len(r.Pilots) == 0 {
		return
	}
	// Only a genuine solo bye — a single pilot with nobody else able to arrive —
	// auto-advances, because one pilot can't race. A heat with two or more pilots
	// is still flown on the day even when everyone in it advances (not enough
	// pilots to eliminate anyone), so it stays open for the race director to
	// record the actual finishing order rather than being auto-resulted before
	// the pilots have raced.
	if len(r.Pilots) == 1 {
		r.Winners = append([]string(nil), r.Pilots...)
		r.Losers = r.Losers[:0]
		r.IsComplete = true
		b.advance(r)
	}
}

func (b *Bracket) checkAutoAdvanceAll() {
	// Snapshot pointers — advance() may append to b.state.Races.
	snapshot := append([]*Race(nil), b.state.Races...)
	for _, r := range snapshot {
		if !r.IsComplete && !r.IsVoid && len(r.Pilots) > 0 {
			b.checkAutoAdvance(r)
		}
	}
}

// CanReceiveMorePilots reports whether a race is still waiting on upstream
// results to deliver more pilots — used by the runner UI to block submitting
// a result until everyone the race expects has actually arrived.
func (b *Bracket) CanReceiveMorePilots(target *Race) bool {
	return b.canReceiveMorePilots(target)
}

func (b *Bracket) canReceiveMorePilots(target *Race) bool {
	if len(target.Pilots) >= b.cfg.PilotsPerRace {
		return false
	}
	switch target.Bracket {
	case SlotLosers:
		for _, r := range b.state.Races {
			if r.Bracket == SlotWinners && !r.IsComplete && len(r.Pilots) > 0 {
				return true
			}
		}
		for _, r := range b.state.Races {
			if r.Bracket == SlotLosers && r.Round < target.Round && !r.IsComplete && len(r.Pilots) > 0 {
				return true
			}
		}
	case SlotWinners:
		for _, r := range b.state.Races {
			if r.Bracket == SlotWinners && r.Round < target.Round && !r.IsComplete && len(r.Pilots) > 0 {
				return true
			}
		}
	case SlotGrandFinal:
		for _, r := range b.state.Races {
			if (r.Bracket == SlotWinners || r.Bracket == SlotLosers) && !r.IsComplete && len(r.Pilots) > 0 {
				return true
			}
		}
	}
	return false
}

// ---------- queries ----------

// Champion returns the tournament winner, or empty string if not yet determined.
func (b *Bracket) Champion() string {
	cfg := b.cfg
	if cfg.FinalsFormat == FinalsChaseTheAce {
		if b.state.ChaseTheAce != nil && b.state.ChaseTheAce.IsComplete {
			return b.state.ChaseTheAce.Champion
		}
		return ""
	}
	if cfg.EliminationType == EliminationSingle {
		final := b.lastWinnersRace()
		if final != nil && final.IsComplete && len(final.Winners) > 0 {
			return final.Winners[0]
		}
		return ""
	}
	if gf2 := b.findRace("GF-2"); gf2 != nil && gf2.IsComplete && len(gf2.Winners) > 0 {
		return gf2.Winners[0]
	}
	if gf1 := b.findRace("GF-1"); gf1 != nil && gf1.IsComplete && len(gf1.Winners) > 0 {
		winnersFinalists := b.getWinnersFinalists()
		if stringInSlice(gf1.Winners[0], winnersFinalists) {
			return gf1.Winners[0]
		}
	}
	return ""
}

// IsComplete returns true if a champion has been determined.
func (b *Bracket) IsComplete() bool { return b.Champion() != "" }

// ---------- internal helpers ----------

func (b *Bracket) findRace(id string) *Race {
	for _, r := range b.state.Races {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (b *Bracket) findChaseTheAceRace() *Race {
	for _, r := range b.state.Races {
		if r.Bracket == SlotChaseTheAce {
			return r
		}
	}
	return nil
}

// chaseTheAceRaces returns every chase-the-ace finals race (including voided
// ones) sorted by their trailing race index (CTA-1, CTA-2, …). Voided races
// stay in the slice so callers that care about IDs see the full set; callers
// that only want live races should filter on !r.IsVoid.
func (b *Bracket) chaseTheAceRaces() []*Race {
	out := []*Race{}
	for _, r := range b.state.Races {
		if r.Bracket == SlotChaseTheAce {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return raceIndexNum(out[i].ID) < raceIndexNum(out[j].ID) })
	return out
}

// nextChaseTheAceRace returns the lowest-numbered chase-the-ace race that is
// neither complete nor voided — the only CTA race allowed to receive a result.
// Returns nil when none remain (champion crowned or all run).
func (b *Bracket) nextChaseTheAceRace() *Race {
	for _, r := range b.chaseTheAceRaces() {
		if r.IsVoid {
			continue
		}
		if !r.IsComplete {
			return r
		}
	}
	return nil
}

// ctaFinalistCount is how many finalists the chase-the-ace round runs with.
// The bracket converges toward 4 finalists; with fewer than 4 pilots overall
// every pilot is a finalist. This is used both to size the pre-created CTA
// race set and (defensively) elsewhere.
func (b *Bracket) ctaFinalistCount() int {
	return ctaFinalists(b.cfg)
}

// ctaFinalists is how many pilots contest the chase-the-ace final: a full heat
// (PilotsPerRace), capped by the field size. It follows PilotsPerRace rather
// than a fixed 4 because the final is a heat like any other — a club racing
// 6-up finishes with six finalists, a 3-up club with three (four pilots would
// not fit the heat at all). Legacy/zero configs fall back to the historical 4.
func ctaFinalists(cfg Config) int {
	ppr := cfg.PilotsPerRace
	if ppr < 2 {
		ppr = 4
	}
	return minInt(ppr, cfg.PilotCount)
}

// maxChaseTheAceRaces returns the maximum number of CTA heats that could ever
// be required: every finalist but one wins (requiredWins-1) heats without
// anyone clinching, then one more heat decides it. That worst case is
// (requiredWins-1)*finalistCount + 1 races. Pre-creating this many lets the
// race director export all CTA heats to the timing system up front.
func maxChaseTheAceRaces(finalistCount, requiredWins int) int {
	if finalistCount < 1 {
		finalistCount = 1
	}
	if requiredWins < 1 {
		requiredWins = 1
	}
	return (requiredWins-1)*finalistCount + 1
}

func (b *Bracket) racesInRound(round int) []*Race {
	out := []*Race{}
	for _, r := range b.state.Races {
		if r.Round == round {
			out = append(out, r)
		}
	}
	return out
}

func (b *Bracket) racesInBracketRound(round int, slot BracketSlot) []*Race {
	out := []*Race{}
	for _, r := range b.state.Races {
		if r.Round == round && r.Bracket == slot {
			out = append(out, r)
		}
	}
	return out
}

func (b *Bracket) winnersRoundCount() int {
	seen := map[int]bool{}
	for _, r := range b.state.Races {
		if r.Bracket == SlotWinners {
			seen[r.Round] = true
		}
	}
	return len(seen)
}

func (b *Bracket) totalWinnersInRound(round int) int {
	total := 0
	for _, r := range b.state.Races {
		if r.Round == round && r.IsComplete {
			total += len(r.Winners)
		}
	}
	return total
}

func (b *Bracket) estimatedNextRoundPilots(round int) int {
	total := 0
	for _, r := range b.state.Races {
		if r.Round != round || r.Bracket != SlotWinners {
			continue
		}
		if r.IsComplete {
			total += len(r.Winners)
		} else if len(r.Pilots) > 0 {
			total += minInt(b.cfg.PilotsToAdvance, len(r.Pilots))
		}
	}
	return total
}

func (b *Bracket) lastWinnersRace() *Race {
	var last *Race
	for _, r := range b.state.Races {
		if r.Bracket == SlotWinners {
			last = r
		}
	}
	return last
}

// getWinnersFinalists collects every winners-bracket race in the highest
// round, sorted by trailing race index, then concatenates the winners. This
// matters when the last winners round has more than one race (e.g. tight
// configs like 3-per-race/2-advance/4-finalists, where two parallel 2-pilot
// auto-advancing heats produce 4 finalists across 2 races). Returns nil if
// any race in the last round is incomplete — the caller waits.
func (b *Bracket) getWinnersFinalists() []string {
	maxRound := 0
	for _, r := range b.state.Races {
		if r.Bracket == SlotWinners && r.Round > maxRound {
			maxRound = r.Round
		}
	}
	if maxRound == 0 {
		return nil
	}
	lastRound := []*Race{}
	for _, r := range b.state.Races {
		if r.Bracket == SlotWinners && r.Round == maxRound {
			if !r.IsComplete {
				return nil
			}
			lastRound = append(lastRound, r)
		}
	}
	sort.Slice(lastRound, func(i, j int) bool { return raceIndexNum(lastRound[i].ID) < raceIndexNum(lastRound[j].ID) })
	out := []string{}
	for _, r := range lastRound {
		out = append(out, r.Winners...)
	}
	return out
}

// getLosersFinalists is the losers-side counterpart to getWinnersFinalists.
func (b *Bracket) getLosersFinalists() []string {
	maxRound := 0
	for _, r := range b.state.Races {
		if r.Bracket == SlotLosers && r.Round > maxRound {
			maxRound = r.Round
		}
	}
	if maxRound == 0 {
		return nil
	}
	lastRound := []*Race{}
	for _, r := range b.state.Races {
		if r.Bracket == SlotLosers && r.Round == maxRound {
			if !r.IsComplete {
				return nil
			}
			lastRound = append(lastRound, r)
		}
	}
	sort.Slice(lastRound, func(i, j int) bool { return raceIndexNum(lastRound[i].ID) < raceIndexNum(lastRound[j].ID) })
	out := []string{}
	for _, r := range lastRound {
		out = append(out, r.Winners...)
	}
	return out
}

// ---------- small utilities ----------

func ceilDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func countRacesInRound(races []*Race, round int) int {
	n := 0
	for _, r := range races {
		if r.Round == round {
			n++
		}
	}
	return n
}

func countRacesInBracketRound(races []*Race, round int, slot BracketSlot) int {
	n := 0
	for _, r := range races {
		if r.Round == round && r.Bracket == slot {
			n++
		}
	}
	return n
}

func stringSet(ss []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range ss {
		out[s] = struct{}{}
	}
	return out
}

func stringInSlice(s string, ss []string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func appendUnique(dst []string, vals ...string) []string {
	seen := stringSet(dst)
	for _, v := range vals {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		dst = append(dst, v)
	}
	return dst
}

func hasChaseTheAceRace(races []*Race) bool {
	for _, r := range races {
		if r.Bracket == SlotChaseTheAce {
			return true
		}
	}
	return false
}
