package main

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/dronedash"
)

// pilotSnapshot is a two-heat meeting with known lap times, plus a practice
// heat carrying a lap faster than anything raced — the case that tells a
// scoring bug from a working one.
//
//	Round 1 Race 1 (Race):  Al  HS 1.0, laps 10.0 / 8.0 / 9.0   → total 28.0
//	                        Bo  HS 2.0, laps 12.0 / 11.5        → total 25.5
//	Round 2 Race 1 (Race):  Al  HS 1.5, laps 7.5 / 7.0          → total 16.0
//	Practice     Race:      Al  laps 3.0                        → must not score
//	Round 3 Race 1 pending: Al + Bo                             → "upcoming"
const pilotSnapshotJSON = `{
  "event":{"ID":"e1","Name":"Pilot Night","EventType":"Race","Laps":2,
           "Channels":["c1","c2"],"ChannelColors":["#f00","#00f"],
           "Rounds":["rd1","rd2","rdp","rd3"],"Races":["r1","r2","rp","r3"]},
  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
  "pilots":[{"ID":"al","Name":"Al Fast"},{"ID":"bo","Name":"Bo"}],
  "rounds":[{"ID":"rd1","RoundNumber":1,"Order":1,"EventType":"Race","Valid":true},
            {"ID":"rd2","RoundNumber":2,"Order":2,"EventType":"Race","Valid":true},
            {"ID":"rdp","RoundNumber":3,"Order":3,"EventType":"Practice","Valid":true},
            {"ID":"rd3","RoundNumber":4,"Order":4,"EventType":"Race","Valid":true}],
  "races":[
    {"ID":"r1","Round":"rd1","RaceNumber":1,"TargetLaps":3,"Valid":true,
     "Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:01:00Z",
     "PilotChannels":[{"ID":"pc1","Pilot":"al","Channel":"c1"},{"ID":"pc2","Pilot":"bo","Channel":"c2"}],
     "Detections":[{"ID":"a0","Pilot":"al","Valid":true},{"ID":"a1","Pilot":"al","Valid":true},
                   {"ID":"a2","Pilot":"al","Valid":true},{"ID":"a3","Pilot":"al","Valid":true},
                   {"ID":"b0","Pilot":"bo","Valid":true},{"ID":"b1","Pilot":"bo","Valid":true},
                   {"ID":"b2","Pilot":"bo","Valid":true}],
     "Laps":[{"Detection":"a0","LapNumber":0,"LengthSeconds":1.0},
             {"Detection":"a1","LapNumber":1,"LengthSeconds":10.0},
             {"Detection":"a2","LapNumber":2,"LengthSeconds":8.0},
             {"Detection":"a3","LapNumber":3,"LengthSeconds":9.0},
             {"Detection":"b0","LapNumber":0,"LengthSeconds":2.0},
             {"Detection":"b1","LapNumber":1,"LengthSeconds":12.0},
             {"Detection":"b2","LapNumber":2,"LengthSeconds":11.5}]},
    {"ID":"r2","Round":"rd2","RaceNumber":1,"TargetLaps":2,"Valid":true,
     "Start":"2026-06-05T09:10:00Z","End":"2026-06-05T09:11:00Z",
     "PilotChannels":[{"ID":"pc3","Pilot":"al","Channel":"c1"}],
     "Detections":[{"ID":"c0","Pilot":"al","Valid":true},{"ID":"c1d","Pilot":"al","Valid":true},
                   {"ID":"c2d","Pilot":"al","Valid":true}],
     "Laps":[{"Detection":"c0","LapNumber":0,"LengthSeconds":1.5},
             {"Detection":"c1d","LapNumber":1,"LengthSeconds":7.5},
             {"Detection":"c2d","LapNumber":2,"LengthSeconds":7.0}]},
    {"ID":"rp","Round":"rdp","RaceNumber":1,"TargetLaps":2,"Valid":true,
     "Start":"2026-06-05T09:20:00Z","End":"2026-06-05T09:21:00Z",
     "PilotChannels":[{"ID":"pc4","Pilot":"al","Channel":"c1"}],
     "Detections":[{"ID":"p1","Pilot":"al","Valid":true}],
     "Laps":[{"Detection":"p1","LapNumber":1,"LengthSeconds":3.0}]},
    {"ID":"r3","Round":"rd3","RaceNumber":1,"TargetLaps":2,"Valid":true,
     "PilotChannels":[{"ID":"pc5","Pilot":"al","Channel":"c1"},{"ID":"pc6","Pilot":"bo","Channel":"c2"}]}
  ]}`

