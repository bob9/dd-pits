package main

// DVR race labels: when a heat is staged, each pilot's goggles are told what
// to name their next DVR recording (MSP_SET_DVR_NAME) —
// "<Meeting>-<Mains-A..F|CTA>-<R|Q><Round>-H<Heat>", where the Mains/CTA tag
// appears only when the round name identifies one, and the meeting name takes
// as much of the remaining space as fits. The goggles prepend the date and
// their own file counter, producing e.g.
// 2026-07-13-0042-WinterCup-Mains-A-R2-H1.mp4. The label is cleared when the
// heat ends so a later non-race recording never inherits the race's name (the
// goggles also expire it after a few minutes as a backstop).

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

// Round names that mark a heat as part of a lettered main or a chase-the-ace
// final, e.g. "A Main", "B-Mains", "CTA", "Chase the Ace". dd-pits only sees
// the timing source's round names, so this is the sole mains/CTA signal.
var (
	dvrMainRe = regexp.MustCompile(`(?i)\b([A-F])[ -]?Mains?\b`)
	dvrCTARe  = regexp.MustCompile(`(?i)\bCTA\b|(?i)chase`)
)

// dvrSanitize keeps the goggles-filename-safe subset: alphanumerics kept,
// everything else dropped (the goggles sanitise again, but dropping here
// keeps the length cap honest).
func dvrSanitize(s string, max int) string {
	out := make([]byte, 0, max)
	for i := 0; i < len(s) && len(out) < max; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}

// dvrLabel builds the recording label for a heat: R (race) or Q (qualifier)
// with round and heat numbers, a Mains-X / CTA tag when the round name
// carries one, and as much of the meeting name in front as still fits the
// goggles' cap.
func dvrLabel(eventName string, race *vmRace) string {
	kind := "R"
	if race.IsTT {
		kind = "Q"
	}
	tail := fmt.Sprintf("%s%d-H%d", kind, race.Round, race.Number)
	if dvrCTARe.MatchString(race.RoundName) {
		tail = "CTA-" + tail
	} else if m := dvrMainRe.FindStringSubmatch(race.RoundName); m != nil {
		tail = "Mains-" + strings.ToUpper(m[1]) + "-" + tail
	}
	if event := dvrSanitize(eventName, elrs.DVRNameMaxLen-len(tail)-1); event != "" {
		return event + "-" + tail
	}
	return tail
}

// dvrLabelRefreshStaged is the re-send cadence while the labelled heat is
// still pending. This window is where labels are won or lost: pilots (and the
// race director) commonly cue the heat FIRST and power goggles on after, so
// the stage-time send misses them, and with record-on-arm the file opens at
// the test-arm — before the start, so the GO-time re-send is too late. A hot
// cadence means goggles booting any time in the staged window get the label
// within seconds. It's cheap when no heat is running (idle link) — and it is
// PAUSED entirely while one is: staged pilots' goggles are often still off,
// a frame to a powered-off goggle costs the backpack its full retry budget
// (see the clock-sync trickle notes), and a pre-cued heat's refresh must
// never queue ahead of the live heat's OSD traffic.
const dvrLabelRefreshStaged = 10 * time.Second

// dvrLabelRefresh is the cadence once the heat is running, sharing the link
// with live lap OSD traffic — relaxed well below the staged cadence: the
// label was already sent at stage and re-armed after the start, the goggles'
// TTL is 5 minutes, and every refresh frame contends with live lap OSD.
const dvrLabelRefresh = 2 * time.Minute

// dvrSendRepeats: each label is sent once per delivery. The netpack retries
// every frame until the goggle MAC-acks it (and reports the outcome), and the
// 30s refresh re-sends the label anyway — a second back-to-back copy only
// burned ~800ms of busy-air right before the RTC/OSD frames queued behind it.
const dvrSendRepeats = 1

// armDVRLabel computes a heat's label, sends it to every mapped pilot, and
// remembers it as the active label refreshDVRLabel keeps alive. Logged so a
// "my recording has no name" report can be diffed against the agent log —
// never sent (our bug) vs sent but not received (goggles off / RF).
func (c *elrsController) armDVRLabel(race *vmRace, eventName string) {
	label := dvrLabel(eventName, race)
	sent := c.sendDVRLabels(race, label, true)
	log.Printf("elrs: dvr label %q armed for race %s (%d/%d pilots mapped)", label, race.ID, sent, len(race.Pilots))
	c.mu.Lock()
	c.labelRaceID, c.label, c.labelSentAt = race.ID, label, time.Now()
	c.labelGen++ // lets a deferred re-arm tell that a newer heat took the label
	c.mu.Unlock()
}

