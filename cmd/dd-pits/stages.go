package main

// Stages: brackets and other multi-race competitions, run from dd-pits.
//
// A stage is a set of races run as one competition: double elimination, the
// MultiGP top-4 qualifier, bracket progression, ladders and points brackets.
// The draw and progression come from internal/stages, the same engine the
// AUFPV platform and bob9's FPVTrackside "Stages" use, so a stage set up here
// produces exactly the races those would.
//
// dd-pits owns the stage; the timer only flies the races. Each race is sent to
// the timer once its whole line-up is known (as a RotorHazard heat, or an
// FPVTrackside race through its REST API), and when that race ends the
// finishing order read from the timing advances the stage. A result can also
// be entered by hand, which locks it against timing.
//
// Stages are kept in one JSON file beside the settings file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/stages/raceformat"
)

// stageSetup is the format options, with the same JSON names as the AUFPV
// platform's format_config and FPVTrackside's StagePlans.json, so a stage's
// settings read the same everywhere.
type stageSetup struct {
	EliminationType string              `json:"eliminationType,omitempty"`
	FinalsFormat    string              `json:"finalsFormat,omitempty"`
	PilotsPerRace   int                 `json:"pilotsPerRace,omitempty"`
	PilotsToAdvance int                 `json:"pilotsToAdvance,omitempty"`
	AceWinsRequired int                 `json:"aceWinsRequired,omitempty"`
	QualifierCTA    bool                `json:"qualifierCta,omitempty"`
	QualifierSeed   string              `json:"qualifierSeed,omitempty"` // "snake" | "photo"
	PilotsPerMain   int                 `json:"pilotsPerMain,omitempty"`
	BumpUp          int                 `json:"bumpUp,omitempty"`
	CTAMains        []string            `json:"ctaMains,omitempty"`
	Mains           int                 `json:"mains,omitempty"`
	Rounds          int                 `json:"rounds,omitempty"`
	Direction       string              `json:"direction,omitempty"`
	PointsTable     []int               `json:"pointsTable,omitempty"`
	DropWorst       int                 `json:"dropWorst,omitempty"`
	MainsRosters    map[string][]string `json:"mainsRosters,omitempty"`

	// The race itself, for the timer: laps to win (0 = the timer's own
	// format) and the race clock in seconds (0 = no limit / the timer's).
	Laps        int `json:"laps,omitempty"`
	RaceSeconds int `json:"raceSeconds,omitempty"`
}

// stageRecord is one stage as stored.
type stageRecord struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Format  raceformat.FormatKind `json:"format"`
	Setup   stageSetup            `json:"setup"`
	Pilots  []string              `json:"pilots"` // seed order, seed 1 first
	State   json.RawMessage       `json:"state"`
	Created time.Time             `json:"created"`

	// Links ties a stage race to the race the timer flies it as: an
	// FPVTrackside race id, or "rh-h<heat>" for a RotorHazard heat.
	Links map[string]string `json:"links,omitempty"`
	// Ext numbers each stage race for FPVTrackside's externalId.
	Ext map[string]int `json:"ext,omitempty"`
	// Locked results were entered by hand; timing never overwrites them.
	Locked map[string]bool `json:"locked,omitempty"`
	// Applied is the finishing order last taken from timing for each race, so
	// an unchanged result is not applied twice.
	Applied map[string]string `json:"applied,omitempty"`
	// RerunAfter holds, per race being flown again, when the re-run was asked
	// for (epoch ms): runs that started before it no longer count.
	RerunAfter map[string]int64 `json:"rerunAfter,omitempty"`

	AutoSend    bool `json:"autoSend"`
	AutoResults bool `json:"autoResults"`

	// Base is this stage's block of FPVTrackside externalIds.
	Base int `json:"base"`

	// LastError is the most recent send or result problem, shown on the page.
	LastError string `json:"lastError,omitempty"`
}

