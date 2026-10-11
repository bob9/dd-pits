package main

// booth: the live AI commentary engine (-booth). Watches the FPVTrackside
// snapshot the agent already fetches, detects race-day "beats" (heat staged,
// launch, holeshot, mid-race progress, finish, up-next, quiet-time filler),
// asks Gemini for one to three lines of two-person booth dialogue per beat —
// Ace (play-by-play) and Torque (analyst), the same personas as the cloud
// replay commentary — renders them with multi-speaker TTS, and plays the
// audio out through the local server's continuous /booth.wav stream, which
// OBS ingests as a media source into the outgoing YouTube mix.
//
// Every cue spoken while a heat is running is journalled with its spoken-at
// wall clock; when the heat ends, the cues are uploaded to the platform
// (booth_upload.go) as that race's replay commentary — the 3D replay then
// plays the exact audio that went to air, no regeneration.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/gemini"
	"github.com/bob9/dd-pits/internal/qwentts"
	"github.com/bob9/dd-pits/internal/tts"
)

const (
	boothSpeakerPBP   = boothpkg.SpeakerPBP
	boothSpeakerColor = boothpkg.SpeakerColor
	boothVoicePBP     = boothpkg.DefaultVoicePBP
	boothVoiceColor   = boothpkg.DefaultVoiceColor

	boothFillerEvery = 55 * time.Second // quiet-time chatter cadence
	// boothInHeatRecap: with nothing new on track, wait this long before
	// calling the race again anyway. Long enough that a re-cap sounds
	// deliberate rather than like the booth repeating itself.
	boothInHeatRecap = 25 * time.Second

	boothInHeatGap        = 5 * time.Second // in-heat: re-arm this soon after the last call so the next generates while the current airs (one lookahead) → near-continuous chatter
	boothPilotStallFactor = 2.2             // in-heat: silent for this many × the pilot's OWN usual lap (their meeting/track history) while racing = looks out
	boothTranscript       = 20              // recent lines kept for "don't repeat yourself"
	boothQueueCap         = 3
)

// boothBeat is one detected moment worth talking about. context is the
// pre-built data block the script prompt gets; higher priority beats evict
// queued lower ones (a finish call must never wait behind filler).
type boothBeat struct {
	kind     string // meeting_open|staged|launch|holeshot|progress|finish|filler
	raceSrc  string // FPVTrackside race id ("" = meeting-level talk)
	priority int
	context  string
	emotion  string
	// eventMs is the venue-clock instant the beat REPRESENTS (race start for
	// the launch, gate-one crossing for the holeshot). The replay upload
	// anchors the cue here instead of its on-air time, so generation latency
	// never pushes the call out of sync with the 3D replay. 0 = anchor at
	// the moment it aired.
	eventMs int64
	// extras is the pilot-cards + Stewards' Room block appended after the data
	// block — the pilots' own commentary preferences and the data-detected gags
	// this beat may riff on. Empty for beats with neither.
	extras string
}

// boothCue is one rendered utterance: the dialogue text, its audio, and —
// once the mixer starts playing it — the wall-clock instant it went to air.
// boothGenConcurrency bounds how many beats generate at once. Two is the
// useful number: it lets the next call be WRITTEN while the current one is
// still rendering audio, which is where the dead air was, without stacking
// requests into a provider rate limit (the free Gemini tier allows 3/minute)
// or spending tokens on calls the meeting has already moved past.
const boothGenConcurrency = 2

// scriptWriter is a model that can write a commentary script. Both the gemini
// and qwenchat clients satisfy it, so the fast path can pick whichever the
// tenant's keys allow without the call site caring.
type scriptWriter interface {
	GenerateText(ctx context.Context, prompt string) (string, error)
}

// boothFastScriptModel writes snappy beats: fast because it does no thinking
// (~1.4s vs 4-14s), and chosen over the marginally quicker qwen-flash on
// ACCURACY. Benchmarking caught qwen-flash calling lap 2 of 3 "the final lap",
// and on air it mis-attributed lap times between pilots and invented a
// seconds-gap for a pilot who was a full lap down. A live race call has to be
// right first and quick second.
const boothFastScriptModel = "gemini-3.1-flash-lite"

// scriptNameSafe reports the fast script model for logging.
func (b *booth) scriptNameSafe() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.scriptName == "" {
		return "fast model"
	}
	return b.scriptName
}

type boothCue struct {
	// bornAt is when the cue was handed to the bus, so the wait before it
	// actually aired can be measured.
	//
	// Every other stage of a race call is instrumented — detection, generation,
	// first audio — and they consistently totalled under 200ms while the
	// operator heard the call seconds late. The gap between "audio exists" and
	// "audio is playing" was the one stage nobody had ever put a number on.
	bornAt time.Time

	RaceSrc string
	Kind    string // beat kind — lets the mixer cut a meeting_talk cue when a heat goes green

	// callKey identifies a race call, so the bus can drop the pre-race ones the
	// moment a heat goes green. Empty for commentary.
	callKey SoundKey

	// commentary marks a cue as the booth's colour rather than a race call.
	//
	// Explicit, because "has no callKey" is NOT the same thing: a relayed call
	// whose sound could not be identified also has no key, and treating it as
	// commentary would rank real lap times below the booth.
	commentary bool

	// fadeLeft counts down a short fade-out when a cue is being cut short.
	//
	// Stopping a cue on the exact sample it was reached truncates a word — what
	// went to air as "Pil—" in place of "Pilots turn on your video
	// transmitters". A brief ramp makes the same interruption sound like a
	// deliberate duck instead of a fault.
	fadeLeft int

	// streaming marks a cue whose audio is still being generated.
	//
	// Race calls must be heard the instant the lap lands, and generation runs
	// at roughly real time — so waiting for a finished utterance puts every
	// call a couple of seconds behind the thing it describes. A streaming cue
	// airs its first fragment immediately and grows while it plays.
	streaming  bool
	streamDone bool

	// leadBytes raises the amount of audio a streaming cue must bank before it
	// STARTS (0 = the default streamLeadBytes). The default is sized for calls
	// that must be heard the instant they exist — but the very start of a
	// render is when the model is coldest, so a 300ms lead can evaporate and
	// the first second of the cue starves. For calls tied to no moment ("Next
	// up…", the post-race wrap) that immediacy buys nothing: they bank a full
	// second instead and start clean.
	leadBytes int

	// rebankLeft holds a streaming cue that ran dry MID-PLAY until this many
	// bytes are banked again. Without it, every starved frame played whatever
	// sliver had arrived and zero-filled the rest — a hard edge into silence
	// fifty times a second, heard as crackle. One clean pause beats that, and
	// the cushion stops the starve/resume flapping from re-creating the edges.
	rebankLeft int
	RaceName   string
	Text       string
	Emotion    string
	PCM        []byte
	Rate       int
	DurationMs int64
	EventMs    int64 // venue-clock anchor of the represented moment (0 = spokenAt)

	// priority orders the bus above everything else. A RACE CALL always beats
	// commentary: pilots and the race director need the times, and colour is
	// there to fill the gaps between them. Zero (the default for every existing
	// booth cue) is commentary.
	priority int

	// seq is the order the BEAT was taken off the queue, stamped before
	// generation starts. Generation runs concurrently, so a later beat can
	// finish first; seq is what puts the calls back in the order they happened.
	// Scheduled cues sort by playAt and only use seq to break a tie.
	seq int64

	// playAt is the wall-clock instant this cue should hit the air so it lines
	// up with the (equally-delayed) video: event moment + the broadcast buffer.
	// Zero = play as soon as the bus is free (meeting-level filler/welcome,
	// which has no on-track moment to sync to).
	playAt time.Time

	mu       sync.Mutex
	spokenAt time.Time
}

func (c *boothCue) markSpoken(t time.Time) {
	c.mu.Lock()
	if c.spokenAt.IsZero() {
		c.spokenAt = t
	}
	c.mu.Unlock()
}

