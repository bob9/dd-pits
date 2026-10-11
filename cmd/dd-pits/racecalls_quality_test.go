package main

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// A lap call's punctuation follows the lap: beat your best and it exclaims,
// crawl and it trails off, ordinary stays neutral, and a pilot with no
// yardstick yet is never guessed at. Punctuation is the delivery channel the
// local cloned voice actually has, so this IS the excitement wiring.
func TestLapCallPunctuationFollowsTheLap(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundRaceLap: {Key: SoundRaceLap, Text: "{pilot} lap {lap} in {time} in {position}", Enabled: true},
	}, loaded: true}
	rc := NewRaceCaller(set)
	_ = t
	race := ddapi.Race{ID: "r1", TypeLabel: "Race"}
	pilot := ddapi.Pilot{Name: "Willman"}

	// No best yet: neutral, whatever the number.
	if call, ok := rc.lapCall(race, pilot, 1, 4.72); !ok ||
		strings.HasSuffix(call.Text, "!") || strings.HasSuffix(call.Text, "...") {
		t.Fatalf("first lap should be neutral, got %q", call.Text)
	}

	rc.bestLap["Willman"] = 4.72

	if call, _ := rc.lapCall(race, pilot, 2, 4.50); !strings.HasSuffix(call.Text, "!") {
		t.Errorf("a lap under their best should exclaim, got %q", call.Text)
	}
	if call, _ := rc.lapCall(race, pilot, 3, 5.10); strings.HasSuffix(call.Text, "!") ||
		strings.HasSuffix(call.Text, "...") {
		t.Errorf("an ordinary lap should stay neutral, got %q", call.Text)
	}
	if call, _ := rc.lapCall(race, pilot, 4, 9.90); !strings.HasSuffix(call.Text, "...") {
		t.Errorf("a crawl (>1.6x best) should trail off, got %q", call.Text)
	}
}

// The race-over flood of results is one call, not a queue; a lone mid-race
// crossing keeps its own instant call.
func TestResultsInOnePassMerge(t *testing.T) {
	calls := []RaceCall{
		{Key: SoundRaceLap, Text: "bob9 lap 4 in 8.37 in 2nd"},
		{Key: SoundRaceDone, Text: "bob9 finished in 2nd"},
		{Key: SoundRaceDone, Text: "Wizz finished in 3rd"},
		{Key: SoundRaceOver, Text: "Race over"},
	}
	got := mergeResults(calls)
	if len(got) != 3 {
		t.Fatalf("want 3 calls after merge, got %d: %+v", len(got), got)
	}
	if got[1].Text != "bob9 finished in 2nd. Wizz finished in 3rd" {
		t.Errorf("merged text = %q", got[1].Text)
	}
	if got[2].Key != SoundRaceOver {
		t.Errorf("ordering not preserved: %+v", got)
	}

	solo := []RaceCall{{Key: SoundRaceDone, Text: "MirellaFPV finished in 1st"}}
	if out := mergeResults(solo); len(out) != 1 || out[0].Text != "MirellaFPV finished in 1st" {
		t.Errorf("a lone crossing must stay its own call: %+v", out)
	}
}

// A pilot marked crashed out MID-RACE is called once, neutrally — and not
// re-called on every poll, and not called at all when the marking lands after
// the heat (results own that moment).
func TestCrashOutIsCalledOnceDuringTheRace(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{}, loaded: true}
	rc := NewRaceCaller(set)
	race := ddapi.Race{ID: "r1", State: "running", TypeLabel: "Race"}
	pilot := ddapi.Pilot{Name: "Willman", DNF: true}

	calls := rc.forPilot(race, pilot)
	found := 0
	for _, c := range calls {
		if c.Text == "Willman is out" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one out-call, got %d in %+v", found, calls)
	}
	// The next poll re-sends the same state: silence.
	for _, c := range rc.forPilot(race, pilot) {
		if c.Text == "Willman is out" {
			t.Fatal("the out-call repeated on the next poll")
		}
	}
	// Marked after the heat: no call.
	rc2 := NewRaceCaller(set)
	ended := ddapi.Race{ID: "r2", State: "ended", TypeLabel: "Race"}
	for _, c := range rc2.forPilot(ended, pilot) {
		if c.Text == "Willman is out" {
			t.Fatal("an after-the-heat DNF must not be announced as an out")
		}
	}
}
