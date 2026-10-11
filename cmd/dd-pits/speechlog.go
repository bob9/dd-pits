package main

// Watching what FPVTrackside SAYS, rather than working out what it would say.
//
// FPVTrackside writes every spoken line to its log the moment it speaks it:
//
//	SOUNDLOG (Notice) - SpeechManager: TTS (Holeshot 'bob9' 2.08)
//	SOUNDLOG (Notice) - SpeechManager: TTS (Next up Round 12 Race 2 . )
//
// with the placeholders already filled. Following that is strictly better than
// deriving calls from timing data, and it is worth being clear about why:
//
//   - It catches EVERYTHING, including the calls no data can reveal. "Hurry Up"
//     is bound to Ctrl+H — a decision the race director makes — and no amount of
//     polling will ever show it. In the log it is just another line.
//   - There is no trigger logic to replicate, so there is nothing to get subtly
//     wrong and nothing to keep in step as FPVTrackside changes.
//   - The wording is FPVTrackside's own, already rendered. A sound switched off
//     never appears, so its settings are honoured for free.
//
// The venue mutes FPVTrackside's audio OUTPUT and keeps everything else exactly
// as it was: it still decides what to say and when, and dd-pits gives it a
// voice on the same bus as the commentary.

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// speechLine is one thing FPVTrackside said.
type speechLine struct {
	Text string
	At   time.Time
}

// ttsLineRE matches a spoken line in the log.
//
// Anchored on "TTS (" and taking everything to the LAST bracket, because the
// spoken text itself often contains brackets — a race name, a lap note — and a
// lazy match would truncate the call mid-sentence.
var ttsLineRE = regexp.MustCompile(`SpeechManager: TTS \((.*)\)\s*$`)

