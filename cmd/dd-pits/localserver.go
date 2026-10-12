package main

// Local web server for dd-pits. When enabled (-serve), the agent runs a small
// HTTP server on the venue laptop that renders the live DroneDash surfaces —
// the current race, the dashboard home, race history and an OBS overlay —
// straight from the in-memory FPVTrackside snapshot it already fetches each
// tick. It needs no database and no cloud connectivity: the data source is the
// local timing computer, so the dashboard keeps working when the internet is
// down. Club name + accent colour + logo are fetched once from the cloud's PWA
// manifest (best-effort) and cached so the local pages match the club's brand.
//
// This is a self-contained mirror, not the full platform site: same race
// layout (lap grid, positions, time-trial best-X-consecutive + PB), themed to
// the club, without the site's nav/footer chrome.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/dronedash"
)

// ── View models (built from a dronedash.Snapshot) ───────────────────────────
//
// The view types + the whole JSON API now live in the shared internal/platform/
// ddapi package, so the offline agent and the cloud server expose an identical
// API. These aliases keep buildMeeting and the page templates (which reference
// the fields by name) unchanged.

type (
	vmPilot     = ddapi.Pilot
	vmRace      = ddapi.Race
	vmStanding  = ddapi.Standing
	meetingView = ddapi.Meeting
)

// maxUpcomingCards caps how many "Next Up" races the home view renders, matching
// the cloud dashboard's LIMIT 3 — without it a freshly-seeded meeting dumps every
// pending heat (100+), all sharing FPVTrackside's default "Round 1 — Race 1"
// label until they're run.
const maxUpcomingCards = 3

// ── Snapshot → meetingView ──────────────────────────────────────────────────

func ttType(s string) bool {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	return strings.Contains(s, "timetrial")
}

// practiceType reports whether an FPVTrackside event/round type denotes practice.
// Mirrors the cloud's isPracticeType: the EventTypes enum carries both spellings
// of the same value (Practice/Practise, CasualPractice/CasualPractise) and C#
// serialises whichever name it resolves first, so match on either stem.
func practiceType(s string) bool {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	return strings.Contains(s, "practic") || strings.Contains(s, "practis")
}

// hasField reports whether a heat has any real pilot in it. FPVTrackside pads
// lineups with blank seats and leaves whole never-run placeholder heats behind,
// and neither should read as a queued heat.
func hasField(r vmRace) bool {
	for _, p := range r.Pilots {
		if p.Name != "" && p.Name != "—" {
			return true
		}
	}
	return false
}

// beforeRace reports whether heat a runs before heat b in FPVTrackside's running
// order: round Order first, then round number, then race number. Mirrors the
// cloud's ddRunOrderSQL — see ddapi.Race.Order for why Round/Number alone is not
// a running order.
func beforeRace(aOrd, aRound, aNum, bOrd, bRound, bNum int) bool {
	if aOrd != bOrd {
		return aOrd < bOrd
	}
	if aRound != bRound {
		return aRound < bRound
	}
	return aNum < bNum
}

// bestConsecutiveSum returns the lowest sum of `window` consecutive lap times.
// Mirrors the platform's scoring. 0 when there aren't enough laps.
func bestConsecutiveSum(laps []float64, window int) float64 {
	sum, _ := bestConsecutiveRun(laps, window)
	return sum
}

// bestConsecutiveRun is bestConsecutiveSum plus WHICH run it was — the index of
// the first lap in the winning window, so the pilot page can name the laps
// ("Laps 2–4"). One implementation, so the number on the leaderboard and the
// laps the pilot page attributes it to can never come from different searches.
func bestConsecutiveRun(laps []float64, window int) (float64, int) {
	if window <= 0 || len(laps) < window {
		return 0, 0
	}
	best, at := 0.0, 0
	for i := 0; i+window <= len(laps); i++ {
		s := 0.0
		for j := 0; j < window; j++ {
			s += laps[i+j]
		}
		if best == 0 || s < best {
			best, at = s, i
		}
	}
	return best, at
}

// boardNextStatus computes each pilot's "Next Race In" cell (mirrors the cloud
// computeNextRaceStatus): "Racing" if they're in the running race, "Staging" if
// they're in the very next pending race, otherwise the count of pending races
// before their first upcoming race ("" when they have none left). Keyed by name.
func boardNextStatus(races []vmRace) map[string]string {
	// FPVTrackside's running order (see beforeRace), so "next" means the earliest
	// pending race in actual run order.
	order := make([]vmRace, len(races))
	copy(order, races)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		return beforeRace(a.Order, a.Round, a.Number, b.Order, b.Round, b.Number)
	})

	// "Staging" is the next heat with a field in it: FPVTrackside leaves piles of
	// empty never-run placeholders at its defaults and they sort ahead of
	// everything, so without the pilot check the badge lands on one of those
	// rather than the heat actually on deck.
	runningIdx, nextPendingIdx := -1, -1
	for i, r := range order {
		if r.State == "running" && runningIdx < 0 {
			runningIdx = i
		}
		if r.State == "pending" && nextPendingIdx < 0 && hasField(r) {
			nextPendingIdx = i
		}
	}

	out := map[string]string{}
	for _, p := range firstUpcomingByPilot(order) {
		idx := p.idx
		switch {
		case idx == runningIdx:
			out[p.name] = "Racing"
		case idx == nextPendingIdx:
			out[p.name] = "Staging"
		default:
			// Count pending races strictly before this pilot's first upcoming race.
			count := 0
			for i := 0; i < idx; i++ {
				if order[i].State == "pending" {
					count++
				}
			}
			if count > 0 {
				out[p.name] = strconv.Itoa(count)
			}
		}
	}
	return out
}

// firstUpcomingByPilot returns, per pilot name, the index (into the supplied
// race-ordered slice) of their first not-yet-ended race.
func firstUpcomingByPilot(order []vmRace) []struct {
	name string
	idx  int
} {
	seen := map[string]bool{}
	var out []struct {
		name string
		idx  int
	}
	for i, r := range order {
		if r.State == "ended" {
			continue
		}
		for _, p := range r.Pilots {
			if p.Name == "" || p.Name == "—" || seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			out = append(out, struct {
				name string
				idx  int
			}{p.Name, i})
		}
	}
	return out
}

// staleHeatGrace mirrors the cloud: a started-but-unended heat with no new lap
// for longer than this is treated as abandoned/done so it stops counting and
// can't hold "current". Generous so a normal inter-lap gap never trips it.
const staleHeatGrace = 4 * time.Minute

// racePositionsNeedRelabel reports whether a heat's pilot positions are a clean,
// distinct, positive set (every pilot finished and they're all distinct, so
// FPVTrackside's finishing ORDER is known) — but possibly inflated/gapped (e.g.
// 2,4,6,8 that FPVTrackside reports when ranking across a re-detected/duplicated
// result set). Such a set is safe to relabel to 1..N by the sorted order (display
// only, order preserved). A tie or a not-yet-finished pilot (position 0) returns
// false → left to the existing order / zero-fill. Matches the cloud card's
// `allReported && distinct` guard.
func racePositionsNeedRelabel(pilots []vmPilot) bool {
	n := len(pilots)
	if n < 2 {
		return false
	}
	seen := map[int]bool{}
	for _, p := range pilots {
		if p.Position <= 0 || seen[p.Position] {
			return false
		}
		seen[p.Position] = true
	}
	return true
}

