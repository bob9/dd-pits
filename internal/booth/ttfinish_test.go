package booth

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// A finished TIME TRIAL wrap was announcing a finishing order — "X takes the
// heat, Y second" — because the per-pilot rows carried the timing system's
// positions (an artefact of staggered starts) and no computed result block
// existed for TT the way FINAL RESULT does for races. The result of
// qualifying IS the board: the context must withhold heat positions and hand
// over each pilot's run, where it landed them, and the leader picture.
func TestTTFinishTalksBoardNeverFinishingOrder(t *testing.T) {
	mv := &ddapi.Meeting{Name: "Cup", Window: 2, Board: []ddapi.Standing{
		{Name: "B", Pos: 1, BestConsec: 20.00},
		{Name: "A", Pos: 2, BestConsec: 21.10},
		{Name: "C", Pos: 3, BestConsec: 24.50},
	}}
	r := ddapi.Race{Label: "TT 4", TypeLabel: "Time Trial", ConsecWindow: 2, State: "ended",
		Pilots: []ddapi.Pilot{
			{Name: "A", ChannelLabel: "R1", Position: 1, Laps: []float64{10.5, 10.6, 11.2}},
			{Name: "C", ChannelLabel: "R2", Position: 2, Laps: []float64{12.4, 12.1}},
		}}
	ctx := RaceContext(r, mv, false)

	// The timing system's heat positions must never reach the model — a figure
	// in the context is a figure that can be quoted.
	if strings.Contains(ctx, "position 1") || strings.Contains(ctx, "position 2") {
		t.Fatalf("TT context carries heat positions — that is the announced finishing order: %s", ctx)
	}
	if !strings.Contains(ctx, "NO finishing order") {
		t.Fatalf("TT result block must state there is no finishing order: %s", ctx)
	}
	// Each pilot's best-window run THIS heat, computed: A = 10.5+10.6 = 21.10,
	// C = 12.4+12.1 = 24.50.
	if !strings.Contains(ctx, "A: best 2-consec THIS RUN 21.10s") {
		t.Fatalf("A's run this heat must be computed and stated: %s", ctx)
	}
	if !strings.Contains(ctx, "C: best 2-consec THIS RUN 24.50s") {
		t.Fatalf("C's run this heat must be computed and stated: %s", ctx)
	}
	// Where the runs put them, with the gap to the top.
	if !strings.Contains(ctx, "P2 on the board (+1.10s off the top)") {
		t.Fatalf("board position + gap to the leader must be stated: %s", ctx)
	}
	// The leader did NOT fly here — the block must say so, and invite the
	// did-anyone-get-close angle instead of leaving the leader unmentioned.
	if !strings.Contains(ctx, "BOARD LEADER: B at 20.00s") ||
		!strings.Contains(ctx, "did NOT fly in this heat") {
		t.Fatalf("leader picture missing or wrong: %s", ctx)
	}

	// When the leader IS in the heat, the block says the top spot was live.
	r.Pilots = append(r.Pilots, ddapi.Pilot{Name: "B", ChannelLabel: "R3",
		Position: 3, Laps: []float64{10.0, 10.0}})
	if ctx := RaceContext(r, mv, false); !strings.Contains(ctx, "THE LEADER") ||
		!strings.Contains(ctx, "top spot was on the line") {
		t.Fatalf("a leader flying in the heat must be flagged: %s", ctx)
	}

	// A RUNNING time trial gets no result block — mid-heat, the ladder lines
	// and qual_move carry the story.
	r.State = "running"
	if ctx := RaceContext(r, mv, false); strings.Contains(ctx, "QUALIFYING RESULT") {
		t.Fatalf("result block must only appear on a finished heat: %s", ctx)
	}

	// And a RACE keeps its positions and its FINAL RESULT — the withholding is
	// TT-only.
	race := ddapi.Race{Label: "Race 1", TypeLabel: "Race", TargetLaps: 2, State: "ended",
		Pilots: []ddapi.Pilot{{Name: "A", Position: 1, Total: 25.0, Laps: []float64{12, 13}}}}
	if ctx := RaceContext(race, mv, false); !strings.Contains(ctx, "position 1") ||
		!strings.Contains(ctx, "FINAL RESULT") {
		t.Fatalf("a race must keep positions and the final result: %s", ctx)
	}
}

func TestBestWindowSum(t *testing.T) {
	if got := bestWindowSum([]float64{12, 10, 10.5, 11}, 2); got != 20.5 {
		t.Fatalf("best 2-window of 12,10,10.5,11 is 20.5, got %v", got)
	}
	if got := bestWindowSum([]float64{12}, 2); got != 0 {
		t.Fatalf("a run shorter than the window has no consec, got %v", got)
	}
	if got := bestWindowSum(nil, 0); got != 0 {
		t.Fatalf("zero window is no window, got %v", got)
	}
}
