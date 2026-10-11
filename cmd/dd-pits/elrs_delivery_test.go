package main

import (
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

// journalOpenSnapshot returns a copy of the journal's open entries (test helper).
func journalOpenSnapshot(j *deliveryJournal) []deliveryEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]deliveryEntry, 0, len(j.open))
	for _, e := range j.open {
		out = append(out, *e)
	}
	return out
}

// waitJournalFrames polls until the open entry matching pilot+kind has at
// least want report frames attached.
func waitJournalFrames(t *testing.T, j *deliveryJournal, pilot, kind string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, e := range journalOpenSnapshot(j) {
			if e.Pilot == pilot && e.Kind == kind && e.Frames >= want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal entry %s/%s never reached %d frames: %+v", pilot, kind, want, journalOpenSnapshot(j))
		}
		time.Sleep(time.Millisecond)
	}
}

// finalizeAll force-sweeps every open entry by pretending time has passed.
func finalizeAll(j *deliveryJournal) []*deliveryEntry {
	old := j.now
	j.now = func() time.Time { return time.Now().Add(deliverySettle + time.Second) }
	j.sweep()
	j.now = old
	return j.drain(1000)
}

func findEntry(entries []*deliveryEntry, pilot, kind string) *deliveryEntry {
	for _, e := range entries {
		if e.Pilot == pilot && e.Kind == kind {
			return e
		}
	}
	return nil
}

// TestDeliveryJournalEndToEnd drives a stage through a report-capable fake
// netpack: the DVR label and stage OSD are journalled per pilot with their
// race, MAC-ack reports attach, and entries finalize as delivered.
func TestDeliveryJournalEndToEnd(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()
	fake.AutoReport(true)

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))      // prime
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil)) // stage

	// The DVR-name frame per pilot gets acked back.
	waitJournalFrames(t, c.journal, "Alice", "dvr_label", 1)
	waitJournalFrames(t, c.journal, "Bob", "dvr_label", 1)

	entries := finalizeAll(c.journal)
	lbl := findEntry(entries, "Alice", "dvr_label")
	if lbl == nil {
		t.Fatalf("no dvr_label entry for Alice in %d entries", len(entries))
	}
	if lbl.status() != elrs.DeliveryStatusDelivered {
		t.Fatalf("dvr_label status = %s (%+v), want delivered", lbl.status(), lbl)
	}
	if lbl.Message != "ClubNight-R1-H1" || lbl.RaceSource != "race1" || lbl.EventSource == "" {
		t.Fatalf("dvr_label context wrong: %+v", lbl)
	}
	if stage := findEntry(entries, "Alice", "osd_stage"); stage == nil || stage.status() != elrs.DeliveryStatusDelivered {
		t.Fatalf("osd_stage entry missing or not delivered: %+v", stage)
	}
}

// TestDeliveryJournalFailedPilot pins the failure path: a pilot whose goggles
// never ack (off / out of range) finalizes as failed while others deliver.
func TestDeliveryJournalFailedPilot(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()
	fake.AutoReport(true)
	fake.FailDeliveriesTo(elrs.UIDFromPhrase("Alice"), true)

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil))
	waitJournalFrames(t, c.journal, "Alice", "dvr_label", 1)
	waitJournalFrames(t, c.journal, "Bob", "dvr_label", 1)

	entries := finalizeAll(c.journal)
	if e := findEntry(entries, "Alice", "dvr_label"); e == nil || e.status() != elrs.DeliveryStatusFailed {
		t.Fatalf("Alice's label should be failed: %+v", e)
	}
	if e := findEntry(entries, "Bob", "dvr_label"); e == nil || e.status() != elrs.DeliveryStatusDelivered {
		t.Fatalf("Bob's label should be delivered: %+v", e)
	}
}

// TestDeliveryJournalNoReportsMeansSent: without the netpack firmware
// extension no reports arrive, and entries finalize as "sent" (unknown),
// never failed.
func TestDeliveryJournalNoReportsMeansSent(t *testing.T) {
	c, _, cancel := elrsTestRig(t) // AutoReport off = old firmware
	defer cancel()

	c.processSnapshot(raceSnap(t, "", false, false, nil, nil))
	c.processSnapshot(raceSnap(t, "race1", false, false, nil, nil))
	// Sends are synchronous; the entries exist by the time processSnapshot
	// returns, with no reports ever arriving.

	entries := finalizeAll(c.journal)
	if e := findEntry(entries, "Alice", "dvr_label"); e == nil || e.status() != elrs.DeliveryStatusSent {
		t.Fatalf("without reports the label should finalize as sent: %+v", e)
	}
}

// TestDeliveryStatusTable pins the status derivation.
func TestDeliveryStatusTable(t *testing.T) {
	cases := []struct {
		e    deliveryEntry
		want string
	}{
		{deliveryEntry{}, elrs.DeliveryStatusSent},
		{deliveryEntry{Frames: 3, OK: 3}, elrs.DeliveryStatusDelivered},
		{deliveryEntry{Frames: 3, Failed: 3}, elrs.DeliveryStatusFailed},
		{deliveryEntry{Frames: 3, OK: 2, Failed: 1}, elrs.DeliveryStatusPartial},
		{deliveryEntry{SendErr: true}, elrs.DeliveryStatusFailed},
	}
	for i, tc := range cases {
		if got := tc.e.status(); got != tc.want {
			t.Errorf("case %d: status = %s, want %s", i, got, tc.want)
		}
	}
}