func (c *boothCue) SpokenAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spokenAt
}

type booth struct {
	gem *gemini.Client // server-configured (or local dev flags); nil until config arrives
	// speech is the VOICE backend when the club configured one explicitly
	// (Qwen3-TTS); nil = render with gem. Scripts always come from gem.
	speech tts.Engine
	// script, when set, writes SNAPPY beats instead of gem. Built from the same
	// Qwen key as the voice backend; nil = every beat is written by Gemini.
	// script, when set, writes SNAPPY beats instead of gem — a model chosen
	// for LATENCY (thinking disabled), which is what made those calls late.
	// nil = every beat is written by gem.
	script     scriptWriter
	scriptName string

	// mainScript replaces Gemini as the writer for EVERY beat.
	//
	// Distinct from `script`, which only takes the snappy beats and falls back
	// to Gemini: this is the whole booth running without a network. A venue's
	// uplink is frequently the worst part of the setup, and a booth that goes
	// silent when it drops is a booth nobody trusts to run a meeting.
	//
	// nil = Gemini writes, which stays the default because it is the better
	// writer whenever the connection can be relied on.
	mainScript     scriptWriter
	mainScriptName string
	// mainScriptLocal distinguishes a writer running HERE from a remote one.
	//
	// mainScriptName is set for a local model, a remote Qwen model and a remote
	// model host alike, so labelling it "on this Mac" was wrong two times out of
	// three — the status panel cheerfully reported a cloud model as local.
	mainScriptLocal bool

	// yieldWhileRacing keeps commentary off the GPU while a heat is running.
	//
	// Local models and the race calls share one GPU, and Metal serialises there
	// no matter how many processes are involved — so a 17-31s commentary
	// generation starves the lap calls behind it, and a call that lands twelve
	// seconds after the lap is worse than no call at all.
	//
	// Between heats there is nothing time-critical, so colour costs nothing
	// there. This trades mid-race commentary for calls that land on the lap,
	// which is the right way round at a race day: the calls are the job.
	//
	// Only meaningful for a LOCAL writer. A cloud model does its thinking on
	// someone else's hardware and never competes for this one.
	yieldWhileRacing bool
	qwenKey          string
	qwenVoicePBP     string
	qwenVoiceColor   string
	mixer            *boothMixer

	// Control from the setup pages: master enable, per-beat toggles, and
	// the Gemini key.
	enabled   bool
	beatCfg   map[string]bool
	cfgKey    string
	cfgModel  string
	voiceMode string // "duo" (default) | "solo"

	// onAnnounceRecords carries the website's personal-best toggle out to the
	// race announcer, which is a separate object from the booth. Set by the
	// agent once both exist; nil in tests and in local-only mode.
	onAnnounceRecords func(bool)

	// heatGen advances every time a heat goes green.
	//
	// Commentary takes many seconds to write and voice, so cutting the bus at
	// the green light does nothing about a line already in flight: it finished
	// afterwards and played over live racing. Each line remembers the
	// generation it was started in and is thrown away if that has moved on.
	heatGen int64

	// preStart marks the window between the operator pressing Start and the
	// heat going green.
	//
	// Every other chatter guard keys on the race RUNNING, which is several
	// seconds too late: the countdown plays first, and during it the race is
	// still "pending" — so a staged cue rendering in that window passes every
	// check and airs over the start. Heard as the booth announcing the next
	// heat on the grid after Start had already been pressed.
	preStart   bool
	voicePBP   string
	voiceColor string
	voiceStyle string // admin-written voice characteristics
	voiceTheme string // commentary personality theme (blank = classic)
	// style carries the resolved presentation config (per-phase personality +
	// humour dial) from the website's config poll; triggers is the Stewards'
	// Room toggle set (nil = the shared defaults).
	style    boothpkg.Style
	triggers map[string]bool
	// trigInHeat also allows those gags in the in-heat action calls, not just
	// the post-race wrap (booth.TriggerBeat). Off = the previous behaviour.
	trigInHeat bool
	// trigSaid marks the bits already handed to the model this heat, keyed
	// "<raceID>\x00<trigger key>\x00<pilot>". Mid-heat detection re-finds the
	// same anomaly on EVERY progress beat (one every few seconds), so without
	// this the booth would hammer the same joke all heat; it also stops the
	// finish call repeating a gag that already aired. Cleared per heat by
	// resetHeatPilots, so a rerun starts fresh.
	trigSaid      map[string]bool
	shortCalls    bool
	scriptBackend string // snappy mode: brevity on the in-heat action beats
	phasePre      bool   // race-director phase toggles (default on)
	phaseIn       bool
	phasePost     bool
	// Mid-race meeting-talk cadence: "periodic" (every meetingTalkSecs during a
	// heat) or "bookend" (once before + once after each race). Cut on green either
	// way. Defaults periodic / 50s.
	meetingTalkMode string
	meetingTalkSecs int
	brackets        *boothBracketContext
	usage           boothUsage

	// Broadcast sync: hold each generated call until event_time + buffer so it
	// lines up with an equally-delayed video. sync sizes the buffer from real
	// generation latency; obs (optional) drives OBS's video delay to match;
	// manualDelayMs is a fixed fallback when the operator sets OBS by hand.
	// Audio is only held when a matching video delay is in force (obs != nil or
	// manualDelayMs > 0) — holding it otherwise would just push it later.
	sync          *boothSync
	obs           *boothOBS
	manualDelayMs int64

	// Broadcast sync can be driven from the website (booth_config.go): runCtx
	// is the base context reconcileSync spawns the OBS goroutine under, audioURL
	// is the /booth.wav source injected into OBS, obsCancel stops the current
	// OBS goroutine, and the wantSync* fields track the applied server settings
	// so a re-poll only re-wires OBS when something actually changed.
	runCtx      context.Context
	audioURL    string
	obsCancel   context.CancelFunc
	wantSync    string // "" (flags in charge) | off | auto | manual
	wantObsURL  string
	wantObsPass string
	wantInject  bool

	mu       sync.Mutex
	snap     dronedash.Snapshot
	have     bool
	beats    []boothBeat // tiny priority queue (cap boothQueueCap)
	beatWake chan struct{}

	// beat-detection state, keyed by FPVTrackside race source id
	lastState    map[string]string
	lapsSeen     map[string]int
	holeshotDone map[string]bool
	stagedDone   map[string]bool
	finishDone   map[string]bool
	openDone     bool
	producing    int   // beats currently generating (gates the in-heat lookahead)
	beatSeq      int64 // monotonic, stamped on each beat as it is popped
	// lastProgressSig is the race state the last in-heat call described, so a
	// new call is only made once something has actually changed.
	lastProgressSig string
	// progressAngle cycles the mid-heat lens so consecutive calls differ in
	// SHAPE, not just wording.
	progressAngle int
	genSlots      chan struct{} // bounds concurrent generation

	// renderCancels are the in-flight beats' context cancels, so the start
	// sequence (and a race skip) can ABORT a render rather than merely
	// discard its result — the discarded render kept the GPU busy for its
	// full length, which is what starved the live lap calls into 3-8s first
	// audio while a 25s staged line rendered through a heat.
	renderCancels map[int64]context.CancelFunc
	renderSeq     int64
	// scriptSlot serialises the script phase (size 1). Audio renders outside
	// it, so beats still overlap — see speak().
	scriptSlot      chan struct{}
	lastSpoke       time.Time
	lastMeetingTalk time.Time // last mid-race meeting-talk beat (paces meeting_talk under progress)

	// Pilot-out/back tracking, keyed by raceID+"\x00"+pilot name: the last lap
	// count seen for each pilot, when it last advanced, and whether they look
	// out of the race (stopped lapping while the field races on).
	pilotLaps  map[string]int
	pilotLapAt map[string]time.Time
	pilotOut   map[string]bool

	// Qualifying ladder tracking (time trials): each pilot's last-seen board
	// position, so a climb up the leaderboard fires a qual_move call. Keyed by
	// pilot name (the board is meeting-wide); reset when the meeting changes.
	pilotBoardPos  map[string]int
	boardSeeded    bool
	boardMeetingID string

	transcript []string

	// tape keeps the last few cues for the debug page, so "is it generating
	// anything?" can be answered without the speaker path working at all.
	tape *boothTape
}

