package main

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
)

func meetingWithRace(state string) ddapi.Meeting {
	r := ddapi.Race{ID: "r1", Round: 1, Number: 1, State: state, TargetLaps: 3}
	return ddapi.Meeting{MeetingID: "m1", Races: []ddapi.Race{r}, Current: &r}
}

func TestStaleCallsAreDroppedByPriority(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name     string
		call     RaceCall
		wantDrop bool
		why      string
	}{
		{
			name:     "fresh lap",
			call:     RaceCall{Priority: callPriorityLap, Made: now.Add(-2 * time.Second)},
			wantDrop: false,
			why:      "two seconds late is still the lap everyone just watched",
		},
		{
			name:     "backlogged lap",
			call:     RaceCall{Priority: callPriorityLap, Made: now.Add(-20 * time.Second)},
			wantDrop: true,
			why:      "the room has moved on; saying it now contradicts what they saw",
		},
		{
			name:     "late race-state",
			call:     RaceCall{Priority: callPriorityRaceState, Made: now.Add(-20 * time.Second)},
			wantDrop: false,
			why:      "a late 'Race over' is untidy; a dropped one leaves pilots flying",
		},
		{
			name:     "race-state from a previous heat",
			call:     RaceCall{Priority: callPriorityRaceState, Made: now.Add(-90 * time.Second)},
			wantDrop: true,
			why:      "this would land in the middle of the NEXT heat",
		},
		{
			name:     "unstamped",
			call:     RaceCall{Priority: callPriorityLap},
			wantDrop: false,
			why:      "the relay and the operator page build calls directly; those must never expire",
		},
	}

	for _, tc := range cases {
		if got := stale(tc.call, now); got != tc.wantDrop {
			t.Errorf("%s: stale = %v, want %v — %s", tc.name, got, tc.wantDrop, tc.why)
		}
	}
}

// TestRaceStateOutlivesLapCalls pins the ordering that matters: whatever the
// exact numbers, a cue people ACT on must always survive longer than a lap time.
func TestRaceStateOutlivesLapCalls(t *testing.T) {
	if freshnessFor(SoundRaceOver, callPriorityRaceState) <= freshnessFor(SoundRaceLap, callPriorityLap) {
		t.Fatal("a start/stop cue must outlive a lap time, or a backlog silences the one that matters")
	}
}

// TestCallsAreStampedWhenMade guards the central stamping: an unstamped call
// never expires, so a call site added later would silently opt out of all of
// this.
func TestCallsAreStampedWhenMade(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundRaceStart: {Key: SoundRaceStart, Text: "Go", Enabled: true},
	}, loaded: true}

	rc := NewRaceCaller(set)
	pending := meetingWithRace("pending")
	rc.Calls(pending) // prime

	calls := rc.Calls(meetingWithRace("running"))
	if len(calls) == 0 {
		t.Fatal("expected a race-start call")
	}
	for _, c := range calls {
		if c.Made.IsZero() {
			t.Errorf("call %s left unstamped — it would never expire", c.Key)
		}
	}
}

// TestYieldWhileRacingOnlyAppliesWhenSet pins the flag actually gating anything
// — a booth that yields unconditionally would never do mid-heat commentary, and
// one that never yields is the 12-second lap calls we started with.
func TestYieldWhileRacingOnlyAppliesWhenSet(t *testing.T) {
	b := &booth{phasePre: true, phaseIn: true, phasePost: true}
	if b.yieldsWhileRacing() {
		t.Fatal("a booth with no local writer must not hold beats — nothing is competing for the GPU")
	}

	b.yieldWhileRacing = true
	if !b.yieldsWhileRacing() {
		t.Fatal("a local booth must stand aside while a heat is running")
	}
}

// TestBuildUpIsDroppedWhenTheHeatGoesGreen covers the behaviour asked for on
// air: the moment a race starts, the build-up must stop — including a cue
// already playing — so the first lap call is not queued behind a channel
// announcement for a race that is already underway.
func TestBuildUpIsDroppedWhenTheHeatGoesGreen(t *testing.T) {
	preRace := []SoundKey{
		SoundAnnouncePilotChannel, SoundPilotsEnableVideo,
		SoundRaceAnnounce, SoundInTheHole, SoundStartRaceIn, SoundUntilRaceStart,
	}
	for _, k := range preRace {
		if !preRaceCall(k) {
			t.Errorf("%s is build-up and must be dropped when the light goes green", k)
		}
	}

	// The race itself, and everything that happens during it, must survive.
	keep := []SoundKey{
		SoundRaceStart, // "Go" IS the moment, not the build-up
		SoundRaceLap, SoundRaceDone, SoundRaceOver, SoundHoleshot,
		SoundHurryUp, // an operator pressed this deliberately, mid-race
	}
	for _, k := range keep {
		if preRaceCall(k) {
			t.Errorf("%s must never be dropped as build-up", k)
		}
	}
}

