package raceformat

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MainsFormat is a points-based A/B/C… mains format. Pilots are split by seed
// into ordered mains (A = top tier); each main runs a fixed number of rounds;
// per-placement points are summed across rounds and pilots are ranked by total.
// Rounds are independent races — no pilot ever moves between them; only the
// points accumulation links them. Scoring can be descending (high points = best,
// 1st earns the most) or ascending (low points = best, F1-style position points).
type MainsFormat struct {
	state *MainsState
}

// MainsConfig holds the mains setup parameters (persisted in format_config).
type MainsConfig struct {
	Mains       []string    `json:"mains"`       // ordered, A = top tier
	Rounds      int         `json:"rounds"`      // races each main runs
	Direction   string      `json:"direction"`   // "ascending" | "descending"
	PointsTable map[int]int `json:"pointsTable"` // finish position (1-based) → points
	// DropWorst discounts each pilot's N worst-scoring rounds from their total
	// (0 = count every round). Always keeps at least one round.
	DropWorst int `json:"dropWorst"`
	// Rosters is the explicit pilot→main assignment captured at creation (empty
	// when pilots were auto-chunked by seed). Kept for reference/regeneration.
	Rosters map[string][]string `json:"rosters,omitempty"`
	// Divisions optionally splits the mains into independent competitions, each
	// with its own champion (the winner of its first/top main). Empty means a
	// single competition over all mains (the A-main winner is the overall
	// champion). All divisions share the same rounds / points / direction.
	Divisions []MainsDivision `json:"divisions,omitempty"`
}

// MainsDivision is a named group of mains scored as one competition. The first
// main listed is the division's top main, whose winner is the division champion.
type MainsDivision struct {
	Name  string   `json:"name"`
	Mains []string `json:"mains"`
	// Skills optionally records the skill levels this division was filtered to at
	// creation (e.g. ["Pro"] or ["Intermediate","Novice"]). It's informational —
	// the setup form does the skill-based pilot distribution and submits the
	// resulting rosters; this is kept so the division can show its skill tag.
	// Empty = no skill filter was applied.
	Skills []string `json:"skills,omitempty"`
	// ClassID optionally links the division to a championship class whose
	// season-long pilot roster (championship_class_pilots) drove the pilot
	// distribution at creation. Like Skills it is informational here — the setup
	// form distributes and submits the resulting rosters — but the finalise flow
	// uses it to pre-select the matching championship + class for the division's
	// results. 0 = no class link.
	ClassID int `json:"classId,omitempty"`
}

// MainsState is the persisted state blob for a mains format.
type MainsState struct {
	Cfg   MainsConfig `json:"cfg"`
	Mains []*Main     `json:"mains"`
	// SeedOrder is the full grid order (seed 1 first). Tie-breaks use it so a
	// pilot manually placed in a main still ranks by their global seed. Empty on
	// brackets created before explicit assignment — falls back to in-main order.
	SeedOrder []string `json:"seedOrder,omitempty"`
}

// Main is one skill tier with its roster and per-round races.
type Main struct {
	Label  string       `json:"label"`
	Pilots []string     `json:"pilots"`
	Races  []*MainsRace `json:"races"`
}

// MainsRace is one round within a main.
type MainsRace struct {
	ID         string   `json:"id"` // "A-R1"
	Round      int      `json:"round"`
	Pilots     []string `json:"pilots"`
	Placement  []string `json:"placement"`
	IsComplete bool     `json:"isComplete"`
}

const (
	mainsAscending  = "ascending"
	mainsDescending = "descending"
)

func newMains(cfg Config) (*MainsFormat, error) {
	names := trimNames(cfg.PilotNames)
	mc := MainsConfig{
		Mains:       cfg.Mains,
		Rounds:      cfg.Rounds,
		Direction:   cfg.Direction,
		PointsTable: cfg.PointsTable,
		DropWorst:   cfg.DropWorst,
		Rosters:     cfg.MainsRosters,
		Divisions:   cfg.MainsDivisions,
	}
	if err := validateMainsConfig(&mc, len(names)); err != nil {
		return nil, err
	}

	seedIdx := map[string]int{}
	for i, n := range names {
		seedIdx[n] = i
	}
	rosters, err := mainsRosters(mc.Mains, names, cfg.MainsRosters, seedIdx)
	if err != nil {
		return nil, err
	}

	st := &MainsState{Cfg: mc, SeedOrder: append([]string(nil), names...)}
	for _, label := range mc.Mains {
		roster := rosters[label]
		m := &Main{Label: label, Pilots: roster}
		for rnd := 1; rnd <= mc.Rounds; rnd++ {
			m.Races = append(m.Races, &MainsRace{
				ID:     label + "-R" + strconv.Itoa(rnd),
				Round:  rnd,
				Pilots: append([]string(nil), roster...),
			})
		}
		st.Mains = append(st.Mains, m)
	}
	return &MainsFormat{state: st}, nil
}