func buildMeeting(snap dronedash.Snapshot) *meetingView {
	mv := &meetingView{MeetingID: snap.Event.ID, Name: snap.Event.Name}

	// Venue-clock "now" the agent stamps while a heat is running (same clock as
	// the lap times). Zero in tests / from an old agent, which disables the
	// staleness check below.
	nowRef := parseTime(snap.CapturedAt)

	// pilot id -> name
	pilotName := map[string]string{}
	for _, p := range snap.Pilots {
		pilotName[p.ID] = p.Name
	}
	// channel id -> color (from the event's parallel arrays) and label.
	chanColor := map[string]string{}
	for i, id := range snap.Event.Channels {
		if i < len(snap.Event.ChannelColors) {
			chanColor[id] = snap.Event.ChannelColors[i]
		}
	}
	chanLabel := map[string]string{}
	for _, c := range snap.Channels {
		if c.DisplayName != "" {
			chanLabel[c.ID] = c.DisplayName
		} else {
			chanLabel[c.ID] = fmt.Sprintf("%s%d", c.ShortBand, c.Number)
		}
	}
	// round id -> (number, running order, name, raw event_type)
	type roundInfo struct {
		num   int
		order int
		name  string
		etype string
	}
	rounds := map[string]roundInfo{}
	for _, r := range snap.Rounds {
		rounds[r.ID] = roundInfo{num: r.RoundNumber, order: r.Order, name: r.Name, etype: r.EventType}
	}
	// results: race id -> pilot id -> (position, dnf)
	type res struct {
		pos int
		dnf bool
	}
	results := map[string]map[string]res{}
	for _, rr := range snap.Results {
		if results[rr.Race] == nil {
			results[rr.Race] = map[string]res{}
		}
		results[rr.Race][rr.Pilot] = res{pos: rr.Position, dnf: rr.DNF}
	}

	// Representative window for the meeting (target laps wins, else PB Laps, else Laps).
	windowFor := func(targetLaps int) int {
		switch {
		case targetLaps > 0:
			return targetLaps
		case snap.Event.PBLaps > 0:
			return snap.Event.PBLaps
		case snap.Event.Laps > 0:
			return snap.Event.Laps
		default:
			return 3
		}
	}
	// Leaderboard "Top N Consec" window. The cloud dashboard uses a single
	// event-wide ConsecN for this column — uniform across every heat, independent
	// of each heat's target laps — derived from the event's Laps setting
	// (COALESCE(NULLIF(laps,0), 3) in loadActiveDashEvent), defaulting to 3 only
	// when unset. Mirror that here so an event configured for "top 2 consec" shows
	// 2, not a floored 3.
	mv.Window = snap.Event.Laps
	if mv.Window < 1 {
		mv.Window = 3
	}

	for _, r := range snap.Races {
		// FPVTrackside keeps deleted/invalidated heats in its payload flagged
		// Valid=false; the cloud Ingest drops them entirely, so mirror that here —
		// otherwise their laps feed the local leaderboard but not the cloud one.
		if !r.Valid {
			continue
		}
		ri := rounds[r.Round]
		// Round type wins when set, else the event type.
		effType := ri.etype
		if effType == "" {
			effType = snap.Event.EventType
		}
		vr := vmRace{
			ID:         r.ID,
			Round:      ri.num,
			Order:      ri.order,
			Number:     r.RaceNumber,
			Practice:   practiceType(effType),
			RoundName:  ri.name,
			TargetLaps: r.TargetLaps,
			IsTT:       ttType(effType),
		}
		vr.ConsecWindow = windowFor(r.TargetLaps)
		vr.Bracket = r.ExternalID != 0
		vr.ExtID = r.ExternalID
		if vr.IsTT {
			mv.IsTT = true
		}
		// Title carries the heat's own type word so a qualifying time trial
		// reads differently from a race on a mixed meeting (mirrors the cloud
		// card's TypeLabel).
		typeWord := "Race"
		if vr.Practice {
			typeWord = "Practice"
		}
		if vr.IsTT {
			typeWord = "Time Trial"
		}
		vr.TypeLabel = typeWord
		if vr.RoundName != "" {
			vr.Label = fmt.Sprintf("%s — %s %d", vr.RoundName, typeWord, vr.Number)
		} else {
			vr.Label = fmt.Sprintf("Round %d — %s %d", vr.Round, typeWord, vr.Number)
		}
		start := parseTime(r.Start)
		end := parseTime(r.End)
		// A started heat FPVTrackside left without an End is done (and must not read
		// "running" / hijack the Current card) when either it already carries
		// finishing results — its finish edge was just never recorded — or it has
		// gone quiet far longer than any real inter-lap gap (an abandoned / false-
		// started heat). Mirrors the cloud Ingest fix.
		hasResults := len(results[r.ID]) > 0
		stale := false
		if !nowRef.IsZero() {
			var lastLapT time.Time
			for _, l := range r.Laps {
				t := parseTime(l.EndTime)
				if t.IsZero() {
					t = parseTime(l.StartTime)
				}
				if t.After(lastLapT) {
					lastLapT = t
				}
			}
			if !lastLapT.IsZero() && nowRef.Sub(lastLapT) > staleHeatGrace {
				stale = true
			}
		}
		switch {
		case start.IsZero():
			vr.State = "pending"
		case !end.IsZero() || hasResults || stale:
			vr.State = "ended"
		default:
			vr.State = "running"
		}
		if !start.IsZero() {
			vr.StartMs = start.UnixMilli()
		}

		// Lap → pilot resolution (mirrors Ingest): a lap carries a Detection id;
		// the detection carries the pilot + a validity flag. Skip laps whose
		// detection is invalid (FPVTrackside emits noise/double-trigger laps).
		type detInfo struct {
			pilot string
			valid bool
			found bool
		}
		detByID := map[string]detInfo{}
		for _, d := range r.Detections {
			detByID[d.ID] = detInfo{pilot: d.Pilot, valid: d.Valid, found: true}
		}

		// Pilots in this race, with channel + laps.
		for _, pc := range r.PilotChannels {
			vp := vmPilot{
				Name:         pilotName[pc.Pilot],
				ChannelLabel: chanLabel[pc.Channel],
				ChannelColor: chanColor[pc.Channel],
			}
			if vp.Name == "" {
				vp.Name = "—"
			}
			for _, lap := range r.Laps {
				d := detByID[lap.Detection]
				if d.pilot != pc.Pilot {
					continue
				}
				if d.found && !d.valid {
					continue // invalid detection (noise / double trigger)
				}
				if lap.LapNumber == 0 {
					vp.Holeshot = lap.LengthSeconds
				} else if lap.LengthSeconds > 0 {
					vp.Laps = append(vp.Laps, lap.LengthSeconds)
				}
			}
			// Counted laps + total. Races cap at target_laps (laps past the target
			// don't count toward the result); time trials count every lap (all laps
			// are shown and scored via best-X-consecutive). Holeshot always counts.
			if vp.Holeshot > 0 {
				vp.CountedLaps++
				vp.Total += vp.Holeshot
			}
			for i, s := range vp.Laps {
				if !vr.IsTT && r.TargetLaps > 0 && i+1 > r.TargetLaps {
					break
				}
				vp.CountedLaps++
				vp.Total += s
				if vp.BestLap == 0 || s < vp.BestLap {
					vp.BestLap = s
				}
			}
			vp.BestConsec = bestConsecutiveSum(vp.Laps, vr.ConsecWindow)
			// Race card display cap (mirrors the cloud's loadDashRaceCardByID):
			// a *race* only shows lap columns up to target_laps — laps past the
			// target (incl. the sub-second noise/double-trigger crossings
			// FPVTrackside keeps Valid=true) don't count toward the result and
			// must not be shown. Time trials (and races with no target) show every
			// recorded lap. Aggregates above (Total/BestLap/BestConsec/board) keep
			// using the full vp.Laps, exactly like the cloud.
			vp.DispLaps = vp.Laps
			if !vr.IsTT && r.TargetLaps > 0 && len(vp.DispLaps) > r.TargetLaps {
				vp.DispLaps = vp.DispLaps[:r.TargetLaps]
			}
			if rr, ok := results[r.ID][pc.Pilot]; ok {
				vp.Position = rr.pos
				vp.DNF = rr.dnf
			}
			vr.Pilots = append(vr.Pilots, vp)
		}

		// Sort: time trials rank by best X-consecutive (the TT metric); otherwise
		// explicit positions when present, else laps desc then total asc.
		sort.SliceStable(vr.Pilots, func(i, j int) bool {
			a, b := vr.Pilots[i], vr.Pilots[j]
			if vr.IsTT {
				if (a.BestConsec > 0) != (b.BestConsec > 0) {
					return a.BestConsec > 0
				}
				if a.BestConsec > 0 && b.BestConsec > 0 {
					return a.BestConsec < b.BestConsec
				}
				if a.CountedLaps != b.CountedLaps {
					return a.CountedLaps > b.CountedLaps
				}
				return a.Total < b.Total
			}
			if a.Position > 0 && b.Position > 0 {
				return a.Position < b.Position
			}
			if a.Position > 0 != (b.Position > 0) {
				return a.Position > 0
			}
			if a.CountedLaps != b.CountedLaps {
				return a.CountedLaps > b.CountedLaps
			}
			return a.Total < b.Total
		})
		// Relabel inflated/gapped reported positions (e.g. 2,4,6,8) to a clean 1..N
		// by the sorted finishing order above — display only, preserves the order
		// (no lap recompute for the gap case), mirrors the cloud. Time trials keep
		// their consec-ranked positions.
		renumber := !vr.IsTT && racePositionsNeedRelabel(vr.Pilots)
		for i := range vr.Pilots {
			if renumber || vr.Pilots[i].Position == 0 {
				vr.Pilots[i].Position = i + 1
			}
		}
		mv.Races = append(mv.Races, vr)
	}

	// Event-wide PB per pilot (best-consec across TT races).
	pb := map[string]float64{}
	for _, vr := range mv.Races {
		if !vr.IsTT {
			continue
		}
		for _, p := range vr.Pilots {
			if p.BestConsec > 0 && (pb[p.Name] == 0 || p.BestConsec < pb[p.Name]) {
				pb[p.Name] = p.BestConsec
			}
		}
	}
	for ri := range mv.Races {
		for pi := range mv.Races[ri].Pilots {
			mv.Races[ri].Pilots[pi].PBConsec = pb[mv.Races[ri].Pilots[pi].Name]
		}
	}

	// Event-wide leaderboard: fold every race into one row per pilot so the local
	// site shows the same standings pane as the cloud dashboard — same columns
	// (laps / hole shot / top lap / top-N consec / fastest race / next race in),
	// keyed by pilot name. Ordered by best-X-consecutive ascending (the same
	// comparator as the cloud's sortLeaderboardByConsec, below).
	//
	// Channel chip (mirrors the cloud's pilotLeaderboardChannels) by priority:
	// 1. racing now (a started-but-not-finished heat); else 2. the next genuine
	// upcoming heat — a pending heat AFTER their last finished one in running
	// order, so FPVTrackside's pile of never-run Round 1 / Race 1 placeholders
	// don't masquerade as "upcoming" on a finished meeting; else 3. their last
	// finished heat. Label + colour come together from the chosen heat.
	type chanCand struct {
		label, color    string
		ord, round, num int
	}
	type agg struct {
		running *chanCand  // started, not finished — earliest in order
		lastFin *chanCand  // finished — latest in order (their last heat)
		pending []chanCand // not started — candidates for "next up"
	}
	// beforeHR reports whether heat a runs before b in FPVTrackside's running order.
	beforeHR := func(a, b chanCand) bool { return beforeRace(a.ord, a.round, a.num, b.ord, b.round, b.num) }
	// The timing columns (laps / hole shot / top lap / top-N consec / fastest
	// race) come from pilotScores — the SAME fold the pilot page's stat cards
	// read, so the board and a pilot's own page can never quote different
	// numbers for the same pilot. Practice heats are excluded there, for the
	// reason spelled out on scoringHeats.
	scored := pilotScores(mv.Races, mv.Window)
	byPilot := map[string]*agg{}
	for _, vr := range mv.Races {
		for _, p := range vr.Pilots {
			if p.Name == "" || p.Name == "—" {
				continue
			}
			a := byPilot[p.Name]
			if a == nil {
				a = &agg{}
				byPilot[p.Name] = a
			}
			// The channel chip answers "what channel am I on", so it considers
			// EVERY heat — a pilot flying a practice heat right now still needs
			// their channel shown. Only the scoring below skips practice.
			if p.ChannelLabel != "" {
				hc := chanCand{label: p.ChannelLabel, color: p.ChannelColor,
					ord: vr.Order, round: vr.Round, num: vr.Number}
				switch vr.State {
				case "ended":
					if a.lastFin == nil || beforeHR(*a.lastFin, hc) {
						cp := hc
						a.lastFin = &cp
					}
				case "running":
					if a.running == nil || beforeHR(hc, *a.running) {
						cp := hc
						a.running = &cp
					}
				default: // pending
					a.pending = append(a.pending, hc)
				}
			}
		}
	}
	// The cloud board lists every dd_pilots row — the meeting's whole pilot
	// roster, which Ingest creates from snap.Pilots — so pilots not (yet) placed
	// in any heat still appear, timeless at the bottom. Mirror that here.
	for _, sp := range snap.Pilots {
		if sp.Name == "" {
			continue
		}
		if _, ok := byPilot[sp.Name]; !ok {
			byPilot[sp.Name] = &agg{}
		}
	}
	nextStatus := boardNextStatus(mv.Races)
	for name, a := range byPilot {
		// 1. racing now → 2. next genuine upcoming (after their last finished) →
		// 3. last finished heat.
		ch := a.running
		if ch == nil {
			var next *chanCand
			for i := range a.pending {
				p := a.pending[i]
				if a.lastFin != nil && !beforeHR(*a.lastFin, p) {
					continue // not after the heat they last ran → placeholder
				}
				if next == nil || beforeHR(p, *next) {
					next = &a.pending[i]
				}
			}
			if next != nil {
				ch = next
			} else {
				ch = a.lastFin
			}
		}
		var chLabel, chColor string
		if ch != nil {
			chLabel, chColor = ch.label, ch.color
		}
		// A pilot on the roster who hasn't flown a scoring heat has no score
		// row — they still list, timeless, like the cloud board.
		var sc pilotScore
		if s := scored[name]; s != nil {
			sc = *s
		}
		mv.Board = append(mv.Board, vmStanding{
			Name: name, ChannelLabel: chLabel, ChannelColor: chColor,
			Laps: sc.TotalLaps, Holeshot: sc.Holeshot.Value, BestLap: sc.BestLap.Value,
			BestConsec: sc.BestConsec.Value, FastestRace: sc.FastestRace.Value,
			NextStatus: nextStatus[name],
		})
	}
	// Order by best-X-consecutive (the "Top N Consec" column) ascending, pilots
	// with no consec time last, best single lap then name as tiebreaks.
	sort.SliceStable(mv.Board, func(i, j int) bool {
		a, b := mv.Board[i], mv.Board[j]
		if (a.BestConsec > 0) != (b.BestConsec > 0) {
			return a.BestConsec > 0 // pilots with a consec time rank ahead of those without
		}
		if a.BestConsec > 0 && b.BestConsec > 0 && a.BestConsec != b.BestConsec {
			return a.BestConsec < b.BestConsec
		}
		if (a.BestLap > 0) != (b.BestLap > 0) {
			return a.BestLap > 0
		}
		if a.BestLap > 0 && b.BestLap > 0 && a.BestLap != b.BestLap {
			return a.BestLap < b.BestLap
		}
		return a.Name < b.Name
	})
	for i := range mv.Board {
		mv.Board[i].Pos = i + 1
	}

	// Current = running race, else most-recently-started.
	var current *vmRace
	var recent, upcoming []vmRace
	for i := range mv.Races {
		r := mv.Races[i]
		switch r.State {
		case "running":
			// Prefer the most-recently-started running heat rather than whichever
			// comes last in snapshot order, so a stale unfinished heat can't beat
			// the heat actually being raced.
			if current == nil || r.StartMs > current.StartMs {
				current = &mv.Races[i]
			}
		case "ended":
			recent = append(recent, r)
		case "pending":
			upcoming = append(upcoming, r)
		}
	}
	// "Next Up" should only list genuinely-queued heats. Three filters:
	//   (a) it must have pilots;
	//   (b) it must run AFTER the meeting's current position — the furthest-along
	//       heat that's started (running or finished) — so never-run leftover heats
	//       from an earlier round (e.g. E-Main round 1 while we're in round 17)
	//       don't reappear as "upcoming"; and
	//   (c) its (round, race number) must be unique among the remaining pending
	//       heats — FPVTrackside leaves piles of placeholders at the default Round 1
	//       / Race 1, and a real running order never repeats a number.
	realPilots := hasField // (a) it must have pilots
	// Most-recently-started heat = the meeting's current position. Use start time,
	// not the highest round/race number: a meeting can carry more than one running
	// order in one event (e.g. qualifying rounds 1-10 then a bracket that restarts
	// at round 1), so "highest started round/race" would pin the position on the
	// finished quali and hide the live bracket's next heats. The latest-started heat
	// is always where racing is now (in a normal monotonic meeting it IS the highest
	// round/race, so behaviour is unchanged).
	maxO, maxR, maxN, havePos := 0, 0, 0, false
	var bestStart int64 = -1
	for _, r := range mv.Races {
		if r.State == "pending" {
			continue
		}
		if r.StartMs > bestStart {
			bestStart = r.StartMs
			maxO, maxR, maxN, havePos = r.Order, r.Round, r.Number, true
		}
	}
	afterPos := func(r vmRace) bool {
		return !havePos || beforeRace(maxO, maxR, maxN, r.Order, r.Round, r.Number)
	}
	// Bracket-exported heats (snapshot ExternalID = the bracket_races.id the
	// "Paste Round" carried) are deliberately-queued races, never FPVTrackside's
	// stray Round-1 placeholders — so they bypass the after-position + uniqueness
	// suppression and sort FIRST, ahead of the normal round config. Mirrors the
	// cloud loadDashHome so the offline board surfaces a finals bracket that
	// restarts at Round 1 after qualifying rounds 1-N.
	eligible := func(r vmRace) bool { return realPilots(r) && (r.Bracket || afterPos(r)) }
	// A bracket race that has already run (a started/ended copy exists in the
	// snapshot) must NOT resurface as upcoming via the bracket exemption —
	// FPVTrackside keeps the old heats around and a leftover/re-paste reads as
	// pending, so e.g. the already-finished E-Main heats were showing in Next Up.
	// Mirrors the cloud's exclude-already-started + dedupe-by-bracket-race.
	startedBracket := map[int]bool{}
	for _, r := range mv.Races {
		if r.Bracket && r.ExtID != 0 && r.StartMs > 0 {
			startedBracket[r.ExtID] = true
		}
	}
	idCount := map[[3]int]int{}
	for _, r := range upcoming {
		if realPilots(r) && !r.Bracket && afterPos(r) {
			idCount[[3]int{r.Order, r.Round, r.Number}]++
		}
	}
	var queued []vmRace
	seenExt := map[int]bool{}
	for _, r := range upcoming {
		if !eligible(r) || (!r.Bracket && idCount[[3]int{r.Order, r.Round, r.Number}] > 1) {
			continue
		}
		if r.Bracket {
			if startedBracket[r.ExtID] || seenExt[r.ExtID] {
				continue // bracket race already run, or already queued (duplicate)
			}
			seenExt[r.ExtID] = true
		}
		queued = append(queued, r)
	}
	upcoming = queued
	// Order in running order, bracket heats first.
	sort.SliceStable(upcoming, func(i, j int) bool {
		a, b := upcoming[i], upcoming[j]
		if a.Bracket != b.Bracket {
			return a.Bracket
		}
		return beforeRace(a.Order, a.Round, a.Number, b.Order, b.Round, b.Number)
	})
	// Between heats: prefer the heat the operator CUED in FPVTrackside
	// (snap.SelectedRaceID, from the patched /current endpoint) so the board shows
	// the next heat before it starts — even out of order. Must be an un-run
	// (pending) heat; a running selection is already the current heat above.
	if current == nil && snap.SelectedRaceID != "" {
		for i := range mv.Races {
			if mv.Races[i].ID == snap.SelectedRaceID && mv.Races[i].State == "pending" {
				current = &mv.Races[i]
				for j := range upcoming {
					if upcoming[j].ID == current.ID {
						upcoming = append(upcoming[:j], upcoming[j+1:]...)
						break
					}
				}
				break
			}
		}
	}

	// Between heats (nothing running): promote the on-deck heat to Current so
	// stopping a heat advances the card to the next heat instead of staying on the
	// just-ended one. Mirrors the cloud loadDashHome.
	if current == nil && len(upcoming) > 0 {
		onDeck := upcoming[0]
		current = &onDeck
		upcoming = upcoming[1:]
	}
	mv.UpcomingTotal = len(upcoming)
	mv.AllUpcoming = append([]vmRace(nil), upcoming...)
	if len(upcoming) > maxUpcomingCards {
		upcoming = upcoming[:maxUpcomingCards]
	}
	mv.Upcoming = upcoming
	if current == nil {
		// Meeting finished (nothing running or upcoming): fall back to the
		// most-recently-started heat so the final heat still shows.
		var bestIdx = -1
		for i := range mv.Races {
			if mv.Races[i].StartMs == 0 {
				continue
			}
			if bestIdx < 0 || mv.Races[i].StartMs > mv.Races[bestIdx].StartMs {
				bestIdx = i
			}
		}
		if bestIdx >= 0 {
			current = &mv.Races[bestIdx]
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].StartMs > recent[j].StartMs })
	mv.Current = current
	mv.Recent = recent

	// Channel set (the frequency grid): the EVENT's configured channels resolved
	// against the catalogue for band/frequency, frequency-ordered — so a client can
	// show every channel incl. ones no pilot is on this heat.
	catalogue := map[string]ddapi.Channel{}
	for _, c := range snap.Channels {
		label := c.DisplayName
		if label == "" {
			label = c.ShortBand + strconv.Itoa(c.Number)
		}
		catalogue[c.ID] = ddapi.Channel{
			Label: label, Band: c.Band, ShortBand: c.ShortBand,
			Number: c.Number, Frequency: c.Frequency,
		}
	}
	var channels []ddapi.Channel
	for i, chID := range snap.Event.Channels {
		ch, ok := catalogue[chID]
		if !ok {
			continue
		}
		if i < len(snap.Event.ChannelColors) {
			ch.Color = snap.Event.ChannelColors[i]
		}
		channels = append(channels, ch)
	}
	sort.SliceStable(channels, func(i, j int) bool { return channels[i].Frequency < channels[j].Frequency })
	mv.Channels = channels
	return mv
}

