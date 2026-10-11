package main

import "testing"

// TestCommentaryCannotOutrankARaceCall pins the single-bus guarantee.
//
// The booth and the announcer both set priority on the same field, but their
// scales were chosen independently: the booth's big beats (finish, holeshot)
// use 5, which is exactly callPriorityLap. Equal priority falls through to
// arrival order, so a commentary cue queued first went to air ahead of a lap
// time — the opposite of what the single bus exists to guarantee.
func TestCommentaryCannotOutrankARaceCall(t *testing.T) {
	m := newBoothMixer()

	// A booth cue claiming the same priority as a lap call. Commentary carries
	// no callKey; that is what distinguishes it.
	com := &boothCue{Text: "big finish", commentary: true,
		priority: callPriorityLap, Rate: boothSampleRate}
	m.enqueue(com)
	if com.priority >= callPriorityLap {
		t.Errorf("commentary priority %d must be below callPriorityLap (%d)",
			com.priority, callPriorityLap)
	}

	// A race call keeps whatever priority it was given.
	lap := &boothCue{Text: "bob9 11.42", callKey: SoundRaceLap,
		priority: callPriorityLap, Rate: boothSampleRate}
	m.enqueue(lap)
	if lap.priority != callPriorityLap {
		t.Errorf("a race call's priority must not be clamped, got %d", lap.priority)
	}

	// And a RELAYED call whose sound could not be identified — no callKey at
	// all, seen live as "call [] lap … Fizz 0.98" — is still a race call and
	// must keep its priority. Keying the clamp on an empty callKey would have
	// demoted exactly these.
	unknown := &boothCue{Text: "Fizz 0.98", priority: callPriorityLap, Rate: boothSampleRate}
	m.enqueue(unknown)
	if unknown.priority != callPriorityLap {
		t.Errorf("an unidentified relayed call is still a race call, got %d", unknown.priority)
	}
}

// TestCommentaryNeverInterruptsARaceCall covers race calls being chopped off
// the bus when commentary arrived.
//
// The booth cuts its own smalltalk when a heat goes green, and identified it as
// "no RaceSrc". But RaceSrc is only ever set on BOOTH cues, so every RACE CALL
// matched too and was cut with it — heard as "Next up Round 5" stopping dead
// when the post-race summary landed, and as three of four pilots never having
// their channel announced.
func TestCommentaryNeverInterruptsARaceCall(t *testing.T) {
	m := newBoothMixer()

	call := &boothCue{Text: "bob9 on R1", callKey: SoundAnnouncePilotChannel,
		priority: callPriorityLap, Rate: boothSampleRate,
		PCM: make([]byte, boothFrameBytes*8)}
	smalltalk := &boothCue{Text: "and what a meeting it has been", commentary: true,
		Rate: boothSampleRate, PCM: make([]byte, boothFrameBytes*20)}
	m.enqueue(call)
	m.enqueue(smalltalk)

	// The same predicate the booth uses when a heat goes green.
	m.interrupt(func(c *boothCue) bool {
		return c.commentary && (c.RaceSrc == "" || c.Kind == "meeting_talk")
	})

	if m.pending() != 1 {
		t.Fatalf("expected the race call to survive and the smalltalk to go, %d left", m.pending())
	}
	m.mu.Lock()
	survived := m.queue[0].Text
	m.mu.Unlock()
	if survived != "bob9 on R1" {
		t.Errorf("the RACE CALL must survive; %q did instead", survived)
	}
}

// The booth must not start generating while a race call is on air or queued —
// the writer starves the voice on a shared GPU, and a starved streaming call
// holds the bus on silence. Measured: "finished in 4th" took 29s to first audio
// while the finish wrap was being written.
func TestRaceCallBusyGatesBoothGeneration(t *testing.T) {
	m := newBoothMixer()
	if m.raceCallBusy() {
		t.Fatal("an empty bus reports race calls busy — the booth would never speak")
	}
	// Commentary alone does not gate: the booth may write while its own colour airs.
	m.enqueue(&boothCue{Text: "colour", commentary: true,
		PCM: make([]byte, boothFrameBytes*4), Rate: boothSampleRate})
	m.nextFrame(make([]byte, boothFrameBytes))
	if m.raceCallBusy() {
		t.Error("the booth's own commentary gates its generation — it would deadlock itself")
	}
	// A queued race call gates, even before it airs.
	m.enqueue(&boothCue{Text: "bob9 4.72", callKey: SoundRaceLap,
		PCM: make([]byte, boothFrameBytes*2), Rate: boothSampleRate})
	if !m.raceCallBusy() {
		t.Error("a queued race call does not gate generation — the writer would starve its voice")
	}
}