// TestMixerCutsOnlyTheBuildUp pins that the cut is selective: it takes the
// build-up off the bus, on air and queued, and leaves the race alone.
func TestMixerCutsOnlyTheBuildUp(t *testing.T) {
	m := newBoothMixer()
	m.cur = &boothCue{callKey: SoundAnnouncePilotChannel, Text: "bob9 on R1"}
	m.queue = []*boothCue{
		{callKey: SoundPilotsEnableVideo, Text: "turn on your video transmitters"},
		{callKey: SoundRaceLap, Text: "bob9 lap 1"},
		{Kind: "progress", Text: "some commentary"},
	}

	m.interrupt(func(c *boothCue) bool { return preRaceCall(c.callKey) })

	if m.cur != nil {
		t.Error("the build-up cue ON AIR must be cut, not allowed to finish")
	}
	if len(m.queue) != 2 {
		t.Fatalf("queue = %d cues, want 2 (the lap and the commentary)", len(m.queue))
	}
	for _, c := range m.queue {
		if preRaceCall(c.callKey) {
			t.Errorf("build-up left in the queue: %q", c.Text)
		}
	}
}

// TestLapCallsOutrankCommentaryInTheQueue pins the ordering that was asked for:
// lap times to the top, commentary below.
func TestLapCallsOutrankCommentaryInTheQueue(t *testing.T) {
	m := newBoothMixer()
	// Commentary queued FIRST, so arrival order alone would play it first.
	m.queue = []*boothCue{
		{Kind: "progress", Text: "commentary", priority: callPriorityCommentary, seq: 1},
		{callKey: SoundRaceLap, Text: "bob9 lap 1", priority: callPriorityLap, seq: 2},
	}

	i := m.pickNextLocked(timeAgo(0))
	if i < 0 || m.queue[i].Text != "bob9 lap 1" {
		t.Fatalf("picked %v, want the lap call — a lap must not wait behind colour", i)
	}
}

// TestCallsAreSpokenInTheOrderTheyWereMade covers the jumbling heard live:
// "bob9 first, Willman third, race over, Fizz second" — a finishing order read
// out in the wrong order with the finish called in the middle of it.
//
// The cause was dispatching each call on its own goroutine, which then raced
// for the single sidecar pipe, so the order they aired was whichever goroutine
// won rather than the order the calls were made.
func TestCallsAreSpokenInTheOrderTheyWereMade(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundRaceDone: {Key: SoundRaceDone, Text: "{pilot} finished in {position}", Enabled: true},
	}, loaded: true}

	ra := &raceAnnouncer{
		sounds:   set,
		caller:   NewRaceCaller(set),
		bridge:   newSpokenBridge(set),
		rendered: map[string][]byte{},
		pending:  make(chan RaceCall, 8),
	}

	want := []string{"bob9 first", "Fizz second", "Willman third", "Wizz fourth"}
	for _, txt := range want {
		ra.enqueue(RaceCall{Key: SoundRaceDone, Text: txt, Priority: callPriorityLap})
	}

	// The queue itself must preserve order — the worker drains it serially, so
	// what comes out of the channel is what goes to air.
	for i, expect := range want {
		got := <-ra.pending
		if got.Text != expect {
			t.Fatalf("position %d: got %q, want %q — results read out of order", i, got.Text, expect)
		}
	}
}

// TestAHopelessBacklogIsDroppedAtTheDoor pins the bound: when calls arrive far
// faster than they can be spoken, the NEW one is dropped rather than the queue
// growing without limit. The queue ahead is older and closer to the moment it
// describes.
func TestAHopelessBacklogIsDroppedAtTheDoor(t *testing.T) {
	ra := &raceAnnouncer{pending: make(chan RaceCall, 2)}
	for i := 0; i < 10; i++ {
		ra.enqueue(RaceCall{Key: SoundRaceLap, Text: "lap"})
	}
	if len(ra.pending) != 2 {
		t.Fatalf("queue holds %d, want it capped at 2", len(ra.pending))
	}
}

