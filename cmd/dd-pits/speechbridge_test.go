package main

import (
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
)

func timeAgo(d time.Duration) time.Time { return time.Now().Add(-d) }

// bridgeFor builds a bridge over FPVTrackside's REAL templates, verbatim from a
// venue Sounds.xml. Invented shapes would pass while the real ones failed.
func bridgeFor(t *testing.T) *spokenBridge {
	t.Helper()
	tmpl := map[SoundKey]string{
		SoundRaceLap:             "{pilot} lap {lapnumber} in {laptime} in {position}",
		SoundRaceDone:            "{pilot} finished in {position}",
		SoundRaceStart:           "Go",
		SoundRaceOver:            "Race over",
		SoundHurryUp:             "Hurry up {pilot}",
		SoundInTheHole:           "In the hole {pilots}",
		SoundStartRaceIn:         "Arm your quads. Starting on the tone in less than {time}",
		SoundRaceAnnounce:        "Next up Round {round} {type} {race} {bracket}. ",
		SoundTimeTrialTargetLaps: "{pilot} {count} lap{s} in {lapstime}",
		SoundCasualLap:           "{pilot} {count} lap{s} in {lapstime}",
		"Holeshot":               "Holeshot {pilot} {laptime}",
	}

	set := &SoundSet{byKey: map[SoundKey]Sound{}, loaded: true}
	for k, v := range tmpl {
		set.byKey[k] = Sound{Key: k, Text: v, Enabled: true}
	}
	return newSpokenBridge(set)
}

func TestBridgeDropsCallsDDPitsDerives(t *testing.T) {
	b := bridgeFor(t)

	// dd-pits builds these from the timing data, so FPVTrackside's logged
	// version of the same call must not also be spoken.
	for _, line := range []string{
		"'bob9' lap 3 in 2.41 in 1",
		"'Wizz' lap 12 in 18.07 in 3",
		"'bob9' finished in 1",
		"Race over",
	} {
		if b.ShouldRelay(line) {
			t.Errorf("dd-pits derives this itself, relaying it would say it twice: %q", line)
		}
	}
}

func TestBridgeRelaysWhatDDPitsCannotSee(t *testing.T) {
	b := bridgeFor(t)

	// A keypress inside FPVTrackside is invisible from outside it — these are
	// the whole reason for watching the log.
	for _, line := range []string{
		"Hurry up 'bob9'",
		"In the hole 'bob9' 'Wizz'",
		"Next up Round 12 Race 2 . ",
		"Arm your quads. Starting on the tone in less than 5",
		"Holeshot 'bob9' 2.08",
	} {
		if !b.ShouldRelay(line) {
			t.Errorf("nothing else will say this, it must be relayed: %q", line)
		}
	}
}

func TestBridgeSurvivesCustomWording(t *testing.T) {
	b := bridgeFor(t)

	// The POINT of deriving a call is to say more than FPVTrackside would, so
	// the suppression keys on which sound the LOGGED line is — not on the words
	// dd-pits chose for its own version.
	key, ok := b.Identify("'bob9' lap 3 in 2.41 in 1")
	if !ok || key != SoundRaceLap {
		t.Fatalf("identify = %q, %v; want RaceLap", key, ok)
	}
	if b.ShouldRelay("'bob9' lap 3 in 2.41 in 1") {
		t.Fatal("the logged lap must be dropped however dd-pits worded its own")
	}
}

func TestBridgeSuppressesAnythingDDPitsJustSaid(t *testing.T) {
	b := bridgeFor(t)

	// The backstop, and the reason it is not limited to ambiguous lines: it
	// answers directly what the template match only approximates, so it also
	// covers a call racecalls.go emits without declaring in derivedSounds.
	//
	// CasualLap and TimeTrialTargetLaps happen to be literally the same
	// template, which is the case no text matcher could ever separate.
	line := "'bob9' 3 laps in 7.24"

	if !b.ShouldRelay(line) {
		t.Fatal("with nothing said, an ambiguous line must be relayed rather than lost")
	}

	// Now dd-pits says its own version, with colour FPVTrackside would not add.
	b.NoteSpoken("'bob9' 3 laps in 7.24, and that moves him up to second")

	if b.ShouldRelay(line) {
		t.Fatal("dd-pits just said this; relaying it would say the same lap twice")
	}
}

