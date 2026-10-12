package main

// The pilot page for the offline board — the local twin of the cloud
// dashboard's /dronedash/pilot/{id} (DashboardPilot + dashboard-pilot.html).
//
// Two things make it a rewrite rather than a port.
//
// It is keyed by NAME, not id. The venue snapshot carries FPVTrackside's own
// pilot records and the local board has always folded them by name (the
// leaderboard, the channel chip, ddapi's /pilot.json all do), so there is no id
// to route on. Links carry ?name= and resolve case-insensitively.
//
// And the chart is server-rendered SVG. The cloud page pulls ECharts off a CDN,
// which is exactly what an offline venue does not have — this whole server
// exists to work with the internet unplugged. The chart is therefore drawn in
// the template from geometry computed here: no script, no download, nothing to
// fail at the field, and it costs a Raspberry Pi display nothing to redraw.
//
// The numbers are NOT recomputed for this page. Every stat card reads the same
// pilotScores pass the leaderboard is built from (see buildMeeting), so the
// pilot page and the board cannot disagree about a pilot's best lap — the
// failure mode that matters here, because a race director reads one and quotes
// the other.

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
)

// pilotPage serves /dronedash/pilot?name=<callsign>.
//
// An unknown name renders the page's own "pilot not found" state rather than a
// 404: these links are shared around the pits ("look at my laps"), and a pilot
// whose callsign FPVTrackside spells differently should land somewhere that
// explains itself and offers the way back.
func (s *liveServer) servePilot(w http.ResponseWriter, r *http.Request) {
	mv, _, ok := s.meeting()
	if !ok {
		if r.URL.Query().Get("fragment") == "1" {
			s.fragment(w, r, "waiting", nil)
			return
		}
		s.render(w, r, "waiting", nil, nil)
		return
	}
	pg, found := buildPilotPage(mv, r.URL.Query().Get("name"))
	extra := map[string]any{}
	if found {
		// The series lives in the URL rather than in a CSS-only toggle like the
		// other chart controls, because the chart is re-laid-out for it (see
		// applySeries) — and the soft refresh re-fetches location.href, so the
		// chosen series survives every poll for free.
		pg.applySeries(r.URL.Query().Get("series"))
		extra["P"] = pg
	}
	if r.URL.Query().Get("fragment") == "1" {
		s.fragmentExtra(w, r, "pilot", mv, extra)
		return
	}
	s.render(w, r, "pilot", mv, extra)
}

// ── Meeting-wide scoring (shared with the leaderboard) ──────────────────────

// pilotStat is one meeting-wide number plus the heat that set it, so the card
// can say where it came from and link there. Mirrors what the cloud's
// findStatSubtitle digs back out of the database after the fact.
type pilotStat struct {
	Value     float64
	Detail    string // what the number is: "Lap 4", "Laps 2–4", "3 laps"
	RaceID    string
	RaceLabel string
}

// Subtitle is the stat card's second line, in the cloud's "<context> · <heat>"
// shape. "—" when the stat was never set.
func (p pilotStat) Subtitle() string {
	switch {
	case p.Value <= 0:
		return "—"
	case p.Detail != "" && p.RaceLabel != "":
		return p.Detail + " · " + p.RaceLabel
	case p.RaceLabel != "":
		return p.RaceLabel
	default:
		return p.Detail
	}
}

// pilotScore is a pilot's whole meeting, folded.
type pilotScore struct {
	TotalLaps   int
	Holeshot    pilotStat
	BestLap     pilotStat
	BestConsec  pilotStat
	FastestRace pilotStat
}

// scoringHeats returns the heats that feed the meeting-wide aggregates.
//
// Practice heats are shown everywhere but must not score — a race director who
// opens the day with a practice round shouldn't find its laps in the standings.
// Skipped only when there is real racing to compare against: an all-practice
// meeting IS the meeting, and blanking its board would be worse. Mirrors the
// cloud's practiceRaceIDs, which returns empty for exactly the same reason.
func scoringHeats(races []vmRace) []vmRace {
	anyCompetitive := false
	for _, vr := range races {
		if !vr.Practice {
			anyCompetitive = true
			break
		}
	}
	if !anyCompetitive {
		return races
	}
	out := make([]vmRace, 0, len(races))
	for _, vr := range races {
		if !vr.Practice {
			out = append(out, vr)
		}
	}
	return out
}

