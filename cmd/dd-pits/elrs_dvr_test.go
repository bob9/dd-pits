package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/elrs"
)

// TestDVRLabel pins the recording-label format the goggles receive.
func TestDVRLabel(t *testing.T) {
	cases := []struct {
		name  string
		event string
		race  vmRace
		want  string
	}{
		{"qualifying", "Winter Cup", vmRace{IsTT: true, Round: 1, Number: 2}, "WinterCup-Q1-H2"},
		{"race", "Winter Cup", vmRace{IsTT: false, Round: 3, Number: 1}, "WinterCup-R3-H1"},
		{"no event name", "", vmRace{IsTT: true, Round: 4, Number: 3}, "Q4-H3"},
		{"punctuation stripped", "CMRC: Round #4!", vmRace{IsTT: false, Round: 1, Number: 1}, "CMRCRound4-R1-H1"},
		{
			"long event fits the 43-char cap",
			"Queensland Winter Championship Series",
			vmRace{IsTT: true, Round: 2, Number: 5},
			"QueenslandWinterChampionshipSeries-Q2-H5",
		},
		{"a main", "Winter Cup", vmRace{Round: 5, Number: 1, RoundName: "A Main"}, "WinterCup-Mains-A-R5-H1"},
		{"b main dashed", "Winter Cup", vmRace{Round: 5, Number: 2, RoundName: "B-Mains"}, "WinterCup-Mains-B-R5-H2"},
		{"cta", "Winter Cup", vmRace{Round: 6, Number: 3, RoundName: "CTA"}, "WinterCup-CTA-R6-H3"},
		{"cta beats mains letter", "Winter Cup", vmRace{Round: 6, Number: 1, RoundName: "A Main CTA"}, "WinterCup-CTA-R6-H1"},
		{"chase the ace spelled out", "Winter Cup", vmRace{Round: 6, Number: 2, RoundName: "Chase the Ace"}, "WinterCup-CTA-R6-H2"},
		{"plain round name ignored", "Winter Cup", vmRace{Round: 2, Number: 4, RoundName: "Round 2"}, "WinterCup-R2-H4"},
		{
			"long event capped with mains tag",
			"Queensland Winter Championship Series",
			vmRace{Round: 5, Number: 1, RoundName: "A Main"},
			"QueenslandWinterChampionshipS-Mains-A-R5-H1",
		},
	}
	for _, tc := range cases {
		if got := dvrLabel(tc.event, &tc.race); got != tc.want {
			t.Errorf("%s: dvrLabel(%q) = %q, want %q", tc.name, tc.event, got, tc.want)
		}
		if got := dvrLabel(tc.event, &tc.race); len(got) > elrs.DVRNameMaxLen {
			t.Errorf("%s: label %q exceeds the goggles' %d-char cap", tc.name, got, elrs.DVRNameMaxLen)
		}
	}
}

// waitForDVR polls the fake until a UID's last-received DVR label matches.
func waitForDVR(t *testing.T, fake *elrs.FakeBackpack, uid elrs.UID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := fake.DVRName(uid)
		if ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dvr name = %q (received=%v), want %q", got, ok, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDVRLabelLifecycle drives stage → start → stop and asserts the label is
// armed at stage and cleared at stop on every pilot's goggles.
func TestDVRLabelLifecycle(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")
	bob := elrs.UIDFromPhrase("Bob")

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil)) // prime, nothing cued
	if _, ok := fake.DVRName(alice); ok {
		t.Fatal("priming snapshot with no cued heat sent a DVR label")
	}

	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil)) // stage
	waitForDVR(t, fake, alice, "ClubNight-R1-H1")
	waitForDVR(t, fake, bob, "ClubNight-R1-H1")

	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil)) // start
	waitForDVR(t, fake, alice, "ClubNight-R1-H1")

	c.processSnapshot(raceSnap(t, "race1", true, true, nil, nil)) // stop
	waitForDVR(t, fake, alice, "")
	waitForDVR(t, fake, bob, "")
}

// TestDVRLabelArmedOnPrimeWithCuedHeat pins the agent-restart hole: dd-pits
// (re)started while a heat already sits cued must still arm that heat's DVR
// label — the cue edge happened before the agent was watching. Label only:
// no stage OSD is replayed onto the goggles.
func TestDVRLabelArmedOnPrimeWithCuedHeat(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil)) // first snapshot, heat cued
	waitForDVR(t, fake, alice, "ClubNight-R1-H1")
	if got := fake.Row(alice, 2); got != "" {
		t.Fatalf("priming replayed stage OSD %q onto the goggles", got)
	}
}