func TestBridgeForgetsOldCalls(t *testing.T) {
	b := bridgeFor(t)
	b.NoteSpoken("'bob9' 3 laps in 7.24")

	// Wind the record back past the window: the same figures later in a meeting
	// are a NEW call, not a duplicate of the old one.
	b.said[figureSignature("'bob9' 3 laps in 7.24")] = timeAgo(saidWindow * 2)

	if !b.ShouldRelay("'bob9' 3 laps in 7.24") {
		t.Fatal("a call from minutes ago must not suppress the same figures now")
	}
}

func TestBridgeRelaysUnrecognisedLines(t *testing.T) {
	b := bridgeFor(t)

	// The safe direction: a sound nobody models going unsaid is a silent race
	// day, whereas a duplicate is merely annoying and obvious enough to fix.
	if !b.ShouldRelay("Something FPVTrackside added in a later version") {
		t.Fatal("an unrecognised line must still be spoken")
	}
}

func TestBridgeIgnoresTemplatesOfPurePlaceholders(t *testing.T) {
	// PracticeLap is "{pilot} {laptime}" — no words of its own, so it matches
	// every line and can only mis-identify.
	if _, ok := buildSoundMatcher(SoundPracticeLap, "{pilot} {laptime}"); ok {
		t.Fatal("a template of pure placeholders must not become a matcher")
	}
}

// TestDerivedSoundsMatchesTheCaller holds derivedSounds and racecalls.go
// together.
//
// The two are edited independently and the failure is silent in both
// directions: a sound the caller emits but does not declare gets announced
// twice, and one declared but never emitted is dropped from the log and never
// spoken at all. Neither shows up until a race day.
//
// So this drives the real caller through a meeting and checks what actually
// comes out, rather than trusting the list.
func TestDerivedSoundsMatchesTheCaller(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{}, loaded: true}
	for k, v := range map[SoundKey]string{
		SoundRaceLap:             "{pilot} lap {lapnumber} in {laptime} in {position}",
		SoundTimeTrialEveryLap:   "{pilot} {laptime}",
		SoundRaceDone:            "{pilot} finished in {position}",
		SoundRaceStart:           "Go",
		SoundRaceOver:            "Race over",
		SoundNewLapRecord:        "{pilot} new record {count} lap{s} in {lapstime}",
		SoundTimeTrialTargetLaps: "{pilot} {count} lap{s} in {lapstime}",
		SoundTimeTrialDone:       "{pilot} finished {count} lap{s} in {lapstime}, Please land",
		SoundHurryUp:             "Hurry up {pilot}",
	} {
		set.byKey[k] = Sound{Key: k, Text: v, Enabled: true}
	}

	declared := map[SoundKey]bool{}
	for _, k := range derivedSounds {
		declared[k] = true
	}

	// A meeting playing through: a heat starts, laps land, times improve, it
	// ends. Run for a race and a time trial, since the caller picks a different
	// lap sound for each.
	for _, tt := range []bool{false, true} {
		rc := NewRaceCaller(set)

		mv := func(state string, laps []float64) ddapi.Meeting {
			r := ddapi.Race{
				ID: "r1", Round: 1, Number: 1, State: state, IsTT: tt,
				TypeLabel:  map[bool]string{true: "Time Trial", false: "Race"}[tt],
				TargetLaps: 3,
				Pilots: []ddapi.Pilot{{
					Name: "bob9", Laps: laps, Position: 1,
					Total: 9.0, BestLap: 2.4, CountedLaps: len(laps),
				}},
			}
			return ddapi.Meeting{MeetingID: "m1", Races: []ddapi.Race{r}, Current: &r}
		}

		rc.Calls(mv("pending", nil)) // prime
		emitted := map[SoundKey]bool{}
		for _, v := range []ddapi.Meeting{
			mv("running", nil),
			mv("running", []float64{3.10}),
			mv("running", []float64{3.10, 2.40}), // faster: a record
			mv("running", []float64{3.10, 2.40, 2.90}),
			mv("ended", []float64{3.10, 2.40, 2.90}),
		} {
			for _, c := range rc.Calls(v) {
				emitted[c.Key] = true
			}
		}

		for key := range emitted {
			if !declared[key] {
				t.Errorf("racecalls emits %s but derivedSounds does not declare it — "+
					"FPVTrackside's logged version would be relayed too and the call said twice", key)
			}
		}
	}
}