// logStampRE pulls FPVTrackside's own timestamp off the front of a log line:
//
//	2026/07/30 17:30:46.569 (2.76.4.881) SoundLog (Notice) - SpeechManager: ...
//
// It exists to answer a question our own timestamps cannot. A relayed line was
// recorded as At: time.Now() — the moment WE read it — so a line FPVTrackside
// wrote late, or one that sat in a buffer, arrived looking perfectly on time and
// the whole delay landed somewhere unmeasurable. Comparing the two is the only
// way to tell "FPVTrackside said it late" from "we were slow to speak it".
var logStampRE = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?)`)

// logLineStamp reads that timestamp, in local time (the log is written by a
// process on this machine). Zero when the line carries none.
func logLineStamp(line string) time.Time {
	m := logStampRE.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	for _, layout := range []string{"2006/01/02 15:04:05.000", "2006/01/02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, m[1], time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// relayLagWarn is how far behind a relayed call has to be before it is worth a
// line. Generous: the poll runs on the tick, so a few hundred milliseconds is
// ordinary and logging that would bury a real problem in noise.
const relayLagWarn = 1500 * time.Millisecond

// SpeechLogWatcher follows FPVTrackside's log and reports what it speaks.
type SpeechLogWatcher struct {
	path string

	mu     sync.Mutex
	offset int64

	// head fingerprints the START of the file.
	//
	// Rotation is otherwise genuinely hard to spot. Size shrinking misses a new
	// log that happens to be longer than the old offset; file identity misses a
	// truncate-in-place, which keeps the same inode. The opening bytes change
	// in both cases, so this catches what neither does alone — and reading 64
	// bytes per poll costs nothing.
	head []byte

	// primed suppresses everything already in the file when watching starts.
	//
	// Without it, attaching mid-meeting replays the whole day at once — the log
	// is a record of what has been said, and only what arrives AFTER we start
	// looking is news.
	primed bool
}

// NewSpeechLogWatcher follows the log for a profile.
func NewSpeechLogWatcher(path string) *SpeechLogWatcher {
	return &SpeechLogWatcher{path: path}
}

// DefaultSpeechLogPath is where FPVTrackside writes its log.
func DefaultSpeechLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Documents", "FPVTrackside", "log", "Log.txt")
}

// Poll returns the lines spoken since the last call.
//
// Cheap by design: it seeks to where it last read and reads forward, so a long
// meeting's log costs nothing to follow.
func (w *SpeechLogWatcher) Poll() ([]speechLine, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// FPVTrackside rotates its log. A different beginning means a different
	// file, and the old offset means nothing in it.
	head := make([]byte, 64)
	n, _ := f.ReadAt(head, 0)
	head = head[:n]

	rotated := w.head != nil && !bytes.Equal(w.head, head)
	if rotated || info.Size() < w.offset {
		w.offset = 0
	}
	w.head = head

	// First look: skip to the end. What is already written is history.
	if !w.primed {
		w.primed = true
		w.offset = info.Size()
		return nil, nil
	}

	if info.Size() == w.offset {
		return nil, nil
	}

	if _, err := f.Seek(w.offset, 0); err != nil {
		return nil, err
	}

	var out []speechLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var read int64
	for sc.Scan() {
		line := sc.Text()
		read += int64(len(line)) + 1

		m := ttsLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		text := CleanSpokenLine(m[1])
		if text == "" {
			continue
		}
		if !believableCall(text) {
			// Observed live: "Holeshot bob9 63920828892.33" — a Unix timestamp
			// read as a duration, because detections arrived before the race
			// had started and there was no start time to subtract.
			//
			// Derived calls are already filtered for this
			// (minimumBelievableLapSeconds); relayed ones were not, on the
			// assumption that FPVTrackside's own wording could be trusted. It
			// can be trusted as WORDING and not as arithmetic.
			log.Printf("relay: dropped an implausible figure: %q", text)
			continue
		}
		// Reported when FPVTrackside's own stamp says the line is old.
		//
		// The cause is not ours to fix and may not be a delay at all — a batch
		// arrives together after a stall, and a busy log (the Debug build writes
		// every HTTP request at Notice, which under a 100ms poll is tens of KB a
		// second) is read in one pass. But when someone says "the call came five
		// seconds late", this is the difference between a measurement and a
		// guess, and there was previously nothing to look at.
		now := time.Now()
		if st := logLineStamp(line); !st.IsZero() {
			if lag := now.Sub(st); lag >= relayLagWarn {
				log.Printf("relay: %q was written %.1fs ago — FPVTrackside logged it "+
					"at %s and it only reached us now", text, lag.Seconds(),
					st.Format("15:04:05.000"))
			}
		}
		out = append(out, speechLine{Text: text, At: now})
	}

	w.offset += read
	return out, sc.Err()
}

// CleanSpokenLine tidies a logged line into something worth speaking.
//
// FPVTrackside quotes a pilot's phonetic name — "Holeshot 'bob9' 2.08" — which
// is punctuation for its own speech engine, not part of the call. Left in, a
// model reads the quotes as emphasis or, worse, says them.
//
// It also leaves the debris of unfilled optional fields: "Next up Round 12 Race
// 2 . " has a stranded full stop where the bracket name would have been.
func CleanSpokenLine(s string) string {
	s = strings.TrimSpace(s)

	// The phonetic quoting. Both forms appear depending on the platform.
	for _, q := range []string{"'", "‘", "’", "\""} {
		s = strings.ReplaceAll(s, q, "")
	}

	// Collapse whitespace and drop punctuation stranded by an empty field.
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(strings.TrimRight(s, " .,;"))

	// A line of pure punctuation is not a call.
	if strings.Trim(s, " .,;:-") == "" {
		return ""
	}
	return s
}

// absurdFigure is larger than any real race number.
//
// Deliberately far above anything a meeting produces — a lap time, a lap count,
// a channel, a countdown — and far below a Unix timestamp, which is the actual
// failure mode. Wide enough that no legitimate call is ever caught by it.
const absurdFigure = 100000

var figureRE = regexp.MustCompile(`\d+(?:\.\d+)?`)

// singleLapRE spots a call whose figure is ONE lap time.
//
// Deliberately narrow. An earlier version matched "laps in" as well and so
// rejected "bob9 finished 3 laps in 489.6, Please land" — a perfectly ordinary
// CUMULATIVE total for three laps, silenced because 489.6 exceeds a single-lap
// bound. Over-filtering silences real calls, which is worse than the nonsense
// it was guarding against.
var singleLapRE = regexp.MustCompile(`(?i)\blap \d+\b|\bholeshot\b`)

// cumulativeRE spots a total across several laps, which is legitimately much
// larger than any one lap.
var cumulativeRE = regexp.MustCompile(`(?i)\d+ laps? in\b`)

// maximumBelievableTotalSeconds bounds a cumulative time.
//
// Generous — a long time-trial session genuinely runs to several minutes — but
// still far below the epoch-scale figures a stale heat produces.
const maximumBelievableTotalSeconds = 3600.0

// believableCall reports whether a line's numbers could describe a race.
//
// Two bounds, because one does not fit. Any figure at epoch scale is wrong by
// six orders of magnitude and needs no context. A figure in a line that is
// clearly ABOUT a lap gets the tighter lap bound — which is what let
// "bob9 4 laps in 11276.92" through: 11,276 is nonsense for a lap and nowhere
// near the generic threshold.
func believableCall(text string) bool {
	// A cumulative total is checked FIRST and more loosely: "3 laps in 489.6"
	// contains "lap"-ish wording but is not a lap time.
	limit := 0.0
	switch {
	case cumulativeRE.MatchString(text):
		limit = maximumBelievableTotalSeconds
	case singleLapRE.MatchString(text):
		limit = maximumBelievableLapSeconds
	}

	for _, f := range figureRE.FindAllString(text, -1) {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			continue
		}
		if v >= absurdFigure {
			return false
		}
		// Only the DECIMAL figures are times; a bare integer in a lap line is a
		// lap count or a position.
		if limit > 0 && strings.Contains(f, ".") && v > limit {
			return false
		}
	}
	return true
}

// SpokenPriority ranks a logged line for the audio bus.
//
// The log does not say which sound produced a line, so this is inferred from
// the words. It only has to be roughly right: the one guarantee that matters is
// that anything FPVTrackside says outranks colour, and every branch here does
// that.
func SpokenPriority(text string) int {
	t := strings.ToLower(text)

	switch {
	// Cues people physically act on.
	case strings.Contains(t, "arm your quads"),
		strings.Contains(t, "starting on the tone"),
		strings.Contains(t, "stand down"),
		strings.Contains(t, "go"),
		strings.Contains(t, "race over"),
		strings.Contains(t, "times up"),
		strings.Contains(t, "land"):
		return callPriorityRaceState
	default:
		return callPriorityLap
	}
}

// String makes a line readable in the log.
func (l speechLine) String() string { return fmt.Sprintf("%q", l.Text) }
