package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/elrs"
)

// elrsTestRig wires an elrsController to a FakeBackpack, seeded with a cloud
// config mapping the test pilots (Alice/Bob/Carol) to phrase-derived UIDs —
// only configured pilots are ever messaged (no callsign fallback), so the
// rig must configure them like the cloud would.
func elrsTestRig(t *testing.T) (*elrsController, *elrs.FakeBackpack, context.CancelFunc) {
	t.Helper()
	fake, tr := elrs.NewFakeBackpack()
	var c *elrsController // OnPacket routes inbound frames, like main.go wires it
	bp := elrs.NewBackpack(elrs.BackpackConfig{
		FrameDelay: time.Microsecond,
		Dial: func(context.Context) (elrs.Transport, string, error) {
			return tr, "fake", nil
		},
		OnPacket: func(p elrs.Packet) {
			if c != nil {
				c.handleInbound(p)
			}
		},
	})
	c = newELRSController(bp)
	c.rearmDelay = 10 * time.Millisecond // don't stall tests on the 5s deferral
	ctx, cancel := context.WithCancel(context.Background())
	go bp.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !bp.Connected() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("backpack never connected")
		}
		time.Sleep(time.Millisecond)
	}
	c.cfg.Store(&elrs.ConfigResponse{
		DefaultLayout: elrs.DefaultLayout(),
		Pilots: map[string]elrs.ConfigPilot{
			"ALICE": {UID: elrs.UIDFromPhrase("Alice").String()},
			"BOB":   {UID: elrs.UIDFromPhrase("Bob").String()},
			"CAROL": {UID: elrs.UIDFromPhrase("Carol").String()},
		},
	})
	// Keep the redundant-send window instant in tests.
	c.channelRepeats = 2
	c.channelRepeatGap = time.Millisecond
	return c, fake, cancel
}

// raceSnap builds a two-pilot 3-lap race snapshot at a given stage.
// aliceLaps/bobLaps are racing-lap times; the holeshot is added when any laps
// exist. started/ended control Start/End stamps.
func raceSnap(t *testing.T, selected string, started, ended bool, aliceLaps, bobLaps []float64) dronedash.Snapshot {
	t.Helper()

	mkLaps := func(pilot string, laps []float64) (string, string) {
		if len(laps) == 0 {
			return "", ""
		}
		var dets, lapEntries []string
		dets = append(dets, fmt.Sprintf(`{"ID":"%s-d0","Pilot":"%s","Valid":true}`, pilot, pilot))
		lapEntries = append(lapEntries, fmt.Sprintf(`{"Detection":"%s-d0","LapNumber":0,"LengthSeconds":2.0}`, pilot))
		for i, l := range laps {
			dets = append(dets, fmt.Sprintf(`{"ID":"%s-d%d","Pilot":"%s","Valid":true}`, pilot, i+1, pilot))
			lapEntries = append(lapEntries, fmt.Sprintf(`{"Detection":"%s-d%d","LapNumber":%d,"LengthSeconds":%v}`, pilot, i+1, i+1, l))
		}
		return strings.Join(dets, ","), strings.Join(lapEntries, ",")
	}

	aDets, aLaps := mkLaps("p1", aliceLaps)
	bDets, bLaps := mkLaps("p2", bobLaps)
	var dets, laps []string
	if aDets != "" {
		dets = append(dets, aDets)
		laps = append(laps, aLaps)
	}
	if bDets != "" {
		dets = append(dets, bDets)
		laps = append(laps, bLaps)
	}

	startStr := ""
	if started {
		startStr = `"Start":"2026-06-05T09:00:00Z",`
	}
	endStr := ""
	if ended {
		endStr = `"End":"2026-06-05T09:05:00Z",`
	}
	selStr := ""
	if selected != "" {
		selStr = fmt.Sprintf(`"selected_race_id":%q,`, selected)
	}

	raw := fmt.Sprintf(`{
	  %s
	  "event":{"ID":"e1","Name":"Club Night","EventType":"Race","Laps":3,
	           "Channels":["c1","c2"],"ChannelColors":["#ff0000","#00ff00"],"Rounds":["r1"],"Races":["race1"]},
	  "channels":[{"ID":"c1","ShortBand":"R","Number":1,"DisplayName":"R1"},
	              {"ID":"c2","ShortBand":"R","Number":2,"DisplayName":"R2"}],
	  "pilots":[{"ID":"p1","Name":"Alice"},{"ID":"p2","Name":"Bob"}],
	  "rounds":[{"ID":"r1","RoundNumber":1,"EventType":"Race","Valid":true}],
	  "races":[{"ID":"race1","Round":"r1","RaceNumber":1,"TargetLaps":3,"Valid":true,
	    %s%s
	    "PilotChannels":[{"ID":"pc1","Pilot":"p1","Channel":"c1"},{"ID":"pc2","Pilot":"p2","Channel":"c2"}],
	    "Detections":[%s],
	    "Laps":[%s]}]}`,
		selStr, startStr, endStr, strings.Join(dets, ","), strings.Join(laps, ","))

	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal test snapshot: %v", err)
	}
	return snap
}

