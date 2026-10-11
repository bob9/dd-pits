package main

// The race announcer: FPVTrackside's sounds, spoken by dd-pits.
//
// This is the piece that lets a venue MUTE FPVTrackside and have everything —
// calls and commentary — come out of one program in one voice.
//
// The whole reason it exists is that two programs cannot share one speaker
// sensibly. With the calls inside FPVTrackside and the booth beside it, every
// lap became a negotiation: the call queued behind colour, or cut it
// mid-sentence, or gave up and used a different voice. Here both are cues on
// the same mixer, which already orders by priority, so a call outranks colour
// by construction and there is nothing left to contend over.
//
// Nothing about FPVTrackside changes. It keeps owning the timing, the race
// clock and the start countdown; dd-pits reads its sound configuration
// (soundset.go) and its meeting data, and does the talking.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/qwentts"
	"github.com/bob9/dd-pits/internal/tts"
)

// raceAnnouncer speaks the calls a meeting produces.
type raceAnnouncer struct {
	mu sync.Mutex

	sounds *SoundSet
	caller *RaceCaller

	// lastRace is the running heat as of the most recent snapshot.
	//
	// A pushed lap names a PILOT; RaceCaller keys its state on FPVTrackside's
	// race id, which the push does not carry. This is the bridge between them.
	lastRace ddapi.Race

	// stockCum is each pilot's last cumulative race time from STOCK push
	// detections, keyed race+pilot — the baseline the next lap's duration is
	// differenced against (stock Time is since-race-start, not per-lap).
	stockCum map[string]float64
	mixer    *boothMixer
	engine   tts.Engine

	// watcher follows FPVTrackside's log for the calls dd-pits does not derive.
	watcher *SpeechLogWatcher

	// bridge keeps the two paths from both saying the same thing.
	bridge *spokenBridge

	// booth is the commentary booth, so the start sequence can silence it.
	booth *booth

	// remoteVoice names the voice when rendering is hosted. Empty for local,
	// where the voice is the cloned reference the sidecar was started with.
	remoteVoice string

	// packOnly stops the announcer GENERATING a call it cannot find in the pack.
	//
	// Set when the voice is a cloud service. A hosted render measured ~320ms to
	// first audio warm, and that is before network variance — fine for a wrap
	// between heats, too slow for a lap time, which has to land on the lap it
	// describes or it is a correction rather than a call.
	//
	// Fixed calls still play, because they are recordings rather than
	// generation: the start, the finish, the channel calls, the countdown. It
	// is only the novel ones — the lap times — that go unsaid, and a lap time
	// that arrives late is worth less than the silence it replaces.
	packOnly bool

	// pack is the pre-generated clip library, tried before generating anything.
	//
	// It is the only route to a call that lands ON the lap: generation runs at
	// roughly 1x real time here, so a two-second call cannot be produced in
	// under two seconds however it is scheduled, while assembling one from
	// clips is a few file reads. nil when no pack is installed, in which case
	// everything is generated live as before.
	pack *voicePack

	// pending is the call queue, drained IN ORDER by a single worker.
	//
	// Calls used to be dispatched on a goroutine each, which read as harmless
	// concurrency and was not: they then raced for the one sidecar pipe, so
	// generation order — and therefore the order they went to air — was
	// whichever goroutine won. Heard live as "bob9 first, Willman third, race
	// over, Fizz second": a finishing order read out in the wrong order, with
	// the finish called in the middle of it.
	//
	// One worker makes the order the same as the order the calls were made,
	// which is the only order that makes sense to a listener.
	pending chan RaceCall

	// warmed guards the one-off pre-render of the fixed lines.
	warmed bool

	// raceGen advances every time a heat goes green.
	//
	// Rendering takes seconds, so cutting the bus alone is not enough: a
	// build-up call already being rendered when the light goes green would
	// finish afterwards and be queued into a race that is underway. Each call
	// carries the generation it was made in, and a stale build-up call is
	// dropped rather than aired.
	raceGen int64

	// rendered caches audio by the exact words.
	//
	// A race day repeats its fixed lines constantly — "Race over", the
	// countdown, "Pilots turn on your video transmitters" — and re-rendering
	// them is pure waste. Lap times never repeat, so they are unaffected.
	rendered map[string][]byte
	order    []string

	seq int64
}

// maxRenderedLines bounds the cache. A meeting has a few dozen distinct fixed
// lines; beyond that it is lap times, which never repeat.
const maxRenderedLines = 200

// How long a call stays worth saying.
//
// Rendering is roughly real-time and strictly serialised, so a flurry of laps
// builds a backlog that the announcer would otherwise read out in full — still
// calling lap times thirty seconds after the heat ended. That is what makes it
// sound like it never stops talking, and it is worse than saying nothing: the
// calls are not merely late, they are contradicted by what the room has already
// seen.
//
// A human commentator drops the call and moves on. So does this.
const (
	// A lap or result is news briefly. Past this the room has moved on.
	lapCallFreshness = 8 * time.Second

	// Cues people act on get much longer — a late "Race over" is untidy, but a
	// DROPPED one leaves pilots flying. Long enough to survive a backlog,
	// short enough that it never lands in the following heat.
	raceStateFreshness = 25 * time.Second

	// Build-up calls barely expire at all.
	//
	// "bob9 on R1", "Next up Round 4" and the rest are not tied to a moment the
	// way a lap time is: hearing a pilot's channel a few seconds late is
	// perfectly useful, and a field standing on the line would rather hear all
	// four than the last one. They were sharing the 8s lap window, so a slow
	// stretch — the post-race summary generating, typically — expired the first
	// three and only the last was spoken.
	//
	// Long is safe here BECAUSE pressing Start clears the bus outright, so
	// these can never leak into a heat no matter how far behind they are. That
	// clear is what makes the deadline unnecessary rather than merely generous.
	buildUpFreshness = 3 * time.Minute
)

