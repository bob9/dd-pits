package main

// The ELRS OSD engine. dd-pits polls FPVTrackside state — there are no
// event callbacks — so this engine diffs successive snapshots (resolved
// through the same buildMeeting the local dashboard uses) into the race
// events the reference RotorHazard plugin handled: stage, start, lap
// recorded, pilot done, finish/stop, laps cleared.
//
// Observe() is called from the tick loop with a coalescing single-slot
// buffer: a slow backpack can never stall FPVTrackside polling, and the
// engine always processes the newest state.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/elrs"
)

// Observe hands the newest snapshot to the engine goroutine.
func (c *elrsController) Observe(snap dronedash.Snapshot) {
	c.mu.Lock()
	c.latest = &snap
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// engineLoop processes coalesced snapshots until ctx is done.
func (c *elrsController) engineLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			c.mu.Lock()
			snap := c.latest
			c.latest = nil
			c.mu.Unlock()
			if snap != nil {
				c.processSnapshot(*snap)
			}
		}
	}
}

// processSnapshot diffs one snapshot against tracked race state and emits
// OSD writes for every mapped pilot affected by a state edge.
func (c *elrsController) processSnapshot(snap dronedash.Snapshot) {
	mv := buildMeeting(snap)

	c.mu.Lock()
	c.lastMeeting = mv
	primed := c.primed
	c.primed = true
	prevCue := c.lastCue
	c.lastCue = snap.SelectedRaceID
	c.mu.Unlock()

	for i := range mv.Races {
		race := &mv.Races[i]
		track := c.trackFor(race.ID)

		if !primed {
			// First snapshot after startup only initializes tracking —
			// never replay history onto goggles.
			c.syncTrack(track, race)
			continue
		}

		prevState := track.state
		switch {
		case race.State == "running" && prevState != "running" && race.StartMs != track.startMs:
			// A heat going green. NOT just pending→running: the pending
			// window between a re-run's laps-clear and its restart is often
			// shorter than the poll gap and gets coalesced away, so the edge
			// that arrives is ended→running. That used to match no case at
			// all — no clear-screen, no GO!, and (below) the previous run's
			// lap/finish bookkeeping left in place, so the pilot flew the
			// whole re-run staring at the last run's FINISHED/PLACEMENT
			// screen. Keyed on a CHANGED start stamp so a heat that merely
			// bounced ended→running (the stale-heat grace expiring, then a
			// late lap landing) never fires a spurious mid-race GO!.
			c.resetTrack(track)
			c.onRaceStart(race, mv.Name)
		case prevState == "running" && race.State == "ended":
			c.onRaceStop(race, track)
		case (prevState == "running" || prevState == "ended") && race.State == "pending":
			// Re-run / laps cleared: wipe the heat's pilots.
			c.onLapsClear(race)
		}

		if race.State == "running" {
			c.resendMissedGo(race)
			c.diffLaps(race, track)
		}

		c.syncTrack(track, race)
	}

	// Staging: the operator cued a different un-run heat. The cue on the
	// priming snapshot also counts — an agent (re)started while a heat sits
	// cued must still arm that heat's DVR label (label only, no stage OSD
	// replay). And a cue change first seen with the race already running
	// means the stage edge was coalesced away (cue + start landed inside one
	// poll gap) — arm the label now rather than never.
	if snap.SelectedRaceID != "" && (!primed || snap.SelectedRaceID != prevCue) {
		for i := range mv.Races {
			if mv.Races[i].ID != snap.SelectedRaceID {
				continue
			}
			switch {
			case mv.Races[i].State == "pending" && primed:
				c.onRaceStage(&mv.Races[i], mv.Name)
			case mv.Races[i].State == "pending" || mv.Races[i].State == "running":
				c.armDVRLabel(&mv.Races[i], mv.Name)
			}
			break
		}
	}

	c.refreshDVRLabel(mv)
}

func (c *elrsController) trackFor(raceID string) *elrsRaceTrack {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.races[raceID]
	if !ok {
		t = &elrsRaceTrack{state: "pending", laps: map[string]int{}, done: map[string]bool{}}
		c.races[raceID] = t
	}
	return t
}