func waitForRow(t *testing.T, fake *elrs.FakeBackpack, uid elrs.UID, row int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := fake.Row(uid, row)
		if strings.Contains(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %d = %q, want contains %q", row, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForRowEmpty(t *testing.T, fake *elrs.FakeBackpack, uid elrs.UID, row int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fake.Row(uid, row) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %d = %q, want empty", row, fake.Row(uid, row))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestEngineRaceLifecycle drives a full heat: prime → stage → start → laps →
// pilot done → race end → re-run clear, asserting each pilot's fake goggle
// screen at every step. Rows come from elrs.DefaultLayout(): status=5,
// lap_position=0, lap_result=15, heat=2, results=13.
func TestEngineRaceLifecycle(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")
	bob := elrs.UIDFromPhrase("Bob")

	// Prime: first snapshot must not touch any goggles.
	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))
	if n := len(fake.Frames()); n > 2 { // allow the version probe exchange
		t.Fatalf("priming snapshot sent %d frames", n)
	}

	// Stage: operator cues race1 → heat name shows, but NO ARM NOW. The
	// staging arming prompt was removed (FPVTrackside cues heats long before
	// the countdown, so it fired far too early); race_status stays empty until
	// GO! at start.
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil))
	waitForRow(t, fake, alice, 2, "ROUND 1")
	waitForRow(t, fake, bob, 2, "ROUND 1")
	if got := fake.Row(alice, 5); got != "" {
		t.Fatalf("staging showed race-status %q on alice, want empty (ARM NOW removed)", got)
	}
	if got := fake.Row(bob, 5); got != "" {
		t.Fatalf("staging showed race-status %q on bob, want empty (ARM NOW removed)", got)
	}

	// Start: pending → running clears the screen and shows GO!.
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	waitForRow(t, fake, alice, 5, "GO!")
	waitForRowEmpty(t, fake, alice, 2) // stage heat name cleared by clear-screen

	// Lap 1 for Alice only.
	c.processSnapshot(raceSnap(t, "race1", true, false, []float64{30}, nil))
	waitForRow(t, fake, alice, 0, "LAP: 2")
	waitForRow(t, fake, alice, 15, "0:30.00")
	if got := fake.Row(bob, 0); got != "" {
		t.Fatalf("bob got a lap message %q without a lap", got)
	}

	// Bob lap 1 (slower): gap line should show ALICE ahead.
	c.processSnapshot(raceSnap(t, "race1", true, false, []float64{30}, []float64{35}))
	waitForRow(t, fake, bob, 0, "LAP: 2")
	waitForRow(t, fake, bob, 14, "ALICE")
	waitForRow(t, fake, bob, 14, "+0:05.00")

	// Alice finishes all 3 laps → FINISHED + placement.
	c.processSnapshot(raceSnap(t, "race1", true, false, []float64{30, 31, 32}, []float64{35}))
	waitForRow(t, fake, alice, 5, "FINISHED!")
	waitForRow(t, fake, alice, 13, "PLACEMENT: 1")
	waitForRow(t, fake, alice, 14, "TOTAL TIME:")

	// Race ends → Bob (not done) told to land; Alice already finished.
	c.processSnapshot(raceSnap(t, "", true, true, []float64{30, 31, 32}, []float64{35, 36}))
	waitForRow(t, fake, bob, 5, "LAND NOW!")

	// Re-run: heat reset to pending → both screens wiped.
	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))
	waitForRowEmpty(t, fake, alice, 5)
	waitForRowEmpty(t, fake, bob, 5)
	waitForRowEmpty(t, fake, alice, 13)
}

