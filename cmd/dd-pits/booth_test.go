package main

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/qwentts"

	"github.com/bob9/dd-pits/internal/ddapi"
)

func TestPilotColour(t *testing.T) {
	b := &booth{brackets: &boothBracketContext{Pilots: map[string]string{
		"DEMON": "from Brisbane; flying since 2019; sponsors AcmeFPV",
	}}}
	race := ddapi.Race{Pilots: []ddapi.Pilot{{Name: "DEMON"}, {Name: "NOPROFILE"}}}
	got := b.pilotColour(race)
	if !strings.Contains(got, "DEMON: from Brisbane") || !strings.Contains(got, "PILOT NOTES") {
		t.Fatalf("pilotColour missing the linked pilot's blurb: %q", got)
	}
	if strings.Contains(got, "NOPROFILE") {
		t.Fatal("a pilot with no blurb should not appear")
	}
	// No context / no blurbs → empty (no section injected).
	if (&booth{}).pilotColour(race) != "" {
		t.Fatal("pilotColour should be empty with no bracket context")
	}
	if b.pilotColour(ddapi.Race{Pilots: []ddapi.Pilot{{Name: "NOBODY"}}}) != "" {
		t.Fatal("pilotColour should be empty when no heat pilot has a blurb")
	}
}

func TestParseBoothLines(t *testing.T) {
	raw := "```\nAce: They're away!\nnarration that should be dropped\ntorque: Watch the inside line.\nACE: Second lap now.\nAce: fourth line beyond the cap\n```"
	lines := boothpkg.ParseLines(raw)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (cap + junk dropped), got %d: %v", len(lines), lines)
	}
	if lines[0] != "Ace: They're away!" {
		t.Fatalf("line 0: %q", lines[0])
	}
	// Speaker casing canonicalised for the TTS speaker match.
	if lines[1] != "Torque: Watch the inside line." {
		t.Fatalf("line 1 should canonicalise the speaker: %q", lines[1])
	}
	if lines[2] != "Ace: Second lap now." {
		t.Fatalf("line 2: %q", lines[2])
	}
	if got := boothpkg.ParseLines("no dialogue here at all"); got != nil {
		t.Fatalf("junk-only response should yield nothing: %v", got)
	}
}

func TestBoothQueuePriorities(t *testing.T) {
	b := newBooth(nil)
	b.mu.Lock()
	b.queueLocked(boothBeat{kind: "filler", priority: 1})
	b.queueLocked(boothBeat{kind: "progress", priority: 2})
	b.queueLocked(boothBeat{kind: "launch", priority: 4})
	b.queueLocked(boothBeat{kind: "finish", priority: 5})
	b.mu.Unlock()
	if len(b.beats) > boothQueueCap {
		t.Fatalf("queue exceeded cap: %d", len(b.beats))
	}
	beat, ok := b.popBeat()
	if !ok || beat.kind != "finish" {
		t.Fatalf("highest-priority beat should pop first, got %+v", beat)
	}
	beat, _ = b.popBeat()
	if beat.kind != "launch" {
		t.Fatalf("expected launch second, got %+v", beat)
	}
}

func TestMixerFramesAndSpokenAt(t *testing.T) {
	m := newBoothMixer()
	silence := make([]byte, boothFrameBytes)
	if f := m.nextFrame(silence); &f[0] != &silence[0] {
		t.Fatal("idle mixer should return the shared silence frame")
	}
	cue := &boothCue{PCM: make([]byte, boothFrameBytes+100), Rate: boothSampleRate}
	for i := range cue.PCM {
		cue.PCM[i] = 0x7f
	}
	m.enqueue(cue)
	f1 := m.nextFrame(silence)
	if f1[0] != 0x7f {
		t.Fatal("first frame should carry cue audio")
	}
	if cue.SpokenAt().IsZero() {
		t.Fatal("cue should be stamped spoken when playback starts")
	}
	f2 := m.nextFrame(silence)
	if f2[0] != 0x7f || f2[100] != 0 {
		t.Fatal("tail frame should carry the remainder zero-padded")
	}
	if f := m.nextFrame(silence); f[0] != 0 {
		t.Fatal("after the cue ends the bus returns to silence")
	}
	if m.busy() {
		t.Fatal("mixer should be idle after the cue finishes")
	}
}

