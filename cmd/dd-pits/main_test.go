package main

import "testing"

// TestPersistCue covers the "run a future race out of sequence and keep it on the
// dashboard while reviewing an earlier race" behaviour: a cued un-run heat stays
// selected even when FPVTrackside's /current goes null (the operator clicked an
// already-run heat to review it), and drops once the cued heat runs or a different
// un-run heat is cued.
func TestPersistCue(t *testing.T) {
	// pending set: which heats are un-run this tick.
	pend := func(ids ...string) func(string) bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return func(id string) bool { return m[id] }
	}

	cases := []struct {
		name           string
		cued, raw      string
		pending        func(string) bool
		wantCued, want string
	}{
		// Operator cues an un-run future heat → it becomes the cue + selection.
		{"fresh cue", "", "C", pend("C", "D"), "C", "C"},
		// Reviewing an already-run heat (raw empty) keeps the still-un-run cue.
		{"persist through review", "C", "", pend("C"), "C", "C"},
		// Still persists across several reviews while the cue is un-run.
		{"persist again", "C", "", pend("C", "E"), "C", "C"},
		// Operator cues a different un-run heat → the cue updates.
		{"recue replaces", "C", "E", pend("C", "E"), "E", "E"},
		// The cued heat is now running (no longer pending) → drop it; the running
		// heat is shown by the normal running-heat path, not the cue.
		{"cue started → drop", "C", "C", pend(), "", ""},
		// No cue and nothing un-run selected → no selection (board falls back to
		// next-up / on-deck).
		{"no cue", "", "", pend(), "", ""},
		// A running heat is selected with no prior cue → not treated as a cue.
		{"running selection, no cue", "", "R", pend(), "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCued, gotSel := persistCue(tc.cued, tc.raw, tc.pending)
			if gotCued != tc.wantCued || gotSel != tc.want {
				t.Fatalf("persistCue(%q,%q) = (%q,%q), want (%q,%q)",
					tc.cued, tc.raw, gotCued, gotSel, tc.wantCued, tc.want)
			}
		})
	}
}
