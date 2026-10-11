package main

// Race calls, derived from the meeting view the agent already builds.
//
// WHY THE AGENT SAYS THESE AND NOT FPVTRACKSIDE
// ---------------------------------------------
// FPVTrackside speaks these itself, so doing it here looks like duplication.
// The reason is that calls and commentary have to share one audio output, and
// across two programs they cannot: the calls live inside FPVTrackside while the
// booth is a separate process, so the two ended up fighting over the speaker —
// calls queued behind colour, colour cut mid-sentence, and a fallback voice
// appearing in the middle of a race.
//
// Here they are cues on the SAME mixer, which already orders by priority. A
// call outranks colour by construction rather than by negotiation, and there is
// nothing left to contend over. The race director mutes FPVTrackside's own
// audio and keeps configuring the sounds exactly where they always have —
// dd-pits reads that same configuration (soundset.go).
//
// WHAT IT DELIBERATELY DOES NOT SAY
// ---------------------------------
// The start countdown stays with FPVTrackside. Pilots ARM to that tone and it
// is timed off the race clock FPVTrackside owns; approximating it from a poll
// would be guessing at a cue people physically act on. Everything here is a
// REACTION to something already recorded, where being a fraction of a second
// late is invisible.

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// RaceCall is one thing to say.
type RaceCall struct {
	Key  SoundKey
	Text string

	// Made is when the call was decided, which is not when it will be heard.
	//
	// Rendering is serialised — one sidecar, one utterance at a time, roughly
	// real-time — so when four pilots cross within a few seconds the fourth call
	// waits for three full renders before its own begins. Without a record of
	// when it was made there is no way to tell, at the point of speaking, that
	// it stopped being worth saying.
	Made time.Time

	// Priority orders the audio bus. Calls outrank commentary; within calls,
	// the ones people act on outrank the ones that merely inform.
	Priority int
}

// Call priorities. Deliberately coarse — the only distinction that matters is
// that a call always beats colour.
const (
	callPriorityCommentary = 1 // filler; yields to everything
	callPriorityLap        = 5 // lap times and results
	callPriorityRaceState  = 8 // the heat starting or ending
)

// RaceCaller turns successive meeting views into the calls that should be made.
//
// State is keyed on what has ALREADY been said, because FPVTrackside re-sends
// the whole running heat on every poll — that is how the ingest reconciles — so
// without this every lap would be announced ten times a second.
type RaceCaller struct {
	mu     sync.Mutex
	sounds *SoundSet

	lapsCalled map[string]int      // race id + pilot -> laps announced
	stateOf    map[string]string   // race id -> last state
	resultSaid map[string]struct{} // race id + pilot
	outSaid    map[string]struct{} // race id + pilot — crash-out announced
	announced  map[string]struct{} // race id (the "next up" call)
	bestLap    map[string]float64  // pilot -> best lap seen this meeting
	bestConsec map[string]float64  // pilot -> best consecutive run seen

	warned map[SoundKey]struct{}

	// restarted is set when a heat's laps go backwards — see forPilot.
	restarted bool

	// announceRecords turns the personal-best calls on. Off unless the website
	// says otherwise, because a meeting where everyone is improving early on
	// would otherwise produce a call on nearly every lap.
	announceRecords bool

	// started is set when a heat goes green — see forRace. Separate from the
	// RaceStart call, which only exists when that sound has wording.
	started bool

	// primed suppresses calls for whatever was already in the first view.
	//
	// Without it, attaching to a meeting mid-session announces every lap of the
	// day at once. The first view establishes what has already happened; only
	// what arrives after it is news.
	primed bool
}

func NewRaceCaller(sounds *SoundSet) *RaceCaller {
	return &RaceCaller{
		sounds:     sounds,
		lapsCalled: map[string]int{},
		stateOf:    map[string]string{},
		resultSaid: map[string]struct{}{},
		outSaid:    map[string]struct{}{},
		announced:  map[string]struct{}{},
		bestLap:    map[string]float64{},
		bestConsec: map[string]float64{},
		warned:     map[SoundKey]struct{}{},
	}
}

// TakeStarted reports whether a heat has gone green since the last check, and
// clears the flag.
//
// Deliberately independent of the RaceStart sound: clearing the bus for a live
// heat is a behaviour, not a thing anyone chose to hear.
func (rc *RaceCaller) TakeStarted() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	was := rc.started
	rc.started = false
	return was
}