// TestSupersededCallsAreNotRelayedTwice covers a doubling heard on air.
//
// On a time trial every crossing produced TWO calls: dd-pits derives
// TimeTrialEveryLap ("Fizz 15.08") and FPVTrackside announces
// TimeTrialTargetLaps ("Fizz 4 laps in 61.20") for the same lap. Both are true;
// together they are twice the audio the bus can carry, and the measured effect
// was calls queueing from 151ms to 4384ms and then being dropped as stale.
//
// The bridge's usual test is "same sound?", which cannot see this — the two are
// different sounds describing the same moment.
func TestSupersededCallsAreNotRelayedTwice(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundTimeTrialEveryLap:   {Key: SoundTimeTrialEveryLap, Text: "{pilot} {laptime}", Enabled: true},
		SoundTimeTrialTargetLaps: {Key: SoundTimeTrialTargetLaps, Text: "{pilot} {count} lap{s} in {lapstime}", Enabled: true},
		SoundHurryUp:             {Key: SoundHurryUp, Text: "Hurry up {pilot}", Enabled: true},
	}, loaded: true}
	b := newSpokenBridge(set)

	if b.ShouldRelay("Fizz 4 laps in 61.20") {
		t.Error("TimeTrialTargetLaps is superseded by the derived per-lap call — relaying it doubles the audio")
	}
	// Unrelated relayed calls are untouched.
	if !b.ShouldRelay("Hurry up bob9") {
		t.Error("an unrelated relayed call must still be spoken")
	}
}

// TestSupersedingOnlyAppliesWhenTheReplacementIsOn.
//
// If the derived call is switched off in FPVTrackside, dd-pits will not make
// it — so dropping the relayed one too would leave the crossing silent, which
// is the failure this whole area keeps producing.
func TestSupersedingOnlyAppliesWhenTheReplacementIsOn(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundTimeTrialEveryLap:   {Key: SoundTimeTrialEveryLap, Text: "{pilot} {laptime}", Enabled: false},
		SoundTimeTrialTargetLaps: {Key: SoundTimeTrialTargetLaps, Text: "{pilot} {count} lap{s} in {lapstime}", Enabled: true},
	}, loaded: true}
	b := newSpokenBridge(set)

	if !b.ShouldRelay("Fizz 4 laps in 61.20") {
		t.Error("with the derived call switched off, the relayed one is all there is")
	}
}

// TestFinishingIsAnnouncedOncePerPilotPerHeat covers a flood measured live.
//
// FPVTrackside re-announces "bob9 finished 4 laps in …, Please land" on EVERY
// crossing after a pilot passes the target laps. In a time trial where pilots
// keep flying that produced eleven such calls in ninety seconds, saturating the
// bus and pushing real lap calls past their freshness window until they were
// dropped — so the flood did not merely add noise, it silenced the racing.
func TestFinishingIsAnnouncedOncePerPilotPerHeat(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundTimeTrialDone: {Key: SoundTimeTrialDone, Text: "{pilot} finished {count} lap{s} in {lapstime}, Please land", Enabled: true},
	}, loaded: true}
	b := newSpokenBridge(set)

	if !b.ShouldRelay("bob9 finished 4 laps in 48.21, Please land") {
		t.Fatal("the first finishing call must be spoken")
	}
	// The cumulative time keeps changing as they fly on, so a figure-based
	// dedupe cannot catch this — it has to key on the pilot.
	if b.ShouldRelay("bob9 finished 4 laps in 61.02, Please land") {
		t.Error("bob9 was already announced as finished this heat")
	}
	// A different pilot is a different call.
	if !b.ShouldRelay("Fizz finished 4 laps in 49.87, Please land") {
		t.Error("Fizz has not been announced yet")
	}
	// And the next heat starts fresh.
	b.NewHeat()
	if !b.ShouldRelay("bob9 finished 4 laps in 47.55, Please land") {
		t.Error("finishing the NEXT heat is a new call, not a repeat")
	}
}

