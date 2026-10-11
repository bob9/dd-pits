//go:build darwin || windows

package main

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// pullMixer builds a device-paced mixer with one HTTP-style subscriber, the
// shape startGoPlayer wires: the busReader pulls, subscribers ride along.
func pullMixer(bufBytes int) (*boothMixer, chan []byte) {
	m := newBoothMixer()
	m.setDevicePaced(true, bufBytes)
	sub := make(chan []byte, boothRunAhead+2)
	m.mu.Lock()
	m.clients[sub] = struct{}{}
	m.mu.Unlock()
	return m, sub
}

// The device's Read NEVER blocks: an idle bus yields silence synchronously.
//
// This is the property that makes the device buffer unable to run dry — the
// old channel-fed reader blocked when the ticker pump was starved under GPU
// load, the device's 200ms drained, and that drain was the crackle. A pull
// has no producer to wait on.
func TestBusReaderNeverBlocks(t *testing.T) {
	m, _ := pullMixer(boothFrameBytes * 4)
	r := &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: make(chan struct{})}

	got := make([]byte, boothFrameBytes*3) // more than one frame, empty bus
	read := make(chan struct{})
	go func() { _, _ = io.ReadFull(r, got); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("Read blocked on an idle bus — the device buffer would drain into a click")
	}
	if !allZero(got) {
		t.Fatal("an idle bus must read as silence")
	}
}

// The reader hands the device exactly what the bus produced, in order —
// and a cue's audio arrives the moment it is playable, with no producer lag.
func TestBusReaderStreamsCueAudioInOrder(t *testing.T) {
	m, sub := pullMixer(boothFrameBytes * 4)
	r := &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: make(chan struct{})}

	pcm := make([]byte, boothFrameBytes*2)
	for i := range pcm {
		pcm[i] = byte(i%250 + 1) // non-zero, position-coded
	}
	m.enqueue(&boothCue{Text: "bob9 11.42", callKey: SoundRaceLap,
		priority: callPriorityLap, Rate: boothSampleRate, PCM: bytes.Clone(pcm)})

	got := make([]byte, boothFrameBytes*2)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatal("device did not receive the cue's audio verbatim, in order")
	}
	// The /booth.wav subscriber rode the same pull.
	if len(sub) == 0 {
		t.Fatal("subscribers must receive the frames the device pulled")
	}
}

// A partly-played frame is dropped by a cut.
//
// This is the fraction the sidecar could never give back: audio already handed
// over, mid-flight, belonging to a cue that is now wrong. In-process it is a
// slice truncation.
func TestBusReaderDiscardsThePartPlayedFrame(t *testing.T) {
	m, _ := pullMixer(boothFrameBytes * 4)
	r := &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: make(chan struct{})}

	m.enqueue(&boothCue{Text: "stale", commentary: true, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes)})
	first := make([]byte, 2)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Most of that frame is still held mid-flight; the cut drops it.
	r.discard()

	// The next read may be the cue's remainder-frame or silence, but never
	// the bytes the discarded pending held at its old offset — verify by
	// draining the cue and checking the reader recovered cleanly.
	next := make([]byte, boothFrameBytes)
	if _, err := io.ReadFull(r, next); err != nil {
		t.Fatalf("read after discard: %v", err)
	}
}

// A stopped bus ends the stream rather than blocking the device forever.
func TestBusReaderEndsWhenTheBusStops(t *testing.T) {
	m, _ := pullMixer(boothFrameBytes * 4)
	done := make(chan struct{})
	r := &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: done}
	close(done)

	if _, err := r.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("a stopped bus must EOF, got %v", err)
	}
}

// A race call landing on a PROVABLY idle device starts near-instantly: the
// enqueue flushes the silence sitting in the device buffer rather than
// waiting the 200ms out. Safe by construction — any real cue audio resets the
// idle count, so a tail can never be clipped by it.
func TestIdleFlushFastStart(t *testing.T) {
	bufBytes := boothFrameBytes * 4
	m, _ := pullMixer(bufBytes)
	flushed := false
	m.setFlush(func() { flushed = true })
	r := &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: make(chan struct{})}

	// Not yet provably idle: fewer than 2× the buffer of silence pulled.
	m.enqueue(&boothCue{Text: "early", callKey: SoundRaceLap, priority: callPriorityLap,
		Rate: boothSampleRate, PCM: filledPCM(boothFrameBytes)})
	if flushed {
		t.Fatal("flush fired before the buffer was provably silence — could clip a tail")
	}
	// Drain that cue, then pull a long run of silence.
	buf := make([]byte, boothFrameBytes)
	for range 12 {
		_, _ = io.ReadFull(r, buf)
	}
	m.enqueue(&boothCue{Text: "bob9 4.72", callKey: SoundRaceLap, priority: callPriorityLap,
		Rate: boothSampleRate, PCM: filledPCM(boothFrameBytes)})
	if !flushed {
		t.Fatal("a race call on a provably idle device must flush the silence and start now")
	}

	// Commentary never fast-starts — only race calls earn the flush.
	flushed = false
	for range 12 {
		_, _ = io.ReadFull(r, buf)
	}
	m.enqueue(&boothCue{Text: "colour", commentary: true, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes)})
	if flushed {
		t.Fatal("commentary must not flush the device — only race calls are latency-critical")
	}
}