// syncTrack records a race's current state/start stamp/lap counts for the
// next diff.
func (c *elrsController) syncTrack(track *elrsRaceTrack, race *vmRace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	track.state = race.State
	track.startMs = race.StartMs
	if race.State == "pending" {
		track.laps = map[string]int{}
		track.done = map[string]bool{}
		return
	}
	for i := range race.Pilots {
		p := &race.Pilots[i]
		track.laps[p.Name] = len(p.DispLaps)
	}
}

// resetTrack wipes a heat's lap/finish bookkeeping at a fresh start, so a
// re-run is diffed from zero. Without it a re-run whose pending window was
// coalesced away keeps the previous run's counts: early laps read as "not
// new" and a pilot who finished last time stays flagged done, so neither ever
// reaches their goggles.
func (c *elrsController) resetTrack(track *elrsRaceTrack) {
	c.mu.Lock()
	defer c.mu.Unlock()
	track.laps = map[string]int{}
	track.done = map[string]bool{}
}

// onRaceStage mirrors the plugin's onRaceStage: clear the screen, show the
// arm message plus heat/class/event names per each pilot's layout.
func (c *elrsController) onRaceStage(race *vmRace, eventName string) {
	// Tell each pilot's goggles what to name the heat's DVR recording —
	// at stage time, before anyone arms and recording starts. Remembered so
	// refreshDVRLabel can re-send it while the heat sits staged.
	c.armDVRLabel(race, eventName)
	// Every pilot onto their race channel, if the race director wants that.
	c.raceChannelsOnStage(race)

	heatMsg := ""
	if race.Label != "" {
		heatMsg = "x " + strings.ToUpper(race.Label) + " w"
	}
	eventMsg := ""
	if eventName != "" {
		eventMsg = "x " + strings.ToUpper(eventName) + " w"
	}

	for i := range race.Pilots {
		p := &race.Pilots[i]
		uid, layout, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}
		c.journalMsg(uid, p.Name, race, "osd_stage", strings.TrimSpace(strings.Trim(heatMsg+" "+eventMsg, "xw ")))
		err := c.bp.Session(uid, func(s *elrs.Session) error {
			if err := s.ClearScreen(); err != nil {
				return err
			}
			// No ARM NOW at staging: FPVTrackside cues the next heat well
			// before its pre-start countdown (and exposes no staging signal),
			// so an arming prompt here fires far too early. The race_status
			// element first shows GO! at the pending→running start.
			//
			// Rows go through writeRow (not raw Text) so each write claims
			// the row generation: a quick re-cue must invalidate the previous
			// stage's pending timed clears, or they fire against the NEW
			// stage text and wipe freshly drawn rows.
			if pos, ok := layout.Elements[elrs.ElemHeatName]; ok && pos.Enabled && heatMsg != "" {
				if err := c.writeRow(s, uid, pos, heatMsg); err != nil {
					return err
				}
			}
			if pos, ok := layout.Elements[elrs.ElemEventName]; ok && pos.Enabled && eventMsg != "" {
				if err := c.writeRow(s, uid, pos, eventMsg); err != nil {
					return err
				}
			}
			return nil
		})
		c.journalOutcome(uid, "osd_stage", err)
		if err == nil {
			if pos, ok := layout.Elements[elrs.ElemHeatName]; ok && pos.Enabled && heatMsg != "" {
				c.holdAfter(uid, pos, 0)
			}
			if pos, ok := layout.Elements[elrs.ElemEventName]; ok && pos.Enabled && eventMsg != "" {
				c.holdAfter(uid, pos, 0)
			}
		} else if err != elrs.ErrNotConnected {
			log.Printf("elrs: stage OSD for %s: %v", p.Name, err)
		}
	}
}

