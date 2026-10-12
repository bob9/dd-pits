// Package qualbracket implements the "Double Elimination — Top 4 Advance"
// qualifier race format: a MultiGP-style double-elimination bracket that does
// NOT crown a single champion but ends in a single FINAL of the top 4 finalists
// (top-2 from the winners-bracket final + top-2 from the redemption final) and
// outputs a ranked "Qualified" top-4 that continue into a later bracket. The
// final may optionally be run as a chase-the-ace mini-series among the four.
//
// The bracket is described declaratively as a Spec: every race lists the exact
// source slots (a finishing position of a prior race) that feed it. The 16-pilot
// case is a literal transcription of MultiGP's published bracket sheet so it
// reproduces it exactly; other field sizes use a generic double-elimination
// generator. The interpreter (qualbracket.go) materialises a *bracket.State from
// the spec, reusing the bracket package's Race/State/slot types so the existing
// SVG renderer, runner grid, DroneDash ingest and finalise flow all work unchanged.
package qualbracket

import (
	"fmt"

	"github.com/bob9/dd-pits/internal/stages/bracket"
)

const (
	// DefaultPilotsPerRace / DefaultAdvance are the format's MultiGP standard
	// (4-up, top-2 advance) — the shape every bracket had before the size
	// became configurable, and what a zero Config still gets.
	DefaultPilotsPerRace = 4
	DefaultAdvance       = 2
	specPPR              = DefaultPilotsPerRace
	specAdvance          = DefaultAdvance
)

// SlotRef points at a finishing position of a prior race: "take the Nth-place
// finisher of race FromID". Position is 1-based (1 = winner). When FromID is
// empty the ref is a Round-1 seed entry and Position is the absolute seed
// (1 = top qualifier).
type SlotRef struct {
	FromID   string
	Position int
}

// SpecRace declares one heat structurally — where each of its pilots comes from.
type SpecRace struct {
	ID      string
	Round   int
	Slot    bracket.BracketSlot
	Advance int       // how many advance out of this race
	Sources []SlotRef // ordered: Sources[i] fills pilot slot i
	// Terminal marks the FINAL heat: its pilots qualify out rather than advancing
	// into another race. FinalTarget mirrors bracket.Race.FinalTarget so the
	// engine-driven renderer reports the right expected-winner count.
	Terminal    bool
	FinalTarget int
}

// Spec is a fully-wired qualifier bracket: every body race's inputs are explicit
// SlotRefs, plus the two finals whose top-2 each feed the terminal final.
type Spec struct {
	Body           []SpecRace // winners + redemption body races
	WinnersFinalID string     // race whose top-2 are winners-side finalists
	LosersFinalID  string     // race whose top-2 are losers-side finalists ("" for tiny fields)
	Finalists      []SlotRef  // the (up to 4) refs feeding the terminal final
	FinalRound     int        // round/column for the terminal final
	CTA            bool       // run the final as a chase-the-ace series
}

// seedSources turns a group of seed numbers into Round-1 SlotRefs.
func seedSources(seeds []int) []SlotRef {
	out := make([]SlotRef, len(seeds))
	for i, s := range seeds {
		out[i] = SlotRef{Position: s}
	}
	return out
}

// photoSeed16 reproduces the supplied photo's exact round-1 slot placement,
// mapping its pilots to ranks 1..16 by their event seed (lowest = best). It is
// NOT a snake — the photo funnels several top seeds toward the same side — so
// top seeds can meet earlier than under MultiGP snake seeding. Each inner slice
// is one round-1 heat in its channel order.
var photoSeed16 = [][]int{
	{5, 12, 6, 8},   // R1: event seeds 18,52,19,26
	{10, 11, 15, 7}, // R2: event seeds 35,43,56,24
	{2, 13, 14, 3},  // R3: event seeds 7,53,54,8
	{9, 1, 4, 16},   // R4: event seeds 28,5,16,61
}

