package marshal

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// pass is one gate pass in a synthetic trace: a bump of the given height at
// the given second; dip > 0 splits it in two (the classic double count).
type pass struct {
	at, peak, dip float64
}

// trace builds a 10 Hz-ish trace: a noisy background around 50 with
// occasional spikes to ~66, and the passes as bumps lasting about 0.6s.
func trace(seed int64, dur float64, passes []pass) Trace {
	r := rand.New(rand.NewSource(seed))
	t := Trace{Start: 100}
	for s := -1.0; s <= dur; s += 0.05 {
		v := 50 + r.NormFloat64()*3
		if r.Float64() < 0.01 {
			v += 12 // a reflection spike
		}
		for _, p := range passes {
			d := s - p.at
			b := (p.peak - 50) * math.Exp(-d*d/(2*0.12*0.12))
			if p.dip > 0 {
				b *= 1 - p.dip*math.Exp(-d*d/(2*0.04*0.04))
			}
			v = math.Max(v, 50+b)
		}
		t.Times = append(t.Times, t.Start+s)
		t.Values = append(t.Values, math.Round(v))
	}
	return t
}

func regular(n int, first, lap, peak float64) []pass {
	var ps []pass
	for i := 0; i < n; i++ {
		ps = append(ps, pass{at: first + float64(i)*lap, peak: peak})
	}
	return ps
}

var opts = Options{MinLapMs: 3000, MinLapDiscard: true, HoleshotCounts: true, TargetLaps: 3}

func TestRecalcMatchesThePasses(t *testing.T) {
	ps := regular(4, 2, 6, 120)
	tr := trace(1, 22, ps)
	cs := Kept(Recalc(tr, 90, 70, opts))
	if len(cs) != 4 {
		t.Fatalf("got %d crossings, want 4: %+v", len(cs), cs)
	}
	for i, c := range cs {
		if math.Abs(c.StampMs/1000-ps[i].at) > 0.06 {
			t.Fatalf("crossing %d stamped %.3fs, pass was at %.3fs", i, c.StampMs/1000, ps[i].at)
		}
	}
	laps := LapTimes(Recalc(tr, 90, 70, opts), true)
	if len(laps) != 3 || math.Abs(laps[0]-6000) > 120 {
		t.Fatalf("laps = %v", laps)
	}
}

func TestRecalcDiscardsTooShortLaps(t *testing.T) {
	tr := trace(2, 14, []pass{{at: 2, peak: 120}, {at: 3.2, peak: 120}, {at: 8, peak: 120}})
	cs := Recalc(tr, 90, 70, opts)
	if len(cs) != 3 || !cs[1].Deleted || cs[1].Why == "" || cs[2].Deleted {
		t.Fatalf("the 1.2s lap should be discarded: %+v", cs)
	}
}

func TestAutoDetectFindsCleanLines(t *testing.T) {
	tr := trace(3, 22, regular(4, 2, 6, 125))
	s, ok := AutoDetect(tr, opts)
	if !ok {
		t.Fatal("no suggestion")
	}
	if s.Passes != 4 {
		t.Fatalf("auto-detect counted %d passes, want 4: %+v", s.Passes, s)
	}
	if !(s.Enter > s.Noise && s.Enter < s.PassFloor && s.Exit < s.Enter) {
		t.Fatalf("lines in the wrong place: %+v", s)
	}
	if s.Confidence != "high" || s.Reason == "" {
		t.Fatalf("clean race should be high confidence with a reason: %+v", s)
	}
}

func TestAutoDetectKeepsAWeakPass(t *testing.T) {
	ps := regular(4, 2, 6, 125)
	ps[2].peak = 88 // a pilot flying wide on one lap
	tr := trace(4, 22, ps)
	s, ok := AutoDetect(tr, opts)
	if !ok || s.Passes != 4 || s.Enter >= 88 {
		t.Fatalf("the weak pass should still count: %+v", s)
	}
}

func TestAutoDetectMergesADoubleCount(t *testing.T) {
	ps := regular(4, 2, 6, 125)
	ps[1].dip = 0.75 // the signal dips mid-pass
	tr := trace(5, 22, ps)
	// Lines like a hand-set node often has: exit too high splits the pass.
	if n := len(Kept(Recalc(tr, 95, 90, Options{HoleshotCounts: true}))); n != 5 {
		t.Fatalf("setup: tight lines should double count (got %d)", n)
	}
	s, ok := AutoDetect(tr, opts)
	if !ok || s.Passes != 4 {
		t.Fatalf("auto-detect should merge the split pass: %+v", s)
	}
}

