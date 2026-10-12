package raceformat

import (
	"encoding/json"
	"strconv"
	"testing"
)

func mpSeedNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "P" + strconv.Itoa(i+1)
	}
	return out
}

func TestMainsProgressionStateRoundTrip(t *testing.T) {
	cfg := Config{PilotNames: mpSeedNames(64), PilotsPerMain: 16, BumpUp: 4}
	f, err := New(KindMainsProgression, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Only the lowest main is built up front → its heats appear in Races().
	if len(f.Races()) == 0 {
		t.Fatal("expected the lowest main's heats in Races()")
	}
	blob, err := json.Marshal(f.State())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f2, err := FromState(KindMainsProgression, cfg, blob)
	if err != nil {
		t.Fatalf("FromState: %v", err)
	}
	if len(f2.Races()) != len(f.Races()) {
		t.Errorf("round-trip changed race count: %d → %d", len(f.Races()), len(f2.Races()))
	}
}

func TestMainsProgressionRacePrefixes(t *testing.T) {
	f, err := New(KindMainsProgression, Config{PilotNames: mpSeedNames(64), PilotsPerMain: 16, BumpUp: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, r := range f.Races() {
		// Every projected race id is "<label>:<inner>" and resolvable for editing.
		if len(r.ID) < 3 || r.ID[1] != ':' {
			t.Errorf("race id %q not main-prefixed", r.ID)
		}
	}
}

func TestMainsProgressionFinalOrderingCoversField(t *testing.T) {
	cfg := Config{PilotNames: mpSeedNames(64), PilotsPerMain: 16, BumpUp: 4}
	f, err := New(KindMainsProgression, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Drive the whole competition seeds-win via the engine's Simulate, rehydrate,
	// then assert FinalOrdering ranks all 64 with no dupes.
	mp := f.(*MainsProgressionFormat)
	blob, _ := json.Marshal(mp.Mains().Simulate())
	done, err := FromState(KindMainsProgression, cfg, blob)
	if err != nil {
		t.Fatalf("FromState(sim): %v", err)
	}
	if !done.IsComplete() {
		t.Fatal("simulated competition should be complete")
	}
	seeds := map[string]int{}
	for i, p := range cfg.PilotNames {
		seeds[p] = i + 1
	}
	order := FinalOrdering(done, seeds)
	if len(order) != 64 {
		t.Fatalf("FinalOrdering covers %d, want 64", len(order))
	}
	seen := map[string]bool{}
	for _, s := range order {
		if seen[s.Pilot] {
			t.Errorf("duplicate in FinalOrdering: %s", s.Pilot)
		}
		seen[s.Pilot] = true
	}
	if done.Champion() != "P1" {
		t.Errorf("champion = %q, want P1", done.Champion())
	}
}