// TakeRestart reports whether a heat has been re-run since the last check, and
// clears the flag.
//
// Read by the announcer so it can clear the bus: everything queued at that
// moment belongs to the run that has just been abandoned.
func (rc *RaceCaller) TakeRestart() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	was := rc.restarted
	rc.restarted = false
	return was
}

// SetAnnounceRecords turns the personal-best calls on or off.
//
// Driven by the website's booth config, so a race director can switch it
// mid-meeting without restarting anything at the venue.
func (rc *RaceCaller) SetAnnounceRecords(on bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.announceRecords != on {
		log.Printf("race calls: personal-best calls %s", map[bool]string{true: "ON", false: "off"}[on])
	}
	rc.announceRecords = on
}

// Reprime makes the next view a baseline rather than news.
//
// Used when the link to FPVTrackside breaks and comes back: the timing system
// may have been restarted onto another event, so nothing we hold about which
// laps have been called still applies.
func (rc *RaceCaller) Reprime(why string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.primed = false
	rc.lapsCalled = map[string]int{}
	rc.stateOf = map[string]string{}
	rc.resultSaid = map[string]struct{}{}
	rc.outSaid = map[string]struct{}{}
	rc.announced = map[string]struct{}{}
	log.Printf("race calls: re-baselining (%s) — the next view is treated as history, not news", why)
}

