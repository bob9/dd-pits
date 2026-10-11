package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/dronedash"
)

// practiceThenTTSnapshot is the meeting shape a race director reported: the day
// opens with a PRACTICE round, then the real time trials start.
//
// The two rounds BOTH carry RoundNumber 1 and BOTH number their races from 1 —
// that is not a mistake in the fixture, it is what FPVTrackside emits.
// RaceManager.GetMaxRoundNumber allocates RoundNumber per EventType, and
// RaceNumber restarts inside each round. Only Round.Order (100 → 200) separates
// them.
//
// The practice heats have run; the time trials are queued.
const practiceThenTTSnapshot = `{
  "event":{"ID":"e1","Name":"Club Night","EventType":"Race",
           "Laps":3,"PBLaps":3,
           "Channels":["c1","c2"],"ChannelColors":["#f00","#0f0"],
           "Rounds":["rdP","rdT"],"Races":["pa","pb","ta","tb","tc"]},
  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
  "pilots":[{"ID":"p1","Name":"Al"},{"ID":"p2","Name":"Bo"}],
  "rounds":[
    {"ID":"rdP","RoundNumber":1,"Order":100,"EventType":"Practice","Valid":true},
    {"ID":"rdT","RoundNumber":1,"Order":200,"EventType":"TimeTrial","Valid":true}
  ],
  "races":[
    {"ID":"pa","Round":"rdP","RaceNumber":1,"TargetLaps":3,"Valid":true,
     "Start":"2026-06-05T09:00:00Z","End":"2026-06-05T09:02:00Z",
     "PilotChannels":[{"ID":"pcA","Pilot":"p1","Channel":"c1"}],
     "Detections":[{"ID":"d1","Pilot":"p1","Valid":true},
                   {"ID":"d2","Pilot":"p1","Valid":true},
                   {"ID":"d3","Pilot":"p1","Valid":true}],
     "Laps":[{"Detection":"d1","LapNumber":1,"LengthSeconds":9.0},
             {"Detection":"d2","LapNumber":2,"LengthSeconds":9.0},
             {"Detection":"d3","LapNumber":3,"LengthSeconds":9.0}]},
    {"ID":"pb","Round":"rdP","RaceNumber":2,"TargetLaps":3,"Valid":true,
     "Start":"2026-06-05T09:05:00Z","End":"2026-06-05T09:07:00Z",
     "PilotChannels":[{"ID":"pcB","Pilot":"p2","Channel":"c2"}],
     "Detections":[{"ID":"e1d","Pilot":"p2","Valid":true}],
     "Laps":[{"Detection":"e1d","LapNumber":1,"LengthSeconds":11.0}]},
    {"ID":"ta","Round":"rdT","RaceNumber":1,"TargetLaps":3,"Valid":true,
     "PilotChannels":[{"ID":"pcC","Pilot":"p1","Channel":"c1"}]},
    {"ID":"tb","Round":"rdT","RaceNumber":2,"TargetLaps":3,"Valid":true,
     "PilotChannels":[{"ID":"pcD","Pilot":"p2","Channel":"c2"}]},
    {"ID":"tc","Round":"rdT","RaceNumber":3,"TargetLaps":3,"Valid":true,
     "PilotChannels":[{"ID":"pcE","Pilot":"p1","Channel":"c1"}]}
  ]}`

func mustMeeting(t *testing.T, raw string) *meetingView {
	t.Helper()
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return buildMeeting(snap)
}

// A leading practice round used to make the whole meeting look finished: every
// time-trial heat shared its (round, race) position with a practice heat that had
// already run, so the after-the-current-position guard and the duplicate-position
// guard both fired and Next Up came out empty. Round.Order separates them.
func TestPracticeRoundDoesNotEmptyNextUp(t *testing.T) {
	mv := mustMeeting(t, practiceThenTTSnapshot)

	if mv.Current == nil {
		t.Fatal("no current heat")
	}
	// Nothing is running, so the on-deck heat is promoted to Current: that is the
	// first time trial, not a practice heat that already ran.
	if mv.Current.ID != "ta" {
		t.Errorf("current = %q, want the first time trial \"ta\"", mv.Current.ID)
	}
	if mv.UpcomingTotal != 2 {
		t.Errorf("upcoming total = %d, want the 2 remaining time trials", mv.UpcomingTotal)
	}
	got := make([]string, len(mv.Upcoming))
	for i, r := range mv.Upcoming {
		got[i] = r.ID
	}
	if strings.Join(got, ",") != "tb,tc" {
		t.Errorf("upcoming = %v, want [tb tc]", got)
	}
}

