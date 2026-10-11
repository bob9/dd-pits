package main

import (
	"reflect"
	"testing"

	"github.com/bob9/dd-pits/internal/elrs"
)

// TestRecentLapsFlowAroundBlockedRows pins the field bug + its fix: the
// recent-laps list must never overwrite rows other enabled elements own
// (the lap-time row vanished from lap 2), and it must FLOW AROUND them so
// the list and the lap-time/gap lines all render at the same time.
func TestRecentLapsFlowAroundBlockedRows(t *testing.T) {
	layout := layoutWith(map[string]elrs.ElementPos{
		elrs.ElemRecentLaps: {Enabled: true, Row: 13, NumLaps: 5},
		elrs.ElemGapResult:  {Enabled: true, Row: 13, Center: true}, // on the anchor
		elrs.ElemLapResult:  {Enabled: true, Row: 15, Center: true}, // mid-window
	})
	blocked := otherElementRows(layout, elrs.ElemRecentLaps)

	// 18 rows (0..17): anchor 13 is blocked (gap), 15 blocked (lap time) —
	// the 5 requested lines flow into 14, 16, 17 and the rest don't fit.
	rows := assignRecentLapRows(5, 13, blocked)
	if want := []int{14, 16, 17}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("assigned rows = %v, want %v", rows, want)
	}

	// No collisions: a clear run below the anchor takes contiguous rows.
	clear := otherElementRows(layoutWith(map[string]elrs.ElementPos{
		elrs.ElemRecentLaps: {Enabled: true, Row: 10, NumLaps: 3},
	}), elrs.ElemRecentLaps)
	if rows := assignRecentLapRows(3, 10, clear); !reflect.DeepEqual(rows, []int{10, 11, 12}) {
		t.Fatalf("contiguous rows = %v, want [10 11 12]", rows)
	}

	// Screen edge still truncates.
	if rows := assignRecentLapRows(4, elrs.OSDRows-2, map[int]bool{}); len(rows) != 2 {
		t.Fatalf("edge rows = %v, want 2", rows)
	}
}
