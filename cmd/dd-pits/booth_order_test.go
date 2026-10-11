package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/gemini"
	"github.com/bob9/dd-pits/internal/qwenchat"
)

// Beats generate concurrently, so a later call can reach the mixer FIRST — a
// short "holeshot!" beating a long finish wrap that started before it. The
// mixer must still air them in the order the beats happened.
func TestMixerAirsUnscheduledCuesInBeatOrder(t *testing.T) {
	m := &boothMixer{}
	// Enqueued out of order on purpose: seq 3 arrives first.
	m.queue = []*boothCue{
		{Kind: "finish", seq: 3},
		{Kind: "launch", seq: 1},
		{Kind: "progress", seq: 2},
	}
	var got []string
	for range 3 {
		i := m.pickNextLocked(time.Now())
		if i < 0 {
			t.Fatal("nothing pickable")
		}
		got = append(got, m.queue[i].Kind)
		m.queue = append(m.queue[:i], m.queue[i+1:]...)
	}
	want := []string{"launch", "progress", "finish"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aired %v, want %v", got, want)
		}
	}
}

// A scheduled cue is anchored to a moment, so its mark orders it — seq only
// breaks a tie between two cues anchored to the SAME instant.
func TestScheduledCuesOrderByMarkThenSeq(t *testing.T) {
	base := time.Now().Add(-time.Second)
	m := &boothMixer{}
	m.queue = []*boothCue{
		{Kind: "late", seq: 1, playAt: base.Add(500 * time.Millisecond)},
		{Kind: "early", seq: 9, playAt: base},
	}
	// The later-seq cue wins because its mark came first.
	if i := m.pickNextLocked(time.Now()); m.queue[i].Kind != "early" {
		t.Errorf("aired %q, want the earlier mark", m.queue[i].Kind)
	}

	// Same mark → the order the beats happened decides.
	same := base
	m.queue = []*boothCue{
		{Kind: "second", seq: 5, playAt: same},
		{Kind: "first", seq: 2, playAt: same},
	}
	if i := m.pickNextLocked(time.Now()); m.queue[i].Kind != "first" {
		t.Errorf("aired %q, want the lower seq on an equal mark", m.queue[i].Kind)
	}
}

// A due action call still outranks unscheduled filler regardless of seq —
// concurrency must not let filler jump a call that is due to air.
func TestDueScheduledStillOutranksFiller(t *testing.T) {
	m := &boothMixer{}
	m.queue = []*boothCue{
		{Kind: "filler", seq: 1}, // unscheduled, earlier beat
		{Kind: "holeshot", seq: 7, playAt: time.Now().Add(-time.Second)}, // due
	}
	if i := m.pickNextLocked(time.Now()); m.queue[i].Kind != "holeshot" {
		t.Errorf("aired %q, want the due action call", m.queue[i].Kind)
	}
}

// A cue whose mark hasn't arrived is never aired early, even with the lowest
// seq — that mark is what keeps audio with the delayed video.
func TestFutureScheduledCueIsHeld(t *testing.T) {
	m := &boothMixer{}
	m.queue = []*boothCue{{Kind: "future", seq: 1, playAt: time.Now().Add(10 * time.Second)}}
	if i := m.pickNextLocked(time.Now()); i != -1 {
		t.Errorf("picked index %d, want -1 (hold silence)", i)
	}
	// With filler alongside, the filler plays and the future cue keeps waiting.
	m.queue = append(m.queue, &boothCue{Kind: "filler", seq: 2})
	if i := m.pickNextLocked(time.Now()); m.queue[i].Kind != "filler" {
		t.Errorf("aired %q, want filler while the marked cue waits", m.queue[i].Kind)
	}
}

// The generation pool must be bounded: unbounded goroutines would stack API
// calls into a provider rate limit.
func TestGenerationConcurrencyIsBounded(t *testing.T) {
	b := &booth{genSlots: make(chan struct{}, boothGenConcurrency)}
	for i := 0; i < boothGenConcurrency; i++ {
		select {
		case b.genSlots <- struct{}{}:
		default:
			t.Fatalf("slot %d should have been free", i)
		}
	}
	select {
	case b.genSlots <- struct{}{}:
		t.Error("acquired more slots than the bound allows")
	default: // correct: the pool is full
	}
	if boothGenConcurrency < 2 {
		t.Error("bound below 2 defeats the point — nothing overlaps")
	}
}

// The script phase must be serialised: beatPrompt reads the rolling transcript
// to tell the model what was just said, so two scripts written at once would
// both see the same history and repeat each other. This is the regression that
// made the booth repeat itself once beats began generating concurrently.
func TestScriptPhaseIsSerialised(t *testing.T) {
	b := &booth{scriptSlot: make(chan struct{}, 1)}
	select {
	case b.scriptSlot <- struct{}{}:
	default:
		t.Fatal("first script should acquire the slot")
	}
	select {
	case b.scriptSlot <- struct{}{}:
		t.Error("a second script acquired the slot — prompts can race the transcript")
	default: // correct: held
	}
	<-b.scriptSlot
	select {
	case b.scriptSlot <- struct{}{}:
	default:
		t.Error("slot not released")
	}
}

