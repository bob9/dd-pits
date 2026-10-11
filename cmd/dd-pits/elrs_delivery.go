package main

// Goggle message delivery journal. Every logical message dd-pits sends to a
// pilot's goggles (DVR label, GO!, lap line, clock sync, channel change, …)
// is journalled with its pilot, race and human-readable content. A netpack
// running the delivery-report firmware extension (elrs.FuncSendReport) acks
// each forwarded ESP-NOW frame back over serial; those reports attach to the
// journal entry so it finalizes as delivered / partial / failed. Without the
// extension entries finalize as "sent" (unknown — never assumed failed).
// Failures are logged as they finalize; finalized entries are then discarded.

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

const (
	// deliverySettle is how long an entry stays open for reports after its
	// last send. Channel changes repeat over ~3s, so the window must outlast
	// them; everything else settles in milliseconds.
	deliverySettle = 6 * time.Second
	// deliverySweepEvery paces the journal sweep.
	deliverySweepEvery = 10 * time.Second
	// deliveryMaxBacklog bounds the finalized-entry buffer; oldest entries are
	// dropped beyond it.
	deliveryMaxBacklog = 4000
	// deliveryBatchMax caps one drain.
	deliveryBatchMax = 500
)

type deliveryEntry struct {
	SentAt      time.Time
	EventSource string
	RaceSource  string
	RaceName    string
	Pilot       string
	UID         elrs.UID
	Kind        string
	Message     string
	Frames      int // netpack reports received
	OK          int
	Failed      int
	Retries     int  // worst per-frame retry count
	SendErr     bool // the serial write itself failed — never left the agent
}

func (e *deliveryEntry) status() string {
	switch {
	case e.SendErr:
		return elrs.DeliveryStatusFailed
	case e.Frames == 0:
		return elrs.DeliveryStatusSent
	case e.Failed == 0:
		return elrs.DeliveryStatusDelivered
	case e.OK == 0:
		return elrs.DeliveryStatusFailed
	default:
		return elrs.DeliveryStatusPartial
	}
}

// deliveryClass collapses kinds and MSP functions onto correlation channels:
// a report for a function attaches to the open entry on the same (uid,
// class). Sends per UID are serialized by the backpack mutex and a new entry
// closes the previous one on its channel, so "the open entry" is unambiguous.
func deliveryClassOfKind(kind string) string {
	switch kind {
	case "dvr_label", "dvr_clear":
		return "dvr"
	case "clock":
		return "clock"
	case "channel":
		return "channel"
	default:
		return "osd"
	}
}

func deliveryClassOfFunc(fn elrs.Function) string {
	switch fn {
	case elrs.FuncSetDVRName:
		return "dvr"
	case elrs.FuncSetRTC:
		return "clock"
	case elrs.FuncSetVTXConfig:
		return "channel"
	case elrs.FuncSetOSD:
		return "osd"
	default:
		return ""
	}
}

type journalKey struct {
	uid   elrs.UID
	class string
}

type deliveryJournal struct {
	mu      sync.Mutex
	open    map[journalKey]*deliveryEntry
	done    []*deliveryEntry
	dropped int // finalized entries lost to the backlog cap
	now     func() time.Time
}

func newDeliveryJournal() *deliveryJournal {
	return &deliveryJournal{open: map[journalKey]*deliveryEntry{}, now: time.Now}
}

// record opens a new entry, finalizing any prior open entry on the same
// (uid, class) channel. Call just before the frames are sent so concurrent
// inbound reports attach to it.
func (j *deliveryJournal) record(uid elrs.UID, pilot, eventSource, raceSource, raceName, kind, message string) {
	key := journalKey{uid, deliveryClassOfKind(kind)}
	j.mu.Lock()
	defer j.mu.Unlock()
	if prev := j.open[key]; prev != nil {
		j.finalizeLocked(key, prev)
	}
	j.open[key] = &deliveryEntry{
		SentAt: j.now(), EventSource: eventSource, RaceSource: raceSource,
		RaceName: raceName, Pilot: pilot, UID: uid, Kind: kind, Message: message,
	}
}

// report attaches a netpack delivery report to the matching open entry.
// Reports with no open entry (auxiliary frames like the RTC paired with a
// DVR label when no clock entry is open) are dropped.
func (j *deliveryJournal) report(rep elrs.SendReport) {
	class := deliveryClassOfFunc(rep.Function)
	if class == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.open[journalKey{rep.UID, class}]; e != nil {
		e.Frames++
		if rep.Delivered {
			e.OK++
		} else {
			e.Failed++
		}
		if rep.Retries > e.Retries {
			e.Retries = rep.Retries
		}
	}
}

