package raceformat

import (
	"encoding/json"
	"strconv"

	"github.com/bob9/dd-pits/internal/stages/bracket"
)

// EliminationFormat wraps the existing bracket engine whole. Every method
// delegates straight through, so the proven generation/advancement logic and the
// persisted bracket.State JSON shape are unchanged. Handlers that still need the
// concrete engine for elimination-specific rendering (the bracket grid, predicted
// slots, SVG flow) reach it via Bracket().
type EliminationFormat struct {
	b *bracket.Bracket
}

// eliminationConfig maps the superset Config onto the engine's Config.
func eliminationConfig(cfg Config) bracket.Config {
	return bracket.Config{
		PilotCount:      cfg.PilotCount,
		PilotNames:      cfg.PilotNames,
		EliminationType: bracket.EliminationType(cfg.EliminationType),
		PilotsPerRace:   cfg.PilotsPerRace,
		PilotsToAdvance: cfg.PilotsToAdvance,
		FinalsFormat:    bracket.FinalsFormat(cfg.FinalsFormat),
		AceWinsRequired: cfg.AceWinsRequired,
	}
}

func newElimination(cfg Config) (*EliminationFormat, error) {
	b, err := bracket.New(eliminationConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &EliminationFormat{b: b}, nil
}

func eliminationFromState(cfg Config, raw json.RawMessage) (*EliminationFormat, error) {
	if len(raw) == 0 {
		return newElimination(cfg)
	}
	st := &bracket.State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	b, err := bracket.FromState(eliminationConfig(cfg), st)
	if err != nil {
		return nil, err
	}
	return &EliminationFormat{b: b}, nil
}

// Bracket returns the underlying engine for elimination-specific rendering.
func (e *EliminationFormat) Bracket() *bracket.Bracket { return e.b }

func (e *EliminationFormat) Kind() FormatKind { return KindElimination }

func (e *EliminationFormat) State() any { return e.b.State() }

func (e *EliminationFormat) RestoreState(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	st := &bracket.State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return err
	}
	b, err := bracket.FromState(e.b.Config(), st)
	if err != nil {
		return err
	}
	e.b = b
	return nil
}

func (e *EliminationFormat) ApplyPlacement(raceID string, placement []string) error {
	return e.b.ApplyPlacement(raceID, placement)
}

func (e *EliminationFormat) CanEditRace(raceID string) (bool, string) {
	return e.b.CanEditRace(raceID)
}

func (e *EliminationFormat) EditRace(raceID string, placement []string) error {
	return e.b.EditRace(raceID, placement)
}

func (e *EliminationFormat) ResetRace(raceID string) error {
	return e.b.ResetRace(raceID)
}

func (e *EliminationFormat) Champion() string { return e.b.Champion() }

func (e *EliminationFormat) IsComplete() bool { return e.b.IsComplete() }

func (e *EliminationFormat) LayoutKind() string { return "bracket-grid" }

// slotLabel maps an engine BracketSlot to a display group label.
func slotLabel(slot bracket.BracketSlot) string {
	switch slot {
	case bracket.SlotWinners:
		return "Winners"
	case bracket.SlotLosers:
		return "Redemption"
	case bracket.SlotGrandFinal:
		return "Grand Final"
	case bracket.SlotChaseTheAce:
		return "Chase the Ace"
	default:
		return string(slot)
	}
}