func (st *stageRecord) config() raceformat.Config {
	s := st.Setup
	cfg := raceformat.Config{
		PilotCount:         len(st.Pilots),
		PilotNames:         append([]string(nil), st.Pilots...),
		EliminationType:    s.EliminationType,
		PilotsPerRace:      s.PilotsPerRace,
		PilotsToAdvance:    s.PilotsToAdvance,
		FinalsFormat:       s.FinalsFormat,
		AceWinsRequired:    s.AceWinsRequired,
		Rounds:             s.Rounds,
		Direction:          s.Direction,
		DropWorst:          s.DropWorst,
		MainsRosters:       s.MainsRosters,
		QualifierCTA:       s.QualifierCTA,
		QualifierPhotoSeed: s.QualifierSeed == "photo",
		PilotsPerMain:      s.PilotsPerMain,
		BumpUp:             s.BumpUp,
		CTAMains:           s.CTAMains,
	}
	// Points brackets: FPVTrackside's defaults for anything left unset.
	table := s.PointsTable
	if len(table) == 0 {
		table = []int{25, 18, 15, 12, 10, 8, 6, 4}
	}
	cfg.PointsTable = map[int]int{}
	for i, p := range table {
		cfg.PointsTable[i+1] = p
	}
	switch strings.ToLower(cfg.Direction) {
	case "asc", "ascending", "low":
		cfg.Direction = "ascending"
	default:
		cfg.Direction = "descending"
	}
	if cfg.Rounds < 1 {
		cfg.Rounds = 3
	}
	mains := s.Mains
	if mains <= 0 {
		mains = (len(st.Pilots) + 7) / 8 // one bracket per 8 pilots
	}
	for i := 0; i < mains && i < 26; i++ {
		cfg.Mains = append(cfg.Mains, string(rune('A'+i)))
	}
	return cfg
}

// engine rehydrates the stage's format from its stored state.
func (st *stageRecord) engine() (raceformat.Format, error) {
	return raceformat.FromState(st.Format, st.config(), st.State)
}

// seeds maps each pilot to their 1-based seed.
func (st *stageRecord) seeds() map[string]int {
	m := make(map[string]int, len(st.Pilots))
	for i, p := range st.Pilots {
		m[p] = i + 1
	}
	return m
}

// stageFormats describes each format for the setup page.
var stageFormats = []struct {
	Kind  raceformat.FormatKind
	Label string
	What  string
}{
	{raceformat.KindElimination, "Double elimination", "Winners and losers brackets; a pilot is out after two losses. Ends in a grand final or chase the ace."},
	{raceformat.KindQualifier, "Double elimination — top 4 advance", "The MultiGP qualifier sheet: ends in a final that ranks the top four, who qualify onward."},
	{raceformat.KindMainsProgression, "Bracket progression", "A qualifier per bracket (A, B, C…). The lowest bracket races first and its top finishers move up."},
	{raceformat.KindLadder, "Ladder", "The lowest seeds race first; the top finishers climb into the next race against higher seeds."},
	{raceformat.KindMains, "Points brackets", "Pilots split into A, B, C… brackets by seed, several rounds each, ranked by points."},
}

// ── store ────────────────────────────────────────────────────────────────────

type stageStore struct {
	mu     sync.Mutex
	path   string
	Stages []*stageRecord `json:"stages"`
	NextID int            `json:"nextId"`
}

func stagesPath(configPath string) string {
	if configPath == "" {
		configPath = "dd-pits-local.json"
	}
	return filepath.Join(filepath.Dir(configPath), "dd-pits-stages.json")
}

func loadStageStore(path string) *stageStore {
	s := &stageStore{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(raw, s); err != nil {
		log.Printf("stages: %s is unreadable (%v) — starting with none", path, err)
		bad := path + ".unreadable"
		_ = os.Rename(path, bad)
	}
	s.path = path
	return s
}

// saveLocked writes the file atomically. Callers hold s.mu.
func (s *stageStore) saveLocked() error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *stageStore) get(id string) *stageRecord {
	for _, st := range s.Stages {
		if st.ID == id {
			return st
		}
	}
	return nil
}