// A starved streaming cue must hold CLEAN silence, not play slivers.
//
// The old path copied whatever bytes had arrived into the 50ms frame and
// zero-filled the rest — a hard edge into silence on every starved frame,
// twenty a second, heard as crackle at the start of "Next up…" that "comes
// good after a second or two" once the model out-runs playback. The fix is
// two-part: never consume a partial frame while the stream is live, and once
// starved, re-bank a full lead before resuming so recovery is one clean pause
// rather than a burst of starve/resume edges.
func TestStarvedStreamHoldsCleanSilenceAndRebanks(t *testing.T) {
	m := newBoothMixer()
	silence := make([]byte, boothFrameBytes)

	c := &boothCue{Text: "Next up Round 4 Race 2", callKey: SoundRaceAnnounce,
		streaming: true, Rate: boothSampleRate,
		PCM: filledPCM(streamLeadBytes)} // exactly the start lead
	m.enqueue(c)

	// Drain what was banked.
	for range streamLeadBytes / boothFrameBytes {
		if f := m.nextFrame(silence); allZero(f) {
			t.Fatal("banked audio played as silence")
		}
	}

	// The generator dribbles in HALF a frame. The old code played it as a
	// sliver; it must now hold silence and consume nothing.
	m.appendPCM(c, filledPCM(boothFrameBytes/2), boothSampleRate)
	before := m.curOffLocked()
	if f := m.nextFrame(silence); !allZero(f) {
		t.Fatal("a starved streaming cue played a partial frame — that edge is the crackle")
	}
	if m.curOffLocked() != before {
		t.Fatal("a starved frame consumed audio — the sliver would be lost or chopped")
	}

	// One more frame's worth arrives — still short of the re-bank lead, so the
	// hold continues even though a full frame now exists.
	m.appendPCM(c, filledPCM(boothFrameBytes), boothSampleRate)
	if f := m.nextFrame(silence); !allZero(f) {
		t.Fatal("resumed on a bare frame — starve/resume flapping recreates the edges")
	}

	// Re-banked a full lead: playback resumes.
	m.appendPCM(c, filledPCM(streamLeadBytes), boothSampleRate)
	if f := m.nextFrame(silence); allZero(f) {
		t.Fatal("did not resume after re-banking a full lead")
	}

	// And when the stream FINISHES, the tail shorter than a frame plays out —
	// the natural end of the audio is not starvation.
	m.finishStream(c)
	drained := false
	for range (streamLeadBytes*2)/boothFrameBytes + 4 {
		m.nextFrame(silence)
		m.mu.Lock()
		drained = m.cur == nil
		m.mu.Unlock()
		if drained {
			break
		}
	}
	if !drained {
		t.Fatal("a finished stream never released the bus")
	}
}

// Moment-untied calls bank a full second before starting; the perishable ones
// keep the 300ms immediacy — a lap time late is wrong, but "Next up…" starting
// 700ms later and CLEAN gives up nothing.
func TestNextUpBanksAFullSecondBeforeAiring(t *testing.T) {
	nextUp := &boothCue{streaming: true, leadBytes: callStreamLead(SoundRaceAnnounce),
		PCM: make([]byte, streamLeadBytes)}
	if nextUp.playable() {
		t.Fatal("next-up aired on the 300ms lead — the cold first second starves it")
	}
	nextUp.PCM = make([]byte, boothSampleRate*2)
	if !nextUp.playable() {
		t.Fatal("next-up not playable with a full second banked")
	}

	lap := &boothCue{streaming: true, leadBytes: callStreamLead(SoundRaceLap),
		PCM: make([]byte, streamLeadBytes)}
	if !lap.playable() {
		t.Fatal("a lap call lost its immediacy — late is wrong, not slow")
	}
}

