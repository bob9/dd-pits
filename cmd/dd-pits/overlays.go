package main

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/bob9/dd-pits/internal/dronedash"
)

// OBS overlays.
//
// The same set the AUFPV platform offers for live timing — corner, top bar,
// scorebug, pilot cam tag, strip, race grid, lineup, Up Now and banner,
// leaderboard, laps rail, spotlight, standings, podium and results — plus a few
// of dd-pits' own. Routes and query parameters match the platform's, so a
// streamer's OBS scene collection works against either.
//
// Each overlay is a transparent page sized for a 1920×1080 Browser Source. One
// rem is 1/120th of the page width, so a 1280×720 source shows the same layout
// smaller rather than a different one. They follow the meeting through the
// dashboard's soft refresh, so every repeated element carries a data-key: the
// refresh pairs children by key, and unkeyed siblings get paired by position,
// which shows one pilot's name beside another's times. Anything that ticks on
// its own (the wall clock) lives outside #app so the refresh never resets it.

// ovParam is one setting an overlay accepts. Path params become the URL's
// path suffix (/overlay/strip/next); the rest are query parameters.
type ovParam struct {
	Key     string // query key, or "_ref" for the path suffix
	Label   string
	Kind    string // select | text | number | color | check | pilots | rows
	Options []string
	Default string
	Hint    string
}

// overlayKind is one overlay as the gallery lists it.
type overlayKind struct {
	Slug   string // /overlay/<slug>
	Ref    string // the example's path suffix, e.g. "/next" or "/2"
	Query  string // the example's query string
	Title  string
	What   string
	Region string
	Group  string
	Params []ovParam
}

// Settings most overlays share.
var (
	pAccent  = ovParam{Key: "accent", Label: "Accent colour", Kind: "color", Hint: "Leave empty for the dd-pits lime"}
	pStill   = ovParam{Key: "still", Label: "No animation", Kind: "check", Hint: "For a slow streaming PC, or a static frame"}
	pBg      = ovParam{Key: "bg", Label: "Background", Kind: "select", Options: []string{"", "transparent", "scrim", "scene"}, Hint: "Empty = transparent over your scene"}
	pRefCur  = ovParam{Key: "_ref", Label: "Which heat", Kind: "select", Options: []string{"", "next", "last"}, Hint: "Empty = the current heat"}
	pRefNext = ovParam{Key: "_ref", Label: "Which heat", Kind: "select", Options: []string{"current", "next", "last"}, Default: "next"}
	pTitle   = ovParam{Key: "title", Label: "Title", Kind: "text", Hint: "Empty = the meeting's name"}
)

