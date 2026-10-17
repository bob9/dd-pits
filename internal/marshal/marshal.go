// Package marshal works out a pilot's laps from their receiver's signal
// strength (RSSI) trace, the way a RotorHazard timer does, and goes further:
// it finds the enter/exit lines on its own and points at the laps that look
// wrong, with the reason.
//
// A crossing starts when the signal rises above the enter line and ends when
// it falls below the exit line; the lap is stamped at the middle of the
// crossing's peak. That is RotorHazard's own rule (static/marshal.js
// processRXData), so a race recalculated here comes out exactly as it would on
// RotorHazard's Marshal page with the same lines.
package marshal

import (
	"math"
	"sort"
)

// Trace is one pilot's signal over a race. Times are seconds on the timer's
// clock, the same clock as Start (RotorHazard's monotonic race start).
type Trace struct {
	Times  []float64
	Values []float64
	Start  float64
}

// Options are the timer's lap rules.
type Options struct {
	MinLapMs           float64 // shortest lap the timer accepts
	MinLapDiscard      bool    // true: a too-short lap is discarded; false: kept but flagged
	MinFirstCrossingMs float64 // the holeshot can't come before this
	// RotorHazard can lower both lines for the start, until the first pass or
	// for this long, to catch the holeshot.
	StartLowerPct  float64
	StartLowerSecs float64
	StartDelayMax  float64 // seconds; the start window includes the staging delay
	TargetLaps     int     // laps to finish (0 = unknown); helps auto-detect
	HoleshotCounts bool    // the first pass is the holeshot, not a lap
}

// Crossing is one pass through the gate.
type Crossing struct {
	StartS, EndS float64 // seconds from the race start
	StampMs      float64 // the lap time stamp: middle of the peak
	Peak         float64
	Deleted      bool
	Why          string // why it was discarded, if it was
	Manual       bool   // added by hand: not second-guessed
}

// PeakNear is the strongest signal within 0.3s of a moment (ms from the
// start): the strength of a lap added by hand.
func (t Trace) PeakNear(atMs float64) float64 {
	best := 0.0
	for i, v := range t.Values {
		if i < len(t.Times) && math.Abs((t.Times[i]-t.Start)*1000-atMs) <= 300 && v > best {
			best = v
		}
	}
	return best
}

// Recalc finds the crossings in a trace for the given lines.
func Recalc(t Trace, enter, exit float64, o Options) []Crossing {
	var out []Crossing
	crossing := false
	var cStart, peak, peakFirst, peakLast float64
	last := math.Inf(-1)

	localEnter, localExit := enter, exit
	lowered := false
	if o.StartLowerPct > 0 && o.StartLowerSecs > 0 {
		if d := (enter - exit) * o.StartLowerPct / 100; d > 0 {
			localEnter, localExit, lowered = enter-d, exit-d, true
		}
	}
	finish := func(end float64) {
		stamp := ((peakLast+peakFirst)/2 - t.Start) * 1000
		if stamp <= 0 { // a pass before the start doesn't count
			return
		}
		c := Crossing{StartS: cStart - t.Start, EndS: end - t.Start, StampMs: stamp, Peak: peak}
		switch {
		case stamp < o.MinFirstCrossingMs:
			c.Deleted, c.Why = true, "before the first crossing is allowed"
		case o.MinLapDiscard && stamp < last+o.MinLapMs:
			c.Deleted, c.Why = true, "shorter than the minimum lap"
		default:
			last = stamp
		}
		out = append(out, c)
	}
	for i, v := range t.Values {
		if i >= len(t.Times) {
			break
		}
		tm := t.Times[i]
		if lowered && (len(out) > 0 || tm >= t.Start+o.StartLowerSecs+o.StartDelayMax) {
			localEnter, localExit, lowered = enter, exit, false
		}
		if !crossing && v > localEnter {
			crossing, cStart = true, tm
		}
		if v >= peak {
			peakLast = tm
			if v > peak {
				peakFirst, peak = tm, v
			}
		}
		if crossing && v < localExit {
			finish(tm)
			crossing, peak = false, 0
		}
	}
	if crossing && len(t.Times) > 0 {
		finish(t.Times[len(t.Times)-1])
	}
	return out
}

// Kept returns the crossings that count, in order.
func Kept(cs []Crossing) []Crossing {
	var out []Crossing
	for _, c := range cs {
		if !c.Deleted {
			out = append(out, c)
		}
	}
	return out
}

// LapTimes turns counted crossings into lap times (ms). With the holeshot
// counting, the first crossing starts lap 1 rather than ending one.
func LapTimes(cs []Crossing, holeshot bool) []float64 {
	k := Kept(cs)
	var out []float64
	prev := 0.0
	for i, c := range k {
		if i == 0 && holeshot {
			prev = c.StampMs
			continue
		}
		out = append(out, c.StampMs-prev)
		prev = c.StampMs
	}
	return out
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(i))
	hi := int(math.Ceil(i))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(i-float64(lo))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return percentile(s, 50)
}