func TestResamplePCM16(t *testing.T) {
	// 1 second of a constant value resampled 48k→24k halves the sample count.
	in := make([]byte, 48000*2)
	for i := 0; i < len(in); i += 2 {
		binary.LittleEndian.PutUint16(in[i:], uint16(int16(1000)))
	}
	out := resamplePCM16(in, 48000, 24000)
	if len(out) != 24000*2 {
		t.Fatalf("resample length: %d", len(out))
	}
	if v := int16(binary.LittleEndian.Uint16(out[100:])); v != 1000 {
		t.Fatalf("constant signal should survive resampling, got %d", v)
	}
	if got := resamplePCM16(in, 24000, 24000); &got[0] != &in[0] {
		t.Fatal("same-rate resample should be a no-op")
	}
}

func TestStreamingWAVHeader(t *testing.T) {
	h := streamingWAVHeader(boothSampleRate, 1, 16)
	if len(h) != 44 || string(h[0:4]) != "RIFF" || string(h[36:40]) != "data" {
		t.Fatal("bad streaming header")
	}
	if binary.LittleEndian.Uint32(h[40:44]) < 1<<30 {
		t.Fatal("data size should be near-maximal for an endless stream")
	}
}

func TestMixerInterrupt(t *testing.T) {
	m := newBoothMixer()
	meeting := &boothCue{PCM: make([]byte, boothFrameBytes*4), Rate: boothSampleRate} // RaceSrc ""
	raceCue := &boothCue{RaceSrc: "R1", PCM: make([]byte, boothFrameBytes), Rate: boothSampleRate}
	m.enqueue(meeting)
	m.enqueue(raceCue)
	silence := make([]byte, boothFrameBytes)
	m.nextFrame(silence) // meeting talk starts playing
	m.interrupt(func(c *boothCue) bool { return c.RaceSrc == "" })
	f := m.nextFrame(silence)
	// The meeting cue was cut mid-play; the race cue plays next (its PCM is
	// zeroes here, but cur must have advanced to it).
	m.mu.Lock()
	cur := m.cur
	m.mu.Unlock()
	if cur != raceCue && len(f) != boothFrameBytes {
		t.Fatalf("race cue should own the bus after interrupt")
	}
	if m.busy() && cur == meeting {
		t.Fatal("meeting talk survived the interrupt")
	}
}

func TestPilotTypicalLapSecs(t *testing.T) {
	mv := &meetingView{Races: []vmRace{
		{ID: "1", Pilots: []vmPilot{
			{Name: "Ace", Laps: []float64{20, 22}},
			{Name: "Bolt", Laps: []float64{30}},
		}},
		{ID: "2", Pilots: []vmPilot{
			{Name: "Ace", Laps: []float64{24, 0, 18}}, // a 0 (no time) is ignored
		}},
	}}
	// Averaged across BOTH heats: (20+22+24+18)/4 = 21.
	if got := pilotTypicalLapSecs(mv, "Ace"); got != 21 {
		t.Fatalf("Ace typical = %v, want 21 (whole-meeting history)", got)
	}
	// A pilot with no completed laps anywhere → 0 (can't judge, don't flag).
	if got := pilotTypicalLapSecs(mv, "Ghost"); got != 0 {
		t.Fatalf("unknown pilot = %v, want 0", got)
	}
}

func TestMixerPendingCountsWaitingCues(t *testing.T) {
	m := newBoothMixer()
	if m.pending() != 0 {
		t.Fatal("fresh mixer should have 0 pending")
	}
	m.enqueue(&boothCue{Text: "a", PCM: make([]byte, boothFrameBytes)})
	m.enqueue(&boothCue{Text: "b", PCM: make([]byte, boothFrameBytes)})
	if m.pending() != 2 {
		t.Fatalf("pending = %d, want 2", m.pending())
	}
}

// TestBoothChatterKinds pins the one list the green-light cut and the
// render-time drop both read. meeting_talk missing from the second list is
// what let bookend pre-race conversation play over a live race.
func TestBoothChatterKinds(t *testing.T) {
	for _, k := range []string{"meeting_open", "filler", "staged", "meeting_talk"} {
		if !boothChatterKind(k) {
			t.Errorf("%s must be chatter — it has to stop when a heat goes green", k)
		}
	}
	for _, k := range []string{"launch", "holeshot", "progress", "finish", "pilot_out", "pilot_back", "qual_move"} {
		if boothChatterKind(k) {
			t.Errorf("%s is a racing call — it must not be cut by the green light", k)
		}
	}
}