var overlayKinds = []overlayKind{
	{"scorebug", "", "", "Scorebug", "A TV-style badge with your title and the time of day. A heat strip slides in when the next heat is cued, stays up while it runs, and shows the result after the flag.", "Top left", "Live timing", []ovParam{
		pTitle, {Key: "clock", Label: "Day and time", Kind: "select", Options: []string{"", "0"}, Hint: "0 hides the clock bar"},
		{Key: "strip", Label: "Heat strip", Kind: "select", Options: []string{"", "0"}, Hint: "0 shows the badge only"},
		{Key: "live", Label: "Strip while racing", Kind: "select", Options: []string{"", "0"}, Hint: "0 keeps the strip down mid-heat, for a commentator cam"},
		{Key: "upnext", Label: "Show the next heat when cued", Kind: "select", Options: []string{"", "0"}},
		{Key: "next", Label: "Seconds to show a cued heat", Kind: "number", Default: "6"},
		{Key: "endwait", Label: "Seconds after the flag before the result", Kind: "number", Default: "5"},
		{Key: "hold", Label: "Seconds to hold the result", Kind: "number", Default: "10"},
		pAccent, pStill}},
	{"topbar", "", "", "Top bar", "A full-width bar: racing now / on deck / just raced with the race timer, the meeting and heat, and your club.", "Top", "Live timing", []ovParam{pRefCur, pAccent}},
	{"corner", "", "", "Corner", "The smallest: your logo, the time of day and the heat, glowing red while it races.", "Top left", "Live timing", []ovParam{pRefCur, pAccent}},
	{"race", "", "", "Race grid", "One tile per pilot for a multi-camera scene. The tiles are transparent so your FPV feeds show through; each has the pilot, position and last four laps.", "Full screen", "Live timing", []ovParam{
		{Key: "layout", Label: "Tiles (columns x rows)", Kind: "select", Options: []string{"", "2x2", "3x2", "4x1", "1x4", "3x3"}, Hint: "Empty = fitted to the heat"}, pAccent, pStill}},
	{"node", "/1", "", "Pilot cam tag", "A lower third for one pilot's FPV feed, by seat: position, channel, callsign, lap, last, best and total. Add one per feed.", "Bottom left", "Live timing", []ovParam{
		{Key: "_ref", Label: "Seat (channel order)", Kind: "select", Options: []string{"1", "2", "3", "4", "5", "6", "7", "8"}, Default: "1"}, pAccent, pStill}},
	{"strip", "", "", "Racing now strip", "The pilots in the air, ringed in their channel colours, pulsing while the heat runs.", "Bottom right", "Live timing", []ovParam{
		{Key: "_ref", Label: "Which heat", Kind: "select", Options: []string{"", "next"}, Hint: "next = the up-next strip, bottom left"}, pAccent, pStill}},
	{"lapchart", "", "", "Lap chart", "Every lap of every pilot in the heat, the fastest highlighted.", "Right", "Live timing", []ovParam{pRefCur, pAccent}},
	{"fastest", "", "", "Fastest lap", "Calls out the fastest lap of the heat so far, and who set it.", "Bottom right", "Live timing", []ovParam{pRefCur, pAccent}},
	{"panel", "", "", "Race panel", "A compact panel: positions, laps, totals and the race clock.", "Top left", "Live timing", []ovParam{pAccent}},
	{"lowerthird", "", "", "Lower third", "The lineup across the bottom, F1 style, with each pilot's last lap and total.", "Bottom", "Live timing", []ovParam{pRefCur, pAccent}},
	{"strip", "/next", "", "Up next strip", "The next heat's pilots and channels, bottom left, while the current one runs.", "Bottom left", "Between heats", []ovParam{
		{Key: "_ref", Label: "Which heat", Kind: "select", Options: []string{"next", ""}, Default: "next"}, pAccent, pStill}},
	{"lineup", "/next", "", "Lineup", "A full-screen lineup that walks each pilot on: meeting rank and best times before the heat, positions and laps once it runs, the result after.", "Full screen", "Between heats", []ovParam{pRefNext, pBg, pAccent, pStill}},
	{"upnow", "/next", "", "Up Now", "Pilot cards walk on one by one with rank, best lap and best consecutive, hold, then step off. Loops.", "Centre", "Between heats", []ovParam{pRefNext, pBg, pAccent, pStill}},
	{"banner", "/next", "", "Up Now banner", "The Up Now cards as a lower-third banner.", "Bottom", "Between heats", []ovParam{pRefNext, pAccent, pStill}},
	{"spotlight", "", "", "Spotlight", "One pilot's profile, or two head to head. Pick pilots here, or press Push to stream and the overlay follows.", "Bottom centre", "Standings", []ovParam{
		{Key: "p", Label: "Pilots (one or two)", Kind: "pilots", Hint: "None = follow what is pushed to stream"}, pBg, pAccent}},
	{"leaderboard", "", "", "Leaderboard", "The meeting standings, with ▲▼ movement since the last heat and what each pilot gained. Long fields scroll.", "Full screen", "Standings", []ovParam{
		{Key: "top", Label: "Show the top", Kind: "number", Hint: "Empty = everyone"}, pBg, pAccent}},
	{"laps", "", "", "Laps rail", "A narrow rail of the meeting: one time per pilot, 🔥 for whoever just improved. Scrolls on a loop.", "Left", "Standings", []ovParam{
		{Key: "mode", Label: "Time shown", Kind: "select", Options: []string{"", "lap", "consec"}, Hint: "Empty = best consecutive at a time trial, best lap otherwise"},
		{Key: "top", Label: "Show the top", Kind: "number", Hint: "Empty = everyone"}, pAccent}},
	{"standings", "", "", "Standings", "The leader as the hero, second and third beside them, the rest of the field below. Live during the meeting, final at the end.", "Full screen", "Standings", []ovParam{
		pTitle, {Key: "top", Label: "Show the top", Kind: "number", Hint: "Empty = everyone"},
		{Key: "cols", Label: "Columns for the rest", Kind: "select", Options: []string{"3", "2", "1"}, Default: "3"},
		{Key: "detail", Label: "Times", Kind: "select", Options: []string{"", "0"}, Hint: "0 hides each pilot's time"},
		{Key: "head", Label: "Header", Kind: "select", Options: []string{"", "0"}, Hint: "0 hides the title row"},
		pBg, pAccent}},
	{"podium", "", "", "Podium", "The top four on podium steps, the winner crowned.", "Full screen", "Standings", []ovParam{pTitle, pBg, pAccent, pStill}},
	{"results", "", "?title=Club+Champs&sub=Round+4&r=Kestrel|37.206&r=Hornet|37.908&r=Blitz|39.086", "Results card", "A results scene from the URL alone: a title and the finishing order you type. Nothing to set up.", "Full screen", "Standings", []ovParam{
		{Key: "title", Label: "Title", Kind: "text", Default: "Club Champs"}, {Key: "sub", Label: "Subtitle", Kind: "text", Default: "Round 4"},
		{Key: "r", Label: "Finishing order", Kind: "rows", Default: "Kestrel|37.206\nHornet|37.908\nBlitz|39.086", Hint: "One per line: Name|detail"}, pBg, pAccent}},
	{"notice", "", "?text=Weather+hold+—+back+in+10+minutes", "Notice", "A banner with your own message: a delay, a sponsor, the next round.", "Bottom", "dd-pits extras", []ovParam{
		{Key: "text", Label: "Message", Kind: "text", Default: "Weather hold — back in 10 minutes"}, pAccent}},
}

