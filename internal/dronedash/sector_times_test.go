package dronedash

import (
	"testing"
)

func TestIsSplitDetection(t *testing.T) {
	cases := []struct {
		name string
		d    ftDetection
		want bool
	}{
		{"primary lap end", ftDetection{Valid: true, TimingSystemIndex: 0, IsLapEnd: true}, false},
		{"primary holeshot", ftDetection{Valid: true, TimingSystemIndex: 0, IsLapEnd: true, IsHoleshot: true}, false},
		{"split crossing", ftDetection{Valid: true, TimingSystemIndex: 2, IsLapEnd: false}, true},
		{"first split gate", ftDetection{Valid: true, TimingSystemIndex: 1, IsLapEnd: false}, true},
		{"invalidated split", ftDetection{Valid: false, TimingSystemIndex: 1, IsLapEnd: false}, false},
		// A patched-FT edge: a split-index detection flagged as a lap end belongs
		// to the lap pipeline, never here.
		{"lap end on split index", ftDetection{Valid: true, TimingSystemIndex: 1, IsLapEnd: true}, false},
		// Old agents / stock builds send zero values — must read as not-a-split.
		{"zero-value detection", ftDetection{Valid: true}, false},
	}
	for _, c := range cases {
		if got := isSplitDetection(c.d); got != c.want {
			t.Errorf("%s: isSplitDetection = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDetectionLapNumber(t *testing.T) {
	cases := []struct {
		name string
		d    ftDetection
		want int
	}{
		{"lap number present", ftDetection{LapNumber: 3, RaceSector: 302}, 3},
		{"race sector fallback", ftDetection{LapNumber: 0, RaceSector: 402}, 4},
		{"neither set", ftDetection{}, 0},
		{"race sector below 100", ftDetection{RaceSector: 2}, 0},
	}
	for _, c := range cases {
		if got := detectionLapNumber(c.d); got != c.want {
			t.Errorf("%s: detectionLapNumber = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseSectorMap(t *testing.T) {
	good := `[{"index":1,"name":"S1","fromGate":0,"toGate":3,"timingIndex":1},
	          {"index":2,"name":"S2","fromGate":3,"toGate":7,"timingIndex":2},
	          {"index":3,"name":"S3","fromGate":7,"toGate":0,"timingIndex":3}]`
	m := parseSectorMap(good)
	if len(m) != 3 || m[1] != 1 || m[2] != 2 || m[3] != 3 {
		t.Errorf("good config: got %v", m)
	}

	// A venue wired out of order: boundary 1 fires split timer 2 and vice versa.
	swapped := `[{"index":1,"fromGate":0,"toGate":3,"timingIndex":2},
	             {"index":2,"fromGate":3,"toGate":0,"timingIndex":1}]`
	m = parseSectorMap(swapped)
	if m[2] != 1 || m[1] != 2 {
		t.Errorf("swapped config: got %v", m)
	}

	if got := parseSectorMap(""); len(got) != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseSectorMap("not json"); len(got) != 0 {
		t.Errorf("malformed: got %v", got)
	}
	// timingIndex 0 / index 0 entries are dropped, not mapped.
	if got := parseSectorMap(`[{"index":0,"timingIndex":0},{"index":1,"timingIndex":0}]`); len(got) != 0 {
		t.Errorf("zero entries: got %v", got)
	}
}

func TestOrderCrossings(t *testing.T) {
	// Race.json's Detections array order is not chronological — orderCrossings
	// must sort by parsed time, tiebreaking on ID for a stable result.
	ds := []ftDetection{
		{ID: "c", Time: "2026/07/13 10:00:30.500"},
		{ID: "a", Time: "2026/07/13 10:00:10.100"},
		{ID: "d", Time: "2026/07/13 10:00:30.500"}, // same instant as "c"
		{ID: "b", Time: "2026/07/13 10:00:20.250"},
	}
	got := orderCrossings(ds)
	wantOrder := []string{"a", "b", "c", "d"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Fatalf("position %d: got %s, want %s (full: %v)", i, got[i].ID, w, ids(got))
		}
	}
	// Input must be untouched (defensive copy).
	if ds[0].ID != "c" {
		t.Errorf("input mutated: %v", ids(ds))
	}
}

func ids(ds []ftDetection) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}