// TestEngineLapJumpEmitsOnce ensures a multi-lap jump between polls emits a
// single (newest) lap message rather than replaying each missed lap.
func TestEngineLapJumpEmitsOnce(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil)) // prime
	c.processSnapshot(raceSnap(t, "", true, false, nil, nil))  // start
	// Jump straight from 0 to 2 laps.
	c.processSnapshot(raceSnap(t, "", true, false, []float64{30, 31}, nil))
	waitForRow(t, fake, alice, 0, "LAP: 3")

	lapWrites := 0
	for _, f := range fake.Frames() {
		if f.Function == elrs.FuncSetOSD && len(f.Payload) > 4 && f.Payload[0] == elrs.OSDSubText &&
			f.Payload[1] == 0 && strings.Contains(string(f.Payload[4:]), "LAP") {
			lapWrites++
		}
	}
	if lapWrites != 1 {
		t.Fatalf("lap jump produced %d lap-row writes, want 1", lapWrites)
	}
}

// TestPilotOSDConfiguredPhraseRequired pins the no-fallback rule: only pilots
// with a cloud-configured bind phrase resolve. A pilot absent from the config
// — never linked a phrase, or opted out via the OSD master switch (the server
// drops them from Pilots) — is never messaged. The old callsign-derived-UID
// fallback is gone: pilots don't bind their goggles to their callsign, so
// those guessed UIDs were dead air that burned the backpack's retry budget
// (and once messaged a pilot who had explicitly opted out — mirellafpv).
func TestPilotOSDConfiguredPhraseRequired(t *testing.T) {
	c := &elrsController{}

	// No config at all (e.g. local-only mode): nobody resolves.
	if _, _, ok := c.pilotOSD("Alice"); ok {
		t.Fatal("pilotOSD resolved a pilot with no config loaded")
	}

	alice := elrs.UIDFromPhrase("alice-phrase")
	c.cfg.Store(&elrs.ConfigResponse{
		EventID:       1,
		DefaultLayout: elrs.DefaultLayout(),
		Pilots:        map[string]elrs.ConfigPilot{"ALICE": {UID: alice.String()}},
		OptOut:        []string{"MIRELLAFPV"},
	})

	// Configured pilot resolves (case-insensitive) to her phrase-derived UID.
	uid, _, ok := c.pilotOSD("alice")
	if !ok || uid != alice {
		t.Fatalf("configured pilot: uid=%s ok=%v, want %s", uid, ok, alice)
	}
	// Opted-out and never-configured pilots never resolve — no guessed UIDs.
	if _, _, ok := c.pilotOSD("mirellafpv"); ok {
		t.Fatal("opted-out pilot resolved — she would still be messaged")
	}
	if _, _, ok := c.pilotOSD("RandoFPV"); ok {
		t.Fatal("unconfigured pilot resolved — callsign fallback should be gone")
	}
}

// TestEngineUnmappedPilotSkipped: with no pilots in the cloud config, no
// pilot resolves and the backpack must stay silent.
func TestEngineUnmappedPilotSkipped(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()
	c.cfg.Store(&elrs.ConfigResponse{DefaultLayout: elrs.DefaultLayout()})

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil))
	c.processSnapshot(raceSnap(t, "race1", true, false, []float64{30}, nil))

	time.Sleep(50 * time.Millisecond)
	for _, f := range fake.Frames() {
		if f.Function == elrs.FuncSetOSD {
			t.Fatalf("OSD frame sent despite no mapped pilots")
		}
	}
}