func validateMainsConfig(mc *MainsConfig, pilotCount int) error {
	mains := make([]string, 0, len(mc.Mains))
	for _, m := range mc.Mains {
		if t := strings.TrimSpace(m); t != "" {
			mains = append(mains, t)
		}
	}
	if len(mains) < 1 {
		return errors.New("mains: need at least 1 main")
	}
	if pilotCount < len(mains) {
		return fmt.Errorf("mains: need at least one pilot per main (%d pilots, %d mains)", pilotCount, len(mains))
	}
	mc.Mains = mains
	if mc.Rounds < 1 {
		return errors.New("mains: need at least 1 round")
	}
	if mc.Direction != mainsAscending && mc.Direction != mainsDescending {
		return errors.New("mains: direction must be \"ascending\" or \"descending\"")
	}
	if mc.DropWorst < 0 {
		mc.DropWorst = 0
	}
	if mc.DropWorst >= mc.Rounds {
		return fmt.Errorf("mains: drop-worst (%d) must be fewer than the number of rounds (%d)", mc.DropWorst, mc.Rounds)
	}
	if len(mc.PointsTable) == 0 {
		return errors.New("mains: a points table is required")
	}
	if err := validateDivisions(mc); err != nil {
		return err
	}
	return nil
}

// validateDivisions checks (and normalises) the optional division grouping: every
// division must hold at least one known main, no main may appear twice, and the
// grouping must cover every main exactly once. Empty Divisions is valid (single
// competition over all mains).
func validateDivisions(mc *MainsConfig) error {
	if len(mc.Divisions) == 0 {
		return nil
	}
	valid := map[string]bool{}
	for _, l := range mc.Mains {
		valid[l] = true
	}
	seen := map[string]string{} // main → division name
	cleaned := make([]MainsDivision, 0, len(mc.Divisions))
	for _, d := range mc.Divisions {
		name := strings.TrimSpace(d.Name)
		dm := make([]string, 0, len(d.Mains))
		for _, l := range d.Mains {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			if !valid[l] {
				return fmt.Errorf("mains: division %q references unknown main %q", name, l)
			}
			if prev, dup := seen[l]; dup {
				return fmt.Errorf("mains: main %q is assigned to both %q and %q", l, prev, name)
			}
			seen[l] = name
			dm = append(dm, l)
		}
		if len(dm) == 0 {
			return fmt.Errorf("mains: division %q has no mains", name)
		}
		var skills []string
		for _, s := range d.Skills {
			if t := strings.TrimSpace(s); t != "" {
				skills = append(skills, t)
			}
		}
		cleaned = append(cleaned, MainsDivision{Name: name, Mains: dm, Skills: skills, ClassID: d.ClassID})
	}
	if len(seen) != len(mc.Mains) {
		return fmt.Errorf("mains: divisions cover %d of %d mains — every main must be in exactly one division", len(seen), len(mc.Mains))
	}
	mc.Divisions = cleaned
	return nil
}

// mainSizes splits n pilots across m mains as evenly as possible, with the last
// (lowest) mains absorbing any remainder.
func mainSizes(n, m int) []int {
	base := n / m
	rem := n % m
	sizes := make([]int, m)
	for i := range sizes {
		sizes[i] = base
		if i >= m-rem {
			sizes[i]++
		}
	}
	return sizes
}

