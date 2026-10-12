package raceformat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// LadderFormat is a MultiGP-style seeded ladder (a.k.a. "regulation" / iron-man).
//
// The lowest seeds race first; the top `Advance` finishers of each bout carry
// forward and join the next-highest seeds for the following bout, narrowing until
// the survivors meet the top seeds in the final. Top seeds therefore get
// effective byes — seed 1 enters only in the last bout. Single-elimination: a
// pilot who doesn't advance is out.
//
// The whole ladder is pre-generated so every bout can be exported to FPVTrackside
// up front; each bout's fresh seeds are known immediately, and carry-over slots
// are filled as upstream bouts complete.
type LadderFormat struct {
	state *LadderState
}

// LadderState is the persisted state blob for a ladder.
type LadderState struct {
	Races         []*LadderRace `json:"races"`
	PilotsPerRace int           `json:"pilotsPerRace"`
	Advance       int           `json:"advance"`
}

// LadderRace is one bout in the ladder.
type LadderRace struct {
	ID            string   `json:"id"`    // "LR1".."LRk", lowest bout first
	Round         int      `json:"round"` // 1-based bout number
	Pilots        []string `json:"pilots"`
	Winners       []string `json:"winners"`
	Losers        []string `json:"losers"`
	IsComplete    bool     `json:"isComplete"`
	FullPlacement bool     `json:"fullPlacement"`
}

func newLadder(cfg Config) (*LadderFormat, error) {
	names := trimNames(cfg.PilotNames)
	p := cfg.PilotsPerRace
	a := cfg.PilotsToAdvance
	if len(names) < 2 {
		return nil, errors.New("ladder: need at least 2 pilots")
	}
	if p < 2 || p > 8 {
		return nil, errors.New("ladder: pilotsPerRace must be between 2 and 8")
	}
	if a < 1 || a >= p {
		return nil, errors.New("ladder: pilotsToAdvance must be ≥1 and < pilotsPerRace")
	}

	n := len(names)
	st := &LadderState{PilotsPerRace: p, Advance: a}

	// Consume fresh seeds from the bottom (worst qualifier) upward. Bout 1 takes
	// the lowest P seeds; each later bout reserves A carry-over slots and takes
	// the next (P-A) higher seeds.
	round := 0
	if n <= p {
		// Single final bout containing everyone.
		round++
		st.Races = append(st.Races, &LadderRace{
			ID:     "LR" + strconv.Itoa(round),
			Round:  round,
			Pilots: append([]string(nil), names...),
		})
		return &LadderFormat{state: st}, nil
	}

	hi := n - p // upper bound (exclusive) of the next fresh chunk in names
	round++
	st.Races = append(st.Races, &LadderRace{
		ID:     "LR" + strconv.Itoa(round),
		Round:  round,
		Pilots: append([]string(nil), names[n-p:n]...), // bottom P seeds
	})
	for hi > 0 {
		freshCount := p - a
		if freshCount > hi {
			freshCount = hi
		}
		fresh := append([]string(nil), names[hi-freshCount:hi]...)
		hi -= freshCount
		round++
		st.Races = append(st.Races, &LadderRace{
			ID:     "LR" + strconv.Itoa(round),
			Round:  round,
			Pilots: fresh, // carry-overs appended when the previous bout completes
		})
	}
	return &LadderFormat{state: st}, nil
}