func newBooth(gem *gemini.Client) *booth {
	b := &booth{
		gem:   gem,
		mixer: newBoothMixer(),
		tape:  newBoothTape(),
		// On until a config says otherwise. These used to be left at the zero
		// value and beatEnabled had to short-circuit on a nil beat map to avoid
		// silencing everything — which meant a venue that set the PHASES but
		// never touched an individual beat had its phases quietly ignored.
		phasePre:        true,
		phaseIn:         true,
		phasePost:       true,
		beatWake:        make(chan struct{}, 1),
		genSlots:        make(chan struct{}, boothGenConcurrency),
		scriptSlot:      make(chan struct{}, 1),
		lastState:       map[string]string{},
		lapsSeen:        map[string]int{},
		holeshotDone:    map[string]bool{},
		stagedDone:      map[string]bool{},
		finishDone:      map[string]bool{},
		pilotLaps:       map[string]int{},
		pilotLapAt:      map[string]time.Time{},
		pilotOut:        map[string]bool{},
		pilotBoardPos:   map[string]int{},
		trigSaid:        map[string]bool{},
		lastSpoke:       time.Now(),
		lastMeetingTalk: time.Now(),
		meetingTalkMode: "periodic",
		meetingTalkSecs: 50,
		sync:            newBoothSync(),
	}
	if gem != nil {
		// Local dev mode: gemini flags on the command line — always on.
		b.enabled = true
		gem.OnUsage = b.usage.add
	}
	return b
}

// observe hands the booth the latest snapshot (called from the fetch loop —
// must never block) and runs beat detection on the diff.
func (b *booth) observe(snap dronedash.Snapshot) {
	if snap.Partial {
		// Partial snapshots carry only the running heat; merge would be complex.
		// The fetch loop sends a full snapshot on every heat start/finish, so
		// beat detection on fulls alone only delays mid-heat progress slightly.
		b.mu.Lock()
		if b.have {
			b.mergeRunning(snap)
		}
		b.mu.Unlock()
		b.detect()
		return
	}
	b.mu.Lock()
	b.snap, b.have = snap, true
	b.mu.Unlock()
	b.detect()
}

// mergeRunning overlays a partial snapshot's running race(s) onto the stored
// full snapshot so lap-progress beats fire between full pushes.
func (b *booth) mergeRunning(part dronedash.Snapshot) {
	for _, pr := range part.Races {
		for i, r := range b.snap.Races {
			if r.ID == pr.ID {
				b.snap.Races[i] = pr
			}
		}
	}
}

