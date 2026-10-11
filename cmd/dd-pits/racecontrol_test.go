package main

import (
	"context"
	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/tts"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const controlSounds = `<?xml version="1.0" encoding="utf-8"?>
<ArrayOfSound>
  <Sound><Key>HurryUp</Key><TextToSpeech>Hurry Up {pilot}</TextToSpeech><Enabled>true</Enabled></Sound>
  <Sound><Key>InTheHole</Key><TextToSpeech>In the hole {pilots}</TextToSpeech><Enabled>false</Enabled></Sound>
</ArrayOfSound>`

func announcerForTest(t *testing.T) *raceAnnouncer {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Sounds.xml")
	if err := os.WriteFile(p, []byte(controlSounds), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSoundSet(p)
	if err != nil {
		t.Fatal(err)
	}
	return newRaceAnnouncer(s, newBoothMixer(), nil)
}

// The page must offer the calls that cannot be detected from data — "Hurry Up"
// is a keyboard shortcut in FPVTrackside, so nothing dd-pits polls will ever
// reveal that the director wants it.
func TestControlPageOffersManualCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	announcerForTest(t).serveRaceControl(rec, httptest.NewRequest(http.MethodGet, "/calls", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "Hurry Up") {
		t.Fatal("the page must offer Hurry Up")
	}
	if !strings.Contains(body, `name="pilot"`) {
		t.Fatal("a per-pilot call needs somewhere to name the pilot")
	}
}

// A sound switched off in FPVTrackside stays off here. This page is a remote
// for their settings, not a way around them.
func TestControlPageDisablesSwitchedOffCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	announcerForTest(t).serveRaceControl(rec, httptest.NewRequest(http.MethodGet, "/calls", nil))

	if !strings.Contains(rec.Body.String(), "Switched off in FPVTrackside") {
		t.Fatal("a disabled call must be shown as unavailable")
	}
}

// Posting a disabled call must say nothing at all.
func TestPostingADisabledCallSaysNothing(t *testing.T) {
	ra := announcerForTest(t)

	form := url.Values{"key": {"InTheHole"}, "pilot": {"bob9"}}
	req := httptest.NewRequest(http.MethodPost, "/calls", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ra.serveRaceControl(httptest.NewRecorder(), req)

	if n := ra.mixer.pending(); n != 0 {
		t.Fatalf("a disabled call must not be queued, got %d cues", n)
	}
}

// Blank pilot means everyone — the same word FPVTrackside itself passes.
func TestBlankPilotMeansEveryone(t *testing.T) {
	tpl := "Hurry Up {pilot}"
	if got := Fill(tpl, map[string]string{"pilot": "everyone"}); got != "Hurry Up everyone" {
		t.Fatalf("got %q", got)
	}
}

// TestRestartClearsTheBusEvenWithNothingToSay covers the ordering that made the
// restart clear a no-op.
//
// A restart clears the heat's laps, so the view it produces has NOTHING to
// say — no lap times left to call, and no RaceStart when the state never left
// "running". Observe returned early on that empty call list, so the check that
// clears the bus was never reached, and the abandoned run's queued lap times
// aired over the new run.
// silentEngine renders nothing, which is enough: this test is about what the
// bus DROPS, not about audio.
type silentEngine struct{}

func (silentEngine) Provider() string                    { return "test" }
func (silentEngine) SetUsageHook(func(string, int, int)) {}
func (silentEngine) Speak(context.Context, tts.Request) ([]byte, int, error) {
	return make([]byte, boothFrameBytes), boothSampleRate, nil
}

func TestRestartClearsTheBusEvenWithNothingToSay(t *testing.T) {
	ra := announcerForTest(t)
	ra.engine = silentEngine{} // Observe returns immediately without one

	mv := ddapi.Meeting{Races: []ddapi.Race{{ID: "r1", State: "running",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{11.4, 11.6}, Position: 1}}}}}
	ra.Observe(mv) // baseline

	// The previous run's calls are still queued.
	ra.mixer.enqueue(&boothCue{Text: "bob9 11.60", callKey: SoundRaceLap,
		priority: callPriorityLap, Rate: boothSampleRate,
		PCM: make([]byte, boothFrameBytes*4)})
	if n := ra.mixer.pending(); n == 0 {
		t.Fatal("expected the old run's call to be queued")
	}

	// The heat is re-run: laps cleared, state unchanged, so no calls result.
	mv.Races[0].Pilots[0].Laps = nil
	ra.Observe(mv)

	if n := ra.mixer.pending(); n != 0 {
		t.Errorf("a restart must clear the queue; %d cue(s) from the abandoned run survived", n)
	}
}

// TestPressingStartCutsCommentaryNotJustTheBuildUp covers the seven seconds of
// commentary heard across a countdown.
//
// Everything that cleared the bus hung off the GREEN LIGHT, but the operator
// presses Start several seconds earlier and the countdown plays in between.
// clearBuildUp ran at the press and ducked only build-up chatter, so commentary
// talked straight through the countdown — measured live at seven seconds
// between "start sequence" and "heat green". The green-light handling was
// correct all along and simply ran too late to matter.
func TestPressingStartCutsCommentaryNotJustTheBuildUp(t *testing.T) {
	ra := announcerForTest(t)

	commentary := &boothCue{Text: "and Willman leads the standings", commentary: true,
		Rate: boothSampleRate, PCM: make([]byte, boothFrameBytes*40)}
	channels := &boothCue{Text: "Wizz on R7", callKey: SoundAnnouncePilotChannel,
		Rate: boothSampleRate, PCM: make([]byte, boothFrameBytes*4)}
	ra.mixer.enqueue(commentary)
	ra.mixer.enqueue(channels)

	ra.clearBuildUp()

	if n := ra.mixer.pending(); n != 0 {
		t.Errorf("pressing Start must clear commentary AND build-up; %d cue(s) left", n)
	}
}

// TestPressingStartClearsRaceCallsToo covers "Wizz finished in 4th" being heard
// after the Start button had been pressed.
//
// The bus-clear at the press spared RACE CALLS, on the reasoning that a result
// is worth hearing. But once Start is pressed those results describe the heat
// BEFORE this one, and airing them over a field that is arming is worse than
// dropping them.
func TestPressingStartClearsRaceCallsToo(t *testing.T) {
	ra := announcerForTest(t)

	// Left over from the heat that just finished.
	ra.mixer.enqueue(&boothCue{Text: "Wizz finished in 4th", callKey: SoundRaceDone,
		priority: callPriorityLap, Rate: boothSampleRate, PCM: make([]byte, boothFrameBytes*8)})
	ra.mixer.enqueue(&boothCue{Text: "Race over", callKey: SoundRaceOver,
		priority: callPriorityRaceState, Rate: boothSampleRate, PCM: make([]byte, boothFrameBytes*4)})
	ra.mixer.enqueue(&boothCue{Text: "and Willman leads", commentary: true,
		Rate: boothSampleRate, PCM: make([]byte, boothFrameBytes*20)})

	before := ra.currentGen()
	ra.clearBuildUp()

	if n := ra.mixer.pending(); n != 0 {
		t.Errorf("pressing Start must clear the whole bus; %d cue(s) survived", n)
	}
	// Work already rendering has to be thrown away when it lands, which is what
	// advancing the generation does.
	if ra.currentGen() == before {
		t.Error("pressing Start must advance the generation so in-flight renders are discarded")
	}
}
