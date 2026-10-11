package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/dronedash"
)

// fillDispLaps mirrors what buildMeeting does for real snapshots: a hand-authored
// view-model fixture only sets .Laps, but the templates render .DispLaps, so copy
// Laps→DispLaps wherever a fixture left it unset. (Fixtures are all time trials /
// uncapped, so DispLaps == Laps; buildMeeting itself applies the race cap.)
func fillDispLaps(mv *meetingView) {
	races := append([]*vmRace{mv.Current}, nil)
	for i := range mv.Recent {
		races = append(races, &mv.Recent[i])
	}
	for i := range mv.Upcoming {
		races = append(races, &mv.Upcoming[i])
	}
	for _, r := range races {
		if r == nil {
			continue
		}
		for i := range r.Pilots {
			if r.Pilots[i].DispLaps == nil {
				r.Pilots[i].DispLaps = r.Pilots[i].Laps
			}
		}
	}
}

// renderPage executes one page template the way the server does.
func renderPage(t *testing.T, s *liveServer, name string, mv *meetingView) string {
	t.Helper()
	var buf bytes.Buffer
	data := map[string]any{"M": mv, "Brand": s.brand, "Accent": s.brand.Accent}
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return buf.String()
}

func TestPagesRender(t *testing.T) {
	s := newLiveServer(brand{Name: "Test Club", Accent: "#00aaff"})
	mv := &meetingView{
		Name: "June TT", IsTT: true, Window: 3,
		Current: &vmRace{
			Label: "Round 1 — Race 1", IsTT: true, ConsecWindow: 3, State: "running", StartMs: 1700000000000,
			Pilots: []vmPilot{
				{Name: "Alice", ChannelLabel: "R1", ChannelColor: "#f00", Holeshot: 2.0,
					Laps: []float64{5.1, 4.9, 5.0}, Total: 17.0, CountedLaps: 4, Position: 1,
					BestLap: 4.9, BestConsec: 15.0, PBConsec: 14.5},
				{Name: "Bob", ChannelLabel: "R2", Position: 2, Laps: []float64{6.0}, CountedLaps: 1},
			},
		},
	}
	mv.Recent = []vmRace{*mv.Current}
	fillDispLaps(mv)

	for _, pg := range []string{"home", "live", "races", "overlay", "waiting"} {
		out := renderPage(t, s, pg, mv)
		if strings.TrimSpace(out) == "" {
			t.Fatalf("%s rendered empty", pg)
		}
	}
	// Spot-check the data actually made it into the live + home pages.
	home := renderPage(t, s, "home", mv)
	if !strings.Contains(home, "Alice") || !strings.Contains(home, "Test Club") {
		t.Fatalf("home missing pilot/brand: %s", home[:min(len(home), 400)])
	}
	live := renderPage(t, s, "live", mv)
	if !strings.Contains(live, "15.00") { // best-consec chip (f2)
		t.Fatalf("live missing best-consec value")
	}
	if !strings.Contains(live, `data-clock-start="1700000000000"`) {
		t.Fatalf("live missing running clock anchor")
	}
}

// TestNextUpFiltersPlaceholders verifies the "Next Up" filter: never-run heats
// FPVTrackside leaves at the default Round 1 / Race 1 (duplicate round+race
// numbers) and empty (0-pilot) heats are dropped, so a finished/template-heavy
// meeting collapses to no upcoming races instead of "123 upcoming" nonsense.
func TestNextUpFiltersPlaceholders(t *testing.T) {
	// Helper builds a pending (no start/end) race with N pilots, given round/race.
	pc := func(ids ...string) string {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = `{"ID":"pc` + id + `","Pilot":"` + id + `","Channel":"c1"}`
		}
		return strings.Join(parts, ",")
	}

	// Three pending heats ALL at Round 1 / Race 1 (FPVTrackside placeholders) with
	// real pilots, plus one empty heat → all should be filtered out.
	dupRaw := `{
	  "event":{"ID":"e1","Name":"Done Night","EventType":"Race",
	           "Channels":["c1"],"ChannelColors":["#f00"],"Rounds":["rd1"],
	           "Races":["a","b","c","empty"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"},{"ID":"p3","Name":"Cy"},
	            {"ID":"p4","Name":"Di"},{"ID":"p5","Name":"Ed"},{"ID":"p6","Name":"Fi"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"a","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[` + pc("p1", "p2") + `]},
	    {"ID":"b","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[` + pc("p3", "p4") + `]},
	    {"ID":"c","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[` + pc("p5", "p6") + `]},
	    {"ID":"empty","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[]}
	  ]}`
	var dup dronedash.Snapshot
	if err := json.Unmarshal([]byte(dupRaw), &dup); err != nil {
		t.Fatalf("unmarshal dup: %v", err)
	}
	mv := buildMeeting(dup)
	if len(mv.Upcoming) != 0 || mv.UpcomingTotal != 0 {
		t.Fatalf("expected no upcoming (all placeholders), got %d shown / %d total", len(mv.Upcoming), mv.UpcomingTotal)
	}

	// Properly-sequenced heats (distinct race numbers) ARE kept.
	seqRaw := `{
	  "event":{"ID":"e1","Name":"Live Night","EventType":"Race",
	           "Channels":["c1"],"ChannelColors":["#f00"],"Rounds":["rd1"],
	           "Races":["a","b","c","d"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"},{"ID":"p3","Name":"Cy"},{"ID":"p4","Name":"Di"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"a","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[` + pc("p1") + `]},
	    {"ID":"b","Round":"rd1","RaceNumber":2,"Valid":true,"PilotChannels":[` + pc("p2") + `]},
	    {"ID":"c","Round":"rd1","RaceNumber":3,"Valid":true,"PilotChannels":[` + pc("p3") + `]},
	    {"ID":"d","Round":"rd1","RaceNumber":4,"Valid":true,"PilotChannels":[` + pc("p4") + `]}
	  ]}`
	var seq dronedash.Snapshot
	if err := json.Unmarshal([]byte(seqRaw), &seq); err != nil {
		t.Fatalf("unmarshal seq: %v", err)
	}
	mv = buildMeeting(seq)
	// Nothing has started, so the first heat is promoted to the on-deck Current and
	// the remaining three are the upcoming queue (mirrors the cloud loadDashHome —
	// stopping/finishing a heat advances Current to the next heat).
	if mv.Current == nil || mv.Current.Number != 1 {
		t.Fatalf("expected heat 1 promoted to on-deck Current, got %+v", mv.Current)
	}
	if mv.UpcomingTotal != 3 {
		t.Fatalf("expected 3 queued upcoming after on-deck promotion, got total %d", mv.UpcomingTotal)
	}
	if len(mv.Upcoming) != 3 {
		t.Fatalf("expected 3 upcoming cards, got %d", len(mv.Upcoming))
	}
}

