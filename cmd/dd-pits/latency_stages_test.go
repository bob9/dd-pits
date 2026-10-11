package main

import (
	"testing"
	"time"
)

// Where does a race call's time actually go?
//
// Every fix this session came from measuring one stage nobody had a number on.
// This measures the stages dd-pits OWNS — the ones between "we decided to speak"
// and "samples left the mixer" — so a future change can be judged rather than
// argued about. Generation (the neural model) and the device buffer are measured
// live in the logs; everything here is plumbing, and plumbing is what a test can
// pin.
func TestBusLatencyStages(t *testing.T) {
	frame := make([]byte, boothFrameBytes)
	frameMs := float64(boothFrameMs)

	// An IDLE bus: the best case, and the floor for any call.
	//
	// A cue cannot start mid-frame, so it waits for the next frame boundary.
	// Averaged over arrival times that is half a frame — pure structural cost,
	// paid by every call including the cached ones that need no generation.
	t.Run("idle bus, frame-boundary wait", func(t *testing.T) {
		var worst, total float64
		const samples = 20
		for i := 0; i < samples; i++ {
			m := newBoothMixer()
			// Arrive at a spread of offsets within a frame.
			cue := &boothCue{Text: "bob9 5.90", callKey: SoundTimeTrialEveryLap,
				bornAt: time.Now(), PCM: make([]byte, boothSampleRate*2*1500/1000),
				Rate: boothSampleRate}
			m.enqueue(cue)

			waited := 0.0
			for n := 0; n < 50; n++ {
				m.mu.Lock()
				cur := m.cur
				m.mu.Unlock()
				if cur == cue {
					break
				}
				m.nextFrame(frame)
				waited += frameMs
			}
			total += waited
			if waited > worst {
				worst = waited
			}
		}
		t.Logf("idle bus: avg %.0fms, worst %.0fms (frame size %dms)",
			total/samples, worst, boothFrameMs)
		// One frame is the structural limit. More than that means something is
		// holding the bus that should not be.
		if worst > frameMs {
			t.Errorf("a call waited %.0fms on an IDLE bus — more than one frame (%.0fms), "+
				"so something other than frame granularity is delaying it", worst, frameMs)
		}
	})

	// Commentary in the way: fixed this session, pinned here so it stays fixed.
	t.Run("commentary in the way", func(t *testing.T) {
		m := newBoothMixer()
		m.enqueue(&boothCue{Text: "long colour", commentary: true,
			PCM: make([]byte, boothSampleRate*2*12000/1000), Rate: boothSampleRate})
		m.nextFrame(frame)

		cue := &boothCue{Text: "bob9 5.90", callKey: SoundTimeTrialEveryLap,
			bornAt: time.Now(), PCM: make([]byte, boothSampleRate*2*1500/1000),
			Rate: boothSampleRate}
		m.enqueue(cue)

		waited := 0.0
		for n := 0; n < 500; n++ {
			m.mu.Lock()
			cur := m.cur
			m.mu.Unlock()
			if cur == cue {
				break
			}
			m.nextFrame(frame)
			waited += frameMs
		}
		t.Logf("behind 12s of commentary: %.0fms", waited)
		if waited > 400 {
			t.Errorf("a lap waited %.0fms behind commentary — the duck is not working", waited)
		}
	})
}