// detect diffs the snapshot against the last-seen state and queues beats.
func (b *booth) detect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.have {
		return
	}
	if !b.enabled {
		// Admin switched the booth off on the website — go quiet immediately,
		// but keep tracking state so re-enabling doesn't replay old moments.
		b.beats = nil
		b.trackStateLocked()
		return
	}
	mv := buildMeeting(b.snap)
	byID := map[string]vmRace{}
	for _, r := range mv.Races {
		byID[r.ID] = r
	}

	// Welcome, once, when a meeting with races is loaded.
	if !b.openDone && len(mv.Races) > 0 {
		b.openDone = true
		b.queueLocked(boothBeat{
			kind: "meeting_open", priority: 2, emotion: "calm",
			context: beatContext("meeting_open", ddapi.Race{}, mv, beatExtrasCtx{BracketStructure: b.bracketStructure()}),
		})
	}

	for _, r := range mv.Races {
		prev := b.lastState[r.ID]
		if r.State != prev {
			switch r.State {
			case "running":
				// A heat just went green: pre-race chatter (welcome, filler,
				// staged talk) is instantly stale — drop it from the queue AND
				// cut it off the air, so the launch/holeshot calls own the bus.
				b.clearMeetingTalkLocked()
				b.resetHeatPilots(r.ID) // fresh pilot-out tracking (also on a rerun)
				if prev != "" || !b.stagedDone[r.ID] {
					b.queueLocked(boothBeat{
						kind: "launch", raceSrc: r.ID, priority: 4, emotion: "excited", eventMs: r.StartMs,
						context: beatContext("launch", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID)}),
						extras:  b.beatExtras("launch", &r),
					})
				}
			case "ended":
				// The heat is over: the booth may talk again. Cleared here
				// rather than on green, because the window this closes starts
				// at the countdown and the green light is inside it.
				b.preStart = false

				if prev == "running" && !b.finishDone[r.ID] {
					b.finishDone[r.ID] = true
					b.queueLocked(boothBeat{
						kind: "finish", raceSrc: r.ID, priority: 5, emotion: "big-finish", eventMs: time.Now().UnixMilli(),
						context: beatContext("finish", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID), BracketStructure: b.bracketStructure(), BoardMove: b.boardMoveContext(mv)}),
						extras:  b.beatExtras("finish", &r),
					})
					// Bookend mode: one meeting-level conversation AFTER the race.
					if b.meetingTalkMode == "bookend" {
						b.queueLocked(boothBeat{
							kind: "meeting_talk", raceSrc: r.ID, priority: 2, emotion: "calm", eventMs: time.Now().UnixMilli(),
							context: beatContext("meeting_talk", r, mv, beatExtrasCtx{BracketStructure: b.bracketStructure()}),
						})
					}
				}
			}
			b.lastState[r.ID] = r.State
		}

		if r.State != "running" {
			continue
		}
		// Holeshot beat: the first pilots have reached gate one.
		hs := 0
		for _, p := range r.Pilots {
			if p.Holeshot > 0 {
				hs++
			}
		}
		if hs > 0 && !b.holeshotDone[r.ID] && !boothpkg.HeatIsTimeTrial(r) {
			b.holeshotDone[r.ID] = true
			// The holeshot call must land NOW: it outranks everything but the
			// finish, and a still-queued launch call for this heat is stale —
			// evict it so the gate-one call isn't stuck behind "and they're off".
			kept := b.beats[:0]
			for _, q := range b.beats {
				if !(q.kind == "launch" && q.raceSrc == r.ID) {
					kept = append(kept, q)
				}
			}
			b.beats = kept
			// Replay anchor: the first pilot's gate-one crossing instant.
			hsMs := r.StartMs
			first := 0.0
			for _, p := range r.Pilots {
				if p.Holeshot > 0 && (first == 0 || p.Holeshot < first) {
					first = p.Holeshot
				}
			}
			if hsMs > 0 {
				hsMs += int64(first * 1000)
			}
			b.queueLocked(boothBeat{
				kind: "holeshot", raceSrc: r.ID, priority: 5, emotion: "excited", eventMs: hsMs,
				context: beatContext("holeshot", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID)}),
				extras:  b.beatExtras("holeshot", &r),
			})
		}
		// Pilot out / back: a racing pilot who stops putting in laps while the
		// field races on looks out of it; when they lap again they're back.
		// Races only — a time-trial pilot pausing between runs isn't "lost".
		if !boothpkg.HeatIsTimeTrial(r) {
			now := time.Now()
			for _, p := range r.Pilots {
				key := r.ID + "\x00" + p.Name
				n := len(p.Laps)
				if prev, seen := b.pilotLaps[key]; !seen || n > prev {
					wasOut := b.pilotOut[key]
					b.pilotLaps[key], b.pilotLapAt[key], b.pilotOut[key] = n, now, false
					if wasOut && seen && n > prev && b.beatEnabled("pilot_back") {
						b.queueLocked(boothBeat{
							kind: "pilot_back", raceSrc: r.ID, priority: 3, emotion: "excited", eventMs: now.UnixMilli(),
							context: beatContext("pilot_back", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID)}),
						})
					}
					continue
				}
				// No new lap since last seen.
				if b.pilotOut[key] || n < 1 {
					continue // already flagged, or hasn't completed a lap yet
				}
				if p.Position > 0 || (r.TargetLaps > 0 && n >= r.TargetLaps) {
					continue // finished their laps — stopping is expected
				}
				lastAt := b.pilotLapAt[key]
				if lastAt.IsZero() {
					continue
				}
				// Threshold scales to the pilot's OWN usual lap at this track,
				// averaged over their whole meeting history — no fixed seconds.
				// No history yet → can't judge their pace, so don't flag.
				typical := pilotTypicalLapSecs(mv, p.Name)
				if typical <= 0 {
					continue
				}
				if now.Sub(lastAt) < time.Duration(typical*boothPilotStallFactor*float64(time.Second)) {
					continue
				}
				b.pilotOut[key] = true
				if b.beatEnabled("pilot_out") {
					b.queueLocked(boothBeat{
						kind: "pilot_out", raceSrc: r.ID, priority: 4, emotion: "tense", eventMs: now.UnixMilli(),
						context: beatContext("pilot_out", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID)}),
					})
				}
			}
		}

		// In-heat coverage: whenever the bus has been quiet for a beat while
		// the heat runs, call the state of the race — this is what fills the
		// space BETWEEN laps with live audio instead of dead air. Every so
		// often, step back to meeting-level conversation (standings/storylines)
		// instead of another running-order call, when that beat is enabled.
		// Both carry eventMs (≈now, the moment they describe) so the broadcast-
		// sync buffer holds them to land on the equally-delayed video rather
		// than airing early. The gate allows queuing while the CURRENT call
		// airs (producing false, nothing waiting) so the next generates during
		// playback — one lookahead — for near-continuous chatter without runaway.
		// A progress call needs something to REPORT. The gate above is purely
		// time-based, but laps land every ~20s while it re-arms every 5 — so
		// four calls in a row would describe an identical race state and the
		// model, having nothing new, would say the same thing again ("bob9
		// leads, 10.97 to 10.53" four times over). Fire only when the race has
		// actually moved since the last call, or when it has been quiet long
		// enough that a re-cap is worth hearing.
		raceSig := boothRaceProgressSig(r)
		moved := raceSig != b.lastProgressSig
		stale := time.Since(b.lastSpoke) >= boothInHeatRecap
		if time.Since(b.lastSpoke) >= boothInHeatGap && (moved || stale) &&
			b.producing == 0 && b.mixer.pending() == 0 && len(b.beats) == 0 {
			now := time.Now()
			b.lastProgressSig = raceSig
			// Periodic mid-race meeting talk only in "periodic" mode, every
			// meetingTalkSecs. In "bookend" mode it fires once before + once after
			// each race instead (staged / ended blocks), never mid-heat.
			mtGap := time.Duration(b.meetingTalkSecs) * time.Second
			// Conversation only fills SILENCE. When the race has actually moved
			// there is a lap time and a position to call, and that always wins —
			// chatter while a new lap is waiting is what makes the booth sound
			// like it is ignoring the racing.
			if !moved && b.meetingTalkMode == "periodic" && b.beatEnabled("meeting_talk") && mtGap > 0 && now.Sub(b.lastMeetingTalk) >= mtGap {
				b.lastMeetingTalk, b.lastSpoke = now, now
				b.queueLocked(boothBeat{
					kind: "meeting_talk", raceSrc: r.ID, priority: 2, emotion: "calm", eventMs: now.UnixMilli(),
					context: beatContext("meeting_talk", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID), BracketStructure: b.bracketStructure()}),
				})
			} else {
				b.lastSpoke = now // re-armed properly when the cue airs
				b.queueLocked(boothBeat{
					kind: "progress", raceSrc: r.ID, priority: 2, emotion: "tense", eventMs: now.UnixMilli(),
					context: beatContext("progress", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID)}),
					extras:  b.beatExtras("progress", &r),
				})
			}
		}
	}

	// Qualifying ladder move: while a time-trial heat runs, a pilot who climbs
	// the leaderboard (a new PB run improving their position) gets a call. The
	// board is meeting-wide and keyed by name, so reset it when the meeting
	// changes and never fire on the first board we see (baseline only).
	if mv.MeetingID != b.boardMeetingID {
		b.boardMeetingID = mv.MeetingID
		b.pilotBoardPos = map[string]int{}
		b.boardSeeded = false
	}
	runningTT := false
	for _, r := range mv.Races {
		if r.State == "running" && boothpkg.HeatIsTimeTrial(r) {
			runningTT = true
			break
		}
	}
	if runningTT {
		var moverName, moverTime string
		var moverFrom, moverTo, bestClimb int
		for _, s := range mv.Board {
			prev, seen := b.pilotBoardPos[s.Name]
			b.pilotBoardPos[s.Name] = s.Pos
			hasTime := s.BestConsec > 0 || s.BestLap > 0
			if b.boardSeeded && seen && hasTime && s.Pos > 0 && s.Pos < prev {
				if climb := prev - s.Pos; climb > bestClimb { // biggest climber this cycle
					bestClimb, moverName, moverFrom, moverTo = climb, s.Name, prev, s.Pos
					if s.BestConsec > 0 {
						moverTime = fmt.Sprintf("%.2fs (best %d consecutive)", s.BestConsec, mv.Window)
					} else {
						moverTime = fmt.Sprintf("best lap %.2fs", s.BestLap)
					}
				}
			}
		}
		b.boardSeeded = true
		if moverName != "" && b.beatEnabled("qual_move") {
			now := time.Now()
			b.queueLocked(boothBeat{
				kind: "qual_move", priority: 3, emotion: "excited", eventMs: now.UnixMilli(),
				context: beatContext("qual_move", ddapi.Race{}, mv, beatExtrasCtx{LadderMove: ladderMoveLine(moverName, moverTime, moverFrom, moverTo)}),
			})
		}
	}

	// Staged beat: a cued (selected, unstarted) heat — announce the lineup.
	if cued := b.snap.SelectedRaceID; cued != "" && b.snap.HasCuedPendingRace() && !b.stagedDone[cued] {
		if r, ok := byID[cued]; ok && len(r.Pilots) > 0 {
			b.stagedDone[cued] = true
			b.queueLocked(boothBeat{
				kind: "staged", raceSrc: r.ID, priority: 3, emotion: "calm", eventMs: time.Now().UnixMilli(),
				context: beatContext("staged", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID), PilotNotes: b.pilotColour(r)}),
				extras:  b.beatExtras("staged", &r),
			})
			// Bookend mode: one meeting-level conversation BEFORE the race (cut
			// the instant it goes green via clearMeetingTalkLocked).
			if b.meetingTalkMode == "bookend" {
				b.queueLocked(boothBeat{
					kind: "meeting_talk", raceSrc: r.ID, priority: 2, emotion: "calm", eventMs: time.Now().UnixMilli(),
					context: beatContext("meeting_talk", r, mv, beatExtrasCtx{BracketRace: b.bracketRaceLine(r.ID), BracketStructure: b.bracketStructure()}),
				})
			}
		}
	}

	// Filler: quiet track, keep the broadcast alive with meeting talk.
	if !b.snap.HasRunningRace() && time.Since(b.lastSpoke) >= boothFillerEvery && len(b.beats) == 0 && !b.mixer.busy() {
		b.lastSpoke = time.Now() // re-armed properly when the cue airs
		b.queueLocked(boothBeat{
			kind: "filler", priority: 1, emotion: "calm",
			context: beatContext("filler", ddapi.Race{}, mv, beatExtrasCtx{BracketStructure: b.bracketStructure()}),
		})
	}
}