// TestBoothGreenLightCutsMeetingTalk: the lights go green mid-conversation.
// The queued and the on-air meeting talk both stop dead; the racing calls
// queued for that heat are untouched.
func TestBoothGreenLightCutsMeetingTalk(t *testing.T) {
	b := newBooth(nil)
	b.mu.Lock()
	// Bookend talk carries the heat's race id, so a RaceSrc-only rule misses it.
	b.queueLocked(boothBeat{kind: "meeting_talk", raceSrc: "race1", priority: 2})
	b.queueLocked(boothBeat{kind: "launch", raceSrc: "race1", priority: 4})
	// commentary:true mirrors production — the booth stamps it at its single
	// enqueue point. It matters: the cut used to key on "no RaceSrc", which is
	// also true of every RACE CALL, so it was cutting those off the bus too.
	onAir := &boothCue{RaceSrc: "race1", Kind: "meeting_talk", commentary: true,
		PCM: make([]byte, boothFrameBytes*4), Rate: boothSampleRate}
	b.mixer.enqueue(onAir)
	b.mu.Unlock()
	b.mixer.nextFrame(make([]byte, boothFrameBytes)) // it's talking

	b.mu.Lock()
	b.clearMeetingTalkLocked()
	kinds := []string{}
	for _, q := range b.beats {
		kinds = append(kinds, q.kind)
	}
	b.mu.Unlock()

	if len(kinds) != 1 || kinds[0] != "launch" {
		t.Fatalf("queue after green = %v, want [launch] only", kinds)
	}
	b.mixer.mu.Lock()
	cur := b.mixer.cur
	b.mixer.mu.Unlock()
	if cur == onAir {
		t.Fatal("meeting talk kept talking through the green light")
	}
}

// TestBoothGreenLightCutsEveryChatterKind: whatever the queue treats as
// chatter, the air does too.
//
// The two used to disagree. Eviction asked boothChatterKind; the cut asked
// "meeting-level, or meeting_talk" — which misses `staged`, because the line-up
// beat is about a heat and carries a RaceSrc. So the build-up was pulled from
// the queue and left playing, and the operator heard commentary continuing over
// the start sequence after pressing the button.
//
// Table-driven over the chatter kinds AND a racing call, because the property is
// that the two predicates agree — not that one particular kind is handled.
func TestBoothGreenLightCutsEveryChatterKind(t *testing.T) {
	for _, kind := range []string{"meeting_open", "filler", "staged", "meeting_talk", "launch"} {
		t.Run(kind, func(t *testing.T) {
			b := newBooth(nil)
			// A RaceSrc throughout: it is what made `staged` survive, and a
			// meeting-level rule cannot be what distinguishes these.
			onAir := &boothCue{RaceSrc: "race1", Kind: kind, commentary: true,
				PCM: make([]byte, boothFrameBytes*4), Rate: boothSampleRate}
			b.mixer.enqueue(onAir)
			b.mixer.nextFrame(make([]byte, boothFrameBytes)) // on air

			b.mu.Lock()
			b.clearMeetingTalkLocked()
			b.mu.Unlock()

			b.mixer.mu.Lock()
			stillOn := b.mixer.cur == onAir
			b.mixer.mu.Unlock()

			if want := boothChatterKind(kind); stillOn == want {
				t.Errorf("%s %s through the green light, but the queue %s it — "+
					"the two must agree on what chatter is",
					kind,
					map[bool]string{true: "kept talking", false: "was cut"}[stillOn],
					map[bool]string{true: "evicts", false: "keeps"}[want])
			}
		})
	}
}

// A race call is never cut by commentary, whatever its kind.
//
// `commentary` is the guard, stamped at the booth's single enqueue point. Race
// calls do not carry it. This is the other direction of the same bug: the old
// rule matched "no RaceSrc", which is true of every race call, and chopped
// "Next up Round 5" off the moment a post-race summary arrived.
func TestGreenLightNeverCutsARaceCall(t *testing.T) {
	b := newBooth(nil)
	call := &boothCue{Kind: "meeting_open" /*worst case: a chatter kind*/, commentary: false,
		PCM: make([]byte, boothFrameBytes*4), Rate: boothSampleRate}
	b.mixer.enqueue(call)
	b.mixer.nextFrame(make([]byte, boothFrameBytes))

	b.mu.Lock()
	b.clearMeetingTalkLocked()
	b.mu.Unlock()

	b.mixer.mu.Lock()
	defer b.mixer.mu.Unlock()
	if b.mixer.cur != call {
		t.Fatal("a race call was cut off the bus by the green light — commentary " +
			"queues behind calls or is dropped, never the other way round")
	}
}