// failOpen marks the open entry on (uid, kind's class) as a local send
// failure — the serial write errored, so no frames left the agent.
func (j *deliveryJournal) failOpen(uid elrs.UID, kind string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.open[journalKey{uid, deliveryClassOfKind(kind)}]; e != nil {
		e.SendErr = true
	}
}

// dropOpen discards the open entry on (uid, kind's class) — used when the
// backpack turned out to be disconnected, so no message was attempted.
func (j *deliveryJournal) dropOpen(uid elrs.UID, kind string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.open, journalKey{uid, deliveryClassOfKind(kind)})
}

// sweep finalizes entries whose settle window has passed.
func (j *deliveryJournal) sweep() {
	now := j.now()
	j.mu.Lock()
	defer j.mu.Unlock()
	for key, e := range j.open {
		if now.Sub(e.SentAt) >= deliverySettle {
			j.finalizeLocked(key, e)
		}
	}
}

func (j *deliveryJournal) finalizeLocked(key journalKey, e *deliveryEntry) {
	delete(j.open, key)
	if st := e.status(); st == elrs.DeliveryStatusFailed || st == elrs.DeliveryStatusPartial {
		log.Printf("elrs: delivery %s for %s: %s %q (%d/%d frames acked)",
			strings.ToUpper(st), e.Pilot, e.Kind, e.Message, e.OK, e.Frames)
	}
	if len(j.done) >= deliveryMaxBacklog {
		j.done = j.done[1:]
		j.dropped++
	}
	j.done = append(j.done, e)
}

// drain pops up to max finalized entries for upload.
func (j *deliveryJournal) drain(max int) []*deliveryEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := min(max, len(j.done))
	if n == 0 {
		return nil
	}
	out := j.done[:n]
	j.done = append([]*deliveryEntry(nil), j.done[n:]...)
	return out
}

// requeue puts a failed upload batch back at the head of the queue, still
// honouring the backlog cap.
func (j *deliveryJournal) requeue(batch []*deliveryEntry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done = append(append([]*deliveryEntry(nil), batch...), j.done...)
	if over := len(j.done) - deliveryMaxBacklog; over > 0 {
		j.done = j.done[over:]
		j.dropped += over
	}
}

// journalMsg opens a journal entry for a message about to be sent to one
// pilot's goggles. No-op with no connected backpack (nothing will be
// attempted) — sites still call the sender, whose ErrNotConnected is benign.
func (c *elrsController) journalMsg(uid elrs.UID, pilot string, race *vmRace, kind, msg string) {
	if c.journal == nil || !c.bp.Connected() {
		return
	}
	c.mu.Lock()
	eventSource := ""
	if c.lastMeeting != nil {
		eventSource = c.lastMeeting.MeetingID
	}
	c.mu.Unlock()
	raceSource, raceName := "", ""
	if race != nil {
		raceSource, raceName = race.ID, race.Label
	}
	c.journal.record(uid, pilot, eventSource, raceSource, raceName, kind, msg)
}

// journalOutcome closes the loop on a send attempt: a serial error marks the
// entry failed; ErrNotConnected discards it (nothing was attempted).
func (c *elrsController) journalOutcome(uid elrs.UID, kind string, err error) {
	if c.journal == nil || err == nil {
		return
	}
	if err == elrs.ErrNotConnected {
		c.journal.dropOpen(uid, kind)
		return
	}
	c.journal.failOpen(uid, kind)
}

// handleInbound is the Backpack OnPacket hook: delivery reports feed the
// journal; a transmitter backpack's DVR switch traffic is just logged.
func (c *elrsController) handleInbound(p elrs.Packet) {
	if rep, ok := elrs.ParseSendReport(p); ok {
		c.mu.Lock()
		c.reportsSeen = true
		c.mu.Unlock()
		if c.journal != nil {
			c.journal.report(rep)
		}
		if rep.Function == elrs.FuncSetOSD {
			c.noteGoFrame(rep.UID, rep.Delivered)
		}
		return
	}
	if p.Type == elrs.TypeCommand && p.Function == elrs.FuncSetRecordingState && len(p.Payload) > 0 {
		log.Printf("elrs: inbound recording-state %d from a transmitter backpack (ignored)", p.Payload[0])
	}
}

// deliveryLoop periodically finalizes settled journal entries and drains them.
func (c *elrsController) deliveryLoop(ctx context.Context) {
	ticker := time.NewTicker(deliverySweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.journal.sweep()
		for len(c.journal.drain(deliveryBatchMax)) > 0 {
		}
	}
}