// TestDVRLabelArmedWhenStageEdgeCoalesced pins the coalescing hole: the cue
// change and the race start can land inside one poll gap (Observe keeps only
// the newest snapshot), so the cued race is already "running" when the cue
// edge is processed — the label must still be armed.
func TestDVRLabelArmedWhenStageEdgeCoalesced(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")
	c.processSnapshot(raceSnap(t, "", true, false, nil, nil)) // prime: race already running, nothing cued
	if _, ok := fake.DVRName(alice); ok {
		t.Fatal("priming snapshot sent a DVR label")
	}
	// Cue first seen while the race is running: no pending-stage, no start
	// edge — only the staging block's running branch can arm the label.
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	waitForDVR(t, fake, alice, "ClubNight-R1-H1")
}

// TestDVRLabelRearmedOnRerunWithoutRecue pins the re-run hole: a heat reset
// and started again without the operator re-cueing it (SelectedRaceID never
// changes) must get its label re-armed at the start edge.
func TestDVRLabelRearmedOnRerunWithoutRecue(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")
	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))      // prime
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil)) // stage
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))  // start
	c.processSnapshot(raceSnap(t, "race1", true, true, nil, nil))   // stop
	waitForDVR(t, fake, alice, "")

	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil)) // laps cleared (re-run)
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))  // started again, same cue
	waitForDVR(t, fake, alice, "ClubNight-R1-H1")
}

// countDVRFrames counts every SetDVRName frame the fake has received.
func countDVRFrames(fake *elrs.FakeBackpack) int {
	n := 0
	for _, f := range fake.Frames() {
		if f.Function == elrs.FuncSetDVRName {
			n++
		}
	}
	return n
}

func waitDVRFrames(t *testing.T, fake *elrs.FakeBackpack, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for countDVRFrames(fake) < want {
		if time.Now().After(deadline) {
			t.Fatalf("dvr frames = %d, want >= %d", countDVRFrames(fake), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// backdateLabel makes the active label look sent `age` ago.
func backdateLabel(c *elrsController, age time.Duration) {
	c.mu.Lock()
	c.labelSentAt = time.Now().Add(-age)
	c.mu.Unlock()
}

// TestDVRLabelRefreshCadence pins the two-tier refresh: hot (10s) while the
// heat is staged — the window where goggles power on after the cue (the race
// director's own workflow: select race in Trackside, THEN turn goggles on,
// test-arm at the blocks opens the recording before the start) — and relaxed
// (2min) once it runs and the link carries live OSD traffic.
func TestDVRLabelRefreshCadence(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	perDelivery := 2 * dvrSendRepeats // two mapped pilots x repeats

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))      // prime
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil)) // stage
	waitDVRFrames(t, fake, perDelivery)

	// Staged + 15s old: past the 10s staged cadence → re-sent.
	backdateLabel(c, 15*time.Second)
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil))
	waitDVRFrames(t, fake, 2*perDelivery)

	// Start: the start-edge re-arm sends again.
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	waitDVRFrames(t, fake, 3*perDelivery)

	// Running + 35s old: inside the 2min running cadence → NOT re-sent.
	backdateLabel(c, 35*time.Second)
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	time.Sleep(50 * time.Millisecond) // settle: no frames should arrive
	if n := countDVRFrames(fake); n != 3*perDelivery {
		t.Fatalf("running refresh fired inside 2min: %d frames, want %d", n, 3*perDelivery)
	}

	// Running + 130s old: past the running cadence → re-sent.
	backdateLabel(c, 130*time.Second)
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	waitDVRFrames(t, fake, 4*perDelivery)
}