func pilotTestMeeting(t *testing.T) *meetingView {
	t.Helper()
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(pilotSnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return buildMeeting(snap)
}

func nearly(a, b float64) bool { return a-b < 0.001 && b-a < 0.001 }

// TestPilotPageMatchesTheLeaderboard is the point of routing both surfaces
// through pilotScores. A race director reads a number off the board and quotes
// it off the pilot page (or the reverse), so the two disagreeing is worse than
// either being slightly wrong — it destroys trust in both.
func TestPilotPageMatchesTheLeaderboard(t *testing.T) {
	mv := pilotTestMeeting(t)
	for _, st := range mv.Board {
		pg, ok := buildPilotPage(mv, st.Name)
		if !ok {
			t.Fatalf("%s is on the board but has no pilot page", st.Name)
		}
		if pg.TotalLaps != st.Laps {
			t.Errorf("%s laps: board %d, pilot page %d", st.Name, st.Laps, pg.TotalLaps)
		}
		if !nearly(pg.BestLap.Value, st.BestLap) {
			t.Errorf("%s best lap: board %.3f, pilot page %.3f", st.Name, st.BestLap, pg.BestLap.Value)
		}
		if !nearly(pg.BestConsec.Value, st.BestConsec) {
			t.Errorf("%s consec: board %.3f, pilot page %.3f", st.Name, st.BestConsec, pg.BestConsec.Value)
		}
		if !nearly(pg.FastestRace.Value, st.FastestRace) {
			t.Errorf("%s fastest race: board %.3f, pilot page %.3f", st.Name, st.FastestRace, pg.FastestRace.Value)
		}
		if !nearly(pg.Holeshot.Value, st.Holeshot) {
			t.Errorf("%s holeshot: board %.3f, pilot page %.3f", st.Name, st.Holeshot, pg.Holeshot.Value)
		}
		if pg.Pos != st.Pos {
			t.Errorf("%s rank: board %d, pilot page %d", st.Name, st.Pos, pg.Pos)
		}
	}
}

// TestPilotStatsPointAtTheHeatThatSetThem — the stat cards are links, and a
// link to the wrong heat is worse than no link: it sends a race director to
// the wrong video for a qualifier submission.
func TestPilotStatsPointAtTheHeatThatSetThem(t *testing.T) {
	pg, ok := buildPilotPage(pilotTestMeeting(t), "Al Fast")
	if !ok {
		t.Fatal("Al Fast not found")
	}
	// Best lap: 7.0, lap 2 of the round-2 heat.
	if !nearly(pg.BestLap.Value, 7.0) || pg.BestLap.RaceID != "r2" {
		t.Errorf("best lap = %.3f in %q, want 7.000 in r2", pg.BestLap.Value, pg.BestLap.RaceID)
	}
	if pg.BestLap.Detail != "Lap 2" {
		t.Errorf("best lap detail = %q, want %q", pg.BestLap.Detail, "Lap 2")
	}
	// Holeshot: the better of 1.0 (r1) and 1.5 (r2).
	if !nearly(pg.Holeshot.Value, 1.0) || pg.Holeshot.RaceID != "r1" {
		t.Errorf("holeshot = %.3f in %q, want 1.000 in r1", pg.Holeshot.Value, pg.Holeshot.RaceID)
	}
	// Fastest race: 1.5+7.5+7.0 = 16.0 (r2) beats 1+10+8+9 = 28.0 (r1).
	if !nearly(pg.FastestRace.Value, 16.0) || pg.FastestRace.RaceID != "r2" {
		t.Errorf("fastest race = %.3f in %q, want 16.000 in r2", pg.FastestRace.Value, pg.FastestRace.RaceID)
	}
	// Best 2 consecutive (Laps=2 → window 2): 7.5+7.0 = 14.5 in r2.
	if pg.Window != 2 {
		t.Fatalf("window = %d, want 2", pg.Window)
	}
	if !nearly(pg.BestConsec.Value, 14.5) || pg.BestConsec.RaceID != "r2" {
		t.Errorf("consec = %.3f in %q, want 14.500 in r2", pg.BestConsec.Value, pg.BestConsec.RaceID)
	}
	if pg.BestConsec.Detail != "Laps 1–2" {
		t.Errorf("consec detail = %q, want %q", pg.BestConsec.Detail, "Laps 1–2")
	}
	// And the subtitle is the cloud's "<context> · <heat>" shape.
	if got := pg.BestLap.Subtitle(); !strings.HasPrefix(got, "Lap 2 · ") {
		t.Errorf("best lap subtitle = %q, want it to lead with the lap", got)
	}
}

// TestPracticeLapsDoNotSetMeetingRecords — the practice heat carries a 3.0s
// lap, far quicker than anything raced.
//
// It must still appear in the lap table (it was flown) and may still carry the
// PER-HEAT highlights, because a practice heat's own card highlights its own
// quickest laps. What it must never do is take a MEETING-wide record: not the
// best-lap stat card, and not the overall-best colour. Same split as the
// cloud's precomputeEventStats.
func TestPracticeLapsDoNotSetMeetingRecords(t *testing.T) {
	pg, ok := buildPilotPage(pilotTestMeeting(t), "Al Fast")
	if !ok {
		t.Fatal("Al Fast not found")
	}
	if nearly(pg.BestLap.Value, 3.0) {
		t.Fatalf("the practice lap became the best lap (%.3f)", pg.BestLap.Value)
	}
	var sawPractice bool
	for _, g := range pg.Groups {
		if g.RaceID != "rp" {
			continue
		}
		sawPractice = true
		for _, l := range g.Laps {
			if !nearly(l.Seconds, 3.0) {
				continue
			}
			if l.Highlight == "overall-best" || l.Highlight == "overall-pb" {
				t.Errorf("practice lap took the meeting record colour %q", l.Highlight)
			}
		}
	}
	if !sawPractice {
		t.Error("the practice heat vanished from the lap table; it was still flown")
	}
	// And the meeting's fastest-lap colour stayed on the lap that earned it.
	var sawOverall bool
	for _, g := range pg.Groups {
		for _, l := range g.Laps {
			if l.Highlight == "overall-best" {
				sawOverall = true
				if !nearly(l.Seconds, 7.0) {
					t.Errorf("overall-best sits on a %.3f lap, want the 7.000 raced one", l.Seconds)
				}
			}
		}
	}
	if !sawOverall {
		t.Error("no lap carries the meeting's fastest-lap colour")
	}
}

// TestPilotLapTableAndUpcoming covers the two non-chart tabs.
func TestPilotLapTableAndUpcoming(t *testing.T) {
	pg, ok := buildPilotPage(pilotTestMeeting(t), "Al Fast")
	if !ok {
		t.Fatal("Al Fast not found")
	}
	// Three flown heats, in running order; the pending one is not among them.
	var ids []string
	for _, g := range pg.Groups {
		ids = append(ids, g.RaceID)
	}
	if strings.Join(ids, ",") != "r1,r2,rp" {
		t.Errorf("lap-table heats = %v, want r1,r2,rp in running order", ids)
	}
	// Holeshot first, then the racing laps in order.
	first := pg.Groups[0].Laps
	if len(first) != 4 || first[0].Number != 0 || !nearly(first[0].Seconds, 1.0) {
		t.Fatalf("r1 laps = %+v, want HS then three laps", first)
	}
	if first[1].Number != 1 || !nearly(first[1].Seconds, 10.0) {
		t.Errorf("r1 lap 1 = %+v, want 10.000", first[1])
	}
	// The pending heat is the upcoming card, with its whole field for the chips.
	if len(pg.Upcoming) != 1 || pg.Upcoming[0].RaceID != "r3" {
		t.Fatalf("upcoming = %+v, want just r3", pg.Upcoming)
	}
	if len(pg.Upcoming[0].Pilots) != 2 {
		t.Errorf("upcoming field = %d pilots, want both", len(pg.Upcoming[0].Pilots))
	}
}

// TestPilotLapHighlights pins the four-colour scheme to the cloud's, since the
// two pages sit side by side at a meeting.
func TestPilotLapHighlights(t *testing.T) {
	mv := pilotTestMeeting(t)
	pg, _ := buildPilotPage(mv, "Al Fast")
	find := func(raceID string, lap int) pilotLap {
		t.Helper()
		for _, g := range pg.Groups {
			if g.RaceID != raceID {
				continue
			}
			for _, l := range g.Laps {
				if l.Number == lap {
					return l
				}
			}
		}
		t.Fatalf("no lap %d in %s", lap, raceID)
		return pilotLap{}
	}
	// 7.0 is the fastest lap anyone flew in a scoring heat.
	if got := find("r2", 2).Highlight; got != "overall-best" {
		t.Errorf("meeting's fastest lap highlighted %q, want overall-best", got)
	}
	// 8.0 is Al's best in r1 and the best lap in that heat — race-best outranks
	// race-pb, as it does on the cloud.
	if got := find("r1", 2).Highlight; got != "race-best" {
		t.Errorf("heat-best lap highlighted %q, want race-best", got)
	}
	// A lap that beat nothing carries no highlight.
	if got := find("r1", 1).Highlight; got != "" {
		t.Errorf("ordinary lap highlighted %q, want none", got)
	}
	// Bo's best in his only heat is that heat's second-best — his own PB.
	pgBo, _ := buildPilotPage(mv, "Bo")
	var bo11 pilotLap
	for _, l := range pgBo.Groups[0].Laps {
		if l.Number == 2 {
			bo11 = l
		}
	}
	if bo11.Highlight != "overall-pb" {
		t.Errorf("Bo's meeting best highlighted %q, want overall-pb", bo11.Highlight)
	}
}

// TestTotalLapsExcludesTheHoleshot — reported from the field: the holeshot is
// the crossing that STARTS lap one, not a lap flown, so counting it added one
// phantom lap per heat to a card that says "completed laps".
//
// Al flew 3 + 2 racing laps in the two scoring heats (the practice heat's lap
// does not count), each behind a holeshot: 5, not 7.
func TestTotalLapsExcludesTheHoleshot(t *testing.T) {
	pg, ok := buildPilotPage(pilotTestMeeting(t), "Al Fast")
	if !ok {
		t.Fatal("Al Fast not found")
	}
	if pg.TotalLaps != 5 {
		t.Errorf("total laps = %d, want 5 (holeshots are not laps)", pg.TotalLaps)
	}
	// The holeshots were still recorded — this is a counting rule, not a
	// filter, and the holeshot card still has its time.
	if !nearly(pg.Holeshot.Value, 1.0) {
		t.Errorf("holeshot stat = %.3f, want 1.000 — still tracked", pg.Holeshot.Value)
	}
}

// TestFastestRaceOnlyCountsCompletedRuns — reported from the field: a run the
// pilot never finished posted a "fastest total race" nothing could beat,
// because a shorter run is always a smaller sum.
//
// Bo has one 2-lap heat behind him (target 3, so he never went the distance)
// and one he completed. Only the completed one may set the card.
func TestFastestRaceOnlyCountsCompletedRuns(t *testing.T) {
	races := []vmRace{
		// Pulled out after one lap — the quickest total on the meeting, and
		// meaningless.
		{ID: "r1", Label: "Round 1 — Race 1", TargetLaps: 3,
			Pilots: []vmPilot{{Name: "Bo", Holeshot: 2.0, Laps: []float64{8.0}}}},
		// Went the distance.
		{ID: "r2", Label: "Round 2 — Race 1", TargetLaps: 3,
			Pilots: []vmPilot{{Name: "Bo", Holeshot: 2.0, Laps: []float64{9.0, 9.0, 9.0}}}},
		// Finished, but retired — FPVTrackside called it a DNF.
		{ID: "r3", Label: "Round 3 — Race 1", TargetLaps: 3,
			Pilots: []vmPilot{{Name: "Bo", Holeshot: 1.0, Laps: []float64{5.0, 5.0, 5.0}, DNF: true}}},
	}
	s := pilotScores(races, 3)["Bo"]
	if s == nil {
		t.Fatal("Bo scored nothing")
	}
	if s.FastestRace.RaceID != "r2" {
		t.Errorf("fastest race came from %q, want r2 — the only run he completed clean",
			s.FastestRace.RaceID)
	}
	if !nearly(s.FastestRace.Value, 29.0) {
		t.Errorf("fastest race = %.3f, want 29.000 (2.0 + 3×9.0)", s.FastestRace.Value)
	}
	if s.FastestRace.Detail != "3 laps" {
		t.Errorf("fastest race detail = %q, want %q", s.FastestRace.Detail, "3 laps")
	}

	// Laps past the flag are not part of the race: they must not inflate the
	// total, or a heat is only ever compared with one of the same lap count.
	over := []vmRace{{ID: "r4", Label: "Round 4 — Time Trial 1", TargetLaps: 3,
		Pilots: []vmPilot{{Name: "Bo", Holeshot: 2.0, Laps: []float64{9.0, 9.0, 9.0, 9.0, 9.0}}}}}
	if got := pilotScores(over, 3)["Bo"].FastestRace; !nearly(got.Value, 29.0) {
		t.Errorf("fastest race with bonus laps = %.3f, want 29.000 (capped at the distance)", got.Value)
	}

	// A heat with no configured distance can't be judged, so it keeps counting
	// everything rather than vanishing from the card.
	none := []vmRace{{ID: "r5", Label: "Round 5 — Race 1",
		Pilots: []vmPilot{{Name: "Bo", Holeshot: 1.0, Laps: []float64{4.0, 4.0}}}}}
	if got := pilotScores(none, 3)["Bo"].FastestRace; !nearly(got.Value, 9.0) {
		t.Errorf("fastest race with no target laps = %.3f, want 9.000", got.Value)
	}
}

// TestChartSeriesFilter — the requested "just the laps" / "just the holeshots"
// views. Filtering happens server-side so the axis rescales: holeshots drawn
// against a lap-time axis are a row of bars flat on the floor.
func TestChartSeriesFilter(t *testing.T) {
	mv := pilotTestMeeting(t)
	pg, ok := buildPilotPage(mv, "Al Fast")
	if !ok {
		t.Fatal("Al Fast not found")
	}
	all := len(pg.Chart.Bars)
	if pg.Series != seriesAll {
		t.Errorf("default series = %q, want %q", pg.Series, seriesAll)
	}

	pg.applySeries(seriesLaps)
	laps := len(pg.Chart.Bars)
	for _, b := range pg.Chart.Bars {
		if b.Label == "HS" {
			t.Error("a holeshot survived the laps-only chart")
		}
	}

	pg.applySeries(seriesHS)
	hs := len(pg.Chart.Bars)
	for _, b := range pg.Chart.Bars {
		if b.Label != "HS" {
			t.Errorf("a racing lap (%s) survived the holeshots-only chart", b.Label)
		}
	}
	if hs == 0 || laps == 0 || laps+hs != all {
		t.Errorf("series split %d laps + %d holeshots != %d bars", laps, hs, all)
	}
	// Al's holeshots are 1.0 and 1.5, so the axis must be scaled for those —
	// not for the 3-10s laps, which is the whole reason this is a re-layout.
	top, err := strconv.ParseFloat(pg.Chart.Ticks[0].Label, 64)
	if err != nil {
		t.Fatalf("top tick %q is not a number: %v", pg.Chart.Ticks[0].Label, err)
	}
	if top > 2.0 {
		t.Errorf("holeshot chart's top tick is %.1f — the axis is still scaled for lap times", top)
	}
	// And the reference line follows the series it is drawn over.
	if !strings.HasPrefix(pg.Chart.BestLabel, "Best HS") {
		t.Errorf("holeshot chart marker = %q, want the best holeshot", pg.Chart.BestLabel)
	}

	// A junk series renders the full chart rather than blanking the panel.
	pg.applySeries("nonsense")
	if pg.Series != seriesAll || len(pg.Chart.Bars) != all {
		t.Errorf("unknown series gave %q with %d bars, want the full chart",
			pg.Series, len(pg.Chart.Bars))
	}
}

// TestPilotChartScalesToTheData — a zero-based axis renders a field of 20-25s
// laps as bars of identical height, which is a chart carrying no information.
func TestPilotChartScalesToTheData(t *testing.T) {
	laps := []pilotLap{
		{Number: 1, Seconds: 20.0, RaceID: "r1"},
		{Number: 2, Seconds: 25.0, RaceID: "r1"},
		{Number: 3, Seconds: 22.5, RaceID: "r1"},
	}
	c := buildPilotChart(laps, 20.0, "PB")
	if c.Empty || len(c.Bars) != 3 {
		t.Fatalf("chart = %+v, want 3 bars", c)
	}
	// The slowest lap is the tallest, and the spread is visible rather than
	// squashed into the top few per cent of the plot.
	slowest, fastest := c.Bars[1].H, c.Bars[0].H
	if slowest <= fastest {
		t.Fatalf("slower lap drew a shorter bar (%.1f vs %.1f)", slowest, fastest)
	}
	if slowest-fastest < pcPlotH*0.5 {
		t.Errorf("25.0s vs 20.0s differ by only %.1f of %d units — axis is not scaled to the data",
			slowest-fastest, pcPlotH)
	}
	// Bars stay inside the plot area.
	for _, b := range c.Bars {
		if b.Y < pcPadTop-0.01 || b.Y+b.H > pcPadTop+pcPlotH+0.01 {
			t.Errorf("bar %s escapes the plot: y=%.2f h=%.2f", b.Key, b.Y, b.H)
		}
		if b.Href == "" {
			t.Errorf("bar %s has no link to its heat", b.Key)
		}
	}
	// The FASTEST bar still has to be drawable and clickable — it sits on the
	// axis floor, and it is the bar you most want to open (the heat that set
	// your best lap). Asymmetric padding keeps it a real target.
	if c.Bars[0].H < 20 {
		t.Errorf("fastest bar is %.1f units tall — too small to see or click", c.Bars[0].H)
	}
	// Every bar identical must not divide by zero.
	flat := buildPilotChart([]pilotLap{{Number: 1, Seconds: 9.0}, {Number: 2, Seconds: 9.0}}, 9.0, "PB")
	for _, b := range flat.Bars {
		if b.H < 0 || b.H > pcPlotH+0.01 {
			t.Fatalf("flat chart produced bar height %.2f", b.H)
		}
	}
	if e := buildPilotChart(nil, 0, "PB"); !e.Empty {
		t.Error("a pilot with no laps should get the empty chart state")
	}
}

// TestPilotPageServes drives the real route, including the soft-refresh
// fragment the page updates itself with.
func TestPilotPageServes(t *testing.T) {
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(pilotSnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := newLiveServer(brand{Name: "Test Club", Accent: "#00aaff"})
	s.setSnapshot(snap)
	h := s.handler()

	get := func(path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}

	// The name carries a space — the link on the board must survive escaping.
	code, body := get("/dronedash/pilot?name=Al%20Fast")
	if code != 200 {
		t.Fatalf("pilot page status %d", code)
	}
	for _, want := range []string{
		"Al Fast",               // header
		"Best Lap",              // stat cards
		"Fastest 2 Consecutive", // the meeting's window, named
		"Holeshot", "Total Laps",
		"7.000",                                    // his best lap
		"Analytics", "Lap Table", "Upcoming Races", // tabs
		"<svg",                            // the chart, server-rendered
		"lap-badge",                       // lap table
		`href="/dronedash/races#race-r2"`, // stat card deep-link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pilot page missing %q", want)
		}
	}
	// No CDN: the venue is offline, so nothing may be fetched off the network.
	// The support link is a plain hyperlink, not a fetched resource.
	offline := strings.ReplaceAll(body, supportURL, "")
	if strings.Contains(offline, "//cdn.") || strings.Contains(offline, "https://") {
		t.Error("pilot page references an external URL — it must work offline")
	}

	// Case-insensitive, like ddapi's /pilot.json.
	if code, _ := get("/dronedash/pilot?name=al+fast"); code != 200 {
		t.Errorf("case-insensitive lookup status %d", code)
	}

	// The series chips are reachable from the page and keep the pilot with them.
	for _, want := range []string{
		`href="/dronedash/pilot?name=Al%20Fast&amp;series=laps"`,
		`href="/dronedash/pilot?name=Al%20Fast&amp;series=hs"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("analytics tab missing series link %q", want)
		}
	}
	// ...and ?series= actually changes what is drawn, on the route, not just in
	// the builder — the soft refresh re-fetches this URL, so it has to hold.
	code, hsBody := get("/dronedash/pilot?name=Al%20Fast&series=hs")
	if code != 200 {
		t.Fatalf("holeshot series status %d", code)
	}
	if strings.Count(hsBody, `class="pc-bar"`) != 2 {
		t.Errorf("holeshots-only chart drew %d bars, want Al's 2 holeshots",
			strings.Count(hsBody, `class="pc-bar"`))
	}
	if !strings.Contains(hsBody, "Best HS") {
		t.Error("holeshots-only chart kept the lap-time marker")
	}

	// An unknown pilot explains itself rather than 404ing a shared link.
	code, body = get("/dronedash/pilot?name=Nobody")
	if code != 200 || !strings.Contains(body, "Pilot not found") {
		t.Errorf("unknown pilot: status %d, body has not-found = %v",
			code, strings.Contains(body, "Pilot not found"))
	}

	// The fragment is the body alone — what the soft-refresh swaps into #app.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dronedash/pilot?name=Al%20Fast&fragment=1", nil))
	frag := rec.Body.String()
	if rec.Code != 200 || strings.Contains(frag, "<!doctype") {
		t.Fatalf("fragment should be a bare body, got status %d", rec.Code)
	}
	if !strings.Contains(frag, "Al Fast") {
		t.Error("fragment lost the pilot")
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("fragment has no ETag — an unchanged board would re-render for nothing")
	}
}

// TestBoardLinksToPilotPages — the page is only useful if you can reach it, and
// the leaderboard is where a race director looks for a pilot.
func TestBoardLinksToPilotPages(t *testing.T) {
	s := newLiveServer(brand{Name: "Test Club", Accent: "#00aaff"})
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(pilotSnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s.setSnapshot(snap)
	mv, _, _ := s.meeting()
	home := renderPage(t, s, "home", mv)
	if !strings.Contains(home, `href="/dronedash/pilot?name=Al%20Fast"`) {
		t.Error("the leaderboard does not link Al Fast to his pilot page")
	}
	// And the race history anchors the stat-card links jump to.
	races := renderPage(t, s, "races", mv)
	if !strings.Contains(races, `id="race-r2"`) {
		t.Error("race history has no per-heat anchor for the pilot page to link at")
	}
}