// boothChatterKind reports the meeting-level (non-racing) beats: the welcome,
// filler, the staged lineup and meeting talk. A green light kills every one of
// them — queued, mid-generation, or on air — and none may start or air while a
// heat is running; the conversation resumes once the race is over.
//
// One list, read by BOTH the green-light cut and the render-time drop in
// speak(). They were separate lists and drifted: meeting_talk was missing from
// the drop, so a bookend "before the race" conversation that was still
// rendering when the lights went green sailed through and played over the
// racing — the queue cut couldn't see it (already popped) and the mixer cut
// couldn't see it (not yet enqueued).
// beginStartSequence closes the door on chatter when the countdown fires.
//
// Called from the race announcer, which sees the countdown; the booth cannot
// see it, because pressing Start is an operator action and the timing data
// still reports the heat as pending until the tone.
func (b *booth) beginStartSequence() {
	b.mu.Lock()
	already := b.preStart
	b.preStart = true
	// Abandon anything being written or voiced right now. It is about the
	// build-up or the heat before, and by the time it is ready the race will
	// be under way. ABORTED, not merely discarded: cancelling reaches through
	// the engine to STOP the sidecar's generation, freeing the GPU for the
	// live lap calls — a discarded 25s staged render used to keep rendering
	// through the whole heat and starve them.
	b.heatGen++
	for _, cancel := range b.renderCancels {
		cancel()
	}
	if !already {
		b.clearMeetingTalkLocked()
	}
	b.mu.Unlock()
	if !already {
		log.Printf("booth: start sequence — chatter closed until the heat is over")
	}
}

// runningRaceID reports the heat currently under way, if any.
func (b *booth) runningRaceID() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snap.RunningRaceID()
}

// chatterClosed reports whether the booth should stay out of the way.
func (b *booth) chatterClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.preStart || b.snap.HasRunningRace()
}

func boothChatterKind(kind string) bool {
	switch kind {
	case "meeting_open", "filler", "staged", "meeting_talk":
		return true
	}
	return false
}

// clearMeetingTalkLocked drops pre-race chatter the instant a heat goes
// green: queued chatter beats are evicted, and a meeting-level cue currently
// ON AIR (or queued in the mixer) is cut off mid-sentence — a live race always
// interrupts the smalltalk. Caller holds b.mu.
func (b *booth) clearMeetingTalkLocked() {
	kept := b.beats[:0]
	for _, q := range b.beats {
		if !boothChatterKind(q.kind) {
			kept = append(kept, q)
		}
	}
	b.beats = kept
	// The same chatter beats are cut off the AIR, using the same predicate that
	// evicted them from the queue above. One definition, so the two cannot
	// disagree about what chatter is — and they did: the air-side test was
	// "meeting-level (no RaceSrc) or meeting_talk", which misses `staged`,
	// because the line-up beat is ABOUT a heat and so carries a RaceSrc. It was
	// evicted from the queue and left playing. Heard as commentary continuing
	// over the start sequence after the button was pressed.
	//
	// `c.commentary` is load-bearing and is what keeps race calls safe. It is
	// stamped at the booth's single enqueue point, so it is true of booth cues
	// and nothing else. The RaceSrc clause it replaces was doing that job by
	// accident and doing it wrong in both directions — it also matched every
	// race call, cutting "Next up Round 5" off the moment a post-race summary
	// arrived. Race calls are never interrupted by commentary; commentary
	// queues behind them or is dropped, never the other way round.
	b.mixer.interrupt(func(c *boothCue) bool {
		return c.commentary && boothChatterKind(c.Kind)
	})
}

// raceIsRunning reports whether a heat is green right now — the gate on every
// meeting-level beat (see boothChatterKind).
// yieldsWhileRacing reports whether commentary should stand aside for the calls.
func (b *booth) yieldsWhileRacing() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.yieldWhileRacing
}

func (b *booth) raceIsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snap.HasRunningRace()
}

// resetHeatPilots clears a heat's pilot-out tracking (caller holds b.mu) so a
// (re)started heat begins fresh — a rerun's lap counts drop back to zero, which
// would otherwise read as "everyone stopped".
func (b *booth) resetHeatPilots(raceID string) {
	prefix := raceID + "\x00"
	for k := range b.pilotLaps {
		if strings.HasPrefix(k, prefix) {
			delete(b.pilotLaps, k)
			delete(b.pilotLapAt, k)
			delete(b.pilotOut, k)
		}
	}
	// A rerun is a fresh heat, so its gags are available again.
	for k := range b.trigSaid {
		if strings.HasPrefix(k, prefix) {
			delete(b.trigSaid, k)
		}
	}
}