// parseTime parses an FPVTrackside/normalized RFC3339-ish timestamp; zero on fail.
// parseTime reads an FPVTrackside timestamp.
//
// The layouts must match the ones the dronedash package accepts, because both
// read the SAME data and a format one understands and the other does not is an
// invisible failure: an unparsed start time reads as "not started", so every
// heat looks pending, no state transition ever fires, and the booth simply
// never comments. Nothing errors — it just goes quiet.
//
// FPVTrackside serialises DateTime with the running machine's culture, so a
// macOS venue writes "2026/07/28 15:30:38.684" — slashes, a space, and a
// single-digit hour — which the ISO layouts alone reject. Confirmed against a
// real Race.json on this machine.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	// FPVTrackside's "not set" sentinel is DateTime.MinValue, written as year
	// 0001 in whichever separator the culture uses.
	if s == "" || strings.HasPrefix(s, "0001-01-01") || strings.HasPrefix(s, "0001/01/01") {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.0000000",
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		// Culture-dependent form; the optional fraction also matches the
		// no-fraction "2026/07/28 15:30:38".
		"2006/01/02 15:04:05.999999999",
	} {
		// Naive values are venue-local wall clock, which is what the timing
		// system is stamping.
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ── HTTP server ─────────────────────────────────────────────────────────────

type brand struct {
	Name    string
	Accent  string
	LogoB64 string // data: URI for offline use

	// Version is shown in the topbar of every page.
	//
	// Worth the pixels: half of diagnosing a report is establishing which build
	// someone is actually on, and "I downloaded it yesterday" is not an answer.
	// Two builds can look identical and behave differently — which has already
	// caught us out once today, when a fix was reported as not working by
	// someone still running the previous binary.
	Version string
}

// raceAnnouncer returns the announcer, or nil when race audio is not running.
func (s *liveServer) raceAnnouncer() *raceAnnouncer {
	if s == nil || s.announcerFn == nil {
		return nil
	}
	return s.announcerFn()
}

type liveServer struct {
	// logs is the in-memory tail of the log, and tape the last few cues the
	// booth actually produced. Both exist for the debug page: a remote venue
	// reporting "no commentary" could not tell "nothing was generated" from
	// "it was generated and went nowhere", and those have opposite fixes.
	logs *logRing
	tape *boothTape

	// The design-in-progress: the last rendered voice-design sample, held so
	// "Keep this voice" clones exactly what was auditioned — never a re-render,
	// which could come out different.
	designMu   sync.Mutex
	designWav  []byte
	designName string

	// ftEvents receives FPVTrackside's pushed events when the venue has the
	// notifier switched on. nil when it has not, which is the stock state — so
	// the route only exists where something is actually sending to it.
	ftEvents *ftEventReceiver

	// applyAudioOut turns local speaker playback on/off or moves it to another
	// device WITHOUT restarting the agent, and audioOutputs lists what is
	// available. nil when this build has no audio bus.
	// applyBoothConfig re-reads the settings file into the running booth, so a
	// change to the beat/phase toggles or the style takes effect NOW. Without
	// it the page saves, says so, and the booth carries on with what it was
	// given at startup — which is indistinguishable from the setting not
	// working, and was exactly the Spanish report.
	applyBoothConfig func()

	applyAudioOut func(enabled bool, device, latency string) error
	audioPlaying  func() bool
	audioOutputs  func() ([]audioDevice, string, error)

	// announcerFn reaches the race announcer, which the AGENT owns and brings
	// up on its own goroutine — a large voice model takes minutes to load, so
	// this is a lookup rather than a field set once at construction.
	announcerFn func() *raceAnnouncer

	// ── local (no-account) mode ──────────────────────────────────────────────
	//
	// cfg is the venue's own settings file, and is what makes an offline race
	// day possible: goggle pilots, the OSD, the booth. nil when the agent was
	// built without it.
	cfg *localConfig

	// stages runs brackets against the timer (stages.go). nil until main
	// creates it beside the settings file.
	stages *stageManager

	// hasVoice is whether anything can actually speak, which decides whether
	// dd-pits or FPVTrackside makes the race calls.
	hasVoice bool

	// tracksideURL is shown on the overview so an operator can see at a glance
	// what the agent is reading.
	tracksideURL string

	// reloadELRS re-reads the goggle config after the pilots page changes it,
	// so a pilot added between heats works on the next one rather than after a
	// restart. nil when ELRS is not enabled.
	reloadELRS func()

	// youtubeKey lets the Detect button use YouTube's official API. Without it
	// the fallback scrape is routinely bot-walled from a venue's IP.
	youtubeKey string

	// commentary is the generated per-heat commentary for this meeting.
	commentary *commentaryStore

	// generateCommentary asks for a finished heat's commentary. nil until the
	// agent is ready.
	generateCommentary func(raceID string)

	// enableBackpack brings the goggle link up without a restart, so ticking
	// the box on the setup page is enough. nil before the agent is ready.
	enableBackpack func() error

	// sendAnnounce puts a message on pilots' goggle screens — one pilot, or
	// everyone when no callsigns are given. nil when ELRS is not running.
	sendAnnounce func(text string, callsigns []string) ([]string, map[string]error)

	// backpackStatus reports whether the ELRS backpack is connected, so the
	// setup page can say so rather than leaving an operator guessing why the
	// goggles are dark. nil when ELRS is not enabled.
	backpackStatus func() bool

	// setChannel changes one pilot's goggle channel — the local replacement for
	// the cloud command queue. nil when ELRS is not enabled.
	setChannel func(callsign, channel string) error

	mu    sync.RWMutex
	snap  dronedash.Snapshot
	have  bool
	brand brand
	tmpl  *template.Template

	// SSE fan-out: every connected page subscribes a channel; setSnapshot pings
	// them all so the browser refreshes the instant new timing data arrives
	// (realtime laps/race start-stop) instead of waiting for the fallback poll.
	subsMu sync.Mutex
	subs   map[chan struct{}]struct{}

	// UI tick coalescing — the difference between "realtime" and "ten times a
	// second".
	//
	// setSnapshot fires on every poll, and -active-interval is 100ms while a
	// race is in flight, so an un-coalesced fan-out woke every connected browser
	// ten times a second. Each of those wake-ups costs the CLIENT a fetch and a
	// re-render; on a Raspberry Pi pits display that alone saturates the main
	// thread. Timing data still ingests at full rate — only the notification is
	// rate-limited, and because subscribers pull the current state rather than
	// receive it, a dropped tick loses nothing: the next one carries everything.
	bcMu    sync.Mutex
	bcLast  time.Time
	bcTimer *time.Timer
	uiTick  time.Duration

	// Optional commentary booth (set from main when -booth is on): mounts the
	// /booth status page + /booth.wav audio stream on this server.
	booth *booth

	// mixer is the audio bus, which exists with or without the booth — the race
	// calls use it on their own.
	mixer *boothMixer

	// Listener handover state, owned by start/stop. A self-update spawns a
	// successor process that must bind the same address, so the outgoing
	// process closes its listener first (stop) and the incoming one retries a
	// still-busy port (listenRetry) — the two together make the swap seamless.
	srvMu sync.Mutex
	srv   *http.Server
	addr  string
}

// accentTooDarkForDashboard reports whether a hex colour is empty/unparseable or
// too dark to read on the dashboard's always-dark background. The 0.22 threshold
// matches the cloud's tooDarkForDashboard: saturated brand mid-tones (Rebels red,
// CMRC blue) read fine and are kept; near-black/navy brands get the lime fallback
// so the local board and cloud dashboard agree on which accents are swapped.
func accentTooDarkForDashboard(hex string) bool {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) < 6 {
		return true
	}
	ri, e1 := strconv.ParseInt(hex[0:2], 16, 0)
	gi, e2 := strconv.ParseInt(hex[2:4], 16, 0)
	bi, e3 := strconv.ParseInt(hex[4:6], 16, 0)
	if e1 != nil || e2 != nil || e3 != nil {
		return true
	}
	return (0.299*float64(ri)+0.587*float64(gi)+0.114*float64(bi))/255 < 0.22
}

// defaultUITick is 0 — OFF. The server pushes every change the instant it lands,
// which is what the race director's own screen and the OBS sources want, and the
// machine running this is not the constrained one. A slow VIEWER slows itself
// down (?lite=1) rather than everybody else. -ui-tick exists for the case where
// the host really is the bottleneck.
const defaultUITick = 0

func newLiveServer(b brand) *liveServer {
	s := &liveServer{brand: b, subs: map[chan struct{}]struct{}{}, uiTick: defaultUITick}
	// Always listening.
	//
	// The receiver costs nothing until FPVTrackside is pointed at it, and having
	// the endpoint present unconditionally means switching the notifier on is a
	// setting in FPVTrackside rather than a setting in two places that have to
	// agree. A venue that turns it on and finds a 404 would reasonably conclude
	// the feature does not exist.
	s.ftEvents = newFTEventReceiver(ftForwardTargets())
	// The dashboard always renders on a dark background, and the accent colours
	// the race-timer/countdown text. A club whose theme colour is unset OR too
	// dark would give an unreadable near-black timer, so fall back to the AUFPV
	// brand lime (high-contrast on the dark surface) in that case.
	if accentTooDarkForDashboard(s.brand.Accent) {
		s.brand.Accent = "#c6ff3d"
	}
	if s.brand.Name == "" {
		s.brand.Name = "dd-pits"
	}
	s.tmpl = template.Must(template.New("dd").Funcs(template.FuncMap{
		// watchLink is empty unless the meeting's stream has been anchored, so
		// the ▶ Watch link simply is not there until there is something to
		// link to — better than a link that goes nowhere.
		"watchLink": func(raceID string, startMs int64) string {
			return s.watchLinkFor(raceID, startMs)
		},
		"f3": func(v float64) string {
			if v <= 0 {
				return "—"
			}
			return fmt.Sprintf("%.3f", v)
		},
		"f2": func(v float64) string {
			if v <= 0 {
				return "—"
			}
			return fmt.Sprintf("%.2f", v)
		},
		"ord": func(n int) string {
			switch n {
			case 1:
				return "1st"
			case 2:
				return "2nd"
			case 3:
				return "3rd"
			default:
				return fmt.Sprintf("%dth", n)
			}
		},
		// dict builds a map inline, so a shared sub-template (the pilot page's
		// stat card) can take more than one argument.
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					m[k] = kv[i+1]
				}
			}
			return m
		},
		// dictRefresh clones the page data and injects a Refresh interval (ms) so
		// the shared "js" partial knows how often to soft-refresh this page.
		"dictRefresh": func(refresh int, data map[string]any) map[string]any {
			out := map[string]any{"Refresh": refresh}
			for k, v := range data {
				out[k] = v
			}
			return out
		},
	}).Funcs(overlayFuncs).Parse(pageTemplates + overlayTemplates))
	return s
}

func (s *liveServer) setSnapshot(snap dronedash.Snapshot) {
	s.mu.Lock()
	s.snap, s.have = snap, true
	s.mu.Unlock()
	s.broadcast()
}

// broadcast pings every connected SSE subscriber, at most once per uiTick.
//
// A tick that arrives inside the window is not dropped — it is deferred to the
// moment the window opens, so the newest state always reaches the page. Only
// ONE deferred tick is ever outstanding: subscribers read the current snapshot
// when they wake, so five coalesced pings and one ping deliver the same thing.
func (s *liveServer) broadcast() {
	if s.uiTick <= 0 {
		s.broadcastNow()
		return
	}
	s.bcMu.Lock()
	defer s.bcMu.Unlock()
	if s.bcTimer != nil {
		return // already scheduled; it will carry whatever is newest by then
	}
	if wait := s.uiTick - time.Since(s.bcLast); wait > 0 {
		s.bcTimer = time.AfterFunc(wait, func() {
			s.bcMu.Lock()
			s.bcTimer, s.bcLast = nil, time.Now()
			s.bcMu.Unlock()
			s.broadcastNow()
		})
		return
	}
	s.bcLast = time.Now()
	s.broadcastNow()
}

// broadcastNow is the raw fan-out (non-blocking). A buffered channel that's
// already full just means a refresh is already pending, so we drop the extra
// ping rather than block the poll loop.
func (s *liveServer) broadcastNow() {
	s.subsMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.subsMu.Unlock()
}

func (s *liveServer) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch
}

func (s *liveServer) unsubscribe(ch chan struct{}) {
	s.subsMu.Lock()
	delete(s.subs, ch)
	s.subsMu.Unlock()
}

func (s *liveServer) meeting() (*meetingView, brand, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.have {
		return nil, s.brand, false
	}
	return buildMeeting(s.snap), s.brand, true
}