// TestEngineGapBehind drives the chaser element (off by default, enabled here
// via the cloud layout): the pilot ahead sees who's behind and by how much,
// refreshed BOTH on their own crossings and — the anxiety moment — the instant
// the chaser crosses the line.
func TestEngineGapBehind(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	layout := elrs.DefaultLayout()
	gp := layout.Elements[elrs.ElemGapBehind]
	gp.Enabled = true
	layout.Elements[elrs.ElemGapBehind] = gp
	c.cfg.Store(&elrs.ConfigResponse{
		DefaultLayout: layout,
		Pilots: map[string]elrs.ConfigPilot{
			"ALICE": {UID: elrs.UIDFromPhrase("Alice").String()},
			"BOB":   {UID: elrs.UIDFromPhrase("Bob").String()},
		},
	})
	alice := elrs.UIDFromPhrase("Alice")
	bob := elrs.UIDFromPhrase("Bob")
	const gapRow = 12 // ElemGapBehind default row

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil)) // prime
	c.processSnapshot(raceSnap(t, "", true, false, nil, nil))  // start

	// Alice laps alone: Bob has no comparable lap yet, so no chaser line.
	c.processSnapshot(raceSnap(t, "", true, false, []float64{30}, nil))
	waitForRow(t, fake, alice, 0, "LAP: 2")
	if got := fake.Row(alice, gapRow); got != "" {
		t.Fatalf("gap-behind %q shown before the chaser had a lap", got)
	}

	// Bob crosses 5s later: ALICE's goggles get the chaser push, even though
	// she didn't lap this snapshot. Bob (last place) shows no chaser line.
	c.processSnapshot(raceSnap(t, "", true, false, []float64{30}, []float64{35}))
	waitForRow(t, fake, alice, gapRow, "BOB")
	waitForRow(t, fake, alice, gapRow, "-0:05.00")
	if got := fake.Row(bob, gapRow); got != "" {
		t.Fatalf("last-place pilot got a chaser line %q", got)
	}
}

// TestEngineLapUsesOneSession pins the queue-latency fix: a lap event pushes
// every enabled OSD element (lap/position, lap result, recent laps, gap) in
// ONE backpack session — exactly one display commit and one set-uid/reset-uid
// exchange — instead of a session per element. Extra sessions multiply the
// frames the netpack must clock out over ESP-NOW per lap, which is what was
// queueing OSD messages behind each other at the field.
func TestEngineLapUsesOneSession(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil)) // prime
	c.processSnapshot(raceSnap(t, "", true, false, nil, nil))  // start
	before := waitFramesQuiet(t, fake)                         // the start's own sessions finish first (slow CI runners)

	// One lap for Alice, with Bob also lapping so Alice gets a gap line too.
	c.processSnapshot(raceSnap(t, "", true, false, []float64{31}, []float64{30}))
	waitForRow(t, fake, alice, 0, "LAP: 2")

	displays, uidSets := 0, 0
	frames := fake.Frames()
	for _, f := range frames[before:] {
		switch {
		case f.Function == elrs.FuncSetOSD && len(f.Payload) > 0 && f.Payload[0] == elrs.OSDSubDisplay:
			displays++
		case f.Function == elrs.FuncSetSendUID && len(f.Payload) > 0 && f.Payload[0] == 0x01:
			uidSets++
		}
	}
	// Two pilots lapped → one session each.
	if displays != 2 {
		t.Fatalf("lap event committed %d displays, want 2 (one session per pilot)", displays)
	}
	if uidSets != 2 {
		t.Fatalf("lap event set the send UID %d times, want 2 (one session per pilot)", uidSets)
	}
}

