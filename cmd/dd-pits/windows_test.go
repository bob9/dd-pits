package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Windows writes CRLF, and FPVTrackside's log is written by FPVTrackside — so
// every line the relay reads there ends "\r\n". bufio.Scanner strips only the
// \n, leaving a carriage return on the end of the captured text.
//
// A stray \r is invisible in a log and poisonous downstream: it lands inside
// the spoken string, breaks the pack lookup (whose key is hashed), and shows up
// in the audio as nothing at all while the call silently falls back to live
// generation.
func TestSpeechLogHandlesWindowsLineEndings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Log.txt")
	if err := os.WriteFile(path, []byte("start\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewSpeechLogWatcher(path)
	if _, err := w.Poll(); err != nil { // prime
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "SOUNDLOG (Notice) - SpeechManager: TTS (Hurry up 'bob9')\r\n")
	fmt.Fprint(f, "SOUNDLOG (Notice) - SpeechManager: TTS (Race over)\r\n")
	f.Close()

	lines, err := w.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("read %d lines, want 2", len(lines))
	}
	for i, want := range []string{"Hurry up bob9", "Race over"} {
		if lines[i].Text != want {
			t.Errorf("line %d = %q, want %q (a trailing \\r would not show here — check the bytes)",
				i, lines[i].Text, want)
		}
		for _, r := range lines[i].Text {
			if r == '\r' || r == '\n' {
				t.Errorf("line %d still carries a line-ending character: %q", i, lines[i].Text)
			}
		}
	}
}

// The pack key is hashed, so any invisible character makes every lookup miss
// and the call falls back to live generation — slower, and silently so.
func TestPackKeyIsUnaffectedByCarriageReturns(t *testing.T) {
	if packFileName("Race over") != packFileName("Race over\r") {
		t.Error("a trailing carriage return changes the pack filename — every Windows lookup would miss")
	}
	if packNormalise("bob9 on R1\r\n") != "bob9 on r1" {
		t.Errorf("normalise left line endings in: %q", packNormalise("bob9 on R1\r\n"))
	}
}

// TestAuditDistinguishesDerivedFromRelayed.
//
// The coupling that makes this worth reporting: dd-pits takes its wording from
// Sounds.xml and honours the enabled flag, so switching off a call it DERIVES
// makes it silent everywhere — dd-pits will not say it and FPVTrackside is
// muted. That happened during testing and cost a long stretch of confusion,
// with one buried log line as the only trace.
func TestAuditDistinguishesDerivedFromRelayed(t *testing.T) {
	set := &SoundSet{byKey: map[SoundKey]Sound{
		SoundRaceLap:           {Key: SoundRaceLap, Text: "{pilot} lap {lapnumber}", Enabled: true},
		SoundHurryUp:           {Key: SoundHurryUp, Text: "Hurry up {pilot}", Enabled: true},
		SoundRaceDone:          {Key: SoundRaceDone, Text: "{pilot} finished", Enabled: false},
		SoundPilotsEnableVideo: {Key: SoundPilotsEnableVideo, Text: "video on", Enabled: false},
	}, loaded: true}

	roles := map[SoundKey]soundRole{}
	for _, a := range auditSounds(set) {
		roles[a.Key] = a.Role
	}

	if roles[SoundRaceLap] != roleDerived {
		t.Error("RaceLap is built from the timing data")
	}
	if roles[SoundHurryUp] != roleRelayed {
		t.Error("HurryUp is a keypress dd-pits cannot see — it must be relayed")
	}
	// A derived call switched off: silent everywhere, and the case worth warning about.
	if roles[SoundRaceDone] != roleSilent {
		t.Error("a switched-off call is silent regardless of who would have produced it")
	}
	if roles[SoundPilotsEnableVideo] != roleSilent {
		t.Error("a switched-off relayed call is silent too")
	}
}

// TestImplausibleLapTimesAreNeverSpoken covers calls heard on air:
//
//	"bob9 4 laps in 11276.92"   (relayed)
//	"Fizz 11271.85"             (derived)
//
// Over three hours for a lap. The heat had been left running for 2h38m as a
// stale leftover, so every lap time computed against its start was the AGE of
// the heat rather than the length of a lap.
//
// The existing epoch guard did not catch it: 11,276 is nonsense for a lap and
// nowhere near a timestamp, so one threshold cannot serve both.
func TestImplausibleLapTimesAreNeverSpoken(t *testing.T) {
	// Derived side.
	for _, s := range []float64{11271.85, 11276.92, 301, 0.4} {
		if believableLap(s) {
			t.Errorf("%.2fs is not a lap time", s)
		}
	}
	// 1.7 is a real lap on a small indoor course and was being swallowed by a
	// 2.0 bound — the guard is for double detections, not for slow tracks.
	for _, s := range []float64{1.0, 1.7, 2.0, 11.42, 45.99, 120, 299} {
		if !believableLap(s) {
			t.Errorf("%.2fs is a perfectly ordinary lap and must survive", s)
		}
	}

	// Relayed side.
	for _, line := range []string{
		"bob9 4 laps in 11276.92",
		"Fizz 11271.85 laps in 11271.85",
		"Holeshot Fizz 63920832629.31",
	} {
		if believableCall(line) {
			t.Errorf("relayed nonsense was allowed through: %q", line)
		}
	}

	// Everything a real meeting says must still pass — over-filtering here
	// silences the calls that matter, which is worse than the bug. Each of
	// these was, at some point, wrongly dropped.
	for _, line := range []string{
		"bob9 4 laps in 34.20",
		// A CUMULATIVE total across several laps is legitimately far larger
		// than any one lap. This exact line was silenced by a single-lap bound.
		"bob9 finished 3 laps in 489.6, Please land",
		"Fizz finished 4 laps in 506.05, Please land",
		"bob9 lap 3 in 11.42 in 1st",
		"Holeshot bob9 2.08",
		"60 seconds remaining",
		"ARM YOUR QUADS! Starting on the tone in less than FIVE!",
		"Hurry up bob9",
		"Race over",
		"bob9 finished in 1st",
	} {
		if !believableCall(line) {
			t.Errorf("a real call was dropped: %q", line)
		}
	}
}
