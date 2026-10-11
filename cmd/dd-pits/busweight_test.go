package main

import (
	"testing"
	"time"
)

// How long does a lap call wait when something is already on the bus?
//
// The mixer deliberately never cuts a cue mid-sentence. That is right for
// commentary, but it means a lap time queues behind whatever is playing — and
// the operator hears the lap seconds after the drone crossed, with every
// generation stage measuring fast. This puts a number on that wait.
func TestLapCallWaitsBehindWhateverIsPlaying(t *testing.T) {
	ms := func(d int) []byte { // d ms of silence at the bus rate
		return make([]byte, boothSampleRate*2*d/1000)
	}
	frame := make([]byte, boothFrameBytes)
	frameMs := int64(len(frame)) * 1000 / int64(boothSampleRate*2)

	cases := []struct {
		name      string
		playingMs int
		// commentary ahead of a lap is FADED OUT; a race call ahead of it is
		// not — losing a lap time is worse than delaying one, and race calls
		// are short. So only the commentary case has to be fast.
		commentaryAhead bool
		maxWaitMs       int64
	}{
		{"a short race call ahead of it", 2000, false, 2000},
		{"a commentary line ahead of it", 12000, true, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newBoothMixer()
			// Something already talking.
			m.enqueue(&boothCue{Text: "already talking", commentary: c.commentaryAhead,
				callKey: map[bool]SoundKey{true: "", false: SoundRaceLap}[c.commentaryAhead],
				PCM:     ms(c.playingMs), Rate: boothSampleRate})
			m.nextFrame(frame) // it is now on air

			// The lap lands.
			lap := &boothCue{Text: "bob9 6.35", callKey: SoundTimeTrialEveryLap,
				bornAt: time.Now(), PCM: ms(1500), Rate: boothSampleRate}
			m.enqueue(lap)

			// Pump until the lap is what is playing.
			var waitedMs int64
			for i := 0; i < 20000; i++ {
				m.mu.Lock()
				cur := m.cur
				m.mu.Unlock()
				if cur == lap {
					break
				}
				m.nextFrame(frame)
				waitedMs += frameMs
			}
			t.Logf("lap call waited %dms behind %dms of audio", waitedMs, c.playingMs)
			if waitedMs > c.maxWaitMs {
				t.Errorf("a lap time waited %dms to get on air — the drone crossed "+
					"the gate %.1fs before anyone heard it", waitedMs, float64(waitedMs)/1000)
			}
		})
	}
}
