package main

// OSD message rendering: text constants and formatting ported from the
// vrxc_elrs reference plugin. Lowercase 'w'/'x' are HDZero font glyphs that
// render as block borders on the goggles.

import (
	"fmt"
	"log"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

// Message texts (reference plugin defaults).
const (
	// msgStage ("ARM NOW") is intentionally not shown: FPVTrackside cues the
	// next heat long before its pre-start countdown, so an arming prompt at
	// staging fires far too early. Kept for reference / possible opt-in later.
	msgStage  = "w ARM NOW x"
	msgStart  = "w   GO!   x"
	msgDone   = "w FINISHED! x"
	msgFinish = "w FINISH LAP! x"
	msgStop   = "w  LAND NOW!  x"
	msgLeader = "RACE LEADER"
)

// Display durations (reference plugin defaults, in deciseconds there).
const (
	startUptime        = 3 * time.Second
	finishUptime       = 2 * time.Second
	resultsUptime      = 4 * time.Second
	announcementUptime = 5 * time.Second
)

// fmtSplit renders seconds as m:ss.cc, matching the plugin's {m}:{s}.{d}.
func fmtSplit(sec float64) string {
	if sec <= 0 {
		return "0:00.00"
	}
	m := int(sec) / 60
	rem := sec - float64(m*60)
	return fmt.Sprintf("%d:%05.2f", m, rem)
}

// rowKey identifies one (uid, row) cell-range for timed-clear invalidation.
func rowKey(uid elrs.UID, row int) string {
	return fmt.Sprintf("%s:%d", uid, row)
}

// writeRow writes one element's text inside a session: clear the row, write
// the text at the layout position, bump the row generation. The row clear is
// skipped when the session already cleared the whole screen — one less frame
// to fight through race-start interference.
func (c *elrsController) writeRow(s *elrs.Session, uid elrs.UID, pos elrs.ElementPos, text string) error {
	if !s.ScreenCleared() {
		if err := s.ClearRow(pos.Row); err != nil {
			return err
		}
	}
	if err := s.Text(pos.Row, pos.StartCol(len(text)), text); err != nil {
		return err
	}
	c.mu.Lock()
	c.rowGen[rowKey(uid, pos.Row)]++
	c.mu.Unlock()
	return nil
}

// clearRowAfter schedules a row clear unless a newer write has claimed the
// row in the meantime (generation counter check).
func (c *elrsController) clearRowAfter(uid elrs.UID, row int, after time.Duration) {
	c.mu.Lock()
	gen := c.rowGen[rowKey(uid, row)]
	c.mu.Unlock()

	time.AfterFunc(after, func() {
		c.mu.Lock()
		current := c.rowGen[rowKey(uid, row)]
		c.mu.Unlock()
		if current != gen {
			return // a newer message owns this row now
		}
		err := c.bp.Session(uid, func(s *elrs.Session) error {
			return s.ClearRow(row)
		})
		if err != nil && err != elrs.ErrNotConnected {
			log.Printf("elrs: timed row clear failed: %v", err)
		}
	})
}

// sendStatus writes a status message and schedules its clear.
func (c *elrsController) sendStatus(uid elrs.UID, layout elrs.Layout, text string, uptime time.Duration) {
	pos, ok := layout.Elements[elrs.ElemRaceStatus]
	if !ok || !pos.Enabled {
		return
	}
	err := c.bp.Session(uid, func(s *elrs.Session) error {
		return c.writeRow(s, uid, pos, text)
	})
	if err != nil {
		if err != elrs.ErrNotConnected {
			log.Printf("elrs: status write failed: %v", err)
		}
		return
	}
	c.holdAfter(uid, pos, int(uptime/time.Second))
}

// sendAnnouncement writes announcement text (already uppercased by the
// caller) and schedules its clear.
func (c *elrsController) sendAnnouncement(uid elrs.UID, layout elrs.Layout, text string, uptime time.Duration) error {
	pos, ok := layout.Elements[elrs.ElemAnnouncement]
	if !ok || !pos.Enabled {
		return nil
	}
	wrapped := "x " + text + " w"
	err := c.bp.Session(uid, func(s *elrs.Session) error {
		return c.writeRow(s, uid, pos, wrapped)
	})
	if err != nil {
		return err
	}
	c.holdAfter(uid, pos, int(uptime/time.Second))
	return nil
}

// clearOSD wipes a pilot's whole screen.
func (c *elrsController) clearOSD(uid elrs.UID) error {
	return c.bp.Session(uid, func(s *elrs.Session) error {
		return s.ClearScreen()
	})
}
