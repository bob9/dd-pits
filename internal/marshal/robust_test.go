package marshal

import "testing"

// Auto-detect must hold up across many noisy races, not one lucky seed.
func TestAutoDetectAcrossManyRaces(t *testing.T) {
	fails := map[string]int{}
	for seed := int64(100); seed < 300; seed++ {
		clean := regular(4, 2, 6, 125)
		if s, ok := AutoDetect(trace(seed, 22, clean), opts); !ok || s.Passes != 4 {
			fails["clean"]++
		}
		weak := regular(4, 2, 6, 125)
		weak[2].peak = 88
		if s, ok := AutoDetect(trace(seed, 22, weak), opts); !ok || s.Passes != 4 {
			fails["weak"]++
		}
		split := regular(4, 2, 6, 125)
		split[1].dip = 0.75
		if s, ok := AutoDetect(trace(seed, 22, split), opts); !ok || s.Passes != 4 {
			fails["split"]++
		}
		if _, ok := AutoDetect(trace(seed, 10, nil), opts); ok {
			fails["flat"]++
		}
	}
	for k, n := range fails {
		if n > 4 { // allow 2%: real timers have bad days too
			t.Errorf("%s: wrong on %d of 200 races", k, n)
		}
	}
	t.Logf("misses per 200: %v", fails)
}

// Problem-finding across many races: a missed pass is found and located,
// and clean races or close racing raise nothing.
func TestIssuesAcrossManyRaces(t *testing.T) {
	fails := map[string]int{}
	for seed := int64(400); seed < 600; seed++ {
		// A missed weak pass at 14s.
		ps := regular(5, 2, 6, 125)
		ps[2].peak = 85
		tr := trace(seed, 28, ps)
		found := false
		for _, x := range Issues(tr, Recalc(tr, 95, 75, opts), 95, opts, nil) {
			if x.Kind == "long" && x.SuggestS > 13.8 && x.SuggestS < 14.2 {
				found = true
			}
		}
		if !found {
			fails["missed"]++
		}
		// A clean race and a close rival: no issues at all.
		a := trace(seed, 22, regular(4, 2, 6, 125))
		b := trace(seed+1000, 22, regular(4, 2.15, 6, 124))
		for _, x := range Issues(a, Recalc(a, 90, 75, opts), 90, opts, []Other{{Name: "B", Trace: b}}) {
			fails["false "+x.Kind]++
		}
	}
	for k, n := range fails {
		if n > 4 {
			t.Errorf("%s: %d of 200 races", k, n)
		}
	}
	t.Logf("per 200: %v", fails)
}
