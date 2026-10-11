package booth

import (
	"strings"
	"testing"
)

// steady is a pilot with utterly unremarkable form — the baseline every case
// perturbs one field of, so a test failure names exactly what tripped.
func steady(name string) PilotForm {
	return PilotForm{
		Callsign: name,
		Laps:     []float64{30, 30, 30, 30},
		BestLap:  30, BestConsec: 60, PriorPB: 60,
		TypicalLap: 30, Position: 2, FieldSize: 4,
		QualPos: 2, FieldQualSize: 8,
	}
}

func keys(ts []Trigger) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Key
	}
	return out
}

func has(ts []Trigger, key string) bool {
	for _, t := range ts {
		if t.Key == key {
			return true
		}
	}
	return false
}

// TestNoTriggersOnOrdinaryRacing is the most important case: a normal heat must
// produce NO bits. A gag that fires every heat stops being a gag, and an empty
// result keeps an ordinary heat's prompt byte-identical.
func TestNoTriggersOnOrdinaryRacing(t *testing.T) {
	got := DetectTriggers([]PilotForm{steady("ALPHA"), steady("BRAVO")}, false, 2, nil)
	if len(got) != 0 {
		t.Fatalf("ordinary racing produced %v", keys(got))
	}
	if TriggerBlock(got) != "" {
		t.Fatal("no triggers must render no prompt block")
	}
}

func TestTriggerDetection(t *testing.T) {
	all := map[string]bool{}
	for _, c := range TriggerCatalogue {
		all[c.Key] = true
	}

	t.Run("drugs test on a smashed PB", func(t *testing.T) {
		p := steady("ALPHA")
		p.BestConsec, p.PriorPB = 50, 60 // 16.7% better
		got := DetectTriggers([]PilotForm{p}, true, 2, all)
		if !has(got, TrigDrugsTest) {
			t.Fatalf("got %v", keys(got))
		}
	})

	t.Run("a modest PB is a breakthrough, not a lab test", func(t *testing.T) {
		p := steady("ALPHA")
		p.BestConsec, p.PriorPB = 58, 60 // 3.3% — real but not absurd
		got := DetectTriggers([]PilotForm{p}, true, 2, all)
		if has(got, TrigDrugsTest) || !has(got, TrigBreakthrough) {
			t.Fatalf("got %v, want breakthrough only", keys(got))
		}
	})

	t.Run("no prior form means no PB claim", func(t *testing.T) {
		p := steady("ALPHA")
		p.PriorPB, p.BestConsec = 0, 20
		got := DetectTriggers([]PilotForm{p}, true, 2, all)
		if has(got, TrigDrugsTest) || has(got, TrigBreakthrough) {
			t.Fatalf("a first heat can't beat a PB that doesn't exist: %v", keys(got))
		}
	})

	t.Run("stewards on an out-of-character fast lap", func(t *testing.T) {
		p := steady("ALPHA")
		p.Laps = []float64{30, 20, 30, 30} // 20 vs a 30s usual
		got := DetectTriggers([]PilotForm{p}, true, 2, all)
		if !has(got, TrigStewards) {
			t.Fatalf("got %v", keys(got))
		}
	})

	t.Run("scenic route on a lap way off their own pace", func(t *testing.T) {
		p := steady("ALPHA")
		p.Laps = []float64{30, 30, 75, 30}
		got := DetectTriggers([]PilotForm{p}, true, 2, all)
		if !has(got, TrigScenicRoute) {
			t.Fatalf("got %v", keys(got))
		}
	})

	t.Run("one hot lap when the scoring run is well off the best", func(t *testing.T) {
		p := steady("ALPHA")
		p.BestLap, p.BestConsec = 20, 60 // ideal 40, actual 60 = 50% spread
		got := DetectTriggers([]PilotForm{p}, true, 2, all)
		if !has(got, TrigOneHotLap) {
			t.Fatalf("got %v", keys(got))
		}
	})

	t.Run("holeshot hero: wins gate one, finishes last", func(t *testing.T) {
		hero := steady("ALPHA")
		hero.Holeshot, hero.Position, hero.FieldSize = 2.0, 4, 4
		other := steady("BRAVO")
		other.Holeshot, other.Position, other.FieldSize = 3.0, 1, 4
		got := DetectTriggers([]PilotForm{hero, other}, false, 2, all)
		if !has(got, TrigHoleshotHero) {
			t.Fatalf("got %v", keys(got))
		}
		// Same shape in a time trial must NOT fire — qualifying starts are
		// staggered, so holeshots aren't comparable between pilots.
		if got := DetectTriggers([]PilotForm{hero, other}, true, 2, all); has(got, TrigHoleshotHero) {
			t.Fatal("holeshot hero must never fire in a time trial")
		}
	})

	t.Run("sandbagger: qualified low, finished at the sharp end", func(t *testing.T) {
		p := steady("ALPHA")
		p.QualPos, p.FieldQualSize, p.Position = 7, 8, 1
		got := DetectTriggers([]PilotForm{p}, false, 2, all)
		if !has(got, TrigSandbagger) {
			t.Fatalf("got %v", keys(got))
		}
	})
}