// onRaceStart shows the GO! message to every mapped pilot in the heat.
// The send is one atomic session — clear-screen, GO! text, display: three
// radio frames, and if any is lost the delivery reports re-fire the whole
// session (see checkGoDelivered), so the screen always ends up right. (A
// 2-frame variant that skipped the clear and wiped stage rows in a deferred
// session was tried and reverted: the cleanup landed in peak arm-up RF with
// no retry, leaving stale stage text on goggles.)
func (c *elrsController) onRaceStart(race *vmRace, eventName string) {
	for i := range race.Pilots {
		p := &race.Pilots[i]
		uid, layout, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}
		c.sendGo(race, p.Name, uid, layout, 0)
	}

	// Re-arm the DVR label once the start-window air quiets down: the
	// stage-time send can drop, the stage edge itself can be missed (a
	// re-run started without a re-cue), and many pilots only start their DVR
	// at arm/GO. Deferred so its frames never contend with GO!, first laps,
	// or GO resends during the worst RF seconds of the heat. A recording
	// that already began keeps the name it opened with.
	raceID := race.ID
	raceCopy := *race
	c.mu.Lock()
	labelGen := c.labelGen
	c.mu.Unlock()
	time.AfterFunc(c.rearmDelay, func() {
		c.mu.Lock()
		mv := c.lastMeeting
		moved := c.labelGen != labelGen
		c.mu.Unlock()
		if mv == nil {
			return
		}
		if moved {
			// The operator pre-cued the next heat inside the start window and
			// its label is already armed — re-arming this heat's would clobber
			// it on any pilot flying both. The newer label wins.
			return
		}
		for i := range mv.Races {
			if mv.Races[i].ID == raceID && mv.Races[i].State == "running" {
				c.armDVRLabel(&raceCopy, eventName)
				return
			}
		}
	})
}

// A GO! session puts three SetOSD frames on the air (clear-screen + text +
// display; the per-row clear is skipped after the full clear), but the
// netpack does NOT report one per frame — production logs show every GO!
// session acking exactly 2 (occasionally 5), never 3. Counting acks against a
// hardcoded expectation therefore read every start as a partial loss and
// resent it twice: 36 of 47 starts at the July 2026 Rebels meeting fired the
// full three-send chain, all reporting delivered — triple the frames in the
// worst RF seconds of the day, for nothing.
//
// So the loss signal is the report itself, not an ack tally: resend when a
// frame comes back FAILED, or when nothing acked at all (and reports are
// known to be flowing, so silence really means silence).
func goSessionLost(framesOK, framesBad int) bool { return framesBad > 0 || framesOK == 0 }

// goResendCheck/goResendMax: how long after a GO! send to check the netpack's
// per-frame delivery reports, and how many resends to attempt before giving
// up. Bounds the tail: a GO! that lost the airtime lottery at the start tone
// retries within ~1.2s instead of never arriving.
const (
	goResendCheck = 1200 * time.Millisecond
	goResendMax   = 2
)

// dvrStartRearmDelay defers the start-time label re-arm out of the busiest
// RF window of the race.
const dvrStartRearmDelay = 5 * time.Second

// sendGo fires the GO! message at one pilot and schedules the delivery
// check. attempt 0 is the start-edge send; attempts 1..goResendMax are
// report-driven resends.
func (c *elrsController) sendGo(race *vmRace, pilot string, uid elrs.UID, layout elrs.Layout, attempt int) {
	pos, posOK := layout.Elements[elrs.ElemRaceStatus]

	c.mu.Lock()
	c.goSentAt[uid] = time.Now()
	c.goFramesOK[uid] = 0
	c.goFramesBad[uid] = 0
	c.mu.Unlock()

	msg := "GO!"
	if attempt > 0 {
		msg = fmt.Sprintf("GO! (resend %d)", attempt)
	}
	c.journalMsg(uid, pilot, race, "osd_go", msg)
	err := c.bp.Session(uid, func(s *elrs.Session) error {
		// Clear the whole screen (stage text, announcements, whatever is
		// stale) and show GO! in ONE atomic render.
		if err := s.ClearScreen(); err != nil {
			return err
		}
		if posOK && pos.Enabled {
			return c.writeRow(s, uid, pos, msgStart)
		}
		return nil
	})
	c.journalOutcome(uid, "osd_go", err)
	if err != nil {
		// Nothing reached the air, so no delivery report can drive a resend:
		// flag it for the snapshot-driven retry instead. Leaving it here is
		// what stranded a pilot on the previous heat's screen for a whole
		// race when the backpack happened to be mid-reconnect at the start.
		c.markGoMissed(uid, race.ID)
		if err != elrs.ErrNotConnected {
			log.Printf("elrs: start OSD for %s: %v", pilot, err)
		}
		return
	}
	c.clearGoMissed(uid)
	// Re-arm the banner's timed clear on EVERY attempt, not just the first.
	// A resend rewrites the row, which bumps its generation and invalidates
	// the clear the first attempt scheduled — so a resent GO! used to sit on
	// the pilot's OSD for the whole race, with nothing left to take it down
	// until the finish. (Field evidence: 36 of 47 starts at the July 2026
	// Rebels meeting resent, i.e. most heats were flown under a stuck "GO!".)
	if posOK && pos.Enabled {
		c.holdAfter(uid, pos, int(startUptime/time.Second))
	}
	if attempt < goResendMax {
		raceID := race.ID
		raceCopy := *race
		time.AfterFunc(goResendCheck, func() {
			c.checkGoDelivered(&raceCopy, raceID, pilot, uid, layout, attempt)
		})
	}
}

