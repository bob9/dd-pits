package main

// Booth state shared with the setup pages: per-beat toggles, bracket context
// for the commentary prompts, and token usage metering for the spend page.

import (
	"sync"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/qwentts"
)

// boothBracketInfo / boothRaceRef describe the meeting's bracket structure.
type boothBracketInfo struct {
	Name      string `json:"name"`
	Format    string `json:"format"`
	Structure string `json:"structure"`
	Status    string `json:"status"` // live series state (e.g. CTA score)
}

type boothRaceRef struct {
	Title   string `json:"title"`
	Bracket string `json:"bracket"`
	Main    string `json:"main"`
	Note    string `json:"note"` // heat-specific semantics (e.g. CTA series heat)
	// Advance spells out what this heat's result decides: which finishing
	// positions go to which next heat (or are eliminated), and that heat's
	// line-up — pilots already locked in by name, undecided seats named as
	// "the 1st of Winners Round 1 — Race 3". Empty when the format has no
	// bracket wiring to describe (or an older server didn't send it).
	Advance string `json:"advance"`
	// Form is the heat's qualifying colour: each pilot's bracket seed and where
	// they qualified — a talking point between the action calls, never the call.
	Form string `json:"form"`
}

type boothBracketContext struct {
	Brackets []boothBracketInfo      `json:"brackets"`
	Races    map[string]boothRaceRef `json:"races"`
	// Pilots maps a callsign → a short PUBLIC-profile colour blurb (location,
	// flying-since, sponsors, bio, drone build) the booth can drop into lineup
	// intros. Server-built from pilot_profiles (is_public only).
	Pilots map[string]string `json:"pilots"`
	// Cards are the pilots' own commentary preferences (pronunciation hint,
	// language flourish, note, don't-rib-me opt-out). Absent on an older
	// server, which simply means nobody has set one.
	Cards []boothpkg.PilotCard `json:"cards"`
}

// boothUsage accumulates token counters per (race, kind) between reports.
// speak() runs serially, so cue attribution is a simple current-slot.
type boothUsage struct {
	mu      sync.Mutex
	curRace string
	pending map[string]*boothUsageEntry // key raceSrc+kind
	// session totals for the /booth page
	TextIn, TextOut, TTSIn, TTSOut int64
	// QwenTTSChars is Qwen speech in CHARACTERS — a different unit from the
	// token counters above, so it is priced separately.
	QwenTTSChars int64
	Requests     int64
}

type boothUsageEntry struct {
	EventSource  string `json:"event_source"`
	RaceSource   string `json:"race_source"`
	Kind         string `json:"kind"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Requests     int    `json:"requests"`
}

func (u *boothUsage) beginCue(raceSrc string) {
	u.mu.Lock()
	u.curRace = raceSrc
	u.mu.Unlock()
}

// add is the gemini.Client.OnUsage hook.
func (u *boothUsage) add(kind string, in, out int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pending == nil {
		u.pending = map[string]*boothUsageEntry{}
	}
	key := u.curRace + "\x00" + kind
	e := u.pending[key]
	if e == nil {
		e = &boothUsageEntry{RaceSource: u.curRace, Kind: kind}
		u.pending[key] = e
	}
	e.InputTokens += int64(in)
	e.OutputTokens += int64(out)
	e.Requests++
	u.Requests++
	switch kind {
	case "text":
		u.TextIn += int64(in)
		u.TextOut += int64(out)
	case "tts":
		u.TTSIn += int64(in)
		u.TTSOut += int64(out)
	case qwentts.UsageKindTTS:
		// Characters, not tokens — kept apart so estimateUSD can price it.
		u.QwenTTSChars += int64(in)
	}
}

// drain returns and clears the pending entries, stamping the event source.
func (u *boothUsage) drain(eventSource string) []boothUsageEntry {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.pending) == 0 {
		return nil
	}
	out := make([]boothUsageEntry, 0, len(u.pending))
	for _, e := range u.pending {
		e.EventSource = eventSource
		out = append(out, *e)
	}
	u.pending = map[string]*boothUsageEntry{}
	return out
}

// estimateUSD mirrors the server's pricing constants (approximate).
func (u *boothUsage) estimateUSD() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	// The trailing term is Qwen speech: per 1M CHARACTERS, not tokens.
	return (float64(u.TextIn)*0.30 + float64(u.TextOut)*2.50 +
		float64(u.TTSIn)*0.50 + float64(u.TTSOut)*10.00 +
		float64(u.QwenTTSChars)*27.59) / 1e6
}

// beatEnabled consults the server-driven per-beat toggles (default on) AND the
// race-director phase toggle for the beat's phase. Caller holds b.mu.
func (b *booth) beatEnabled(kind string) bool {
	// Phase first, and it applies whether or not individual beats were ever
	// configured. This used to return early on a nil beat map, so a venue that
	// switched a whole PHASE off but never ticked a single beat kept getting
	// the lot — the phase toggle looked broken because it was.
	switch boothpkg.BeatPhase(kind) {
	case "pre":
		if !b.phasePre {
			return false
		}
	case "in":
		if !b.phaseIn {
			return false
		}
	case "post":
		if !b.phasePost {
			return false
		}
	}
	if b.beatCfg == nil {
		return true // phase allowed it and no per-beat config exists
	}
	on, known := b.beatCfg[kind]
	return !known || on
}