// TestTriggersRespectToggles pins that a club's off switch actually works —
// a disabled gag must never reach the prompt however loud the data is.
func TestTriggersRespectToggles(t *testing.T) {
	p := steady("ALPHA")
	p.BestConsec, p.PriorPB = 40, 60 // screaming drugs-test
	off := map[string]bool{TrigDrugsTest: false}
	if got := DetectTriggers([]PilotForm{p}, true, 2, off); has(got, TrigDrugsTest) {
		t.Fatalf("disabled trigger fired: %v", keys(got))
	}
	on := map[string]bool{TrigDrugsTest: true}
	if got := DetectTriggers([]PilotForm{p}, true, 2, on); !has(got, TrigDrugsTest) {
		t.Fatal("enabled trigger did not fire")
	}
}

// TestNoRoastIsHonoured is the one that protects a real person: a pilot who
// opted out of ribbing may still be CELEBRATED, but never becomes the bit.
func TestNoRoastIsHonoured(t *testing.T) {
	all := map[string]bool{}
	for _, c := range TriggerCatalogue {
		all[c.Key] = true
	}

	shy := steady("ALPHA")
	shy.NoRoast = true
	shy.Laps = []float64{30, 30, 90, 30} // begging for a scenic-route gag
	shy.BestConsec, shy.PriorPB = 40, 60 // and a lab test
	got := DetectTriggers([]PilotForm{shy}, true, 2, all)
	for _, tr := range got {
		if tr.Key != TrigBreakthrough {
			t.Fatalf("opted-out pilot got roasted with %q", tr.Key)
		}
	}

	// The celebratory one still reaches them.
	proud := steady("BRAVO")
	proud.NoRoast = true
	proud.BestConsec, proud.PriorPB = 58, 60
	if got := DetectTriggers([]PilotForm{proud}, true, 2, all); !has(got, TrigBreakthrough) {
		t.Fatalf("opting out of ribbing must not opt out of praise: %v", keys(got))
	}
}

// TestTriggersCappedAndDeterministic pins the two properties a regenerate
// depends on: a chaotic heat can't turn into a comedy routine, and the same
// data always yields the same bits in the same order.
func TestTriggersCappedAndDeterministic(t *testing.T) {
	all := map[string]bool{}
	for _, c := range TriggerCatalogue {
		all[c.Key] = true
	}
	var field []PilotForm
	for _, n := range []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO"} {
		p := steady(n)
		p.Laps = []float64{30, 18, 95, 30}
		p.BestLap, p.BestConsec, p.PriorPB = 18, 60, 90
		field = append(field, p)
	}
	first := DetectTriggers(field, true, 2, all)
	if len(first) > TrigMaxPerHeat {
		t.Fatalf("%d triggers exceeds the cap of %d", len(first), TrigMaxPerHeat)
	}
	// One bit per pilot — two gags about the same person reads as picking on them.
	seen := map[string]bool{}
	for _, tr := range first {
		if seen[tr.Pilot] {
			t.Fatalf("pilot %s got more than one bit", tr.Pilot)
		}
		seen[tr.Pilot] = true
	}
	for i := 0; i < 5; i++ {
		again := DetectTriggers(field, true, 2, all)
		if len(again) != len(first) {
			t.Fatal("detection is not deterministic")
		}
		for j := range again {
			if again[j].Key != first[j].Key || again[j].Pilot != first[j].Pilot {
				t.Fatal("detection order is not deterministic — a regenerate would differ")
			}
		}
	}
}

// TestTriggerBlockNamesPilots pins that the rendered block carries the pilot
// and stays advisory — the model must be free to skip the gag.
func TestTriggerBlockNamesPilots(t *testing.T) {
	block := TriggerBlock([]Trigger{{Key: TrigDrugsTest, Pilot: "ALPHA", Prompt: "ALPHA did a thing"}})
	if !strings.Contains(block, "ALPHA") {
		t.Fatal("block must name the pilot")
	}
	if !strings.Contains(block, "AT MOST one") || !strings.Contains(block, "skip them entirely") {
		t.Fatal("block must stay advisory, not mandatory")
	}
}

