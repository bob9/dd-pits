package main

// Moving on to the next race skips what was left to say about the last one.
//
// The post-race tail — results, "race over", "finish your lap and land", the
// booth's wrap — used to die only when the NEXT start sequence fired. An
// operator who advanced straight to the next heat got the old heat's audio
// talking over the new one's staging: true sentences about a race everyone
// has mentally left. The operator changing FPVTrackside's selected race IS
// the "we've moved on" signal, so it is also the cut.

import "log"

// postRaceKeys are the calls that describe a heat AFTER it — meaningful while
// the room is still on that heat, noise the moment the operator advances.
func postRaceCall(key SoundKey) bool {
	switch key {
	case SoundRaceDone, SoundRaceOver, SoundAfterTimesUp, SoundRaceAnnounceResults:
		return true
	}
	return false
}

// skipPostRace drops the previous heat's audio wholesale, the same shape as
// the start sequence: everything on air and queued goes, the device buffer is
// flushed, and the generation advances so a call STILL RENDERING dies when it
// completes instead of airing late.
//
// It used to drop only the post-race KEYS from the queue and the bus — which
// left the mid-render hole: a placings call already inside the TTS pipeline
// enqueued when it finished, because the generation check only bumped on
// heat-green. Heard as the old heat's placings reading out over the next
// heat's staging after the operator advanced. Clearing everything is right
// for the same reason it is at the green light: at the instant the operator
// advances, every call in flight is about the race everyone just left, and
// the NEW race's "Next up…" and channel calls are derived after this hook on
// the same tick — they capture the new generation and enter an empty bus, so
// they are what plays next.
func (a *agent) skipPostRace() {
	if ra := a.raceAnnouncer(); ra != nil {
		ra.mu.Lock()
		ra.raceGen++
		ra.mu.Unlock()
		ra.drainPending(func(SoundKey) bool { return true })
		cut, dropped := ra.mixer.duck(func(*boothCue) bool { return true })
		ra.mixer.flushDevice()
		// A pilot finishing the NEXT heat is a new call, not a repeat.
		ra.bridge.NewHeat()
		log.Printf("race calls: operator advanced to the next race — cut %q on air, "+
			"dropped %d queued, the next race's calls take the bus", cut, dropped)
	}
	if a.booth != nil {
		a.booth.mu.Lock()
		// Queued finish beats die here; one mid-render is ABORTED outright —
		// the cancel reaches through the engine and STOPs the sidecar's
		// generation, so the GPU is free for the next race's calls rather
		// than finishing a wrap nobody will hear.
		kept := a.booth.beats[:0]
		for _, q := range a.booth.beats {
			if q.kind != "finish" {
				kept = append(kept, q)
			}
		}
		a.booth.beats = kept
		a.booth.heatGen++
		for _, cancel := range a.booth.renderCancels {
			cancel()
		}
		a.booth.mu.Unlock()
	}
}