// Calls returns what to say for this view of the meeting.
func (rc *RaceCaller) Calls(mv ddapi.Meeting) []RaceCall {
	if rc.sounds == nil {
		return nil
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	var out []RaceCall
	for _, race := range mv.Races {
		out = append(out, rc.forRace(race)...)
	}

	if !rc.primed {
		rc.primed = true
		log.Printf("race calls: primed on %d races already in progress — only new events will be called",
			len(mv.Races))
		return nil // history, not news
	}

	// Several finishing places landing in ONE pass are said as ONE call.
	//
	// When the clock runs out, everyone still flying gets their result in the
	// same tick — and each was its own render and its own turn on the bus, so
	// the end of a heat was a queue draining: result, wait, result, wait,
	// "finish your lap", wait. Heard as buffering, and reasonably. One sentence
	// renders once and airs once. A pilot crossing MID-race is alone in their
	// tick and keeps their own instant call — this only merges the flood.
	out = mergeResults(out)

	// Stamped here rather than at each call site, so a new kind of call cannot
	// be added without one — which would silently give it an unlimited lifetime
	// and put it back in the "still talking about a finished heat" bucket.
	now := time.Now()
	for i := range out {
		out[i].Made = now
	}
	return out
}

func (rc *RaceCaller) forRace(race ddapi.Race) []RaceCall {
	var out []RaceCall

	prev := rc.stateOf[race.ID]
	rc.stateOf[race.ID] = race.State

	if race.State == "running" && prev != "running" {
		// Recorded whether or not anything is SAID about it.
		//
		// A heat going green clears the bus, and that must not depend on the
		// RaceStart sound having wording: blanking its text to stop dd-pits
		// saying "Go" — a reasonable thing to want, since FPVTrackside plays
		// the tone itself — also stopped the heat being noticed at all, so
		// build-up and stale commentary carried on over the start.
		rc.started = true

		if text, ok := rc.sounds.Template(SoundRaceStart); ok {
			out = append(out, RaceCall{Key: SoundRaceStart, Text: Fill(text, nil),
				Priority: callPriorityRaceState})
		}
	}

	for _, p := range race.Pilots {
		out = append(out, rc.forPilot(race, p)...)
	}

	if race.State == "ended" && prev != "ended" {
		if text, ok := rc.sounds.Template(SoundRaceOver); ok {
			out = append(out, RaceCall{Key: SoundRaceOver, Text: Fill(text, nil),
				Priority: callPriorityRaceState})
		}
	}
	return out
}

func (rc *RaceCaller) forPilot(race ddapi.Race, p ddapi.Pilot) []RaceCall {
	var out []RaceCall

	key := race.ID + "\x00" + p.Name

	// A pilot marked CRASHED OUT mid-race is called, once.
	//
	// The operator marking it is the signal (it lands as a live DNF result row
	// while the heat runs — FPVTrackside itself has no sound for it, so there
	// is nothing to relay and this is dd-pits' own wording). Neutral on
	// purpose, per the booth's own guardrail: "is out", never "crashed" — the
	// PA does not diagnose, and the pilot's day is bad enough. Marked AFTER the
	// heat is not called: the results flow owns that moment.
	if p.DNF && strings.EqualFold(race.State, "running") {
		if _, said := rc.outSaid[key]; !said {
			rc.outSaid[key] = struct{}{}
			out = append(out, RaceCall{
				Key:      SoundKey("PilotOut"),
				Text:     p.Name + " is out",
				Priority: callPriorityLap,
			})
		}
	}
	// Laps here are already the RACING laps — the view model drops the
	// holeshot. Counting it would announce a pilot on lap 2 as being on lap 3
	// and quote its 2-5 seconds as a blistering lap.
	done := len(p.Laps)
	said := rc.lapsCalled[key]

	// A lap count going BACKWARDS is a heat being re-run.
	//
	// The state-transition signal (pending -> running) does not always fire for
	// a restart: FPVTrackside can clear a heat's laps and re-arm it while the
	// state stays "running", and then the only evidence is that a pilot who had
	// four laps now has none. Without this, the previous run's queued lap times
	// air over the new one.
	if done < said {
		rc.restarted = true
	}

	for n := said + 1; n <= done; n++ {
		lapTime := p.Laps[n-1]
		if call, ok := rc.lapCall(race, p, n, lapTime); ok {
			out = append(out, call)
		}
		out = append(out, rc.recordCalls(race, p, lapTime)...)
		if rc.announceRecords {
			out = append(out, rc.consecCall(race, p)...)
		}
	}
	rc.lapsCalled[key] = done

	// The pilot's result, once the heat is over.
	//
	// A DNF is NOT a finishing position. FPVTrackside still assigns one for
	// ordering purposes, so announcing it produces "Willman finished in 2nd"
	// for a pilot who did not finish at all — heard on air, and nonsense to
	// anyone who watched the race. There is no DNF wording in the sound set, so
	// the honest thing is to say nothing rather than say something untrue.
	if race.State == "ended" && p.DNF {
		rc.resultSaid[key] = struct{}{} // don't reconsider it every snapshot
		return out
	}
	if race.State == "ended" && p.Position > 0 {
		if _, done := rc.resultSaid[key]; !done {
			rc.resultSaid[key] = struct{}{}
			if text, ok := rc.sounds.Template(SoundRaceDone); ok {
				out = append(out, RaceCall{
					Key:      SoundRaceDone,
					Text:     Fill(text, map[string]string{"pilot": p.Name, "position": ordinal(p.Position)}),
					Priority: callPriorityLap,
				})
			}
		}
	}
	return out
}

// countdownWords are the numbers a start countdown actually uses.
//
// Deliberately small: this exists to make ONE line read as speech rather than
// data, not to become a general number-speller. A lap time must stay "11.42",
// and spelling numbers wherever they appear would wreck exactly the calls that
// matter most.
var countdownWords = map[string]string{
	"1": "ONE", "2": "TWO", "3": "THREE", "4": "FOUR", "5": "FIVE",
	"6": "SIX", "7": "SEVEN", "8": "EIGHT", "9": "NINE", "10": "TEN",
	"15": "FIFTEEN", "20": "TWENTY", "30": "THIRTY", "60": "SIXTY",
}

// spellCountdown replaces the countdown number with its word.
//
// "in less than 5" reads like a form being filled in; "in less than FIVE" reads
// like someone calling a race start. The template keeps its {time} placeholder
// so a different start delay still works — the spelling happens here, once, on
// the way to being spoken.
//
// Only the number immediately after "than", which is the countdown's position
// in the template. Spelling every number instead would turn "lap 1" into "lap
// ONE" and "4 laps" into "FOUR laps" — and a function that is only safe when
// called from the right place is one waiting to be called from the wrong one.
func spellCountdown(text string) string {
	fields := strings.Fields(text)
	for i := 1; i < len(fields); i++ {
		if !strings.EqualFold(strings.TrimRight(fields[i-1], "!.,"), "than") {
			continue
		}
		trimmed := strings.TrimRight(fields[i], "!.,")
		if w, ok := countdownWords[trimmed]; ok {
			fields[i] = w + fields[i][len(trimmed):]
		}
	}
	return strings.Join(fields, " ")
}

// buildUpChatter reports the build-up calls that the START SEQUENCE supersedes.
//
// Everything in preRaceCall EXCEPT the countdown itself. The countdown is the
// start sequence, not the build-up to it.
//
// This exists because the build-up is longer than the gap FPVTrackside leaves
// before the tone: four channel announcements plus the video prompt is ~10s of
// audio, and the race starts about 3s after the operator presses Start. The
// countdown therefore queued behind them and was still playing as the tone
// went — heard, correctly, as "it says arm your quads AFTER the start".
//
// So the countdown clears the queue ahead of it. Nothing before the start
// matters more than the call telling pilots to arm.
func buildUpChatter(key SoundKey) bool {
	return preRaceCall(key) && key != SoundStartRaceIn
}

// preRaceCall reports whether a call belongs to the BUILD-UP rather than the race.
//
// These are worth saying while the field is staging and worthless once the
// light goes green: nobody needs to hear which channel a pilot is on while they
// are already flying, and every second spent on it is a second the first lap
// call waits. The race starting is what makes them obsolete, so that is what
// clears them.
//
// The start cue itself is NOT in here. "Go" is the moment, not the build-up.
func preRaceCall(key SoundKey) bool {
	switch key {
	case SoundAnnouncePilotChannel,
		SoundPilotsEnableVideo,
		SoundRaceAnnounce,
		SoundInTheHole,
		SoundStartRaceIn,
		SoundUntilRaceStart:
		return true
	}
	return false
}

// warnOnce reports a configuration problem a single time. Repeating it every
// lap would bury the log in the middle of a race.
func (rc *RaceCaller) warnOnce(key SoundKey, msg string) {
	if _, done := rc.warned[key]; done {
		return
	}
	rc.warned[key] = struct{}{}
	log.Printf("race calls: [%s] %s", key, msg)
}

// lapCall builds the announcement for one completed lap.
func (rc *RaceCaller) lapCall(race ddapi.Race, p ddapi.Pilot, lapNumber int, seconds float64) (RaceCall, bool) {
	// Not a lap time at all.
	//
	// A stale heat left running produces lap times that are the AGE of the heat
	// rather than the length of a lap — heard on air as "Fizz 11271.85". Caught
	// here, at the point the call is built, so no path can reach the bus with
	// it: the low bound catches a double detection at the gate, the high one
	// catches a heat that has been sitting open.
	if !believableLap(seconds) {
		rc.warnOnce(SoundRaceLap, fmt.Sprintf(
			"ignoring an implausible lap time (%.2fs) — a heat left running produces the age of the heat, not a lap", seconds))
		return RaceCall{}, false
	}

	// A lap with no pilot on it.
	//
	// The name IS the call — "lap 6 in 12.68 in 1st" tells nobody anything, and
	// it was heard on air, repeatedly, from a half-formed snapshot read while
	// FPVTrackside was starting up. Dropped rather than voiced with a blank:
	// there is no useful thing to say about a pilot we cannot name.
	if strings.TrimSpace(p.Name) == "" {
		rc.warnOnce(SoundRaceLap, "ignoring a lap for an unnamed pilot — the snapshot is half-formed")
		return RaceCall{}, false
	}

	// A time trial and a race are announced differently — FPVTrackside has
	// separate sounds for each, and using the race one for qualifying would
	// call a position that means nothing when pilots fly solo.
	key := SoundRaceLap
	if race.IsTT || race.TypeLabel == "Time Trial" {
		key = SoundTimeTrialEveryLap
	}

	text, ok := rc.sounds.Template(key)
	if !ok {
		// The configured sound is switched off; fall back to the general lap
		// call rather than going silent on a time trial.
		if key == SoundTimeTrialEveryLap {
			if text, ok = rc.sounds.Template(SoundRaceLap); ok {
				key = SoundRaceLap
			}
		}
		if !ok {
			// Logged once per sound: a silent call is otherwise
			// indistinguishable from a broken one, and "it is switched off in
			// FPVTrackside" is the likeliest explanation by far.
			rc.warnOnce(key, "lap detected but the sound is switched off in FPVTrackside")
			return RaceCall{}, false
		}
	}

	values := map[string]string{
		"pilot":     p.Name,
		"lapnumber": fmt.Sprintf("%d", lapNumber),
		"laptime":   formatSeconds(seconds),
		"position":  ordinal(p.Position),
		"count":     fmt.Sprintf("%d", lapNumber),
		"lapstime":  formatSeconds(p.Total),
		"s":         lapPlural(lapNumber),
	}

	spoken := Fill(text, values)
	// The DELIVERY of a lap call follows the lap, judged against the pilot's
	// own best so far — a hot lap gets an exclamation, an ordinary one a full
	// stop, a struggling one trails off. On the local cloned voice punctuation
	// IS the delivery channel (the same fact the clipped-endings fix rests on:
	// the words are all it takes, so the marks are all the direction there is),
	// and it costs nothing on cloud voices, which read it the same way. Without
	// this every lap of a meeting landed with identical inflection, and a
	// personal best sounded exactly like a crawl.
	//
	// Judged against the pilot's OWN best, not the field's: "quick for Willman"
	// is the thing a caller's tone actually tracks. First laps have no
	// yardstick and stay neutral rather than guessing.
	if prev, seen := rc.bestLap[p.Name]; seen && believableLap(seconds) {
		switch {
		case seconds <= prev:
			spoken += "!"
		case seconds >= prev*1.6:
			spoken += "..."
		}
	}
	return RaceCall{Key: key, Text: spoken, Priority: callPriorityLap}, true
}

// recordCalls announces a new meeting-best lap.
//
// The comparison is against what this agent has SEEN, which is why the first
// view is primed rather than announced — otherwise attaching mid-meeting would
// declare a record on the first lap it happened to observe.
func (rc *RaceCaller) recordCalls(race ddapi.Race, p ddapi.Pilot, seconds float64) []RaceCall {
	if !believableLap(seconds) {
		return nil // a double detection at the gate, not a lap
	}
	if !rc.announceRecords {
		// Still TRACKED while off, so switching it on mid-meeting compares
		// against what has actually been flown rather than declaring a record
		// on the next lap anyone happens to fly.
		rc.trackBests(race, p, seconds)
		return nil
	}

	best, seen := rc.bestLap[p.Name]
	if !seen || seconds < best {
		rc.bestLap[p.Name] = seconds
		if seen {
			if text, ok := rc.sounds.Template(SoundNewLapRecord); ok {
				return []RaceCall{{
					Key: SoundNewLapRecord,
					Text: Fill(text, map[string]string{
						"pilot": p.Name, "count": "1", "s": "",
						"lapstime": formatSeconds(seconds), "laptime": formatSeconds(seconds),
					}),
					Priority: callPriorityLap,
				}}
			}
		}
	}
	return nil
}

// trackBests updates the bests without announcing anything.
func (rc *RaceCaller) trackBests(race ddapi.Race, p ddapi.Pilot, seconds float64) {
	if best, seen := rc.bestLap[p.Name]; !seen || seconds < best {
		rc.bestLap[p.Name] = seconds
	}
	if c := bestConsecutiveSum(p.Laps, race.ConsecWindow); c > 0 {
		if best, seen := rc.bestConsec[p.Name]; !seen || c < best {
			rc.bestConsec[p.Name] = c
		}
	}
}

// consecCall announces a new best run over the meeting's scoring window.
//
// On a time trial this is the number pilots are actually ranked on — a new best
// three-lap run matters far more than a single quick lap — and nothing was
// saying it. The wording reuses FPVTrackside's own NewLapRecord template, which
// already carries a lap COUNT ("{pilot} new record {count} lap{s} in
// {lapstime}"), so this says "bob9 new record 3 laps in 34.2" without inventing
// a sound the operator cannot edit.
func (rc *RaceCaller) consecCall(race ddapi.Race, p ddapi.Pilot) []RaceCall {
	// Each heat carries its own scoring window, so it is read from the race
	// rather than assumed for the meeting.
	window := race.ConsecWindow
	if window < 2 {
		return nil // no scoring window: the single-lap record already covers it
	}
	c := bestConsecutiveSum(p.Laps, window)
	if c <= 0 {
		return nil // not enough laps yet
	}

	best, seen := rc.bestConsec[p.Name]
	if seen && c >= best {
		return nil
	}
	rc.bestConsec[p.Name] = c
	if !seen {
		return nil // first run of the meeting is a baseline, not a record
	}

	text, ok := rc.sounds.Template(SoundNewLapRecord)
	if !ok {
		return nil
	}
	return []RaceCall{{
		Key: SoundNewLapRecord,
		Text: Fill(text, map[string]string{
			"pilot": p.Name, "count": fmt.Sprintf("%d", window),
			"s": "s", "lapstime": formatSeconds(c), "laptime": formatSeconds(c),
		}),
		Priority: callPriorityLap,
	}}
}

// minimumBelievableLapSeconds is the shortest time that can be a real lap.
//
// A sub-second "lap" is a double detection at the gate. Left in, it is quoted
// as a record and read out as fact.
// 1.0, not 2.0. The bound exists to reject a DOUBLE DETECTION — the timing gate
// firing twice on one pass, which produces a fraction of a second — not to
// judge how fast a track is.
//
// 2.0 was too aggressive and swallowed a real lap: a 1.7s lap on a small indoor
// course went uncalled. Whoop and micro tracks run laps well under two seconds,
// so the old bound would have silenced a whole class of racing.
//
// A double detection is typically well under half a second, so 1.0 still
// catches it with room to spare.
const minimumBelievableLapSeconds = 1.0

// maximumBelievableLapSeconds is the longest time that can be a real lap.
//
// Observed on air: "bob9 4 laps in 11276.92" and "Fizz 11271.85" — over three
// hours for a lap. The heat had been left running for 2h38m as a stale
// leftover, so every lap time computed against its start was the age of the
// heat rather than the length of a lap.
//
// Five minutes is far beyond any real lap on any track, and far below the
// figures a stale heat produces, so nothing genuine is caught by it. The low
// bound above catches the opposite failure — a double detection at the gate.
const maximumBelievableLapSeconds = 300.0

// believableLap reports whether a figure can be a lap time.
//
// Checked at the DATA level rather than trusted from the timing system: a
// number in a call is a number that gets spoken, and "eleven thousand two
// hundred and seventy six" is worse than saying nothing at all.
func believableLap(seconds float64) bool {
	return seconds >= minimumBelievableLapSeconds && seconds <= maximumBelievableLapSeconds
}

// lapPlural is the {s} placeholder FPVTrackside uses for "lap" vs "laps".
func lapPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func formatSeconds(s float64) string {
	// Two decimals at most, and NEVER a trailing zero.
	//
	// "%.2f" turns a 2.6 second lap into "2.60", which a voice reads literally
	// as "two point six ZERO" — heard on air, and it is the kind of thing that
	// makes a call sound machine-generated. FPVTrackside has never had the
	// problem because it trims the same way ("9.43", "812.1", "23.0").
	//
	// One decimal is kept even when it is a zero, so a whole number reads as
	// "twenty three point oh" rather than a bare "twenty three" — matching
	// FPVTrackside, and a lap time with no decimal at all sounds wrong.
	out := strings.TrimSuffix(strconv.FormatFloat(s, 'f', 2, 64), "0")
	if strings.HasSuffix(out, ".") {
		out += "0"
	}
	return out
}

// ordinal renders a position as "1st", "2nd" — the form FPVTrackside's own
// templates use, so the text reads identically to what it would have said.
func ordinal(n int) string {
	if n <= 0 {
		return ""
	}
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return fmt.Sprintf("%dth", n)
	case n%10 == 1:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// mergeResults folds the finishing calls of one pass into a single call.
//
// Everything else keeps its position; the combined result takes the slot of
// the first finishing call so ordering against "Race over" is preserved.
func mergeResults(calls []RaceCall) []RaceCall {
	var results []int
	for i, c := range calls {
		if c.Key == SoundRaceDone {
			results = append(results, i)
		}
	}
	if len(results) < 2 {
		return calls
	}
	var parts []string
	for _, i := range results {
		parts = append(parts, strings.TrimSuffix(calls[i].Text, "."))
	}
	merged := calls[results[0]]
	merged.Text = strings.Join(parts, ". ")
	out := calls[:0]
	for i, c := range calls {
		if i == results[0] {
			out = append(out, merged)
			continue
		}
		if c.Key == SoundRaceDone {
			continue
		}
		out = append(out, c)
	}
	return out
}

// perishableDeadline is how long a moment-tied call may take to produce its
// FIRST audio before it is dead. Healthy first-audio is ~100-300ms; a stall is
// 20s+. Six seconds is far above jitter and far below useless.
const perishableDeadline = 6 * time.Second

// perishableCall reports whether a call describes THIS INSTANT — a lap landing,
// a holeshot, the clock — and is therefore wrong, not merely late, once the
// moment has passed. Results and announcements stay speakable: "Willman
// finished 2nd" is as true a minute later.
func perishableCall(key SoundKey) bool {
	switch key {
	case SoundHoleshot, SoundRaceLap, SoundTimeTrialEveryLap,
		SoundNewLapRecord, SoundTimeRemaining:
		return true
	}
	return false
}
