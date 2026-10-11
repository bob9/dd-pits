package booth

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

func race(pilots ...ddapi.Pilot) ddapi.Race {
	return ddapi.Race{Label: "Round 9 — Race 1", TypeLabel: "Race", TargetLaps: 4, State: "running", Pilots: pilots}
}

// The gap to the pilot ahead is the single most useful thing to say about a
// running race, and the model was previously left to infer it from positions
// and lap times — so it either skipped it or guessed.
func TestRaceContextStatesGapsInSeconds(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "bob9", Position: 1, Laps: []float64{10.97, 11.02}, Total: 22.0},
		ddapi.Pilot{Name: "Willman", Position: 2, Laps: []float64{10.53, 12.30}, Total: 23.4},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "GAPS") {
		t.Fatalf("no GAPS block:\n%s", out)
	}
	if !strings.Contains(out, "Willman is 1.40s behind bob9") {
		t.Errorf("gap not stated in seconds:\n%s", out)
	}
}

// Being a lap down is a bigger statement than any number of seconds.
func TestRaceContextPrefersLapsDownOverSeconds(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "bob9", Position: 1, Laps: []float64{11, 11}, Total: 22},
		ddapi.Pilot{Name: "Willman", Position: 2, Laps: []float64{12}, Total: 12},
	), &ddapi.Meeting{}, false)

	// A one-second deficit at the shared lap is NOT a lap down, however many
	// laps separate their counts.
	if strings.Contains(out, "genuinely lapped") {
		t.Errorf("one second apart is not lapped:\n%s", out)
	}
}

// Handing over a flat list of laps invited the model to re-read times it had
// already called — the same figures repeated across consecutive updates.
func TestRaceContextNamesTheLatestLapAndFencesTheRest(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "bob9", Position: 1, Laps: []float64{10.97, 11.02, 10.88}, Total: 32.9},
		ddapi.Pilot{Name: "Willman", Position: 2, Laps: []float64{10.53}, Total: 10.53},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "LATEST LAP (lap 3): 10.88") {
		t.Errorf("latest lap not called out:\n%s", out)
	}
	if !strings.Contains(out, "do NOT re-read") {
		t.Errorf("earlier laps not fenced off:\n%s", out)
	}
	// A pilot on their first lap has nothing to fence.
	if strings.Count(out, "do NOT re-read") != 1 {
		t.Errorf("fence should only appear for pilots with prior laps:\n%s", out)
	}
}

// Qualifying starts are staggered, so elapsed totals are not comparable
// between pilots — a gap there would be fiction.
func TestNoGapsInQualifying(t *testing.T) {
	tt := race(
		ddapi.Pilot{Name: "bob9", Position: 1, Laps: []float64{10.97}, Total: 10.97},
		ddapi.Pilot{Name: "Willman", Position: 2, Laps: []float64{11.5}, Total: 11.5},
	)
	tt.TypeLabel = "Time Trial"
	tt.IsTT = true
	if out := RaceContext(tt, &ddapi.Meeting{Window: 3}, false); strings.Contains(out, "GAPS") {
		t.Errorf("gaps must not be offered in qualifying:\n%s", out)
	}
}

// The lineup (pre-race) has no laps or gaps to report yet.
func TestNoGapsInLineup(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "bob9", Position: 1},
		ddapi.Pilot{Name: "Willman", Position: 2},
	), &ddapi.Meeting{}, true)
	if strings.Contains(out, "GAPS") {
		t.Errorf("lineup should carry no gaps:\n%s", out)
	}
}

// A mid-heat call must be TIGHT and about the heat in progress.
//
// The brief used to licence colour "after the time and the gap" and offered
// angles built around the wider meeting — which is what produced calls that
// wandered off the race in front of the listener. A mid-race update has one
// job: the lap, and at most one thing that follows from it.
func TestProgressBriefIsTightAndAboutThisHeat(t *testing.T) {
	b := BeatBriefs["progress"]

	// The lap comes first, and repeats are still the worst failure.
	for _, want := range []string{"OPEN WITH THE LAP", "GAPS", "re-read", "STOP"} {
		if !strings.Contains(b, want) {
			t.Errorf("progress brief missing %q", want)
		}
	}

	// Exactly the three things a mid-race call may add.
	for _, want := range []string{"POSITION", "PERSONAL BEST", "LEADERBOARD"} {
		if !strings.Contains(b, want) {
			t.Errorf("progress brief should allow %q", want)
		}
	}

	// And it must forbid the wandering.
	for _, want := range []string{"wider meeting", "No similes"} {
		if !strings.Contains(b, want) {
			t.Errorf("progress brief should rule out %q", want)
		}
	}

	// A pilot who has not raced must not be described at all.
	if !strings.Contains(b, "NO LAPS YET") {
		t.Error("progress brief must tell it to ignore pilots with no laps")
	}
}

