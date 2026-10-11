package main

// Goggle clock sync: dd-pits runs on the venue laptop, whose clock (and
// timezone) is the best time source at the field, so it sends the local time
// to the goggles via the backpack — on connect and then periodically. HDZero
// goggles set their Linux OS clock + hardware RTC from it, which is what
// stamps DVR recordings with the correct date and time. The goggles' clock
// is not battery-backed, so this needs to repeat: goggles powered on
// mid-meeting are correct within one resync interval.
//
// A goggle backpack only accepts frames from the UID it is bound to, so each
// configured pilot gets their own uid-targeted send (the same
// set-uid/reset-uid exchange the OSD engine uses).
//
// Sends are TRICKLED, not burst: a frame to a powered-off goggle costs the
// netpack its full retry budget (no ack ever comes), so at a big event a
// whole-field sweep would occupy the send queue for seconds and delay OSD
// traffic queued behind it. Instead the field is spread evenly across the
// resync window — a few pilots per tick — so the queue never holds more
// than a handful of frames, and the trickle stops the moment a heat starts.

import (
	"context"
	"log"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

const (
	// clockResyncInterval is the target period between syncs of any one
	// pilot's goggles. Goggles powered on mid-meeting pick the time up
	// within one interval; drift never accumulates.
	clockResyncInterval = time.Minute
	// clockMaxInterval caps how long a running heat can defer the trickle —
	// syncs prefer the gaps between heats so they never contend with live
	// OSD traffic, but a long time-trial session must not starve them.
	clockMaxInterval = 10 * time.Minute
	// clockPollInterval is the trickle tick: each tick sends to just enough
	// pilots to cover the whole field within clockResyncInterval.
	clockPollInterval = 2 * time.Second
)

// clockTarget is one pilot's goggles in the clock-sync rotation.
type clockTarget struct {
	callsign string
	uid      elrs.UID
}

// clockTargets collects every goggle worth syncing: all cloud-configured
// pilots, deduped by UID, with bad UIDs dropped. Pilots without a
// cloud-linked bind phrase have no reachable goggles, so there is nothing to
// sync (opt-outs are already absent from Pilots).
func (c *elrsController) clockTargets() []clockTarget {
	seen := map[elrs.UID]bool{}
	var out []clockTarget

	if cfg := c.cfg.Load(); cfg != nil {
		for callsign, p := range cfg.Pilots {
			uid, err := elrs.ParseUID(p.UID)
			if err != nil {
				log.Printf("elrs: pilot %s has bad uid %q: %v", callsign, p.UID, err)
				continue
			}
			if !seen[uid] {
				seen[uid] = true
				out = append(out, clockTarget{callsign: callsign, uid: uid})
			}
		}
	}
	return out
}

// heatRunning reports whether a heat is currently in progress.
func (c *elrsController) heatRunning() bool {
	c.mu.Lock()
	mv := c.lastMeeting
	c.mu.Unlock()
	return mv != nil && mv.Current != nil && mv.Current.State == "running"
}

// clockBatchSize returns how many pilots to sync per tick so a field of
// total pilots is fully covered within clockResyncInterval.
func clockBatchSize(total int) int {
	ticks := int(clockResyncInterval / clockPollInterval)
	if ticks < 1 {
		ticks = 1
	}
	batch := (total + ticks - 1) / ticks
	if batch < 1 {
		batch = 1
	}
	return batch
}

// clockLoop trickles the local time to every known pilot's goggles, covering
// the whole field once per clockResyncInterval, pausing while a heat runs.
func (c *elrsController) clockLoop(ctx context.Context) {
	var pending []clockTarget // pilots still to sync in the current cycle
	var cycleStart time.Time
	var heatSince time.Time // when the current heat-pause began
	logged := false

	ticker := time.NewTicker(clockPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !c.bp.Connected() {
			pending = nil
			cycleStart = time.Time{}
			logged = false
			continue
		}

		// Prefer the gaps between heats: while a heat is live the OSD
		// engine owns the link, so hold off (up to clockMaxInterval).
		if c.heatRunning() {
			if heatSince.IsZero() {
				heatSince = time.Now()
			}
			if time.Since(heatSince) < clockMaxInterval {
				continue
			}
		} else {
			heatSince = time.Time{}
		}

		// Start a new cycle once the previous one is drained and due.
		if len(pending) == 0 {
			if !cycleStart.IsZero() && time.Since(cycleStart) < clockResyncInterval {
				continue
			}
			pending = c.clockTargets()
			cycleStart = time.Now()
			if !logged && len(pending) > 0 {
				log.Printf("elrs: goggle clock sync started (%d pilot goggles, trickled over each %s)", len(pending), clockResyncInterval)
				logged = true
			}
			if len(pending) == 0 {
				continue
			}
		}

		batch := clockBatchSize(len(pending))
		if batch > len(pending) {
			batch = len(pending)
		}
		for _, tgt := range pending[:batch] {
			c.journalMsg(tgt.uid, tgt.callsign, nil, "clock", "time sync")
			err := c.bp.SendRTCTo(tgt.uid, time.Now())
			c.journalOutcome(tgt.uid, "clock", err)
			if err != nil {
				log.Printf("elrs: goggle clock sync failed: %v", err)
				pending = nil
				logged = false
				break
			}
		}
		if pending != nil {
			pending = pending[batch:]
		}
	}
}