// refreshDVRLabel re-sends the active heat's label (with its paired clock
// sync) once it is getting stale — on the hot staged cadence while the heat
// is pending, the relaxed one while it runs. No-op until a stage sets a
// label; stops when the heat ends (onRaceStop clears it) or the race
// vanishes.
func (c *elrsController) refreshDVRLabel(mv *meetingView) {
	c.mu.Lock()
	raceID, label, sentAt := c.labelRaceID, c.label, c.labelSentAt
	c.mu.Unlock()
	if raceID == "" {
		return
	}
	anyRunning := false
	for i := range mv.Races {
		if mv.Races[i].State == "running" {
			anyRunning = true
			break
		}
	}

	for i := range mv.Races {
		if mv.Races[i].ID != raceID {
			continue
		}
		if mv.Races[i].State == "ended" {
			return
		}
		interval := dvrLabelRefresh
		if mv.Races[i].State == "pending" {
			// A pre-cued heat's refresh is deferred while another heat runs —
			// its likely-powered-off goggles cost retry budget that would
			// delay the live heat's OSD messages. It resumes within one
			// snapshot of the track going quiet, still well before this
			// heat's pilots arm.
			if anyRunning {
				return
			}
			interval = dvrLabelRefreshStaged
		}
		if time.Since(sentAt) < interval {
			return
		}
		// No RTC pairing on refreshes: the clock trickle owns routine sync,
		// and the paired frame doubled every refresh's airtime.
		c.sendDVRLabels(&mv.Races[i], label, false)
		c.mu.Lock()
		c.labelSentAt = time.Now()
		c.mu.Unlock()
		return
	}
}

// sendDVRLabels sends label to every mapped pilot in the heat, returning how
// many pilots were sent to. withRTC pairs a clock sync with the label (arm
// time only) so the recording the label names also carries correct
// timestamps, even on goggles that have seen no other time source that day.
func (c *elrsController) sendDVRLabels(race *vmRace, label string, withRTC bool) int {
	now := time.Now()
	sent := 0
	for i := range race.Pilots {
		p := &race.Pilots[i]
		uid, _, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}
		sent++
		c.journalMsg(uid, p.Name, race, "dvr_label", label)
		var sendErr error
		for r := 0; r < dvrSendRepeats; r++ {
			if err := c.bp.SendDVRNameTo(uid, label); err != nil {
				sendErr = err
				if err != elrs.ErrNotConnected {
					log.Printf("elrs: dvr label for %s: %v", p.Name, err)
				}
				break
			}
		}
		c.journalOutcome(uid, "dvr_label", sendErr)
		if withRTC {
			if err := c.bp.SendRTCTo(uid, now); err != nil && err != elrs.ErrNotConnected {
				log.Printf("elrs: rtc for %s: %v", p.Name, err)
			}
		}
	}
	return sent
}

// clearDVRLabels clears the pending label on the heat's pilots at race stop —
// except pilots who are also in the currently-armed (pre-cued next) heat: an
// operator cueing heat N+1 before heat N ends must not have N's stop wipe a
// shared pilot's fresh label.
func (c *elrsController) clearDVRLabels(race *vmRace) {
	c.mu.Lock()
	labelRaceID := c.labelRaceID
	mv := c.lastMeeting
	c.mu.Unlock()

	skip := map[string]bool{}
	if labelRaceID != "" && labelRaceID != race.ID && mv != nil {
		for i := range mv.Races {
			if mv.Races[i].ID == labelRaceID {
				for _, p := range mv.Races[i].Pilots {
					skip[p.Name] = true
				}
				break
			}
		}
	}

	for i := range race.Pilots {
		p := &race.Pilots[i]
		if skip[p.Name] {
			continue
		}
		uid, _, ok := c.pilotOSD(p.Name)
		if !ok {
			continue
		}
		c.journalMsg(uid, p.Name, race, "dvr_clear", "")
		var sendErr error
		for r := 0; r < dvrSendRepeats; r++ {
			if err := c.bp.SendDVRNameTo(uid, ""); err != nil {
				sendErr = err
				if err != elrs.ErrNotConnected {
					log.Printf("elrs: dvr label clear for %s: %v", p.Name, err)
				}
				break
			}
		}
		c.journalOutcome(uid, "dvr_clear", sendErr)
	}
}