// mainsRosters resolves each main's pilot roster. When an explicit assignment is
// supplied (drag-drop in the setup form) it is used verbatim after validating it
// covers every pilot exactly once; otherwise pilots are chunked by seed (lower
// mains absorb the remainder). Each roster is ordered by global seed.
func mainsRosters(labels, names []string, explicit map[string][]string, seedIdx map[string]int) (map[string][]string, error) {
	out := map[string][]string{}
	if len(explicit) > 0 {
		assigned := map[string]string{}
		for _, label := range labels {
			for _, p := range explicit[label] {
				if _, ok := seedIdx[p]; !ok {
					return nil, fmt.Errorf("mains: assignment references unknown pilot %q", p)
				}
				if prev, dup := assigned[p]; dup {
					return nil, fmt.Errorf("mains: pilot %q assigned to both %s and %s", p, prev, label)
				}
				assigned[p] = label
			}
		}
		if len(assigned) != len(names) {
			return nil, fmt.Errorf("mains: assignment covers %d of %d pilots", len(assigned), len(names))
		}
		for _, label := range labels {
			roster := append([]string(nil), explicit[label]...)
			sort.SliceStable(roster, func(i, j int) bool { return seedIdx[roster[i]] < seedIdx[roster[j]] })
			if len(roster) < 1 {
				return nil, fmt.Errorf("mains: %s main has no pilots", label)
			}
			out[label] = roster
		}
		return out, nil
	}
	sizes := mainSizes(len(names), len(labels))
	cursor := 0
	for i, label := range labels {
		out[label] = append([]string(nil), names[cursor:cursor+sizes[i]]...)
		cursor += sizes[i]
	}
	return out, nil
}

