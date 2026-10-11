package main

import (
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/elrs"
)

// TestClockUIDs pins which goggles the clock sync targets: every
// cloud-configured pilot's UID and nothing else — bad UIDs dropped, and
// board pilots without a configured phrase are NOT synced (no
// callsign-derived guessing; those UIDs were dead air).
func TestClockUIDs(t *testing.T) {
	c, _, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("alice-phrase")
	c.cfg.Store(&elrs.ConfigResponse{
		Pilots: map[string]elrs.ConfigPilot{
			"ALICE":  {UID: alice.String()},
			"BROKEN": {UID: "not-hex"},
		},
		OptOut: []string{"CAROL"},
	})
	c.mu.Lock()
	c.lastMeeting = &meetingView{Board: []ddapi.Standing{
		{Name: "Alice"}, // configured — synced via her config entry
		{Name: "Bob"},   // on the board but no phrase — never synced
		{Name: "Carol"}, // opted out — never synced
		{Name: "—"},     // placeholder — skipped
	}}
	c.mu.Unlock()

	targets := c.clockTargets()
	want := map[elrs.UID]bool{alice: true}
	if len(targets) != len(want) {
		t.Fatalf("clockTargets() = %d targets %v, want %d", len(targets), targets, len(want))
	}
	for _, tgt := range targets {
		if !want[tgt.uid] {
			t.Errorf("clockTargets() unexpectedly targets %s (%s)", tgt.uid, tgt.callsign)
		}
	}
}

// TestClockBatchSize pins the trickle rate: the whole field must be covered
// within one resync interval, a few pilots at a time.
func TestClockBatchSize(t *testing.T) {
	ticks := int(clockResyncInterval / clockPollInterval)
	cases := []struct{ total, want int }{
		{0, 1}, {1, 1}, {5, 1}, {30, 1}, {31, 2}, {60, 2}, {90, 3}, {200, 7},
	}
	for _, tc := range cases {
		if got := clockBatchSize(tc.total); got != tc.want {
			t.Errorf("clockBatchSize(%d) = %d, want %d", tc.total, got, tc.want)
		}
		if tc.total > 0 {
			if covered := clockBatchSize(tc.total) * ticks; covered < tc.total {
				t.Errorf("clockBatchSize(%d)*%d ticks = %d, does not cover the field", tc.total, ticks, covered)
			}
		}
	}
}