// overlayTemplateFor maps a slug to its template.
func overlayTemplateFor(slug string) (string, bool) {
	for _, k := range overlayKinds {
		if k.Slug == slug {
			return "ov-" + slug, true
		}
	}
	return "", false
}

// ── view models ────────────────────────────────────────────────────────────

type ovLap struct {
	Key   string
	T     float64
	Delta float64 // against the pilot's best in this heat
	Best  bool
}

type ovPilot struct {
	Name, Color, Channel, Initial string
	Pos                           int
	Laps, Target                  int
	Last, Best, Total, Holeshot   float64
	Consec                        float64
	DNF                           bool
	Chips                         []ovLap // every lap, best marked
	Recent                        []ovLap // the last four
	// Meeting-wide, from the leaderboard.
	Rank                     int
	MBest, MConsec, MFastest float64
	MLaps                    int
}

type ovRace struct {
	ID, Label, RoundName, Status, State string
	Number, Round, Target, Window       int
	StartMs                             int64
	IsTT                                bool
	Pilots                              []ovPilot // position order
	Seats                               []ovPilot // channel order — stays put as positions change
}

type ovRow struct {
	vmStanding
	Initial  string
	Move     string // "▲2", "▼1", "—", "NEW"
	MoveDir  string // up | down | same | new
	Gain     float64
	Improved bool
}

func initial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "?"
}

// refRace resolves current | next | last | a race id.
func refRace(m *meetingView, ref string) (*vmRace, string) {
	if m == nil {
		return nil, ""
	}
	switch ref {
	case "", "current":
		if m.Current != nil {
			switch m.Current.State {
			case "running":
				return m.Current, "Racing Now"
			case "ended":
				return m.Current, "Just Raced"
			}
			return m.Current, "On Deck"
		}
		if len(m.Upcoming) > 0 {
			return &m.Upcoming[0], "Up Next"
		}
		if len(m.Recent) > 0 {
			return &m.Recent[0], "Just Raced"
		}
	case "next":
		for i := range m.Upcoming {
			if m.Current == nil || m.Upcoming[i].ID != m.Current.ID {
				return &m.Upcoming[i], "Up Next"
			}
		}
	case "last":
		if len(m.Recent) > 0 {
			return &m.Recent[0], "Just Raced"
		}
	default:
		for i := range m.Races {
			if m.Races[i].ID == ref {
				st := map[string]string{"running": "Racing Now", "ended": "Just Raced"}[m.Races[i].State]
				if st == "" {
					st = "Up Next"
				}
				return &m.Races[i], st
			}
		}
	}
	return nil, ""
}

