package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

const callSounds = `<?xml version="1.0" encoding="utf-8"?>
<ArrayOfSound>
  <Sound><Key>RaceLap</Key><TextToSpeech>{pilot} lap {lapnumber} in {laptime} in {position}</TextToSpeech><Enabled>true</Enabled></Sound>
  <Sound><Key>RaceDone</Key><TextToSpeech>{pilot} finished in {position}</TextToSpeech><Enabled>true</Enabled></Sound>
  <Sound><Key>RaceStart</Key><TextToSpeech>Go</TextToSpeech><Enabled>true</Enabled></Sound>
  <Sound><Key>RaceOver</Key><TextToSpeech>Race over</TextToSpeech><Enabled>true</Enabled></Sound>
  <Sound><Key>NewLapRecord</Key><TextToSpeech>{pilot} new record {count} lap{s} in {lapstime}</TextToSpeech><Enabled>true</Enabled></Sound>
</ArrayOfSound>`

func callerForTest(t *testing.T) *RaceCaller {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Sounds.xml")
	if err := os.WriteFile(p, []byte(callSounds), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSoundSet(p)
	if err != nil {
		t.Fatal(err)
	}
	return NewRaceCaller(s)
}

func meeting(races ...ddapi.Race) ddapi.Meeting { return ddapi.Meeting{Races: races} }

// Attaching mid-meeting must not announce the whole day at once. The first view
// is history; only what arrives after it is news.
func TestFirstViewIsPrimedNotAnnounced(t *testing.T) {
	rc := callerForTest(t)
	got := rc.Calls(meeting(ddapi.Race{
		ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{23.9, 24.1}, Position: 1}},
	}))
	if len(got) != 0 {
		t.Fatalf("first view should be silent, got %d calls: %v", len(got), got)
	}
}

func TestNewLapIsCalledOnce(t *testing.T) {
	rc := callerForTest(t)
	race := ddapi.Race{ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{23.90}, Position: 1}}}
	rc.Calls(meeting(race))

	race.Pilots[0].Laps = []float64{23.90, 24.08}
	got := rc.Calls(meeting(race))

	if len(got) != 1 {
		t.Fatalf("expected one call, got %d: %v", len(got), got)
	}
	if want := "bob9 lap 2 in 24.08 in 1st"; got[0].Text != want {
		t.Fatalf("got %q want %q", got[0].Text, want)
	}

	// The same view again must say nothing — FPVTrackside re-sends the running
	// heat on every poll.
	if again := rc.Calls(meeting(race)); len(again) != 0 {
		t.Fatalf("a repeated view must be silent, got %v", again)
	}
}

// Several crossings between polls must all be called, in order.
func TestLapsMissedBetweenPollsAreAllCalled(t *testing.T) {
	rc := callerForTest(t)
	race := ddapi.Race{ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{23.9}, Position: 1}}}
	rc.Calls(meeting(race))

	race.Pilots[0].Laps = []float64{23.9, 24.0, 24.2, 23.7}
	got := rc.Calls(meeting(race))

	if len(got) < 3 {
		t.Fatalf("expected three lap calls, got %d: %v", len(got), got)
	}
	for i, want := range []string{"lap 2", "lap 3", "lap 4"} {
		if !strings.Contains(got[i].Text, want) {
			t.Fatalf("call %d = %q, expected it to contain %q", i, got[i].Text, want)
		}
	}
}

func TestRaceStateCallsFireOnTransition(t *testing.T) {
	rc := callerForTest(t)
	rc.Calls(meeting(ddapi.Race{ID: "r1", State: "pending"}))

	started := rc.Calls(meeting(ddapi.Race{ID: "r1", State: "running"}))
	if len(started) != 1 || started[0].Text != "Go" {
		t.Fatalf("expected the start call, got %v", started)
	}

	ended := rc.Calls(meeting(ddapi.Race{ID: "r1", State: "ended"}))
	if len(ended) == 0 || ended[len(ended)-1].Text != "Race over" {
		t.Fatalf("expected the race-over call, got %v", ended)
	}
}

// A race call must always outrank commentary — that is the whole reason the
// audio moved into one process.
func TestCallsOutrankCommentary(t *testing.T) {
	if callPriorityLap <= callPriorityCommentary {
		t.Fatal("a lap call must outrank commentary")
	}
	if callPriorityRaceState <= callPriorityLap {
		t.Fatal("the heat starting must outrank a lap call")
	}
}

// A sub-second "lap" is a double detection at the gate. Announcing it as a
// record reads a false fact out over the PA.
func TestImpossibleLapIsNotARecord(t *testing.T) {
	rc := callerForTest(t)
	race := ddapi.Race{ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{23.9}, Position: 1}}}
	rc.Calls(meeting(race))

	race.Pilots[0].Laps = []float64{23.9, 0.47}
	for _, c := range rc.Calls(meeting(race)) {
		if c.Key == SoundNewLapRecord {
			t.Fatalf("a 0.47s detection must not be called a record: %q", c.Text)
		}
	}
}