// TestDNFIsNeverAnnouncedAsAFinish covers a call heard on air:
// "Willman finished in 2nd" for a pilot who did not finish.
//
// FPVTrackside assigns a position to a DNF for ordering purposes, so reading it
// out states something untrue about the race everyone just watched. There is no
// DNF wording in the sound set, so silence is the honest option.
func TestDNFIsNeverAnnouncedAsAFinish(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundRaceDone: {Key: SoundRaceDone, Text: "{pilot} finished in {position}", Enabled: true},
	}, loaded: true}

	ended := func(pilots ...ddapi.Pilot) ddapi.Meeting {
		r := ddapi.Race{ID: "r1", Round: 1, Number: 1, State: "ended", TargetLaps: 3, Pilots: pilots}
		return ddapi.Meeting{MeetingID: "m1", Races: []ddapi.Race{r}, Current: &r}
	}

	rc := NewRaceCaller(set)
	rc.Calls(ended()) // prime

	calls := rc.Calls(ended(
		ddapi.Pilot{Name: "bob9", Position: 1, Laps: []float64{3.1, 3.0, 2.9}},
		ddapi.Pilot{Name: "Willman", Position: 2, DNF: true},
	))

	for _, c := range calls {
		if c.Key == SoundRaceDone && strings.Contains(c.Text, "Willman") {
			t.Errorf("announced a finishing position for a DNF: %q", c.Text)
		}
	}

	// The pilot who DID finish must still be called — the guard must not
	// silence real results.
	var announced bool
	for _, c := range calls {
		if c.Key == SoundRaceDone && strings.Contains(c.Text, "bob9") {
			announced = true
		}
	}
	if !announced {
		t.Error("a pilot who finished was not announced")
	}
}

// TestCountdownSupersedesTheBuildUp covers what was heard on air: "arm your
// quads, starting on the tone in less than 5" landing AFTER the tone.
//
// It was not late to be decided — it was queued behind ~10s of channel
// announcements for a start that comes about 3s later. The countdown must
// clear the queue ahead of it, and must not clear itself.
func TestCountdownSupersedesTheBuildUp(t *testing.T) {
	superseded := []SoundKey{
		SoundAnnouncePilotChannel, SoundPilotsEnableVideo,
		SoundRaceAnnounce, SoundInTheHole, SoundUntilRaceStart,
	}
	for _, k := range superseded {
		if !buildUpChatter(k) {
			t.Errorf("%s must be cleared when the start sequence begins", k)
		}
	}

	// The countdown is the start sequence, not the build-up to it.
	if buildUpChatter(SoundStartRaceIn) {
		t.Fatal("the countdown must never clear itself")
	}
	// Nor may it touch anything from the race.
	for _, k := range []SoundKey{SoundRaceStart, SoundRaceLap, SoundHoleshot, SoundRaceOver} {
		if buildUpChatter(k) {
			t.Errorf("%s is not build-up and must survive", k)
		}
	}
}

// TestDrainPendingClearsOnlySupersededCalls: the bus interrupt reaches cues
// already rendered, but a call still queued would be spoken afterwards and the
// build-up would simply resume after the countdown.
func TestDrainPendingClearsOnlySupersededCalls(t *testing.T) {
	ra := &raceAnnouncer{pending: make(chan RaceCall, 8)}
	ra.pending <- RaceCall{Key: SoundAnnouncePilotChannel, Text: "bob9 on R1"}
	ra.pending <- RaceCall{Key: SoundPilotsEnableVideo, Text: "video on"}
	ra.pending <- RaceCall{Key: SoundRaceLap, Text: "bob9 lap 1"}

	ra.drainPending(buildUpChatter)

	if len(ra.pending) != 1 {
		t.Fatalf("queue holds %d, want 1 (the lap call)", len(ra.pending))
	}
	if got := <-ra.pending; got.Key != SoundRaceLap {
		t.Errorf("kept %s, want the lap call — race calls must never be drained", got.Key)
	}
}

// TestClearingTheBuildUpNeverCutsLiveSpeech covers what went to air: "Pil—"
// instead of "Pilots turn on your video transmitters", because clearing the
// queue for the countdown also killed the cue that was mid-word.
//
// A truncated word sounds like a fault. Cutting a live cue is right when the
// RACE has started; it is wrong merely to get ahead in a queue.
func TestDuckingFadesRatherThanTruncates(t *testing.T) {
	// The interruption must be immediate — "arm your quads" has to reach pilots
	// before the tone — but stopping on the exact sample truncated a word and
	// went to air as "Pil—". A fade makes the same cut sound deliberate.
	m := newBoothMixer()
	loud := make([]byte, boothSampleRate) // 0.5s of samples
	for i := 0; i+1 < len(loud); i += 2 {
		loud[i], loud[i+1] = 0x00, 0x40 // ~16384
	}
	m.cur = &boothCue{callKey: SoundPilotsEnableVideo, PCM: loud}

	m.duck(func(c *boothCue) bool { return buildUpChatter(c.callKey) })

	if m.cur == nil {
		t.Fatal("ducking must fade the cue out, not delete it mid-sample")
	}
	if m.cur.fadeLeft <= 0 {
		t.Fatal("no fade was armed — the cue would still be truncated")
	}

	// Pump frames until it finishes; the tail must ramp down, not stop dead.
	silence := make([]byte, boothFrameBytes)
	var last int16 = 1 << 14
	for i := 0; i < 20 && m.cur != nil; i++ {
		f := m.nextFrame(silence)
		v := int16(binary.LittleEndian.Uint16(f[len(f)-2:]))
		if v > last {
			t.Fatalf("level rose during the fade (%d -> %d)", last, v)
		}
		last = v
	}
	if m.cur != nil {
		t.Error("the ducked cue never finished")
	}
	if last > 4000 {
		t.Errorf("cue ended at level %d — that is a truncation, not a fade", last)
	}
}