// ── manager ──────────────────────────────────────────────────────────────────

// stageManager runs every stage against the timer.
type stageManager struct {
	store *stageStore
	timer stageTimer

	// kick wakes the sync loop when a fresh snapshot arrives.
	kick chan struct{}
	// opMu serialises everything that talks to the timer, so the background
	// loop and a button press can never both send the same race.
	opMu sync.Mutex
	// failedAt holds off automatic sending after a failure, per stage, so an
	// unreachable timer is not asked again on every poll.
	failMu   sync.Mutex
	failedAt map[string]time.Time
	snapMu   sync.Mutex
	snap     dronedash.Snapshot
}

func newStageManager(path string, timer stageTimer) *stageManager {
	return &stageManager{store: loadStageStore(path), timer: timer, kick: make(chan struct{}, 1),
		failedAt: map[string]time.Time{}}
}

// observe hands over the latest snapshot. Never blocks the poll loop: sending
// races and applying results happen on the manager's own goroutine.
func (m *stageManager) observe(snap dronedash.Snapshot) {
	m.snapMu.Lock()
	m.snap = snap
	m.snapMu.Unlock()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *stageManager) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-m.kick:
			m.syncAll(false)
		}
	}
}

func (m *stageManager) snapshot() dronedash.Snapshot {
	m.snapMu.Lock()
	defer m.snapMu.Unlock()
	return m.snap
}

// create builds a new stage. The engine draws it straight away, so a field the
// format can't use is refused here rather than discovered mid-meeting.
func (m *stageManager) create(name string, kind raceformat.FormatKind, pilots []string, setup stageSetup, autoSend bool) (*stageRecord, error) {
	var clean []string
	seen := map[string]bool{}
	for _, p := range pilots {
		p = strings.TrimSpace(p)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		clean = append(clean, p)
	}
	if len(clean) < 2 {
		return nil, errors.New("a stage needs at least two pilots")
	}
	if strings.TrimSpace(name) == "" {
		name = "Stage"
	}
	st := &stageRecord{
		Name: strings.TrimSpace(name), Format: kind, Setup: setup, Pilots: clean,
		Created: time.Now(), AutoSend: autoSend, AutoResults: true,
		Links: map[string]string{}, Ext: map[string]int{}, Locked: map[string]bool{}, Applied: map[string]string{},
	}
	f, err := raceformat.New(kind, st.config())
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(f.State())
	if err != nil {
		return nil, err
	}
	st.State = raw

	m.store.mu.Lock()
	m.store.NextID++
	st.ID = fmt.Sprintf("s%d", m.store.NextID)
	// FPVTrackside externalIds: a block of 10,000 per stage, well clear of the
	// AUFPV platform's database ids.
	st.Base = 70_000_000 + m.store.NextID*10_000
	m.store.Stages = append(m.store.Stages, st)
	err = m.store.saveLocked()
	m.store.mu.Unlock()
	if err != nil {
		return nil, err
	}
	log.Printf("stages: created %q (%s, %d pilots)", st.Name, kind, len(clean))
	m.syncAll(false)
	return st, nil
}

// deleteWithRaces deletes a stage and takes its races that were never flown
// off the timer, so a deleted bracket doesn't leave heats behind.
func (m *stageManager) deleteWithRaces(id string, mv *meetingView) (int, error) {
	removed := 0
	var err error
	if st, ok := m.get(id); ok && m.timer != nil {
		if races, _, rerr := m.races(st, mv); rerr == nil {
			var links []string
			for _, r := range races {
				if r.Link != "" && r.State == "sent" {
					links = append(links, r.Link)
				}
			}
			if len(links) > 0 {
				m.opMu.Lock()
				removed, err = m.timer.removeRaces(links)
				m.opMu.Unlock()
			}
		}
	}
	m.delete(id)
	return removed, err
}