// TestNextUpExcludesLeftovers verifies a "next up" heat must run AFTER the
// meeting's current position: a never-run heat from an earlier round (distinct
// race number, so not caught by the duplicate filter) must not reappear as
// upcoming while a later round is live.
func TestNextUpExcludesLeftovers(t *testing.T) {
	// Live in Round 17 / Race 3. Round 17 / Race 4 is the genuine next; the two
	// Round 1 heats are never-run leftovers from the first round → excluded.
	raw := `{
	  "event":{"ID":"e1","Name":"Elims","EventType":"Race",
	           "Channels":["c1","c2"],"ChannelColors":["#f00","#00f"],
	           "Rounds":["rd1","rd17"],
	           "Races":["cur","next","lo1","lo2"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"},{"ID":"p3","Name":"Cy"},{"ID":"p4","Name":"Di"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true},
	            {"ID":"rd17","RoundNumber":17,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"cur","Round":"rd17","RaceNumber":3,"Valid":true,"Start":"2026-06-05T09:05:00Z","PilotChannels":[{"ID":"x1","Pilot":"p1","Channel":"c1"},{"ID":"x2","Pilot":"p2","Channel":"c2"}]},
	    {"ID":"next","Round":"rd17","RaceNumber":4,"Valid":true,"PilotChannels":[{"ID":"x3","Pilot":"p3","Channel":"c1"},{"ID":"x4","Pilot":"p4","Channel":"c2"}]},
	    {"ID":"lo1","Round":"rd1","RaceNumber":3,"Valid":true,"PilotChannels":[{"ID":"x5","Pilot":"p1","Channel":"c1"},{"ID":"x6","Pilot":"p2","Channel":"c2"}]},
	    {"ID":"lo2","Round":"rd1","RaceNumber":5,"Valid":true,"PilotChannels":[{"ID":"x7","Pilot":"p3","Channel":"c1"},{"ID":"x8","Pilot":"p4","Channel":"c2"}]}
	  ]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	if mv.UpcomingTotal != 1 || len(mv.Upcoming) != 1 {
		t.Fatalf("expected exactly 1 upcoming (Round 17 Race 4), got %d shown / %d total", len(mv.Upcoming), mv.UpcomingTotal)
	}
	if mv.Upcoming[0].Round != 17 || mv.Upcoming[0].Number != 4 {
		t.Fatalf("expected upcoming = Round 17 Race 4, got Round %d Race %d", mv.Upcoming[0].Round, mv.Upcoming[0].Number)
	}
}

// TestStaleResultsHeatNotCurrent reproduces the "stuck Current Race" bug: a heat
// FPVTrackside left with a Start but no End, yet which already carries finishing
// results, must be treated as ended (not running) and must not hijack the Current
// card from the heat actually being raced.
func TestStaleResultsHeatNotCurrent(t *testing.T) {
	// "stale" = Round 1 Race 5: started long ago, no End, but has results.
	// "live"  = Round 3 Race 2: started later, no End, no results — the real one.
	// Snapshot order deliberately puts the stale heat LAST so the old
	// last-in-loop election would have wrongly picked it as Current.
	raw := `{
	  "event":{"ID":"e1","Name":"Whoop Champs","EventType":"Race",
	           "Channels":["c1","c2"],"ChannelColors":["#f00","#00f"],
	           "Rounds":["rd1","rd3"],
	           "Races":["live","stale"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true},
	            {"ID":"rd3","RoundNumber":3,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"live","Round":"rd3","RaceNumber":2,"Valid":true,"Start":"2026-06-27T04:13:00Z","PilotChannels":[{"ID":"x1","Pilot":"p1","Channel":"c1"},{"ID":"x2","Pilot":"p2","Channel":"c2"}]},
	    {"ID":"stale","Round":"rd1","RaceNumber":5,"Valid":true,"Start":"2026-06-27T02:52:00Z","PilotChannels":[{"ID":"x3","Pilot":"p1","Channel":"c1"},{"ID":"x4","Pilot":"p2","Channel":"c2"}]}
	  ],
	  "results":[
	    {"ID":"r1","Race":"stale","Pilot":"p1","Position":1,"Valid":true},
	    {"ID":"r2","Race":"stale","Pilot":"p2","Position":2,"Valid":true}
	  ]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	if mv.Current == nil {
		t.Fatal("expected a Current race, got nil")
	}
	if mv.Current.ID != "live" {
		t.Fatalf("Current should be the live heat (Round 3 Race 2), got %q (Round %d Race %d)",
			mv.Current.ID, mv.Current.Round, mv.Current.Number)
	}
	if mv.Current.State != "running" {
		t.Fatalf("live heat should be running, got %q", mv.Current.State)
	}
	// The stale heat must read as ended (it has results), not running.
	for i := range mv.Races {
		if mv.Races[i].ID == "stale" && mv.Races[i].State != "ended" {
			t.Fatalf("stale results-bearing heat should be ended, got %q", mv.Races[i].State)
		}
	}
}

// TestStaleAbandonedHeatNotCurrent covers the no-results case: a started, unended
// heat with no lap for longer than staleHeatGrace (an abandoned / false-started
// heat the operator left selected) is retired to "ended" so it can't keep holding
// the Current card — while a genuinely live heat (recent lap) stays running.
func TestStaleAbandonedHeatNotCurrent(t *testing.T) {
	// CapturedAt 06:00. "stale" last lap 05:40 (20 min gap → abandoned). "live"
	// last lap 05:59:50 (10s gap → racing). Neither has an End or results.
	raw := `{
	  "captured_at":"2026-06-27T06:00:00Z",
	  "event":{"ID":"e1","Name":"Whoop Champs","EventType":"Race",
	           "Channels":["c1"],"ChannelColors":["#f00"],
	           "Rounds":["rd2","rd5"],"Races":["live","stale"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"}],
	  "rounds":[{"ID":"rd2","RoundNumber":2,"EventType":"Race","Valid":true},
	            {"ID":"rd5","RoundNumber":5,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"live","Round":"rd5","RaceNumber":2,"Valid":true,"Start":"2026-06-27T05:58:00Z",
	     "PilotChannels":[{"ID":"x1","Pilot":"p1","Channel":"c1"}],
	     "Laps":[{"ID":"l1","LapNumber":1,"LengthSeconds":30,"StartTime":"2026-06-27T05:59:20Z","EndTime":"2026-06-27T05:59:50Z"}]},
	    {"ID":"stale","Round":"rd2","RaceNumber":4,"Valid":true,"Start":"2026-06-27T05:30:00Z",
	     "PilotChannels":[{"ID":"x2","Pilot":"p2","Channel":"c1"}],
	     "Laps":[{"ID":"l2","LapNumber":1,"LengthSeconds":30,"StartTime":"2026-06-27T05:39:30Z","EndTime":"2026-06-27T05:40:00Z"}]}
	  ]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	for i := range mv.Races {
		r := mv.Races[i]
		if r.ID == "stale" && r.State != "ended" {
			t.Fatalf("stale abandoned heat should be ended, got %q", r.State)
		}
		if r.ID == "live" && r.State != "running" {
			t.Fatalf("live heat should be running, got %q", r.State)
		}
	}
	if mv.Current == nil || mv.Current.ID != "live" {
		t.Fatalf("Current should be the live heat, got %v", mv.Current)
	}
}

// TestLeaderboardChannelPriority verifies the board channel rule: racing now →
// next genuine upcoming heat → last finished heat (and that never-run placeholder
// heats sorting before a pilot's last race don't masquerade as "upcoming").
func TestLeaderboardChannelPriority(t *testing.T) {
	// Al: finished R1 in round 1, currently racing R2 in round 2  → shows R2.
	// Bo: finished R1 in round 1, staged for R8 in round 2 (pending) → shows R8.
	// Cy: finished R7 in round 5, plus a pending Round-1/Race-1 placeholder on R2
	//     (sorts before their last race) → shows R7, not the placeholder R2.
	raw := `{
	  "event":{"ID":"e1","Name":"Night","EventType":"Race",
	           "Channels":["c1","c2","c3","c4"],"ChannelColors":["#e11","#22e","#dd2","#2d2"],
	           "Rounds":["rd1","rd2","rd5"],
	           "Races":["a1","a2","b1","b2","c1r","c2r"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"},
	              {"ID":"c3","ShortBand":"R","Number":7,"DisplayName":"R7"},
	              {"ID":"c4","ShortBand":"R","Number":8,"DisplayName":"R8"}],
	  "pilots":[{"ID":"al","Name":"Al"},{"ID":"bo","Name":"Bo"},{"ID":"cy","Name":"Cy"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true},
	            {"ID":"rd2","RoundNumber":2,"EventType":"Race","Valid":true},
	            {"ID":"rd5","RoundNumber":5,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"a1","Round":"rd1","RaceNumber":1,"Valid":true,"Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z","PilotChannels":[{"ID":"x1","Pilot":"al","Channel":"c1"}]},
	    {"ID":"a2","Round":"rd2","RaceNumber":1,"Valid":true,"Start":"2026-06-05T09:05:00Z","PilotChannels":[{"ID":"x2","Pilot":"al","Channel":"c2"}]},
	    {"ID":"b1","Round":"rd1","RaceNumber":1,"Valid":true,"Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z","PilotChannels":[{"ID":"x3","Pilot":"bo","Channel":"c1"}]},
	    {"ID":"b2","Round":"rd2","RaceNumber":1,"Valid":true,"PilotChannels":[{"ID":"x4","Pilot":"bo","Channel":"c4"}]},
	    {"ID":"c1r","Round":"rd5","RaceNumber":1,"Valid":true,"Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z","PilotChannels":[{"ID":"x5","Pilot":"cy","Channel":"c3"}]},
	    {"ID":"c2r","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[{"ID":"x6","Pilot":"cy","Channel":"c2"}]}
	  ]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	got := map[string]string{}
	for _, b := range mv.Board {
		got[b.Name] = b.ChannelLabel
	}
	for name, want := range map[string]string{"Al": "R2", "Bo": "R8", "Cy": "R7"} {
		if got[name] != want {
			t.Errorf("%s: channel = %q, want %q", name, got[name], want)
		}
	}
}

func TestBuildMeetingFromSnapshot(t *testing.T) {
	// A minimal time-trial snapshot: one 3-lap heat, one pilot, holeshot + 3 laps.
	raw := `{
	  "event":{"ID":"e1","Name":"TT Night","EventType":"TimeTrial","Laps":3,"PBLaps":3,
	           "Channels":["c1"],"ChannelColors":["#ff0000"],"Rounds":["r1"],"Races":["race1"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Alice"}],
	  "rounds":[{"ID":"r1","RoundNumber":1,"EventType":"TimeTrial","Valid":true}],
	  "races":[{"ID":"race1","Round":"r1","RaceNumber":1,"TargetLaps":3,"Valid":true,
	    "Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z",
	    "PilotChannels":[{"ID":"pc1","Pilot":"p1","Channel":"c1"}],
	    "Detections":[
	      {"ID":"d0","Pilot":"p1","Valid":true},
	      {"ID":"d1","Pilot":"p1","Valid":true},
	      {"ID":"d2","Pilot":"p1","Valid":true},
	      {"ID":"d3","Pilot":"p1","Valid":true},
	      {"ID":"dx","Pilot":"p1","Valid":false}
	    ],
	    "Laps":[
	      {"Detection":"d0","LapNumber":0,"LengthSeconds":2.0},
	      {"Detection":"d1","LapNumber":1,"LengthSeconds":5.0},
	      {"Detection":"d2","LapNumber":2,"LengthSeconds":4.0},
	      {"Detection":"d3","LapNumber":3,"LengthSeconds":4.5},
	      {"Detection":"dx","LapNumber":4,"LengthSeconds":0.3}
	    ]}]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	if mv.Current == nil {
		t.Fatal("no current race")
	}
	if !mv.IsTT || mv.Current.ConsecWindow != 3 {
		t.Fatalf("expected TT window 3, got tt=%v window=%d", mv.IsTT, mv.Current.ConsecWindow)
	}
	if len(mv.Current.Pilots) != 1 {
		t.Fatalf("expected 1 pilot, got %d", len(mv.Current.Pilots))
	}
	p := mv.Current.Pilots[0]
	// The invalid detection lap (0.3s) must be excluded; 3 racing laps remain.
	if len(p.Laps) != 3 {
		t.Fatalf("expected 3 valid racing laps, got %d (%v)", len(p.Laps), p.Laps)
	}
	if p.Holeshot != 2.0 {
		t.Fatalf("expected holeshot 2.0, got %v", p.Holeshot)
	}
	// best-3-consec over [5,4,4.5] = 13.5
	if p.BestConsec < 13.49 || p.BestConsec > 13.51 {
		t.Fatalf("expected best-3-consec 13.5, got %v", p.BestConsec)
	}
	// total = holeshot 2 + 3 laps (5+4+4.5) = 15.5
	if p.Total < 15.49 || p.Total > 15.51 {
		t.Fatalf("expected total 15.5, got %v", p.Total)
	}
	if p.BestLap != 4.0 {
		t.Fatalf("expected best lap 4.0, got %v", p.BestLap)
	}
	// Event-wide leaderboard row carries the full column set. Laps counts the 3
	// RACING laps — the holeshot is a gate crossing, not a lap flown.
	if len(mv.Board) != 1 {
		t.Fatalf("expected 1 leaderboard row, got %d", len(mv.Board))
	}
	b := mv.Board[0]
	if b.Name != "Alice" || b.Laps != 3 || b.Holeshot != 2.0 || b.BestLap != 4.0 {
		t.Fatalf("leaderboard row wrong: %+v", b)
	}
	if b.BestConsec < 13.49 || b.BestConsec > 13.51 {
		t.Fatalf("expected leaderboard best-consec 13.5, got %v", b.BestConsec)
	}
	if b.FastestRace < 15.49 || b.FastestRace > 15.51 {
		t.Fatalf("expected leaderboard fastest-race 15.5, got %v", b.FastestRace)
	}
}

// TestRaceLapDisplayCap verifies the race-card display cap: a *race* (not a time
// trial) with more recorded laps than target_laps shows only target_laps lap
// columns (DispLaps) — mirroring the cloud's loadDashRaceCardByID — while the
// underlying Laps/Total stay full/capped as before. The sub-second "noise" laps
// FPVTrackside keeps Valid=true (0.151, 0.149) must NOT appear on the card.
func TestRaceLapDisplayCap(t *testing.T) {
	// A 4-lap RACE where the pilot flew 6 laps incl. two sub-second noise laps.
	raw := `{
	  "event":{"ID":"e1","Name":"Race Night","EventType":"Race","Laps":4,
	           "Channels":["c1"],"ChannelColors":["#ff0000"],"Rounds":["r1"],"Races":["race1"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Nacho"}],
	  "rounds":[{"ID":"r1","RoundNumber":1,"EventType":"Race","Valid":true}],
	  "races":[{"ID":"race1","Round":"r1","RaceNumber":1,"TargetLaps":4,"Valid":true,
	    "Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z",
	    "PilotChannels":[{"ID":"pc1","Pilot":"p1","Channel":"c1"}],
	    "Detections":[
	      {"ID":"d0","Pilot":"p1","Valid":true},
	      {"ID":"d1","Pilot":"p1","Valid":true},
	      {"ID":"d2","Pilot":"p1","Valid":true},
	      {"ID":"d3","Pilot":"p1","Valid":true},
	      {"ID":"d4","Pilot":"p1","Valid":true},
	      {"ID":"d5","Pilot":"p1","Valid":true}
	    ],
	    "Laps":[
	      {"Detection":"d0","LapNumber":0,"LengthSeconds":1.024},
	      {"Detection":"d1","LapNumber":1,"LengthSeconds":3.993},
	      {"Detection":"d2","LapNumber":2,"LengthSeconds":5.183},
	      {"Detection":"d3","LapNumber":3,"LengthSeconds":71.884},
	      {"Detection":"d4","LapNumber":4,"LengthSeconds":0.833},
	      {"Detection":"d5","LapNumber":5,"LengthSeconds":0.151}
	    ]}]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	if mv.Current == nil || len(mv.Current.Pilots) != 1 {
		t.Fatal("expected one pilot in current race")
	}
	p := mv.Current.Pilots[0]
	// Display is capped at target_laps=4 — the 0.151 noise lap (lap 5) is hidden.
	if len(p.DispLaps) != 4 {
		t.Fatalf("expected 4 displayed laps (capped at target), got %d (%v)", len(p.DispLaps), p.DispLaps)
	}
	for _, s := range p.DispLaps {
		if s < 0.5 {
			t.Fatalf("sub-second noise lap leaked into the display: %v", p.DispLaps)
		}
	}
	// Underlying Laps stay full (board/consec use them); total = HS + first 4 laps.
	if len(p.Laps) != 5 {
		t.Fatalf("expected 5 full racing laps preserved, got %d", len(p.Laps))
	}
	if p.Total < 82.91 || p.Total > 82.93 { // 1.024 + 3.993 + 5.183 + 71.884 + 0.833
		t.Fatalf("expected total 82.917, got %v", p.Total)
	}
}

// TestBoardMatchesCloudInputs pins the leaderboard aggregation to the cloud's
// precomputeEventStats semantics, so the local and cloud boards sort identically:
//   - Top Lap (the sort tiebreak) is the best over EVERY racing lap, not capped
//     at a race's target_laps;
//   - Valid=false heats are dropped (Ingest deletes them, so the cloud never
//     counts their laps);
//   - roster pilots with no heats still get a (timeless, bottom) row, like the
//     cloud listing every dd_pilots row.
//
// Two columns are DELIBERATELY not the cloud's — both from race-director
// feedback on the numbers themselves, and both flagged for porting up:
//   - Laps counts racing laps only, where the cloud's PilotTotalLaps counts
//     lap 0 (the holeshot) too;
//   - Fastest race is the race distance over completed runs, where the cloud
//     sums every recorded lap of every heat — so a DNF after one lap posts an
//     unbeatable "fastest race".
func TestBoardMatchesCloudInputs(t *testing.T) {
	raw := `{
	  "event":{"ID":"e1","Name":"Race Night","EventType":"Race","Laps":2,
	           "Channels":["c1","c2"],"ChannelColors":["#f00","#00f"],"Rounds":["rd1","rd2"],"Races":["r1","r2"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
	  "pilots":[{"ID":"al","Name":"Al"},{"ID":"bo","Name":"Bo"},{"ID":"zed","Name":"Zed"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true},
	            {"ID":"rd2","RoundNumber":2,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"r1","Round":"rd1","RaceNumber":1,"TargetLaps":2,"Valid":true,
	     "Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:40Z",
	     "PilotChannels":[{"ID":"pc1","Pilot":"al","Channel":"c1"},{"ID":"pc2","Pilot":"bo","Channel":"c2"}],
	     "Detections":[
	       {"ID":"a0","Pilot":"al","Valid":true},{"ID":"a1","Pilot":"al","Valid":true},
	       {"ID":"a2","Pilot":"al","Valid":true},{"ID":"a3","Pilot":"al","Valid":true},
	       {"ID":"a4","Pilot":"al","Valid":true},
	       {"ID":"b0","Pilot":"bo","Valid":true},{"ID":"b1","Pilot":"bo","Valid":true},
	       {"ID":"b2","Pilot":"bo","Valid":true}
	     ],
	     "Laps":[
	       {"Detection":"a0","LapNumber":0,"LengthSeconds":1.0},
	       {"Detection":"a1","LapNumber":1,"LengthSeconds":10.0},
	       {"Detection":"a2","LapNumber":2,"LengthSeconds":10.0},
	       {"Detection":"a3","LapNumber":3,"LengthSeconds":4.0},
	       {"Detection":"a4","LapNumber":4,"LengthSeconds":4.0},
	       {"Detection":"b0","LapNumber":0,"LengthSeconds":1.0},
	       {"Detection":"b1","LapNumber":1,"LengthSeconds":9.0},
	       {"Detection":"b2","LapNumber":2,"LengthSeconds":9.0}
	     ]},
	    {"ID":"r2","Round":"rd2","RaceNumber":1,"TargetLaps":2,"Valid":false,
	     "Start":"2026-06-05T09:10:00Z","End":"2026-06-05T09:11:00Z",
	     "PilotChannels":[{"ID":"pc3","Pilot":"bo","Channel":"c2"}],
	     "Detections":[{"ID":"x1","Pilot":"bo","Valid":true},{"ID":"x2","Pilot":"bo","Valid":true}],
	     "Laps":[
	       {"Detection":"x1","LapNumber":1,"LengthSeconds":1.0},
	       {"Detection":"x2","LapNumber":2,"LengthSeconds":1.0}
	     ]}
	  ]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(snap)
	// The invalid heat is dropped entirely (Ingest parity).
	if len(mv.Races) != 1 {
		t.Fatalf("expected the Valid=false heat dropped, got %d races", len(mv.Races))
	}
	if len(mv.Board) != 3 {
		t.Fatalf("expected 3 leaderboard rows (incl. heatless roster pilot), got %d: %+v", len(mv.Board), mv.Board)
	}
	// Al's best-2-consec is 8.0 (the two 4.0s past the race cap) vs Bo's 18.0 —
	// without the invalid heat, whose 1.0s would put Bo on 2.0 and flip the order.
	if mv.Board[0].Name != "Al" || mv.Board[1].Name != "Bo" || mv.Board[2].Name != "Zed" {
		t.Fatalf("board order wrong: %+v", mv.Board)
	}
	al, bo, zed := mv.Board[0], mv.Board[1], mv.Board[2]
	if al.BestConsec < 7.99 || al.BestConsec > 8.01 {
		t.Fatalf("Al best-2-consec = %v, want 8.0 (uncapped racing laps)", al.BestConsec)
	}
	if bo.BestConsec < 17.99 || bo.BestConsec > 18.01 {
		t.Fatalf("Bo best-2-consec = %v, want 18.0 (invalid heat excluded)", bo.BestConsec)
	}
	// Top Lap is uncapped: Al's fastest lap is the 4.0 flown past target_laps.
	if al.BestLap != 4.0 {
		t.Fatalf("Al top lap = %v, want 4.0 (laps past target count on the board)", al.BestLap)
	}
	// Laps counts RACING laps: the holeshot is a gate crossing, not a lap flown,
	// and counting it inflated every total by one per heat (reported from the
	// field). Al flew 4 racing laps behind his holeshot, Bo 2.
	if al.Laps != 4 || bo.Laps != 2 {
		t.Fatalf("laps = Al %d / Bo %d, want 4 / 2 (the holeshot is not a lap)", al.Laps, bo.Laps)
	}
	// Fastest race is the RACE DISTANCE: holeshot + the first target_laps laps,
	// over heats the pilot actually completed. Al: 1+10+10 = 21 (his two laps
	// past the 2-lap target are not part of the race), Bo: 1+9+9 = 19.
	if al.FastestRace < 20.99 || al.FastestRace > 21.01 {
		t.Fatalf("Al fastest race = %v, want 21.0 (holeshot + the 2-lap distance)", al.FastestRace)
	}
	if bo.FastestRace < 18.99 || bo.FastestRace > 19.01 {
		t.Fatalf("Bo fastest race = %v, want 19.0", bo.FastestRace)
	}
	// The heatless roster pilot carries no times and no channel.
	if zed.Laps != 0 || zed.BestConsec != 0 || zed.ChannelLabel != "" {
		t.Fatalf("Zed should be a timeless roster row: %+v", zed)
	}
}

// TestAccentTooDarkForDashboard locks the contrast guard: saturated brand
// mid-tones used by clubs on the dashboard stay; near-black/navy/empty fall
// back to the lime. Values mirror the production club primaries.
func TestAccentTooDarkForDashboard(t *testing.T) {
	cases := []struct {
		hex  string
		dark bool
	}{
		{"", true},         // unset → fallback
		{"#zzzzzz", true},  // unparseable → fallback
		{"#1a1a1a", true},  // Rotorcross near-black
		{"#1b2a4a", true},  // ADF navy
		{"#c33739", false}, // Rebels red — readable, keep
		{"#00a0e3", false}, // CMRC blue — readable, keep
		{"#e8972c", false}, // Eastside orange — keep
		{"#b6e925", false}, // AUFPV lime — keep
		{"#7b2d8e", false}, // Panorama purple — keep
	}
	for _, c := range cases {
		if got := accentTooDarkForDashboard(c.hex); got != c.dark {
			t.Errorf("accentTooDarkForDashboard(%q) = %v, want %v", c.hex, got, c.dark)
		}
	}
}

// TestNextUpExcludesAlreadyRunBracketHeat guards the fix for already-finished
// bracket heats resurfacing in Next Up on the local board: FPVTrackside keeps the
// old heats around, so a re-paste/leftover copy of a bracket race that already ran
// reads as pending and (via the bracket exemption) bypassed the round suppression.
// The already-run bracket race must be excluded; a genuine upcoming one stays.
func TestNextUpExcludesAlreadyRunBracketHeat(t *testing.T) {
	pcj := func(ids ...string) string {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = `{"ID":"pc` + id + `","Pilot":"` + id + `","Channel":"c1"}`
		}
		return strings.Join(parts, ",")
	}
	raw := `{
	  "event":{"ID":"e1","Name":"Champs","EventType":"Race","Channels":["c1"],"ChannelColors":["#f00"],
	           "Rounds":["rd1","rd2","rd3"],"Races":["live","ran","leftover","next"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"},{"ID":"p3","Name":"Cy"},{"ID":"p4","Name":"Di"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true},
	            {"ID":"rd2","RoundNumber":2,"EventType":"Race","Valid":true},
	            {"ID":"rd3","RoundNumber":3,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"live","Round":"rd3","RaceNumber":1,"Valid":true,"ExternalID":100,"Start":"2026-06-28T02:00:00Z",
	     "PilotChannels":[` + pcj("p1") + `],"Laps":[{"ID":"l1","Detection":"d1","LapNumber":1}],
	     "Detections":[{"ID":"d1","Pilot":"p1","Valid":true}]},
	    {"ID":"ran","Round":"rd1","RaceNumber":1,"Valid":true,"ExternalID":101,
	     "Start":"2026-06-28T01:00:00Z","End":"2026-06-28T01:03:00Z","PilotChannels":[` + pcj("p2") + `]},
	    {"ID":"leftover","Round":"rd1","RaceNumber":1,"Valid":true,"ExternalID":101,"PilotChannels":[` + pcj("p3") + `]},
	    {"ID":"next","Round":"rd2","RaceNumber":1,"Valid":true,"ExternalID":102,"PilotChannels":[` + pcj("p4") + `]}
	  ]}`
	var s dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mv := buildMeeting(s)
	for _, u := range mv.Upcoming {
		if u.ExtID == 101 {
			t.Fatalf("already-run bracket race 101 resurfaced in Next Up: %+v", u)
		}
	}
	if mv.Current != nil && mv.Current.ExtID == 101 {
		t.Fatalf("already-run bracket race 101 became Current")
	}
	// The genuine upcoming bracket heat (102) must still surface (as Next Up or the
	// on-deck Current — the running heat is 100).
	found := mv.Current != nil && mv.Current.ExtID == 102
	for _, u := range mv.Upcoming {
		if u.ExtID == 102 {
			found = true
		}
	}
	if !found {
		t.Fatalf("genuine upcoming bracket race 102 was dropped")
	}
}

// TestJSONAPIEndpoints exercises the dd-pits local JSON API over the real HTTP
// handler: round/race lookups (by id and by round+number), the rounds index,
// pilot detail, leaderboard, filters, and CORS.
func TestJSONAPIEndpoints(t *testing.T) {
	// Two rounds: round 1 has two heats (one ended w/ results, one running),
	// round 2 has one pending heat.
	pc := func(p, ch string) string {
		return `{"ID":"pc` + p + ch + `","Pilot":"` + p + `","Channel":"` + ch + `"}`
	}
	raw := `{
	  "event":{"ID":"e1","Name":"API Night","EventType":"Race",
	           "Channels":["c1","c2"],"ChannelColors":["#f00","#0f0"],"Rounds":["rd1","rd2"],
	           "Races":["a","b","c"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
	  "pilots":[{"ID":"p1","Name":"Alice"},{"ID":"p2","Name":"Bob"},
	            {"ID":"p3","Name":"Cara"},{"ID":"p4","Name":"Dan"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true},
	            {"ID":"rd2","RoundNumber":2,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"a","Round":"rd1","RaceNumber":1,"Valid":true,"Start":"2024-01-01T10:00:00Z","End":"2024-01-01T10:02:00Z",
	     "PilotChannels":[` + pc("p1", "c1") + `,` + pc("p2", "c2") + `],
	     "results":[{"ID":"r1","Pilot":"p1","Position":1},{"ID":"r2","Pilot":"p2","Position":2}]},
	    {"ID":"b","Round":"rd1","RaceNumber":2,"Valid":true,"Start":"2024-01-01T10:05:00Z",
	     "PilotChannels":[` + pc("p3", "c1") + `,` + pc("p4", "c2") + `]},
	    {"ID":"c","Round":"rd2","RaceNumber":1,"Valid":true,
	     "PilotChannels":[` + pc("p1", "c1") + `,` + pc("p3", "c2") + `]}
	  ]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := newLiveServer(brand{Name: "API Club", Accent: "#00aaff"})
	s.setSnapshot(snap)
	h := s.handler()

	get := func(path string) (*httptest.ResponseRecorder, []byte) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, rec.Body.Bytes()
	}

	// /meeting.json — counts.
	rec, body := get("/meeting.json")
	if rec.Code != 200 {
		t.Fatalf("meeting.json status %d", rec.Code)
	}
	var ms struct{ RaceCount, Ended, Running, Pending, PilotCount int }
	if err := json.Unmarshal(body, &ms); err != nil {
		t.Fatalf("meeting.json: %v", err)
	}
	if ms.RaceCount != 3 || ms.Ended != 1 || ms.Running != 1 || ms.Pending != 1 {
		t.Fatalf("meeting counts wrong: %+v", ms)
	}
	if ms.PilotCount != 4 {
		t.Fatalf("expected 4 pilots, got %d", ms.PilotCount)
	}

	// /race.json by round+number — round 1 race 1 (the ended heat).
	rec, body = get("/race.json?round=1&number=1")
	if rec.Code != 200 {
		t.Fatalf("race by coord status %d: %s", rec.Code, body)
	}
	var rc vmRace
	if err := json.Unmarshal(body, &rc); err != nil {
		t.Fatalf("race.json: %v", err)
	}
	if rc.Round != 1 || rc.Number != 1 || rc.State != "ended" {
		t.Fatalf("wrong race by coord: %+v", rc)
	}
	if len(rc.Pilots) != 2 || rc.Pilots[0].Position != 1 {
		t.Fatalf("race pilots/positions wrong: %+v", rc.Pilots)
	}
	// Same race by id should match.
	rec2, body2 := get("/race.json?id=" + rc.ID)
	if rec2.Code != 200 || !bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(body2)) {
		t.Fatalf("race by id != race by coord")
	}
	// Missing selector → 400; bad coord → 404.
	if rec, _ := get("/race.json"); rec.Code != 400 {
		t.Fatalf("race.json no selector: expected 400, got %d", rec.Code)
	}
	if rec, _ := get("/race.json?round=9&number=9"); rec.Code != 404 {
		t.Fatalf("race.json bad coord: expected 404, got %d", rec.Code)
	}

	// /rounds.json — 2 rounds, round 1 has 2 races.
	rec, body = get("/rounds.json")
	var rounds []ddapi.RoundSummary
	if err := json.Unmarshal(body, &rounds); err != nil {
		t.Fatalf("rounds.json: %v", err)
	}
	if len(rounds) != 2 || rounds[0].Round != 1 || rounds[0].RaceCount != 2 {
		t.Fatalf("rounds wrong: %+v", rounds)
	}

	// /round.json?round=2 — one pending race.
	rec, body = get("/round.json?round=2")
	var rd ddapi.RoundDetail
	if err := json.Unmarshal(body, &rd); err != nil {
		t.Fatalf("round.json: %v", err)
	}
	if rd.Round != 2 || len(rd.Races) != 1 || rd.Pending != 1 {
		t.Fatalf("round 2 wrong: %+v", rd)
	}

	// /races.json?state=ended → 1 race.
	rec, body = get("/races.json?state=ended")
	var ended []vmRace
	if err := json.Unmarshal(body, &ended); err != nil {
		t.Fatalf("races.json: %v", err)
	}
	if len(ended) != 1 || ended[0].State != "ended" {
		t.Fatalf("state filter wrong: %+v", ended)
	}
	// ?round=1 → 2 races.
	if _, b := get("/races.json?round=1"); func() bool { var r []vmRace; json.Unmarshal(b, &r); return len(r) != 2 }() {
		t.Fatalf("round filter expected 2 races")
	}

	// /pilot.json?name=Alice (case-insensitive) — in 2 races (r1r1, r2r1).
	rec, body = get("/pilot.json?name=alice")
	if rec.Code != 200 {
		t.Fatalf("pilot.json status %d", rec.Code)
	}
	var pv ddapi.PilotView
	if err := json.Unmarshal(body, &pv); err != nil {
		t.Fatalf("pilot.json: %v", err)
	}
	if pv.Name != "Alice" || len(pv.Races) != 2 {
		t.Fatalf("pilot view wrong: name=%q races=%d", pv.Name, len(pv.Races))
	}
	if rec, _ := get("/pilot.json?name=Nobody"); rec.Code != 404 {
		t.Fatalf("unknown pilot: expected 404, got %d", rec.Code)
	}

	// /board.json?limit=2 — capped.
	if _, b := get("/board.json?limit=2"); func() bool { var r []vmStanding; json.Unmarshal(b, &r); return len(r) != 2 }() {
		t.Fatalf("board limit expected 2")
	}

	// CORS header present.
	rec, _ = get("/state.json")
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing CORS header")
	}
}

// TestCurrentStreamEdgeTriggered verifies /current.stream sends an initial
// frame, pushes the current race only when the current heat changes, and
// suppresses ticks where the current heat is unchanged.
func TestCurrentStreamEdgeTriggered(t *testing.T) {
	ev := `"event":{"ID":"e1","Name":"Stream Night","EventType":"Race",` +
		`"Channels":["c1","c2"],"ChannelColors":["#f00","#0f0"],"Rounds":["rd1"],"Races":["a","b","c"]},` +
		`"channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},` +
		`{"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],` +
		`"pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"},{"ID":"p3","Name":"Cy"},` +
		`{"ID":"p4","Name":"Di"},{"ID":"p5","Name":"Ed"},{"ID":"p6","Name":"Fi"}],` +
		`"rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true}]`
	race := func(id string, rn int, started, ended bool, p1, p2 string) string {
		ts := ""
		if started {
			ts = `"Start":"2024-01-01T10:00:00Z",`
		}
		if ended {
			ts = `"Start":"2024-01-01T10:00:00Z","End":"2024-01-01T10:02:00Z",`
		}
		return fmt.Sprintf(`{"ID":%q,"Round":"rd1","RaceNumber":%d,"Valid":true,%s`+
			`"PilotChannels":[{"ID":"pc%s1","Pilot":%q,"Channel":"c1"},{"ID":"pc%s2","Pilot":%q,"Channel":"c2"}]}`,
			id, rn, ts, id, p1, id, p2)
	}
	mk := func(a, b, c [2]bool) dronedash.Snapshot {
		raw := "{" + ev + `,"races":[` +
			race("a", 1, a[0], a[1], "p1", "p2") + "," +
			race("b", 2, b[0], b[1], "p3", "p4") + "," +
			race("c", 3, c[0], c[1], "p5", "p6") + "]}"
		var snap dronedash.Snapshot
		if err := json.Unmarshal([]byte(raw), &snap); err != nil {
			t.Fatalf("mk: %v", err)
		}
		return snap
	}
	snapA := mk([2]bool{true, false}, [2]bool{false, false}, [2]bool{false, false}) // a running
	snapB := mk([2]bool{true, true}, [2]bool{true, false}, [2]bool{false, false})   // b running
	snapD := mk([2]bool{true, true}, [2]bool{true, true}, [2]bool{true, false})     // c running

	s := newLiveServer(brand{Name: "Stream Club", Accent: "#00aaff"})
	s.setSnapshot(snapA)
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/current.stream")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	frames := make(chan string, 16)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(frames)
				return
			}
			if l := strings.TrimSpace(line); strings.HasPrefix(l, "data: ") {
				frames <- strings.TrimPrefix(l, "data: ")
			}
		}
	}()
	nextRaceID := func() string {
		to := time.After(3 * time.Second)
		for {
			select {
			case d, ok := <-frames:
				if !ok {
					t.Fatal("stream closed early")
					return ""
				}
				var rc vmRace
				if json.Unmarshal([]byte(d), &rc) == nil && rc.ID != "" {
					return rc.ID
				}
			case <-to:
				t.Fatal("timeout waiting for current-race frame")
				return ""
			}
		}
	}

	if id := nextRaceID(); id != "a" {
		t.Fatalf("initial frame: want current=a, got %s", id)
	}
	s.setSnapshot(snapB)
	if id := nextRaceID(); id != "b" {
		t.Fatalf("after B: want current=b, got %s", id)
	}
	// Re-push the identical snapshot (no current change) then a real change. The
	// next frame must be c — if the unchanged tick weren't suppressed we'd get a
	// duplicate b here instead.
	s.setSnapshot(snapB)
	s.setSnapshot(snapD)
	if id := nextRaceID(); id != "c" {
		t.Fatalf("after dup-B + D: want current=c (dup suppressed), got %s", id)
	}
}

// TestSelectedRaceBecomesCurrent verifies the operator's CUED race (snapshot
// SelectedRaceID, from FPVTrackside's /current) is shown as Current when nothing
// is running — even when it's not first in running order (i.e. run out of order).
func TestSelectedRaceBecomesCurrent(t *testing.T) {
	pc := func(p, ch string) string {
		return `{"ID":"pc` + p + ch + `","Pilot":"` + p + `","Channel":"` + ch + `"}`
	}
	base := `{
	  "event":{"ID":"e1","Name":"OOO","EventType":"Race","Channels":["c1"],"ChannelColors":["#f00"],"Rounds":["rd1"],"Races":["a","b","c"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"},{"ID":"p3","Name":"Cy"}],
	  "rounds":[{"ID":"rd1","RoundNumber":1,"EventType":"Race","Valid":true,"Order":0}],
	  "races":[
	    {"ID":"a","Round":"rd1","RaceNumber":1,"Valid":true,"PilotChannels":[` + pc("p1", "c1") + `]},
	    {"ID":"b","Round":"rd1","RaceNumber":2,"Valid":true,"PilotChannels":[` + pc("p2", "c1") + `]},
	    {"ID":"c","Round":"rd1","RaceNumber":3,"Valid":true,"PilotChannels":[` + pc("p3", "c1") + `]}
	  ]`

	// With the operator cueing race "c" (the 3rd heat), it becomes Current.
	var sel dronedash.Snapshot
	if err := json.Unmarshal([]byte(base+`,"selected_race_id":"c"}`), &sel); err != nil {
		t.Fatalf("unmarshal sel: %v", err)
	}
	mv := buildMeeting(sel)
	if mv.Current == nil || mv.Current.ID != "c" {
		t.Fatalf("cued race c should be Current, got %+v", mv.Current)
	}
	for _, u := range mv.Upcoming {
		if u.ID == "c" {
			t.Fatalf("cued race c must not also be in Upcoming")
		}
	}

	// With no selection, Current falls back to the on-deck (first) heat "a".
	var none dronedash.Snapshot
	if err := json.Unmarshal([]byte(base+`}`), &none); err != nil {
		t.Fatalf("unmarshal none: %v", err)
	}
	if mv2 := buildMeeting(none); mv2.Current == nil || mv2.Current.ID != "a" {
		t.Fatalf("without selection, on-deck a should be Current, got %+v", mv2.Current)
	}
}

// TestFragmentIsExactlyTheShellBody pins the lock-step between a page and its
// "-body" partial. The soft-refresh fetches the body alone and patches it into
// #app, so anything the shell puts inside #app that the body does not render
// would be silently wiped the first time the board updated — a page that looks
// right until timing data arrives, which is the worst kind of drift.
func TestFragmentIsExactlyTheShellBody(t *testing.T) {
	s := newLiveServer(brand{Name: "Frag Club", Accent: "#00aaff"})
	mv := &meetingView{
		Name: "June TT", IsTT: true, Window: 3,
		Current: &vmRace{
			Label: "Round 1 — Race 1", IsTT: true, ConsecWindow: 3, State: "running", StartMs: 1700000000000,
			Pilots: []vmPilot{
				{Name: "Alice", ChannelLabel: "R1", Laps: []float64{5.1, 4.9}, Total: 12.0, CountedLaps: 3, Position: 1, BestLap: 4.9, BestConsec: 10.0},
				{Name: "Bob", ChannelLabel: "R2", Position: 2, Laps: []float64{6.0}, CountedLaps: 1},
			},
		},
	}
	mv.Recent = []vmRace{*mv.Current}
	mv.Board = []vmStanding{{Pos: 1, Name: "Alice", ChannelLabel: "R1", Laps: 3, BestLap: 4.9, BestConsec: 10.0}}
	fillDispLaps(mv)

	for _, pg := range []string{"home", "live", "races", "overlay", "waiting"} {
		page := renderPage(t, s, pg, mv)
		body := renderPage(t, s, pg+"-body", mv)
		if strings.TrimSpace(body) == "" {
			t.Fatalf("%s-body rendered empty", pg)
		}
		open := strings.Index(page, `<div id="app">`)
		if open < 0 {
			t.Fatalf("%s: no <div id=\"app\"> in the shell", pg)
		}
		inner := page[open+len(`<div id="app">`):]
		if !strings.HasPrefix(inner, body) {
			t.Fatalf("%s: #app does not start with %s-body — the shell and the fragment have drifted", pg, pg)
		}
	}
}

// TestFragmentETagSuppressesUnchangedBoards is the whole point of the ETag: an
// idle board must cost a 304 with no body, not a re-render. buildMeeting has no
// wall-clock in it, so an unchanged snapshot must hash identically.
func TestFragmentETagSuppressesUnchangedBoards(t *testing.T) {
	raw := `{
	  "event":{"ID":"e1","Name":"TT Night","EventType":"TimeTrial","Laps":3,"PBLaps":3,
	           "Channels":["c1"],"ChannelColors":["#ff0000"],"Rounds":["r1"],"Races":["race1"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"}],
	  "pilots":[{"ID":"p1","Name":"Alice"}],
	  "rounds":[{"ID":"r1","RoundNumber":1,"EventType":"TimeTrial","Valid":true}],
	  "races":[{"ID":"race1","Round":"r1","RaceNumber":1,"TargetLaps":3,"Valid":true,
	    "Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z",
	    "PilotChannels":[{"ID":"pc1","Pilot":"p1","Channel":"c1"}],
	    "Detections":[{"ID":"d0","Pilot":"p1","Valid":true},{"ID":"d1","Pilot":"p1","Valid":true}],
	    "Laps":[{"Detection":"d0","LapNumber":0,"LengthSeconds":2.0},
	            {"Detection":"d1","LapNumber":1,"LengthSeconds":5.0}]}]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := newLiveServer(brand{Name: "ETag Club", Accent: "#00aaff"})
	s.setSnapshot(snap)
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	get := func(etag string) (int, string, string) {
		req, err := http.NewRequest("GET", ts.URL+"/?fragment=1", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("ETag"), string(b)
	}

	code, tag, body := get("")
	if code != 200 {
		t.Fatalf("first fetch: want 200, got %d", code)
	}
	if tag == "" {
		t.Fatal("first fetch carried no ETag — every refresh will re-send the whole body")
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "<style") {
		t.Fatalf("fragment carried page chrome; that is the cost it exists to avoid:\n%s", body[:min(len(body), 300)])
	}

	code, _, body = get(tag)
	if code != http.StatusNotModified {
		t.Fatalf("unchanged board: want 304, got %d", code)
	}
	if body != "" {
		t.Fatalf("304 carried a body of %d bytes", len(body))
	}
}