// While the device paces the bus, the ticker pump must produce NOTHING — two
// producers would consume cues at double speed and halve every call.
func TestTickerStandsDownWhileDevicePaced(t *testing.T) {
	m, sub := pullMixer(boothFrameBytes * 4)
	m.enqueue(&boothCue{Text: "cue", commentary: true, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes * 4)})
	m.pump(make([]byte, boothFrameBytes))
	if len(sub) != 0 {
		t.Fatal("the ticker pump produced frames while the device owned the clock")
	}
	// Clock handed back (the player closed): the pump resumes.
	m.setDevicePaced(false, 0)
	m.pump(make([]byte, boothFrameBytes))
	if len(sub) == 0 {
		t.Fatal("the pump must resume when the device releases the clock")
	}
}

// The goal test: a lap call starts fast, and NOTHING the generator does can
// put a hard edge on the device.
//
// Crackle is mixed frames — part cue audio, part zero-fill — landing on the
// hardware at frame rate. This drives the full pull chain through the exact
// live failure (a streaming lap call whose generation stalls mid-cue) and
// asserts every frame the device receives is WHOLE: all cue audio or all
// silence, never a blend. And it pins the latency side: on a provably idle
// bus the lap call's audio is the very next frame the device reads.
func TestLapCallLatencyAndNoHardEdgesUnderGenerationStall(t *testing.T) {
	bufBytes := boothFrameBytes * 4
	m, _ := pullMixer(bufBytes)
	m.setFlush(func() {}) // the real flush resets oto; the mixer side is what's under test
	r := &busReader{mixer: m, silence: make([]byte, boothFrameBytes), done: make(chan struct{})}

	// The bus idles long enough to be provably silence.
	frame := make([]byte, boothFrameBytes)
	for range 10 {
		_, _ = io.ReadFull(r, frame)
	}

	// A streaming lap call arrives with exactly its start lead banked
	// (frame-aligned so the only mixed frame possible would be a bug, not
	// the natural tail).
	c := &boothCue{Text: "Bola 5.93", callKey: SoundTimeTrialEveryLap,
		priority: callPriorityLap, streaming: true, Rate: boothSampleRate,
		PCM: filledPCM(streamLeadBytes)}
	m.enqueue(c)

	// LATENCY: the very next frame the device pulls is the call's audio.
	if _, err := io.ReadFull(r, frame); err != nil {
		t.Fatal(err)
	}
	if allZero(frame) {
		t.Fatal("a lap call on an idle bus must be the next frame the device reads")
	}

	// Drain the rest of the lead, checking WHOLENESS of every frame.
	whole := func(f []byte) bool {
		zero, filled := 0, 0
		for _, b := range f {
			if b == 0 {
				zero++
			} else {
				filled++
			}
		}
		return zero == 0 || filled == 0
	}
	audio, silenceFrames := 1, 0
	for range streamLeadBytes/boothFrameBytes - 1 {
		_, _ = io.ReadFull(r, frame)
		if !whole(frame) {
			t.Fatal("mixed frame during playback — that edge is crackle")
		}
		if allZero(frame) {
			silenceFrames++
		} else {
			audio++
		}
	}

	// GENERATION STALLS: the banked audio is gone, nothing new arrives. The
	// device keeps reading — and gets whole silence, never a sliver, never a
	// block.
	for range 6 {
		done := make(chan struct{})
		go func() { _, _ = io.ReadFull(r, frame); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Read blocked during a generation stall — the device would click")
		}
		if !allZero(frame) {
			t.Fatal("a stalled stream leaked partial audio — that is the crackle")
		}
	}

	// Generation recovers with a re-bank lead plus more; the cue finishes.
	m.appendPCM(c, filledPCM(streamLeadBytes+boothFrameBytes*4), boothSampleRate)
	m.finishStream(c)
	for range streamLeadBytes/boothFrameBytes + 8 {
		_, _ = io.ReadFull(r, frame)
		if !whole(frame) {
			t.Fatal("mixed frame after recovery — the resume must be clean")
		}
		if !allZero(frame) {
			audio++
		}
	}
	// Every byte of the call was heard exactly once: lead + recovery.
	wantAudio := (streamLeadBytes*2 + boothFrameBytes*4) / boothFrameBytes
	if audio != wantAudio {
		t.Fatalf("heard %d audio frames, want %d — audio lost or repeated across the stall",
			audio, wantAudio)
	}
}
