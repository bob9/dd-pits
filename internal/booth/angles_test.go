package booth

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// Asking a model to "vary it" fails over a long heat: given the same data and
// brief it converges on one shape, and a listener hearing "X leads, Y is
// closing" on a loop knows at once they are hearing a machine. Rotation makes
// consecutive calls structurally different regardless of what it would pick.
func TestProgressAngleRotatesAndNeverRepeatsBackToBack(t *testing.T) {
	if len(ProgressAngles) < 5 {
		t.Fatalf("only %d angles — too few to keep a long heat varied", len(ProgressAngles))
	}
	var prev string
	for i := range len(ProgressAngles) * 2 {
		a := ProgressAngle(i)
		if a == "" {
			t.Fatalf("angle %d empty", i)
		}
		if a == prev {
			t.Errorf("angle repeated back-to-back at call %d", i)
		}
		prev = a
	}
	// A full cycle covers every angle.
	seen := map[string]bool{}
	for i := range len(ProgressAngles) {
		seen[ProgressAngle(i)] = true
	}
	if len(seen) != len(ProgressAngles) {
		t.Errorf("a full cycle produced %d distinct angles, want %d", len(seen), len(ProgressAngles))
	}
}

// The angle must not override the thing the pilots are listening for.
func TestAngleStillPutsTheLapTimeFirst(t *testing.T) {
	a := ProgressAngle(0)
	if !strings.Contains(a, "lap time still comes first") {
		t.Error("angle does not preserve lap-time-first")
	}
	if !strings.Contains(a, "do not announce it") {
		t.Error("model may read the angle aloud")
	}
	if !strings.Contains(a, "Do not reuse a phrase") {
		t.Error("no guard against recycling phrasing")
	}
}

// Negative counters must not panic — the caller increments forever.
func TestProgressAngleHandlesAnyCounter(t *testing.T) {
	for _, n := range []int{-1, -999, 0, 1, 1 << 20} {
		if ProgressAngle(n) == "" {
			t.Errorf("counter %d produced no angle", n)
		}
	}
}

// Form gives the booth something concrete to say beyond the raw number.
func TestLapShapeDescribesTheRunNotJustTheTime(t *testing.T) {
	cases := []struct {
		name string
		laps []float64
		want string
	}{
		{"personal best", []float64{22.0, 21.0, 20.0}, "best lap of the heat"},
		{"picking up", []float64{24.0, 20.0, 21.5}, "picking up"},
		{"lost time", []float64{20.0, 20.0, 30.0}, "lost time"},
		{"steady", []float64{20.0, 20.2, 20.1}, "holding steady"},
	}
	for _, c := range cases {
		got := LapShape(ddapi.Pilot{Name: "bob9", Laps: c.laps})
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to mention %q", c.name, got, c.want)
		}
	}
	// A single lap has nothing to compare against.
	if LapShape(ddapi.Pilot{Name: "bob9", Laps: []float64{20}}) != "" {
		t.Error("one lap should produce no form line")
	}
}
