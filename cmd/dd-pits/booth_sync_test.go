package main

import (
	"testing"
	"time"
)

func TestBoothSyncRatchetsUpOnly(t *testing.T) {
	s := newBoothSync()
	if got := s.buffer(); got != boothSyncFloorMs {
		t.Fatalf("initial buffer = %d, want floor %d", got, boothSyncFloorMs)
	}
	// A latency comfortably under the floor keeps the floor.
	if got := s.observe(3000); got != boothSyncFloorMs {
		t.Fatalf("small latency changed buffer to %d, want floor", got)
	}
	// A big latency raises the buffer to clear it + margin (rounded up).
	want := roundUpMs(25000+boothSyncMarginMs, boothSyncStepMs) // 30000
	if got := s.observe(25000); got != want {
		t.Fatalf("buffer after 25s latency = %d, want %d", got, want)
	}
	// A later smaller latency must NOT shrink the buffer (no video skip).
	if got := s.observe(6000); got != want {
		t.Fatalf("buffer shrank to %d after a fast call, want it held at %d", got, want)
	}
	if st := s.stats(); st.MaxLatMs != 25000 || st.LastLatMs != 6000 {
		t.Fatalf("stats last=%d max=%d, want last=6000 max=25000", st.LastLatMs, st.MaxLatMs)
	}
}

func TestBoothSyncCeiling(t *testing.T) {
	s := newBoothSync()
	if got := s.observe(10 * boothSyncCeilMs); got != boothSyncCeilMs {
		t.Fatalf("buffer = %d, want clamped to ceil %d", got, boothSyncCeilMs)
	}
}

func TestBoothSyncSetFloor(t *testing.T) {
	s := newBoothSync()
	// Raising the floor lifts the starting buffer.
	s.setFloor(40000)
	if got := s.buffer(); got != 40000 {
		t.Fatalf("buffer after setFloor(40s) = %d, want 40000", got)
	}
	// A small call can't drop below the new floor.
	if got := s.observe(5000); got != 40000 {
		t.Fatalf("buffer after a fast call = %d, want held at floor 40000", got)
	}
	// A slower-than-floor call still ratchets above it.
	want := roundUpMs(50000+boothSyncMarginMs, boothSyncStepMs)
	if got := s.observe(50000); got != want {
		t.Fatalf("buffer after 50s latency = %d, want %d", got, want)
	}
	// A floor set above the sanity ceiling is honoured, not clamped down.
	s2 := newBoothSync()
	s2.setFloor(boothSyncCeilMs + 30000)
	if got := s2.observe(1000); got != boothSyncCeilMs+30000 {
		t.Fatalf("explicit floor above ceil = %d, want honoured %d", got, boothSyncCeilMs+30000)
	}
	// A lower floor is a no-op (never shrinks).
	s.setFloor(1000)
	if got := s.buffer(); got != want {
		t.Fatalf("setFloor below current shrank buffer to %d, want %d", got, want)
	}
}

func TestSplitDelay(t *testing.T) {
	cases := []struct {
		total int64
		want  []int64
	}{
		{0, nil},
		{20000, []int64{20000}},
		{30000, []int64{20000, 10000}},
		{45000, []int64{20000, 20000, 5000}},
		{20001, []int64{20000, 1}},
	}
	for _, c := range cases {
		got := splitDelay(c.total, obsFilterCapMs)
		if len(got) != len(c.want) {
			t.Fatalf("splitDelay(%d) = %v, want %v", c.total, got, c.want)
		}
		var sum int64
		for i, v := range got {
			if v != c.want[i] || v > obsFilterCapMs {
				t.Fatalf("splitDelay(%d) = %v, want %v", c.total, got, c.want)
			}
			sum += v
		}
		if sum != c.total {
			t.Fatalf("splitDelay(%d) sums to %d, not %d", c.total, sum, c.total)
		}
	}
}

// A scheduled cue is held until its mark; a due scheduled call out-ranks filler.
func TestMixerHoldsAndPreempts(t *testing.T) {
	now := time.Now()
	m := newBoothMixer()
	filler := &boothCue{Text: "filler", PCM: make([]byte, boothFrameBytes)} // playAt zero
	future := &boothCue{Text: "future", PCM: make([]byte, boothFrameBytes), playAt: now.Add(time.Hour)}
	m.queue = []*boothCue{future}

	// Only a future-scheduled cue queued → nothing airs yet, not "busy".
	if idx := m.pickNextLocked(now); idx != -1 {
		t.Fatalf("pickNextLocked chose %d, want -1 (future cue must be held)", idx)
	}
	if m.busy() {
		t.Fatal("busy() true while only a future cue is queued")
	}

	// Filler + a DUE scheduled cue → the due action call wins.
	due := &boothCue{Text: "due", PCM: make([]byte, boothFrameBytes), playAt: now.Add(-time.Second)}
	m.queue = []*boothCue{filler, due}
	if idx := m.pickNextLocked(now); idx != 1 {
		t.Fatalf("pickNextLocked chose %d, want 1 (the due scheduled cue)", idx)
	}
	if idx := m.dueScheduledIdxLocked(now); idx != 1 {
		t.Fatalf("dueScheduledIdxLocked = %d, want 1", idx)
	}

	// Earliest mark wins among two due scheduled cues.
	early := &boothCue{Text: "early", PCM: make([]byte, boothFrameBytes), playAt: now.Add(-3 * time.Second)}
	m.queue = []*boothCue{due, early}
	if idx := m.pickNextLocked(now); m.queue[idx].Text != "early" {
		t.Fatalf("pickNextLocked chose %q, want the earliest-mark cue", m.queue[idx].Text)
	}
}

// A cue that has started plays to completion — a due scheduled call waits its
// turn rather than cutting a commentator off mid-sentence.
func TestNextFrameDoesNotCutPlayingCue(t *testing.T) {
	m := newBoothMixer()
	playing := &boothCue{Text: "commentary", PCM: make([]byte, boothFrameBytes*4)}
	m.cur = playing
	m.curOff = boothFrameBytes // mid-play
	due := &boothCue{Text: "action", PCM: make([]byte, boothFrameBytes*2), playAt: time.Now().Add(-time.Second)}
	m.queue = []*boothCue{due}

	m.nextFrame(make([]byte, boothFrameBytes))
	if m.cur != playing {
		t.Fatalf("playing cue was cut off (cur=%v); it must finish first", m.cur)
	}
	if len(m.queue) != 1 {
		t.Fatalf("the due call should still be waiting in the queue, got %d", len(m.queue))
	}
}