func (m *stageManager) delete(id string) {
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	out := m.store.Stages[:0]
	for _, st := range m.store.Stages {
		if st.ID != id {
			out = append(out, st)
		}
	}
	m.store.Stages = out
	_ = m.store.saveLocked()
}

// update applies fn to a stage under the lock and saves.
func (m *stageManager) update(id string, fn func(st *stageRecord) error) error {
	return m.updateIf(id, func(st *stageRecord) (bool, error) { return true, fn(st) })
}

// updateIf saves only when fn reports a change: it runs on every poll.
func (m *stageManager) updateIf(id string, fn func(st *stageRecord) (bool, error)) error {
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	st := m.store.get(id)
	if st == nil {
		return errors.New("no such stage")
	}
	changed, err := fn(st)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return m.store.saveLocked()
}

// list returns copies, safe to render without the lock.
func (m *stageManager) list() []stageRecord {
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	out := make([]stageRecord, 0, len(m.store.Stages))
	for _, st := range m.store.Stages {
		out = append(out, *st)
	}
	return out
}

func (m *stageManager) get(id string) (stageRecord, bool) {
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	st := m.store.get(id)
	if st == nil {
		return stageRecord{}, false
	}
	return *st, true
}

// ── race state ───────────────────────────────────────────────────────────────

// The engine names things the AUFPV way. FPVTrackside's stages rename them
// (Webb/Stages/Terms.cs), and dd-pits follows FPVTrackside so a race reads the
// same on the timer, the dashboard and the stage page: "Redemption" is
// "Losers", "A Main" is "Bracket A", a "heat" is a "race", and the
// competition itself is a "stage".
var (
	termMain    = regexp.MustCompile(`\b(?:the )?([A-Z]) Main\b`)
	termHeat    = regexp.MustCompile(`\b([Hh])eat(s?)\b`)
	termBracket = regexp.MustCompile(`\b([Bb])racket(s?)\b`)
)

func stageTerms(t string) string {
	t = strings.NewReplacer("Redemption", "Losers", "redemption", "losers").Replace(t)
	// Go's regexp has no look-behind: protect the bracket names that stay.
	t = strings.NewReplacer("Winners bracket", "\x00W", "Losers bracket", "\x00L").Replace(t)
	t = termBracket.ReplaceAllStringFunc(t, func(m string) string {
		g := termBracket.FindStringSubmatch(m)
		if g[1] == "B" {
			return "Stage" + g[2]
		}
		return "stage" + g[2]
	})
	t = strings.NewReplacer("\x00W", "Winners bracket", "\x00L", "Losers bracket").Replace(t)
	t = termMain.ReplaceAllString(t, "Bracket $1")
	t = termHeat.ReplaceAllStringFunc(t, func(m string) string {
		g := termHeat.FindStringSubmatch(m)
		if g[1] == "H" {
			return "Race" + g[2]
		}
		return "race" + g[2]
	})
	return t
}

// standings is the full finishing order, everyone ranked, in stage terms.
func standings(f raceformat.Format, seeds map[string]int) []raceformat.Standing {
	rows := raceformat.FinalOrdering(f, seeds)
	for i := range rows {
		rows[i].Detail = stageTerms(rows[i].Detail)
	}
	return rows
}

// stageRace is one race of a stage as the pages show it.
type stageRace struct {
	raceformat.RaceView
	State  string // waiting | ready | sent | running | flown | finished | void
	Link   string // the timer's race, once sent
	Locked bool   // result entered by hand
	Seeds  map[string]int
}

// ready reports whether a race's line-up is final: nothing it could draw a
// pilot from is still to be run. Races only take pilots from earlier rounds,
// so every earlier round (within the same bracket, for bracket progression)
// being finished means nobody else can join.
func ready(rv raceformat.RaceView, all []raceformat.RaceView) bool {
	if rv.IsComplete || rv.IsVoid || rv.Locked || len(rv.Pilots) == 0 {
		return false
	}
	main := mainOf(rv.ID)
	for _, o := range all {
		if o.ID == rv.ID || mainOf(o.ID) != main || o.Round >= rv.Round {
			continue
		}
		if !o.IsComplete && !o.IsVoid {
			return false
		}
	}
	return true
}