func TestClearingTheBuildUpNeverCutsLiveSpeech(t *testing.T) {
	m := newBoothMixer()
	speaking := &boothCue{callKey: SoundPilotsEnableVideo, Text: "Pilots turn on your video transmitters"}
	m.cur = speaking
	m.queue = []*boothCue{
		{callKey: SoundAnnouncePilotChannel, Text: "bob9 on R1"},
		{callKey: SoundAnnouncePilotChannel, Text: "Fizz on R2"},
		{callKey: SoundStartRaceIn, Text: "Arm your quads"},
		{callKey: SoundRaceLap, Text: "bob9 lap 1"},
	}

	m.dropQueued(func(c *boothCue) bool { return buildUpChatter(c.callKey) })

	if m.cur != speaking {
		t.Error("dropQueued must never touch the cue on air")
	}
	if len(m.queue) != 2 {
		t.Fatalf("queue holds %d, want 2 (countdown + lap)", len(m.queue))
	}
	for _, c := range m.queue {
		if buildUpChatter(c.callKey) {
			t.Errorf("build-up left queued: %q", c.Text)
		}
	}
}

// TestGoingGreenStillCutsLiveSpeech: the distinction is deliberate, so pin the
// other side of it. Once the race is under way, talking over it is worse than a
// clipped word.
func TestGoingGreenStillCutsLiveSpeech(t *testing.T) {
	m := newBoothMixer()
	m.cur = &boothCue{callKey: SoundAnnouncePilotChannel, Text: "Wizz on R7"}
	m.interrupt(func(c *boothCue) bool { return preRaceCall(c.callKey) })
	if m.cur != nil {
		t.Error("a build-up cue must be cut when the heat goes green")
	}
}

// TestGoingGreenCutsStaleCommentary: colour is written about the race that just
// finished, so once the next heat is green it describes something the field has
// moved past — worse than silence, because it actively misleads — and it is
// occupying the bus the new heat's calls need.
func TestGoingGreenCutsStaleCommentary(t *testing.T) {
	m := newBoothMixer()
	m.cur = &boothCue{Kind: "finish", Text: "and that wraps up a great heat"} // no callKey
	m.queue = []*boothCue{
		{Kind: "progress", Text: "more colour"},
		{callKey: SoundAnnouncePilotChannel, Text: "bob9 on R1"},
		{callKey: SoundRaceLap, Text: "bob9 lap 1"},
	}

	green := func(c *boothCue) bool { return preRaceCall(c.callKey) || c.callKey == "" }
	m.duck(green)

	if m.cur == nil || m.cur.fadeLeft == 0 {
		t.Error("commentary on air must be ducked when a heat goes green")
	}
	if len(m.queue) != 1 {
		t.Fatalf("queue holds %d, want 1 (the lap call)", len(m.queue))
	}
	if m.queue[0].callKey != SoundRaceLap {
		t.Errorf("kept %q — a race call must survive", m.queue[0].Text)
	}
}

// TestSpellCountdownOnlyTouchesTheCountdown.
//
// "in less than 5" reads like a form being filled in; "in less than FIVE" reads
// like someone calling a race start. But spelling numbers wherever they appear
// would wreck exactly the calls that matter most — a lap time must stay 11.42.
func TestSpellCountdownOnlyTouchesTheCountdown(t *testing.T) {
	got := spellCountdown("ARM YOUR QUADS! Starting on the tone in less than 5!")
	want := "ARM YOUR QUADS! Starting on the tone in less than FIVE!"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// Other countdown values still work — the template keeps its placeholder,
	// so a different start delay must not fall back to a digit.
	if got := spellCountdown("in less than 10"); got != "in less than TEN" {
		t.Errorf("10 -> %q", got)
	}

	// A decimal is a TIME and must survive untouched.
	for _, s := range []string{
		"bob9 lap 1 in 11.42 in 1st",
		"Holeshot bob9 2.08",
		"bob9 4 laps in 34.20",
	} {
		if got := spellCountdown(s); got != s {
			t.Errorf("a lap time was altered:\n got  %q\n want %q", got, s)
		}
	}
}

