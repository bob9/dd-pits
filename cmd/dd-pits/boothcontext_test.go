package main

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// The audition must be given exactly what the live booth gives.
//
// This is the guarantee, and it is structural rather than comparative: both
// callers go through beatContext, so there is nothing to keep in step. The test
// pins the ROUTING — which builders each beat gets — because that is what
// silently drifted before, when staged and launch were auditioned on a
// lineup-only race with no meeting context while live they get both.
func TestBeatContextRoutingPerBeat(t *testing.T) {
	race, mv := scriptTestRace()

	// Distinctive markers, so a missing section is a failed assertion rather
	// than something a reader has to spot.
	x := beatExtrasCtx{
		BracketRace:      "\n[[BRACKET-RACE]]\n",
		BracketStructure: "\n[[BRACKET-STRUCTURE]]\n",
		PilotNotes:       "\n[[PILOT-NOTES]]\n",
		LadderMove:       "\n[[LADDER-MOVE]]\n",
	}

	cases := []struct {
		kind                       string
		wantMeeting, wantRace      bool
		wantUpNext, wantPilotNotes bool
		wantLadder                 bool
	}{
		{"meeting_open", true, false, true, false, false},
		{"filler", true, false, true, false, false},
		{"meeting_talk", true, true, true, false, false},
		{"staged", true, true, false, true, false},
		{"launch", true, true, false, false, false},
		{"holeshot", false, true, false, false, false},
		{"progress", false, true, false, false, false},
		{"pilot_out", false, true, false, false, false},
		{"pilot_back", false, true, false, false, false},
		{"qual_move", true, false, false, false, true},
		{"finish", true, true, true, false, false},
	}
	for _, c := range cases {
		got := beatContext(c.kind, race, mv, x)
		check := func(label string, want bool, marker string) {
			has := strings.Contains(got, marker)
			if has != want {
				t.Errorf("%s: %s %s — it should be %s",
					c.kind, label,
					map[bool]string{true: "is present", false: "is missing"}[has],
					map[bool]string{true: "there", false: "absent"}[want])
			}
		}
		check("the meeting standings", c.wantMeeting, "STANDINGS")
		check("the heat data", c.wantRace, "HEAT:")
		check("up-next", c.wantUpNext, "[[BRACKET-STRUCTURE]]" /*rides with up-next beats*/)
		check("pilot notes", c.wantPilotNotes, "[[PILOT-NOTES]]")
		check("the ladder move", c.wantLadder, "[[LADDER-MOVE]]")
	}
}

// A time trial is not a race, and the context has to say so.
//
// The two are scored and called completely differently — staggered starts, no
// holeshot to win, best-consecutive rather than first-across-the-line — and
// getting it wrong produces a call that is confidently about the wrong sport.
// The booth already withholds the holeshot figure for a TT at DATA level rather
// than only asking the model not to mention it, and this pins that.
func TestTimeTrialContextDiffersFromRace(t *testing.T) {
	race, mv := scriptTestRace()
	tt := race
	tt.TypeLabel = "Time Trial"
	tt.IsTT = true

	raceCtx := beatContext("progress", race, mv, beatExtrasCtx{})
	ttCtx := beatContext("progress", tt, mv, beatExtrasCtx{})

	if raceCtx == ttCtx {
		t.Fatal("a time trial and a race produce identical context — the format " +
			"is not reaching the model, and it will call a TT like a race")
	}

	// The race framing must name wheel-to-wheel racing; the TT must not.
	if !strings.Contains(raceCtx, "RACE") {
		t.Error("the race context does not state the format")
	}
	// Careful: the TT text SAYS "no wheel-to-wheel racing", so matching that
	// phrase alone catches the negation. Match what only a race claims.
	if !strings.Contains(ttCtx, "QUALIFYING") {
		t.Error("a time trial is not framed as qualifying")
	}
	if !strings.Contains(ttCtx, "STAGGERED") {
		t.Error("a time trial does not mention the staggered start, which is why " +
			"start times are not comparable between pilots")
	}
	if strings.Contains(ttCtx, "first across the line wins") {
		t.Error("a time trial is being described as first-across-the-line")
	}

	// The holeshot: a staggered start makes it neither scored nor comparable, so
	// the figure is withheld rather than merely discouraged. A prompt-level
	// instruction can be ignored; absent data cannot be quoted.
	if strings.Contains(ttCtx, "HOLESHOT WINNER") {
		t.Error("a time trial should have no holeshot winner — starts are staggered")
	}
	if !strings.Contains(raceCtx, "HOLESHOT WINNER") {
		t.Error("a race should name the holeshot winner")
	}

	// Gaps are meaningless on staggered starts, and the elapsed totals they are
	// computed from are not comparable between pilots.
	if strings.Contains(ttCtx, "GAPS (") {
		t.Error("a time trial should not carry pilot-to-pilot gaps — the starts " +
			"are staggered, so elapsed totals are not comparable")
	}
}