// TestBoothSpeakDropsChatterWhileRacing: a chatter beat that reaches the
// generator while a heat is running is dropped before a single token is spent
// — and only chatter is. (gem is nil here, so anything NOT dropped errors,
// which is what tells the two apart.)
func TestBoothSpeakDropsChatterWhileRacing(t *testing.T) {
	b := newBooth(nil)
	b.mu.Lock()
	b.enabled, b.have = true, true
	b.snap = raceSnap(t, "race1", true, false, nil, nil) // green
	b.mu.Unlock()
	if !b.raceIsRunning() {
		t.Fatal("test snapshot should read as running")
	}

	for _, kind := range []string{"meeting_talk", "filler", "staged", "meeting_open"} {
		if err := b.speak(context.Background(), boothBeat{kind: kind, raceSrc: "race1"}, 0); err != nil {
			t.Fatalf("%s during a running heat should be dropped silently, got %v", kind, err)
		}
	}
	// A racing call still goes through to generation.
	if err := b.speak(context.Background(), boothBeat{kind: "progress", raceSrc: "race1"}, 0); err == nil {
		t.Fatal("progress must still be generated during a heat")
	}

	// Track quiet again → the conversation is allowed back.
	b.mu.Lock()
	b.snap = raceSnap(t, "race1", false, false, nil, nil)
	b.mu.Unlock()
	if err := b.speak(context.Background(), boothBeat{kind: "meeting_talk"}, 0); err == nil {
		t.Fatal("meeting talk must resume once the race is over")
	}
}

// TestBeatExtrasInHeatGags pins the opt-in: with the flag off the Stewards'
// Room only reaches the finish call (the pre-existing behaviour), and with it
// on a mid-heat call carries it too — but only ONCE per heat, however many
// progress beats re-detect the same anomaly.
func TestBeatExtrasInHeatGags(t *testing.T) {
	// SLOW has one lap way off their own pace: the scenic route, which is true
	// mid-heat as well as at the flag.
	race := ddapi.Race{
		ID: "r1", TypeLabel: "Race", ConsecWindow: 3,
		Pilots: []ddapi.Pilot{
			{Name: "SLOW", Laps: []float64{20, 55, 20}, Holeshot: 3, Position: 2},
			{Name: "FAST", Laps: []float64{20, 20, 20}, Holeshot: 2, Position: 1},
		},
	}
	newBooth := func(inHeat bool) *booth {
		return &booth{
			trigInHeat: inHeat,
			trigSaid:   map[string]bool{},
			pilotLaps:  map[string]int{},
			pilotLapAt: map[string]time.Time{},
			pilotOut:   map[string]bool{},
		}
	}

	// Off: mid-heat calls stay clean, the finish call still gets the gag.
	off := newBooth(false)
	if got := off.beatExtras("progress", &race); strings.Contains(got, "STEWARDS' ROOM") {
		t.Error("flag off: a mid-heat call must not carry the gags")
	}
	if got := off.beatExtras("finish", &race); !strings.Contains(got, "STEWARDS' ROOM") {
		t.Error("flag off: the finish call must still carry the gags")
	}

	// On: the mid-heat call gets it, worded as live, and only the first time.
	on := newBooth(true)
	first := on.beatExtras("progress", &race)
	if !strings.Contains(first, "STEWARDS' ROOM") || !strings.Contains(first, "SLOW") {
		t.Fatalf("flag on: mid-heat call missing the gag: %q", first)
	}
	if !strings.Contains(first, "LIVE ACTION comes first") {
		t.Error("a mid-heat gag must be worded as an aside to the racing")
	}
	if again := on.beatExtras("progress", &race); strings.Contains(again, "STEWARDS' ROOM") {
		t.Error("the same gag was offered twice in one heat — the booth would repeat itself")
	}
	if fin := on.beatExtras("finish", &race); strings.Contains(fin, "SLOW took one lap") {
		t.Error("the finish call repeated a gag that already aired mid-heat")
	}

	// A rerun is a fresh heat, so the gag is available again.
	on.resetHeatPilots("r1")
	if after := on.beatExtras("progress", &race); !strings.Contains(after, "STEWARDS' ROOM") {
		t.Error("a rerun must start with a clean slate of gags")
	}
}

