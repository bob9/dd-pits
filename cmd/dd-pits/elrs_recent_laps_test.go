package main

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/elrs"
)

func lapsJoined(p *vmPilot, n int) string {
	return strings.Join(recentLapsLines(p, n), " / ")
}

func TestRecentLapsLines(t *testing.T) {
	p := &vmPilot{Holeshot: 2.14, DispLaps: []float64{45.012, 44.229, 43.561, 44.005}}

	// Window 3: only the last three entries of [HS, L1..L4] survive.
	if got, want := lapsJoined(p, 3), "L2:44.23 / L3:43.56 / L4:44.01"; got != want {
		t.Errorf("window 3 = %q, want %q", got, want)
	}

	// Window 5: holeshot rolls back in.
	if got, want := lapsJoined(p, 5), "HS:2.14 / L1:45.01 / L2:44.23 / L3:43.56 / L4:44.01"; got != want {
		t.Errorf("window 5 = %q, want %q", got, want)
	}

	// First lap of a race: holeshot + L1.
	early := &vmPilot{Holeshot: 2.14, DispLaps: []float64{45.01}}
	if got, want := lapsJoined(early, 3), "HS:2.14 / L1:45.01"; got != want {
		t.Errorf("early = %q, want %q", got, want)
	}

	// No holeshot recorded (e.g. TT with holeshot suppressed).
	noHS := &vmPilot{DispLaps: []float64{45.01, 44.23}}
	if got, want := lapsJoined(noHS, 3), "L1:45.01 / L2:44.23"; got != want {
		t.Errorf("no holeshot = %q, want %q", got, want)
	}

	// Nothing recorded yet.
	if got := recentLapsLines(&vmPilot{}, 3); len(got) != 0 {
		t.Errorf("empty = %v, want none", got)
	}
}

func TestHoldFor(t *testing.T) {
	cases := []struct{ hold, def, want int }{
		{0, 0, 0},   // default: stays
		{0, 4, 4},   // default: element's own uptime
		{-1, 4, 0},  // user forced always-on
		{10, 4, 10}, // user override
		{5, 0, 5},   // user timed on a normally-static element
	}
	for _, c := range cases {
		pos := elrs.ElementPos{HoldSecs: c.hold}
		if got := pos.HoldFor(c.def); got != c.want {
			t.Errorf("HoldFor(hold=%d, def=%d) = %d, want %d", c.hold, c.def, got, c.want)
		}
	}
}