// The practice laps must not reach the standings — the reported "it does count on
// the dashboard". Al flew 9.0s laps in practice and nothing since, so he has no
// competitive time at all; his board row must be empty rather than showing a
// practice best.
func TestPracticeLapsDoNotScore(t *testing.T) {
	mv := mustMeeting(t, practiceThenTTSnapshot)

	if len(mv.Board) != 2 {
		t.Fatalf("board rows = %d, want 2", len(mv.Board))
	}
	for _, row := range mv.Board {
		if row.Laps != 0 || row.BestLap != 0 || row.BestConsec != 0 || row.FastestRace != 0 {
			t.Errorf("%s scored off practice: laps=%d best=%v consec=%v fastest=%v",
				row.Name, row.Laps, row.BestLap, row.BestConsec, row.FastestRace)
		}
		// The channel chip still resolves — it answers "what channel am I on",
		// which practice heats are perfectly good evidence for.
		if row.ChannelLabel == "" {
			t.Errorf("%s lost their channel chip", row.Name)
		}
	}
}

// A club that runs a pure practice day has nothing else to rank, so the exclusion
// must not blank their board.
func TestAllPracticeMeetingStillScores(t *testing.T) {
	raw := strings.NewReplacer(
		`{"ID":"rdT","RoundNumber":1,"Order":200,"EventType":"TimeTrial","Valid":true}`,
		`{"ID":"rdT","RoundNumber":2,"Order":200,"EventType":"Practice","Valid":true}`,
	).Replace(practiceThenTTSnapshot)

	mv := mustMeeting(t, raw)
	var al *vmStanding
	for i := range mv.Board {
		if mv.Board[i].Name == "Al" {
			al = &mv.Board[i]
		}
	}
	if al == nil {
		t.Fatal("Al missing from board")
	}
	if al.BestLap != 9.0 || al.Laps != 3 {
		t.Errorf("all-practice meeting should still score: best=%v laps=%d, want 9 / 3",
			al.BestLap, al.Laps)
	}
}

// "Staging" marks the heat actually on deck. With both rounds numbered 1 the sort
// interleaved them and the badge landed on a practice heat that had already run.
func TestStagingLandsOnTheNextRealHeat(t *testing.T) {
	mv := mustMeeting(t, practiceThenTTSnapshot)
	status := boardNextStatus(mv.Races)
	// "ta" is the first pending heat in running order and Al is in it.
	if status["Al"] != "Staging" {
		t.Errorf("Al next-race status = %q, want Staging", status["Al"])
	}
	// Bo's first pending heat is "tb", one heat further out.
	if status["Bo"] != "1" {
		t.Errorf("Bo next-race status = %q, want \"1\"", status["Bo"])
	}
}

// An empty never-run placeholder must not steal the Staging badge from the heat
// with a field in it.
func TestStagingSkipsEmptyPlaceholders(t *testing.T) {
	raw := strings.Replace(practiceThenTTSnapshot,
		`{"ID":"ta","Round":"rdT","RaceNumber":1,"TargetLaps":3,"Valid":true,
     "PilotChannels":[{"ID":"pcC","Pilot":"p1","Channel":"c1"}]}`,
		`{"ID":"ta","Round":"rdT","RaceNumber":1,"TargetLaps":3,"Valid":true,"PilotChannels":[]}`, 1)
	if raw == practiceThenTTSnapshot {
		t.Fatal("fixture rewrite did not apply")
	}
	mv := mustMeeting(t, raw)
	status := boardNextStatus(mv.Races)
	// "ta" is now empty, so "tb" (Bo) is the heat on deck.
	if status["Bo"] != "Staging" {
		t.Errorf("Bo next-race status = %q, want Staging", status["Bo"])
	}
}

func TestPracticeTypeMatchesEveryFPVTracksideSpelling(t *testing.T) {
	// EventTypes declares Practice/Practise = 0 and CasualPractice/CasualPractise
	// = 5, and C# serialises whichever name it resolves first — so all four must
	// match, as must the space-separated form we already tolerate elsewhere.
	for _, s := range []string{"Practice", "Practise", "CasualPractice", "CasualPractise", "Casual Practice"} {
		if !practiceType(s) {
			t.Errorf("practiceType(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"Race", "TimeTrial", "Time Trial", "Freestyle", "Endurance", "AggregateLaps", ""} {
		if practiceType(s) {
			t.Errorf("practiceType(%q) = true, want false", s)
		}
	}
}

func TestBeforeRaceUsesRoundOrderNotRoundNumber(t *testing.T) {
	// Practice Round 1 / Race 1 (order 100) vs TimeTrial Round 1 / Race 1 (order
	// 200): identical round + race numbers, so only Order can separate them.
	if !beforeRace(100, 1, 1, 200, 1, 1) {
		t.Error("practice heat should sort before the time trial that follows it")
	}
	if beforeRace(200, 1, 1, 100, 1, 1) {
		t.Error("the time trial must not sort before the practice round")
	}
	// Within one round, race number still decides.
	if !beforeRace(200, 1, 1, 200, 1, 2) {
		t.Error("race 1 should sort before race 2 in the same round")
	}
	// A meeting ingested before Order was carried (all zeros) falls through to the
	// old round/race tiebreak unchanged.
	if !beforeRace(0, 1, 1, 0, 2, 1) {
		t.Error("with no Order, round number must still decide")
	}
}