// TestTrailingZeroDoesNotDefeatTheDedupe covers a duplicate heard on air.
//
// TimeTrialEveryLap is "{pilot} {laptime}" — nothing but placeholders — so it
// cannot be identified by pattern (such a pattern would match almost any line),
// and suppressing its relayed twin falls entirely to the already-said check.
// dd-pits writes the time as "3.30" and FPVTrackside as "3.3", so a textual
// comparison saw two different events and said the lap twice.
//
// The cost was not only the repetition: the duplicate takes a full generation
// and the next real lap call queues behind it. Measured live at 185ms for the
// derived call, 2800ms for the duplicate, 3533ms for the lap after it.
func TestTrailingZeroDoesNotDefeatTheDedupe(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundTimeTrialEveryLap: {Key: SoundTimeTrialEveryLap, Text: "{pilot} {laptime}", Enabled: true},
	}, loaded: true}
	b := newSpokenBridge(set)

	b.NoteSpoken("Willman 3.30") // as dd-pits derived it
	if b.ShouldRelay("Willman 3.3") {
		t.Error("FPVTrackside's 3.3 is the same lap dd-pits already called as 3.30")
	}

	// And the other way round, since either side may be the one to drop a zero.
	b.NoteSpoken("bob9 12.0")
	if b.ShouldRelay("bob9 12") {
		t.Error("12.0 and 12 are the same figure")
	}

	// A genuinely different lap is still relayed.
	if !b.ShouldRelay("Fizz 3.31") {
		t.Error("3.31 is a different lap from 3.30 and must not be swallowed")
	}
}

// TestChannelAnnouncementsAreNotConfusedForEachOther covers a pilot silently
// never having his channel announced.
//
// The dedupe reduced a call to its FIGURES, so "Fizz on R2" and "Willman on F2"
// both became "2" and the second was swallowed as a repeat of the first. Heard
// as only some of the field being called before a heat.
func TestChannelAnnouncementsAreNotConfusedForEachOther(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundAnnouncePilotChannel: {Key: SoundAnnouncePilotChannel,
			Text: "{pilot} on {channel}", Enabled: true},
	}, loaded: true}
	b := newSpokenBridge(set)

	for _, line := range []string{"bob9 on R1", "Fizz on R2", "Willman on F2", "Wizz on R7"} {
		if !b.ShouldRelay(line) {
			t.Errorf("every pilot must be announced; %q was swallowed", line)
		}
		b.NoteSpoken(line)
	}

	// The case the dedupe exists for still works: the same pilot's same lap,
	// worded differently by dd-pits and FPVTrackside, is still one event.
	b.NoteSpoken("Willman 3.30")
	if b.ShouldRelay("Willman 3.3") {
		t.Error("the same pilot's same lap must still dedupe across wordings")
	}
	// And a DIFFERENT pilot with the same figure is not a repeat.
	if !b.ShouldRelay("Fizz 3.3") {
		t.Error("a different pilot's identical time is a different call")
	}
}

// A relayed line that BEAT the poll must silence the derived twin.
//
// Dedup only ran derived-first: NoteSpoken suppressed the logged twin, but a
// relayed lap that aired first claimed nothing, and the poll said it again two
// seconds later — heard live as "MirellaFPV 1.97" twice. FPVTrackside logs its
// call at the moment of detection while the poll derives from a snapshot, so
// on any line the template matcher cannot identify the relay WINS that race.
// Both paths now mark what they air, and both check before speaking.
func TestRelayedTwinSilencesTheDerivedCall(t *testing.T) {
	b := newSpokenBridge(&SoundSet{})

	// The relay airs FPVTrackside's phrasing and marks it.
	b.NoteSpoken("MirellaFPV 1.97")

	// The derived call words it differently (lap-quality punctuation), but it
	// is the same pilot and the same figures — the same event.
	if !b.recentlySaid("MirellaFPV 1.97...") {
		t.Fatal("the derived twin of a relayed call must be recognised as already said")
	}

	// A DIFFERENT pilot clocking the same time is a different event — the
	// first word rides with the figures precisely so this is never swallowed.
	if b.recentlySaid("Bola 1.97...") {
		t.Fatal("another pilot's identical time must not be suppressed")
	}

	// And the relay's own re-log of the marked line stays suppressed, which is
	// what ShouldRelay's final check has always leaned on.
	if b.ShouldRelay("MirellaFPV 1.97") {
		t.Fatal("a marked line must not relay again inside the window")
	}
}