// checkGoDelivered consults the netpack's per-frame delivery reports for the
// last GO! send and re-fires it if too few frames were acked — unless the
// race is no longer running (stopped or re-run) or reports aren't flowing at
// all (no report firmware: resending blind would double-send on every start).
func (c *elrsController) checkGoDelivered(race *vmRace, raceID, pilot string, uid elrs.UID, layout elrs.Layout, attempt int) {
	c.mu.Lock()
	framesOK := c.goFramesOK[uid]
	framesBad := c.goFramesBad[uid]
	mv := c.lastMeeting
	reportsSeen := c.reportsSeen
	c.mu.Unlock()
	if !goSessionLost(framesOK, framesBad) {
		return
	}
	if !reportsSeen {
		return // no report firmware — "0 acked" means unknown, not lost
	}
	if !c.bp.Connected() {
		return
	}
	if mv == nil {
		return
	}
	running := false
	for i := range mv.Races {
		if mv.Races[i].ID == raceID && mv.Races[i].State == "running" {
			running = true
			break
		}
	}
	if !running {
		return
	}
	log.Printf("elrs: GO! for %s: %d acked / %d failed after %s — resending", pilot, framesOK, framesBad, goResendCheck)
	c.sendGo(race, pilot, uid, layout, attempt+1)
}

// goMissedMax bounds the snapshot-driven retries of a failed start-edge
// clear+GO! — enough to ride out a backpack reconnect at the start tone,
// short of pestering a pilot mid-race.
const goMissedMax = 4

func (c *elrsController) markGoMissed(uid elrs.UID, raceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.goMissed[uid]; ok && m.raceID == raceID {
		return // keep the existing try count for this heat
	}
	c.goMissed[uid] = &goMissedState{raceID: raceID}
}

func (c *elrsController) clearGoMissed(uid elrs.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.goMissed, uid)
}

// resendMissedGo re-fires the start-edge clear+GO! for pilots whose session
// failed outright, from a later snapshot of the same still-running heat. The
// clear is the part that matters: a pilot who missed it flies the whole race
// with the previous heat's screen over their video. Silent while the backpack
// is down (a resend would fail the same way) and capped per heat.
func (c *elrsController) resendMissedGo(race *vmRace) {
	c.mu.Lock()
	none := len(c.goMissed) == 0
	c.mu.Unlock()
	if none || !c.bp.Connected() {
		return
	}
	for i := range race.Pilots {
		p := &race.Pilots[i]
		uid, layout, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}

		c.mu.Lock()
		m, pending := c.goMissed[uid]
		switch {
		case !pending:
		case m.raceID != race.ID || m.tries >= goMissedMax:
			delete(c.goMissed, uid)
			pending = false
		default:
			m.tries++
		}
		tries := 0
		if pending {
			tries = m.tries
		}
		c.mu.Unlock()

		if !pending {
			continue
		}
		log.Printf("elrs: start clear+GO! never sent to %s — retrying (%d/%d)", p.Name, tries, goMissedMax)
		c.sendGo(race, p.Name, uid, layout, 0)
	}
}

// noteGoFrame credits a reported SetOSD frame to an in-flight GO! send.
// Called from the inbound-report path; frames from other OSD traffic within
// the window are indistinguishable, which is acceptable — the check errs
// toward not resending.
func (c *elrsController) noteGoFrame(uid elrs.UID, delivered bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.goSentAt[uid]; ok && time.Since(at) < 5*time.Second {
		if delivered {
			c.goFramesOK[uid]++
		} else {
			c.goFramesBad[uid]++
		}
	}
}