// freshnessFor reports how long a call is worth saying.
func freshnessFor(key SoundKey, priority int) time.Duration {
	// Checked before priority: a build-up call carries lap priority, so keying
	// on priority alone put it on the 8s lap deadline.
	if preRaceCall(key) {
		return buildUpFreshness
	}
	if priority >= callPriorityRaceState {
		return raceStateFreshness
	}
	return lapCallFreshness
}

// stale reports whether a call has been overtaken by events.
//
// A zero Made is never stale: the relay and the operator page build calls
// directly, and defaulting those to "expired" would silence them entirely.
func stale(call RaceCall, now time.Time) bool {
	if call.Made.IsZero() {
		return false
	}
	return now.Sub(call.Made) > freshnessFor(call.Key, call.Priority)
}

func newRaceAnnouncer(sounds *SoundSet, mixer *boothMixer, engine tts.Engine) *raceAnnouncer {
	return newRaceAnnouncerAt(sounds, mixer, engine, "")
}

// newRaceAnnouncerAt takes an explicit speech-log path.
//
// The relay is HALF the voice: everything dd-pits does not derive from timing
// data — "Hurry Up", the video prompts, the countdown — is learnt by following
// FPVTrackside's log. Point it at the wrong file and those calls simply never
// happen, with nothing to say why, so it follows the same configured location
// as the wording file rather than assuming its own.
func newRaceAnnouncerAt(sounds *SoundSet, mixer *boothMixer, engine tts.Engine, logPath string) *raceAnnouncer {
	if logPath == "" {
		logPath = DefaultSpeechLogPath()
	}
	ra := &raceAnnouncer{
		sounds:   sounds,
		caller:   NewRaceCaller(sounds),
		mixer:    mixer,
		engine:   engine,
		watcher:  NewSpeechLogWatcher(logPath),
		bridge:   newSpokenBridge(sounds),
		rendered: map[string][]byte{},
		// Deep enough to absorb a flurry of laps, shallow enough that a
		// hopeless backlog is dropped at the door rather than read out minutes
		// late.
		pending: make(chan RaceCall, 64),
	}
	go ra.run()
	return ra
}

// run speaks queued calls one at a time, in the order they were made.
func (ra *raceAnnouncer) run() {
	for call := range ra.pending {
		ra.mu.Lock()
		ra.seq++
		seq := ra.seq
		ra.mu.Unlock()
		ra.speak(call, seq)
	}
}

// enqueue submits a call, dropping it if the queue is hopelessly behind.
func (ra *raceAnnouncer) enqueue(call RaceCall) {
	select {
	case ra.pending <- call:
	default:
		// Full. Dropping the NEW call is right: the queue ahead of it is older
		// and closer to the moment it describes.
		log.Printf("call [%s] dropped, the queue is full: %s", call.Key, call.Text)
	}
}

// Observe takes a meeting view and speaks whatever is new.
//
// Returns immediately: rendering happens on its own goroutine so the poll loop
// is never waiting on a model. A call that renders slowly is late; a poll loop
// that blocks is a venue whose timing display has stopped.
func (ra *raceAnnouncer) Observe(mv ddapi.Meeting) {
	if ra == nil || ra.engine == nil {
		return
	}

	ra.prewarm(mv)

	// Recorded before the calls are built, so a pushed lap arriving mid-heat has
	// the current heat to match against.
	ra.mu.Lock()
	for _, r := range mv.Races {
		if r.State == "running" {
			ra.lastRace = r
			break
		}
	}
	ra.mu.Unlock()

	calls := ra.caller.Calls(mv)

	// A re-run heat clears the bus, and it is checked BEFORE the empty-calls
	// return.
	//
	// A restart clears the laps, so the view it produces has nothing to say —
	// no lap times left to call, and no RaceStart when the state never left
	// "running". That is exactly the moment the old run's queued calls must be
	// dropped, so returning early here left them to air over the new run.
	// A heat going green or being re-run both clear the bus, and both are
	// checked BEFORE the empty-calls return: a restart clears the laps, so the
	// view it produces has nothing to say, and a green heat says nothing either
	// when the RaceStart sound has no wording.
	// BOTH are consumed, never short-circuited: `a() || b()` would leave the
	// second flag set when the first is true, and it would then fire a
	// spurious bus-clear during the next heat.
	started := ra.caller.TakeStarted()
	restarted := ra.caller.TakeRestart()
	if started || restarted {
		ra.startRace()
	}

	if len(calls) == 0 {
		return
	}

	// A heat going green makes the whole build-up obsolete. Cut it — on air and
	// queued — before anything else is dispatched, so the first lap call is not
	// waiting behind a channel announcement for a race already in progress.
	// The countdown supersedes the build-up. Pilots need "arm your quads"
	// BEFORE the tone, and it was arriving behind ~10s of channel
	// announcements for a start that comes ~3s later.
	for _, call := range calls {
		if call.Key == SoundStartRaceIn {
			ra.clearBuildUp()
			break
		}
	}

	for _, call := range calls {
		// The reverse of the relay's own check: if the relayed twin already
		// aired (it wins the race whenever FPVTrackside logs the call before
		// our snapshot carries the lap), saying it again is the duplicate.
		// Safe to skip here — the caller advanced its lap counter when it
		// BUILT the call, so a skipped call is not rebuilt next tick.
		if ra.bridge.recentlySaid(call.Text) {
			log.Printf("call [%s] skipped, its relayed twin already aired: %s",
				call.Key, call.Text)
			continue
		}
		// Recorded BEFORE rendering. Rendering takes seconds, and FPVTrackside
		// logs its own version of the same call immediately — so waiting until
		// the audio is ready would let the logged twin through in the meantime.
		ra.bridge.NoteSpoken(call.Text)
		ra.enqueue(call)
	}
}

// drainPending discards queued calls that have been superseded.
//
// The bus interrupt only reaches cues that are already rendered; a call still
// waiting in the queue would be spoken afterwards regardless. Both have to be
// cleared or the build-up simply resumes after the countdown.
func (ra *raceAnnouncer) drainPending(drop func(SoundKey) bool) {
	for {
		select {
		case call := <-ra.pending:
			if !drop(call.Key) {
				// Not superseded — put it back and stop, rather than reorder
				// the queue behind it.
				select {
				case ra.pending <- call:
				default:
				}
				return
			}
		default:
			return
		}
	}
}