func mainsFromState(cfg Config, raw json.RawMessage) (*MainsFormat, error) {
	if len(raw) == 0 {
		return newMains(cfg)
	}
	st := &MainsState{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	return &MainsFormat{state: st}, nil
}

func (m *MainsFormat) Kind() FormatKind   { return KindMains }
func (m *MainsFormat) State() any         { return m.state }
func (m *MainsFormat) LayoutKind() string { return "mains-table" }

func (m *MainsFormat) RestoreState(raw json.RawMessage) error {
	st := &MainsState{}
	if err := json.Unmarshal(raw, st); err != nil {
		return err
	}
	m.state = st
	return nil
}

// findRace returns the race and its owning main by id.
func (m *MainsFormat) findRace(id string) (*MainsRace, *Main) {
	for _, main := range m.state.Mains {
		for _, r := range main.Races {
			if r.ID == id {
				return r, main
			}
		}
	}
	return nil, nil
}

func (m *MainsFormat) Races() []RaceView {
	// Map each main label to its (optional, named) division so titles can be
	// prefixed with the division when a day runs more than one competition.
	divByMain := map[string]string{}
	for _, d := range m.resolveDivisions() {
		for _, label := range d.Mains {
			divByMain[label] = d.Name
		}
	}
	out := []RaceView{}
	for _, main := range m.state.Mains {
		for _, r := range main.Races {
			pilots := r.Pilots
			if r.IsComplete && len(r.Placement) > 0 {
				pilots = r.Placement
			}
			title := main.Label + " Main — Round " + strconv.Itoa(r.Round)
			if div := divByMain[main.Label]; div != "" {
				title = div + " · " + title
			}
			out = append(out, RaceView{
				ID:              r.ID,
				Round:           r.Round,
				GroupLabel:      main.Label + " Main · Round " + strconv.Itoa(r.Round),
				Title:           title,
				Pilots:          append([]string(nil), pilots...),
				IsComplete:      r.IsComplete,
				FullPlacement:   r.IsComplete,
				ExpectedWinners: len(r.Pilots), // rank everyone
				CanEdit:         r.IsComplete,  // rounds are independent — always editable
			})
		}
	}
	return out
}

func (m *MainsFormat) ApplyPlacement(raceID string, placement []string) error {
	r, _ := m.findRace(raceID)
	if r == nil {
		return fmt.Errorf("mains: race %s not found", raceID)
	}
	if r.IsComplete {
		return fmt.Errorf("mains: race %s already complete (use edit)", raceID)
	}
	if err := samePilotSet(r.Pilots, placement); err != nil {
		return fmt.Errorf("mains: %s placement %w", raceID, err)
	}
	r.Placement = append([]string(nil), placement...)
	r.IsComplete = true
	return nil
}

// CanEditRace is always allowed for mains — rounds have no downstream dependency.
func (m *MainsFormat) CanEditRace(raceID string) (bool, string) {
	if r, _ := m.findRace(raceID); r == nil {
		return false, "race not found"
	}
	return true, ""
}

func (m *MainsFormat) EditRace(raceID string, placement []string) error {
	r, _ := m.findRace(raceID)
	if r == nil {
		return fmt.Errorf("mains: race %s not found", raceID)
	}
	if err := samePilotSet(r.Pilots, placement); err != nil {
		return fmt.Errorf("mains: %s placement %w", raceID, err)
	}
	r.Placement = append([]string(nil), placement...)
	r.IsComplete = true
	return nil
}

// ResetRace un-runs a completed round, clearing its placement/points so it
// becomes pending. Mains rounds have no downstream dependency. No-op if not run.
func (m *MainsFormat) ResetRace(raceID string) error {
	r, _ := m.findRace(raceID)
	if r == nil {
		return fmt.Errorf("mains: race %s not found", raceID)
	}
	r.Placement = nil
	r.IsComplete = false
	return nil
}

// roundScore is one completed round's result for a pilot.
type roundScore struct {
	round int
	pts   int
	pos   int // finishing position that round (1-based)
}

// pilotStats holds a pilot's accumulated scoring within a main.
type pilotStats struct {
	pilot   string
	rounds  []roundScore // completed rounds, in round order
	total   int          // counted total after drop-worst
	dropped map[int]bool // round numbers discounted by drop-worst
	seedIdx int          // seeding order — the sole tie-break
}

func (m *MainsFormat) points(pos int) int { return m.state.Cfg.PointsTable[pos] }

// rankedStats computes each pilot's accumulated scoring within one main and
// returns them in finishing order (best first): counted points (worst N rounds
// dropped), ties broken by seeding order. Shared by rankMain (Detail strings) and
// MainScoreboard (structured per-round cells) so they can't drift.
func (m *MainsFormat) rankedStats(main *Main) []*pilotStats {
	return m.rankedStatsThrough(main, 0)
}

// rankedStatsThrough is rankedStats counting only completed rounds with
// round <= maxRound (maxRound <= 0 = every completed round). It backs the
// round-over-round position-movement deltas on the OBS scoreboard overlays: the
// drop-worst maths is re-run over the capped round set so the prior standings are
// exactly what they were before the latest round, not an approximation.
func (m *MainsFormat) rankedStatsThrough(main *Main, maxRound int) []*pilotStats {
	asc := m.state.Cfg.Direction == mainsAscending
	// Tie-break seed = global grid position (so a manually-placed pilot still
	// ranks by their real seed). Falls back to in-main order for legacy state.
	seed := map[string]int{}
	for i, n := range m.state.SeedOrder {
		seed[n] = i
	}
	stats := map[string]*pilotStats{}
	for i, p := range main.Pilots {
		si, ok := seed[p]
		if !ok {
			si = i
		}
		stats[p] = &pilotStats{pilot: p, seedIdx: si, dropped: map[int]bool{}}
	}
	for _, r := range main.Races {
		if !r.IsComplete {
			continue
		}
		if maxRound > 0 && r.Round > maxRound {
			continue
		}
		for pos, p := range r.Placement {
			if s := stats[p]; s != nil {
				s.rounds = append(s.rounds, roundScore{round: r.Round, pts: m.points(pos + 1), pos: pos + 1})
			}
		}
	}
	for _, s := range stats {
		m.applyDrops(s, asc)
	}

	ranked := make([]*pilotStats, 0, len(stats))
	for _, p := range main.Pilots {
		ranked = append(ranked, stats[p])
	}
	sort.SliceStable(ranked, func(i, j int) bool { return m.compareStats(ranked[i], ranked[j], asc) < 0 })
	return ranked
}

// rankMain computes the ordered standings within one main: counted points
// (worst N rounds dropped), ties broken by seeding order. The slice is in
// finishing order (best first).
func (m *MainsFormat) rankMain(main *Main) []Standing {
	ranked := m.rankedStats(main)
	out := make([]Standing, len(ranked))
	for i, s := range ranked {
		out[i] = Standing{Pilot: s.pilot, Detail: m.detail(s)}
	}
	return out
}

// applyDrops discounts the pilot's worst DropWorst completed rounds (capped so at
// least one round always counts) and sets the counted total. "Worst" = lowest
// points when high-is-best (descending), highest points when low-is-best (ascending).
func (m *MainsFormat) applyDrops(s *pilotStats, asc bool) {
	s.total = 0
	for _, rs := range s.rounds {
		s.total += rs.pts
	}
	drop := m.state.Cfg.DropWorst
	if max := len(s.rounds) - 1; drop > max {
		drop = max
	}
	if drop <= 0 {
		return
	}
	order := append([]roundScore(nil), s.rounds...)
	sort.SliceStable(order, func(i, j int) bool {
		if asc {
			return order[i].pts > order[j].pts // ascending scoring: worst = highest points
		}
		return order[i].pts < order[j].pts // descending scoring: worst = lowest points
	})
	for i := 0; i < drop; i++ {
		s.total -= order[i].pts
		s.dropped[order[i].round] = true
	}
}

// compareStats returns <0 if a ranks ahead of b, else >0. Counted points decide
// (per direction); ties are broken purely by seeding order (lower seed index
// first), which is unique per pilot so there are never unresolved ties.
func (m *MainsFormat) compareStats(a, b *pilotStats, asc bool) int {
	if a.total != b.total {
		if asc {
			return a.total - b.total // lower is better
		}
		return b.total - a.total // higher is better
	}
	return a.seedIdx - b.seedIdx // tie → higher seed (lower index) wins
}

func (m *MainsFormat) detail(s *pilotStats) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(s.total))
	b.WriteString(" pts")
	if len(s.rounds) > 0 {
		b.WriteString(" (")
		for i, rs := range s.rounds {
			if i > 0 {
				b.WriteString(" ")
			}
			if s.dropped[rs.round] {
				b.WriteString("✗") // discounted by drop-worst
			}
			b.WriteString("R")
			b.WriteString(strconv.Itoa(rs.round))
			b.WriteString(":")
			b.WriteString(strconv.Itoa(rs.pts))
		}
		b.WriteString(")")
	}
	return b.String()
}

