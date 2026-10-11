package main

import (
	"testing"
	"time"
)

// The single guarantee the whole single-bus design exists to provide: a race
// call is spoken before commentary, whatever order they arrived in.
//
// With the calls in FPVTrackside and the booth beside it this could only be
// negotiated — and lost, repeatedly. On one mixer it is decided by the queue.
func TestRaceCallOutranksQueuedCommentary(t *testing.T) {
	m := newBoothMixer()

	m.enqueue(&boothCue{Text: "colour that was already waiting", seq: 1})
	m.enqueue(&boothCue{Text: "more colour", seq: 2})
	m.enqueue(&boothCue{Text: "bob9 lap 2 in 24.08", seq: 3, priority: callPriorityLap})

	m.mu.Lock()
	i := m.pickNextLocked(time.Now())
	got := m.queue[i].Text
	m.mu.Unlock()

	if got != "bob9 lap 2 in 24.08" {
		t.Fatalf("a race call must be picked first, got %q", got)
	}
}

// The heat starting outranks a lap time, which outranks colour.
func TestPriorityOrderingWithinCalls(t *testing.T) {
	m := newBoothMixer()

	m.enqueue(&boothCue{Text: "lap", seq: 1, priority: callPriorityLap})
	m.enqueue(&boothCue{Text: "go", seq: 2, priority: callPriorityRaceState})

	m.mu.Lock()
	got := m.queue[m.pickNextLocked(time.Now())].Text
	m.mu.Unlock()

	if got != "go" {
		t.Fatalf("the race starting must outrank a lap call, got %q", got)
	}
}

// Equal priority must still respect the order things happened — priority
// decides between KINDS, never between two calls of the same kind.
func TestEqualPriorityKeepsHappenedOrder(t *testing.T) {
	m := newBoothMixer()

	m.enqueue(&boothCue{Text: "second lap", seq: 9, priority: callPriorityLap})
	m.enqueue(&boothCue{Text: "first lap", seq: 4, priority: callPriorityLap})

	m.mu.Lock()
	got := m.queue[m.pickNextLocked(time.Now())].Text
	m.mu.Unlock()

	if got != "first lap" {
		t.Fatalf("two calls of the same kind must keep their order, got %q", got)
	}
}

// A due, scheduled commentary cue must still lose to a race call. Scheduling
// exists to line colour up with delayed video; it is not a way to outrank the
// racing.
func TestScheduledCommentaryStillLosesToACall(t *testing.T) {
	m := newBoothMixer()

	m.enqueue(&boothCue{Text: "scheduled colour", seq: 1, playAt: time.Now().Add(-time.Second)})
	m.enqueue(&boothCue{Text: "Race over", seq: 2, priority: callPriorityRaceState})

	m.mu.Lock()
	got := m.queue[m.pickNextLocked(time.Now())].Text
	m.mu.Unlock()

	if got != "Race over" {
		t.Fatalf("a race call must outrank due commentary, got %q", got)
	}
}