// filledPCM is non-zero audio, so silence can be told from signal.
func filledPCM(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x11
	}
	return b
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// curOffLocked reads the play offset of the cue on air.
func (m *boothMixer) curOffLocked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.curOff
}

// The bus buffers deep ONLY while a cue is on air.
//
// The device's 200ms was not enough cover while a 30B writer pegged the
// machine — the post-race wrap dropped out mid-word. But buffering deep at
// all times would start every lap call late, so the depth is adaptive: idle
// ticks stay shallow (one frame per tick — a fresh call starts as fast as
// ever), and a playing cue tops the client buffers up (tail latency nobody
// hears buys the cover).
func TestPumpRunsAheadOnlyMidCue(t *testing.T) {
	m := newBoothMixer()
	silence := make([]byte, boothFrameBytes)
	ch := make(chan []byte, boothRunAhead+2)
	m.mu.Lock()
	m.clients[ch] = struct{}{}
	m.mu.Unlock()

	// Idle: one frame per tick, nothing more.
	m.pump(silence)
	if len(ch) != 1 {
		t.Fatalf("an idle pump must emit exactly one frame, got %d", len(ch))
	}
	for len(ch) > 0 {
		<-ch
	}

	// A cue on air: the pump tops the buffer up, leaving one slot of headroom
	// so the next scheduled tick's frame never becomes the one that drops.
	m.enqueue(&boothCue{Text: "the wrap", commentary: true, Rate: boothSampleRate,
		PCM: filledPCM(boothFrameBytes * 40)})
	m.pump(silence)
	if len(ch) != cap(ch)-1 {
		t.Fatalf("a playing cue must fill the buffer to cap-1 (%d), got %d", cap(ch)-1, len(ch))
	}

	// And the frames it ran ahead with are the CUE's audio, not silence.
	if f := <-ch; allZero(f) {
		t.Fatal("run-ahead emitted silence while a cue was playing")
	}
}

// A LIVE STREAM is never topped up — the burst eats its banked lead.
//
// A holeshot banks 300ms and airs; the top-up consumed all of it at the first
// tick, so the call starved instantly, re-banked, starved again — heard live
// as "a huge delay to say holeshot bola and it was crackly". A streaming cue
// stays tick-paced until its generation FINISHES; only then does the deep
// buffer take over for the tail.
func TestPumpNeverRunsAheadIntoALiveStream(t *testing.T) {
	m := newBoothMixer()
	silence := make([]byte, boothFrameBytes)
	ch := make(chan []byte, boothRunAhead+2)
	m.mu.Lock()
	m.clients[ch] = struct{}{}
	m.mu.Unlock()

	c := &boothCue{Text: "Holeshot Bola 2.37", callKey: SoundHoleshot,
		streaming: true, Rate: boothSampleRate, PCM: filledPCM(streamLeadBytes)}
	m.enqueue(c)

	// While the stream holds ONLY its lead: one frame per tick, lead untouched.
	m.pump(silence)
	if len(ch) != 1 {
		t.Fatalf("a live stream with no surplus must stay tick-paced, got %d frames", len(ch))
	}
	for len(ch) > 0 {
		<-ch
	}

	// Generation pulls ahead: the SURPLUS above the lead may flow into the
	// device pipeline (cover for pump stalls under GPU load), but the lead
	// itself is never consumed by the top-up.
	m.appendPCM(c, filledPCM(streamLeadBytes*3), boothSampleRate)
	m.pump(silence)
	if len(ch) < 2 {
		t.Fatal("a stream with surplus banked should be topped up")
	}
	m.mu.Lock()
	remaining := len(c.PCM) - m.curOff
	m.mu.Unlock()
	if remaining < streamLeadBytes {
		t.Fatalf("the top-up consumed the stream's lead: %d bytes left, lead is %d",
			remaining, streamLeadBytes)
	}
	for len(ch) > 0 {
		<-ch
	}

	// The stream finishes: the tail is fully-rendered audio now, and the deep
	// buffer may cover it.
	m.appendPCM(c, filledPCM(boothFrameBytes*20), boothSampleRate)
	m.finishStream(c)
	m.pump(silence)
	if len(ch) != cap(ch)-1 {
		t.Fatalf("a finished stream's tail should be topped up to cap-1 (%d), got %d",
			cap(ch)-1, len(ch))
	}
}