// The rotating angles must all be about the heat on track.
//
// Two were removed for wandering: a two-hander "aside", and one built around
// the pace of the MEETING. Both pulled the call away from the race the listener
// is watching.
func TestProgressAnglesStayOnThisHeat(t *testing.T) {
	for _, a := range ProgressAngles {
		for _, bad := range []string{"PACE OF THE MEETING", "SHORT ASIDE"} {
			if strings.Contains(a, bad) {
				t.Errorf("angle wanders off the heat: %q", a)
			}
		}
	}
	// The two that replaced them are the ones asked for on air.
	var pb, board bool
	for _, a := range ProgressAngles {
		pb = pb || strings.Contains(a, "PERSONAL BEST")
		board = board || strings.Contains(a, "MOVE ON THE LEADERBOARD")
	}
	if !pb || !board {
		t.Error("angles should cover personal bests and leaderboard moves")
	}
}

// Observed live: bob9 led Willman by 1.37s on elapsed time while FPVTrackside's
// Position field had Willman P1 — so ordering by Position announced bob9 as
// 1.37s BEHIND. Right magnitude, wrong direction, which is worse than silence.
func TestGapDirectionFollowsTimingNotPositionField(t *testing.T) {
	// Real numbers from the race that exposed this. Total = holeshot + laps.
	out := RaceContext(race(
		ddapi.Pilot{Name: "Willman", Position: 1, Laps: []float64{32.03}, Total: 40.65},
		ddapi.Pilot{Name: "bob9", Position: 2, Laps: []float64{33.32}, Total: 39.29},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "Willman is 1.36s behind bob9") {
		t.Errorf("gap direction wrong — bob9 is ahead on elapsed time:\n%s", out)
	}
	if strings.Contains(out, "bob9 is 1.36s behind") {
		t.Error("bob9 announced as behind while leading on time")
	}
	if !strings.Contains(out, "LEADER: bob9") {
		t.Errorf("leader must match the gap ordering:\n%s", out)
	}
}

// The holeshot is the FIRST pilot to gate one — the smallest time. It was
// being credited to the wrong pilot because the model had to find the minimum
// across rows itself.
func TestHoleshotWinnerIsStatedNotInferred(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "Willman", Position: 1, Holeshot: 8.62, Laps: []float64{32.03}, Total: 40.65},
		ddapi.Pilot{Name: "bob9", Position: 2, Holeshot: 5.97, Laps: []float64{33.32}, Total: 39.29},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "HOLESHOT WINNER: bob9, 5.97s") {
		t.Errorf("holeshot winner not stated as the smallest time:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT credit anyone else") {
		t.Error("no guard against crediting the wrong pilot")
	}
}

// More laps completed still decides the ORDER, but not the wording of the gap:
// at their last shared lap these two are level, so calling bob9 "a lap down"
// would badly misrepresent a race he is 0.04s off.
func TestLapsCompletedDecideOrderNotTheGapWording(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "Willman", Position: 1, Laps: []float64{32.03, 35.02, 24.17}, Total: 99.84},
		ddapi.Pilot{Name: "bob9", Position: 2, Laps: []float64{33.32, 33.77}, Total: 73.05},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "LEADER: Willman") {
		t.Errorf("more laps completed should lead:\n%s", out)
	}
	// At their shared lap 2 these two are 0.04s apart — nothing like "a lap down".
	if !strings.Contains(out, "compared at lap 2") && !strings.Contains(out, "AHEAD of Willman at lap 2") {
		t.Errorf("gap should be measured at the shared lap:\n%s", out)
	}
	if strings.Contains(out, "genuinely lapped") {
		t.Errorf("two pilots 0.04s apart are not lapped:\n%s", out)
	}
}

// Attribution guard: every number belongs to the pilot on its line.
func TestAttributionWarningPresent(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "a", Position: 1, Laps: []float64{10}, Total: 10},
		ddapi.Pilot{Name: "b", Position: 2, Laps: []float64{11}, Total: 11},
	), &ddapi.Meeting{}, false)
	if !strings.Contains(out, "NEVER give one pilot's lap time or holeshot to another") {
		t.Errorf("attribution guard missing:\n%s", out)
	}
}