// raceTitles builds a full display title per race id. Because single- and
// double-elimination use different race-id schemes (race-N / R%d-%d vs W%d-%d /
// L%d-%d) the round comes from the engine's Round field, and the race index
// within a (bracket, round) group is computed by enumeration rather than parsed
// out of the id (which can be "L4-extra"). Single elimination gets the friendlier
// Final / Semifinal / Quarterfinal names for its closing rounds.
func (e *EliminationFormat) raceTitles() map[string]string {
	state := e.b.State()
	type grp struct {
		slot  bracket.BracketSlot
		round int
	}
	counts := map[grp]int{}   // races per (slot, round)
	index := map[string]int{} // race id -> 1-based position within its group
	maxWinners := 0
	for _, r := range state.Races {
		g := grp{r.Bracket, r.Round}
		counts[g]++
		index[r.ID] = counts[g]
		if r.Bracket == bracket.SlotWinners && r.Round > maxWinners {
			maxWinners = r.Round
		}
	}
	single := e.b.Config().EliminationType == bracket.EliminationSingle
	titles := make(map[string]string, len(state.Races))
	for _, r := range state.Races {
		n := index[r.ID]
		multi := counts[grp{r.Bracket, r.Round}] > 1
		var base string
		switch r.Bracket {
		case bracket.SlotWinners:
			switch {
			case single && r.Round == maxWinners:
				base = "Final"
			case single && r.Round == maxWinners-1:
				base = "Semifinal"
			case single && r.Round == maxWinners-2:
				base = "Quarterfinal"
			case single:
				base = "Round " + strconv.Itoa(r.Round)
			default:
				base = "Winners Round " + strconv.Itoa(r.Round)
			}
		case bracket.SlotLosers:
			base = "Redemption Round " + strconv.Itoa(r.Round)
		case bracket.SlotGrandFinal:
			if r.ID == "GF-2" {
				titles[r.ID] = "Grand Final — Reset"
			} else {
				titles[r.ID] = "Grand Final"
			}
			continue
		case bracket.SlotChaseTheAce:
			titles[r.ID] = "Chase the Ace — Heat " + strconv.Itoa(n)
			continue
		default:
			base = slotLabel(r.Bracket)
		}
		if multi {
			base += " — Race " + strconv.Itoa(n)
		}
		titles[r.ID] = base
	}
	return titles
}

func (e *EliminationFormat) Races() []RaceView {
	state := e.b.State()
	titles := e.raceTitles()
	out := make([]RaceView, 0, len(state.Races))
	for _, r := range state.Races {
		canEdit := false
		if r.IsComplete {
			canEdit, _ = e.b.CanEditRace(r.ID)
		}
		out = append(out, RaceView{
			ID:              r.ID,
			Round:           r.Round,
			GroupLabel:      slotLabel(r.Bracket),
			Title:           titles[r.ID],
			Pilots:          append([]string(nil), r.Pilots...),
			Winners:         append([]string(nil), r.Winners...),
			Losers:          append([]string(nil), r.Losers...),
			IsComplete:      r.IsComplete,
			FullPlacement:   r.FullPlacement,
			IsVoid:          r.IsVoid,
			ExpectedWinners: e.b.ExpectedWinnersCount(r),
			CanEdit:         canEdit,
		})
	}
	return out
}

// Standings returns the chase-the-ace ranking when present, otherwise a
// champion-only list (elimination has no general running order).
func (e *EliminationFormat) Standings() []Standing {
	state := e.b.State()
	if cta := state.ChaseTheAce; cta != nil && cta.IsComplete {
		out := make([]Standing, 0, len(cta.Standings)+1)
		out = append(out, Standing{Rank: 1, Pilot: cta.Champion, Detail: aceDetail(cta, cta.Champion)})
		for i, p := range cta.Standings {
			out = append(out, Standing{Rank: i + 2, Pilot: p, Detail: aceDetail(cta, p)})
		}
		return out
	}
	if champ := e.b.Champion(); champ != "" {
		// When the deciding heat recorded a full finishing order, that order
		// IS the podium. Returning the champion alone left FinalOrdering to
		// rank everyone else in the final by seed: the 2026 Nationals Old Dogs
		// final ran Foxur, bob9, Voland, Clog wog but was published as
		// Foxur, Voland (seed 3), MacDaddy (seed 4).
		if final := deciderRace(state.Races, champ); final != nil && final.FullPlacement {
			out := make([]Standing, 0, len(final.Pilots))
			for i, p := range final.Pilots {
				out = append(out, Standing{Rank: i + 1, Pilot: p})
			}
			if len(out) > 0 && out[0].Pilot == champ {
				return out
			}
		}
		return []Standing{{Rank: 1, Pilot: champ}}
	}
	return nil
}

// deciderRace is the race that crowned champ: the latest-round complete,
// non-void heat the champion won.
func deciderRace(races []*bracket.Race, champ string) *bracket.Race {
	var best *bracket.Race
	for _, r := range races {
		if r == nil || !r.IsComplete || r.IsVoid || len(r.Winners) == 0 || r.Winners[0] != champ {
			continue
		}
		if best == nil || r.Round >= best.Round {
			best = r
		}
	}
	return best
}

func aceDetail(cta *bracket.ChaseTheAceState, pilot string) string {
	wins := cta.Wins[pilot]
	if wins == 1 {
		return "1 win"
	}
	return strconv.Itoa(wins) + " wins"
}