// resolveDivisions returns the effective division grouping. When none are
// configured, all mains form one unnamed division (the single-competition case),
// preserving the original A-main-wins-overall behaviour.
func (m *MainsFormat) resolveDivisions() []MainsDivision {
	if len(m.state.Cfg.Divisions) > 0 {
		return m.state.Cfg.Divisions
	}
	labels := make([]string, len(m.state.Mains))
	for i, main := range m.state.Mains {
		labels[i] = main.Label
	}
	return []MainsDivision{{Name: "", Mains: labels}}
}

func (m *MainsFormat) mainByLabel(label string) *Main {
	for _, main := range m.state.Mains {
		if main.Label == label {
			return main
		}
	}
	return nil
}

// Standings concatenates each main's ranking (top main first), grouped by
// division order, with a continuous global rank. Pilots in lower mains always
// rank below the main above.
func (m *MainsFormat) Standings() []Standing {
	out := []Standing{}
	rank := 0
	for _, d := range m.resolveDivisions() {
		for _, label := range d.Mains {
			main := m.mainByLabel(label)
			if main == nil {
				continue
			}
			for _, s := range m.rankMain(main) {
				rank++
				s.Rank = rank
				out = append(out, s)
			}
		}
	}
	return out
}

// MainStandings returns the per-main rankings for table rendering, in main order.
type MainStandings struct {
	Label     string
	Standings []Standing
}

func (m *MainsFormat) MainStandings() []MainStandings {
	out := make([]MainStandings, 0, len(m.state.Mains))
	for _, main := range m.state.Mains {
		out = append(out, MainStandings{Label: main.Label, Standings: m.rankMain(main)})
	}
	return out
}

// MainRoundCell is one round's scoring for a pilot in the per-main scoreboard.
// HasResult is false for rounds not yet run (rendered as a blank cell); Dropped
// marks a round discounted by drop-worst; Position is the pilot's finishing place
// that round (1-based) — the overlay golds a cell when Position == 1 (a round win).
type MainRoundCell struct {
	Round     int
	Points    int
	Position  int
	HasResult bool
	Dropped   bool
}

// MainScoreRow is one pilot's row in the per-main scoreboard. Cells always has
// exactly MainScoreboard.Rounds entries (index 0 = round 1).
type MainScoreRow struct {
	Pilot string
	Total int
	Cells []MainRoundCell
}

// MainScoreboard is a single main's full points table — per-round point cells
// plus the counted total — used by the OBS overlay. Rows are in finishing order
// (winner first).
type MainScoreboard struct {
	Label  string
	Rounds int
	Rows   []MainScoreRow
}

