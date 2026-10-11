package main

import "testing"

// A cut must take back the frames we are still holding.
//
// The cut has to reach four places: the mixer queue, this channel, the OS pipe
// and the device. Only the queue and the device were cleared, so frames sitting
// in the channel were written to the sidecar right AFTER the abort — stale
// audio arriving just in time to play. Heard as commentary continuing past the
// Start button.
func TestFlushDropsFramesStillInHand(t *testing.T) {
	p := &localPlayer{frames: make(chan []byte, 2)}
	p.frames <- make([]byte, boothFrameBytes)
	p.frames <- make([]byte, boothFrameBytes)

	if len(p.frames) != 2 {
		t.Fatalf("setup: %d frames queued, want 2", len(p.frames))
	}
	// cmd is nil here, so Flush returns before signalling the device — the
	// drain must happen regardless of whether there is a process to signal.
	p.Flush()

	if n := len(p.frames); n != 0 {
		t.Errorf("%d frame(s) survived the cut (%dms of stale audio) — they would "+
			"be written to the device straight after the abort", n, n*boothFrameMs)
	}
}