func buildOvRace(m *meetingView, r *vmRace, status string) *ovRace {
	if r == nil {
		return nil
	}
	board := map[string]vmStanding{}
	for _, b := range m.Board {
		board[strings.ToUpper(b.Name)] = b
	}
	out := &ovRace{ID: r.ID, Label: r.Label, RoundName: r.RoundName, Status: status, State: r.State,
		Number: r.Number, Round: r.Round, Target: r.TargetLaps, Window: r.ConsecWindow,
		StartMs: r.StartMs, IsTT: r.IsTT}
	for _, p := range r.Pilots {
		op := ovPilot{Name: p.Name, Color: p.ChannelColor, Channel: p.ChannelLabel, Initial: initial(p.Name),
			Pos: p.Position, Laps: len(p.DispLaps), Target: r.TargetLaps, Best: p.BestLap, Total: p.Total,
			Holeshot: p.Holeshot, Consec: p.BestConsec, DNF: p.DNF}
		if n := len(p.DispLaps); n > 0 {
			op.Last = p.DispLaps[n-1]
		}
		for i, l := range p.DispLaps {
			lap := ovLap{Key: "l" + strconv.Itoa(i), T: l, Best: p.BestLap > 0 && l <= p.BestLap}
			if p.BestLap > 0 {
				lap.Delta = l - p.BestLap
			}
			op.Chips = append(op.Chips, lap)
		}
		if n := len(op.Chips); n > 4 {
			op.Recent = op.Chips[n-4:]
		} else {
			op.Recent = op.Chips
		}
		if b, ok := board[strings.ToUpper(p.Name)]; ok {
			op.Rank, op.MBest, op.MConsec, op.MFastest, op.MLaps = b.Pos, b.BestLap, b.BestConsec, b.FastestRace, b.Laps
		}
		out.Pilots = append(out.Pilots, op)
	}
	// Seats follow the meeting's channel order, so a camera feed and its tile
	// stay together however the positions shuffle.
	order := map[string]int{}
	for i, c := range m.Channels {
		order[c.Label] = i
	}
	out.Seats = append([]ovPilot(nil), out.Pilots...)
	sort.SliceStable(out.Seats, func(i, j int) bool {
		oi, iok := order[out.Seats[i].Channel]
		oj, jok := order[out.Seats[j].Channel]
		if iok && jok {
			return oi < oj
		}
		return out.Seats[i].Channel < out.Seats[j].Channel
	})
	return out
}

// prevBoardCache holds the standings as they were before the most recent heat,
// keyed by that heat, so movement badges cost one extra build per heat rather
// than one per refresh.
var prevBoardCache struct {
	sync.Mutex
	key  string
	rows map[string]vmStanding
}

func previousBoard(snap dronedash.Snapshot, last string) map[string]vmStanding {
	prevBoardCache.Lock()
	defer prevBoardCache.Unlock()
	if prevBoardCache.key == last && prevBoardCache.rows != nil {
		return prevBoardCache.rows
	}
	cut := snap
	cut.Races = nil
	for _, r := range snap.Races {
		if r.ID != last {
			cut.Races = append(cut.Races, r)
		}
	}
	cut.Results = nil
	for _, r := range snap.Results {
		if r.Race != last {
			cut.Results = append(cut.Results, r)
		}
	}
	rows := map[string]vmStanding{}
	for _, b := range buildMeeting(cut).Board {
		rows[strings.ToUpper(b.Name)] = b
	}
	prevBoardCache.key, prevBoardCache.rows = last, rows
	return rows
}