// currentGen reports the generation a call is being rendered against.
func (ra *raceAnnouncer) currentGen() int64 {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	return ra.raceGen
}

// Relay speaks the calls FPVTrackside made that dd-pits does not derive itself.
//
// This is the half of the voice that cannot be worked out from timing data.
// "Hurry Up" is a keypress, the video prompts are operator decisions, and a
// keystroke inside another program is invisible from outside it — but every one
// of them lands in the log the moment it is spoken.
func (ra *raceAnnouncer) Relay() {
	if ra == nil || ra.engine == nil || ra.watcher == nil {
		return
	}

	lines, err := ra.watcher.Poll()
	if err != nil {
		// The log going missing is not worth a message per tick — FPVTrackside
		// may simply not be running yet — and the derived calls are unaffected.
		return
	}

	for _, line := range lines {
		if !ra.bridge.ShouldRelay(line.Text) {
			continue
		}

		key, _ := ra.bridge.Identify(line.Text)

		// The countdown is relayed (dd-pits cannot see the operator press
		// Start), so the queue-clearing has to happen on this path too.
		text := line.Text
		if key == SoundStartRaceIn {
			ra.clearBuildUp()
			text = spellCountdown(text)
		}
		// The relay marks what it airs, exactly as the derived path does.
		//
		// Dedup only ever ran in one direction: a derived call suppressed its
		// logged twin, but a relayed line that BEAT the poll claimed nothing —
		// so the poll said it again two seconds later. Heard live as
		// "MirellaFPV 1.97" twice: FPVTrackside logs its call at the moment of
		// detection, the poll derives from a snapshot, so on an unidentified
		// line (empty sound key — the template matcher had no claim on it) the
		// relay wins the race and the poll had no way to know.
		ra.bridge.NoteSpoken(text)
		ra.enqueue(RaceCall{
			Key:      key,
			Text:     text,
			Priority: SpokenPriority(text),
			Made:     line.At,
		})
	}
}

// clearBuildUp makes way for the countdown without cutting anything off.
//
// Queued cues only. The cue already speaking is allowed to finish — cutting it
// put "Pil—" on air in place of "Pilots turn on your video transmitters", which
// sounds like a fault rather than an interruption. Dropping what is waiting is
// enough: the build-up is four channel announcements deep, and losing those is
// what gets the countdown out before the tone.
// booth is the commentary booth, when one is running.
//
// The announcer sees the countdown; the booth cannot, because pressing Start is
// an operator action and the timing data still reports the heat as pending
// until the tone.
func (ra *raceAnnouncer) clearBuildUp() {
	// Ducked, not dropped and not truncated. "Arm your quads" has to reach
	// pilots the moment the start sequence begins, so waiting for a channel
	// announcement to finish is not an option — but cutting it on the exact
	// sample put "Pil—" on air. A short fade gives both: the countdown starts
	// immediately and the thing it interrupts ends cleanly.
	// COMMENTARY GOES TOO, not just the build-up chatter.
	//
	// This runs when the operator presses Start, and the green light is
	// several seconds later — the countdown plays in between. Ducking only the
	// build-up here left commentary talking across that entire gap, measured at
	// seven seconds, which is what "I started a race and it did not stop"
	// actually was. Everything downstream of the green light was already
	// correct and simply ran too late to matter.
	// EVERYTHING goes, and the generation advances so work already in flight
	// is thrown away when it finishes.
	//
	// This used to spare race calls, on the reasoning that a result is worth
	// hearing. But by the time Start is pressed those results belong to the
	// heat BEFORE this one — "Wizz finished in 4th" was heard after the button
	// went down, describing a race everyone had stopped watching. The same
	// applies to a lap time still rendering: it is about the previous run and
	// airing it late is worse than not airing it.
	//
	// The countdown is unaffected: it is dispatched after this returns, so it
	// captures the new generation and is queued into a bus that is now empty.
	ra.mu.Lock()
	ra.raceGen++
	ra.mu.Unlock()

	cut, dropped := ra.mixer.duck(func(*boothCue) bool { return true })
	ra.drainPending(func(SoundKey) bool { return true })
	ra.mixer.flushDevice() // and what the speaker has already buffered

	// The booth stops talking too. Its own guards key on the race RUNNING,
	// which is several seconds later — the countdown plays first — so without
	// this a staged cue rendering now airs over the start.
	if ra.booth != nil {
		ra.booth.beginStartSequence()
	}

	log.Printf("race calls: start sequence — cut %q on air, dropped %d queued, countdown goes now",
		cut, dropped)
}

// startRace clears the build-up the moment a heat goes green.
func (ra *raceAnnouncer) startRace() {
	ra.mu.Lock()
	ra.raceGen++
	gen := ra.raceGen
	ra.mu.Unlock()

	// The bus is cleared COMPLETELY: whatever is on air is ducked out and
	// everything queued is dropped.
	//
	// Every cue that exists at this instant was written about a moment the
	// field has already left — the build-up to a heat now under way, colour
	// about the heat before it, lap times from the previous run of a restarted
	// heat. Airing any of it is worse than silence, because it actively
	// misleads about a race happening right now, and every second it occupies
	// is a second the new heat's first lap call waits behind it.
	//
	// This is deliberately broader than it used to be. It previously spared
	// race calls, on the reasoning that a call should never be cut mid-word —
	// but on a RESTART those spared calls are the old run's lap times, and they
	// went to air over the new one. Ducking rather than truncating is what
	// keeps this from sounding broken: a short fade, so nothing ends mid-word.
	cut, dropped := ra.mixer.duck(func(*boothCue) bool { return true })
	ra.drainPending(func(SoundKey) bool { return true })
	ra.mixer.flushDevice() // and what the speaker has already buffered

	// The booth abandons whatever it is writing or voicing. Normally the
	// countdown has already done this, but the countdown is a sound that can be
	// switched off, and the green light is the moment that actually matters.
	if ra.booth != nil {
		ra.booth.beginStartSequence()
	}

	// A new heat: a pilot finishing it is a new call, not a repeat of the last.
	ra.bridge.NewHeat()

	log.Printf("race calls: heat green (gen %d) — cut %q on air, dropped %d queued, laps take the bus",
		gen, cut, dropped)
}