func TestIssuesPointAtAMissedPass(t *testing.T) {
	ps := regular(5, 2, 6, 125)
	ps[2].peak = 85
	tr := trace(6, 28, ps)
	cs := Recalc(tr, 95, 75, opts) // enter above the weak pass: it's missed
	is := Issues(tr, cs, 95, opts, nil)
	var long *Issue
	for i := range is {
		if is[i].Kind == "long" {
			long = &is[i]
		}
	}
	if long == nil {
		t.Fatalf("no long-lap issue: %+v", is)
	}
	if math.Abs(long.SuggestS-14) > 0.2 || !strings.Contains(long.Text, "add a lap") {
		t.Fatalf("should point at the weak pass at 14s: %+v", long)
	}
}

func TestIssuesSpotCrosstalk(t *testing.T) {
	mine := regular(4, 2, 6, 125)
	mine = append(mine, pass{at: 11, peak: 92}) // a bleed from the other pilot
	tr := trace(7, 22, mine)
	other := trace(8, 26, regular(4, 5, 6, 140)) // their pass at 11s is strong
	oc := Recalc(other, 90, 70, opts)
	cs := Recalc(tr, 85, 70, Options{HoleshotCounts: true})
	is := Issues(tr, cs, 85, opts, []Other{{Name: "Hornet", Crossings: oc}})
	found := false
	for _, x := range is {
		if x.Kind == "crosstalk" && math.Abs(x.StampMs-11000) < 200 && strings.Contains(x.Text, "Hornet") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no crosstalk issue at 11s: %+v", is)
	}
}

func TestAutoDetectGivesUpOnFlatSignal(t *testing.T) {
	if _, ok := AutoDetect(trace(9, 10, nil), opts); ok {
		t.Fatal("a trace with no passes should give no suggestion")
	}
}

// A race with only a couple of laps, one of them twice as long: the missed
// pass must still be found (a median of two laps would hide it).
func TestIssuesFindAMissedPassInAShortRace(t *testing.T) {
	ps := []pass{{at: 2.3, peak: 125}, {at: 7.9, peak: 124}, {at: 13.3, peak: 86}, {at: 18.8, peak: 126}}
	tr := trace(21, 24, ps)
	cs := Recalc(tr, 90, 80, opts)
	is := Issues(tr, cs, 90, opts, nil)
	var long, count bool
	for _, x := range is {
		if x.Kind == "long" && math.Abs(x.SuggestS-13.3) < 0.2 {
			long = true
		}
		if x.Kind == "count" {
			count = true
		}
	}
	if !long || !count {
		t.Fatalf("want a long lap pointing at 13.3s and a lap-count warning: %+v", is)
	}
}

// Two pilots racing side by side pass the gate a fraction of a second apart
// at full strength: that is close racing, not bleed.
func TestCloseRacingIsNotCrosstalk(t *testing.T) {
	a := trace(22, 24, regular(4, 2.3, 5.5, 125))
	b := trace(23, 24, regular(4, 2.5, 5.5, 124))
	ca := Recalc(a, 90, 80, opts)
	cb := Recalc(b, 90, 80, opts)
	for _, x := range Issues(a, ca, 90, opts, []Other{{Name: "B", Crossings: cb}}) {
		if x.Kind == "crosstalk" {
			t.Fatalf("close racing flagged as crosstalk: %+v", x)
		}
	}
}

// A bleed splits one lap in two: the usual lap must stay the real one, and
// no "missed pass" may be suggested at background noise.
func TestTypicalLapSurvivesASplitLap(t *testing.T) {
	if got := typicalLap([]float64{6300, 3300, 2500, 5900}, 3000); math.Abs(got-6100) > 300 {
		t.Fatalf("typical lap = %.0f, want about 6100", got)
	}
	if got := typicalLap([]float64{5600, 10900}, 3000); math.Abs(got-5600) > 1 {
		t.Fatalf("with a double-length lap, typical = %.0f, want 5600", got)
	}
	ps := []pass{{at: 2.9, peak: 122}, {at: 9.2, peak: 123}, {at: 12.5, peak: 95}, {at: 15.0, peak: 124}, {at: 20.9, peak: 121}}
	tr := trace(24, 24, ps)
	for _, x := range Issues(tr, Recalc(tr, 90, 80, opts), 90, opts, nil) {
		if x.Kind == "long" {
			t.Fatalf("no lap here is long: %+v", x)
		}
	}
}