// Counting completed laps alone overstates the gap: a pilot one lap "down" is
// usually most of the way round that lap and only seconds behind on the road.
// The gap is therefore measured at the last lap BOTH have completed.
func TestGapMeasuredAtLastSharedLapNotLapCount(t *testing.T) {
	// Willman has finished 2 laps; bob9 has finished 1 and is part-way round
	// his second. At their shared lap 1 the gap is only 1.36s.
	out := RaceContext(race(
		ddapi.Pilot{Name: "Willman", Position: 1, Holeshot: 8.62,
			Laps: []float64{32.03, 35.02}, Total: 75.67},
		ddapi.Pilot{Name: "bob9", Position: 2, Holeshot: 5.97,
			Laps: []float64{33.32}, Total: 39.29},
	), &ddapi.Meeting{}, false)

	// bob9 was actually QUICKER at the lap they share, and is mid-lap. Calling
	// him a lap down would invert the race.
	if !strings.Contains(out, "bob9 was 1.36s AHEAD of Willman at lap 1") {
		t.Errorf("mid-lap pilot's real standing not stated:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT call bob9 a lap down") {
		t.Errorf("no guard against the lap-down phrasing:\n%s", out)
	}
}

// Genuinely lapped is when the gap exceeds a whole lap of the leader's pace.
func TestGenuinelyLappedStillReadsAsLapped(t *testing.T) {
	// Willman averages ~33s a lap; bob9 is over a minute back at their shared lap.
	out := RaceContext(race(
		ddapi.Pilot{Name: "Willman", Position: 1, Holeshot: 5.0,
			Laps: []float64{33, 33, 33}, Total: 104},
		ddapi.Pilot{Name: "bob9", Position: 2, Holeshot: 5.0,
			Laps: []float64{95}, Total: 100},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "genuinely lapped") {
		t.Errorf("a pilot over a lap back should read as lapped:\n%s", out)
	}
}

// With no shared completed lap there is nothing honest to compare.
func TestNoSharedLapSaysSo(t *testing.T) {
	out := RaceContext(race(
		ddapi.Pilot{Name: "Willman", Position: 1, Holeshot: 5.0, Laps: []float64{33}, Total: 38},
		ddapi.Pilot{Name: "bob9", Position: 2, Holeshot: 6.0, Total: 6},
	), &ddapi.Meeting{}, false)

	if !strings.Contains(out, "no shared lap to compare yet") {
		t.Errorf("should decline to state a gap:\n%s", out)
	}
}

// A finished race's context states the result outright — winner by TOTAL with
// the gaps computed — because "the winning time" is a selection across numbers
// and the model selected the HOLESHOT: it announced a win "with a time of
// 33.12 seconds" (the holeshot) while getting the runner-up's +9.70s gap
// right. The numbers below are that real heat's.
func TestFinishedRaceStatesTheResult(t *testing.T) {
	r := ddapi.Race{
		ID: "r1", TypeLabel: "Race", State: "ended",
		Pilots: []ddapi.Pilot{
			{Name: "bob9", Holeshot: 33.12, Laps: []float64{3.93, 9.5, 7.03, 90.28}, Total: 143.87, Position: 1},
			{Name: "MirellaFPV", Holeshot: 73.04, Laps: []float64{1.6, 6.7, 66.73, 5.5, 3.7}, Total: 153.57, Position: 2},
		},
	}
	got := raceFacts(r)
	if !strings.Contains(got, "FINAL RESULT") {
		t.Fatalf("no result block for an ended race:\n%s", got)
	}
	if !strings.Contains(got, "1st bob9 — race time 143.87s") {
		t.Errorf("the winner's time must be the TOTAL, stated outright:\n%s", got)
	}
	if !strings.Contains(got, "+9.70s behind the winner") {
		t.Errorf("the gap must be computed, not left to the model:\n%s", got)
	}
	if !strings.Contains(got, "never the holeshot") {
		t.Errorf("the block must say what a race time is NOT")
	}

	// A running race carries no result block — there is no result yet.
	r.State = "running"
	if strings.Contains(raceFacts(r), "FINAL RESULT") {
		t.Error("a running race must not state a final result")
	}
}

// An empty race queue is STATED to the model, never omitted — told to "hand
// toward what's next" with no up-next data at all, it announced "Round 6,
// Race 7 is on the grid" with a full line-up and an advancement rule, when
// nothing was queued.
func TestEmptyQueueIsStatedNotOmitted(t *testing.T) {
	got := UpNextContext(&ddapi.Meeting{})
	if !strings.Contains(got, "nothing is queued") {
		t.Fatalf("an empty queue must be stated outright, got %q", got)
	}
	if !strings.Contains(got, "Do NOT announce, name, or invent") {
		t.Error("the prohibition must be explicit — absent data is what invited the invention")
	}
}
