package main

import "testing"

// TestBoothDelayTimeline traces, for a race moment that happens at real time 0,
// when a VIEWER sees the video of it vs hears the AI commentary about it, under
// three OBS delay setups. It exposes whether the booth audio ends up
// double-delayed. All times are seconds of real/stream clock.
//
//	H = dd-pits hold (Manual delay): booth cue is emitted onto the live
//	    /booth.wav bus at moment + H  (H must cover generation latency).
//	R = OBS Render Delay on the VIDEO sources only (per-source, VRAM).
//	S = OBS Stream Delay on the whole program (post-mix, RAM) — common-mode.
//
// Viewer sees video-of-M at:      0 + R + S
// Viewer hears commentary-of-M at: (0 + H)  [bus emit]  + S   (booth audio is a
//
//	normal source, so it also gets Stream Delay S,
//	but NOT the video-only Render Delay R)
//
// Synced  <=>  R + S == H + S  <=>  R == H.  (S cancels; it is common-mode.)
func TestBoothDelayTimeline(t *testing.T) {
	const latency = 15 // s: commentary is only ready this long after the moment

	type setup struct {
		name    string
		H, R, S int // dd-pits hold, Render Delay (video), Stream Delay (program)
	}
	setups := []setup{
		{"YOUR PLAN: Stream Delay S=60, dd-pits hold H=0, no Render Delay", 0, 0, 60},
		{"Stream Delay ONLY with H matched: S=60, H=60, no Render Delay", 60, 0, 60},
		{"Render Delay ONLY: R=60 on video, H=60, no Stream Delay", 60, 60, 0},
		{"Correct combo: Render Delay R=30 (covers latency) + Stream Delay S=90 cushion, H=30", 30, 30, 90},
		{"Nothing (Off): let it run late", 0, 0, 0},
	}

	for _, s := range setups {
		// booth cue can't be emitted before it's generated.
		emit := s.H
		if emit < latency {
			emit = latency
		}
		videoSeen := s.R + s.S
		audioHeard := emit + s.S
		off := audioHeard - videoSeen
		verdict := "IN SYNC ✓"
		switch {
		case off > 0:
			verdict = "commentary LATE by " + itoa(off) + "s ✗"
		case off < 0:
			verdict = "commentary EARLY by " + itoa(-off) + "s ✗"
		}
		t.Logf("%-72s viewer sees video@%2ds, hears call@%2ds  ->  %s",
			s.name, videoSeen, audioHeard, verdict)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