// MainScoreboard returns the structured scoreboard for one main. An empty label
// selects the first main; ok is false when label names no existing main.
func (m *MainsFormat) MainScoreboard(label string) (MainScoreboard, bool) {
	var main *Main
	if label == "" {
		if len(m.state.Mains) > 0 {
			main = m.state.Mains[0]
		}
	} else {
		main = m.mainByLabel(label)
	}
	if main == nil {
		return MainScoreboard{}, false
	}
	rounds := m.state.Cfg.Rounds
	sb := MainScoreboard{Label: main.Label, Rounds: rounds}
	for _, s := range m.rankedStats(main) {
		row := MainScoreRow{Pilot: s.pilot, Total: s.total, Cells: make([]MainRoundCell, rounds)}
		for i := range row.Cells {
			row.Cells[i] = MainRoundCell{Round: i + 1}
		}
		for _, rs := range s.rounds {
			if rs.round >= 1 && rs.round <= rounds {
				row.Cells[rs.round-1] = MainRoundCell{
					Round:     rs.round,
					Points:    rs.pts,
					Position:  rs.pos,
					HasResult: true,
					Dropped:   s.dropped[rs.round],
				}
			}
		}
		sb.Rows = append(sb.Rows, row)
	}
	return sb, true
}

// MainRanksThrough returns each pilot's 1-based finishing rank within a main
// counting only completed rounds up to and including maxRound (maxRound <= 0 =
// all rounds). Only pilots with at least one counted round in that window appear,
// so a pilot who first scored after maxRound is absent (the OBS overlay treats
// that as "new"). It powers the scoreboards' round-over-round movement arrows:
// compare the live rank to MainRanksThrough(label, latestRound-1). An empty label
// selects the first main; nil when the label names no main.
func (m *MainsFormat) MainRanksThrough(label string, maxRound int) map[string]int {
	var main *Main
	if label == "" {
		if len(m.state.Mains) > 0 {
			main = m.state.Mains[0]
		}
	} else {
		main = m.mainByLabel(label)
	}
	if main == nil {
		return nil
	}
	ranked := m.rankedStatsThrough(main, maxRound)
	out := make(map[string]int, len(ranked))
	rank := 0
	for _, s := range ranked {
		if len(s.rounds) == 0 {
			continue // no counted round in this window — not yet ranked
		}
		rank++
		out[s.pilot] = rank
	}
	return out
}

// DivisionStandings groups the per-main scoreboards by division and reports each
// division's champion (winner of its top main, once that main's rounds are all
// recorded) and completion. With no configured divisions this returns a single
// entry with Name "" covering every main.
type DivisionStandings struct {
	Name     string
	ClassID  int // linked championship class (0 = none) — see MainsDivision.ClassID
	Champion string
	Complete bool
	Mains    []MainStandings
}

func (m *MainsFormat) DivisionStandings() []DivisionStandings {
	divs := m.resolveDivisions()
	out := make([]DivisionStandings, 0, len(divs))
	for _, d := range divs {
		ds := DivisionStandings{Name: d.Name, ClassID: d.ClassID, Complete: true}
		for i, label := range d.Mains {
			main := m.mainByLabel(label)
			if main == nil {
				continue
			}
			ranked := m.rankMain(main)
			ds.Mains = append(ds.Mains, MainStandings{Label: main.Label, Standings: ranked})
			complete := mainComplete(main)
			if !complete {
				ds.Complete = false
			}
			if i == 0 && complete && len(ranked) > 0 { // top main decides the champion
				ds.Champion = ranked[0].Pilot
			}
		}
		out = append(out, ds)
	}
	return out
}

// mainComplete reports whether every round of a main has been recorded.
func mainComplete(main *Main) bool {
	for _, r := range main.Races {
		if !r.IsComplete {
			return false
		}
	}
	return true
}

// Champion is the overall winner. It exists only for the single-competition case
// (no configured divisions, or a lone division) — the winner of the top main once
// all its rounds are complete. With multiple divisions there is no single overall
// champion (each division has its own — see DivisionStandings).
func (m *MainsFormat) Champion() string {
	divs := m.resolveDivisions()
	if len(divs) != 1 || len(divs[0].Mains) == 0 {
		return ""
	}
	top := m.mainByLabel(divs[0].Mains[0])
	if top == nil || !mainComplete(top) {
		return ""
	}
	ranked := m.rankMain(top)
	if len(ranked) == 0 {
		return ""
	}
	return ranked[0].Pilot
}

// IsComplete is true once every round of every main has been recorded.
func (m *MainsFormat) IsComplete() bool {
	for _, main := range m.state.Mains {
		for _, r := range main.Races {
			if !r.IsComplete {
				return false
			}
		}
	}
	return true
}