// pilotTypicalLapSecs is the pilot's usual lap time at this meeting, averaged
// over EVERY racing lap they've put in across all heats — their track history,
// a stable pace estimate for stall detection that scales to the track and the
// pilot. Returns 0 when they have no completed laps anywhere (can't judge).
func pilotTypicalLapSecs(mv *meetingView, name string) float64 {
	var sum float64
	var count int
	for _, r := range mv.Races {
		for _, p := range r.Pilots {
			if p.Name != name {
				continue
			}
			for _, l := range p.Laps {
				if l > 0 {
					sum += l
					count++
				}
			}
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// trackStateLocked advances the seen-state maps without queueing beats — used
// while the booth is disabled so re-enabling never replays stale moments.
func (b *booth) trackStateLocked() {
	mv := buildMeeting(b.snap)
	for _, r := range mv.Races {
		b.lastState[r.ID] = r.State
		if r.State != "pending" {
			b.stagedDone[r.ID] = true
		}
		if r.State == "ended" {
			b.finishDone[r.ID] = true
			b.holeshotDone[r.ID] = true
		}
	}
	if len(mv.Races) > 0 {
		b.openDone = true
	}
	b.lastSpoke = time.Now()
}

// bracketStructure formats the meeting's bracket structure block for prompts
// (caller holds b.mu). Empty when the meeting has no linked brackets.
func (b *booth) bracketStructure() string {
	if b.brackets == nil {
		return "" // context not fetched yet — assert nothing either way
	}
	if len(b.brackets.Brackets) == 0 {
		// Say the absence outright. Left silent, the finish brief's talk of
		// advancement had nothing to attach to and the booth invented a
		// bracket — pilots "advancing to Winners Round 2" at a meeting that
		// was simply a series of races.
		return "\nEVENT STRUCTURE: NO BRACKET at this meeting — these are standalone races. " +
			"There are no winners or redemption lanes, no advancement to another heat, and nobody is eliminated. " +
			"NEVER mention a winners bracket, a redemption bracket, advancing, or knock-out — none of it exists here. " +
			"A result is simply the finishing order of that race.\n"
	}
	var sb strings.Builder
	sb.WriteString("\nEVENT STRUCTURE (race brackets running at this meeting — call advancement correctly):\n")
	for _, br := range b.brackets.Brackets {
		fmt.Fprintf(&sb, "- %s: %s\n", br.Name, br.Structure)
		if br.Status != "" {
			fmt.Fprintf(&sb, "  LIVE SERIES STATE: %s\n", br.Status)
		}
	}
	return sb.String()
}

// bracketRaceLine names a heat's place in its bracket for prompts (caller
// holds b.mu): "this heat is Winners Round 2 — Race 3 of the Pro Spec bracket".
// pilotColour appends a short PUBLIC-profile note per pilot in the heat (from
// the server's booth-context pilots map) so the booth can personalise lineup
// intros — home town, flying-since, sponsors, drone build, bio. Only pilots with
// a public profile blurb are listed. Caller holds b.mu (like bracketRaceLine).
func (b *booth) pilotColour(race ddapi.Race) string {
	if b.brackets == nil || len(b.brackets.Pilots) == 0 {
		return ""
	}
	var lines []string
	for _, p := range race.Pilots {
		if blurb := b.brackets.Pilots[p.Name]; blurb != "" {
			lines = append(lines, "- "+p.Name+": "+blurb)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nPILOT NOTES (public profiles — weave in for colour where it fits, don't force every detail, never invent):\n" +
		strings.Join(lines, "\n")
}

// beatExtras builds the pilot-cards + Stewards' Room block for a beat. Cards go
// on the beats with room for them — the lineup, the between-heats chatter and
// the post-race wrap — because that's where a pronunciation hint or a greeting
// in a pilot's own language belongs; a live action call has no space for either.
// The gags ride on the FINISH beat only, once the heat's numbers are final.
// Caller holds b.mu.
func (b *booth) beatExtras(kind string, race *ddapi.Race) string {
	var cards []boothpkg.PilotCard
	if b.brackets != nil && len(b.brackets.Cards) > 0 && race != nil {
		inHeat := map[string]bool{}
		for _, p := range race.Pilots {
			inHeat[p.Name] = true
		}
		for _, c := range b.brackets.Cards {
			if inHeat[c.Callsign] {
				cards = append(cards, c)
			}
		}
	}
	var out string
	if kind == "staged" || kind == "finish" {
		out += boothpkg.PilotCardBlock(cards)
	}
	// Mid-heat calls rotate through a set of angles and carry each pilot's
	// form. Given identical data and brief a model converges on one shape, and
	// a listener hearing "X leads, Y is closing" on a loop knows at once they
	// are hearing a machine. Rotating deterministically guarantees the variety
	// that asking for it does not.
	if kind == "progress" && race != nil {
		out += boothpkg.LapShapes(*race)
		out += boothpkg.ProgressAngle(b.progressAngle)
		b.progressAngle++
	}
	// The Stewards' Room. Always on the finish call; on the in-heat action
	// calls too when the club opted in (b.trigInHeat).
	if race != nil && boothpkg.TriggerBeat(kind, b.trigInHeat) {
		opts := boothpkg.TriggerOpts{Live: kind != "finish"}
		noRoast := boothpkg.NoRoastSet(cards)
		forms := make([]boothpkg.PilotForm, 0, len(race.Pilots))
		for _, p := range race.Pilots {
			laps := p.Laps
			forms = append(forms, boothpkg.PilotForm{
				Callsign:   p.Name,
				Laps:       laps,
				Holeshot:   p.Holeshot,
				Position:   p.Position,
				FieldSize:  len(race.Pilots),
				BestLap:    p.BestLap,
				BestConsec: p.BestConsec,
				PriorPB:    p.PBConsec,
				TypicalLap: boothpkg.MedianLap(laps),
				NoRoast:    noRoast[p.Name],
			})
		}
		trigs := boothpkg.DetectTriggersOpt(
			forms, boothpkg.HeatIsTimeTrial(*race), race.ConsecWindow, b.triggers, opts)
		out += boothpkg.TriggerBlockOpt(b.freshTriggers(race.ID, trigs), opts)
	}
	return out
}

// freshTriggers drops the bits already offered during this heat and records the
// rest, so each gag is handed to the model at most once per heat however many
// in-heat calls detect it. Caller holds b.mu.
func (b *booth) freshTriggers(raceID string, ts []boothpkg.Trigger) []boothpkg.Trigger {
	out := ts[:0:0]
	for _, t := range ts {
		key := raceID + "\x00" + t.Key + "\x00" + t.Pilot
		if b.trigSaid[key] {
			continue
		}
		b.trigSaid[key] = true
		out = append(out, t)
	}
	return out
}

func (b *booth) bracketRaceLine(raceSrc string) string {
	if b.brackets == nil {
		return ""
	}
	ref, ok := b.brackets.Races[raceSrc]
	if !ok {
		return ""
	}
	line := fmt.Sprintf("\nBRACKET CONTEXT: this heat is %q in the %s bracket", ref.Title, ref.Bracket)
	if ref.Main != "" {
		line += fmt.Sprintf(" (%s Main)", ref.Main)
	}
	if ref.Note != "" {
		// Heat-specific semantics outrank the generic advancement line — a
		// Chase-the-Ace heat is a series heat, never a bracket round.
		return line + ".\n" + ref.Note + "\n"
	}
	line += " — winning and losing here moves pilots through that structure; call what the result means.\n"
	// Advance is the concrete wiring (which positions go to which heat and who
	// they meet there); Form is the seeding/qualifying colour for this heat.
	return line + ref.Advance + ref.Form
}

// queueLocked adds a beat (caller holds b.mu): higher priority evicts queued
// lower ones; the queue never exceeds boothQueueCap. Beats the admin has
// switched off on the website are dropped here — one gate for every source.
func (b *booth) queueLocked(beat boothBeat) {
	if !b.beatEnabled(beat.kind) {
		return
	}
	kept := b.beats[:0]
	for _, q := range b.beats {
		if q.priority >= beat.priority || len(kept) < boothQueueCap-1 {
			kept = append(kept, q)
		}
	}
	b.beats = append(kept, beat)
	sort.SliceStable(b.beats, func(i, j int) bool { return b.beats[i].priority > b.beats[j].priority })
	if len(b.beats) > boothQueueCap {
		b.beats = b.beats[:boothQueueCap]
	}
	select {
	case b.beatWake <- struct{}{}:
	default:
	}
}

func (b *booth) popBeat() (boothBeat, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.beats) == 0 {
		return boothBeat{}, false
	}
	beat := b.beats[0]
	b.beats = b.beats[1:]
	return beat, true
}

// run is the generation loop: one beat at a time — script, TTS, on air.
func (b *booth) run(ctx context.Context) {
	go b.mixer.run(ctx)
	b.mu.Lock()
	b.runCtx = ctx
	if b.obs != nil { // flag-configured OBS (local dev / initial state)
		octx, cancel := context.WithCancel(ctx)
		b.obsCancel = cancel
		go b.obs.run(octx) // drive OBS video delay for broadcast sync
	}
	b.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.beatWake:
		case <-time.After(2 * time.Second):
		}
		for {
			// Race calls own the machine as well as the bus.
			//
			// Generation is the contention: writing a beat runs the script model
			// flat-out on the GPU the voice also needs, and a starved streaming
			// call holds the bus on silence. So while a race call is on air or
			// queued, no beat STARTS generating — the beat waits its turn (the
			// outer loop retries within two seconds), the results render at full
			// speed, and the wrap follows them instead of strangling them.
			if b.mixer != nil && b.mixer.raceCallBusy() {
				break
			}
			beat, ok := b.popBeat()
			if !ok {
				break
			}
			// Beats generate CONCURRENTLY so a slow script doesn't hold up the
			// next call: while one beat is rendering audio, the next is already
			// being written. seq is stamped here, in queue order, because a
			// later beat can finish first — the mixer airs by seq, not by
			// whichever generation happened to return first.
			b.mu.Lock()
			b.beatSeq++
			seq := b.beatSeq
			b.producing++
			b.mu.Unlock()

			// Bounded: unbounded goroutines would stack API calls into a rate
			// limit (the free Gemini tier is 3 RPM) and spend tokens on calls
			// the meeting has already moved past.
			b.genSlots <- struct{}{}
			go func(beat boothBeat, seq int64) {
				defer func() {
					<-b.genSlots
					b.mu.Lock()
					b.producing--
					b.mu.Unlock()
				}()
				if err := b.speak(ctx, beat, seq); err != nil {
					log.Printf("booth: %s beat failed: %v", beat.kind, err)
				}
			}(beat, seq)
		}
	}
}

// startOBS (re)creates the OBS control goroutine with the given target,
// replacing any existing one. Caller must not hold b.mu.
func (b *booth) startOBS(url, password string, inject bool) {
	b.stopOBS()
	b.mu.Lock()
	base, audioURL := b.runCtx, b.audioURL
	b.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	octx, cancel := context.WithCancel(base)
	obs := newBoothOBS(url, password, nil, inject, audioURL)
	b.mu.Lock()
	b.obs, b.obsCancel = obs, cancel
	b.mu.Unlock()
	go obs.run(octx)
	obs.setBufferMs(b.sync.buffer()) // apply the current delay once it connects
	log.Printf("booth: broadcast sync — driving OBS at %s (audio inject=%v)", url, inject)
}

// stopOBS cancels the current OBS control goroutine, if any. Idempotent.
func (b *booth) stopOBS() {
	b.mu.Lock()
	cancel := b.obsCancel
	b.obs, b.obsCancel = nil, nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// speak generates and airs one beat: script lines from Gemini, multi-speaker
// TTS, then onto the mixer. The cue is journalled when tied to a race.
func (b *booth) speak(ctx context.Context, beat boothBeat, seq int64) error {
	b.mu.Lock()
	gem := b.gem
	speech := b.speech // nil = render with gem (Gemini TTS)
	script := b.script // nil = the main writer takes every beat

	// The main writer: a local model when the venue configured one, else
	// Gemini. Resolved once here so every generation path below agrees.
	var main scriptWriter = b.mainScript
	if main == nil && gem != nil {
		main = gem
	}
	solo := b.voiceMode == "solo"
	vPBP, vColor, style := b.voicePBP, b.voiceColor, b.voiceStyle
	if speech != nil && speech.Provider() == tts.ProviderQwen {
		vPBP, vColor = b.qwenVoicePBP, b.qwenVoiceColor
	}
	bstyle := b.style
	enabled := b.enabled
	racing := b.snap.HasRunningRace()
	short := b.shortCalls && boothpkg.ShortBeat(beat.kind)
	// The website picks the writer per tenant: "qwen" routes EVERY beat through
	// the fast DashScope model (its own quota, thinking off — the engine the
	// snappy calls already trusted), not just the frame-tied ones. Gemini stays
	// the fallback on any error, so a bad key degrades to the old behaviour
	// rather than to silence.
	allQwen := b.scriptBackend == "qwen"
	b.mu.Unlock()
	if !enabled {
		return nil // switched off while queued — drop silently
	}
	// A heat is already running: never START chatter. The render-time check
	// below catches a heat going green mid-generation; this one catches a beat
	// queued just before green and popped just after, and saves the ~20s and
	// the tokens of a generation that could only be thrown away.
	if boothChatterKind(beat.kind) && (racing || b.chatterClosed()) {
		return nil
	}

	// Racing, and commentary is sharing this machine's GPU with the race calls.
	// Yield the whole beat rather than make every lap call wait behind it.
	if racing && b.yieldsWhileRacing() {
		log.Printf("booth: holding the %s beat — a heat is running and the race calls have the GPU", beat.kind)
		return nil
	}
	if main == nil {
		return fmt.Errorf("no script writer yet (waiting for server booth config)")
	}
	if speech == nil && gem == nil {
		// Nothing can voice the lines. Worth its own message: a local writer
		// with no local voice is a plausible half-configuration, and "no gemini
		// client" would send someone looking for the wrong thing.
		return fmt.Errorf("no voice: a local script model needs a local voice too (-local-voice-python)")
	}
	// The fallback has to know WHICH engine is speaking.
	//
	// boothVoicePBP is a GEMINI voice ("Puck"). Filling it in unconditionally
	// meant a venue running Qwen with no Qwen voice set got Gemini's default
	// sent to Qwen, which rejects it outright:
	//   Voice 'Puck' is not supported
	// — so every beat failed and the booth was simply silent. ResolveVoice is
	// built for this: it degrades an unknown or foreign voice name to the
	// engine's own default, while passing a cloned voice id through by prefix.
	if speech != nil && speech.Provider() == tts.ProviderQwen {
		vPBP = qwentts.ResolveVoice(vPBP, qwentts.DefaultVoicePBP)
		vColor = qwentts.ResolveVoice(vColor, qwentts.DefaultVoiceColor)
	} else {
		if vPBP == "" {
			vPBP = boothVoicePBP
		}
		if vColor == "" {
			vColor = boothVoiceColor
		}
	}
	if speech == nil {
		speech = tts.FromGemini(gem)
	}
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	b.mu.Lock()
	startedGen := b.heatGen
	// Registered so the start sequence can abort this render outright — a
	// discarded RESULT still costs the GPU its full render time otherwise.
	b.renderSeq++
	cancelID := b.renderSeq
	if b.renderCancels == nil {
		b.renderCancels = map[int64]context.CancelFunc{}
	}
	b.renderCancels[cancelID] = cancel
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.renderCancels, cancelID)
		b.mu.Unlock()
	}()

	// Usage attribution: speak() runs serially, so the OnUsage hook can bill
	// everything between here and the TTS return to this race.
	b.usage.beginCue(beat.raceSrc)

	// The script phase is serialised even though beats generate concurrently.
	// beatPrompt READS the rolling transcript to tell the model what has just
	// been said; if two scripts were written at once they would both see the
	// same history and repeat each other. Holding this slot across
	// prompt-build → generate → record keeps "don't repeat yourself" true.
	//
	// It costs nothing that matters: the TTS render, which happens after the
	// slot is released, is what the next beat overlaps with.
	select {
	case b.scriptSlot <- struct{}{}:
	case <-sctx.Done():
		return sctx.Err()
	}
	scriptDone := false
	releaseScript := func() {
		if !scriptDone {
			scriptDone = true
			<-b.scriptSlot
		}
	}
	defer releaseScript()

	prompt := b.beatPrompt(beat, solo, bstyle)
	if short {
		prompt += boothpkg.BrevitySuffix
	}
	// Snappy beats are the live action calls that must land with the video, and
	// the script — not the voice — is what makes them late. The fast model has
	// thinking disabled, which is where the seconds actually come from. Other
	// beats (the finish wrap, between-heats analysis) are not frame-tied and
	// stay on Gemini, whose output was the more reliable about race state.
	var raw string
	var err error
	if (short || allQwen) && script != nil {
		raw, err = script.GenerateText(sctx, prompt)
		if err != nil {
			// A fast-path failure must not lose the call — fall back rather than
			// drop the beat.
			log.Printf("booth: snappy script (%s) failed (%v) — falling back to the main model", b.scriptNameSafe(), err)
			raw, err = main.GenerateText(sctx, prompt)
		}
	} else {
		raw, err = main.GenerateText(sctx, prompt)
	}
	if err != nil {
		return fmt.Errorf("script: %w", err)
	}
	lines := boothpkg.ParseLines(raw)
	if len(lines) == 0 {
		return fmt.Errorf("no dialogue lines in script response")
	}
	if short {
		lines = boothpkg.CapShort(lines)
	}
	text := strings.Join(lines, "\n")

	// Record what was said BEFORE releasing the script slot and before the
	// audio render — the next beat's prompt must see these lines, and it is
	// allowed to start as soon as the slot frees.
	b.mu.Lock()
	b.transcript = append(b.transcript, lines...)
	if len(b.transcript) > boothTranscript {
		b.transcript = b.transcript[len(b.transcript)-boothTranscript:]
	}
	b.mu.Unlock()
	releaseScript()

	var pcm []byte
	var rate int
	if solo {
		// Single commentator: strip the speaker tags so the name isn't read.
		pcm, rate, err = speech.Speak(sctx, tts.Request{
			Text:        sayable(strings.Join(boothpkg.StripSpeakerTags(lines), "\n")),
			Instruction: boothpkg.TTSInstructionStyled(beat.emotion, style, true, bstyle, beat.kind),
			Voice:       vPBP,
			Language:    bstyle.Language,
		})
	} else {
		pcm, rate, err = speech.Speak(sctx, tts.Request{
			Text:        sayable(text),
			Instruction: boothpkg.TTSInstructionStyled(beat.emotion, style, false, bstyle, beat.kind),
			Speakers: []tts.Speaker{
				{Speaker: boothSpeakerPBP, Voice: vPBP},
				{Speaker: boothSpeakerColor, Voice: vColor},
			},
			Language: bstyle.Language,
		})
	}
	if err != nil {
		return fmt.Errorf("tts: %w", err)
	}

	// The world may have moved on during generation: chatter that finished
	// rendering AFTER a heat went green is dropped, never aired — a race is on,
	// so the booth stays out of it until the heat is over.
	if boothChatterKind(beat.kind) && b.chatterClosed() {
		log.Printf("booth: dropping stale %s cue — the start sequence began while it was rendering", beat.kind)
		return nil
	}

	// A beat about a DIFFERENT heat than the one now running.
	//
	// The chatter test above is by KIND, and "finish" is not a chatter kind —
	// so a post-race wrap still generating when the next heat went green passed
	// straight through and talked over the start. Kind was the wrong question:
	// what makes a beat stale is that its race is over, whatever it is called.
	if beat.raceSrc != "" {
		if cur, ok := b.runningRaceID(); ok && cur != beat.raceSrc {
			log.Printf("booth: dropping stale %s cue — it is about a heat that has finished, and %s is under way",
				beat.kind, cur)
			return nil
		}
	}

	cue := &boothCue{
		RaceSrc: beat.raceSrc,
		Kind:    beat.kind,
		Text:    text,
		Emotion: beat.emotion,
		PCM:     pcm,
		Rate:    rate,
		EventMs: beat.eventMs,
		seq:     seq,
	}
	if rate > 0 {
		cue.DurationMs = int64(len(pcm)) * 1000 / int64(rate*2)
	}
	b.mu.Lock()
	if beat.raceSrc != "" {
		for _, r := range buildMeeting(b.snap).Races {
			if r.ID == beat.raceSrc {
				cue.RaceName = r.Label
			}
		}
	}
	b.lastSpoke = time.Now()
	b.mu.Unlock()

	// Broadcast sync: measure this call's real latency (event moment → audio
	// ready) and, when a matching video delay is in force, hold the cue until
	// event+buffer so it lands on the same frame the moment reaches the viewer.
	if beat.eventMs > 0 {
		lat := time.Now().UnixMilli() - beat.eventMs
		autoBuf := b.sync.observe(lat)
		// obs / manualDelayMs can be re-wired by the config poll, so read them
		// under the lock rather than racing the reconciler.
		b.mu.Lock()
		obs := b.obs
		manualDelay := b.manualDelayMs
		b.mu.Unlock()
		var buf int64
		switch {
		case obs != nil:
			buf = autoBuf
			obs.setBufferMs(buf) // drive OBS's video delay to match
		case manualDelay > 0:
			buf = manualDelay
			if autoBuf > buf {
				log.Printf("booth: generation latency %.1fs exceeds the manual video delay %.1fs — raise the delay to stay in sync",
					float64(autoBuf)/1000, float64(buf)/1000)
			}
		}
		if buf > 0 {
			cue.playAt = time.UnixMilli(beat.eventMs).Add(time.Duration(buf) * time.Millisecond)
			if lat > buf {
				b.sync.noteLate()
			}
		}
	}

	// LAST GATE before the bus, and deliberately absolute.
	//
	// Two ways a line can be wrong by the time it is ready:
	//
	//   1. the heat went green while it was being written or voiced, so it
	//      describes a moment the field has left;
	//   2. the operator has pressed Start, or a heat is running, and the line
	//      simply has no business on the bus at all.
	//
	// The generation check alone left case 2 open: a beat that STARTS being
	// made after the bump captures the new generation and passes. Both are
	// checked here, at the one point every line goes through, because the
	// requirement is that pressing Start throws away everything — not that it
	// throws away everything the earlier guards happened to catch.
	b.mu.Lock()
	stale := b.heatGen != startedGen
	closed := b.preStart || b.snap.HasRunningRace()
	b.mu.Unlock()
	if stale {
		log.Printf("booth: dropped a %s line — the heat went green while it was being made", beat.kind)
		return nil
	}
	if closed {
		log.Printf("booth: dropped a %s line — the start button has been pressed / a heat is running", beat.kind)
		return nil
	}

	// Logged on the way to the bus, WITH the state the gates just read.
	//
	// A staged line was heard airing one second after the start sequence, which
	// both gates above should have caught. Guessing which one failed is what
	// has made this take several attempts, so the values are recorded at the
	// moment of the decision rather than inferred from timestamps afterwards.
	log.Printf("booth: airing a %s line (%.1fs of audio) [preStart=%v running=%v gen=%d/%d]",
		beat.kind, float64(len(cue.PCM))/float64(boothSampleRate*2),
		closed, b.snapHasRunningRace(), startedGen, b.currentHeatGen())

	// Said plainly, because this is the failure that is impossible to hear.
	//
	// The cue is fine, the bus is fine, and there is simply nothing on the other
	// end — so the room is silent and every other line in the log looks healthy.
	// Without this you are left comparing "airing" lines against your own ears.
	if n := b.mixer.clientCount(); n == 0 {
		log.Printf("booth: NOTHING IS LISTENING — this %s line is being produced but will "+
			"not be heard. Open /booth.wav in a browser tab, add it to OBS as a Media "+
			"Source, or turn on 'play through this laptop's speakers' on the Commentary page.",
			beat.kind)
	}

	cue.commentary = true
	// Kept for the debug page BEFORE the bus takes it: the mixer fades and
	// truncates a cue's buffer in place when a call is cut short, so recording
	// it afterwards would capture the clipped version rather than what was made.
	b.tape.add(beat.kind, cue.Text, cue.PCM)
	b.mixer.enqueue(cue)
	return nil
}

// ---------------------------------------------------------------------------
// Prompt building
// ---------------------------------------------------------------------------

// beatPrompt assembles a beat's script request via the shared booth package
// (identical to the website's moment preview), adding the live transcript.
func (b *booth) beatPrompt(beat boothBeat, solo bool, st boothpkg.Style) string {
	b.mu.Lock()
	said := strings.Join(b.transcript, "\n")
	b.mu.Unlock()
	return boothpkg.PromptStyled(beat.kind, beat.context, said, solo, st, beat.extras)
}

// boothRaceProgressSig fingerprints the part of a heat a running-order call
// actually describes: who is where, and how many laps each has put in.
//
// It exists because the in-heat trigger is time-based (every boothInHeatGap)
// while laps land every ~20s. Without it, four consecutive calls describe an
// identical race state, and a model with nothing new to say repeats itself —
// the same lap times, almost word for word. Comparing this signature makes a
// call conditional on the race having actually moved.
//
// Deliberately coarse: lap COUNT and position, not lap times. A lap time never
// changes once recorded, so counting laps is enough to notice a new one, and
// leaving times out keeps the signature stable against float formatting.
func boothRaceProgressSig(r vmRace) string {
	var sb strings.Builder
	sb.WriteString(r.ID)
	for _, p := range r.Pilots {
		fmt.Fprintf(&sb, "|%s:%d:%d:%t", p.Name, len(p.Laps), p.Position, p.DNF)
	}
	return sb.String()
}

// snapHasRunningRace / currentHeatGen expose the gate inputs for logging, so a
// line that slips past them can be diagnosed from what they actually saw.
func (b *booth) snapHasRunningRace() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snap.HasRunningRace()
}

func (b *booth) currentHeatGen() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.heatGen
}

// boardMoveContext reports what the last heat changed on the standings.
//
// Reuses pilotBoardPos, which is already maintained for the qual_move beat, so
// the result wrap gets the same before-and-after without a second bookkeeping
// path. Advanced for EVERY pilot afterwards, not just the movers — otherwise a
// pilot who moved twice would be compared against a stale position.
func (b *booth) boardMoveContext(mv *ddapi.Meeting) string {
	if mv == nil || len(mv.Board) == 0 {
		return ""
	}
	line := ""
	if b.boardSeeded {
		line = boardMoveLine(boardMovesSince(b.pilotBoardPos, mv.Board))
	}
	// First sight of a board is not movement — it is the starting state, and
	// announcing it as change would say everyone climbed from nowhere.
	for _, st := range mv.Board {
		b.pilotBoardPos[st.Name] = st.Pos
	}
	b.boardSeeded = true
	return line
}