func (ra *raceAnnouncer) speak(call RaceCall, seq int64) {
	defer func() {
		// A panic while rendering must not take the agent down mid-meeting.
		if r := recover(); r != nil {
			log.Printf("race announcer: recovered while speaking %q: %v", call.Text, r)
		}
	}()

	if call.Text == "" {
		return
	}

	// Checked BEFORE rendering: a call already past its moment must not spend
	// the sidecar's time, because that time is what makes the NEXT call late
	// too. This is the check that actually breaks the cascade.
	if stale(call, time.Now()) {
		log.Printf("call [%s] dropped, %s late and overtaken: %s",
			call.Key, time.Since(call.Made).Round(time.Second), call.Text)
		return
	}

	gen := ra.currentGen()

	// Race calls STREAM: the first fragment airs the moment it exists rather
	// than after the whole line is generated. Generation runs at roughly real
	// time, so waiting for a finished utterance puts a two-second call two
	// seconds behind the lap — and a lap time that lands after the pilot has
	// crossed again is a correction, not a call.
	//
	// Only calls, not commentary: colour is not tied to a moment, and holding
	// the pipe open for a twenty-second wrap would block the very calls this
	// is meant to speed up.
	// A call not in the pack is generated, and generation is where the
	// remaining delay lives. Streaming it starts the audio at the FIRST
	// fragment rather than after the whole line, which is the only way to cut
	// that without shortening or splicing anything.
	//
	// This was switched off once, correctly: generation then measured ~1x real
	// time and a streamed cue starved mid-word. It measures ~0.62x now (3.1s of
	// audio in 1.93s) — the pack took the fixed calls off the generator and the
	// booth yields the GPU during a heat — so there is margin again, and
	// streamLeadBytes banks 600ms of it before a note is played.
	//
	// Only for calls, never commentary: a twenty-second wrap would hold the
	// sidecar pipe open and delay the very calls this exists to speed up.
	// The PACK first, always. A line it holds whole is already recorded, so
	// generating it would spend a second producing audio that exists on disk —
	// which is exactly what happened to "bob9 on R1" (1485ms, streamed) when
	// this branch ran behind the generator.
	if pcm, ok := ra.pack.Render(call.Text); ok {
		ra.mu.Lock()
		ra.rendered[call.Text] = pcm
		ra.order = append(ra.order, call.Text)
		ra.mu.Unlock()
		ra.mixer.enqueue(ra.cueFor(call, seq, pcm, boothSampleRate))
		log.Printf("call [%s] %s (pack): %s", call.Key, priorityName(call.Priority), call.Text)
		return
	}

	// Not in the pack, and the voice is a cloud service: say nothing rather
	// than say it late. See packOnly — a lap time that arrives after the pilot
	// has crossed again is a correction, not a call.
	if ra.packOnly {
		log.Printf("call [%s] skipped, not in the pack and the voice is remote: %s", call.Key, call.Text)
		return
	}

	if call.Priority >= callPriorityLap {
		if ra.speakStreaming(call, seq, gen) {
			return
		}
		// No streaming engine — render whole rather than lose the call.
	}

	started := time.Now()
	pcm, rate, cached, err := ra.render(call.Text)
	if err != nil {
		log.Printf("call [%s] FAILED to render %q: %v", call.Key, call.Text, err)
		return
	}
	if len(pcm) == 0 {
		log.Printf("call [%s] produced no audio: %q", call.Key, call.Text)
		return
	}

	// Keep it, under THIS voice — but only when it is worth keeping.
	//
	// The clip is written into the pack directory named after the voice that
	// produced it, so it can never be replayed by a different one — which is the
	// fault that started this: a pack made by a cloud clone was being used while
	// a local recording spoke everything else, and the broadcast came out as two
	// people. A directory per voice makes that unrepresentable rather than merely
	// unlikely.
	//
	// Generated once, instant every time after: a meeting only ever needs the
	// phrases for the pilots actually present, so a pack grows to what is used
	// rather than being pre-rendered in the thousands.
	// NOT "priority below lap" — race-state is priority 8 and lap is 5, so that
	// comparison excluded the countdown and "race over", which are precisely the
	// fixed lines worth keeping. It cached only filler.
	if !cached && call.Priority != callPriorityLap {
		// Fixed lines only: "Race over", the countdown, "pilots turn on your
		// video". They are identical every heat, so caching pays for itself
		// immediately — and they are the long ones that take seconds to render.
		ra.pack.save(call.Text, pcm, rate)
	}

	// Every call is logged as it goes to air. Watching the log is how a race
	// director tells "it never decided to say that" from "it said it and the
	// speaker is muted" — two very different problems that sound identical
	// from the room.
	source := fmt.Sprintf("%dms", time.Since(started).Milliseconds())
	if cached {
		source = "cached"
	}
	log.Printf("call [%s] %s (%.1fs, %s): %s",
		call.Key, priorityName(call.Priority),
		float64(len(pcm))/float64(rate*2), source, call.Text)

	// The heat went green while this was rendering, so this belongs to the
	// race before it. Airing it now would talk over a race already underway.
	//
	// Applies to EVERY call, not only the build-up. Rendering takes seconds,
	// and cutting the bus at the green light does nothing about work already
	// in flight — a commentary line mid-render finished afterwards and played
	// into the live heat, which is precisely the "it did not stop immediately"
	// that clearing the queue alone did not fix.
	//
	// A call belonging to the CURRENT heat is unaffected: it captured this
	// generation when it started rendering, so its generation still matches.
	if ra.currentGen() != gen {
		log.Printf("call [%s] dropped, the heat went green while it rendered: %s", call.Key, call.Text)
		return
	}

	// Checked AGAIN after rendering. A call can be fresh when it starts and
	// stale by the time it finishes — that is precisely the case a backlog
	// creates, and it is the one worth catching.
	if stale(call, time.Now()) {
		log.Printf("call [%s] rendered but dropped, %s late: %s",
			call.Key, time.Since(call.Made).Round(time.Second), call.Text)
		return
	}

	ra.mixer.enqueue(&boothCue{
		Kind:       "race_call",
		Text:       call.Text,
		PCM:        pcm,
		Rate:       rate,
		DurationMs: int64(len(pcm)) * 1000 / int64(rate*2),
		seq:        seq,
		priority:   call.Priority,
		callKey:    call.Key,
	})
}