func buildRows(m *meetingView, prev map[string]vmStanding) []ovRow {
	var out []ovRow
	for _, b := range m.Board {
		row := ovRow{vmStanding: b, Initial: initial(b.Name), Move: "—", MoveDir: "same"}
		if prev != nil {
			if p, ok := prev[strings.ToUpper(b.Name)]; !ok {
				row.Move, row.MoveDir = "NEW", "new"
			} else {
				switch d := p.Pos - b.Pos; {
				case d > 0:
					row.Move, row.MoveDir = fmt.Sprintf("▲%d", d), "up"
				case d < 0:
					row.Move, row.MoveDir = fmt.Sprintf("▼%d", -d), "down"
				}
				metric, was := b.BestConsec, p.BestConsec
				if !m.IsTT || metric == 0 {
					metric, was = b.BestLap, p.BestLap
				}
				if was > 0 && metric > 0 && metric < was {
					row.Gain, row.Improved = was-metric, true
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// spotPilot is one spotlit pilot with what the card shows.
type spotPilot struct {
	ovRow
	Of int
}

// ── the handler ────────────────────────────────────────────────────────────

// overlaySpotlight is the selection pushed from the gallery, which a spotlight
// overlay with no ?p= follows.
var overlaySpotlight struct {
	sync.Mutex
	names []string
}

func (s *liveServer) overlayPage(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/overlay/"), "/"), "/")
	slug, ref := parts[0], ""
	if len(parts) > 1 {
		ref = parts[1]
	}
	view, ok := overlayTemplateFor(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	s.mu.RLock()
	snap, have := s.snap, s.have
	s.mu.RUnlock()
	var mv *meetingView
	if have {
		mv = buildMeeting(snap)
	}

	extra := map[string]any{"Q": q, "Slug": slug, "Ref": ref, "Text": q.Get("text"),
		"Title": q.Get("title"), "Sub": q.Get("sub"), "Bg": q.Get("bg"), "OvAccent": safeColor(q.Get("accent"))}
	if q.Get("demo") == "1" || q.Get("config") == "1" {
		extra["Demo"] = true
	}

	needsMeeting := slug != "notice" && slug != "results"
	if needsMeeting && mv == nil {
		view = "overlay-waiting"
	}
	if mv != nil {
		extra["Event"] = mv.Name
		switch slug {
		case "node":
			seat := atoiDefault(ref, atoiDefault(q.Get("seat"), 1))
			race, st := refRace(mv, "current")
			if or := buildOvRace(mv, race, st); or != nil && seat <= len(or.Seats) {
				p := or.Seats[seat-1]
				extra["P"], extra["R"], extra["Ord"] = p, or, ordinal(p.Pos)
			}
		case "leaderboard", "laps", "standings", "podium", "spotlight":
			var prev map[string]vmStanding
			if len(mv.Recent) > 0 {
				prev = previousBoard(snap, mv.Recent[0].ID)
			}
			rows := buildRows(mv, prev)
			extra["Rows"] = rows
			extra["Final"] = mv.Current == nil && len(mv.Upcoming) == 0
			extra["Mode"] = q.Get("mode")
			if extra["Mode"] == "" {
				extra["Mode"] = map[bool]string{true: "consec", false: "lap"}[mv.IsTT]
			}
			if top := atoiDefault(q.Get("top"), 0); top > 0 && top < len(rows) && slug != "spotlight" {
				rows = rows[:top]
				extra["Rows"] = rows
			}
			extra["Cols"] = atoiDefault(q.Get("cols"), 3)
			if slug == "spotlight" {
				names := q["p"]
				if len(names) == 0 {
					overlaySpotlight.Lock()
					names = append([]string(nil), overlaySpotlight.names...)
					overlaySpotlight.Unlock()
				}
				var spot []spotPilot
				for _, n := range names {
					for _, row := range rows {
						if strings.EqualFold(row.Name, strings.TrimSpace(n)) && len(spot) < 2 {
							spot = append(spot, spotPilot{ovRow: row, Of: len(rows)})
						}
					}
				}
				extra["Spot"] = spot
				if len(spot) == 2 {
					a, b := spot[0], spot[1]
					extra["GapLap"] = abs(a.BestLap - b.BestLap)
					extra["GapConsec"] = abs(a.BestConsec - b.BestConsec)
				}
			}
		default:
			if ref == "" && (slug == "lineup" || slug == "upnow" || slug == "banner") {
				ref = "current"
			}
			race, st := refRace(mv, ref)
			extra["R"] = buildOvRace(mv, race, st)
		}
	}
	if slug == "results" {
		var rows []map[string]any
		for i, rv := range q["r"] {
			name, detail, _ := strings.Cut(rv, "|")
			rows = append(rows, map[string]any{"Pos": i + 1, "Name": name, "Detail": detail, "Initial": initial(name)})
		}
		extra["Results"] = rows
	}
	if q.Get("fragment") == "1" {
		s.fragmentExtra(w, r, view, mv, extra)
		return
	}
	s.render(w, r, view, mv, extra)
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// safeColor accepts a hex colour from the URL and nothing else, so ?accent=
// cannot inject CSS.
func safeColor(s string) template.CSS {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 3 && len(s) != 6 {
		return ""
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return ""
		}
	}
	return template.CSS("#" + s)
}

func atoiDefault(s string, d int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return d
	}
	return n
}

// overlayBase is the address to give OBS: this laptop's network address when
// the page was opened as localhost, so the URL also works on a streaming PC.
func overlayBase(r *http.Request) string {
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	h, port, _ := strings.Cut(host, ":")
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" {
		if ip := lanIP(); ip != "" {
			host = ip
			if port != "" {
				host += ":" + port
			}
		}
	}
	return scheme + "://" + host + forwardedPrefix(r)
}

func (s *liveServer) overlaySpotlightPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	_ = r.ParseForm()
	names := r.Form["p"]
	if r.FormValue("clear") == "1" {
		names = nil
	}
	if len(names) > 2 {
		names = names[:2]
	}
	overlaySpotlight.Lock()
	overlaySpotlight.names = names
	overlaySpotlight.Unlock()
	msg := "Spotlight taken off air."
	if len(names) > 0 {
		msg = "On air: " + strings.Join(names, " vs ")
	}
	http.Redirect(w, r, "/overlays?msg="+url.QueryEscape(msg)+"#spotlight", http.StatusSeeOther)
}

// overlayFuncs are the template helpers the overlays need on top of the
// dashboard's own.
var overlayFuncs = template.FuncMap{
	// timer names the timing system dd-pits reads: "FPVTrackside" or "RotorHazard".
	"timer": func() string { return timingSystem },
	"add":   func(a, b int) int { return a + b },
	"signed": func(v float64) string {
		if v == 0 {
			return ""
		}
		return fmt.Sprintf("%+.2f", v)
	},
	"seq": func(n int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = i
		}
		return s
	},
	"delayS": func(i int, step float64) string { return fmt.Sprintf("%.2fs", float64(i)*step) },
	"podiumOrder": func(rows []ovRow) []ovRow {
		var out []ovRow
		for _, i := range []int{1, 0, 2, 3} {
			if i < len(rows) {
				out = append(out, rows[i])
			}
		}
		return out
	},
	"afterN": func(n int, rows []ovRow) []ovRow {
		if n < len(rows) {
			return rows[n:]
		}
		return nil
	},
	"firstN": func(n int, rows []ovRow) []ovRow {
		if n < len(rows) {
			return rows[:n]
		}
		return rows
	},
	"fastestLap": func(r *ovRace) *ovPilot {
		if r == nil {
			return nil
		}
		var best *ovPilot
		for i := range r.Pilots {
			p := &r.Pilots[i]
			if p.Best > 0 && (best == nil || p.Best < best.Best) {
				best = p
			}
		}
		return best
	},
	"gridCols": func(q url.Values, n int) int {
		if l := q.Get("layout"); l != "" {
			if c, _, ok := strings.Cut(l, "x"); ok {
				return atoiDefault(c, 2)
			}
		}
		if n <= 1 {
			return 1
		}
		if n <= 4 {
			return 2
		}
		return 3
	},
}
