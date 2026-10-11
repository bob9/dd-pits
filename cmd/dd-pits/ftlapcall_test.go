package main

import "testing"

// A lap must be spoken exactly once, whichever path sees it first.
//
// Both remain live: the push calls it early, the poll finds it moments later.
// They agree only through lapsCalled, so this is the whole safety of the
// cutover — get it wrong and the booth talks over itself on every lap.
func TestClaimLapSpeaksALapOnce(t *testing.T) {
	rc := &RaceCaller{lapsCalled: map[string]int{}}

	if !rc.claimLap("race1", "bob9", 1) {
		t.Fatal("the first claim on a lap was refused")
	}
	// The poller arriving second must find nothing to say.
	if rc.claimLap("race1", "bob9", 1) {
		t.Error("the same lap was claimed twice — it would be announced twice")
	}
	// The poller's own counter must agree, or it re-announces on its next pass.
	if got := rc.lapsCalled["race1\x00bob9"]; got != 1 {
		t.Errorf("lapsCalled = %d, want 1 — the poll would replay this lap", got)
	}

	// A later lap is genuinely new.
	if !rc.claimLap("race1", "bob9", 2) {
		t.Error("lap 2 was refused after lap 1")
	}
	// An older lap arriving late is not.
	if rc.claimLap("race1", "bob9", 1) {
		t.Error("a stale lap 1 was claimed after lap 2 — it would air out of order")
	}

	// Pilots and heats are independent.
	if !rc.claimLap("race1", "Willman", 1) {
		t.Error("one pilot's lap blocked another's")
	}
	if !rc.claimLap("race2", "bob9", 1) {
		t.Error("a lap in one heat blocked the same lap number in another")
	}
}

// A gap in the push must not leave the poller replaying old laps behind the
// current one. Claiming lap 5 means laps 1-5 are done, not that lap 1 is.
func TestClaimLapSkipsRatherThanReplays(t *testing.T) {
	rc := &RaceCaller{lapsCalled: map[string]int{}}
	if !rc.claimLap("r", "p", 5) {
		t.Fatal("lap 5 was refused")
	}
	for lap := 1; lap <= 5; lap++ {
		if rc.claimLap("r", "p", lap) {
			t.Errorf("lap %d was claimable after lap 5 — the poll would air it late", lap)
		}
	}
}