// prewarm renders the lines a meeting repeats, before they are needed.
//
// This is the fix for "it takes five or six seconds to say who is racing".
// Those calls are not slow because of any queue: they are four separate lines,
// each generated from scratch at roughly real time, one after another. But they
// are the SAME words every meeting — the pilots' channels, the video prompt,
// the countdown, "Go", "Race over" — so none of it needs generating at the
// moment it is wanted.
//
// Rendered once at startup and served from the cache thereafter, they become
// instant. Only lap times are left generating live, because their numbers are
// new every time.
func (ra *raceAnnouncer) prewarm(mv ddapi.Meeting) {
	if ra == nil || ra.engine == nil {
		return
	}

	ra.mu.Lock()
	if ra.warmed {
		ra.mu.Unlock()
		return
	}
	ra.warmed = true
	ra.mu.Unlock()

	var lines []string
	add := func(key SoundKey, values map[string]string) {
		if t, ok := ra.sounds.Template(key); ok {
			if text := Fill(t, values); text != "" {
				lines = append(lines, text)
			}
		}
	}

	// The fixed calls of any meeting.
	for _, k := range []SoundKey{SoundRaceStart, SoundRaceOver, SoundPilotsEnableVideo} {
		add(k, nil)
	}
	// The countdown, at the values FPVTrackside actually announces — and in the
	// form it is SPOKEN, not the form the template produces.
	//
	// The relay runs the countdown through spellCountdown ("less than 5" ->
	// "less than FIVE"), so pre-rendering the digit form cached a string that
	// is never asked for. The cache is keyed on the exact words, so the one
	// call that most needs to be instant was regenerated on every start:
	// measured at 108ms, on the line the operator is waiting to hear.
	for _, t := range []string{"5", "10", "30", "60"} {
		add(SoundStartRaceIn, map[string]string{"time": t})
		add(SoundTimeRemaining, map[string]string{"time": t})
	}
	for i, text := range lines {
		lines[i] = spellCountdown(text)
	}

	// Every pilot on their channel — the "who is racing" call, which is the one
	// that was taking five or six seconds.
	seen := map[string]bool{}
	for _, race := range mv.Races {
		for _, p := range race.Pilots {
			if p.Name == "" || seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			add(SoundAnnouncePilotChannel, map[string]string{
				"pilot": p.Name, "channel": p.ChannelLabel,
			})
		}
	}

	go func() {
		started := time.Now()
		done := 0
		for _, text := range lines {
			// Never while there is live racing to serve. The old guard slept
			// ONCE on a pending call and rendered regardless, and knew nothing
			// about a running heat — so a voice switch just before a heat ran
			// this whole sweep (fifteen lines, 2-4s of engine each, a minute
			// total) through the first heat: the holeshot took 8.7s to first
			// audio and was dropped as perished. Each line now waits for a
			// genuinely quiet moment; the sweep takes longer across a meeting
			// and costs nothing, which is the right way round.
			for !ra.quietForPrewarm() {
				time.Sleep(2 * time.Second)
			}
			if _, _, cached, err := ra.render(text); err == nil && !cached {
				done++
			}
		}
		log.Printf("race calls: pre-rendered %d/%d fixed lines in %s — these now play instantly",
			done, len(lines), time.Since(started).Round(time.Second))
	}()
}

// quietForPrewarm reports whether the voice engine can be borrowed for a
// fixed-line render without a live call paying for it: no heat running, no
// call waiting to render, no race call on the bus (the countdown included).
func (ra *raceAnnouncer) quietForPrewarm() bool {
	ra.mu.Lock()
	running := strings.EqualFold(ra.lastRace.State, "running")
	pending := len(ra.pending)
	ra.mu.Unlock()
	return !running && pending == 0 && !ra.mixer.raceCallBusy()
}