func ladderFromState(cfg Config, raw json.RawMessage) (*LadderFormat, error) {
	if len(raw) == 0 {
		return newLadder(cfg)
	}
	st := &LadderState{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	return &LadderFormat{state: st}, nil
}

func (l *LadderFormat) Kind() FormatKind { return KindLadder }

func (l *LadderFormat) State() any { return l.state }

func (l *LadderFormat) RestoreState(raw json.RawMessage) error {
	st := &LadderState{}
	if err := json.Unmarshal(raw, st); err != nil {
		return err
	}
	l.state = st
	return nil
}

func (l *LadderFormat) LayoutKind() string { return "ladder-grid" }

// raceIndex returns the slice index of a bout by id, or -1.
func (l *LadderFormat) raceIndex(id string) int {
	for i, r := range l.state.Races {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func (l *LadderFormat) isFinal(idx int) bool { return idx == len(l.state.Races)-1 }

// advanceCount is how many pilots carry forward from a bout. The final bout
// crowns one champion.
func (l *LadderFormat) advanceCount(idx int) int {
	if l.isFinal(idx) {
		return 1
	}
	return l.state.Advance
}

// ready reports whether a bout has all its pilots (carry-overs delivered). Bout 0
// is always ready; later bouts wait for the previous bout to complete.
func (l *LadderFormat) ready(idx int) bool {
	return idx == 0 || l.state.Races[idx-1].IsComplete
}

func (l *LadderFormat) Races() []RaceView {
	out := make([]RaceView, 0, len(l.state.Races))
	for i, r := range l.state.Races {
		label := "Ladder Bout " + strconv.Itoa(r.Round)
		if l.isFinal(i) {
			label = "Final"
		}
		rv := RaceView{
			ID:              r.ID,
			Round:           r.Round,
			GroupLabel:      label,
			Title:           label, // "Ladder Bout 1" / "Final" — already self-contained
			Pilots:          append([]string(nil), r.Pilots...),
			Winners:         append([]string(nil), r.Winners...),
			Losers:          append([]string(nil), r.Losers...),
			IsComplete:      r.IsComplete,
			FullPlacement:   r.FullPlacement,
			ExpectedWinners: l.advanceCount(i),
		}
		if r.IsComplete {
			ok, _ := l.CanEditRace(r.ID)
			rv.CanEdit = ok
		} else if !l.ready(i) {
			rv.Locked = true
			rv.LockedReason = "waiting on " + l.state.Races[i-1].ID
		}
		out = append(out, rv)
	}
	return out
}

func (l *LadderFormat) ApplyPlacement(raceID string, placement []string) error {
	idx := l.raceIndex(raceID)
	if idx < 0 {
		return fmt.Errorf("ladder: race %s not found", raceID)
	}
	race := l.state.Races[idx]
	if race.IsComplete {
		return fmt.Errorf("ladder: race %s already complete (use edit)", raceID)
	}
	if !l.ready(idx) {
		return fmt.Errorf("ladder: race %s is waiting on upstream pilots", raceID)
	}
	if err := samePilotSet(race.Pilots, placement); err != nil {
		return fmt.Errorf("ladder: %s placement %w", raceID, err)
	}

	race.Pilots = append([]string(nil), placement...)
	race.FullPlacement = true
	a := l.advanceCount(idx)
	if a > len(placement) {
		a = len(placement)
	}
	race.Winners = append([]string(nil), placement[:a]...)
	race.Losers = append([]string(nil), placement[a:]...)
	race.IsComplete = true

	// Carry advancers into the next bout.
	if !l.isFinal(idx) {
		next := l.state.Races[idx+1]
		next.Pilots = append(next.Pilots, race.Winners...)
	}
	return nil
}

func (l *LadderFormat) CanEditRace(raceID string) (bool, string) {
	idx := l.raceIndex(raceID)
	if idx < 0 {
		return false, "race not found"
	}
	if !l.state.Races[idx].IsComplete {
		return true, ""
	}
	// Refuse once the bout that received these advancers has itself been run.
	if !l.isFinal(idx) && l.state.Races[idx+1].IsComplete {
		return false, "the next bout has already been run"
	}
	return true, ""
}

func (l *LadderFormat) EditRace(raceID string, placement []string) error {
	idx := l.raceIndex(raceID)
	if idx < 0 {
		return fmt.Errorf("ladder: race %s not found", raceID)
	}
	if ok, reason := l.CanEditRace(raceID); !ok {
		return fmt.Errorf("ladder: cannot edit %s: %s", raceID, reason)
	}
	race := l.state.Races[idx]
	// Rewind the advancers this bout pushed into the next (un-run) bout.
	if race.IsComplete && !l.isFinal(idx) {
		next := l.state.Races[idx+1]
		next.Pilots = removeAll(next.Pilots, race.Winners)
	}
	race.Winners = nil
	race.Losers = nil
	race.IsComplete = false
	race.FullPlacement = false
	return l.ApplyPlacement(raceID, placement)
}

// ResetRace un-runs a completed bout: rewinds the advancers it pushed into the
// next (un-run) bout and clears its own result so it becomes pending. Refused
// once the next bout has been run. No-op if not yet run.
func (l *LadderFormat) ResetRace(raceID string) error {
	idx := l.raceIndex(raceID)
	if idx < 0 {
		return fmt.Errorf("ladder: race %s not found", raceID)
	}
	if ok, reason := l.CanEditRace(raceID); !ok {
		return fmt.Errorf("ladder: cannot reset %s: %s", raceID, reason)
	}
	race := l.state.Races[idx]
	if !race.IsComplete {
		return nil
	}
	if !l.isFinal(idx) {
		next := l.state.Races[idx+1]
		next.Pilots = removeAll(next.Pilots, race.Winners)
	}
	race.Winners = nil
	race.Losers = nil
	race.IsComplete = false
	race.FullPlacement = false
	return nil
}

func (l *LadderFormat) Champion() string {
	if len(l.state.Races) == 0 {
		return ""
	}
	last := l.state.Races[len(l.state.Races)-1]
	if last.IsComplete && len(last.Winners) > 0 {
		return last.Winners[0]
	}
	return ""
}

func (l *LadderFormat) IsComplete() bool { return l.Champion() != "" }

// Standings ranks the final bout's finishers (champion first) then the pilots
// eliminated in each earlier bout, latest bout first (later eliminations rank
// higher) — placements only.
func (l *LadderFormat) Standings() []Standing {
	if !l.IsComplete() {
		return nil
	}
	races := l.state.Races
	out := make([]Standing, 0, len(races))
	rank := 0
	// Final bout, in placement order.
	final := races[len(races)-1]
	for _, p := range final.Pilots {
		rank++
		out = append(out, Standing{Rank: rank, Pilot: p, Detail: "Final"})
	}
	// Earlier bouts' eliminated pilots, latest first.
	for i := len(races) - 2; i >= 0; i-- {
		for _, p := range races[i].Losers {
			rank++
			out = append(out, Standing{Rank: rank, Pilot: p, Detail: "Bout " + strconv.Itoa(races[i].Round)})
		}
	}
	return out
}