// twoRaceSnap builds a snapshot with race1 (Alice+Bob) and race2 (Alice+
// Carol) in one round, for the pre-cue/shared-pilot test.
func twoRaceSnap(t *testing.T, selected string, r1Started, r1Ended bool) dronedash.Snapshot {
	t.Helper()
	startStr := ""
	if r1Started {
		startStr = `"Start":"2026-06-05T09:00:00Z",`
	}
	endStr := ""
	if r1Ended {
		endStr = `"End":"2026-06-05T09:05:00Z",`
	}
	selStr := ""
	if selected != "" {
		selStr = `"selected_race_id":"` + selected + `",`
	}
	raw := `{
	  ` + selStr + `
	  "event":{"ID":"e1","Name":"Club Night","EventType":"Race","Laps":3,
	           "Channels":["c1","c2"],"ChannelColors":["#ff0000","#00ff00"],"Rounds":["r1"],"Races":["race1","race2"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
	  "pilots":[{"ID":"p1","Name":"Alice"},{"ID":"p2","Name":"Bob"},{"ID":"p3","Name":"Carol"}],
	  "rounds":[{"ID":"r1","RoundNumber":1,"EventType":"Race","Valid":true}],
	  "races":[
	    {"ID":"race1","Round":"r1","RaceNumber":1,"TargetLaps":3,"Valid":true,
	     ` + startStr + endStr + `
	     "PilotChannels":[{"ID":"pc1","Pilot":"p1","Channel":"c1"},{"ID":"pc2","Pilot":"p2","Channel":"c2"}],
	     "Detections":[],"Laps":[]},
	    {"ID":"race2","Round":"r1","RaceNumber":2,"TargetLaps":3,"Valid":true,
	     "PilotChannels":[{"ID":"pc3","Pilot":"p1","Channel":"c1"},{"ID":"pc4","Pilot":"p3","Channel":"c2"}],
	     "Detections":[],"Laps":[]}]}`
	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal two-race snapshot: %v", err)
	}
	return snap
}

// TestDVRLabelRefreshDefersWhileHeatRuns pins the queue-contention guard: a
// pre-cued heat's staged refresh must never send while another heat is
// running — its likely-powered-off goggles burn the backpack's retry budget
// and would queue ahead of the live heat's OSD messages — and must resume
// once the track goes quiet.
func TestDVRLabelRefreshDefersWhileHeatRuns(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	c.processSnapshot(twoRaceSnap(t, "race1", true, false)) // prime: race1 running + cued
	waitDVRFrames(t, fake, 2)                               // race1 label armed (2 pilots x 1)

	c.processSnapshot(twoRaceSnap(t, "race2", true, false)) // race2 pre-cued mid-race
	waitDVRFrames(t, fake, 4)                               // race2 label armed (2 pilots x 1)

	// Stale label + race1 still running: the staged refresh must hold off.
	backdateLabel(c, 15*time.Second)
	c.processSnapshot(twoRaceSnap(t, "race2", true, false))
	time.Sleep(50 * time.Millisecond) // settle: no frames should arrive
	if n := countDVRFrames(fake); n != 4 {
		t.Fatalf("staged refresh sent during a running heat: %d frames, want 4", n)
	}

	// race1 ends: its stop clears Bob (1 frame; Alice shared with race2 is
	// skipped) and the deferred race2 refresh fires (2 frames).
	c.processSnapshot(twoRaceSnap(t, "race2", true, true))
	waitDVRFrames(t, fake, 7)
}

// TestDVRLabelStopSkipsPreCuedSharedPilot pins the back-to-back-heats hole:
// the operator cues heat 2 while heat 1 is still running, then heat 1 ends.
// Heat 1's stop-time clear must not wipe the fresh heat-2 label from a pilot
// flying both.
func TestDVRLabelStopSkipsPreCuedSharedPilot(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice") // in both heats
	bob := elrs.UIDFromPhrase("Bob")     // heat 1 only
	carol := elrs.UIDFromPhrase("Carol") // heat 2 only

	c.processSnapshot(twoRaceSnap(t, "race1", false, false)) // prime, race1 cued
	c.processSnapshot(twoRaceSnap(t, "race1", true, false))  // race1 starts
	waitForDVR(t, fake, alice, "ClubNight-R1-H1")
	waitForDVR(t, fake, bob, "ClubNight-R1-H1")

	c.processSnapshot(twoRaceSnap(t, "race2", true, false)) // race2 pre-cued mid-race
	waitForDVR(t, fake, alice, "ClubNight-R1-H2")
	waitForDVR(t, fake, carol, "ClubNight-R1-H2")

	c.processSnapshot(twoRaceSnap(t, "race2", true, true)) // race1 ends
	waitForDVR(t, fake, bob, "")
	// Alice keeps the race2 label she was just armed with.
	waitForDVR(t, fake, alice, "ClubNight-R1-H2")
	if got, _ := fake.DVRName(carol); got != "ClubNight-R1-H2" {
		t.Fatalf("carol's label = %q, want race2 label untouched", got)
	}
}