// speakStreaming airs a call as it is generated. Reports whether it handled it.
func (ra *raceAnnouncer) speakStreaming(call RaceCall, seq, gen int64) bool {
	// Any engine that can deliver audio progressively, not just the local one.
	//
	// Asserting the concrete type meant a model host on the LAN silently fell
	// back to whole renders — losing the ~110ms first-audio that streaming
	// exists for, on the setup that needs it most.
	type streamer interface {
		SpeakStream(context.Context, string) (<-chan []byte, error)
	}
	local, ok := ra.engine.(streamer)
	if !ok {
		return false // this backend cannot stream; render whole instead
	}

	// Already rendered once: play it whole. Streaming a cached line would add
	// latency to the one case that has none.
	ra.mu.Lock()
	cachedPCM, isCached := ra.rendered[call.Text]
	ra.mu.Unlock()
	if isCached {
		ra.mixer.enqueue(ra.cueFor(call, seq, cachedPCM, tts.LocalSampleRate))
		log.Printf("call [%s] %s (cached): %s", call.Key, priorityName(call.Priority), call.Text)
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	started := time.Now()

	chunks, err := local.SpeakStream(ctx, sayable(call.Text))
	if err != nil || chunks == nil {
		cancel()
		return false
	}

	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("race announcer: recovered while streaming %q: %v", call.Text, r)
			}
		}()

		var cue *boothCue
		var whole []byte

		for pcm := range chunks {
			whole = append(whole, pcm...)

			if cue == nil {
				// The heat went green while the FIRST fragment was
				// generating, so this belongs to the race before it. Checked
				// here rather than before starting, because that is when it
				// becomes knowable — and for EVERY call, not just the
				// build-up: a commentary line mid-render is exactly what
				// carried on over a heat that had already started.
				if ra.currentGen() != gen {
					log.Printf("call [%s] dropped, the heat went green while it rendered: %s",
						call.Key, call.Text)
					return
				}
				// A moment-tied call that took too long to START is dead.
				//
				// The gen check above only catches a NEW heat; within one heat
				// a stalled render sailed through and aired whenever the
				// starved machine got around to it — measured at 25.7 seconds
				// for a lap call, which aired after "race over" into the next
				// heat's staging and was heard as a pilot who "wasn't even in
				// the race". A lap time describes NOW; late, it is not slow
				// information but wrong information. Dropped loudly, so a
				// recurrence names itself in the log instead of on the PA.
				if perishableCall(call.Key) && time.Since(started) > perishableDeadline {
					log.Printf("call [%s] dropped, first audio took %.1fs — the moment has "+
						"passed (something is starving the voice engine): %s",
						call.Key, time.Since(started).Seconds(), call.Text)
					return
				}
				cue = ra.cueFor(call, seq, pcm, tts.LocalSampleRate)
				cue.streaming = true
				ra.mixer.enqueue(cue)
				log.Printf("call [%s] %s (first audio in %dms, streaming): %s",
					call.Key, priorityName(call.Priority),
					time.Since(started).Milliseconds(), call.Text)
				continue
			}
			ra.mixer.appendPCM(cue, pcm, tts.LocalSampleRate)
		}

		if cue == nil {
			return // produced nothing
		}
		// Releases the bus: without this the cue holds silence forever.
		ra.mixer.finishStream(cue)

		// Generated vs played, so a call heard as cut off can be attributed:
		// short generation is the model, short playback is the bus.
		genMs := int64(len(whole)) * 1000 / int64(tts.LocalSampleRate*2)
		go func() {
			time.Sleep(time.Duration(genMs)*time.Millisecond + 2*time.Second)
			if txt, playedMs := ra.mixer.streamPlayed(); txt == call.Text {
				if playedMs+150 < genMs {
					log.Printf("call [%s] CUT SHORT: generated %dms, played %dms — %s",
						call.Key, genMs, playedMs, call.Text)
				}
			}
		}()

		// Cached whole, so a fixed line repeats for free.
		ra.mu.Lock()
		ra.rendered[call.Text] = whole
		// Deliberately NOT written to disk here.
		//
		// This path is the lap calls, and caching them is the wrong trade twice
		// over. They are already fast — measured at 65ms to first audio, because
		// they are short and stream — so there is nothing to gain. And each one
		// carries a unique time, so a cached "Wizz 1.28" is essentially never
		// asked for again: it is the variable calls that made the existing pack
		// 7,092 clips and 496MB, for almost no benefit.
		//
		// Fixed phrases are cached instead, below — they repeat every heat.
		ra.order = append(ra.order, call.Text)
		for len(ra.order) > maxRenderedLines {
			delete(ra.rendered, ra.order[0])
			ra.order = ra.order[1:]
		}
		ra.mu.Unlock()
	}()

	return true
}

// cueFor builds a bus cue for a call.
func (ra *raceAnnouncer) cueFor(call RaceCall, seq int64, pcm []byte, rate int) *boothCue {
	return &boothCue{
		bornAt:     time.Now(),
		Kind:       "race_call",
		Text:       call.Text,
		PCM:        pcm,
		Rate:       rate,
		DurationMs: int64(len(pcm)) * 1000 / int64(rate*2),
		seq:        seq,
		priority:   call.Priority,
		callKey:    call.Key,
		leadBytes:  callStreamLead(call.Key),
	}
}

// callStreamLead sizes the audio a streaming call banks before it starts.
//
// The default 300ms is for calls that must be heard the instant they exist —
// a lap time, the start sequence. But the very START of a render is when the
// model is coldest, so 300ms can evaporate and the first second starves
// (heard as crackle on "Next up…" that "comes good after a second or two").
// Calls tied to no moment give up nothing by banking a full second first.
func callStreamLead(key SoundKey) int {
	switch key {
	case SoundRaceAnnounce, SoundAnnouncePilotChannel, SoundPilotsEnableVideo,
		SoundRaceDone, SoundRaceOver, SoundAfterTimesUp, SoundRaceAnnounceResults:
		return boothSampleRate * 2 // one second
	}
	return 0 // the streamLeadBytes default
}

// priorityName describes a call's rank in the log, so a queue-order question
// can be answered from the log alone.
func priorityName(p int) string {
	switch {
	case p >= callPriorityRaceState:
		return "race-state"
	case p >= callPriorityLap:
		return "lap"
	default:
		return "colour"
	}
}