// mainOf is the bracket a bracket-progression race belongs to ("A" for
// "A:W1-1"); "" for every other format.
func mainOf(id string) string {
	if i := strings.Index(id, ":"); i > 0 {
		return id[:i]
	}
	return ""
}

// races projects a stage for the pages, with each race's state against the
// timer.
func (m *stageManager) races(st stageRecord, mv *meetingView) ([]stageRace, raceformat.Format, error) {
	f, err := st.engine()
	if err != nil {
		return nil, nil, err
	}
	all := f.Races()
	seeds := st.seeds()
	byID := map[string]vmRace{}
	if mv != nil {
		for _, r := range mv.Races {
			byID[r.ID] = r
		}
	}
	out := make([]stageRace, 0, len(all))
	for _, rv := range all {
		sr := stageRace{RaceView: rv, Link: st.Links[rv.ID], Locked: st.Locked[rv.ID], Seeds: seeds}
		after := st.RerunAfter[rv.ID]
		sr.Title = stageTerms(rv.Title)
		sr.GroupLabel = stageTerms(rv.GroupLabel)
		tr, flown := m.timerRace(sr.Link, byID)
		if flown && after > 0 && tr.StartMs < after {
			flown = false // the run being replaced
		}
		switch {
		case rv.IsVoid:
			sr.State = "void"
		case rv.IsComplete:
			sr.State = "finished"
		case sr.Link != "" && flown && tr.State == "running":
			sr.State = "running"
		case sr.Link != "" && flown && tr.State == "ended":
			sr.State = "flown"
		case sr.Link != "":
			sr.State = "sent"
		case ready(rv, all):
			sr.State = "ready"
		default:
			sr.State = "waiting"
		}
		out = append(out, sr)
	}
	return out, f, nil
}

// timerRace finds the race the timer flew for a link. A RotorHazard heat can
// be run more than once; its first run that has started is the one that
// counts, and a later re-run replaces it.
func (m *stageManager) timerRace(link string, byID map[string]vmRace) (vmRace, bool) {
	if link == "" {
		return vmRace{}, false
	}
	if r, ok := byID[link]; ok {
		return r, true
	}
	if strings.HasPrefix(link, "rh-h") {
		var best vmRace
		found := false
		for id, r := range byID {
			if strings.HasPrefix(id, link+"-r") && r.State != "pending" {
				if !found || r.StartMs > best.StartMs {
					best, found = r, true
				}
			}
		}
		return best, found
	}
	return vmRace{}, false
}

// linkedRaceID is the meeting race a link points at now: the race itself
// for FPVTrackside, the latest run of the heat for RotorHazard.
func (m *stageManager) linkedRaceID(link string, mv *meetingView) string {
	if mv == nil {
		return link
	}
	byID := map[string]vmRace{}
	for _, r := range mv.Races {
		byID[r.ID] = r
	}
	if r, ok := m.timerRace(link, byID); ok {
		return r.ID
	}
	return link
}

// ── syncing with the timer ───────────────────────────────────────────────────

// syncAll applies finished results and sends ready races, for every stage.
// sendAll forces a send of every ready race even where automatic sending is
// off (the "Send ready races" button).
func (m *stageManager) syncAll(sendAll bool) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	snap := m.snapshot()
	var mv *meetingView
	if snap.Event.ID != "" {
		mv = buildMeeting(snap)
	}
	for _, st := range m.list() {
		m.syncStage(st.ID, mv, sendAll)
	}
}