// onRaceStop shows LAND NOW to pilots that hadn't already finished.
func (c *elrsController) onRaceStop(race *vmRace, track *elrsRaceTrack) {
	c.mu.Lock()
	doneCopy := make(map[string]bool, len(track.done))
	for k, v := range track.done {
		doneCopy[k] = v
	}
	c.mu.Unlock()

	for i := range race.Pilots {
		p := &race.Pilots[i]
		if doneCopy[p.Name] {
			continue
		}
		uid, layout, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}
		c.journalMsg(uid, p.Name, race, "osd_land", "LAND NOW")
		c.sendStatus(uid, layout, msgStop, finishUptime)
	}

	// The heat is over: clear the pending DVR label so a later non-race
	// recording never inherits this heat's name (pilots already re-labelled
	// for a pre-cued next heat are left alone). The recording that already
	// started keeps the name it was opened with.
	c.clearDVRLabels(race)
	c.mu.Lock()
	if c.labelRaceID == race.ID {
		c.labelRaceID, c.label = "", ""
	}
	c.mu.Unlock()
}

// onLapsClear wipes the OSD for every mapped pilot in a reset heat.
func (c *elrsController) onLapsClear(race *vmRace) {
	for i := range race.Pilots {
		p := &race.Pilots[i]
		uid, _, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}
		c.journalMsg(uid, p.Name, race, "osd_clear", "screen cleared (re-run)")
		err := c.clearOSD(uid)
		c.journalOutcome(uid, "osd_clear", err)
		if err != nil && err != elrs.ErrNotConnected {
			log.Printf("elrs: laps-clear for %s: %v", p.Name, err)
		}
	}
}

// diffLaps emits lap/position/result messages for pilots whose counted lap
// total increased, and the done sequence for pilots reaching target laps.
func (c *elrsController) diffLaps(race *vmRace, track *elrsRaceTrack) {
	lapped := map[string]bool{} // pilots whose crossing was processed this pass
	for i := range race.Pilots {
		p := &race.Pilots[i]

		c.mu.Lock()
		prevLaps := track.laps[p.Name]
		alreadyDone := track.done[p.Name]
		c.mu.Unlock()

		curLaps := len(p.DispLaps)
		if curLaps <= prevLaps || alreadyDone {
			continue
		}
		lapped[p.Name] = true

		uid, layout, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}

		pilotDone := !race.IsTT && race.TargetLaps > 0 && curLaps >= race.TargetLaps
		if pilotDone {
			c.mu.Lock()
			track.done[p.Name] = true
			c.mu.Unlock()
			c.onPilotDone(race, p, uid, layout)
			continue
		}

		c.onLapRecorded(race, p, uid, layout)
	}

	// A crossing also dates the pilot AHEAD's gap-behind line: push them the
	// fresh number. Runs after the lap pass so a pilot who lapped in the same
	// snapshot (their own session already carried it) is never double-sent,
	// and finished pilots keep their results screen.
	if len(lapped) == 0 {
		return
	}
	for i := range race.Pilots {
		chaser := &race.Pilots[i]
		if !lapped[chaser.Name] || chaser.Position <= 1 {
			continue
		}
		ahead := pilotAtPosition(race, chaser.Position-1)
		if ahead == nil || lapped[ahead.Name] {
			continue
		}
		c.mu.Lock()
		aheadDone := track.done[ahead.Name]
		c.mu.Unlock()
		if aheadDone {
			continue
		}
		uid, layout, ok := c.pilotOSD(ahead.Name)
		if !ok {
			continue
		}
		c.onChaserLap(race, ahead, uid, layout)
	}
}

// otherElementRows returns the rows owned by enabled elements other than
// skip. Multi-row elements contribute their full footprint.
func otherElementRows(layout elrs.Layout, skip string) map[int]bool {
	rows := map[int]bool{}
	for slug, p := range layout.Elements {
		if slug == skip || !p.Enabled {
			continue
		}
		n := 1
		if slug == elrs.ElemRecentLaps {
			n = p.RecentLapWindow()
		}
		for i := 0; i < n && p.Row+i < elrs.OSDRows; i++ {
			rows[p.Row+i] = true
		}
	}
	return rows
}