// TestRepriMeTreatsTheNextViewAsHistory covers a flood heard on air.
//
// FPVTrackside was restarted underneath a running dd-pits. What came back was a
// different event with race ids the announcer had never seen, so every race in
// it looked like news and the day's whole history was called at once — over a
// hundred calls in one snapshot, which filled the bus and pushed the actual
// racing off it ("dropped, the queue is full" for lap after lap).
func TestReprimeTreatsTheNextViewAsHistory(t *testing.T) {
	rc := callerForTest(t)

	mv := ddapi.Meeting{Races: []ddapi.Race{{
		ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{11.4}, Position: 1}},
	}}}
	rc.Calls(mv) // startup baseline

	// A lap flown while connected is news, as normal.
	mv.Races[0].Pilots[0].Laps = []float64{11.4, 11.6}
	if got := rc.Calls(mv); len(got) == 0 {
		t.Fatal("a new lap on a connected link must be called")
	}

	// The link breaks and comes back on a different event.
	rc.Reprime("test")
	back := ddapi.Meeting{Races: []ddapi.Race{{
		ID: "other-event-r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "Fizz", Laps: []float64{12.1, 12.3, 12.0}, Position: 1}},
	}}}
	if got := rc.Calls(back); len(got) != 0 {
		t.Errorf("the first view after a reconnect is the state of the world, not news; got %d calls", len(got))
	}
	// And racing resumes normally from that baseline.
	back.Races[0].Pilots[0].Laps = append(back.Races[0].Pilots[0].Laps, 11.9)
	if got := rc.Calls(back); len(got) == 0 {
		t.Error("a lap flown after the re-baseline must be called")
	}
}

// TestNamelessPilotIsNotCalled covers the other half of the same incident: the
// half-formed snapshot produced laps with no pilot on them, voiced as "lap 6 in
// 12.68 in 1st", which tells a listener nothing at all.
func TestNamelessPilotIsNotCalled(t *testing.T) {
	rc := callerForTest(t)
	mv := ddapi.Meeting{Races: []ddapi.Race{{ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "", Laps: []float64{}, Position: 1}}}}}
	rc.Calls(mv)

	mv.Races[0].Pilots[0].Laps = []float64{12.68}
	for _, c := range rc.Calls(mv) {
		if c.Key == SoundRaceLap || c.Key == SoundTimeTrialEveryLap {
			t.Errorf("a lap with no pilot on it must not be announced; got %q", c.Text)
		}
	}
}

// TestRestartIsDetectedWhenLapsGoBackwards covers a re-run heat.
//
// The state-transition signal (pending -> running) does not always fire for a
// restart: FPVTrackside can clear a heat's laps and re-arm it while the state
// stays "running". The only remaining evidence is a pilot who had four laps now
// having none — and without noticing it, the abandoned run's queued lap times
// go to air over the new one.
func TestRestartIsDetectedWhenLapsGoBackwards(t *testing.T) {
	rc := callerForTest(t)
	mv := ddapi.Meeting{Races: []ddapi.Race{{ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{11.4, 11.6}, Position: 1}}}}}
	rc.Calls(mv)
	if rc.TakeRestart() {
		t.Fatal("a first view is not a restart")
	}

	// Laps flown normally: still not a restart.
	mv.Races[0].Pilots[0].Laps = []float64{11.4, 11.6, 11.5}
	rc.Calls(mv)
	if rc.TakeRestart() {
		t.Error("flying another lap is not a restart")
	}

	// The heat is re-run: laps cleared, state never left "running".
	mv.Races[0].Pilots[0].Laps = nil
	rc.Calls(mv)
	if !rc.TakeRestart() {
		t.Error("laps going backwards is a re-run heat and must clear the bus")
	}
	if rc.TakeRestart() {
		t.Error("TakeRestart must clear the flag, or every later view re-clears the bus")
	}
}

// TestGreenIsNoticedEvenWhenNothingIsSaidAboutIt covers a fault created by
// silencing the "Go" call.
//
// A heat going green clears the bus — build-up and stale commentary have to
// stop the moment there is live racing. That was triggered by dd-pits seeing a
// RaceStart CALL, so blanking that sound's wording (a reasonable thing to want,
// since FPVTrackside plays the start tone itself) also stopped the heat being
// noticed, and the old audio played on over the start.
func TestGreenIsNoticedEvenWhenNothingIsSaidAboutIt(t *testing.T) {
	// A sound set with RaceStart present but SILENT — exactly the live config.
	p := filepath.Join(t.TempDir(), "Sounds.xml")
	xml := `<?xml version="1.0" encoding="utf-8"?>
<ArrayOfSound>
  <Sound><Key>RaceStart</Key><TextToSpeech></TextToSpeech><Enabled>true</Enabled></Sound>
  <Sound><Key>RaceLap</Key><TextToSpeech>{pilot} lap {lapnumber}</TextToSpeech><Enabled>true</Enabled></Sound>
</ArrayOfSound>`
	if err := os.WriteFile(p, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSoundSet(p)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRaceCaller(set)

	rc.Calls(meeting(ddapi.Race{ID: "r1", State: "pending"})) // baseline
	if rc.TakeStarted() {
		t.Fatal("a pending heat has not started")
	}

	calls := rc.Calls(meeting(ddapi.Race{ID: "r1", State: "running"}))
	if len(calls) != 0 {
		t.Errorf("nothing should be SAID when RaceStart has no wording, got %v", calls)
	}
	if !rc.TakeStarted() {
		t.Error("the heat going green must be noticed even when nothing is said about it")
	}
	if rc.TakeStarted() {
		t.Error("TakeStarted must clear the flag")
	}
}
