package marshal

import (
	"fmt"
	"math"
	"sort"
)

// Suggestion is auto-detect's answer: the lines, how sure it is, and why.
type Suggestion struct {
	Enter, Exit float64
	Passes      int     // crossings counted with these lines
	Noise       float64 // the signal's background ceiling between passes
	PassFloor   float64 // the weakest counted pass's peak
	Confidence  string  // high, medium or low
	Reason      string
	score       float64
}

// AutoDetect works out the enter and exit lines from the trace itself.
//
// It tries lines across the gap between the background and the passes and
// keeps the pair whose laps are the most believable: lap times that agree
// with each other, no laps too short to be real, no gaps where a pass must
// have been missed, the expected number of passes when the race's lap count
// is known, and lines that sit well clear of both the background and the
// weakest pass (so a slightly weaker pass next race still counts).
func AutoDetect(t Trace, o Options) (Suggestion, bool) {
	vals := make([]float64, 0, len(t.Values))
	for i, v := range t.Values {
		if i < len(t.Times) && t.Times[i] >= t.Start-0.5 {
			vals = append(vals, v)
		}
	}
	if len(vals) < 20 {
		return Suggestion{}, false
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	floor := percentile(sorted, 50)
	top := sorted[len(sorted)-1]
	low := percentile(sorted, 70)
	if top-low < 4 {
		return Suggestion{}, false // no passes stand out of the background
	}

	expected := 0
	if o.TargetLaps > 0 {
		expected = o.TargetLaps
		if o.HoleshotCounts {
			expected++
		}
	}
	step := math.Max(1, (top-low)/80)
	best := Suggestion{score: math.Inf(-1)}
	for enter := low + step; enter < top; enter += step {
		span := enter - floor
		for _, f := range []float64{0.2, 0.35, 0.5, 0.65} {
			exit := math.Round(enter - math.Max(2, span*f))
			s, ok := score(t, math.Round(enter), exit, o, expected)
			if ok && s.score > best.score {
				best = s
			}
		}
	}
	if math.IsInf(best.score, -1) {
		return Suggestion{}, false
	}
	gap := best.PassFloor - best.Noise
	if gap < math.Max(8, 0.2*(top-floor)) {
		return Suggestion{}, false // nothing stands clear of the background
	}
	best.Confidence = "low"
	switch {
	case gap > 0.35*(top-floor) && best.score > -4:
		best.Confidence = "high"
	case gap > 0.15*(top-floor) && best.score > -10:
		best.Confidence = "medium"
	}
	best.Reason = fmt.Sprintf("Background stays below %.0f; real passes peak at %.0f or more. Enter %.0f and exit %.0f count %d passes with steady lap times.",
		best.Noise, best.PassFloor, best.Enter, best.Exit, best.Passes)
	return best, true
}

// score rates one pair of lines: higher is more believable.
func score(t Trace, enter, exit float64, o Options, expected int) (Suggestion, bool) {
	cs := Recalc(t, enter, exit, o)
	kept := Kept(cs)
	if len(kept) < 2 {
		return Suggestion{}, false
	}
	laps := LapTimes(cs, o.HoleshotCounts)
	if len(laps) == 0 {
		return Suggestion{}, false
	}
	med := typicalLap(laps, o.MinLapMs)
	if med <= 0 {
		return Suggestion{}, false
	}
	var short, long int
	var good []float64
	for _, l := range laps {
		switch {
		case l < 0.6*med:
			short++
		case l > 1.6*med:
			long++
		default:
			good = append(good, l)
		}
	}
	cv := 0.0
	if len(good) > 1 {
		m := 0.0
		for _, g := range good {
			m += g
		}
		m /= float64(len(good))
		v := 0.0
		for _, g := range good {
			v += (g - m) * (g - m)
		}
		cv = math.Sqrt(v/float64(len(good))) / m
	}
	// How far the lines sit from the background and from the weakest pass.
	noise := backgroundCeiling(t, cs)
	floorPeak := math.Inf(1)
	for _, c := range kept {
		floorPeak = math.Min(floorPeak, c.Peak)
	}
	rng := floorPeak - noise
	margin := 0.0
	if rng > 0 {
		margin = math.Min(enter-noise, floorPeak-enter) / rng
	}
	s := -cv*20 - float64(short)*6 - float64(long)*5 + margin*6
	if margin < 0 {
		s += margin * 20 // a line inside the noise or above a real pass
	}
	if expected > 0 {
		s -= math.Abs(float64(len(kept)-expected)) * 3
	}
	return Suggestion{Enter: enter, Exit: exit, Passes: len(kept), Noise: noise, PassFloor: floorPeak, score: s}, true
}

// backgroundCeiling is the loudest the signal gets outside the crossings: the
// line must sit above it.
func backgroundCeiling(t Trace, cs []Crossing) float64 { return backgroundLevel(t, cs, 99) }

// backgroundLevel is a percentile of the signal outside the crossings.
func backgroundLevel(t Trace, cs []Crossing, pct float64) float64 {
	var out []float64
	j := 0
	for i, v := range t.Values {
		if i >= len(t.Times) {
			break
		}
		at := t.Times[i] - t.Start
		if at < 0 {
			continue
		}
		for j < len(cs) && cs[j].EndS < at {
			j++
		}
		if j < len(cs) && at >= cs[j].StartS && at <= cs[j].EndS {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return 0
	}
	sort.Float64s(out)
	return percentile(out, pct)
}

// Issue is a lap that looks wrong, and what to do about it.
type Issue struct {
	Kind     string  // short, long, crosstalk, weak
	StampMs  float64 // the crossing it's about (for long: the lap's end)
	Text     string
	SuggestS float64 // long: where the missed pass probably was (seconds from start); 0 if none
}

// Other is another pilot's signal in the same race, to spot one pilot's pass
// showing up on another pilot's channel.
type Other struct {
	Name      string
	Trace     Trace      // their signal: its strength at the moment is what counts
	Crossings []Crossing // used when there's no trace
}

// strongest is the other pilot's strongest signal within 0.2s of a moment.
func (ot Other) strongest(atMs float64) (peak, dt float64) {
	peak, dt = math.Inf(-1), math.Inf(1)
	if len(ot.Trace.Values) > 0 {
		for i, v := range ot.Trace.Values {
			if i >= len(ot.Trace.Times) {
				break
			}
			d := math.Abs((ot.Trace.Times[i]-ot.Trace.Start)*1000 - atMs)
			if d < 200 && v > peak {
				peak, dt = v, math.Min(dt, d)
			}
		}
		return
	}
	for _, oc := range ot.Crossings {
		if d := math.Abs(oc.StampMs - atMs); d < 200 && oc.Peak > peak {
			peak, dt = oc.Peak, d
		}
	}
	return
}

// Issues points at the laps worth a second look.
func Issues(t Trace, cs []Crossing, enter float64, o Options, others []Other) []Issue {
	kept := Kept(cs)
	laps := LapTimes(cs, o.HoleshotCounts)
	med := typicalLap(laps, o.MinLapMs)
	var peaks []float64
	for _, c := range kept {
		peaks = append(peaks, c.Peak)
	}
	medPeak := median(peaks)
	var out []Issue

	// Laps by crossing: lap i ends at kept[i+offset].
	offset := 0
	if o.HoleshotCounts {
		offset = 1
	}
	// A missed pass must stand clear of the usual background, so a blip of
	// noise is never offered as one.
	bg := backgroundLevel(t, cs, 90)
	for i, l := range laps {
		end := kept[i+offset]
		prevMs := 0.0
		if i+offset > 0 {
			prevMs = kept[i+offset-1].StampMs
		}
		switch {
		case med > 0 && l < 0.6*med:
			out = append(out, Issue{Kind: "short", StampMs: end.StampMs,
				Text: fmt.Sprintf("%.2fs lap, much shorter than the usual %.2fs: probably a double count or a stray signal.", l/1000, med/1000)})
		case med > 0 && l > 1.6*med:
			is := Issue{Kind: "long", StampMs: end.StampMs,
				Text: fmt.Sprintf("%.2fs lap, about %.1f× the usual %.2fs: a pass was probably missed.", l/1000, l/med, med/1000)}
			if at, v, ok := missedPeak(t, prevMs, end.StampMs, med); ok && v > bg+0.25*(medPeak-bg) {
				is.SuggestS = at
				is.Text += fmt.Sprintf(" There's a weaker peak (%.0f) at %s: add a lap there?", v, clock(at))
			}
			out = append(out, is)
		}
	}
	if o.TargetLaps > 0 && len(laps) < o.TargetLaps {
		out = append(out, Issue{Kind: "count", StampMs: lastStamp(kept),
			Text: fmt.Sprintf("Only %d of %d laps counted.", len(laps), o.TargetLaps)})
	}
	for _, c := range kept {
		if c.Manual {
			continue // the race director put it there
		}
		if medPeak > 0 && c.Peak < enter+0.15*(medPeak-enter) {
			out = append(out, Issue{Kind: "weak", StampMs: c.StampMs,
				Text: fmt.Sprintf("Only just over the enter line (peak %.0f against a usual %.0f): check it's a real pass.", c.Peak, medPeak)})
		}
		// Bleed from another channel: a clearly weaker bump at almost exactly
		// the moment of someone else's strong pass. (Pilots racing close pass
		// within a fraction of a second of each other at full strength; that
		// isn't bleed.) Every crossing on their channel counts here, not just
		// their laps: a bleed is a real signal whether it was a lap for them.
		src, best := "", math.Inf(1)
		for _, ot := range others {
			op, dt := ot.strongest(c.StampMs)
			if c.Peak < 0.8*op && c.Peak < 0.9*medPeak && dt < best {
				src, best = ot.Name, dt
			}
		}
		if src != "" {
			out = append(out, Issue{Kind: "crosstalk", StampMs: c.StampMs,
				Text: fmt.Sprintf("Lines up with %s's much stronger pass: probably their signal bleeding into this channel.", src)})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].StampMs < out[b].StampMs })
	return out
}

// missedPeak finds the strongest peak where a missed pass should be: between
// two counted passes, away from both.
func missedPeak(t Trace, fromMs, toMs, med float64) (float64, float64, bool) {
	lo := (fromMs + 0.5*med) / 1000
	hi := (toMs - 0.5*med) / 1000
	best, at := math.Inf(-1), 0.0
	for i, v := range t.Values {
		if i >= len(t.Times) {
			break
		}
		s := t.Times[i] - t.Start
		if s >= lo && s <= hi && v > best {
			best, at = v, s
		}
	}
	if math.IsInf(best, -1) {
		return 0, 0, false
	}
	return at, best, true
}

func clock(s float64) string {
	m := int(s) / 60
	return fmt.Sprintf("%d:%04.1f", m, s-float64(m*60))
}

// typicalLap is the lap time a normal lap takes, for judging the others.
//
// It's the time most laps agree with: a lap about that long supports it
// fully, a lap about twice as long (a missed pass) and two laps in a row
// that add up to one or two of it (a split lap or a bleed, perhaps with a
// pass lost beside it) support it too. A median
// can't do this with only a few laps: one double-length or one split lap
// pulls it off course.
func typicalLap(laps []float64, minLapMs float64) float64 {
	near := func(a, b float64) bool { return b > 0 && math.Abs(a-b) <= 0.2*b }
	best, bestScore := 0.0, -1.0
	for _, v := range laps {
		if v < minLapMs {
			continue
		}
		pair := func(sum float64) bool { return near(sum, v) || near(sum, 2*v) }
		sc := 0.0
		for i, l := range laps {
			switch {
			case near(l, v):
				sc += 2
			case near(l, 2*v):
				sc++
			case i+1 < len(laps) && pair(l+laps[i+1]), i > 0 && pair(laps[i-1]+l):
				sc++ // half of a split pair
			}
		}
		if sc > bestScore || (sc == bestScore && v < best) {
			best, bestScore = v, sc
		}
	}
	if bestScore < 0 {
		return median(laps)
	}
	var agree []float64
	for _, l := range laps {
		if near(l, best) {
			agree = append(agree, l)
		}
	}
	return median(agree)
}

func lastStamp(cs []Crossing) float64 {
	if len(cs) == 0 {
		return 0
	}
	return cs[len(cs)-1].StampMs
}