func (s *liveServer) render(w http.ResponseWriter, name string, mv *meetingView, extra map[string]any) {
	_, b, _ := s.meeting()
	data := map[string]any{"M": mv, "Brand": b, "Accent": template.CSS(b.Accent)}
	for k, v := range extra {
		data[k] = v
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil && !isClientGone(err) {
		log.Printf("dd-pits: render %s: %v", name, err)
	}
}

// fragment renders ONLY the #app contents for a page, and answers 304 when they
// are byte-identical to what the caller already has.
//
// The soft-refresh used to re-fetch the whole document — 7KB of CSS and 14KB of
// inline script that never change — and hand all of it to DOMParser, which
// builds a complete document and constructs its stylesheet before a single byte
// of it is used. That is affordable on a laptop and is most of the per-update
// cost on a Raspberry Pi. The body alone is a fraction of the bytes and parses
// in a <template> instead of a document.
//
// The ETag closes the other half: buildMeeting is a pure function of the
// snapshot (no wall-clock anywhere — the running clock is client-side, rendered
// as a literal 0.0 plus its start epoch), so an unchanged board hashes to the
// same value and costs one 304 with no body, no parse and no DOM work at all.
func (s *liveServer) fragment(w http.ResponseWriter, r *http.Request, name string, mv *meetingView) {
	s.fragmentExtra(w, r, name, mv, nil)
}

// fragmentExtra is fragment for a page whose body needs more than the meeting —
// the pilot page, which is rendered for one named pilot.
func (s *liveServer) fragmentExtra(w http.ResponseWriter, r *http.Request, name string, mv *meetingView, extra map[string]any) {
	_, b, _ := s.meeting()
	data := map[string]any{"M": mv, "Brand": b, "Accent": template.CSS(b.Accent)}
	for k, v := range extra {
		data[k] = v
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name+"-body", data); err != nil {
		log.Printf("dd-pits: render %s-body: %v", name, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	h := fnv.New64a()
	h.Write(buf.Bytes())
	etag := fmt.Sprintf(`"%x"`, h.Sum64())
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(buf.Bytes()); err != nil && !isClientGone(err) {
		log.Printf("dd-pits: write %s-body: %v", name, err)
	}
}

// isClientGone reports whether a response-write error is just the browser
// disconnecting (closed tab, aborted refresh) rather than a real render fault —
// common now that every page soft-refreshes in realtime. Not worth logging.
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset by peer") ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// localSource adapts the in-memory liveServer to the shared ddapi.Source. There
// is exactly one meeting (the venue's), so the meeting id arg is ignored.
type localSource struct{ s *liveServer }

func (ls localSource) Meeting(_ *http.Request, _ string) (*ddapi.Meeting, bool) {
	mv, _, ok := ls.s.meeting()
	return mv, ok
}

func (ls localSource) CurrentMeeting(_ *http.Request) (string, string, bool) {
	mv, _, ok := ls.s.meeting()
	if !ok {
		return "", "", false
	}
	return mv.MeetingID, mv.Name, true
}

func (ls localSource) Subscribe(_ *http.Request) (<-chan struct{}, func()) {
	ch := ls.s.subscribe()
	return ch, func() { ls.s.unsubscribe(ch) }
}

func (s *liveServer) handler() http.Handler {
	mux := http.NewServeMux()
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mv, _, ok := s.meeting()
			// Never assign to the captured `name` — this closure is shared by
			// every request for the route.
			view := name
			if !ok {
				// The overlay sits on a live stream, so it waits invisibly
				// rather than putting the full-screen welcome on air.
				view, mv = "waiting", nil
				if name == "overlay" {
					view = "overlay-waiting"
				}
			}
			if r.URL.Query().Get("fragment") == "1" {
				s.fragment(w, r, view, mv)
				return
			}
			s.render(w, view, mv, nil)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/dronedash" {
			http.NotFound(w, r)
			return
		}
		page("home")(w, r)
	})
	mux.HandleFunc("/dronedash/races", page("races"))
	mux.HandleFunc("/dronedash/upcoming", page("upcoming"))
	// One pilot's meeting — keyed by name, because the venue snapshot has no
	// pilot id the local board could route on (see localpilot.go).
	mux.HandleFunc("/dronedash/pilot", s.servePilot)
	mux.HandleFunc("/live", page("live"))
	mux.HandleFunc("/overlay", page("overlay"))
	mux.HandleFunc("/overlay/", s.overlayPage)
	mux.HandleFunc("/overlays", s.overlayGallery)
	mux.HandleFunc("/overlays/spotlight", s.overlaySpotlightPush)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.Handle("/assets/", assetHandler())

	// Is the bus speaking RIGHT NOW?
	//
	// Read by the silent speaker standing in for FPVTrackside's own voice.
	// FPVTrackside starts the race clock from its speech-finished callback, and
	// the stand-in could only guess when that was — a word-count estimate — so
	// it reported "finished" while dd-pits was still mid-sentence and the race
	// began under the countdown. This lets it answer with the truth instead.
	mux.HandleFunc("/speaking", func(w http.ResponseWriter, r *http.Request) {
		busy := false
		if s.mixer != nil {
			busy = s.mixer.busy() || s.mixer.pending() > 0
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"speaking":%t}`, busy)
	})

	// The race director's own calls — the ones no amount of polling can detect,
	// because they are decisions rather than events.
	mux.HandleFunc("/calls", func(w http.ResponseWriter, r *http.Request) {
		ann := s.raceAnnouncer()
		if ann == nil {
			http.Error(w, "race audio is not running (start the agent with -race-audio)", http.StatusServiceUnavailable)
			return
		}
		ann.serveRaceControl(w, r)
	})
	// Always mounted, even with nothing to play.
	//
	// This address goes into an OBS Media Source days before a meeting, and a
	// 404 there reads as "dd-pits is broken" rather than "there is no audio
	// configured yet" — which is the actual situation and a fixable one. So the
	// route answers with the reason and where to fix it.
	mux.HandleFunc("/booth.wav", func(w http.ResponseWriter, r *http.Request) {
		if s.mixer != nil {
			s.mixer.serveStream(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "No audio is being produced yet.\n\n"+
			"This address carries dd-pits' race calls and commentary. It is silent because\n"+
			"neither is switched on:\n\n"+
			"  * race calls   - "+setupBaseURL+"/setup/calls  (choose dd-pits, not FPVTrackside)\n"+
			"  * commentary   - "+setupBaseURL+"/setup/booth  (choose a voice engine)\n\n"+
			"Leave this source in OBS; it starts working as soon as one of them is on.\n")
	})
	if s.booth != nil {
		mux.HandleFunc("/booth", s.booth.servePage)
	}
	// The stream link, chapters, the watch player and the commentary it can
	// play over the video.
	if s.cfg != nil {
		s.registerVideoPages(mux)
	}

	// The venue's own setup pages — goggle pilots, OSD, booth, race calls.
	// Mounted whenever a settings file exists, so a cloud-connected agent can
	// still SHOW its configuration (read-only) rather than 404ing.
	s.registerLocalSetup(mux)
	s.registerStages(mux)
	s.registerMarshal(mux)

	// JSON read-API + SSE streams + Swagger UI / OpenAPI — the shared surface
	// (internal/platform/ddapi), served identically by the cloud server. There is
	// one meeting here, so the optional ?meeting= selector is ignored.
	ddapi.Register(mux, localSource{s})
	return ddapi.WithCORSPreflight(mux)
}

// listenRetry bounds how long start keeps trying a busy address, and
// listenRetryEvery how often. A self-update hands this port from the outgoing
// process to its successor, and the successor boots while the predecessor is
// still verifying it came up alive — so the first Listen legitimately fails.
// Without the retry the successor lost the port for good and the venue's local
// board stayed dark for the rest of the meeting. Comfortably longer than the
// predecessor's whole verify-then-exit window (see restartSelf).
const (
	listenRetry      = 30 * time.Second
	listenRetryEvery = 250 * time.Millisecond
)

// start runs the local server until ctx is cancelled, retrying a busy address
// for listenRetry so a self-update handover can't lose the port.
func (s *liveServer) start(ctx context.Context, addr string) {
	srv := &http.Server{Addr: addr, Handler: withBasePath(rememberSetupPIN(s.handler()))}
	s.srvMu.Lock()
	s.srv, s.addr = srv, addr
	s.srvMu.Unlock()

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	deadline := time.Now().Add(listenRetry)
	warned := false
	for ctx.Err() == nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if time.Now().After(deadline) {
				log.Printf("dd-pits: local server: %v", err)
				return
			}
			if !warned {
				log.Printf("dd-pits: local dashboard port %s busy — retrying for up to %s (handover from a previous instance?)", addr, listenRetry)
				warned = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(listenRetryEvery):
			}
			continue
		}
		log.Printf("dd-pits: local dashboard on http://%s  (/, /live, /dronedash/races, /overlay)", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("dd-pits: local server: %v", err)
		}
		return
	}
}

// stop releases the listener WITHOUT tearing the rest of the agent down, so a
// self-update's successor can bind the address while this process is still
// alive and able to roll the update back. No-op when the server never started.
func (s *liveServer) stop() {
	s.srvMu.Lock()
	srv := s.srv
	s.srvMu.Unlock()
	if srv == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
}

// restart re-listens after a stop whose reason evaporated — a self-update that
// rolled back. start's own retry loop covers the port the dead successor may
// still be releasing.
func (s *liveServer) restart(ctx context.Context) {
	s.srvMu.Lock()
	addr := s.addr
	s.srvMu.Unlock()
	if addr == "" {
		return
	}
	go s.start(ctx, addr)
}

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// pageTemplates holds the self-contained HTML for every local surface. They share
// a CSS + JS block (themed via .Accent) and a soft auto-refresh that re-fetches
// the page and swaps #app in place (no flash) while ticking the running clock.
const pageTemplates = `
{{define "css"}}
` + headBrand + `
<style>
:root{ --accent:{{.Accent}}; --bg:#0f1115; --panel:#1b1e24; --panel2:#23272f; --line:#313742; --txt:#e8eaed; --muted:#9aa3af; }
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--txt);font:15px/1.4 -apple-system,Segoe UI,Roboto,system-ui,sans-serif}
a{color:var(--accent);text-decoration:none}
.wrap{max-width:1100px;margin:0 auto;padding:1rem}
.top{display:flex;align-items:center;gap:.7rem;padding:.7rem 1rem;border-bottom:1px solid var(--line);background:#13151a;position:sticky;top:0;z-index:5}
.top img{height:30px;width:auto;border-radius:6px}
.top .name{font-weight:800;font-size:1.05rem}
.top .meet{color:var(--muted);font-size:.85rem}
.top nav{margin-left:auto;display:flex;gap:.3rem;flex-wrap:wrap}
.top .bmc{color:#fd0;font-size:.78rem;text-decoration:none;white-space:nowrap}
.overlay .bmc{display:none}
.top .ver{color:var(--muted);font-size:.72rem;opacity:.55;margin-left:.6rem;font-variant-numeric:tabular-nums;white-space:nowrap}
.top nav a{padding:.35rem .7rem;border:1px solid var(--line);border-radius:8px;color:var(--txt);font-weight:600;font-size:.82rem}
.top nav a:hover{border-color:var(--accent)}
.card{background:var(--panel);border:1px solid var(--line);border-radius:14px;padding:1rem 1.1rem;margin:1rem 0;box-shadow:0 1px 4px rgba(0,0,0,.18)}
.card h2{margin:.1rem 0 .2rem;font-size:1.15rem}
.sub{color:var(--muted);font-size:.85rem}
.badge{display:inline-block;font-size:.7rem;font-weight:700;padding:.12rem .5rem;border-radius:999px;background:#2a2f38;color:#cfd5dd;margin-left:.4rem}
.badge.run{background:#16432a;color:#76e29a}
.clock{font-variant-numeric:tabular-nums;font-weight:800;color:var(--accent);font-size:1.3rem;margin-left:auto}
table.rt{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}
table.rt th{font-size:.7rem;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);text-align:left;padding:.4rem .5rem;border-bottom:1px solid var(--line)}
table.rt td{padding:.4rem .5rem;border-bottom:1px solid #262b33}
/* Race cards share fixed column positions, so CHAN/HS/LAPS/TOTAL line up from
   card to card instead of drifting with the longest name in each heat — every
   table auto-sized itself, and MirellaFPV in one heat shoved that card's
   columns somewhere the next card's weren't. The leaderboard (.lb) keeps its
   own layout: different columns, one table, nothing to align with.

   The shares are PERCENTAGES, not rem, and that is the whole point. They were
   rem: the seven sized columns claimed a fixed ~46.6rem (~746px) and Laps —
   column 5, deliberately unsized — got whatever was left over. On the home
   page the race cards sit in the left pane beside a 720px leaderboard, so
   there was nothing left: Laps collapsed to a few pixels and its chips painted
   straight over the Total, which rendered two numbers on top of each other.
   A percentage cannot collapse — Laps keeps its share of whatever width the
   card has, and the columns still line up card to card because every card is
   the same width. */
table.rt:not(.lb){table-layout:fixed}
table.rt:not(.lb) th,table.rt:not(.lb) td{padding-left:.4rem;padding-right:.4rem}
table.rt:not(.lb) th:nth-child(1){width:8%}
table.rt:not(.lb) th:nth-child(2){width:13.5%}
table.rt:not(.lb) th:nth-child(3){width:7.5%}
table.rt:not(.lb) th:nth-child(4){width:10%}
/* 5 = Laps, unsized on purpose: it takes everything the others don't (~30%). */
table.rt:not(.lb) th:nth-child(6){width:12%}
table.rt:not(.lb) th:nth-child(7),table.rt:not(.lb) th:nth-child(8){width:9.5%}
table.rt:not(.lb) td:nth-child(2){overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
table.rt:not(.lb) td:nth-child(3){white-space:nowrap}
.pos{font-weight:800;min-width:2rem;text-align:center;border-radius:7px;background:#3a4049;display:inline-block;padding:.1rem .35rem}
.pos.p1{background:linear-gradient(180deg,#ffd95e,#e0a800);color:#2a2000}
.pos.p2{background:linear-gradient(180deg,#dbe1ea,#9aa6b6);color:#1b1f25}
.pos.p3{background:linear-gradient(180deg,#e8a064,#c06a2c);color:#241200}
.dot{width:.8rem;height:.8rem;border-radius:50%;display:inline-block;border:1px solid rgba(255,255,255,.35);vertical-align:middle;margin-right:.35rem}
.pname{font-weight:700}
/* Pilot names link to their page. Deliberately NOT accent-coloured: these
   tables are dense, and forty accent-coloured names read as an alert. */
.plink{color:inherit;text-decoration:none}
.plink:hover{color:var(--accent);text-decoration:underline}
/* The heat a pilot-page stat card jumped to, so it's obvious which one it is. */
.card:target{border-color:var(--accent)}
.laps{display:inline-flex;gap:.3rem;flex-wrap:wrap}
.lap{background:#0f1115;border:1px solid #3a4049;border-radius:6px;padding:.1rem .4rem;font-size:.82rem;min-width:3.4rem;text-align:center}
.lap.best{border-color:#34c759;color:#9be7ad}
.chip{font-size:.74rem;padding:.15rem .5rem;border-radius:999px;background:#343b45;color:#cfd5dd;font-weight:600}
.chip.consec{background:#16432a;color:#76e29a}
.muted{color:var(--muted)}
/* live grid */
.grid{display:grid;gap:.8rem;grid-template-columns:repeat(auto-fit,minmax(320px,1fr))}
.tile{background:var(--panel2);border:1px solid var(--line);border-radius:12px;padding:.8rem;border-left:6px solid var(--accent)}
.tile .h{display:flex;align-items:center;gap:.5rem}
.tile .h .pn{font-weight:800;font-size:1.1rem}
.tile .h .pp{margin-left:auto;font-weight:800;font-size:1.2rem;color:var(--accent)}
.big{font-size:1.6rem;font-weight:800;font-variant-numeric:tabular-nums}
.kv{display:flex;gap:1rem;flex-wrap:wrap;margin-top:.4rem}
.kv div span{display:block}
.kv .k{font-size:.65rem;text-transform:uppercase;color:var(--muted)}
.kv .v{font-weight:700}
.empty{color:var(--muted);text-align:center;padding:3rem 1rem}
/* home two-pane: race cards left, leaderboard right (mirrors the cloud dashboard).
   Uses the full screen width (no max-width clamp). */
.wrap.home{max-width:none;width:100%;padding:1rem 1.4rem}
.home-grid{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,720px);gap:1.2rem;align-items:start}
.home-right{position:sticky;top:4rem}
.home-right .card{margin-top:0;overflow-x:auto}
/* Stack below 1500px, not 1100. The leaderboard track is a fixed 720px, so at
   1100 the race cards were left ~330px — narrower than the eight columns of lap
   data need, and the pane with the lap times is the one that matters. 1500 is
   the width at which both panes are comfortable; below it, one full-width
   column under the other beats two cramped ones. */
@media(max-width:1500px){.home-grid{grid-template-columns:minmax(0,1fr)}.home-right{position:static}}
/* Lite (?lite=1). A sticky element keeps its own compositor layer alive for as
   long as the page exists, and the leaderboard scrolls underneath two of them;
   the shadow and the medal gradients are then re-composited against it. None of
   that carries information, so a display that cannot afford it drops it. */
html[data-lite] .top,html[data-lite] .home-right{position:static}
html[data-lite] .card{box-shadow:none}
html[data-lite] .pos.p1{background:#e0a800}
html[data-lite] .pos.p2{background:#9aa6b6}
html[data-lite] .pos.p3{background:#c06a2c}
.lb th,.lb td{white-space:nowrap}
.lb .lbnum{font-variant-numeric:tabular-nums;text-align:right}
/* Leaderboard scroll box — bounded so the auto-scroll has somewhere to travel. */
.lb-viewport{max-height:calc(100vh - 9rem);overflow:auto}
.dd-as{float:right;display:inline-flex;align-items:center;gap:.3rem;font-size:.7rem;font-weight:600;
  color:var(--muted);cursor:pointer;text-transform:none;letter-spacing:0}
.dd-as input{cursor:pointer;margin:0}
.next-racing{color:#76e29a;font-weight:700}
.next-staging{color:var(--accent);font-weight:700}
/* Next Races panel */
.up-race{padding:.55rem 0;border-bottom:1px solid #262b33}
.card.up-race{padding:.9rem 1.1rem;border-bottom:1px solid var(--line);margin-bottom:.6rem}
.card.up-now{border-color:rgba(198,255,61,.45)}
.up-when{margin-left:auto;font-size:.78rem;font-weight:700;color:var(--accent,#c6ff3d);white-space:nowrap}
.up-all{display:inline-block;margin-top:.7rem;font-weight:600;font-size:.9rem;color:var(--accent,#c6ff3d)}
.up-race:last-child{border-bottom:none}
.up-head{display:flex;align-items:baseline;gap:.5rem;margin-bottom:.35rem}
.up-head .sub{margin-left:auto}
.up-pilots{display:flex;flex-wrap:wrap;gap:.4rem}
.up-chip{display:inline-flex;align-items:center;gap:.3rem;background:var(--panel2);border:1px solid var(--line);border-radius:999px;padding:.15rem .55rem;font-size:.82rem}
/* ── Pilot page ────────────────────────────────────────────────────────────
   Deliberately a copy of the cloud dashboard's pilot page (dashboard-pilot.html
   + dronedash-home.css): same header, same five stat cards, same three tabs,
   same lap-highlight fills. A race director moves between the venue board and
   the club site during a meeting, and a page that looks different reads as a
   different — and less trustworthy — number.

   The tabs and the chart's series toggles are CSS-only, driven by hidden
   radio/checkbox inputs. Not a purity exercise: this page soft-refreshes every
   few seconds, and tab state held in classes the SERVER renders would snap back
   to Analytics on every refresh while you were reading the lap table. Held in an
   input's checked property, it survives, because the refresh patches attributes
   and an input the user has touched ignores them. */
.pilot-page{max-width:1200px;margin:0 auto;padding:1rem;display:flex;flex-direction:column;gap:1rem}
.pilot-header{display:flex;flex-direction:column;gap:.75rem}
.pilot-header-main{display:flex;flex-direction:column;gap:.25rem}
.pilot-back-link{color:var(--accent);font-size:.9rem}
.pilot-header-main h1{margin:0;font-size:2rem;line-height:1.2}
.pilot-header-meta{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center}
.pilot-meta-chip{display:inline-flex;align-items:center;gap:.5rem;padding:.35rem .65rem;
  background:var(--panel2);border:1px solid var(--line);border-radius:999px;font-size:.9rem}
@media(min-width:768px){.pilot-header{flex-direction:row;justify-content:space-between;align-items:flex-end}
  .pilot-header-main{max-width:60%}}
.pilot-stat-grid{display:grid;gap:.75rem;grid-template-columns:repeat(auto-fit,minmax(160px,1fr))}
.pilot-stat-card{background:var(--panel2);border:1px solid transparent;border-radius:12px;padding:.75rem;
  display:flex;flex-direction:column;gap:.25rem}
.pilot-stat-label{font-size:.85rem;color:var(--accent);text-transform:uppercase;letter-spacing:.04em}
.pilot-stat-value{font-size:1.3rem;font-weight:600;font-variant-numeric:tabular-nums}
.pilot-stat-subtitle{font-size:.85rem;color:var(--muted)}
a.pilot-stat-card{text-decoration:none;color:inherit;transition:background-color .12s,border-color .12s,transform .12s}
a.pilot-stat-card:hover{border-color:var(--accent);transform:translateY(-1px)}
.pilot-stat-go{color:var(--muted);font-weight:700}
a.pilot-stat-card:hover .pilot-stat-go{color:var(--accent)}
/* Tabs */
.ptab{position:absolute;opacity:0;width:0;height:0;pointer-events:none}
.pilot-tabs{display:flex;gap:.25rem;background:var(--panel2);border-radius:999px;padding:.25rem;align-self:flex-start;width:max-content}
.pilot-tabs label{padding:.5rem 1rem;border-radius:999px;cursor:pointer;font-size:.95rem;color:var(--txt);user-select:none}
.pilot-tab-panel{display:none;background:#141920;border:1px solid var(--line);border-radius:16px;padding:1rem;min-height:240px}
#ptab-analytics:checked~.pilot-tabs label[for=ptab-analytics],
#ptab-laps:checked~.pilot-tabs label[for=ptab-laps],
#ptab-upcoming:checked~.pilot-tabs label[for=ptab-upcoming]{background:var(--accent);color:#0f1115;font-weight:600}
#ptab-laps:checked~[data-panel=laps],
#ptab-upcoming:checked~[data-panel=upcoming]{display:block}
#ptab-analytics:checked~[data-panel=analytics]{display:flex}
/* Analytics */
.pilot-analytics-tab{flex-direction:column;gap:1rem}
.pilot-analytics-controls{display:flex;flex-wrap:wrap;gap:.75rem 1.25rem;align-items:center;justify-content:space-between}
.pilot-series-chips{display:flex;flex-wrap:wrap;gap:.35rem;background:var(--panel2);border-radius:999px;padding:.25rem}
.pilot-series-chip{padding:.35rem .8rem;border-radius:999px;font-size:.85rem;color:var(--txt);
  text-decoration:none;white-space:nowrap}
.pilot-series-chip:hover{background:var(--panel)}
.pilot-series-chip.on{background:var(--accent);color:#0f1115;font-weight:600}
.pilot-overlay-toggles{display:flex;flex-wrap:wrap;gap:.75rem}
.pilot-overlay-toggle{display:inline-flex;align-items:center;gap:.5rem;font-size:.9rem;cursor:pointer;user-select:none}
/* The series checkboxes are drawn rather than native, because the input itself
   has to sit outside the controls row (a sibling of the chart) for the
   :checked~ selectors to reach it without :has(). */
.pser{position:absolute;opacity:0;width:0;height:0;pointer-events:none}
.pser-box{width:14px;height:14px;border:1px solid var(--line);border-radius:3px;background:var(--panel);
  display:inline-block;position:relative}
#pser-best:checked~.pilot-analytics-controls label[for=pser-best] .pser-box{background:var(--accent);border-color:var(--accent)}
.pilot-chart-scroll{overflow-x:auto;background:rgba(12,12,18,.65);border-radius:16px;padding:.25rem}
.pc-grid line{stroke:#262b33}
.pc-axis text{fill:var(--muted);font-size:10px}
.pc-xlabel{fill:var(--muted);font-size:11px}
.pc-sep{stroke:#2f3742;stroke-dasharray:2 4}
.pc-heat{fill:var(--muted);font-size:11px;font-weight:700;letter-spacing:.03em}
.pc-best{stroke:var(--accent);stroke-width:1.5;stroke-dasharray:5 4}
.pc-bestbg{fill:#12161c;stroke:var(--accent);stroke-opacity:.5}
.pc-bestlabel{fill:var(--accent);font-size:10px;font-weight:700}
.pc-bar rect{transition:opacity .12s}
.pc-bar:hover rect{opacity:.75}
#pser-best:not(:checked)~.pilot-chart-scroll .pc-bestline{display:none}
.pilot-chart-note{color:var(--muted);font-size:.8rem}
/* Lap table — the cloud's lap-badge grid, dark fills and all. */
.pilot-lap-group{margin-bottom:1rem}
.pilot-lap-group-head{color:var(--accent);font-size:.85rem;text-transform:uppercase;letter-spacing:.05em;
  margin-bottom:.4rem;display:flex;gap:.5rem;align-items:baseline;flex-wrap:wrap}
.pilot-card-laps{display:flex;flex-wrap:wrap;gap:4px}
.lap-badge{display:flex;flex-direction:column;align-items:center;min-width:52px;padding:3px 6px;
  border-radius:6px;background:var(--panel2);border:1px solid var(--line)}
.lap-badge-label{font-size:.65rem;text-transform:uppercase;color:var(--muted);letter-spacing:.5px;line-height:1;margin-bottom:1px}
.lap-badge-value{font-size:.85rem;font-weight:500;font-variant-numeric:tabular-nums;color:var(--txt)}
.total-badge{background:#2a2f38}
.total-badge .lap-badge-value{font-weight:700}
.lap-badge.lap-race-best{background:#1a472a;border-color:rgba(26,71,42,.6)}
.lap-badge.lap-race-pb{background:#2a2a4a;border-color:rgba(42,42,74,.6)}
.lap-badge.lap-overall-best{background:#4a1a1a;border-color:rgba(74,26,26,.6)}
.lap-badge.lap-overall-pb{background:#1a1a4a;border-color:rgba(26,26,74,.6)}
.lap-badge.pattern-hatched{background-image:repeating-linear-gradient(45deg,rgba(0,0,0,.2),rgba(0,0,0,.2) 2px,transparent 2px,transparent 4px)}
/* Upcoming */
.pilot-upcoming-grid{display:grid;gap:.75rem;grid-template-columns:repeat(auto-fit,minmax(260px,1fr))}
.pilot-upcoming-card{background:var(--panel2);border:1px solid var(--line);border-radius:12px;padding:.75rem}
.pilot-upcoming-card h3{margin:0 0 .2rem;font-size:1rem}
.pilot-upcoming-competitors{display:flex;flex-wrap:wrap;gap:.4rem;margin-top:.5rem}
.pilot-upcoming-competitor{display:inline-flex;align-items:center;gap:.3rem;background:var(--panel);
  border:1px solid var(--line);border-radius:999px;padding:.15rem .55rem;font-size:.82rem}
.pilot-empty{color:var(--muted);text-align:center;padding:2rem}
body.overlay{background:transparent}
body.overlay .top,body.overlay .sub{display:none}

/* ── Theme ────────────────────────────────────────────────────────────────
   A layer over the layout rules above: colour, depth and type only, so every
   column-width and stacking fix above still holds. Lite mode and the OBS
   overlay opt out of the expensive parts (blur, glows, the backdrop image). */
:root{--bg:#07080a;--panel:rgba(22,24,30,.72);--panel2:rgba(34,37,46,.66);--line:rgba(255,255,255,.08);
  --txt:#f2f4f7;--muted:#8e96a3;--cyan:#3de0ff;--glow:0 0 0 1px rgba(255,255,255,.04),0 18px 50px -20px rgba(0,0,0,.8)}
body{font:15px/1.45 -apple-system,BlinkMacSystemFont,"SF Pro Text","Inter","Segoe UI",Roboto,system-ui,sans-serif;
  -webkit-font-smoothing:antialiased;letter-spacing:-.005em;
  background:radial-gradient(1200px 600px at 85% -10%,color-mix(in srgb,var(--accent) 14%,transparent),transparent 60%),
    radial-gradient(900px 500px at -10% 10%,rgba(61,224,255,.08),transparent 60%),var(--bg);background-attachment:fixed;min-height:100vh}
body:not(.overlay)::before{content:"";position:fixed;inset:0;z-index:-1;pointer-events:none;
  background:url(/assets/ambient.webp) center top/cover no-repeat;opacity:.18;mix-blend-mode:screen}
.top{background:rgba(9,10,13,.62);backdrop-filter:saturate(180%) blur(20px);-webkit-backdrop-filter:saturate(180%) blur(20px);
  border-bottom:1px solid var(--line);padding:.6rem 1.2rem}
.top .logo{height:30px;width:30px;border-radius:8px;box-shadow:0 0 18px color-mix(in srgb,var(--accent) 35%,transparent)}
.top .name{font-weight:750;font-size:1.08rem;letter-spacing:-.02em}
.top .name .dd{background:linear-gradient(90deg,var(--accent),var(--cyan));-webkit-background-clip:text;background-clip:text;color:transparent}
.top .meet{color:var(--muted);font-size:.86rem;padding:.2rem .65rem;border:1px solid var(--line);border-radius:999px;background:rgba(255,255,255,.03)}
.top nav a{border:1px solid transparent;border-radius:999px;padding:.38rem .85rem;color:#cfd4dc;font-weight:600;transition:background .15s,color .15s}
.top nav a:hover{background:rgba(255,255,255,.07);border-color:transparent;color:#fff}
.top .bmc{padding:.34rem .75rem;border-radius:999px;background:rgba(255,221,0,.1);border:1px solid rgba(255,221,0,.25);color:#ffdd00;font-weight:600}
.top .bmc:hover{background:rgba(255,221,0,.18)}
.card{background:linear-gradient(180deg,rgba(255,255,255,.035),rgba(255,255,255,.01)),var(--panel);border:1px solid var(--line);
  border-radius:22px;padding:1.2rem 1.35rem;box-shadow:var(--glow);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
.card h2{font-size:1.28rem;font-weight:750;letter-spacing:-.025em}
.card h2 .sub{font-weight:600;margin-left:.35rem}
.badge{letter-spacing:.06em}
.badge.run{background:rgba(52,199,89,.14);color:#5df08a;border:1px solid rgba(93,240,138,.35);position:relative;padding-left:1.15rem}
.badge.run::before{content:"";position:absolute;left:.45rem;top:50%;width:.42rem;height:.42rem;margin-top:-.21rem;border-radius:50%;
  background:#5df08a;box-shadow:0 0 0 0 rgba(93,240,138,.7);animation:ddpulse 1.6s infinite}
@keyframes ddpulse{0%{box-shadow:0 0 0 0 rgba(93,240,138,.6)}70%{box-shadow:0 0 0 .5rem rgba(93,240,138,0)}100%{box-shadow:0 0 0 0 rgba(93,240,138,0)}}
.clock{font-size:2rem;font-weight:800;letter-spacing:-.03em;text-shadow:0 0 28px color-mix(in srgb,var(--accent) 55%,transparent)}
table.rt th{font-size:.68rem;letter-spacing:.09em;font-weight:650;border-bottom:1px solid var(--line)}
table.rt td{border-bottom:1px solid rgba(255,255,255,.05);padding-top:.55rem;padding-bottom:.55rem}
table.rt tbody tr{transition:background .15s}
table.rt tbody tr:hover{background:rgba(255,255,255,.03)}
.pos{border-radius:9px;min-width:2.1rem;background:rgba(255,255,255,.08)}
.pos.p1{box-shadow:0 0 16px rgba(255,200,40,.35)}
.lap{background:rgba(0,0,0,.35);border:1px solid rgba(255,255,255,.1);border-radius:8px;padding:.14rem .45rem}
.lap.best{border-color:color-mix(in srgb,var(--accent) 70%,transparent);color:var(--accent);background:color-mix(in srgb,var(--accent) 10%,transparent)}
.chip.consec,.next-racing{color:var(--accent)}
.chip.consec{background:color-mix(in srgb,var(--accent) 14%,transparent)}
.tile{background:var(--panel2);border-radius:18px;border:1px solid var(--line);border-left:4px solid var(--accent);box-shadow:var(--glow)}
.up-chip,.pilot-meta-chip,.pilot-upcoming-competitor{background:rgba(255,255,255,.05)}
.pilot-stat-card{background:var(--panel2);border:1px solid var(--line);border-radius:18px}
.pilot-tab-panel{background:var(--panel);border-radius:22px}
.empty h2{font-size:1.6rem;letter-spacing:-.03em}
/* The waiting screen is the first thing a new venue sees. */
.hero-wait{position:relative;min-height:calc(100vh - 3.5rem);display:grid;place-items:center;text-align:center;overflow:hidden;padding:2rem 1.2rem}
.hero-wait::before{content:"";position:absolute;inset:0;background:url(/assets/hero.webp) center/cover no-repeat;opacity:.55;
  -webkit-mask-image:linear-gradient(180deg,#000 40%,transparent);mask-image:linear-gradient(180deg,#000 40%,transparent)}
.hero-wait>div{position:relative;max-width:40rem}
.hero-wait h1{font-size:clamp(2.2rem,6vw,4.2rem);line-height:1.02;letter-spacing:-.045em;margin:.6rem 0;font-weight:800}
.hero-wait h1 span{background:linear-gradient(90deg,var(--accent),var(--cyan));-webkit-background-clip:text;background-clip:text;color:transparent}
.hero-wait p{color:#c3c9d2;font-size:1.1rem;margin:.4rem auto 1.4rem}
.hero-wait .pill{display:inline-flex;align-items:center;gap:.55rem;padding:.45rem 1rem;border-radius:999px;background:rgba(0,0,0,.45);
  border:1px solid var(--line);color:#dfe3e8;font-weight:600;font-size:.9rem;backdrop-filter:blur(10px)}
.hero-wait .pill i{width:.55rem;height:.55rem;border-radius:50%;background:var(--accent);animation:ddpulse 1.6s infinite}
.hero-wait .steps{display:inline-block;text-align:left;margin:.2rem auto 1rem;padding:1rem 1.3rem 1rem 2.6rem;border-radius:18px;
  background:rgba(0,0,0,.45);border:1px solid var(--line);backdrop-filter:blur(10px);color:#dfe3e8;line-height:1.7}
.hero-wait .steps b{color:#fff}
.hero-wait .cta{display:inline-flex;gap:.6rem;flex-wrap:wrap;justify-content:center;margin-top:1.2rem}
.hero-wait .cta a{padding:.7rem 1.3rem;border-radius:999px;font-weight:700;background:var(--accent);color:#07080a}
.hero-wait .cta a.sec{background:rgba(255,255,255,.08);color:#fff;border:1px solid var(--line)}
html[data-lite] body{background:var(--bg)}
html[data-lite] body::before,html[data-lite] .hero-wait::before{display:none}
html[data-lite] .top,html[data-lite] .card{backdrop-filter:none;-webkit-backdrop-filter:none}
html[data-lite] .badge.run::before{animation:none}
html[data-lite] .clock{text-shadow:none}
body.overlay{background:transparent!important}
/* OBS overlay: a compact broadcast panel, top-left, readable at stream size. */
.obs{position:fixed;left:28px;top:28px;width:420px;border-radius:18px;overflow:hidden;
  background:linear-gradient(180deg,rgba(14,16,20,.92),rgba(8,9,12,.88));border:1px solid rgba(255,255,255,.1);
  box-shadow:0 20px 50px -18px rgba(0,0,0,.9),0 0 0 1px rgba(198,255,61,.08);font-variant-numeric:tabular-nums}
.obs-head{display:flex;align-items:center;gap:.55rem;padding:.6rem .85rem;
  background:linear-gradient(90deg,color-mix(in srgb,var(--accent) 22%,transparent),transparent 70%);border-bottom:1px solid rgba(255,255,255,.08)}
.obs-head img{width:22px;height:22px;border-radius:6px}
.obs-race{font-weight:800;letter-spacing:-.01em;font-size:1rem}
.obs-live{font-size:.66rem;font-weight:800;letter-spacing:.1em;color:#07080a;background:var(--accent);padding:.12rem .45rem;border-radius:6px}
.obs-clock{margin-left:auto;font-weight:800;font-size:1.15rem;color:var(--accent)}
.obs-row{display:flex;align-items:center;gap:.6rem;padding:.48rem .85rem;border-top:1px solid rgba(255,255,255,.05)}
.obs-row:first-child{border-top:0}
.obs-row .pos{min-width:1.8rem;font-size:.9rem}
.obs-row i{width:4px;height:1.3rem;border-radius:2px;flex:none}
.obs-row b{flex:1;font-size:1.02rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.obs-laps{color:var(--muted);font-size:.85rem;font-weight:700}
.obs-total{font-weight:800;min-width:4.6rem;text-align:right}
body.overlay .card{backdrop-filter:none;-webkit-backdrop-filter:none}
@media(prefers-reduced-motion:reduce){.badge.run::before,.hero-wait .pill i{animation:none}}
.top .name{white-space:nowrap}
@media(max-width:640px){
  .top{gap:.5rem;padding:.55rem .8rem;flex-wrap:nowrap}
  .top .meet,.top .ver{display:none}
  .top nav{flex-wrap:nowrap;overflow-x:auto;scrollbar-width:none;margin-left:auto;min-width:0}
  .top nav::-webkit-scrollbar{display:none}
  .top nav a{padding:.32rem .6rem;white-space:nowrap}
  .top .bmc{font-size:0;padding:.3rem .55rem}.top .bmc::before{content:"☕";font-size:.9rem}
  .card{border-radius:18px;padding:1rem;overflow-x:auto}
  table.rt:not(.lb){min-width:34rem}
  .wrap.home{padding:.8rem}
  .clock{font-size:1.5rem}}
</style>
{{end}}

{{define "js"}}
<script>
(function(){
  // Lite mode — the switch for a weak viewer, set on the VIEWER not the server.
  //
  // The machine serving these pages is not the constrained one; a Raspberry Pi
  // showing them is. So this is per-browser: open the board once with ?lite=1
  // and that display stays lite (remembered, so the nav links between Home /
  // Live / Races keep it). ?lite=0 turns it back off. Everything else — the
  // race director's screen, the OBS sources — is untouched and full speed.
  var lite=(function(){
    try{
      var q=new URLSearchParams(location.search), v=q.get('lite');
      if(v==='1') localStorage.setItem('dd-lite','1');
      else if(v==='0') localStorage.removeItem('dd-lite');
      return localStorage.getItem('dd-lite')==='1';
    }catch(e){ return new URLSearchParams(location.search).get('lite')==='1'; }
  })();
  window.__ddLite=lite;
  if(lite) document.documentElement.setAttribute('data-lite','1');

  var ms={{.Refresh}}; if(lite) ms=Math.max(ms*4,10000);
  // A tenth-of-a-second clock is ten layout invalidations a second. On a Pi that
  // is worth more than the extra digit, so lite counts in whole seconds at 2Hz.
  var clockMs=lite?500:100, clockDp=lite?0:1;
  // The running-race clock, ten times a second — but made nearly free.
  //
  // This used to querySelectorAll the whole document on every tick, running
  // whether or not a race was live and whether or not the tab was even visible.
  // Ten full DOM queries a second is unnoticeable on a laptop and is exactly
  // what makes a Raspberry Pi pits display feel broken. The nodes are cached
  // instead (re-scanned only when the page content actually changes), so an
  // idle board does no work at all.
  var clocks=[];
  function rescanClocks(){ clocks=Array.prototype.slice.call(
    document.querySelectorAll('[data-clock-start]')); }
  function tickClock(){
    if(!clocks.length || document.hidden) return;
    for(var i=0;i<clocks.length;i++){
      var el=clocks[i];
      var st=parseInt(el.getAttribute('data-clock-start'),10); if(!st)continue;
      el.textContent=((Date.now()-st)/1000).toFixed(clockDp);
    }
  }
  rescanClocks(); setInterval(tickClock,clockMs); tickClock();

  // Swap #app with the freshly-rendered page. busy/pending coalesces a burst of
  // updates (several laps within one poll) into a single in-flight fetch.
  var busy=false, pending=false, lastHTML='';

  // Partial updates, matched BY KEY.
  //
  // This is the second attempt. The first paired children by POSITION, which
  // corrupted the race table: pilots reorder as laps land, so row one was
  // patched against row three cell by cell and the table showed one pilot's
  // name with another's lap times. Plausible-looking wrong data, which is the
  // dangerous kind.
  //
  // The rows now carry data-key="<pilot>" from the server — identity that does
  // NOT change when the order does. Lists whose children are keyed are matched
  // on that key; anything else is replaced wholesale rather than guessed at.
  // The whole point is the Pi: replacing ~5,600 nodes costs ~95ms on a laptop
  // and several times that on a Pi, and almost none of it is necessary.
  function keyOf(el){ return el.getAttribute ? (el.getAttribute('data-key')||'') : ''; }
  // A fixed copy, never the live collection: patch() moves nodes out of the
  // new tree (replaceWith), which would shift a live list under the loop and
  // skip the next child — leaving a stale row behind and dropping a new one.
  function kids(el){ return Array.prototype.slice.call(el.children); }
  function keyedChildren(el){
    var c=kids(el);
    if(!c.length) return null;
    for(var i=0;i<c.length;i++){ if(!keyOf(c[i])) return null; }
    return c;
  }
  function countNodes(el){ return 1+el.getElementsByTagName('*').length; }
  function syncAttrs(o,n){
    for(var i=0;i<n.attributes.length;i++){
      var a=n.attributes[i];
      if(o.getAttribute(a.name)!==a.value) o.setAttribute(a.name,a.value);
    }
    for(var j=o.attributes.length-1;j>=0;j--){
      var b=o.attributes[j];
      if(!n.hasAttribute(b.name)) o.removeAttribute(b.name);
    }
  }

  // reorderable: repeated same-tag children with no key. Cannot be paired
  // safely, so such a branch is replaced rather than patched.
  function reorderable(el){
    var c=el.children, seen={};
    if(c.length<2) return false;
    for(var i=0;i<c.length;i++){
      if(keyOf(c[i])) continue;
      var t=c[i].nodeName;
      if(seen[t]) return true;
      seen[t]=true;
    }
    return false;
  }

  function patch(o,n){
    // The running clock is owned by tickClock, not the server: patching it reset
    // the display and swapped out the node the timer was holding.
    if(o.hasAttribute && o.hasAttribute('data-clock-start')){ syncAttrs(o,n); return 0; }
    if(o.nodeName!==n.nodeName){ o.replaceWith(n); return countNodes(n); }
    if(o.isEqualNode(n)) return 0;

    var ok=keyedChildren(o), nk=keyedChildren(n);
    if(ok&&nk){
      syncAttrs(o,n);
      // Index the existing rows by key, then walk the NEW order, moving and
      // patching each one into place. A key only in the new set is inserted; one
      // only in the old set is dropped.
      var have={}, i;
      for(i=0;i<ok.length;i++) have[keyOf(ok[i])]=ok[i];
      var changed=0, seenKeys={};
      for(i=0;i<nk.length;i++){
        var nc=nk[i], k=keyOf(nc); seenKeys[k]=true;
        var cur=have[k];
        if(cur){
          changed+=patch(cur,nc);
          // patch() may have swapped cur out for nc (replaceWith); move
          // whichever is now in the page, never the discarded node, or the
          // old row comes back as a duplicate.
          var node=cur.parentNode===o?cur:nc;
          if(o.children[i]!==node) o.insertBefore(node,o.children[i]||null); // reorder
        } else {
          o.insertBefore(nc.cloneNode(true),o.children[i]||null);
          changed+=countNodes(nc);
        }
      }
      for(i=o.children.length-1;i>=0;i--){
        var oc=o.children[i];
        if(!seenKeys[keyOf(oc)]){ oc.remove(); changed++; }
      }
      return changed;
    }
    if(o.children.length!==n.children.length){ o.replaceWith(n); return countNodes(n); }
    if(o.children.length===0){ o.replaceWith(n); return 1; }
    if(reorderable(o)){ o.replaceWith(n); return countNodes(n); }
    syncAttrs(o,n);
    var ch=0, oc=kids(o), ncs=kids(n);
    for(var x=0;x<oc.length;x++) ch+=patch(oc[x],ncs[x]);
    return ch;
  }

  // Superseded note kept short: the old wholesale-replace path lives on as the
  // fallback inside patch() for anything it cannot identify.
  //
  // Two attempts at it both shipped bugs, and both were worse than the problem.
  // Pairing children by index corrupted the race table — pilots REORDER as laps
  // land, so row one was patched against row three cell by cell and the table
  // showed one pilot's name three times with another's lap times spliced in.
  // Not obviously broken, which is the dangerous kind: a race director could
  // read it and believe it.
  //
  // Guarding that by refusing to pair unkeyed repeated children then replaced
  // whole branches on every poll instead — measured at 96,934 elements in
  // twenty-five seconds, roughly seventeen times the page per second, which
  // saturated the main thread and stalled the clock.
  //
  // A correct partial update needs stable per-row identity rendered by the
  // server, which is a real change to the templates rather than a clever walk
  // over the DOM. Until then this replaces #app wholesale, which is honest and
  // fast enough — the win that mattered was not rebuilding at all unless
  // something actually changed, and that is the guard in refresh() below.
  // Coalesce bursts: at most one redraw per redrawGapMs.
  //
  // Measured with laps landing steadily: thirty-four laps produced TWENTY-NINE
  // full redraws in thirty seconds, and 2.7 seconds of main-thread stalls —
  // roughly a redraw a second, each one blocking. On a Raspberry Pi those stalls
  // are several times longer, which is the sluggishness people actually feel.
  //
  // Nothing is dropped: a redraw that arrives too soon is remembered and runs
  // when the window opens, so the newest data always lands. It just lands as one
  // redraw instead of five. Half a second of latency on a spectator board is not
  // worth nine per cent of the main thread.
  //
  // Lite raises the gap to three seconds. The server keeps pushing every change
  // as it happens; this is the one display deciding it cannot draw that often.
  var redrawGapMs=lite?3000:700, lastDraw=0, holdT=null, etag='';
  async function refresh(){
    if(busy){ pending=true; return; }
    var since=Date.now()-lastDraw;
    if(since<redrawGapMs){
      // Inside the window — schedule one run for when it opens, once.
      if(!holdT){ holdT=setTimeout(function(){ holdT=null; refresh(); }, redrawGapMs-since); }
      return;
    }
    busy=true; lastDraw=Date.now();
    try{
      // Fetch the BODY only, and let the server say "unchanged" before it sends
      // a single byte of it. The old path re-fetched the whole document — head,
      // 7KB stylesheet, 14KB of this script — and re-parsed all of it through
      // DOMParser on every update, which is most of the per-update cost on a
      // slow display and none of it was ever used.
      var u=new URL(location.href); u.searchParams.set('fragment','1');
      var hdr={}; if(etag) hdr['If-None-Match']=etag;
      var r=await fetch(u.toString(),{cache:'no-store',headers:hdr});
      if(r.status===304){ busy=false; pending=false; return; }
      var et=r.headers.get('ETag'); if(et) etag=et;
      var t=await r.text();
      // Nothing MEANINGFUL changed? Then do not touch the DOM.
      //
      // Replacing #app is a full reflow of ~5,600 nodes, and it was happening
      // on every SSE tick and every poll. Measured with a race running: nine
      // complete rebuilds in fifteen seconds. That is what makes a Raspberry Pi
      // pits display painful to watch.
      //
      // A plain string compare did not help, because the server renders the
      // running clock INTO the html — so every response differed by a few
      // hundredths of a second and the guard never matched. The clock is
      // already advanced client-side by tickClock, so its text is excluded from
      // the comparison: a poll where only the clock moved is not a change worth
      // rebuilding the page for, and the timer keeps running regardless.
      //
      // The ETag above now catches this case a step earlier, before the body is
      // sent at all. This stays as the backstop for a response that reaches us
      // without one (a proxy stripping headers, an older server).
      var norm=t.replace(/(data-clock-start="[^"]*"[^>]*>)[^<]*/g,'$1');
      if(norm===lastHTML){ busy=false; if(pending){pending=false;} return; }
      lastHTML=norm;
      // Parse the fragment in a detached div rather than building a whole
      // document: no <head>, no stylesheet construction, no script parsing.
      // patch() compares node against node, so the holder must look like the
      // #app it will be diffed against.
      var n=document.createElement('div'); n.id='app'; n.innerHTML=t;
      var o=document.getElementById('app');
      if(n&&o){
        // Preserve the leaderboard scroll position across the full #app swap, so
        // auto-scroll (and a manual scroll) don't jump back to the top each poll.
        var lb=document.querySelector('.lb-viewport'); var lbTop=lb?lb.scrollTop:0;
        patch(o,n); rescanClocks(); tickClock();
        var lb2=document.querySelector('.lb-viewport'); if(lb2) lb2.scrollTop=lbTop;
        if(window.__ddSyncAutoscroll) window.__ddSyncAutoscroll();
      }
    }catch(e){}
    busy=false;
    if(pending){ pending=false; setTimeout(refresh,40); }
  }

  // Realtime: the server pushes an SSE 'tick' the instant new timing data lands
  // (lap recorded, race started/stopped). Reconnects automatically if dropped.
  var es=null, reconnectT=null, lastActivity=Date.now();
  function markAlive(){ lastActivity=Date.now(); }
  var pollT=null;
  function connect(){
    if(es) return; // already connected (e.g. pageshow after explicit connect)
    // Inside a frame — the overlay gallery's previews — poll instead. Each live
    // stream holds a connection open, and a browser allows only six per site
    // over plain HTTP: twenty previews streaming would stall the gallery and
    // every other page on this host.
    if(window.self!==window.top){ if(!pollT) pollT=setInterval(refresh,2500); return; }
    try{
      es=new EventSource('/events');
      es.onopen=markAlive;
      es.onmessage=function(){ markAlive(); refresh(); };
      es.addEventListener('ping', markAlive); // server keepalive heartbeat
      es.onerror=function(){ disconnect(); reconnectT=setTimeout(connect,2000); };
    }catch(e){ reconnectT=setTimeout(connect,2000); }
  }
  function disconnect(){
    clearTimeout(reconnectT); reconnectT=null;
    if(es){ try{es.close();}catch(_){} es=null; }
  }
  // Self-heal watchdog: the server pings every 15s, so a silent gap well past
  // that means the stream is dead even though no error fired — reconnect and
  // re-sync. onerror covers hard drops; this covers silent stalls (proxy/NAT
  // idle-close, laptop wake) so the local board never needs a manual refresh.
  setInterval(function(){
    if(document.hidden) return;
    if(Date.now()-lastActivity > 30000){ markAlive(); disconnect(); connect(); refresh(); }
  }, 10000);
  // Close the stream whenever the page is hidden — including when it's frozen
  // into the back/forward cache — and reopen when it's shown again. Without
  // this, a bfcached page keeps its SSE socket open; a few "back to live" hops
  // exhaust the browser's per-origin connection limit and every tab on this
  // host stalls ("keeps loading"). pageshow also fires on the initial load and
  // on bfcache restore, so it doubles as the connect trigger.
  window.addEventListener('pageshow', connect);
  window.addEventListener('pagehide', disconnect);
  connect();

  // Fallback poll — covers a stalled/unsupported SSE stream so the page still
  // self-heals even if the push channel is silently dropped. Skipped while the
  // tab is hidden so background tabs don't pile up connections either.
  if(ms>0){ setInterval(function(){ if(!document.hidden) refresh(); },ms); }
})();

// Leaderboard auto-scroll — mirrors the cloud dashboard. Gently ping-pong scrolls
// the leaderboard's own scroll box (.lb-viewport) so viewers on a stream/wall see
// the whole field hands-free. Toggle with the header checkbox (on by default).
// Resolves the viewport fresh each frame — the whole #app is replaced every poll,
// so the box is recreated; refresh() preserves its scrollTop and re-syncs the box.
(function(){
  var SPEED=28.8, PAUSE_MS=2500, RESUME_MS=4000, STORE='dd-autoscroll';
  var raf=null, active=false, dir=1, pauseUntil=0, lastTs=0, manualUntil=0;
  // Lite pages the board instead of gliding it.
  //
  // The smooth scroll is a requestAnimationFrame loop that runs FOREVER, writing
  // scrollTop sixty times a second — a repaint and composite per frame, on the
  // largest table on the page, whether or not anything is happening. It is the
  // one thing here with no idle state, and on a weak GPU it is a tax on every
  // other thing the display is trying to do. Lite jumps a screen at a time: same
  // job, one repaint every few seconds instead of sixty a second.
  var LITE=!!window.__ddLite, LITE_HOLD_MS=4500;
  function vp(){ return document.querySelector('.lb-viewport'); }
  function enabled(){ try{ return localStorage.getItem(STORE)!=='0'; }catch(e){ return true; } }
  function maxScroll(el){ return el?el.scrollHeight-el.clientHeight:0; }
  function stop(){
    active=false;
    if(raf){ if(LITE) clearTimeout(raf); else cancelAnimationFrame(raf); raf=null; }
  }
  function liteStep(){
    if(!active)return;
    var el=vp(); if(!el||maxScroll(el)<=8){ stop(); return; }
    var max=maxScroll(el), next=el.scrollTop+el.clientHeight*0.85*dir;
    if(next>=max){ next=max; dir=-1; } else if(next<=0){ next=0; dir=1; }
    el.scrollTop=next;
    raf=setTimeout(liteStep,LITE_HOLD_MS);
  }
  function start(){
    if(active||!enabled())return;
    var el=vp(); if(!el||maxScroll(el)<=8)return;
    active=true; dir=el.scrollTop<=4?1:dir; lastTs=0; pauseUntil=0;
    if(LITE){ raf=setTimeout(liteStep,LITE_HOLD_MS); return; }
    raf=requestAnimationFrame(step);
  }
  function step(ts){
    if(!active)return;
    var el=vp(); if(!el||maxScroll(el)<=8){ stop(); return; }
    if(!lastTs)lastTs=ts; var dt=ts-lastTs; lastTs=ts;
    if(ts<pauseUntil){ raf=requestAnimationFrame(step); return; }
    var max=maxScroll(el), next=el.scrollTop+(SPEED*dt/1000)*dir;
    if(next>=max){ next=max; dir=-1; pauseUntil=ts+PAUSE_MS; }
    else if(next<=0){ next=0; dir=1; pauseUntil=ts+PAUSE_MS; }
    el.scrollTop=next; raf=requestAnimationFrame(step);
  }
  function onInteract(){ if(!enabled())return; stop(); manualUntil=Date.now()+RESUME_MS; }
  ['wheel','touchstart','touchmove','pointerdown','keydown'].forEach(function(ev){
    document.addEventListener(ev,function(e){ var el=vp(); if(el&&e.target&&(el===e.target||el.contains(e.target))) onInteract(); },{passive:true});
  });
  document.addEventListener('visibilitychange',function(){ if(document.hidden)stop(); else if(enabled()&&Date.now()>=manualUntil)start(); });
  setInterval(function(){ if(enabled()&&!active&&Date.now()>=manualUntil)start(); },1500);
  // Re-sync the (recreated) checkbox after each #app swap and on first load.
  window.__ddSyncAutoscroll=function(){
    var t=document.getElementById('dd-autoscroll-toggle');
    if(!t)return;
    try{ t.checked=enabled(); }catch(e){}
    if(!t.__wired){
      t.__wired=true;
      t.addEventListener('change',function(){
        try{ localStorage.setItem(STORE,t.checked?'1':'0'); }catch(e){}
        manualUntil=0; if(t.checked)start(); else stop();
      });
    }
  };
  window.__ddSyncAutoscroll();
  if(enabled())start();
})();
</script>
{{end}}

{{define "shell-top"}}
<div class="top">
  {{if .Brand.LogoB64}}<img src="{{.Brand.LogoB64}}" alt="">{{else}}<a href="/"><img class="logo" src="/assets/icon-256.webp" alt="dd-pits"></a>{{end}}
  <span class="name">{{if eq .Brand.Name "dd-pits"}}<span class="dd">dd</span>-pits{{else}}{{.Brand.Name}}{{end}}</span>
  {{if .M}}<span class="meet">{{.M.Name}}</span>{{end}}
  <nav>
    <a href="/">Home</a><a href="/live">Live</a><a href="/dronedash/upcoming">Upcoming</a><a href="/dronedash/races">Races</a><a href="/stages">Brackets</a><a href="/overlays">Overlays</a>
  </nav>
  <a class="bmc" href="` + supportURL + `" target="_blank" rel="noopener" title="dd-pits is free — support the developer">☕ Support</a>
  {{if .Brand.Version}}<span class="ver" title="dd-pits version — quote this when reporting a problem">{{.Brand.Version}}</span>{{end}}
</div>
{{end}}

{{define "race-rows"}}
<table class="rt">
  <thead><tr><th>Pos</th><th>Name</th><th>Chan</th><th>HS</th><th>Laps</th><th>Total</th>{{if .IsTT}}<th>Best {{.ConsecWindow}}</th><th>PB</th>{{end}}</tr></thead>
  <tbody>
  {{range .Pilots}}
    <tr data-key="{{.Name}}">
      <td data-key="pos"><span class="pos{{if le .Position 3}} p{{.Position}}{{end}}">{{.Position}}</span></td>
      <td data-key="name" class="pname"><a class="plink" href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></td>
      <td data-key="chan">{{if .ChannelColor}}<span class="dot" style="background:{{.ChannelColor}}"></span>{{end}}{{.ChannelLabel}}</td>
      <td data-key="hs">{{f3 .Holeshot}}</td>
      <td data-key="laps"><span class="laps">{{$best:=.BestLap}}{{range $i, $lap := .DispLaps}}<span data-key="l{{$i}}" class="lap{{if and (gt $best 0.0) (le $lap $best)}} best{{end}}">{{f3 $lap}}</span>{{end}}</span></td>
      <td data-key="total">{{f3 .Total}}</td>
      {{if $.IsTT}}<td data-key="consec"><span class="chip consec">{{f2 .BestConsec}}</span></td><td data-key="pb"><span class="chip">{{f2 .PBConsec}}</span></td>{{end}}
    </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{define "leaderboard"}}
{{if .M.Board}}
<div class="card"><h2>Leaderboard <span class="sub">best {{.M.Window}} consec</span>
  <label class="dd-as" title="Auto-scroll the leaderboard"><input type="checkbox" id="dd-autoscroll-toggle" checked> Auto-scroll</label></h2>
<div class="lb-viewport">
<table class="rt lb">
  <thead><tr><th>#</th><th>Pilot</th><th>Chan</th><th class="lbnum">Laps</th><th class="lbnum">Hole shot</th><th class="lbnum">Top Lap</th><th class="lbnum">Top {{.M.Window}} Consec</th><th>Next Race In</th></tr></thead>
  <tbody>
  {{range .M.Board}}
    <tr data-key="{{.Name}}">
      <td data-key="pos"><span class="pos{{if le .Pos 3}} p{{.Pos}}{{end}}">{{.Pos}}</span></td>
      <td data-key="name" class="pname"><a class="plink" href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></td>
      <td data-key="chan">{{if .ChannelColor}}<span class="dot" style="background:{{.ChannelColor}}"></span>{{end}}{{.ChannelLabel}}</td>
      <td data-key="laps" class="lbnum">{{.Laps}}</td>
      <td data-key="hs" class="lbnum">{{if gt .Holeshot 0.0}}{{f3 .Holeshot}}{{else}}<span class="muted">—</span>{{end}}</td>
      <td data-key="best" class="lbnum">{{if gt .BestLap 0.0}}{{f3 .BestLap}}{{else}}<span class="muted">—</span>{{end}}</td>
      <td data-key="consec" class="lbnum">{{if gt .BestConsec 0.0}}<span class="chip consec">{{f3 .BestConsec}}</span>{{else}}<span class="muted">—</span>{{end}}</td>
      <td data-key="next">{{if eq .NextStatus "Racing"}}<span class="next-racing">Racing</span>{{else if eq .NextStatus "Staging"}}<span class="next-staging">Staging</span>{{else if .NextStatus}}<span class="muted">{{.NextStatus}}</span>{{else}}<span class="muted">—</span>{{end}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>
</div>
{{end}}
{{end}}

{{/* Every page is a shell plus a "-body" partial. The soft-refresh fetches the
     body ALONE (?fragment=1), so an update no longer re-ships and re-parses the
     head, the stylesheet and the script. Keep the two in lock-step: the shell's
     <div id="app"> must contain nothing but its body partial, or a refresh will
     silently drop whatever else was in there. */}}

{{define "waiting"}}<!doctype html><html><head><meta charset="utf-8"><title>dd-pits — waiting for {{timer}}</title>{{template "css" .}}</head>
<body>{{template "shell-top" .}}<div id="app">{{template "waiting-body" .}}</div>{{template "js" (dictRefresh 2000 .)}}</body></html>{{end}}

{{define "waiting-body"}}<section class="hero-wait"><div>
  <span class="pill"><i></i>Waiting for {{timer}}</span>
  <h1>Race day, <span>live.</span></h1>
  {{if eq timer "RotorHazard"}}
  <p>Set up your event in RotorHazard and this board lights up on its own: heats, lap times and standings, on every phone at the field.</p>
  <ol class="steps">
    <li>In RotorHazard, add your pilots and heats, and pick the heat to race.</li>
    <li>Check the dd-pits plugin is running: <b>Settings → dd-pits</b>.</li>
    <li>This page updates by itself. Nothing else to do.</li>
  </ol>
  {{else}}
  <p>Open your event in FPVTrackside and this board lights up on its own: heats, lap times and standings, on every phone at the field.</p>
  <ol class="steps">
    <li>In FPVTrackside: <b>Menu → Application Settings → Web</b>, tick <b>Auto start HTTP Server</b>.</li>
    <li>Restart FPVTrackside and open your event.</li>
    <li>This page updates by itself. Nothing else to do.</li>
  </ol>
  {{end}}
  <div class="cta"><a href="/setup/help">Get set up</a><a class="sec" href="/setup">Settings</a><a class="sec" href="/overlays">OBS overlays</a></div>
  <p style="margin-top:1.4rem;font-size:.9rem;color:#9aa3af">Free and open source · made by <a href="https://github.com/bob9" target="_blank" rel="noopener">bob9</a></p>
</div></section>{{end}}

{{define "home"}}<!doctype html><html><head><meta charset="utf-8"><title>{{.Brand.Name}} — Live</title>{{template "css" .}}</head>
<body>{{template "shell-top" .}}<div id="app">{{template "home-body" .}}</div>{{template "js" (dictRefresh 2500 .)}}</body></html>{{end}}

{{define "home-body"}}<div class="wrap home"><div class="home-grid">
  <div class="home-left">
  {{with .M.Current}}
  <div class="card">
    <div style="display:flex;align-items:center;gap:.5rem">
      <h2>Current Race <span class="sub">{{.Label}}</span>{{if eq .State "running"}}<span class="badge run">LIVE</span>{{end}}</h2>
      {{if eq .State "running"}}<span class="clock" data-clock-start="{{.StartMs}}">0.0</span>{{end}}
    </div>
    {{template "race-rows" .}}
  </div>
  {{else}}<div class="empty"><h2>No current race</h2></div>{{end}}
  <div class="card"><h2>Next Races <span class="sub">{{if .M.Upcoming}}{{.M.UpcomingTotal}} upcoming{{else}}Pending{{end}}</span></h2>
    {{if not .M.Upcoming}}<div class="sub" style="padding:.4rem 0">No upcoming race set yet.</div>{{end}}
    {{range .M.Upcoming}}
    <div class="up-race">
      <div class="up-head"><strong>{{.Label}}</strong><span class="sub">{{len .Pilots}} pilot{{if ne (len .Pilots) 1}}s{{end}}</span></div>
      <div class="up-pilots">{{range .Pilots}}<span class="up-chip">{{if .ChannelColor}}<span class="dot" style="background:{{.ChannelColor}}"></span>{{end}}<span class="pname"><a class="plink" href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></span> <span class="muted">{{.ChannelLabel}}</span></span>{{end}}</div>
    </div>{{end}}
    {{if gt .M.UpcomingTotal (len .M.Upcoming)}}<a class="up-all" href="/dronedash/upcoming">See all {{.M.UpcomingTotal}} upcoming races →</a>{{end}}
  </div>
  {{if .M.Recent}}
  <div class="card"><h2>Recent races</h2>
    {{range .M.Recent}}<div style="margin:.6rem 0"><div class="sub" style="font-weight:700;color:var(--txt)">{{.Label}}</div>{{template "race-rows" .}}</div>{{end}}
  </div>{{end}}
  </div>
  <div class="home-right">
    {{template "leaderboard" .}}
  </div>
</div></div>{{end}}

{{define "upcoming"}}<!doctype html><html><head><meta charset="utf-8"><title>{{.Brand.Name}} — Upcoming races</title>{{template "css" .}}</head>
<body>{{template "shell-top" .}}<div id="app">{{template "upcoming-body" .}}</div>{{template "js" (dictRefresh 3000 .)}}</body></html>{{end}}

{{define "upcoming-body"}}<div class="wrap">
  <div class="card" data-key="up-head"><h2>Upcoming races <span class="sub">{{len .M.AllUpcoming}} to run</span></h2>
    <div class="sub">In running order. Find your name to see when you fly.</div></div>
  {{if .M.Current}}<div class="card up-now" data-key="up-now"><div class="up-head"><strong>{{if .M.Current.StartMs}}Racing now{{else}}On deck{{end}} · {{.M.Current.Label}}</strong></div>
    <div class="up-pilots">{{range .M.Current.Pilots}}<span class="up-chip">{{if .ChannelColor}}<span class="dot" style="background:{{.ChannelColor}}"></span>{{end}}<span class="pname"><a class="plink" href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></span> <span class="muted">{{.ChannelLabel}}</span></span>{{end}}</div></div>{{end}}
  {{range $i, $r := .M.AllUpcoming}}<div class="card up-race" data-key="up-{{$r.ID}}">
    <div class="up-head"><strong>{{$r.Label}}</strong><span class="up-when">{{if eq $i 0}}Next{{else}}In {{add $i 1}} races{{end}}</span></div>
    <div class="up-pilots">{{range $r.Pilots}}<span class="up-chip">{{if .ChannelColor}}<span class="dot" style="background:{{.ChannelColor}}"></span>{{end}}<span class="pname"><a class="plink" href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></span> <span class="muted">{{.ChannelLabel}}</span></span>{{end}}</div>
  </div>{{else}}<div class="empty" data-key="up-none">No upcoming races yet. They appear here as soon as the timer has them.</div>{{end}}
</div>{{end}}

{{define "races"}}<!doctype html><html><head><meta charset="utf-8"><title>{{.Brand.Name}} — Race history</title>{{template "css" .}}</head>
<body>{{template "shell-top" .}}<div id="app">{{template "races-body" .}}</div>{{template "js" (dictRefresh 4000 .)}}</body></html>{{end}}

{{define "races-body"}}<div class="wrap">
  <div class="card"><h2>Race history <span class="sub">{{len .M.Recent}} completed</span></h2></div>
  {{/* id= is the anchor the pilot page's stat cards and lap bars jump to. */}}
  {{range .M.Recent}}<div class="card" id="race-{{.ID}}"><h2 style="font-size:1.02rem">{{.Label}}{{if .IsTT}}<span class="badge">Time Trial</span>{{end}}{{with watchLink .ID .StartMs}}<a href="{{.}}" class="watch">&#9654; Watch</a>{{end}}</h2>{{template "race-rows" .}}</div>{{else}}<div class="empty">No completed races yet.</div>{{end}}
</div>{{end}}

{{define "live"}}<!doctype html><html><head><meta charset="utf-8"><title>{{.Brand.Name}} — Live</title>{{template "css" .}}</head>
<body>{{template "shell-top" .}}<div id="app">{{template "live-body" .}}</div>{{template "js" (dictRefresh 800 .)}}</body></html>{{end}}

{{define "live-body"}}<div class="wrap">
  {{with .M.Current}}{{$race := .}}
  <div style="display:flex;align-items:center;gap:.6rem;margin-bottom:.6rem"><h2 style="margin:0">{{.Label}}</h2>{{if eq .State "running"}}<span class="badge run">LIVE</span><span class="clock" data-clock-start="{{.StartMs}}">0.0</span>{{end}}</div>
  <div class="grid">
    {{range .Pilots}}
    <div class="tile" style="{{if .ChannelColor}}border-left-color:{{.ChannelColor}}{{end}}">
      <div class="h"><span class="pn"><a class="plink" href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></span><span class="pp">P{{.Position}}</span></div>
      <div class="kv">
        <div><span class="k">Chan</span><span class="v">{{.ChannelLabel}}</span></div>
        <div><span class="k">Laps</span><span class="v">{{.CountedLaps}}</span></div>
        <div><span class="k">Total</span><span class="v">{{f3 .Total}}</span></div>
        {{if $race.IsTT}}<div><span class="k">Best {{$race.ConsecWindow}}</span><span class="v">{{f2 .BestConsec}}</span></div><div><span class="k">PB</span><span class="v">{{f2 .PBConsec}}</span></div>{{end}}
      </div>
      <div class="laps" style="margin-top:.5rem">{{$best:=.BestLap}}{{range .DispLaps}}<span class="lap{{if and (gt $best 0.0) (le . $best)}} best{{end}}">{{f3 .}}</span>{{end}}</div>
    </div>
    {{end}}
  </div>
  {{else}}<div class="empty"><h2>No race running</h2></div>{{end}}
</div>{{end}}

{{/* ── Pilot page ──────────────────────────────────────────────────────────
     The local twin of the cloud's dashboard-pilot.html: same header, same five
     stat cards, same Analytics / Lap Table / Upcoming Races tabs. Every stat
     that resolves to a heat links into the race history at that heat, which is
     the point of the page at a meeting — from "my fastest three consecutive"
     straight to the heat, and its ▶ Watch link. */}}

{{define "pilot"}}<!doctype html><html><head><meta charset="utf-8"><title>{{if .P}}{{.P.Name}} — {{end}}{{.Brand.Name}}</title>{{template "css" .}}</head>
<body>{{template "shell-top" .}}<div id="app">{{template "pilot-body" .}}</div>{{template "js" (dictRefresh 3000 .)}}</body></html>{{end}}

{{define "pilot-body"}}
{{- if not .P -}}
<div class="empty"><h2>Pilot not found</h2><p>Nobody by that name is in this meeting.</p><p><a href="/">← Back to the dashboard</a></p></div>
{{- else -}}
<div class="pilot-page">
  <header class="pilot-header">
    <div class="pilot-header-main">
      <a class="pilot-back-link" href="/">← Back to dashboard</a>
      <h1>{{.P.Name}}</h1>
    </div>
    <div class="pilot-header-meta">
      {{if .P.Pos}}<div class="pilot-meta-chip"><span class="pos{{if le .P.Pos 3}} p{{.P.Pos}}{{end}}">{{.P.Pos}}</span> on the leaderboard</div>{{end}}
      {{if .P.ChannelLabel}}<div class="pilot-meta-chip">{{if .P.ChannelColor}}<span class="dot" style="background:{{.P.ChannelColor}}"></span>{{end}}{{.P.ChannelLabel}}</div>{{end}}
    </div>
  </header>

  <section class="pilot-stat-grid" aria-label="Pilot overview statistics">
    {{template "pstat" (dict "Label" "Best Lap" "S" .P.BestLap "Value" (f3 .P.BestLap.Value) "Sub" .P.BestLap.Subtitle)}}
    {{/* The cloud prefixes this one with "N laps ·"; here the subtitle already
         names the run ("Laps 1–2 · Round 2 — Race 1"), which says the same
         thing and also says WHICH laps, so the prefix would only repeat it. */}}
    {{template "pstat" (dict "Label" (printf "Fastest %d Consecutive" .P.Window) "S" .P.BestConsec "Value" (f3 .P.BestConsec.Value) "Sub" .P.BestConsec.Subtitle)}}
    {{template "pstat" (dict "Label" "Fastest Total Race" "S" .P.FastestRace "Value" (f3 .P.FastestRace.Value) "Sub" .P.FastestRace.Subtitle)}}
    {{template "pstat" (dict "Label" "Holeshot" "S" .P.Holeshot "Value" (f3 .P.Holeshot.Value) "Sub" .P.Holeshot.Subtitle)}}
    <div class="pilot-stat-card">
      <div class="pilot-stat-label">Total Laps</div>
      <div class="pilot-stat-value">{{.P.TotalLaps}}</div>
      <div class="pilot-stat-subtitle">Completed laps this meeting</div>
    </div>
  </section>

  <input type="radio" name="ptab" class="ptab" id="ptab-analytics" checked>
  <input type="radio" name="ptab" class="ptab" id="ptab-laps">
  <input type="radio" name="ptab" class="ptab" id="ptab-upcoming">
  <nav class="pilot-tabs">
    <label for="ptab-analytics">Analytics</label>
    <label for="ptab-laps">Lap Table</label>
    <label for="ptab-upcoming">Upcoming Races</label>
  </nav>

  <div class="pilot-tab-panel pilot-analytics-tab" data-panel="analytics">
    <input type="checkbox" class="pser" id="pser-best" checked>
    <div class="pilot-analytics-controls">
      {{/* Series chips are LINKS, not checkboxes: the chart is laid out again
           for the chosen series (rescaled axis, re-packed bars), which CSS
           cannot do. ?series= rides on the soft-refresh URL, so it sticks. */}}
      <div class="pilot-series-chips" role="group" aria-label="Chart series">
        <a class="pilot-series-chip{{if eq .P.Series "all"}} on{{end}}" href="/dronedash/pilot?name={{.P.Name}}">Laps + holeshots</a>
        <a class="pilot-series-chip{{if eq .P.Series "laps"}} on{{end}}" href="/dronedash/pilot?name={{.P.Name}}&amp;series=laps">Laps only</a>
        <a class="pilot-series-chip{{if eq .P.Series "hs"}} on{{end}}" href="/dronedash/pilot?name={{.P.Name}}&amp;series=hs">Holeshots only</a>
      </div>
      <div class="pilot-overlay-toggles">
        <label class="pilot-overlay-toggle" for="pser-best"><span class="pser-box"></span> {{if eq .P.Series "hs"}}Best holeshot{{else}}Best lap{{end}} marker</label>
      </div>
    </div>
    <div class="pilot-chart-scroll">{{template "pilot-chart" .P.Chart}}</div>
    <div class="pilot-chart-note">
      {{if eq .P.Series "laps"}}Every racing lap of the meeting in running order, holeshots left out.
      {{else if eq .P.Series "hs"}}Every holeshot of the meeting in running order — one bar per heat you started.
      {{else}}Every lap of the meeting in running order; the darker bars are holeshots.{{end}}
      Click a bar to open that heat.
    </div>
  </div>

  <div class="pilot-tab-panel" data-panel="laps">
    {{range .P.Groups}}
    <div class="pilot-lap-group" data-key="{{.RaceID}}">
      <div class="pilot-lap-group-head"><a href="/dronedash/races#race-{{.RaceID}}">{{.RaceLabel}}</a>
        {{if .TypeLabel}}<span class="sub">{{.TypeLabel}}</span>{{end}}
        {{if .Position}}<span class="sub">finished {{ord .Position}}</span>{{end}}</div>
      <div class="pilot-card-laps">
        {{range .Laps}}<div class="lap-badge{{if .Highlight}} lap-{{.Highlight}}{{if or (eq .Highlight "race-best") (eq .Highlight "race-pb")}} pattern-hatched{{end}}{{end}}">
          <div class="lap-badge-label">{{.Label}}</div>
          <div class="lap-badge-value">{{f3 .Seconds}}</div>
        </div>{{end}}
        {{if gt .Total 0.0}}<div class="lap-badge total-badge">
          <div class="lap-badge-label">Total</div>
          <div class="lap-badge-value">{{f3 .Total}}</div>
        </div>{{end}}
      </div>
    </div>
    {{else}}<p class="pilot-empty">No laps recorded yet.</p>{{end}}
  </div>

  <div class="pilot-tab-panel" data-panel="upcoming">
    {{if .P.Upcoming}}
    <div class="pilot-upcoming-grid">
      {{range .P.Upcoming}}
      <div class="pilot-upcoming-card" data-key="{{.RaceID}}">
        <h3>{{.Label}}</h3>
        <p class="sub">{{len .Pilots}} pilot{{if ne (len .Pilots) 1}}s{{end}}</p>
        <div class="pilot-upcoming-competitors">
          {{range .Pilots}}<span class="pilot-upcoming-competitor">{{if .ChannelColor}}<span class="dot" style="background:{{.ChannelColor}}"></span>{{end}}<a href="/dronedash/pilot?name={{.Name}}">{{.Name}}</a></span>{{end}}
        </div>
      </div>
      {{end}}
    </div>
    {{else}}<p class="pilot-empty">No upcoming races scheduled for this pilot.</p>{{end}}
  </div>
</div>
{{- end -}}
{{end}}

{{/* One overview stat card. A stat that resolves to a heat becomes a link into
     the race history at that heat; aggregates (Total Laps) render plain.
     . = {Label, S: pilotStat, Value: formatted, Sub: subtitle}. */}}
{{define "pstat"}}
{{- $rid := .S.RaceID -}}
{{if $rid}}<a class="pilot-stat-card" href="/dronedash/races#race-{{$rid}}" title="Open this heat in the race history">{{else}}<div class="pilot-stat-card">{{end}}
  <div class="pilot-stat-label">{{.Label}}</div>
  <div class="pilot-stat-value">{{.Value}}</div>
  <div class="pilot-stat-subtitle">{{.Sub}}{{if $rid}} <span class="pilot-stat-go" aria-hidden="true">↗</span>{{end}}</div>
{{if $rid}}</a>{{else}}</div>{{end}}
{{end}}

{{/* The lap chart, drawn server-side. No script and nothing to download: the
     venue is offline, and the cloud page's ECharts CDN is exactly what isn't
     there. Bars are keyed so the soft-refresh patches them in place. */}}
{{define "pilot-chart"}}
{{- $c := . -}}
{{- if .Empty -}}
<p class="pilot-empty">No laps recorded yet.</p>
{{- else -}}
<svg width="{{$c.Width}}" height="{{$c.Height}}" viewBox="0 0 {{$c.Width}} {{$c.Height}}" role="img" aria-label="Lap times, in running order">
  <g class="pc-grid" data-key="grid">{{range $c.Ticks}}<line x1="52" y1="{{.Y}}" x2="{{$c.Width}}" y2="{{.Y}}"/>{{end}}</g>
  <g class="pc-axis" data-key="axis">{{range $c.Ticks}}<text x="46" y="{{.Y}}" text-anchor="end" dominant-baseline="middle">{{.Label}}</text>{{end}}</g>
  <g class="pc-bars" data-key="bars">{{range $c.Bars}}<a data-key="{{.Key}}" class="pc-bar" href="{{.Href}}"><title>{{.Title}}</title><rect x="{{.X}}" y="{{.Y}}" width="{{.W}}" height="{{.H}}" rx="2" fill="{{.Fill}}"/><text class="pc-xlabel" x="{{.LabelX}}" y="{{.LabelY}}" text-anchor="end" transform="rotate(-45 {{.LabelX}} {{.LabelY}})">{{.Label}}</text></a>{{end}}</g>
  <g class="pc-heats" data-key="heats">{{range $c.Groups}}{{if .HasSep}}<line class="pc-sep" x1="{{.SepX}}" y1="14" x2="{{.SepX}}" y2="{{$c.GroupY}}"/>{{end}}<text class="pc-heat" x="{{.MidX}}" y="{{$c.GroupY}}" text-anchor="middle">{{.Label}}</text>{{end}}</g>
  {{if $c.HasBest}}<g class="pc-bestline" data-key="best"><line class="pc-best" x1="52" y1="{{$c.BestY}}" x2="{{$c.Width}}" y2="{{$c.BestY}}"/><rect class="pc-bestbg" x="{{$c.BestBoxX}}" y="{{$c.BestBoxY}}" width="{{$c.BestBoxW}}" height="14" rx="3"/><text class="pc-bestlabel" x="{{$c.BestX}}" y="{{$c.BestY}}" dy="-5">{{$c.BestLabel}}</text></g>{{end}}
</svg>
{{- end -}}
{{end}}

{{define "overlay-waiting"}}<!doctype html><html><head><meta charset="utf-8"><title>Overlay</title>{{template "css" .}}</head>
<body class="overlay"><div id="app">{{template "overlay-waiting-body" .}}</div>{{template "js" (dictRefresh 2000 .)}}</body></html>{{end}}
{{define "overlay-waiting-body"}}{{end}}

{{define "overlay"}}<!doctype html><html><head><meta charset="utf-8"><title>Overlay</title>{{template "css" .}}</head>
<body class="overlay"><div id="app">{{template "overlay-body" .}}</div>{{template "js" (dictRefresh 800 .)}}</body></html>{{end}}

{{define "overlay-body"}}
  {{with .M.Current}}{{$race := .}}
  {{/* Every element is keyed: the soft refresh pairs children by data-key,
       and unkeyed siblings get paired by position, which mixes pilots up. */}}
  <div class="obs">
    <div class="obs-head" data-key="head">
      <img data-key="logo" src="/assets/icon-256.webp" alt="">
      <span data-key="race" class="obs-race">{{.Label}}</span>
      {{if eq .State "running"}}<span data-key="live" class="obs-live">LIVE</span><span data-key="clock" class="obs-clock" data-clock-start="{{.StartMs}}">0.0</span>{{end}}
    </div>
    <div class="obs-rows" data-key="rows">{{range .Pilots}}<div class="obs-row" data-key="{{.Name}}">
      <span data-key="pos" class="pos{{if le .Position 3}} p{{.Position}}{{end}}">{{.Position}}</span>
      <i data-key="chan" style="background:{{.ChannelColor}}"></i>
      <b data-key="name">{{.Name}}</b>
      <span data-key="laps" class="obs-laps">{{if $race.IsTT}}{{f2 .BestConsec}}{{else}}L{{len .DispLaps}}{{end}}</span>
      <span data-key="total" class="obs-total">{{f3 .Total}}</span>
    </div>{{end}}</div>
  </div>
  {{end}}
{{end}}
`

// defaultBrand is the neutral branding the local pages start with.
func defaultBrand() brand {
	return brand{Version: version}
}
