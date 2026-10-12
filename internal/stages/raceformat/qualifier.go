package raceformat

import (
	"encoding/json"
	"strconv"

	"github.com/bob9/dd-pits/internal/stages/bracket"
	"github.com/bob9/dd-pits/internal/stages/qualbracket"
)

// QualifierFormat wraps the qualbracket interpreter (the "Double Elimination —
// Top 4 Advance" format). Its persisted state is a bracket.State, so handlers
// reuse the elimination SVG grid by wrapping that state; the qualifier exposes
// the interpreter via Qual() for the spec-derived predicted-slot labels.
type QualifierFormat struct {
	q *qualbracket.Qual
}

func qualifierConfig(cfg Config) qualbracket.Config {
	return qualbracket.Config{
		PilotCount:      cfg.PilotCount,
		PilotNames:      cfg.PilotNames,
		CTA:             cfg.QualifierCTA,
		AceWinsRequired: cfg.AceWinsRequired,
		PhotoSeed:       cfg.QualifierPhotoSeed,
		PilotsPerRace:   cfg.PilotsPerRace,
		Advance:         cfg.PilotsToAdvance,
	}
}

func newQualifier(cfg Config) (*QualifierFormat, error) {
	q, err := qualbracket.New(qualifierConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &QualifierFormat{q: q}, nil
}

func qualifierFromState(cfg Config, raw json.RawMessage) (*QualifierFormat, error) {
	if len(raw) == 0 {
		return newQualifier(cfg)
	}
	st := &bracket.State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	q, err := qualbracket.FromState(qualifierConfig(cfg), st)
	if err != nil {
		return nil, err
	}
	return &QualifierFormat{q: q}, nil
}

// Qual returns the underlying interpreter for qualifier-specific rendering.
func (q *QualifierFormat) Qual() *qualbracket.Qual { return q.q }

func (q *QualifierFormat) Kind() FormatKind { return KindQualifier }

func (q *QualifierFormat) State() any { return q.q.State() }

func (q *QualifierFormat) RestoreState(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	st := &bracket.State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return err
	}
	nq, err := qualbracket.FromState(q.q.Cfg(), st)
	if err != nil {
		return err
	}
	q.q = nq
	return nil
}

func (q *QualifierFormat) ApplyPlacement(raceID string, placement []string) error {
	return q.q.ApplyPlacement(raceID, placement)
}

func (q *QualifierFormat) CanEditRace(raceID string) (bool, string) {
	return q.q.CanEditRace(raceID)
}

func (q *QualifierFormat) EditRace(raceID string, placement []string) error {
	return q.q.EditRace(raceID, placement)
}

func (q *QualifierFormat) ResetRace(raceID string) error {
	return q.q.ResetRace(raceID)
}

// Champion is empty by design — the qualifier crowns nobody; it produces a
// ranked top-4 that qualify onward (see Standings/Qualified).
func (q *QualifierFormat) Champion() string { return "" }

func (q *QualifierFormat) IsComplete() bool { return q.q.IsComplete() }

func (q *QualifierFormat) LayoutKind() string { return "bracket-grid" }

// Standings returns the ranked top-4 qualified pilots, each tagged "Qualified".
func (q *QualifierFormat) Standings() []Standing {
	qual := q.q.Qualified()
	out := make([]Standing, 0, len(qual))
	for i, p := range qual {
		out = append(out, Standing{Rank: i + 1, Pilot: p, Detail: "Qualified"})
	}
	return out
}

// Races projects the qualifier's heats with qualifier-specific titles.
func (q *QualifierFormat) Races() []RaceView {
	state := q.q.State()
	titles := q.raceTitles()
	out := make([]RaceView, 0, len(state.Races))
	for _, r := range state.Races {
		canEdit := false
		if r.IsComplete {
			canEdit, _ = q.q.CanEditRace(r.ID)
		}
		out = append(out, RaceView{
			ID:              r.ID,
			Round:           r.Round,
			GroupLabel:      qualSlotLabel(r.Bracket),
			Title:           titles[r.ID],
			Pilots:          append([]string(nil), r.Pilots...),
			Winners:         append([]string(nil), r.Winners...),
			Losers:          append([]string(nil), r.Losers...),
			IsComplete:      r.IsComplete,
			FullPlacement:   r.FullPlacement,
			IsVoid:          r.IsVoid,
			ExpectedWinners: q.q.ExpectedWinners(r),
			CanEdit:         canEdit,
		})
	}
	return out
}

// qualSlotLabel maps a slot to a qualifier display label. The terminal final
// (grand-final slot) reads "Final" since the qualifier has no grand final.
func qualSlotLabel(slot bracket.BracketSlot) string {
	switch slot {
	case bracket.SlotWinners:
		return "Winners"
	case bracket.SlotLosers:
		return "Redemption"
	case bracket.SlotGrandFinal:
		return "Final"
	case bracket.SlotChaseTheAce:
		return "Chase the Ace"
	default:
		return string(slot)
	}
}

// raceTitles builds a display title per race id (Winners/Redemption Round N —
// Race M, the top-4 Final, and chase-the-ace heats).
func (q *QualifierFormat) raceTitles() map[string]string {
	return qualRaceTitles(q.q.State())
}

// qualRaceTitles is the shared title builder for a qualifier bracket.State —
// reused by the mains-progression wrapper, which renders one qualifier per main.
func qualRaceTitles(state *bracket.State) map[string]string {
	type grp struct {
		slot  bracket.BracketSlot
		round int
	}
	counts := map[grp]int{}
	index := map[string]int{}
	for _, r := range state.Races {
		g := grp{r.Bracket, r.Round}
		counts[g]++
		index[r.ID] = counts[g]
	}
	titles := make(map[string]string, len(state.Races))
	for _, r := range state.Races {
		n := index[r.ID]
		multi := counts[grp{r.Bracket, r.Round}] > 1
		var base string
		switch r.Bracket {
		case bracket.SlotWinners:
			base = "Winners Round " + strconv.Itoa(r.Round)
		case bracket.SlotLosers:
			base = "Redemption Round " + strconv.Itoa(r.Round)
		case bracket.SlotGrandFinal:
			titles[r.ID] = "Final — Top 4 Qualify"
			continue
		case bracket.SlotChaseTheAce:
			titles[r.ID] = "Final (Chase the Ace) — Heat " + strconv.Itoa(n)
			continue
		default:
			base = qualSlotLabel(r.Bracket)
		}
		if multi {
			base += " — Race " + strconv.Itoa(n)
		}
		titles[r.ID] = base
	}
	return titles
}