// TestChatterClosesAtTheCOUNTDOWNNotTheGreenLight covers a cue heard on air:
// the booth announcing the next heat "on the grid" after Start had already been
// pressed.
//
// Every other guard keys on the race RUNNING, which is several seconds too
// late — the countdown plays first, and during it the timing data still reports
// the heat as pending. A staged cue rendering in that window passed every check.
func TestChatterClosesAtTheCOUNTDOWNNotTheGreenLight(t *testing.T) {
	b := &booth{mixer: newBoothMixer()}

	if b.chatterClosed() {
		t.Fatal("with no race and no countdown the booth is free to talk")
	}

	b.beginStartSequence()
	if !b.chatterClosed() {
		t.Fatal("the countdown must close chatter — the heat is not running yet, which is the whole bug")
	}

	// Re-opened when the heat is over, not when it goes green: the green light
	// is INSIDE the window the countdown opened.
	b.mu.Lock()
	b.preStart = false
	b.mu.Unlock()
	if b.chatterClosed() {
		t.Fatal("once the heat has ended the booth may talk again")
	}
}

// TestBeginStartSequenceIsIdempotent: the countdown can be seen more than once
// (derived and relayed paths both notice it), and it must not clear the queue
// repeatedly or log on every poll.
func TestBeginStartSequenceIsIdempotent(t *testing.T) {
	b := &booth{mixer: newBoothMixer()}
	b.beginStartSequence()
	b.beginStartSequence()
	if !b.chatterClosed() {
		t.Fatal("chatter must stay closed")
	}
}

// TestStaleFinishCueIsDroppedWhenANewHeatIsRunning covers commentary heard over
// the start of a race: a post-race wrap that was still generating when the next
// heat went green.
//
// The existing guard tested the beat's KIND, and "finish" is not a chatter
// kind — so it passed straight through. Kind was the wrong question: what makes
// a beat stale is that its race is over, whatever it is called.
func TestStaleFinishCueIsDroppedWhenANewHeatIsRunning(t *testing.T) {
	if boothChatterKind("finish") {
		t.Fatal("finish is not chatter — which is exactly why the kind test missed it")
	}
	// The kinds the old guard did cover, for contrast.
	for _, k := range []string{"meeting_open", "filler", "staged", "meeting_talk"} {
		if !boothChatterKind(k) {
			t.Errorf("%s should be chatter", k)
		}
	}
}

// A Qwen booth must never be handed a Gemini voice name.
//
// Observed live: a venue running Qwen with no Qwen voice configured fell back
// to boothVoicePBP — "Puck", a GEMINI voice — and Model Studio rejected every
// call with "Voice 'Puck' is not supported", so the booth was silent all
// meeting with only a log line to show for it. The fallback has to know which
// engine is speaking.
func TestQwenNeverGetsAGeminiVoiceName(t *testing.T) {
	// The two defaults are from different catalogues and must not be confused.
	if qwentts.ValidVoice(boothVoicePBP) {
		t.Fatalf("this test assumes %q is not a Qwen voice", boothVoicePBP)
	}

	// Unset, and a leftover Gemini name, both degrade to Qwen's own default.
	for _, in := range []string{"", boothVoicePBP, boothVoiceColor, "NotAVoice"} {
		if got := qwentts.ResolveVoice(in, qwentts.DefaultVoicePBP); !qwentts.ValidVoice(got) {
			t.Errorf("ResolveVoice(%q) = %q, which Qwen would reject", in, got)
		}
	}
	// A real Qwen voice is left alone...
	if got := qwentts.ResolveVoice("Cherry", qwentts.DefaultVoicePBP); got != "Cherry" {
		t.Errorf("a valid Qwen voice must survive, got %q", got)
	}
	// ...and so is a cloned voice, which is in no static catalogue.
	cloned := qwentts.ClonedVoicePrefix + "abc123"
	if got := qwentts.ResolveVoice(cloned, qwentts.DefaultVoicePBP); got != cloned {
		t.Errorf("a cloned voice must survive, got %q", got)
	}
}