// render turns words into audio, reusing anything already rendered.
// The bool reports whether it came from the cache.
func (ra *raceAnnouncer) render(text string) ([]byte, int, bool, error) {
	ra.mu.Lock()
	if pcm, ok := ra.rendered[text]; ok {
		ra.mu.Unlock()
		return pcm, tts.LocalSampleRate, true, nil
	}
	ra.mu.Unlock()

	// The pack first. This is what makes a lap call immediate rather than
	// merely prompt — assembling one from clips takes milliseconds, where
	// generating it takes about as long as the call itself.
	if pcm, ok := ra.pack.Render(text); ok {
		ra.mu.Lock()
		ra.rendered[text] = pcm
		ra.order = append(ra.order, text)
		ra.mu.Unlock()
		return pcm, boothSampleRate, true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pcm, rate, err := ra.engine.Speak(ctx, tts.Request{Text: sayable(text), Voice: ra.remoteVoice})
	if err != nil {
		return nil, 0, false, err
	}

	ra.mu.Lock()
	ra.rendered[text] = pcm
	ra.order = append(ra.order, text)
	for len(ra.order) > maxRenderedLines {
		delete(ra.rendered, ra.order[0])
		ra.order = ra.order[1:]
	}
	ra.mu.Unlock()

	return pcm, rate, false, nil
}

// startRaceAudio brings up the local voice and the announcer.
//
// Best-effort by design. Everything here is an ENHANCEMENT over FPVTrackside
// speaking for itself, so a missing model, a missing python or a missing
// Sounds.xml means the agent runs exactly as it did before — it just does not
// talk. Failing to start the agent over an audio feature would be a poor trade
// on race day.
// remoteVoice describes a hosted renderer, when one is configured.
type remoteVoice struct {
	APIKey  string
	Voice   string
	BaseURL string

	// HostURL is a model host on the local network — another machine lending
	// its GPU. Preferred over the hosted API when both are given: it is the
	// more explicit choice, it keeps the audio inside the venue, and a LAN hop
	// costs under a millisecond against the ~110ms the model itself takes.
	HostURL string
}

func (a *agent) startRaceAudio(profile string, cfg tts.LocalConfig, remote remoteVoice) {
	if a.mixer == nil {
		log.Printf("race audio: no audio bus, not starting")
		return
	}

	// A venue that has configured no voice keeps the race day it already had.
	//
	// dd-pits taking over the audio is an improvement, not a requirement: if
	// nothing here can speak — no local model, no Qwen key — then stepping in
	// would replace working FPVTrackside audio with silence. So it stands down
	// and says so, and everything else (dashboard, overlays, goggles, the JSON
	// API) is unaffected.
	// Recomputed from what this call was actually given, which is the truth for
	// the race calls specifically. It does not overwrite what the setup pages
	// were told, because that is resolved earlier and covers the case where
	// race audio never starts at all.
	hasVoice := cfg.Python != "" || remote.APIKey != "" || remote.HostURL != ""
	if source := callsSource(a.localCfg.CallsConfig(), hasVoice); source == "trackside" {
		log.Printf("race audio: %s", describeCallsSource(source, hasVoice))
		return
	}

	// A configured FPVTrackside location wins over the assumed one.
	soundsPath := a.localCfg.CallsConfig().soundsPath()
	if soundsPath == "" {
		soundsPath = DefaultSoundsPath(profile)
	}
	sounds, err := LoadSoundSet(soundsPath)
	if err != nil {
		log.Printf("race audio: %v — not speaking race calls", err)
		return
	}
	log.Printf("race audio: read %d sounds from %s", sounds.Count(), soundsPath)
	if lp := a.localCfg.CallsConfig().logPath(); lp != "" {
		log.Printf("race audio: following FPVTrackside's log at %s", lp)
	}

	// Where every call will come from, and a loud warning for the one
	// combination that is silent everywhere and looks like a bug.
	reportSoundConfig(sounds)

	if cfg.Script == "" {
		cfg.Script = defaultLocalVoiceScript()
	}

	// A REMOTE renderer, when one is configured.
	//
	// The trade, measured rather than assumed: ~320ms to first audio warm
	// against ~110ms local, in exchange for using no GPU at all — which is the
	// constraint behind nearly everything else here, since the commentary model
	// wants the same one. It also makes the calls depend on the venue's
	// internet, and a dropout means silence rather than lateness.
	//
	// Started synchronously: there is no model to load, so there is nothing to
	// wait for and no reason to defer the announcer.
	if remote.HostURL != "" {
		hasVoice, _, err := probeModelHost(remote.HostURL)
		if err != nil {
			log.Printf("race audio: model host %s unreachable (%v) — not speaking race calls", remote.HostURL, err)
			return
		}
		if !hasVoice {
			log.Printf("race audio: model host %s serves no voice (start it with -local-voice-python)", remote.HostURL)
			return
		}
		ra := newRaceAnnouncerAt(sounds, a.mixer, newRemoteEngine(remote.HostURL),
			a.localCfg.CallsConfig().logPath())
		ra.booth = a.booth
		if dir := defaultVoicePackDir(profile); dir != "" {
			if pack, perr := loadVoicePack(dir); perr == nil {
				ra.pack = pack
				log.Printf("race audio: %d pre-generated clips available locally", pack.Count())
			}
		}
		a.setRaceAnnouncer(ra)
		log.Printf("race audio: speaking FPVTrackside's calls via the model host at %s", remote.HostURL)
		return
	}

	// The venue's CHOICE wins over the mere presence of a key.
	//
	// Reported as "race calls are set to dd-pits but I hear no lap times". They
	// were set correctly. A Qwen key is needed for the SCRIPT, and its presence
	// alone routed the race calls to the cloud — which then drops every lap time
	// on purpose, because the cloud cannot land one on the lap. So configuring a
	// local voice and keeping a key for the words silently cost the lap calls,
	// with only a debug line to say so.
	preferLocal := cfg.Python != "" && cfg.Script != ""
	if a.localCfg != nil && strings.EqualFold(a.localCfg.BoothConfig().Engine, "local") {
		preferLocal = preferLocal && true
	} else if remote.APIKey != "" {
		preferLocal = false
	}
	if remote.APIKey != "" && !preferLocal {
		client, err := qwentts.New(qwentts.Config{APIKey: remote.APIKey, BaseURL: remote.BaseURL})
		if err != nil {
			log.Printf("race audio: remote voice unavailable (%v) — not speaking race calls", err)
			return
		}
		engine := tts.FromQwen(client)

		ra := newRaceAnnouncerAt(sounds, a.mixer, engine, a.localCfg.CallsConfig().logPath())
		ra.remoteVoice = remote.Voice
		ra.booth = a.booth

		// Cloud is too slow for a lap time (~320ms warm, before variance), so
		// the announcer plays only what the pack already holds. Commentary is a
		// different matter — a wrap between heats is not tied to a moment — so
		// the booth gets the same engine and uses it freely.
		ra.packOnly = true
		if a.booth != nil {
			a.booth.mu.Lock()
			if a.booth.speech == nil {
				a.booth.speech = engine
				a.booth.qwenVoicePBP, a.booth.qwenVoiceColor = remote.Voice, remote.Voice
			}
			a.booth.mu.Unlock()
		}
		if dir := defaultVoicePackDir(profile); dir != "" {
			if pack, perr := loadVoicePack(dir); perr == nil {
				ra.pack = pack
			}
		}
		a.setRaceAnnouncer(ra)
		voice := remote.Voice
		if voice == "" {
			voice = "default"
		}
		log.Printf("race audio: REMOTE Qwen3-TTS (voice %s) — fixed calls play from the pack, "+
			"lap times are SKIPPED (cloud is too slow to land them on the lap), commentary uses the cloud",
			voice)
		return
	}

	// Started in the background: a large model takes minutes to load, and the
	// timing loop must not wait for it. Calls simply begin once it is ready.
	go func() {
		engine, err := tts.StartLocal(context.Background(), cfg)
		if err != nil {
			log.Printf("race audio: local voice unavailable (%v) — not speaking race calls", err)
			return
		}

		ra := newRaceAnnouncer(sounds, a.mixer, engine)
		// Wired on EVERY construction path, and this one had been missed. The
		// remote and cloud paths set it; the local path did not — so on the
		// engine most venues actually run, the start sequence could not tell
		// the booth to stand down. The booth's own guard keys on the race
		// RUNNING, which begins seconds after the countdown, and a staged line
		// rendered into exactly that window: "the grid is set", aired one
		// second after ARM YOUR QUADS. preStart=false gen=0/0 in the airing
		// diagnostic is the fingerprint of this wiring being absent.
		ra.booth = a.booth

		// The clip library FPVTrackside generated in the operator's own voice.
		// Optional: without it every call is generated live, which works and is
		// simply slower.
		//
		// Matched to the voice ACTUALLY being spoken. A pack made in a different
		// voice is worse than no pack: the calls it holds play in that voice
		// while everything else is generated in this one, and the broadcast comes
		// out as two different people mid-heat.
		if dir := a.voicePackForCurrentVoice(profile); dir != "" {
			if pack, perr := newVoicePackAt(dir); perr == nil {
				ra.pack = pack
				log.Printf("race audio: %d pre-generated clips from %s — a call the pack holds WHOLE plays instantly, anything else is generated fresh",
					pack.Count(), filepath.Base(dir))
			} else {
				log.Printf("race audio: no clip library (%v) — every call will be generated live", perr)
			}
		}

		a.setRaceAnnouncer(ra)
		log.Printf("race audio: speaking FPVTrackside's calls — mute its own audio output")
	}()
}

// defaultLocalVoiceScript looks for the sidecar beside the agent binary, so a
// normal install needs no configuration.
func defaultLocalVoiceScript() string {
	return sidecarPath("local_tts_server.py")
}

// besideExecutable finds a helper script shipped next to the agent, so a normal
// install needs no paths configured.
func besideExecutable(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// raceAnnouncer returns the announcer once the voice has finished loading.
//
// Guarded because the model is brought up on its own goroutine — it takes
// minutes — while the poll loop reads this every tick.
func (a *agent) raceAnnouncer() *raceAnnouncer {
	a.announcerMu.Lock()
	defer a.announcerMu.Unlock()
	return a.announcer
}

func (a *agent) setRaceAnnouncer(ra *raceAnnouncer) {
	a.announcerMu.Lock()
	a.announcer = ra
	a.announcerMu.Unlock()
}

// Reprime re-baselines the calls after the link to FPVTrackside is re-made.
//
// Anything still queued describes a meeting that may no longer be loaded, and a
// pilot finishing is a fresh call again, so both are cleared with it.
func (ra *raceAnnouncer) Reprime(why string) {
	if ra == nil {
		return
	}
	if ra.caller != nil {
		ra.caller.Reprime(why)
	}
	if ra.bridge != nil {
		ra.bridge.NewHeat()
	}
}

// voicePackForCurrentVoice finds the clip library made in the voice now being
// used, and says plainly when a pack exists for a DIFFERENT one.
//
// The "different one" case matters: a venue that switches to their own local
// recording still has a 500MB pack sitting there from a cloud clone, and
// silently using it splits the broadcast between two voices.
func (a *agent) voicePackForCurrentVoice(profile string) string {
	if a.localCfg == nil {
		return ""
	}
	bc := a.localCfg.BoothConfig()

	// A local recording is the voice; the pack is keyed by its file name.
	want := strings.TrimSpace(bc.VoicePBP)
	if strings.EqualFold(bc.Engine, "local") && strings.TrimSpace(bc.VoiceRef) != "" {
		want = strings.TrimSuffix(filepath.Base(bc.VoiceRef), ".wav")
	}
	if dir := voicePackDirFor(profile, want); dir != "" {
		return dir
	}
	// Nothing for this voice YET. Start one, so the calls generated during the
	// first heat are kept and instant from then on.
	if want != "" {
		if home, err := os.UserHomeDir(); err == nil {
			pr := profile
			if pr == "" {
				pr = "Profile 1"
			}
			fresh := filepath.Join(home, "Documents", "FPVTrackside", "data", pr, "AIVoice", want)
			if err := os.MkdirAll(fresh, 0o755); err == nil {
				log.Printf("race audio: no clip library for %q yet — starting one. "+
					"The first heat generates each call and keeps it; after that they play "+
					"instantly.", want)
				return fresh
			}
		}
	}
	// Say what IS there, so "why is the countdown a different person" is
	// answered rather than left to be noticed.
	if other := defaultVoicePackDir(profile); other != "" {
		log.Printf("race audio: there is a clip library for %q but the voice in use is %q — "+
			"NOT using it, or the countdown and the lap times would be two different people. "+
			"Every call will be generated live instead, which is slower.",
			filepath.Base(other), want)
	}
	return ""
}