// specFor16 is the MultiGP 16-pilot double-elimination "Top 4 advance" bracket
// (the supplied photo). The redemption drop composition is transcribed verbatim
// from the sheet. The Round-1 seeding is selectable:
//
//   - snake (default): MultiGP snake seeding, heats ordered by top seed 1,4,2,3
//     (the standard 4-slot bracket split) so seeds 1 and 2 fall on opposite
//     halves and only meet in the winners final. Heats:
//     W1-1=[1,8,9,16] W1-2=[4,5,12,13] W1-3=[2,7,10,15] W1-4=[3,6,11,14].
//   - photo: the photo's exact slot placement (see photoSeed16).
func specFor16(cta, photo bool) Spec {
	var heats [][]int
	if photo {
		heats = photoSeed16
	} else {
		g := bracket.BalancedSeeding(seedNames(16), 4) // top seeds 1,2,3,4
		// Bracket-half order 1,4,2,3 so seeds 1 and 2 fall on opposite halves
		// (W1-1/W1-2 vs W1-3/W1-4) and only meet in the winners final.
		for _, idx := range []int{0, 3, 1, 2} {
			heats = append(heats, namesToSeeds(g[idx]))
		}
	}
	w1 := make([]SpecRace, 4)
	for i := 0; i < 4; i++ {
		w1[i] = SpecRace{
			ID:      fmt.Sprintf("W1-%d", i+1),
			Round:   1,
			Slot:    bracket.SlotWinners,
			Advance: 2,
			Sources: seedSources(heats[i]),
		}
	}
	ref := func(pos int, from string) SlotRef { return SlotRef{FromID: from, Position: pos} }

	body := []SpecRace{
		w1[0], w1[1], w1[2], w1[3],

		// Redemption Round 2 (fed by Round-1 3rd/4th place finishers).
		{ID: "L1-1", Round: 2, Slot: bracket.SlotLosers, Advance: 2, Sources: []SlotRef{
			ref(4, "W1-1"), ref(3, "W1-2"), ref(3, "W1-3"), ref(4, "W1-4")}},
		{ID: "L1-2", Round: 2, Slot: bracket.SlotLosers, Advance: 2, Sources: []SlotRef{
			ref(3, "W1-1"), ref(4, "W1-2"), ref(4, "W1-3"), ref(3, "W1-4")}},

		// Winners Round 3.
		{ID: "W2-1", Round: 3, Slot: bracket.SlotWinners, Advance: 2, Sources: []SlotRef{
			ref(1, "W1-1"), ref(2, "W1-1"), ref(1, "W1-2"), ref(2, "W1-2")}},
		{ID: "W2-2", Round: 3, Slot: bracket.SlotWinners, Advance: 2, Sources: []SlotRef{
			ref(1, "W1-3"), ref(2, "W1-3"), ref(1, "W1-4"), ref(2, "W1-4")}},

		// Redemption Round 4 (winners Round-3 3rd/4th + redemption Round-2 top-2).
		{ID: "L2-1", Round: 4, Slot: bracket.SlotLosers, Advance: 2, Sources: []SlotRef{
			ref(3, "W2-1"), ref(1, "L1-1"), ref(2, "L1-1"), ref(4, "W2-2")}},
		{ID: "L2-2", Round: 4, Slot: bracket.SlotLosers, Advance: 2, Sources: []SlotRef{
			ref(4, "W2-1"), ref(2, "L1-2"), ref(1, "L1-2"), ref(3, "W2-2")}},

		// Winners Round 5 — winners final, top-2 are winners-side finalists.
		{ID: "W3-1", Round: 5, Slot: bracket.SlotWinners, Advance: 2, Sources: []SlotRef{
			ref(1, "W2-1"), ref(2, "W2-1"), ref(1, "W2-2"), ref(2, "W2-2")}},

		// Redemption Round 6.
		{ID: "L3-1", Round: 6, Slot: bracket.SlotLosers, Advance: 2, Sources: []SlotRef{
			ref(1, "L2-1"), ref(2, "L2-1"), ref(1, "L2-2"), ref(2, "L2-2")}},

		// Redemption Round 7 — redemption final (winners-final 3rd/4th + Round-6 top-2).
		{ID: "L4-1", Round: 7, Slot: bracket.SlotLosers, Advance: 2, Sources: []SlotRef{
			ref(3, "W3-1"), ref(4, "W3-1"), ref(1, "L3-1"), ref(2, "L3-1")}},
	}

	return Spec{
		Body:           body,
		WinnersFinalID: "W3-1",
		LosersFinalID:  "L4-1",
		Finalists: []SlotRef{
			ref(1, "W3-1"), ref(2, "W3-1"), ref(1, "L4-1"), ref(2, "L4-1")},
		FinalRound: 8,
		CTA:        cta,
	}
}

// buildSpec selects the spec for a field. A 16-pilot 4-up/top-2 field is pinned
// to the MultiGP sheet (with selectable snake/photo round-1 seeding); everything
// else — every other size, and ANY other heat shape (a 6-up Nationals bracket)
// — uses the generic generator (always snake-separated).
func buildSpec(n int, cta, photoSeed bool, ppr, adv int) Spec {
	if n == 16 && ppr == specPPR && adv == specAdvance {
		return specFor16(cta, photoSeed)
	}
	return genericSpec(n, cta, ppr, adv)
}

