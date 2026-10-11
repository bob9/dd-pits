package main

// Speaking a lap the moment FPVTrackside detects it, rather than when we notice.
//
// The poller reads Race.json, and Race.json is written when FPVTrackside gets
// round to it. Every figure we derive from it is internally consistent — the
// green edge, the holeshot and each lap all agree — because they are all read
// from the same file at the same staleness. That is why our own logs looked
// correct while the operator, counting out loud, heard the call four seconds
// after the lap.
//
// The push carries the lap time and number outright, so nothing has to be
// derived and there is nothing to wait for.
//
// DEDUP IS THE WHOLE PROBLEM
//
// Both paths remain live: the push speaks the lap, and the poll will see that
// same lap moments later and speak it AGAIN unless told. They agree through
// RaceCaller.lapsCalled, the counter the poller already uses to avoid
// re-announcing on every one of its ten polls a second. Claiming the lap here
// sets that counter forward, so the poll finds nothing new.
//
// It is deliberately a CLAIM and not a handover: if the push is missing, wrong
// about the pilot, or arrives for a heat we do not have, nothing is claimed and
// the poller announces it exactly as it does today. The push can only ever make
// a call earlier, never remove one.

import (
	"log"
	"strings"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// onFTLap speaks a pushed lap, if it is one we can safely claim.
func (ra *raceAnnouncer) onFTLap(e ftEvent) {
	if ra == nil || ra.engine == nil || ra.caller == nil {
		return
	}

	// STOCK's crash-out: the operator (or auto-detect) marked a pilot down —
	// the button the DNF flag never reflects mid-race, which is why this call
	// stayed silent on timing data alone. FPVTrackside only sends it while
	// the heat runs, so the moment is live by construction. Same wording and
	// the same once-per-heat dedup as the DNF path: "is out", never
	// "crashed" — the PA does not diagnose.
	if e.stockCrashedOut() {
		race, pilot, ok := ra.currentRacePilot(e.Name)
		if !ok {
			return
		}
		if !ra.caller.claimOut(race.ID, pilot.Name) {
			return // already announced, by either path
		}
		log.Printf("ft events: %s marked crashed out — calling it from the push", pilot.Name)
		call := RaceCall{Key: SoundKey("PilotOut"), Text: pilot.Name + " is out",
			Priority: callPriorityLap}
		ra.bridge.NoteSpoken(call.Text)
		ra.enqueue(call)
		return
	}

	// STOCK FPVTrackside's notifier — armed by NotificationURL alone, no fork.
	// Its Time is CUMULATIVE seconds since the race start, so the lap's own
	// duration is the difference against the pilot's previous crossing.
	if e.stockLapEnd() {
		race, pilot, ok := ra.currentRacePilot(e.PilotName)
		if !ok {
			return
		}
		secs, ok := ra.stockLapSeconds(race.ID, pilot.Name, e.LapNumber, e.TimeSecs)
		if !ok {
			return // holeshot, missing baseline, or a rerun reset — the poll covers it
		}
		ra.pushLapCall(race, pilot, e.LapNumber, secs)
		return
	}

	// The fork's ExtensionNotifier: carries the lap duration outright, plus
	// Valid — FPVTrackside's own judgement on a double-detection.
	if e.Type != "DetectionExt" || !e.IsLapEnd || !e.Valid {
		return
	}
	if e.LapNumber <= 0 || e.LapTimeSoFar <= 0 || strings.TrimSpace(e.PilotName) == "" {
		return
	}
	race, pilot, ok := ra.currentRacePilot(e.PilotName)
	if !ok {
		// We have no heat matching this push — starting mid-meeting, or the
		// snapshot has not caught up with a heat change. The poller will handle
		// it once it has; being early is not worth being wrong.
		return
	}
	ra.pushLapCall(race, pilot, e.LapNumber, e.LapTimeSoFar)
}

// pushLapCall claims and speaks one pushed lap — the shared tail of both
// notifier dialects.
func (ra *raceAnnouncer) pushLapCall(race ddapi.Race, pilot ddapi.Pilot, lap int, secs float64) {
	// Claim it BEFORE speaking. If the call is dropped downstream the lap is
	// still marked said, which is the right way round: a missed lap call is a
	// gap, a doubled one is the booth talking over itself.
	if !ra.caller.claimLap(race.ID, pilot.Name, lap) {
		return // the poller got there first
	}

	call, ok := ra.caller.lapCallFor(race, pilot, lap, secs)
	if !ok {
		return // the sound is switched off in FPVTrackside, or the figure is implausible
	}
	log.Printf("ft events: calling %s lap %d (%.2fs) from the push, ahead of the poll",
		pilot.Name, lap, secs)

	// Told to the RELAY as well as claimed from the poller.
	//
	// There are three paths that can speak a lap, not two: the poll, this push,
	// and the relay of FPVTrackside's own spoken log. Claiming lapsCalled only
	// silences the poll. FPVTrackside logs its version of the call at the moment
	// it makes it, so without this the relay repeats the lap about two seconds
	// later — heard as every lap being announced twice, which is worse than the
	// lateness this was built to fix.
	ra.bridge.NoteSpoken(call.Text)
	ra.enqueue(call)
}

// stockLapSeconds turns stock cumulative race time into one lap's duration.
//
// Every crossing is recorded as the pilot's new baseline, INCLUDING the ones
// that produce no call — the holeshot (lap 0, its cumulative time IS the
// baseline lap 1 subtracts from), a lap with no baseline (agent started
// mid-heat; the poll calls it, the next lap computes), and a rerun (time went
// backwards: resync silently, never announce a negative).
func (ra *raceAnnouncer) stockLapSeconds(raceID, pilot string, lap int, cum float64) (float64, bool) {
	key := raceID + "\x00" + pilot
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if ra.stockCum == nil {
		ra.stockCum = map[string]float64{}
	}
	prev, had := ra.stockCum[key]
	ra.stockCum[key] = cum
	if lap <= 0 || !had {
		return 0, false
	}
	secs := cum - prev
	if secs <= 0 {
		return 0, false
	}
	return secs, true
}

// currentRacePilot finds the running heat and the pilot the push names.
//
// Matched on the snapshot we last saw. The push tells us a pilot NAME and a
// round/race NUMBER; the caller keys its state on FPVTrackside's race id, which
// the push does not carry, so the snapshot is what bridges them.
func (ra *raceAnnouncer) currentRacePilot(name string) (ddapi.Race, ddapi.Pilot, bool) {
	ra.mu.Lock()
	race := ra.lastRace
	ra.mu.Unlock()

	if race.ID == "" {
		return ddapi.Race{}, ddapi.Pilot{}, false
	}
	for _, p := range race.Pilots {
		if strings.EqualFold(strings.TrimSpace(p.Name), strings.TrimSpace(name)) {
			return race, p, true
		}
	}
	return ddapi.Race{}, ddapi.Pilot{}, false
}

// claimOut marks a pilot's crash-out as announced, and reports whether this
// caller got it first — the push and the poll's DNF path share the map, so
// whichever sees it first speaks and the other stays quiet.
func (rc *RaceCaller) claimOut(raceID, pilot string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	key := raceID + "\x00" + pilot
	if _, said := rc.outSaid[key]; said {
		return false
	}
	rc.outSaid[key] = struct{}{}
	return true
}

// claimLap marks a lap as said, and reports whether this caller got it first.
//
// The counter is the poller's own: lapsCalled[race+pilot] is the number of laps
// already announced, and the poller speaks laps said+1..done on each pass. Moving
// it forward is exactly how the poller is told to skip one.
func (rc *RaceCaller) claimLap(raceID, pilot string, lap int) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	key := raceID + "\x00" + pilot
	if rc.lapsCalled[key] >= lap {
		return false // already announced, by either path
	}
	// Set to this lap rather than incrementing: if the push skipped one (a
	// dropped event), we do not want the poller replaying an old lap out of
	// order behind the current one. It is stale by then and would confuse.
	rc.lapsCalled[key] = lap
	return true
}

// lapCallFor builds the lap call for an explicitly supplied lap and time.
//
// The poller reads both from the snapshot; the push supplies them directly. The
// wording, the sound key, the time-trial/race distinction and the plausibility
// check are all the shared lapCall, so the two paths cannot say it differently.
func (rc *RaceCaller) lapCallFor(race ddapi.Race, p ddapi.Pilot, lap int, seconds float64) (RaceCall, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.lapCall(race, p, lap, seconds)
}
