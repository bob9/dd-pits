package main

import "testing"

// Advancing to the next race clears the LAST one's audio the way the start
// sequence does — including work still rendering.
//
// The old skip dropped only post-race KEYS from the queue and bus, which left
// the mid-render hole: a placings call already inside the TTS pipeline
// enqueued when it finished (the generation check only bumped on heat-green),
// heard as the previous heat's placings reading out over the next heat's
// staging. The skip now advances the generation, drains everything and ducks
// the bus, so the new race's "Next up…" enters an empty bus.
func TestSkipPostRaceClearsEverythingAndAdvancesGen(t *testing.T) {
	m := newBoothMixer()
	ra := newRaceAnnouncerAt(&SoundSet{}, m, nil, "")
	a := &agent{}
	a.setRaceAnnouncer(ra)

	// The previous heat's placings: one on air, one queued behind it.
	onAir := &boothCue{Text: "Bola finished in 1st. bob9 finished in 2nd",
		callKey: SoundRaceDone, priority: callPriorityLap, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes * 20)}
	queued := &boothCue{Text: "Race over", callKey: SoundRaceOver,
		priority: callPriorityRaceState, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes * 4)}
	m.enqueue(onAir)
	m.nextFrame(make([]byte, boothFrameBytes)) // placings go to air
	m.enqueue(queued)

	genBefore := ra.currentGen()
	a.skipPostRace()

	// The generation advanced: a placings call STILL RENDERING at the skip
	// dies at its completion check instead of airing late.
	if ra.currentGen() == genBefore {
		t.Fatal("skip must advance the generation — a mid-render placings call would air late")
	}
	// Nothing queued survives.
	if m.pending() != 0 {
		t.Fatalf("queued post-race audio survived the skip: %d cue(s)", m.pending())
	}
	// The cue on air is fading out, not playing on.
	m.mu.Lock()
	fading := m.cur == nil || m.cur.fadeLeft > 0
	m.mu.Unlock()
	if !fading {
		t.Fatal("the placings on air must be ducked out by the skip")
	}
}

// The fixed-line prewarm may only borrow the voice engine when nothing live
// needs it — a running heat, a pending call, or a race call on the bus each
// block it. A voice switch just before a heat used to run the whole sweep
// through the first heat and starve the holeshot into a perished drop.
func TestPrewarmYieldsToLiveRacing(t *testing.T) {
	m := newBoothMixer()
	ra := newRaceAnnouncerAt(&SoundSet{}, m, nil, "")

	if !ra.quietForPrewarm() {
		t.Fatal("an idle announcer must allow prewarming")
	}

	// A running heat blocks it — the next lap can land any second.
	ra.mu.Lock()
	ra.lastRace.State = "running"
	ra.mu.Unlock()
	if ra.quietForPrewarm() {
		t.Fatal("prewarm must never run during a live heat")
	}
	ra.mu.Lock()
	ra.lastRace.State = "ended"
	ra.mu.Unlock()

	// A race call on the bus blocks it — the countdown included.
	m.enqueue(&boothCue{Text: "ARM YOUR QUADS!", callKey: SoundStartRaceIn,
		priority: callPriorityRaceState, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes * 4)})
	if ra.quietForPrewarm() {
		t.Fatal("prewarm must wait while a race call is on the bus")
	}
}