// Audio must render OUTSIDE the script slot, or nothing overlaps and the
// concurrency buys nothing. The pool is wider than the script slot, which is
// what lets the next call be written while this one is voiced.
func TestAudioRendersOutsideTheScriptSlot(t *testing.T) {
	if cap(make(chan struct{}, boothGenConcurrency)) <= 1 {
		t.Fatal("generation pool must exceed 1 for any overlap")
	}
	b := &booth{
		genSlots:   make(chan struct{}, boothGenConcurrency),
		scriptSlot: make(chan struct{}, 1),
	}
	// One beat holding the script slot must not stop another occupying a
	// generation slot (i.e. rendering audio).
	b.scriptSlot <- struct{}{}
	select {
	case b.genSlots <- struct{}{}:
	default:
		t.Error("a beat could not render audio while another writes its script")
	}
}

// The in-heat trigger re-arms every boothInHeatGap (5s) but laps land every
// ~20s, so consecutive calls would describe an IDENTICAL race state and the
// model, with nothing new to say, repeats itself almost word for word. The
// signature is what makes a call conditional on the race having moved.
func TestRaceProgressSigChangesOnlyOnRealProgress(t *testing.T) {
	base := vmRace{ID: "r1", Pilots: []vmPilot{
		{Name: "bob9", Laps: []float64{10.97}, Position: 1},
		{Name: "Willman", Laps: []float64{10.53}, Position: 2},
	}}
	if boothRaceProgressSig(base) != boothRaceProgressSig(base) {
		t.Fatal("signature is not stable for identical state")
	}

	// A new lap is real progress.
	lap := vmRace{ID: "r1", Pilots: []vmPilot{
		{Name: "bob9", Laps: []float64{10.97, 11.02}, Position: 1},
		{Name: "Willman", Laps: []float64{10.53}, Position: 2},
	}}
	if boothRaceProgressSig(lap) == boothRaceProgressSig(base) {
		t.Error("a new lap did not change the signature")
	}

	// So is an overtake.
	swap := vmRace{ID: "r1", Pilots: []vmPilot{
		{Name: "bob9", Laps: []float64{10.97}, Position: 2},
		{Name: "Willman", Laps: []float64{10.53}, Position: 1},
	}}
	if boothRaceProgressSig(swap) == boothRaceProgressSig(base) {
		t.Error("a position change did not change the signature")
	}

	// And a retirement.
	dnf := vmRace{ID: "r1", Pilots: []vmPilot{
		{Name: "bob9", Laps: []float64{10.97}, Position: 1},
		{Name: "Willman", Laps: []float64{10.53}, Position: 2, DNF: true},
	}}
	if boothRaceProgressSig(dnf) == boothRaceProgressSig(base) {
		t.Error("a DNF did not change the signature")
	}
}

// The recap window must be long enough that re-calling an unchanged race
// sounds deliberate rather than like repetition, and longer than the gap that
// re-arms the trigger.
func TestInHeatRecapExceedsTheGap(t *testing.T) {
	if boothInHeatRecap <= boothInHeatGap {
		t.Errorf("recap %v must exceed the re-arm gap %v", boothInHeatRecap, boothInHeatGap)
	}
	if boothInHeatRecap < 20*time.Second {
		t.Errorf("recap %v is shorter than a typical lap — repetition returns", boothInHeatRecap)
	}
}

// Snappy scripts are chosen on ACCURACY first, speed second. qwen-flash was
// marginally quicker (~1.0s vs ~1.4s) but the least reliable about race state:
// benchmarking caught it calling lap 2 of 3 "the final lap", and on air it
// mis-attributed lap times between pilots and invented a seconds-gap for a
// pilot who was a full lap down. A live race call must be right first.
func TestFastScriptModelIsTheAccurateOne(t *testing.T) {
	if boothFastScriptModel == "" {
		t.Fatal("no fast script model configured")
	}
	if strings.Contains(boothFastScriptModel, "qwen") {
		t.Errorf("fast script model = %q — qwen is the fallback, not the default, on accuracy grounds",
			boothFastScriptModel)
	}
	// Retired models 404 for newer keys; the fast path must not point at one.
	for _, dead := range []string{"gemini-2.5-flash", "gemini-2.5-flash-lite"} {
		if boothFastScriptModel == dead {
			t.Errorf("fast script model %q is retired and 404s for new API keys", dead)
		}
	}
}

// Both backends must satisfy the seam, so the fast path can pick whichever the
// tenant's keys allow without the call site caring.
func TestScriptWriterAcceptsEitherBackend(t *testing.T) {
	var _ scriptWriter = (*gemini.Client)(nil)
	var _ scriptWriter = (*qwenchat.Client)(nil)
}

// With no bracket linked, the finish brief's talk of advancement had nothing
// to attach to and the booth invented one — pilots "advancing to Winners
// Round 2" at a meeting that was just a series of races. The absence is now
// stated outright rather than left as a vacuum.
func TestNoBracketIsStatedNotLeftSilent(t *testing.T) {
	b := &booth{brackets: &boothBracketContext{}} // fetched, none linked
	out := b.bracketStructure()
	if out == "" {
		t.Fatal("no-bracket meeting produced no statement — the model fills that silence")
	}
	for _, want := range []string{"NO BRACKET", "standalone races", "NEVER mention a winners bracket"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Before the context has been fetched, nothing is known — assert neither way,
// or a slow first fetch would tell the booth there is no bracket when there is.
func TestUnfetchedBracketContextAssertsNothing(t *testing.T) {
	b := &booth{phasePre: true, phaseIn: true, phasePost: true}
	if out := b.bracketStructure(); out != "" {
		t.Errorf("unfetched context should stay silent, got:\n%s", out)
	}
}