// A pre-flag beat must not be able to describe the race.
//
// Observed before this was fixed: the line-up beat produced "bob9 charges from
// the grid, leading the holeshot — Willman's strong start pushes him into
// second", a full account of racing that had not happened, because it had been
// handed a finished heat. The fix is the DATA, not a warning in the prompt.
func TestStagedCarriesNoRaceData(t *testing.T) {
	race, mv := scriptTestRace()
	got := beatContext("staged", preStart(race), mv, beatExtrasCtx{})

	// Only the HEAT section may be free of times. The meeting standings above it
	// legitimately carry every pilot's past best — that is history, not this
	// heat, and withholding it would leave nothing to build the line-up around.
	heat := got
	if i := strings.Index(got, "HEAT:"); i >= 0 {
		heat = got[i:]
	}
	for _, n := range []string{"11.42", "10.88", "11.05", "12.10", "13.05"} {
		if strings.Contains(heat, n) {
			t.Errorf("the line-up's HEAT section contains lap time %s — it has not run", n)
		}
	}
	// And it must say so outright, per pilot.
	if !strings.Contains(heat, "NO LAPS YET") {
		t.Error("the line-up does not state that pilots have no laps yet, which is " +
			"what stops a model describing their pace")
	}
	if strings.Contains(got, "HOLESHOT WINNER") {
		t.Error("the line-up context names a holeshot winner before the start")
	}
	// It must still know who is on the grid, or there is nothing to introduce.
	for _, who := range []string{"bob9", "Willman", "Fizz", "Wizz"} {
		if !strings.Contains(got, who) {
			t.Errorf("the line-up context does not mention %s", who)
		}
	}
}

// The ladder move is the fact qual_move exists to report.
//
// Without it the model invented one: it announced the slowest pilot on the board
// climbing to second with a "blistering" time three seconds faster than a rival
// who was in fact quicker. Every figure confidently wrong.
func TestQualMoveCarriesTheMove(t *testing.T) {
	_, mv := scriptTestRace()
	bare := beatContext("qual_move", ddapi.Race{}, mv, beatExtrasCtx{})
	if strings.Contains(bare, "LADDER MOVE") {
		t.Fatal("this test assumes no move was supplied")
	}
	withMove := beatContext("qual_move", ddapi.Race{}, mv,
		beatExtrasCtx{LadderMove: ladderMoveLine("bob9", "33.35s", 2, 1)})
	for _, want := range []string{"LADDER MOVE", "bob9", "33.35s", "P2", "P1"} {
		if !strings.Contains(withMove, want) {
			t.Errorf("the ladder move line is missing %q", want)
		}
	}
}

// Every beat's context defines who exists, positively.
//
// The prompt said "never invent" four times and a 4B writer announced a pilot
// called Mirella anyway. A prohibition asks for restraint; the whitelist
// defines the world. This pins that the fence rides on EVERY beat and names
// every pilot the beat may legitimately mention — heat AND board, because the
// finish beat reads the standings.
func TestEveryBeatCarriesTheRosterFence(t *testing.T) {
	race, mv := scriptTestRace()
	for _, kind := range []string{"meeting_open", "staged", "launch", "holeshot",
		"progress", "finish", "meeting_talk", "filler", "qual_move"} {
		got := beatContext(kind, race, mv, beatExtrasCtx{})
		if !strings.Contains(got, "THE ONLY PILOTS WHO EXIST") {
			t.Errorf("%s: no roster fence — an invented pilot has nothing stopping it", kind)
			continue
		}
		for _, who := range []string{"bob9", "Willman", "Fizz", "Wizz"} {
			if !strings.Contains(got, who) {
				t.Errorf("%s: %s missing from the fence — a legitimate mention would "+
					"read as forbidden", kind, who)
			}
		}
	}
}