// TestCloudVoiceSpeaksOnlyWhatThePackHolds.
//
// A hosted render measured ~320ms to first audio warm, before network
// variance — fine for a wrap between heats, too slow for a lap time, which has
// to land on the lap it describes or it is a correction rather than a call.
//
// So on a cloud voice the announcer plays recordings and skips generation. The
// distinction is not "race calls vs commentary" but "already recorded vs has to
// be made now", which is why the flag lives on the announcer rather than being
// decided per beat.
func TestCloudVoiceSpeaksOnlyWhatThePackHolds(t *testing.T) {
	dir := defaultVoicePackDir("Profile 1")
	if dir == "" {
		t.Skip("no voice pack on this machine")
	}
	pack, err := loadVoicePack(dir)
	if err != nil {
		t.Skip(err)
	}

	// A fixed call the pack holds: still spoken, and instantly.
	if _, ok := pack.Render("Race over"); !ok {
		t.Skip("this pack lacks the fixed call the test relies on")
	}

	// A lap time is never in the pack — its numbers are new every lap — so on a
	// cloud voice it is the call that gets skipped.
	if _, ok := pack.Render("bob9 lap 7 in 9.13 in 2nd"); ok {
		t.Fatal("a novel lap call should not be in the pack; the premise of packOnly is wrong")
	}
}

// TestPackOnlyIsOffForLocalVoices: local generation is ~110ms to first audio,
// which lands well inside the gap between crossings — skipping there would
// throw away the calls that matter for no reason.
func TestPackOnlyIsOffForLocalVoices(t *testing.T) {
	ra := &raceAnnouncer{}
	if ra.packOnly {
		t.Fatal("packOnly must default off — a local voice is fast enough to generate lap calls")
	}
}

// TestBuildUpCallsOutlastLapCalls covers three of four pilots never being
// announced.
//
// FPVTrackside called "bob9 on R1", "Fizz on R2", "Willman on F2", "Wizz on R7"
// in four seconds; dd-pits spoke only the last. The booth was generating the
// post-race summary, the render queue backed up, and the first three passed the
// 8s LAP deadline they were sharing — so they were discarded as "overtaken by
// events" when nothing had overtaken them at all. A field on the line would
// rather hear all four late than one on time.
func TestBuildUpCallsOutlastLapCalls(t *testing.T) {
	// Both carry lap priority — which is exactly why keying the deadline on
	// priority alone was wrong.
	lap := freshnessFor(SoundRaceLap, callPriorityLap)
	chn := freshnessFor(SoundAnnouncePilotChannel, callPriorityLap)
	if chn <= lap {
		t.Errorf("a channel announcement (%s) must outlast a lap call (%s)", chn, lap)
	}

	made := time.Now().Add(-30 * time.Second)
	if stale(RaceCall{Key: SoundAnnouncePilotChannel, Priority: callPriorityLap, Made: made}, time.Now()) {
		t.Error("a channel announcement 30s behind is still worth saying")
	}
	if !stale(RaceCall{Key: SoundRaceLap, Priority: callPriorityLap, Made: made}, time.Now()) {
		t.Error("a lap time 30s behind is not — it describes a moment that has passed")
	}
}

// TestPrewarmCachesTheSpokenFormOfTheCountdown covers the countdown being
// regenerated on every race start.
//
// The relay speaks the countdown through spellCountdown ("less than 5" becomes
// "less than FIVE", which reads far better), but the prewarm rendered the
// template's digit form. The cache is keyed on the exact words, so the two
// never matched and the single line the operator is waiting to hear was
// generated from scratch each time — 108ms on the critical path.
func TestPrewarmCachesTheSpokenFormOfTheCountdown(t *testing.T) {
	template := "ARM YOUR QUADS! Starting on the tone in less than {time}!"
	rendered := Fill(template, map[string]string{"time": "5"})

	if spellCountdown(rendered) == rendered {
		t.Skip("countdown spelling is not in play for this wording")
	}
	// What prewarm stores must equal what the relay asks for.
	if spellCountdown(rendered) != spellCountdown(spellCountdown(rendered)) {
		t.Error("spellCountdown must be idempotent, or prewarm and the relay can still disagree")
	}
}