// pilotScores folds every scoring heat into one row per pilot, recording which
// heat set each number.
//
// This is THE definition of a pilot's meeting-wide stats on the local board:
// buildMeeting's leaderboard reads it and so does the pilot page, so the two
// cannot drift. The rules are pinned to the cloud's precomputeEventStats and
// are load-bearing — see TestBoardMatchesCloudInputs for why each one is what
// it is:
//
//   - laps count every RACING lap. The holeshot is a gate crossing, not a lap
//     flown, and counting it inflated every pilot's total by one per heat —
//     reported from the field, and the card does say "completed laps";
//   - best lap is over p.Laps (every racing lap, uncapped) — NOT p.BestLap,
//     which races cap at target_laps, and which is also the board's sort
//     tiebreak, so a capped value would reorder the board against the cloud;
//   - best-N-consecutive uses the uniform event window, recomputed here — NOT
//     p.BestConsec, which is the per-heat window the race cards use;
//   - fastest race is the race DISTANCE, over heats the pilot actually
//     completed — see completedRaceTotal.
func pilotScores(races []vmRace, window int) map[string]*pilotScore {
	out := map[string]*pilotScore{}
	for _, vr := range scoringHeats(races) {
		for _, p := range vr.Pilots {
			if p.Name == "" || p.Name == "—" {
				continue
			}
			s := out[p.Name]
			if s == nil {
				s = &pilotScore{}
				out[p.Name] = s
			}
			s.TotalLaps += len(p.Laps)
			if p.Holeshot > 0 && (s.Holeshot.Value == 0 || p.Holeshot < s.Holeshot.Value) {
				s.Holeshot = pilotStat{Value: p.Holeshot, RaceID: vr.ID, RaceLabel: vr.Label}
			}
			for i, secs := range p.Laps {
				if secs > 0 && (s.BestLap.Value == 0 || secs < s.BestLap.Value) {
					s.BestLap = pilotStat{Value: secs, Detail: fmt.Sprintf("Lap %d", i+1),
						RaceID: vr.ID, RaceLabel: vr.Label}
				}
			}
			if sum, at := bestConsecutiveRun(p.Laps, window); sum > 0 &&
				(s.BestConsec.Value == 0 || sum < s.BestConsec.Value) {
				s.BestConsec = pilotStat{Value: sum, Detail: consecDetail(at, window),
					RaceID: vr.ID, RaceLabel: vr.Label}
			}
			if total, n, ok := completedRaceTotal(vr, p); ok &&
				(s.FastestRace.Value == 0 || total < s.FastestRace.Value) {
				s.FastestRace = pilotStat{Value: total,
					Detail: fmt.Sprintf("%d lap%s", n, plural(n)),
					RaceID: vr.ID, RaceLabel: vr.Label}
			}
		}
	}
	return out
}

// completedRaceTotal returns a pilot's time for the RACE DISTANCE in one heat —
// holeshot plus the first target_laps racing laps — the lap count it covers, and
// whether it counts at all.
//
// Two rules, both from the field. A heat the pilot never finished is not a
// "fastest total race": a run cut short after one lap posted a total nothing
// else could beat, which is how the card ended up advertising a DNF as a
// personal best. And the total is capped at the race distance, so heats are
// compared over the SAME distance — without that, a time trial where pilots fly
// as many laps as they like makes the number a lap count in disguise, and the
// shortest run wins again.
//
// A heat with no target laps configured can't be judged either way, so it keeps
// the old behaviour: every recorded lap, no completion test.
func completedRaceTotal(vr vmRace, p vmPilot) (total float64, laps int, ok bool) {
	if p.DNF {
		return 0, 0, false
	}
	counted := p.Laps
	if vr.TargetLaps > 0 {
		if len(counted) < vr.TargetLaps {
			return 0, 0, false // never went the distance
		}
		counted = counted[:vr.TargetLaps]
	}
	if len(counted) == 0 {
		return 0, 0, false
	}
	if p.Holeshot > 0 {
		total = p.Holeshot
	}
	for _, secs := range counted {
		total += secs
	}
	return total, len(counted), total > 0
}

// consecDetail names the laps a best-consecutive run was flown over — the
// window is 1-based to match the lap numbering everywhere else on the page.
func consecDetail(at, window int) string {
	if window <= 1 {
		return fmt.Sprintf("Lap %d", at+1)
	}
	return fmt.Sprintf("Laps %d–%d", at+1, at+window)
}

// ── The page ────────────────────────────────────────────────────────────────