// assignRecentLapRows allocates a screen row per list line, starting at
// startRow and flowing around blocked rows (rows owned by other enabled
// elements). Returns fewer rows than asked when the screen runs out.
func assignRecentLapRows(nLines, startRow int, blocked map[int]bool) []int {
	var rows []int
	for row := startRow; row < elrs.OSDRows && len(rows) < nLines; row++ {
		if blocked[row] {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// onLapRecorded mirrors the plugin's onRaceLapRecorded: current lap/position
// plus a win-condition-aware lap/gap result line.
//
// Every enabled element rides in ONE backpack session — a single set-uid/
// display/reset-uid exchange and one goggle render for the whole lap update,
// instead of one session per element. That roughly halves the frames a lap
// costs, so the send queue drains to the next pilot sooner and lap messages
// land on the goggles with minimal delay.
func (c *elrsController) onLapRecorded(race *vmRace, p *vmPilot, uid elrs.UID, layout elrs.Layout) {
	lapNum := len(p.DispLaps)
	lastLap := p.DispLaps[lapNum-1]

	var stage []func(s *elrs.Session) error // row writes, run inside the session
	var after []func()                      // timed clears, scheduled on success

	// Current lap & position.
	if pos, ok := layout.Elements[elrs.ElemLapPosition]; ok && pos.Enabled {
		var msg string
		if p.Position > 0 {
			msg = fmt.Sprintf("POSN: %d | LAP: %d", p.Position, lapNum+1)
		} else {
			msg = fmt.Sprintf("LAP: %d", lapNum+1)
		}
		pos := pos
		stage = append(stage, func(s *elrs.Session) error { return c.writeRow(s, uid, pos, msg) })
		after = append(after, func() { c.holdAfter(uid, pos, 0) })
	}

	// Lap result line (TT: last lap + best consecutive; race: last lap + total).
	if pos, ok := layout.Elements[elrs.ElemLapResult]; ok && pos.Enabled {
		var msg string
		if race.IsTT {
			msg = fmt.Sprintf("x %s | %d/%s w", fmtSplit(lastLap), race.ConsecWindow, fmtSplit(p.BestConsec))
		} else {
			msg = fmt.Sprintf("x %s | %s w", fmtSplit(lastLap), fmtSplit(p.Total))
		}
		pos := pos
		stage = append(stage, func(s *elrs.Session) error { return c.writeRow(s, uid, pos, msg) })
		after = append(after, func() { c.holdAfter(uid, pos, int(resultsUptime/time.Second)) })
	}

	// Rolling recent laps: a vertical list of the pilot's last N laps
	// (L2:44.23 / L3:43.56 / …, one per row), refreshed on every lap. Window
	// size and how long it stays up are per-pilot layout options (default:
	// stays until the next lap or a screen clear).
	if pos, ok := layout.Elements[elrs.ElemRecentLaps]; ok && pos.Enabled {
		// The rolling list grows a row per lap up to its window, but the
		// layout editor places it as a single row — so it can land on rows
		// other elements own. The list FLOWS AROUND those rows: each line
		// takes the next free row at/below the anchor, skipping rows owned
		// by enabled single-row elements, so the list and the lap-time/gap
		// lines all render at the same time (previously lap 2's second list
		// line silently overwrote the lap-time row).
		lines := recentLapsLines(p, pos.RecentLapWindow())
		rows := assignRecentLapRows(len(lines), pos.Row, otherElementRows(layout, elrs.ElemRecentLaps))
		if len(rows) < len(lines) {
			lines = lines[:len(rows)]
		}
		if len(lines) > 0 {
			pos := pos
			stage = append(stage, func(s *elrs.Session) error {
				for i, line := range lines {
					rp := pos
					rp.Row = rows[i]
					if err := c.writeRow(s, uid, rp, line); err != nil {
						return err
					}
				}
				return nil
			})
			after = append(after, func() {
				if secs := pos.HoldFor(0); secs > 0 {
					for i := range lines {
						c.clearRowAfter(uid, rows[i], time.Duration(secs)*time.Second)
					}
				}
			})
		}
	}

	// Gap line (races only): time behind the next-ranked pilot at the same
	// lap number; the leader sees RACE LEADER | last lap.
	if pos, ok := layout.Elements[elrs.ElemGapResult]; ok && pos.Enabled && !race.IsTT {
		msg := ""
		if p.Position == 1 {
			msg = fmt.Sprintf("x %s | %s w", msgLeader, fmtSplit(lastLap))
		} else if ahead := pilotAtPosition(race, p.Position-1); ahead != nil {
			if gap := gapToAhead(p, ahead); gap > 0 {
				msg = fmt.Sprintf("x %s | +%s w", strings.ToUpper(ahead.Name), fmtSplit(gap))
			}
		}
		if msg != "" {
			pos := pos
			stage = append(stage, func(s *elrs.Session) error { return c.writeRow(s, uid, pos, msg) })
			after = append(after, func() { c.holdAfter(uid, pos, int(resultsUptime/time.Second)) })
		}
	}

	// Chaser line (races only): the pilot at the next position back and how
	// far behind they were at their last crossing. Also refreshed on the
	// CHASER's crossings via onChaserLap — that's when the gap is truly known.
	if pos, ok := layout.Elements[elrs.ElemGapBehind]; ok && pos.Enabled && !race.IsTT {
		if msg := gapBehindMsg(race, p); msg != "" {
			pos := pos
			stage = append(stage, func(s *elrs.Session) error { return c.writeRow(s, uid, pos, msg) })
			after = append(after, func() { c.holdAfter(uid, pos, int(resultsUptime/time.Second)) })
		}
	}

	if len(stage) == 0 {
		return
	}
	lapMsg := fmt.Sprintf("lap %d · %s", lapNum, fmtSplit(lastLap))
	if p.Position > 0 {
		lapMsg = fmt.Sprintf("P%d · %s", p.Position, lapMsg)
	}
	c.journalMsg(uid, p.Name, race, "osd_lap", lapMsg)
	err := c.bp.Session(uid, func(s *elrs.Session) error {
		for _, write := range stage {
			if err := write(s); err != nil {
				return err
			}
		}
		return nil
	})
	c.journalOutcome(uid, "osd_lap", err)
	if err == nil {
		for _, f := range after {
			f()
		}
	} else if err != elrs.ErrNotConnected {
		log.Printf("elrs: lap OSD for %s: %v", p.Name, err)
	}
}

// onPilotDone mirrors the plugin's onRacePilotDone: clear the lap rows, show
// FINISHED plus placement/result rows, then clear the status after a delay.
func (c *elrsController) onPilotDone(race *vmRace, p *vmPilot, uid elrs.UID, layout elrs.Layout) {
	statusPos, statusOK := layout.Elements[elrs.ElemRaceStatus]
	lapPos, lapOK := layout.Elements[elrs.ElemLapPosition]
	resPos, resOK := layout.Elements[elrs.ElemResults]

	c.journalMsg(uid, p.Name, race, "osd_finish", fmt.Sprintf("FINISHED · P%d", p.Position))
	err := c.bp.Session(uid, func(s *elrs.Session) error {
		if lapOK {
			if err := s.ClearRow(lapPos.Row); err != nil {
				return err
			}
		}
		if statusOK && statusPos.Enabled {
			if err := c.writeRow(s, uid, statusPos, msgDone); err != nil {
				return err
			}
		}
		if resOK && resPos.Enabled {
			place := fmt.Sprintf("PLACEMENT: %d", p.Position)
			if err := s.Text(resPos.Row, resPos.StartCol(len(place)), place); err != nil {
				return err
			}
			var win string
			if race.IsTT {
				win = fmt.Sprintf("FASTEST %d CONSEC: %s", race.ConsecWindow, fmtSplit(p.BestConsec))
			} else {
				win = fmt.Sprintf("TOTAL TIME: %s", fmtSplit(p.Total))
			}
			row2 := resPos.Row + 1
			if row2 < elrs.OSDRows {
				col := resPos.Col
				if resPos.Center {
					col = elrs.CenterCol(len(win))
				}
				if err := s.Text(row2, col, win); err != nil {
					return err
				}
			}
		}
		return nil
	})
	c.journalOutcome(uid, "osd_finish", err)
	if err != nil {
		if err != elrs.ErrNotConnected {
			log.Printf("elrs: pilot done for %s: %v", p.Name, err)
		}
		return
	}
	if statusOK && statusPos.Enabled {
		c.holdAfter(uid, statusPos, int(finishUptime/time.Second))
	}
	if resOK && resPos.Enabled {
		if secs := resPos.HoldFor(0); secs > 0 {
			c.clearRowAfter(uid, resPos.Row, time.Duration(secs)*time.Second)
			if resPos.Row+1 < elrs.OSDRows {
				c.clearRowAfter(uid, resPos.Row+1, time.Duration(secs)*time.Second)
			}
		}
	}
}

// holdAfter schedules the timed clear a user configured for an element:
// defSecs is the element's built-in behaviour (0 = normally stays up), and
// the pilot's HoldSecs overrides it (see ElementPos.HoldFor).
func (c *elrsController) holdAfter(uid elrs.UID, pos elrs.ElementPos, defSecs int) {
	if secs := pos.HoldFor(defSecs); secs > 0 {
		c.clearRowAfter(uid, pos.Row, time.Duration(secs)*time.Second)
	}
}

// recentLapsLines builds the rolling recent-laps list: the last n entries of
// [HS, L1, L2, …] in the reference plugin's compact seconds-only format,
// one line per lap ("L2:44.23"). Empty when the pilot has no laps yet.
func recentLapsLines(p *vmPilot, n int) []string {
	type entry struct {
		label string
		secs  float64
	}
	var entries []entry
	if p.Holeshot > 0 {
		entries = append(entries, entry{"HS", p.Holeshot})
	}
	for i, l := range p.DispLaps {
		entries = append(entries, entry{fmt.Sprintf("L%d", i+1), l})
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = fmt.Sprintf("%s:%.2f", e.label, e.secs)
	}
	return lines
}

// pilotAtPosition finds the pilot ranked at a given position in a race.
func pilotAtPosition(race *vmRace, position int) *vmPilot {
	for i := range race.Pilots {
		if race.Pilots[i].Position == position {
			return &race.Pilots[i]
		}
	}
	return nil
}

// gapBehindMsg builds p's chaser line: the pilot ranked one position behind
// and how far back they are, compared at the CHASER's lap count (the last
// gate both pilots have crossed). Empty when p is unranked, has nobody
// behind, or the chaser hasn't put in a comparable lap yet.
func gapBehindMsg(race *vmRace, p *vmPilot) string {
	if p.Position <= 0 {
		return ""
	}
	chaser := pilotAtPosition(race, p.Position+1)
	if chaser == nil {
		return ""
	}
	if gap := gapToAhead(chaser, p); gap > 0 {
		return fmt.Sprintf("x %s | -%s w", strings.ToUpper(chaser.Name), fmtSplit(gap))
	}
	return ""
}

// onChaserLap refreshes the gap-behind row on the AHEAD pilot's goggles the
// moment their chaser crosses the line — the instant the gap stops being
// stale. One row write in one session, only for pilots who enabled the
// element, so it costs no airtime on default layouts.
func (c *elrsController) onChaserLap(race *vmRace, p *vmPilot, uid elrs.UID, layout elrs.Layout) {
	pos, ok := layout.Elements[elrs.ElemGapBehind]
	if !ok || !pos.Enabled || race.IsTT {
		return
	}
	msg := gapBehindMsg(race, p)
	if msg == "" {
		return
	}
	c.journalMsg(uid, p.Name, race, "osd_gap", strings.TrimSpace(strings.Trim(msg, "xw ")))
	err := c.bp.Session(uid, func(s *elrs.Session) error { return c.writeRow(s, uid, pos, msg) })
	c.journalOutcome(uid, "osd_gap", err)
	if err == nil {
		c.holdAfter(uid, pos, int(resultsUptime/time.Second))
	} else if err != elrs.ErrNotConnected {
		log.Printf("elrs: chaser OSD for %s: %v", p.Name, err)
	}
}

// gapToAhead computes how far p trails the ahead pilot, comparing cumulative
// times at p's lap count (the ahead pilot has completed at least as many).
func gapToAhead(p, ahead *vmPilot) float64 {
	n := len(p.DispLaps)
	if n == 0 || len(ahead.DispLaps) < n {
		return 0
	}
	mine := cumulativeAt(p, n)
	theirs := cumulativeAt(ahead, n)
	if mine <= theirs {
		return 0
	}
	return mine - theirs
}

func cumulativeAt(p *vmPilot, laps int) float64 {
	total := p.Holeshot
	for i := 0; i < laps && i < len(p.DispLaps); i++ {
		total += p.DispLaps[i]
	}
	return total
}