func (m *stageManager) syncStage(id string, mv *meetingView, send bool) {
	// Results first: they can make more races ready.
	if mv != nil {
		m.applyResults(id, mv)
	}
	st, ok := m.get(id)
	if !ok || !(send || st.AutoSend) || m.timer == nil {
		return
	}
	if !send {
		m.failMu.Lock()
		at := m.failedAt[id]
		m.failMu.Unlock()
		if time.Since(at) < 20*time.Second {
			return
		}
	}
	races, _, err := m.races(st, mv)
	if err != nil {
		return
	}
	var toSend []stageRace
	for _, r := range races {
		if r.State == "ready" {
			toSend = append(toSend, r)
		}
	}
	if len(toSend) == 0 {
		return
	}
	m.sendRaces(id, toSend, false)
}

// sendOne sends a single race by hand ("Send & make current").
func (m *stageManager) sendOne(id, raceID string, mv *meetingView) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, ok := m.get(id)
	if !ok {
		return errors.New("no such stage")
	}
	races, _, err := m.races(st, mv)
	if err != nil {
		return err
	}
	for _, rc := range races {
		if rc.ID == raceID {
			if rc.State != "ready" && rc.State != "sent" {
				return errors.New("that race can't be sent yet")
			}
			return m.sendRaces(id, []stageRace{rc}, true)
		}
	}
	return errors.New("no such race")
}

// sendRaces creates races on the timer and records their links. Callers hold
// opMu.
func (m *stageManager) sendRaces(id string, races []stageRace, makeCurrent bool) error {
	st, ok := m.get(id)
	if !ok {
		return errors.New("no such stage")
	}
	// Number new races for FPVTrackside before sending, so a retry reuses them.
	_ = m.update(id, func(s *stageRecord) error {
		if s.Ext == nil {
			s.Ext = map[string]int{}
		}
		for _, r := range races {
			if s.Ext[r.ID] == 0 {
				s.Ext[r.ID] = s.Base + 1000 + len(s.Ext)
			}
		}
		st = *s
		return nil
	})
	links, err := m.timer.send(st, races, makeCurrent)
	m.failMu.Lock()
	if err != nil {
		m.failedAt[id] = time.Now()
	} else {
		delete(m.failedAt, id)
	}
	m.failMu.Unlock()
	_ = m.update(id, func(s *stageRecord) error {
		if s.Links == nil {
			s.Links = map[string]string{}
		}
		for k, v := range links {
			s.Links[k] = v
		}
		if err != nil {
			s.LastError = "Sending to " + m.timer.name() + ": " + err.Error()
		} else {
			s.LastError = ""
		}
		return nil
	})
	if err != nil {
		log.Printf("stages: %s: send failed: %v", st.Name, err)
	} else if len(links) > 0 {
		log.Printf("stages: %s: sent %d race(s) to %s", st.Name, len(links), m.timer.name())
	}
	return err
}

// finishOrder is a flown race's finishing order, restricted to the stage
// race's pilots. dd-pits' own race view already orders a heat: explicit timer
// positions when it has them, else laps then total time. Pilots who never
// crossed are appended in seed order, so formats that rank everyone get a
// full placement.
func finishOrder(tr vmRace, pilots []string, seeds map[string]int) []string {
	in := map[string]string{}
	for _, p := range pilots {
		in[strings.ToLower(p)] = p
	}
	var out []string
	done := map[string]bool{}
	for _, p := range tr.Pilots {
		name, ok := in[strings.ToLower(p.Name)]
		if !ok || done[name] || (p.CountedLaps == 0 && p.Position == 0) {
			continue
		}
		out = append(out, name)
		done[name] = true
	}
	var rest []string
	for _, p := range pilots {
		if !done[p] {
			rest = append(rest, p)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return seeds[rest[i]] < seeds[rest[j]] })
	return append(out, rest...)
}