// genericSpec builds a generic double-elimination → top-N qualifier for an
// arbitrary field size at ppr pilots per race with adv advancing (4/2 is the
// format's MultiGP standard; a 6-up Nationals bracket runs 6/3). It is not
// guaranteed to match MultiGP's published sheets for non-16 sizes, but always
// converges: a winners final and (when the field is big enough) a redemption
// final feed a single terminal final of up to ppr pilots — adv from each side —
// whose top-min(ppr,N) qualify.
func genericSpec(n int, cta bool, ppr, adv int) Spec {
	ppr, adv = normaliseShape(ppr, adv)
	seeds := seedNames(n)

	// Tiny field: one heat ranks everyone — that heat IS the qualifying final.
	if n <= ppr {
		final := SpecRace{
			ID: "W1-1", Round: 1, Slot: bracket.SlotGrandFinal,
			Advance: minI(ppr, n), Terminal: true, FinalTarget: minI(ppr, n),
			Sources: seedSources(rangeInts(1, n)),
		}
		return Spec{Body: []SpecRace{final}, WinnersFinalID: "W1-1", FinalRound: 1, CTA: cta}
	}

	var body []SpecRace
	add := func(sr SpecRace) { body = append(body, sr) }

	// ---- Winners bracket ----
	round := 1
	groups := bracket.BalancedSeeding(seeds, ppr)
	var prevIDs []string
	var prevSizes []int
	for i, g := range groups {
		id := fmt.Sprintf("W1-%d", i+1)
		add(SpecRace{ID: id, Round: 1, Slot: bracket.SlotWinners,
			Advance: minI(adv, len(g)), Sources: seedSources(namesToSeeds(g))})
		prevIDs = append(prevIDs, id)
		prevSizes = append(prevSizes, len(g))
	}
	// droppedByRound[r] = the loser refs (positions adv+1..size) produced by
	// winners round r — they fall into the losers bracket.
	droppedByRound := [][]SlotRef{loserRefs(prevIDs, prevSizes, adv)}

	wrnum := 1
	for len(prevIDs) > 1 {
		advRefs := advancerRefs(prevIDs, prevSizes, adv)
		wrnum++
		round += 2 // winners occupy odd columns; redemption interleaves on even
		chunks := chunkRefs(advRefs, ppr)
		var ids []string
		var sizes []int
		for i, ch := range chunks {
			id := fmt.Sprintf("W%d-%d", wrnum, i+1)
			add(SpecRace{ID: id, Round: round, Slot: bracket.SlotWinners,
				Advance: minI(adv, len(ch)), Sources: ch})
			ids = append(ids, id)
			sizes = append(sizes, len(ch))
		}
		prevIDs, prevSizes = ids, sizes
		droppedByRound = append(droppedByRound, loserRefs(ids, sizes, adv))
	}
	winnersFinalID := prevIDs[0]
	winnersFinalRound := round

	// ---- Losers (redemption) bracket ----
	//
	// Real double-elimination cadence — the one the 16-pilot sheet transcribes
	// and the elimination engine's losersRoundPlan follows. The first two intakes
	// run back to back (W1's drops, then the survivors + W2's drops); after that
	// the redemption side ALTERNATES a MINOR round, which halves the pool with
	// nobody new entering, and a MAJOR round that folds in the next winners
	// round's drops. The minor round is what makes the numbers meet: a major
	// round of k heats leaves 2k survivors, halving makes that k, and the next
	// winners round (half as many heats again) drops k — so every major round is
	// full heats. This generator used to fold the next drops straight in after
	// every round, so W3's two droppers landed on L2's four survivors and the
	// halving ran as two heats of three — the 13-pilot C Main got an R6-1 AND an
	// R6-2 where the sheet has one. Only the 16 field escaped, via specFor16.
	pool := append([]SlotRef(nil), droppedByRound[0]...)
	lround := 2
	lrnum := 0
	var lastIDs []string
	reduce := func() {
		var sizes []int
		lastIDs, sizes = emitLosersRound(add, &lrnum, lround, pool, ppr, adv)
		lround += 2
		pool = advancerRefs(lastIDs, sizes, adv)
	}
	for w := 1; w < len(droppedByRound); w++ {
		if w == 1 {
			if len(pool) > 1 { // first intake: W1's drops alone (a lone dropper waits)
				reduce()
			}
		} else if len(pool) > adv { // minor round: halve, nobody enters
			reduce()
		}
		pool = append(pool, droppedByRound[w]...)
		reduce() // major round: survivors + this winners round's drops
	}
	// A ragged field can leave the last major round as several heats — keep
	// halving until a single redemption final remains.
	for len(lastIDs) > 1 {
		reduce()
	}
	losersFinalID := ""
	if len(lastIDs) == 1 {
		losersFinalID = lastIDs[0]
	}

	// ---- Terminal final ----
	finalRound := winnersFinalRound
	if lround-2 > finalRound {
		finalRound = lround - 2
	}
	finalRound++

	// The terminal final takes adv from each side (top-2 + top-2 = the sheet's
	// 4; top-3 + top-3 = a 6-up final).
	var finalists []SlotRef
	if losersFinalID != "" {
		for p := 1; p <= adv; p++ {
			finalists = append(finalists, SlotRef{FromID: winnersFinalID, Position: p})
		}
		for p := 1; p <= adv; p++ {
			finalists = append(finalists, SlotRef{FromID: losersFinalID, Position: p})
		}
	} else {
		// No redemption final materialised — qualify the winners-final top-ppr.
		for p := 1; p <= ppr; p++ {
			finalists = append(finalists, SlotRef{FromID: winnersFinalID, Position: p})
		}
	}

	return Spec{
		Body:           body,
		WinnersFinalID: winnersFinalID,
		LosersFinalID:  losersFinalID,
		Finalists:      finalists,
		FinalRound:     finalRound,
		CTA:            cta,
	}
}