func TestMedianLap(t *testing.T) {
	if got := MedianLap(nil); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
	if got := MedianLap([]float64{30, 10, 20}); got != 20 {
		t.Fatalf("odd = %v, want 20", got)
	}
	if got := MedianLap([]float64{40, 10, 30, 20}); got != 25 {
		t.Fatalf("even = %v, want 25", got)
	}
	// Must not reorder the caller's slice — it's the pilot's lap order.
	laps := []float64{30, 10, 20}
	MedianLap(laps)
	if laps[0] != 30 {
		t.Fatal("MedianLap mutated its input")
	}
}

// TestTriggerBeat pins which beats may carry the gags. The finish call always
// may; the in-heat action calls only on the club's opt-in; and the beats where
// a joke would be wrong (a pilot down) or off-topic (meeting chatter) never do.
func TestTriggerBeat(t *testing.T) {
	for _, c := range []struct {
		kind   string
		inHeat bool
		want   bool
	}{
		{"finish", false, true}, // always, opt-in or not
		{"finish", true, true},
		{"progress", false, false}, // the default: wrap call only
		{"progress", true, true},
		{"launch", true, true},
		{"holeshot", true, true},
		{"pilot_out", true, false}, // never joke about a pilot going down
		{"pilot_back", true, false},
		{"meeting_talk", true, false}, // about the meeting, not this heat's data
		{"qual_move", true, false},
		{"staged", true, false}, // nothing has happened yet
		{"meeting_open", true, false},
		{"filler", true, false},
	} {
		if got := TriggerBeat(c.kind, c.inHeat); got != c.want {
			t.Errorf("TriggerBeat(%q, %v) = %v, want %v", c.kind, c.inHeat, got, c.want)
		}
	}
}

// TestLiveDropsFinishOnlyTriggers pins the honesty rule: mid-heat, Position is
// the RUNNING order, so the two gags that assert where a pilot FINISHED must
// not fire — while the lap-shape gags, which are true either way, still do.
func TestLiveDropsFinishOnlyTriggers(t *testing.T) {
	// LAST won the holeshot and sits last; SLOW took the scenic route.
	pilots := []PilotForm{
		{Callsign: "LAST", Laps: []float64{20, 20, 20}, Holeshot: 1.0, Position: 4, FieldSize: 4,
			TypicalLap: 20, QualPos: 4, FieldQualSize: 8},
		{Callsign: "SLOW", Laps: []float64{20, 55, 20}, Holeshot: 3.0, Position: 1, FieldSize: 4,
			TypicalLap: 20, QualPos: 6, FieldQualSize: 8},
	}
	all := map[string]bool{}
	for _, tr := range TriggerCatalogue {
		all[tr.Key] = true
	}

	has := func(ts []Trigger, key string) bool {
		for _, tr := range ts {
			if tr.Key == key {
				return true
			}
		}
		return false
	}

	done := DetectTriggers(pilots, false, 3, all)
	if !has(done, TrigHoleshotHero) {
		t.Fatal("finished heat: holeshot hero should fire (it's the existing contract)")
	}

	live := DetectTriggersOpt(pilots, false, 3, all, TriggerOpts{Live: true})
	if has(live, TrigHoleshotHero) {
		t.Error("live: holeshot hero fired mid-heat — they haven't finished at the back yet")
	}
	if has(live, TrigSandbagger) {
		t.Error("live: sandbagger fired mid-heat — the result hasn't counted yet")
	}
	if !has(live, TrigScenicRoute) {
		t.Error("live: scenic route should still fire — a slow lap is a slow lap")
	}
}

// TestTriggerBlockLiveKeepsRacingFirst pins that the mid-heat wording subordinates
// the gag to the live call — an aside, never the call itself.
func TestTriggerBlockLiveKeepsRacingFirst(t *testing.T) {
	ts := []Trigger{{Key: TrigScenicRoute, Pilot: "ALPHA", Prompt: "ALPHA did a thing"}}
	live := TriggerBlockOpt(ts, TriggerOpts{Live: true})
	if !strings.Contains(live, "ALPHA") || !strings.Contains(live, "AT MOST one") {
		t.Fatal("live block must still name the pilot and stay capped")
	}
	if !strings.Contains(live, "LIVE ACTION comes first") {
		t.Error("live block must put the racing ahead of the gag")
	}
	// The finished-heat wording is unchanged.
	if TriggerBlockOpt(ts, TriggerOpts{}) != TriggerBlock(ts) {
		t.Error("zero opts must be byte-identical to TriggerBlock")
	}
	if strings.Contains(TriggerBlock(ts), "LIVE ACTION") {
		t.Error("the finish-call block must not have picked up the live wording")
	}
}