// applyResults takes the finishing order of every flown, linked race.
func (m *stageManager) applyResults(id string, mv *meetingView) {
	byID := map[string]vmRace{}
	for _, r := range mv.Races {
		byID[r.ID] = r
	}
	_ = m.updateIf(id, func(st *stageRecord) (bool, error) {
		if !st.AutoResults {
			return false, nil
		}
		f, err := st.engine()
		if err != nil {
			return false, nil
		}
		changed, touched := false, false
		for _, rv := range f.Races() {
			link := st.Links[rv.ID]
			if link == "" || st.Locked[rv.ID] || rv.IsVoid {
				continue
			}
			tr, ok := m.timerRace(link, byID)
			if !ok || tr.State != "ended" || tr.StartMs < st.RerunAfter[rv.ID] {
				continue
			}
			order := finishOrder(tr, rv.Pilots, st.seeds())
			sig := tr.ID + "|" + strings.Join(order, ",")
			if st.Applied[rv.ID] == sig {
				continue
			}
			if rv.IsComplete {
				if ok, _ := f.CanEditRace(rv.ID); !ok {
					st.Applied[rv.ID] = sig
					touched = true
					continue
				}
				err = f.EditRace(rv.ID, order)
			} else {
				err = f.ApplyPlacement(rv.ID, order)
			}
			if err != nil {
				st.LastError = fmt.Sprintf("Result for %s: %v", stageTerms(rv.Title), err)
				log.Printf("stages: %s: %s", st.Name, st.LastError)
				st.Applied[rv.ID] = sig
				touched = true
				continue
			}
			if st.Applied == nil {
				st.Applied = map[string]string{}
			}
			st.Applied[rv.ID] = sig
			changed = true
			log.Printf("stages: %s: %s finished %s", st.Name, stageTerms(rv.Title), strings.Join(order, ", "))
		}
		if changed {
			raw, err := json.Marshal(f.State())
			if err == nil {
				st.State = raw
			}
		}
		return changed || touched, nil
	})
}

// raceControl starts, stops, restarts or discards a stage race on the timer.
// Starting a race that isn't on the timer yet sends it first. Restarting or
// discarding a race whose result has already been taken from timing clears
// that result, so the bracket waits for the new run.
func (m *stageManager) raceControl(id, raceID, action string, mv *meetingView) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.timer == nil {
		return errors.New("no timer")
	}
	st, ok := m.get(id)
	if !ok {
		return errors.New("no such stage")
	}
	races, f, err := m.races(st, mv)
	if err != nil {
		return err
	}
	var rc *stageRace
	for i := range races {
		if races[i].ID == raceID {
			rc = &races[i]
		}
	}
	if rc == nil {
		return errors.New("no such race")
	}
	if rc.State == "waiting" || rc.State == "void" {
		return errors.New("that race can't be flown yet")
	}
	if rc.IsComplete && (action == "restart" || action == "discard") {
		if ok, why := f.CanEditRace(raceID); !ok {
			return errors.New("can't re-run it: " + why)
		}
	}
	link := rc.Link
	if link == "" {
		if action != "start" {
			return errors.New("that race isn't on the timer yet")
		}
		if err := m.sendRaces(id, []stageRace{*rc}, true); err != nil {
			return err
		}
		st, _ = m.get(id)
		link = st.Links[raceID]
		if link == "" {
			return errors.New("the timer didn't take the race")
		}
	}
	rerunAt := time.Now().UnixMilli()
	newLink, err := m.timer.control(link, action)
	if err != nil {
		return err
	}
	if newLink != "" && newLink != link {
		// The re-run is a new race on the timer: follow it.
		_ = m.update(id, func(s *stageRecord) error {
			s.Links[raceID] = newLink
			delete(s.Applied, raceID)
			return nil
		})
	}
	if (action == "restart" || action == "discard") && rc.IsComplete {
		// The new run decides it now: hand the race back to timing.
		return m.update(id, func(s *stageRecord) error {
			f, err := s.engine()
			if err != nil {
				return err
			}
			if err := f.ResetRace(raceID); err != nil {
				return err
			}
			raw, err := json.Marshal(f.State())
			if err != nil {
				return err
			}
			s.State = raw
			delete(s.Locked, raceID)
			delete(s.Applied, raceID)
			if s.RerunAfter == nil {
				s.RerunAfter = map[string]int64{}
			}
			s.RerunAfter[raceID] = rerunAt
			return nil
		})
	}
	return nil
}