// emitLosersRound chunks the pool into redemption heats, appends them, and
// returns their ids + sizes. lrnum is bumped per round for stable L{n}-{i} ids.
func emitLosersRound(add func(SpecRace), lrnum *int, lround int, pool []SlotRef, ppr, adv int) ([]string, []int) {
	*lrnum++
	chunks := chunkRefs(pool, ppr)
	var ids []string
	var sizes []int
	for i, ch := range chunks {
		id := fmt.Sprintf("L%d-%d", *lrnum, i+1)
		add(SpecRace{ID: id, Round: lround, Slot: bracket.SlotLosers,
			Advance: minI(adv, len(ch)), Sources: ch})
		ids = append(ids, id)
		sizes = append(sizes, len(ch))
	}
	return ids, sizes
}

// advancerRefs lists every advancing slot (positions 1..adv, capped to each
// race's size) across a round's races, ordered position-major so the winners
// of different heats are spread before the runners-up when re-chunked.
func advancerRefs(ids []string, sizes []int, adv int) []SlotRef {
	var refs []SlotRef
	for p := 1; p <= adv; p++ {
		for i, id := range ids {
			if p <= sizes[i] {
				refs = append(refs, SlotRef{FromID: id, Position: p})
			}
		}
	}
	return refs
}

// loserRefs lists every eliminated slot (positions adv+1..size) across a
// round's races — the pilots that drop into the losers bracket.
func loserRefs(ids []string, sizes []int, adv int) []SlotRef {
	var refs []SlotRef
	for i, id := range ids {
		for p := adv + 1; p <= sizes[i]; p++ {
			refs = append(refs, SlotRef{FromID: id, Position: p})
		}
	}
	return refs
}

// chunkRefs splits refs into groups of at most size, evening out the last two
// groups so no heat is left with a single pilot when it can be avoided.
func chunkRefs(refs []SlotRef, size int) [][]SlotRef {
	if len(refs) == 0 {
		return nil
	}
	races := (len(refs) + size - 1) / size
	out := make([][]SlotRef, races)
	// Balanced fill: distribute round-robin so counts differ by at most 1.
	for i, r := range refs {
		out[i%races] = append(out[i%races], r)
	}
	return out
}

// normaliseShape applies the defaults and keeps the shape sane: 2..8 per race,
// at least one advancing and at least one dropping.
func normaliseShape(ppr, adv int) (int, int) {
	if ppr < 2 || ppr > 8 {
		ppr = DefaultPilotsPerRace
	}
	if adv < 1 {
		adv = DefaultAdvance
	}
	if adv >= ppr {
		adv = ppr - 1
	}
	return ppr, adv
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func rangeInts(lo, hi int) []int {
	out := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// seedNames returns ["1","2",…,"n"] — placeholder pilot names used to derive the
// deterministic Round-1 seeding via bracket.BalancedSeeding.
func seedNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%d", i+1)
	}
	return out
}

// namesToSeeds maps the seed-name groups back to integer seeds.
func namesToSeeds(names []string) []int {
	out := make([]int, len(names))
	for i, s := range names {
		var v int
		fmt.Sscanf(s, "%d", &v)
		out[i] = v
	}
	return out
}