// pilotLap is one lap on the chart and in the lap table. Number 0 is the
// holeshot, matching the cloud's lap_number convention.
type pilotLap struct {
	RaceID    string
	RaceLabel string // "Round 2 — Race 1"
	RaceCode  string // "R2-1", for the chart's heat band
	Number    int
	Seconds   float64
	Highlight string // "overall-best" | "overall-pb" | "race-best" | "race-pb" | ""
}

// Label is the lap's short badge text.
func (l pilotLap) Label() string {
	if l.Number == 0 {
		return "HS"
	}
	return fmt.Sprintf("L%d", l.Number)
}

// pilotRaceGroup is one heat the pilot flew, for the lap table.
type pilotRaceGroup struct {
	RaceID    string
	RaceLabel string
	TypeLabel string
	Position  int
	Total     float64
	Laps      []pilotLap
}

// pilotUpcomingRace is a heat the pilot is drawn in that hasn't started.
type pilotUpcomingRace struct {
	RaceID string
	Label  string
	Pilots []vmPilot
}

// pilotPage is everything the pilot template renders.
type pilotPage struct {
	Name         string
	ChannelLabel string
	ChannelColor string
	Pos          int // leaderboard position (0 = not on the board)
	Window       int // the meeting's best-N-consecutive window
	TotalLaps    int
	BestLap      pilotStat
	BestConsec   pilotStat
	FastestRace  pilotStat
	Holeshot     pilotStat
	Groups       []pilotRaceGroup
	Upcoming     []pilotUpcomingRace
	Series       string // which chart series is drawn: seriesAll | seriesLaps | seriesHS
	Chart        *pilotChart

	allLaps []pilotLap // every plotted point, before the series filter
}

// The analytics chart's series filter.
const (
	seriesAll  = "all"  // holeshots and racing laps together
	seriesLaps = "laps" // racing laps only
	seriesHS   = "hs"   // holeshots only
)

// normaliseSeries folds an unknown ?series= back to the full chart, so a stale
// or hand-typed link renders rather than blanking the panel.
func normaliseSeries(mode string) string {
	switch mode {
	case seriesLaps, seriesHS:
		return mode
	}
	return seriesAll
}

// applySeries redraws the chart for one series.
//
// The filtering is done HERE and not by hiding bars in CSS, which is how the
// other chart toggles work. The two series are an order of magnitude apart — a
// ~3s holeshot against ~20s laps — so hiding one client-side leaves the other
// drawn against an axis scaled for times it no longer contains: holeshots alone
// come out as a row of bars flat on the floor, which answers nothing. Laying
// them out again rescales the axis, re-packs the bars and rebuilds the heat
// band, which is the whole point of asking for the series on its own.
func (p *pilotPage) applySeries(mode string) {
	p.Series = normaliseSeries(mode)
	laps := make([]pilotLap, 0, len(p.allLaps))
	for _, l := range p.allLaps {
		switch p.Series {
		case seriesLaps:
			if l.Number == 0 {
				continue
			}
		case seriesHS:
			if l.Number != 0 {
				continue
			}
		}
		laps = append(laps, l)
	}
	marker, label := p.BestLap.Value, "PB"
	if p.Series == seriesHS {
		marker, label = p.Holeshot.Value, "Best HS"
	}
	p.Chart = buildPilotChart(laps, marker, label)
}

// maxPilotUpcoming caps the Upcoming Races list, matching the cloud's LIMIT 6.
const maxPilotUpcoming = 6