// setRaceSettings changes a stage's laps and race time and applies them to
// the races still to fly.
func (m *stageManager) setRaceSettings(id string, laps, secs int, mv *meetingView) error {
	if laps < 0 || secs < 0 {
		return errors.New("laps and race time can't be negative")
	}
	if err := m.update(id, func(st *stageRecord) error {
		st.Setup.Laps, st.Setup.RaceSeconds = laps, secs
		return nil
	}); err != nil {
		return err
	}
	if m.timer == nil {
		return nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, _ := m.get(id)
	races, _, err := m.races(st, mv)
	if err != nil {
		return err
	}
	var sent []stageRace
	for _, r := range races {
		if r.State == "sent" {
			sent = append(sent, r)
		}
	}
	return m.timer.applySettings(st, sent)
}

// setResult records a finishing order entered by hand and locks it.
func (m *stageManager) setResult(id, raceID string, order []string) error {
	err := m.update(id, func(st *stageRecord) error {
		f, err := st.engine()
		if err != nil {
			return err
		}
		var rv *raceformat.RaceView
		for _, r := range f.Races() {
			if r.ID == raceID {
				r := r
				rv = &r
			}
		}
		if rv == nil {
			return errors.New("no such race")
		}
		if rv.IsComplete {
			if ok, why := f.CanEditRace(raceID); !ok {
				return errors.New(why)
			}
			err = f.EditRace(raceID, order)
		} else {
			err = f.ApplyPlacement(raceID, order)
		}
		if err != nil {
			return err
		}
		raw, err := json.Marshal(f.State())
		if err != nil {
			return err
		}
		st.State = raw
		if st.Locked == nil {
			st.Locked = map[string]bool{}
		}
		st.Locked[raceID] = true
		return nil
	})
	if err == nil {
		go m.syncAll(false)
	}
	return err
}

// resetResult un-runs a race and hands it back to timing.
func (m *stageManager) resetResult(id, raceID string) error {
	return m.update(id, func(st *stageRecord) error {
		f, err := st.engine()
		if err != nil {
			return err
		}
		if err := f.ResetRace(raceID); err != nil {
			return err
		}
		raw, err := json.Marshal(f.State())
		if err != nil {
			return err
		}
		st.State = raw
		delete(st.Locked, raceID)
		delete(st.Applied, raceID)
		return nil
	})
}

// seedingFromMeeting ranks the meeting's pilots for seeding: best consecutive
// laps first (the time-trial measure), then most laps, then fastest lap; pilots
// with no time follow in name order.
func seedingFromMeeting(mv *meetingView) []string {
	if mv == nil {
		return nil
	}
	type row struct {
		name        string
		consec, lap float64
		laps        int
	}
	var rows []row
	seen := map[string]bool{}
	for _, b := range mv.Board {
		if b.Name == "" || b.Name == "—" || seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		rows = append(rows, row{b.Name, b.BestConsec, b.BestLap, b.Laps})
	}
	for _, r := range mv.Races {
		for _, p := range r.Pilots {
			if p.Name != "" && p.Name != "—" && !seen[p.Name] {
				seen[p.Name] = true
				rows = append(rows, row{name: p.Name})
			}
		}
	}
	better := func(a, b float64) (bool, bool) { // (decided, a better)
		switch {
		case a > 0 && b > 0 && a != b:
			return true, a < b
		case a > 0 && b == 0:
			return true, true
		case a == 0 && b > 0:
			return true, false
		}
		return false, false
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if d, w := better(rows[i].consec, rows[j].consec); d {
			return w
		}
		if rows[i].laps != rows[j].laps {
			return rows[i].laps > rows[j].laps
		}
		if d, w := better(rows[i].lap, rows[j].lap); d {
			return w
		}
		return strings.ToLower(rows[i].name) < strings.ToLower(rows[j].name)
	})
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.name
	}
	return out
}