// waitFramesQuiet waits until no new frames have arrived for 100ms and
// returns the frame count.
func waitFramesQuiet(t *testing.T, fake *elrs.FakeBackpack) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	n := len(fake.Frames())
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		m := len(fake.Frames())
		if m == n {
			return n
		}
		n = m
	}
	t.Fatal("frames never stopped arriving")
	return n
}

// withRaceStart replaces the heat's start stamp — a re-run gets a new one
// from FPVTrackside.
func withRaceStart(snap dronedash.Snapshot, start string) dronedash.Snapshot {
	snap.Races[0].Start = start
	return snap
}

// countOSDText counts OSD text frames addressed to uid containing want,
// following the send-UID targeting the same way the backpack does.
func countOSDText(fake *elrs.FakeBackpack, uid elrs.UID, want string) int {
	n := 0
	var target elrs.UID
	targeted := false
	for _, f := range fake.Frames() {
		if f.Function == elrs.FuncSetSendUID {
			targeted = len(f.Payload) == 7 && f.Payload[0] == 0x01
			if targeted {
				copy(target[:], f.Payload[1:])
			}
			continue
		}
		if !targeted || target != uid {
			continue
		}
		if f.Function == elrs.FuncSetOSD && len(f.Payload) > 4 && f.Payload[0] == elrs.OSDSubText &&
			strings.Contains(string(f.Payload[4:]), want) {
			n++
		}
	}
	return n
}

// TestEngineCoalescedRerunClearsScreen pins the bug bob9 hit at the field: a
// re-run whose pending window fell between two polls arrives as an
// ended→running edge, which used to match no case at all — the pilot flew the
// whole re-run with the previous run's FINISHED/PLACEMENT screen over their
// video, and their laps were swallowed too (the old lap/done counts said
// nothing was new). The clear+GO! must fire on ANY start with a new stamp.
func TestEngineCoalescedRerunClearsScreen(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))     // prime (pending)
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil)) // start
	waitForRow(t, fake, alice, 5, "GO!")
	c.processSnapshot(raceSnap(t, "race1", true, false, []float64{30, 31, 32}, nil)) // Alice finishes
	waitForRow(t, fake, alice, 13, "PLACEMENT: 1")
	c.processSnapshot(raceSnap(t, "", true, true, []float64{30, 31, 32}, nil)) // heat ends

	// Re-run, with the pending snapshot coalesced away: straight back to
	// running, no laps, new start stamp.
	rerun := func(a []float64) dronedash.Snapshot {
		return withRaceStart(raceSnap(t, "race1", true, false, a, nil), "2026-06-05T09:20:00Z")
	}
	c.processSnapshot(rerun(nil))

	waitForRow(t, fake, alice, 5, "GO!")
	waitForRowEmpty(t, fake, alice, 13) // last run's placement wiped by the clear

	// …and the re-run's laps reach her: the previous run's lap count and her
	// done flag must have been reset with the start.
	c.processSnapshot(rerun([]float64{29}))
	waitForRow(t, fake, alice, 0, "LAP: 2")
}

// TestEngineStaleBounceDoesNotRestart guards the other side of that change: a
// heat that merely flips ended→running with the SAME start stamp (the
// stale-heat grace expiring, then a late lap landing) must NOT fire a second
// GO! over a race already in progress.
func TestEngineStaleBounceDoesNotRestart(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))     // prime
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil)) // start
	waitForRow(t, fake, alice, 5, "GO!")

	// Reads as ended (no more laps for a while), then running again on the
	// same start stamp.
	c.processSnapshot(raceSnap(t, "race1", true, true, []float64{30}, nil))
	waitForRow(t, fake, alice, 5, "LAND NOW!")
	c.processSnapshot(raceSnap(t, "race1", true, false, []float64{30}, nil))

	time.Sleep(50 * time.Millisecond)
	if n := countOSDText(fake, alice, "GO!"); n != 1 {
		t.Fatalf("GO! written %d times, want 1 (the real start only)", n)
	}
}

