package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLog(t *testing.T, lines string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Log.txt")
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendLog(t *testing.T, p, lines string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(lines); err != nil {
		t.Fatal(err)
	}
}

const ttsLine = "2026/07/27 19:00:00.123 (2.76) SOUNDLOG (Notice)  - SpeechManager: TTS (%s)\n"

// What is already in the log is history. Attaching mid-meeting must not replay
// the whole day.
func TestWatcherIsPrimedOnFirstLook(t *testing.T) {
	p := writeLog(t, "old noise\n"+fmt.Sprintf(ttsLine, "Race over"))
	w := NewSpeechLogWatcher(p)

	got, err := w.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the first look must be silent, got %v", got)
	}
}

func TestWatcherReportsNewlySpokenLines(t *testing.T) {
	p := writeLog(t, "header\n")
	w := NewSpeechLogWatcher(p)
	if _, err := w.Poll(); err != nil {
		t.Fatal(err)
	}

	appendLog(t, p, fmt.Sprintf(ttsLine, "bob9 lap 2 in 24.08 in 1st"))
	got, err := w.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "bob9 lap 2 in 24.08 in 1st" {
		t.Fatalf("got %v", got)
	}

	// Nothing new appended: nothing to say.
	if again, _ := w.Poll(); len(again) != 0 {
		t.Fatalf("a second poll must be silent, got %v", again)
	}
}

// The manual calls are the whole reason for reading the log — no amount of
// polling timing data can reveal that the director pressed Ctrl+H.
func TestWatcherCatchesManualCalls(t *testing.T) {
	p := writeLog(t, "header\n")
	w := NewSpeechLogWatcher(p)
	w.Poll()

	appendLog(t, p, fmt.Sprintf(ttsLine, "Hurry Up bob9"))
	got, _ := w.Poll()

	if len(got) != 1 || got[0].Text != "Hurry Up bob9" {
		t.Fatalf("a manually triggered call must be caught, got %v", got)
	}
}

// FPVTrackside rotates its log; a shrunken file is a NEW one, and reading from
// the old offset would skip the start or read garbage.
func TestWatcherHandlesLogRotation(t *testing.T) {
	p := writeLog(t, "header\n"+fmt.Sprintf(ttsLine, "first"))
	w := NewSpeechLogWatcher(p)
	w.Poll()

	if err := os.WriteFile(p, []byte(fmt.Sprintf(ttsLine, "after rotation")), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := w.Poll()

	if len(got) != 1 || got[0].Text != "after rotation" {
		t.Fatalf("a rotated log must be picked up from the start, got %v", got)
	}
}

// FPVTrackside quotes the phonetic name for its own engine. Left in, a model
// reads the quotes aloud or as emphasis.
func TestPhoneticQuotesAreStripped(t *testing.T) {
	if got := CleanSpokenLine("Holeshot 'bob9' 2.08"); got != "Holeshot bob9 2.08" {
		t.Fatalf("got %q", got)
	}
}

// An unfilled optional field leaves stranded punctuation: a plain race has no
// bracket, so "Next up Round 12 Race 2 . " arrives with a floating full stop.
func TestStrandedPunctuationIsDropped(t *testing.T) {
	if got := CleanSpokenLine("Next up Round 12 Race 2 . "); got != "Next up Round 12 Race 2" {
		t.Fatalf("got %q", got)
	}
}

func TestPunctuationOnlyLineIsNotACall(t *testing.T) {
	if got := CleanSpokenLine(" . "); got != "" {
		t.Fatalf("expected nothing to say, got %q", got)
	}
}

// Whatever FPVTrackside says outranks colour — that is the one guarantee.
func TestEverythingSpokenOutranksCommentary(t *testing.T) {
	for _, line := range []string{
		"Arm your quads. Starting on the tone in less than 5",
		"bob9 lap 2 in 24.08 in 1st",
		"Hurry Up everyone",
		"Race over",
	} {
		if SpokenPriority(line) <= callPriorityCommentary {
			t.Fatalf("%q must outrank commentary", line)
		}
	}
}

// TestImplausibleFiguresAreNotSpoken covers a call seen live:
// "Holeshot bob9 63920828892.33" — a Unix timestamp read as a duration,
// because detections arrived before the race had a start time to subtract.
//
// Derived calls already filter this. Relayed ones did not, on the assumption
// that FPVTrackside's own wording could be trusted — which is true of the
// WORDING and not of the arithmetic.
func TestImplausibleFiguresAreNotSpoken(t *testing.T) {
	bad := []string{
		"Holeshot bob9 63920828892.33",
		"bob9 lap 1 in 1775576548.12 in 1st",
	}
	for _, line := range bad {
		if believableCall(line) {
			t.Errorf("this is an epoch timestamp, not a race figure: %q", line)
		}
	}

	// Everything a meeting actually says must survive. Over-filtering here
	// silences real calls, which is worse than the bug being fixed.
	good := []string{
		"Holeshot bob9 2.08",
		"bob9 lap 3 in 2.41 in 1st",
		"60 seconds remaining",
		"Next up Round 12 Race 2",
		"Arm your quads. Starting on the tone in less than 5",
		"Hurry up bob9",
		"bob9 on R7",
		"Race over",
		"bob9 4 laps in 34.20",
	}
	for _, line := range good {
		if !believableCall(line) {
			t.Errorf("a real call must never be dropped: %q", line)
		}
	}
}

// The lag measurement is only worth having if it reads FPVTrackside's real
// stamp, in the same clock as ours. A wrong parse here would report a bogus lag
// on every line — worse than no measurement, because it looks authoritative.
func TestLogLineStampParsesFPVTracksideFormat(t *testing.T) {
	cases := []struct {
		line string
		want string // "" = no usable stamp
	}{
		{"2026/07/30 17:30:46.569 (2.76.4.881) SoundLog (Notice)  - SpeechManager: TTS (Race over)",
			"2026-07-30 17:30:46.569"},
		{"2025/07/12 15:25:16.842 SoundLog (Notice) - SpeechManager: TTS ('Wizz' 3 laps in 57.29)",
			"2025-07-12 15:25:16.842"},
		{"2026/07/30 17:30:46 SoundLog - no millis", "2026-07-30 17:30:46.000"},
		{"SpeechManager: TTS (no timestamp at all)", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := logLineStamp(c.line)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("%q: read a stamp (%s) where there is none — a bogus lag "+
					"would be reported for this line", c.line, got)
			}
			continue
		}
		if got.IsZero() {
			t.Errorf("%q: no stamp read, so the lag can never be measured", c.line)
			continue
		}
		if g := got.Format("2006-01-02 15:04:05.000"); g != c.want {
			t.Errorf("%q: read %s, want %s", c.line, g, c.want)
		}
	}

	// Local time, not UTC: the log is written by a process on this machine, and
	// reading it as UTC would report the timezone offset as lag — hours of it.
	st := logLineStamp("2026/07/30 17:30:46.569 x")
	if st.Location() != time.Local {
		t.Errorf("stamp parsed in %v, not local time — the whole UTC offset would "+
			"be reported as delay", st.Location())
	}
}
