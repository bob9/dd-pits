package main

import (
	"reflect"
	"testing"

	"github.com/bob9/dd-pits/internal/elrs"
)

func layoutWith(elems map[string]elrs.ElementPos) elrs.Layout {
	return elrs.Layout{Version: elrs.LayoutVersion, Elements: elems}
}

// TestStaleLayoutRows pins the "hidden means gone" diff: rows an edit
// vacated are returned, rows still owned by any enabled element are not.
func TestStaleLayoutRows(t *testing.T) {
	base := map[string]elrs.ElementPos{
		elrs.ElemRaceStatus: {Enabled: true, Row: 5, Center: true},
		elrs.ElemLapResult:  {Enabled: true, Row: 7, Center: true},
	}

	t.Run("element hidden → its row is stale", func(t *testing.T) {
		after := map[string]elrs.ElementPos{
			elrs.ElemRaceStatus: {Enabled: true, Row: 5, Center: true},
			elrs.ElemLapResult:  {Enabled: false, Row: 7, Center: true},
		}
		if got := staleLayoutRows(layoutWith(base), layoutWith(after)); !reflect.DeepEqual(got, []int{7}) {
			t.Fatalf("stale rows = %v, want [7]", got)
		}
	})

	t.Run("element moved → old row stale, new row not", func(t *testing.T) {
		after := map[string]elrs.ElementPos{
			elrs.ElemRaceStatus: {Enabled: true, Row: 5, Center: true},
			elrs.ElemLapResult:  {Enabled: true, Row: 9, Center: true},
		}
		if got := staleLayoutRows(layoutWith(base), layoutWith(after)); !reflect.DeepEqual(got, []int{7}) {
			t.Fatalf("stale rows = %v, want [7]", got)
		}
	})

	t.Run("row taken over by another element → not stale", func(t *testing.T) {
		after := map[string]elrs.ElementPos{
			elrs.ElemRaceStatus: {Enabled: true, Row: 7, Center: true}, // moved onto 7
			elrs.ElemLapResult:  {Enabled: false, Row: 7, Center: true},
		}
		if got := staleLayoutRows(layoutWith(base), layoutWith(after)); !reflect.DeepEqual(got, []int{5}) {
			t.Fatalf("stale rows = %v, want [5] (7 is still owned)", got)
		}
	})

	t.Run("recent laps hidden → whole window stale", func(t *testing.T) {
		before := map[string]elrs.ElementPos{
			elrs.ElemRecentLaps: {Enabled: true, Row: 10, NumLaps: 4},
		}
		after := map[string]elrs.ElementPos{
			elrs.ElemRecentLaps: {Enabled: false, Row: 10, NumLaps: 4},
		}
		if got := staleLayoutRows(layoutWith(before), layoutWith(after)); !reflect.DeepEqual(got, []int{10, 11, 12, 13}) {
			t.Fatalf("stale rows = %v, want [10 11 12 13]", got)
		}
	})

	t.Run("no change → nothing stale", func(t *testing.T) {
		if got := staleLayoutRows(layoutWith(base), layoutWith(base)); len(got) != 0 {
			t.Fatalf("stale rows = %v, want none", got)
		}
	})
}