// buildPilotPage assembles one pilot's page from the meeting view. ok=false
// when no pilot of that name flew (or is drawn in) this meeting.
func buildPilotPage(mv *meetingView, name string) (*pilotPage, bool) {
	target := strings.ToLower(strings.TrimSpace(name))
	if mv == nil || target == "" {
		return nil, false
	}

	pg := &pilotPage{Window: mv.Window}

	// Identity, rank and the channel chip all come from the board row, so the
	// chip here is resolved by the same running/next/last-heat priority the
	// leaderboard uses rather than a second guess at it.
	for _, st := range mv.Board {
		if strings.ToLower(st.Name) == target {
			pg.Name, pg.Pos = st.Name, st.Pos
			pg.ChannelLabel, pg.ChannelColor = st.ChannelLabel, st.ChannelColor
			break
		}
	}

	// Running order, so the chart reads left-to-right as the day was flown.
	// mv.Races is in snapshot order; beforeRace is the meeting's real running
	// order (see its comment — round number alone is not one).
	order := make([]vmRace, len(mv.Races))
	copy(order, mv.Races)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		return beforeRace(a.Order, a.Round, a.Number, b.Order, b.Round, b.Number)
	})

	// Fall back to the heat lineups for a pilot with no board row (a roster-only
	// pilot is on the board, so this is mostly belt and braces).
	if pg.Name == "" {
		for _, vr := range order {
			for _, p := range vr.Pilots {
				if strings.ToLower(p.Name) == target {
					pg.Name = p.Name
					break
				}
			}
			if pg.Name != "" {
				break
			}
		}
	}
	if pg.Name == "" {
		return nil, false
	}

	if s := pilotScores(mv.Races, mv.Window)[pg.Name]; s != nil {
		pg.TotalLaps = s.TotalLaps
		pg.BestLap, pg.BestConsec = s.BestLap, s.BestConsec
		pg.FastestRace, pg.Holeshot = s.FastestRace, s.Holeshot
	}

	// Highlight baselines, and the practice split matters here.
	//
	// The MEETING-wide records (overall-best, overall-pb) come from the scoring
	// heats only — the same gate the stat cards are under, so a warm-up lap
	// can't take the meeting's-fastest colour off the lap that earned it. The
	// PER-HEAT ones (race-best, race-pb) fill for every heat including practice,
	// so a practice heat still highlights its own quickest laps. That's the
	// cloud's split too: precomputeEventStats excludes practice from the
	// event-wide aggregates while its per-race maps stay populated.
	overallBest, raceBest := 0.0, map[string]float64{}
	scoring := map[string]bool{}
	for _, vr := range scoringHeats(mv.Races) {
		scoring[vr.ID] = true
	}
	for _, vr := range mv.Races {
		for _, p := range vr.Pilots {
			for _, secs := range p.Laps {
				if secs <= 0 {
					continue
				}
				if b := raceBest[vr.ID]; b == 0 || secs < b {
					raceBest[vr.ID] = secs
				}
				if scoring[vr.ID] && (overallBest == 0 || secs < overallBest) {
					overallBest = secs
				}
			}
		}
	}

	for _, vr := range order {
		var mine *vmPilot
		for i := range vr.Pilots {
			if strings.ToLower(vr.Pilots[i].Name) == target {
				mine = &vr.Pilots[i]
				break
			}
		}
		if mine == nil {
			continue
		}
		if vr.State == "pending" {
			if len(pg.Upcoming) < maxPilotUpcoming {
				pg.Upcoming = append(pg.Upcoming, pilotUpcomingRace{
					RaceID: vr.ID, Label: vr.Label, Pilots: vr.Pilots})
			}
			continue
		}
		if mine.Holeshot <= 0 && len(mine.Laps) == 0 {
			continue // started or finished, but nothing recorded for them
		}

		// This pilot's best lap in THIS heat, for the race-pb colour.
		myRaceBest := 0.0
		for _, secs := range mine.Laps {
			if secs > 0 && (myRaceBest == 0 || secs < myRaceBest) {
				myRaceBest = secs
			}
		}
		g := pilotRaceGroup{RaceID: vr.ID, RaceLabel: vr.Label,
			TypeLabel: vr.TypeLabel, Position: mine.Position, Total: mine.Total}
		code := fmt.Sprintf("R%d-%d", vr.Round, vr.Number)
		if mine.Holeshot > 0 {
			g.Laps = append(g.Laps, pilotLap{RaceID: vr.ID, RaceLabel: vr.Label, RaceCode: code,
				Number: 0, Seconds: mine.Holeshot})
		}
		for i, secs := range mine.Laps {
			g.Laps = append(g.Laps, pilotLap{RaceID: vr.ID, RaceLabel: vr.Label, RaceCode: code,
				Number: i + 1, Seconds: secs,
				Highlight: lapHighlight(secs, overallBest, pg.BestLap.Value,
					raceBest[vr.ID], myRaceBest)})
		}
		pg.Groups = append(pg.Groups, g)
	}

	for _, g := range pg.Groups {
		pg.allLaps = append(pg.allLaps, g.Laps...)
	}
	pg.applySeries(seriesAll)
	return pg, true
}

// lapHighlight classifies a lap against the four baselines, most significant
// first — mirrors the cloud's highlightFor so the same lap gets the same colour
// on both boards.
func lapHighlight(secs, overallBest, pilotBest, raceBest, pilotRaceBest float64) string {
	switch {
	case secs <= 0:
		return ""
	case lapEq(secs, overallBest):
		return "overall-best"
	case lapEq(secs, pilotBest):
		return "overall-pb"
	case lapEq(secs, raceBest):
		return "race-best"
	case lapEq(secs, pilotRaceBest):
		return "race-pb"
	}
	return ""
}