// TestEngineMissedGoRetried: a start-edge clear+GO! whose session failed
// outright (backpack mid-reconnect at the start tone) put nothing on the air,
// so no delivery report can drive a resend. The next snapshot of the still-
// running heat must retry it — and stop once it lands.
func TestEngineMissedGoRetried(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))     // prime
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil)) // start
	waitForRow(t, fake, alice, 5, "GO!")
	if n := countOSDText(fake, alice, "GO!"); n != 1 {
		t.Fatalf("start wrote GO! %d times, want 1", n)
	}

	// Alice's send failed: her goggles still carry the last heat's screen.
	c.markGoMissed(alice, "race1")
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))

	deadline := time.Now().Add(5 * time.Second)
	for countOSDText(fake, alice, "GO!") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("missed GO! was never retried")
		}
		time.Sleep(time.Millisecond)
	}

	// It landed — later snapshots must not keep re-firing it.
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil))
	time.Sleep(50 * time.Millisecond)
	if n := countOSDText(fake, alice, "GO!"); n != 2 {
		t.Fatalf("GO! written %d times, want 2 (start + one retry)", n)
	}
}

// TestGoSessionLostSemantics pins the loss signal the GO! resend loop uses.
// The old rule ("fewer than 3 SetOSD frames acked") never matched reality —
// the netpack acks 2 frames for a perfectly delivered GO! session — so every
// start at the July 2026 Rebels meeting fired the full resend chain (36 of 47
// starts, all reporting delivered). Only a FAILED frame, or total silence,
// means the pilot missed it.
func TestGoSessionLostSemantics(t *testing.T) {
	cases := []struct {
		name       string
		ok, bad    int
		wantResend bool
	}{
		{"netpack acks 2 of 3 frames — delivered", 2, 0, false},
		{"fully acked", 3, 0, false},
		{"a frame came back failed", 2, 1, true},
		{"nothing acked at all", 0, 0, true},
		{"all frames failed", 0, 3, true},
	}
	for _, tc := range cases {
		if got := goSessionLost(tc.ok, tc.bad); got != tc.wantResend {
			t.Errorf("%s: goSessionLost(%d,%d)=%v, want %v", tc.name, tc.ok, tc.bad, got, tc.wantResend)
		}
	}
}

// TestEngineStartSendsGoOnce: with delivery reports flowing and every frame
// acked, a race start must put ONE GO! session on the air per pilot. Three
// (the old behaviour) tripled the frames in the worst RF seconds of the day.
func TestEngineStartSendsGoOnce(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()
	fake.AutoReport(true)

	alice := elrs.UIDFromPhrase("Alice")
	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))     // prime
	c.processSnapshot(raceSnap(t, "race1", true, false, nil, nil)) // start
	waitForRow(t, fake, alice, 5, "GO!")

	time.Sleep(goResendCheck + 400*time.Millisecond) // past the resend check
	if n := countOSDText(fake, alice, "GO!"); n != 1 {
		t.Fatalf("start put %d GO! sessions on the air, want 1", n)
	}
}

// TestGoResendRearmsBannerClear is bob9's symptom: a resend rewrites the
// status row, which bumps its generation and cancels the clear the first
// attempt scheduled — so "GO!" stayed on the OSD for the whole race. Every
// attempt must re-arm its own clear.
func TestGoResendRearmsBannerClear(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()

	alice := elrs.UIDFromPhrase("Alice")
	c.processSnapshot(raceSnap(t, "", false, false, nil, nil)) // prime
	start := raceSnap(t, "race1", true, false, nil, nil)
	c.processSnapshot(start) // attempt 0
	waitForRow(t, fake, alice, 5, "GO!")

	// The report-driven resend, ~1.2s later.
	mv := buildMeeting(start)
	uid, layout, ok := c.pilotOSD("Alice")
	if !ok {
		t.Fatal("Alice did not resolve")
	}
	c.sendGo(&mv.Races[0], "Alice", uid, layout, 1)
	waitForRow(t, fake, alice, 5, "GO!")

	// startUptime after the LAST write, the banner must come down.
	waitForRowEmpty(t, fake, alice, 5)
}