// lapEq is the cloud's floatEq: lap times are stored to the millisecond, so
// compare with a tolerance rather than by identity. Zero never matches.
func lapEq(a, b float64) bool {
	if a == 0 || b == 0 {
		return false
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.0005
}

// ── The chart ───────────────────────────────────────────────────────────────
//
// Geometry only — the template does the drawing. Kept apart so the numbers are
// testable without parsing SVG out of a rendered page.

// Chart layout, in SVG user units (the viewBox scales it to the container).
const (
	pcAxisW   = 52  // left gutter for the y-axis labels
	pcPadR    = 14  // right gutter, so the last bar isn't flush to the edge
	pcPadTop  = 14  // headroom above the tallest bar
	pcPadBot  = 64  // room for the rotated lap labels + the heat band under them
	pcPlotH   = 210 // the plot area itself
	pcBarGap  = 6
	pcPitch   = 20  // bar + gap at full meeting length
	pcMaxPtch = 60  // ...widened for a short meeting, so it isn't a sliver
	pcAimPlot = 620 // plot width a short meeting spreads itself to
	pcTicks   = 5   // horizontal gridlines, inclusive of both ends
	// Breathing room above and below the data, as a fraction of its spread.
	// The floor gets much more than the ceiling on purpose: the fastest time in
	// the series would otherwise sit ON the axis with no bar left to draw, and
	// that bar is the one you most want to see — and to click, since every bar
	// links to its heat and the quickest lap is the heat you go looking for.
	// The axis labels carry the real scale, so the padding costs nothing but a
	// little height.
	pcHeadPad = 0.06
	pcFootPad = 0.18
)

type pilotChartBar struct {
	Key            string // data-key, so the soft-refresh patches bars in place
	X, Y           float64
	W, H           float64
	Fill           string
	Title          string // native SVG tooltip
	Label          string // x-axis tick text
	LabelX, LabelY float64
	Href           string
}

type pilotChartTick struct {
	Y     float64
	Label string
}

// pilotChartGroup is one heat's run of bars — the band under the axis that says
// where each heat starts. Without it a long meeting is sixty bars labelled
// HS, L1, L2, L3, L4 over and over, and you cannot see the heat boundaries at
// all. (The cloud chart puts the heat in every bar's label; at this density
// those collide, and ECharts silently drops most of them.)
type pilotChartGroup struct {
	Label  string
	MidX   float64
	SepX   float64
	HasSep bool // false for the first group — nothing to separate it from
}

type pilotChart struct {
	Width, Height int
	PlotBottom    float64
	GroupY        float64
	Bars          []pilotChartBar
	Groups        []pilotChartGroup
	Ticks         []pilotChartTick
	BestY         float64
	BestX         float64
	BestBoxX      float64 // backing chip, so the label stays legible over a bar
	BestBoxY      float64
	BestBoxW      float64
	HasBest       bool
	BestLabel     string
	Empty         bool
}

// buildPilotChart lays out one bar per recorded lap, in the order flown.
//
// The y-axis is scaled to the DATA, not to zero: lap times cluster in a narrow
// band (a meeting of 20-25s laps) and a zero-based axis renders every bar the
// same height, which is a chart that carries no information. On the full chart
// the holeshot is plotted alongside the racing laps and compresses that band —
// deliberately, so this reads as the same chart as the cloud's, which does the
// same thing and colours the holeshot bar differently for it. The series filter
// (see applySeries) is the way out of that compression when you want it.
//
// marker draws a dashed reference line at one time, named by markerLabel ("PB",
// "Best HS"); it is skipped when it falls outside the plotted range.
func buildPilotChart(laps []pilotLap, marker float64, markerLabel string) *pilotChart {
	c := &pilotChart{Height: pcPadTop + pcPlotH + pcPadBot, PlotBottom: pcPadTop + pcPlotH}
	if len(laps) == 0 {
		c.Empty, c.Width = true, pcAxisW+pcAimPlot+pcPadR
		return c
	}

	lo, hi := laps[0].Seconds, laps[0].Seconds
	for _, l := range laps {
		if l.Seconds < lo {
			lo = l.Seconds
		}
		if l.Seconds > hi {
			hi = l.Seconds
		}
	}
	// Breathing room, and a floor for the degenerate single-value case (every
	// bar identical) where a zero range would divide by zero.
	if span := hi - lo; span > 0 {
		lo, hi = lo-span*pcFootPad, hi+span*pcHeadPad
	} else {
		lo, hi = lo*0.85, hi*1.05
	}
	if hi <= lo {
		hi = lo + 1
	}
	// Rounded to 2dp: these go straight into SVG attributes, and Go's default
	// float formatting would otherwise write 17 significant figures per
	// coordinate into a page that re-renders every few seconds.
	y := func(v float64) float64 {
		return math.Round((pcPadTop+pcPlotH*(hi-v)/(hi-lo))*100) / 100
	}

	// Bars are a fixed pitch once there are enough of them to fill the panel,
	// and scroll past it after that. Below that they widen to spread across it:
	// a five-lap meeting drawn at racing pitch is a cluster of thin bars pinned
	// to the left of an empty panel, which reads as a broken chart rather than
	// a short one.
	pitch := pcPitch
	if n := len(laps); n*pcPitch < pcAimPlot {
		if pitch = pcAimPlot / n; pitch > pcMaxPtch {
			pitch = pcMaxPtch
		}
	}
	barW := float64(pitch - pcBarGap)
	c.Width = pcAxisW + len(laps)*pitch + pcPadR
	for i := 0; i < pcTicks; i++ {
		v := hi - (hi-lo)*float64(i)/float64(pcTicks-1)
		c.Ticks = append(c.Ticks, pilotChartTick{Y: y(v), Label: fmt.Sprintf("%.1f", v)})
	}
	if marker > 0 && marker >= lo && marker <= hi {
		c.HasBest, c.BestY = true, y(marker)
		// Anchored at the LEFT, beside the axis: a long meeting scrolls, and a
		// label parked at the far end is off-screen exactly when the line it
		// names is the thing you're looking at.
		c.BestX = pcAxisW + 10
		c.BestLabel = fmt.Sprintf("%s %.3f", markerLabel, marker)
		// Sat directly on the bars, the label is unreadable. ~6.2px per glyph
		// at the 10px label size, which is close enough for a backing chip.
		c.BestBoxW = math.Round(float64(len(c.BestLabel))*6.2) + 10
		c.BestBoxX, c.BestBoxY = c.BestX-5, c.BestY-15
	}

	for i, l := range laps {
		x := float64(pcAxisW + i*pitch)
		top := y(l.Seconds)
		c.Bars = append(c.Bars, pilotChartBar{
			Key: fmt.Sprintf("b%d", i), X: x, Y: top, W: barW,
			H: math.Round(((pcPadTop+pcPlotH)-top)*100) / 100,
			// Racing lap colours mirror the cloud chart's; the holeshot is the
			// muted line colour there too, because it is not a lap time and
			// shouldn't read as a fast one.
			Fill:   lapBarFill(l),
			Title:  lapChartTitle(l),
			Label:  l.Label(),
			LabelX: x + barW/2,
			LabelY: pcPadTop + pcPlotH + 14,
			// Every bar is a link into the heat it was flown in — the same jump
			// the stat cards offer, and the reason the pilot page is useful at
			// the field: from "my fastest lap" to that heat's video link.
			Href: "/dronedash/races#race-" + l.RaceID,
		})

		// Heat band: open a group on the first bar and whenever the heat changes.
		if n := len(c.Groups); n == 0 || c.Groups[n-1].Label != l.RaceCode {
			c.Groups = append(c.Groups, pilotChartGroup{
				Label: l.RaceCode, SepX: math.Round(x - float64(pcBarGap)/2), HasSep: n > 0})
		}
	}
	// Centre each band's label under the bars it spans.
	start := pcAxisW
	for gi := range c.Groups {
		end := c.Width - pcPadR
		if gi+1 < len(c.Groups) {
			end = int(c.Groups[gi+1].SepX)
		}
		c.Groups[gi].MidX = math.Round(float64(start+end) / 2)
		start = end
	}
	c.GroupY = pcPadTop + pcPlotH + 44
	return c
}

func lapBarFill(l pilotLap) string {
	if l.Number == 0 {
		return "#3a4049"
	}
	switch l.Highlight {
	case "overall-best":
		return "#d63333"
	case "overall-pb":
		return "#3361d6"
	case "race-best":
		return "#33d65a"
	case "race-pb":
		return "var(--accent)"
	}
	return "#9aa3af"
}

func lapChartTitle(l pilotLap) string {
	what := fmt.Sprintf("Lap %d", l.Number)
	if l.Number == 0 {
		what = "Holeshot"
	}
	return fmt.Sprintf("%s\n%s: %.3fs", l.RaceLabel, what, l.Seconds)
}
