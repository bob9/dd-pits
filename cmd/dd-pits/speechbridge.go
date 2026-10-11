package main

// Deciding which calls dd-pits makes ITSELF and which it simply relays.
//
// Two sources feed one voice, and they must not both say the same thing:
//
//   - DERIVED (racecalls.go) — the calls dd-pits builds from the timing data.
//     These are the ones worth owning, because owning them is what allows
//     anything FPVTrackside cannot do: a lap time that also mentions a personal
//     best, a call that knows where the pilot sits on the meeting board, wording
//     that changes with the format.
//
//   - RELAYED (speechlog.go) — everything else FPVTrackside says, taken from its
//     log. This exists for the calls no data can reveal: "Hurry Up" is bound to
//     Ctrl+H, a decision the race director makes, and no amount of polling will
//     ever show it.
//
// So the rule is: derived where we have something better to say, relayed where
// we do not.
//
// Matching the two by their TEXT is the obvious approach and the wrong one. The
// entire reason for deriving a call is to say something FPVTrackside would not —
// the moment dd-pits adds "and that's a new personal best", the words stop
// matching, and the lap is announced twice in two different wordings. That is a
// bug that appears exactly when the feature starts being useful.
//
// So the match is on WHICH SOUND a line is, recovered by comparing it against
// the literal parts of FPVTrackside's own templates. "Lap 3 bob9 2.41" is
// recognisably SoundRaceLap whatever dd-pits chose to say about it.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// spokenBridge decides whether a line from the log still needs speaking.
type spokenBridge struct {
	// owned are the sounds dd-pits derives itself.
	owned map[SoundKey]bool

	// matchers recognise a rendered line as one of the sounds.
	matchers []soundMatcher

	// enabled records which sounds the venue actually has switched on, so a
	// superseding call that is switched off does not silence the one it would
	// have replaced.
	enabled map[SoundKey]bool

	mu sync.Mutex

	// finished tracks who has already been announced as finishing this heat.
	finished map[string]bool

	// said is what dd-pits has recently spoken, as figure signatures.
	//
	// The second net. Template ownership is the primary mechanism and covers
	// nearly everything, but two of FPVTrackside's templates are LITERALLY
	// identical — CasualLap and TimeTrialTargetLaps are both
	// "{pilot} {count} lap{s} in {lapstime}" — and one is derived while the
	// other is not. No amount of text matching separates them, so a second,
	// different signal is needed for the lines that land ambiguously.
	said map[string]time.Time
}

// saidWindow is how long a derived call suppresses its logged twin.
//
// The two paths observe the same lap moments apart — dd-pits from the timing
// data, FPVTrackside from its own event — so this must comfortably cover that
// gap while staying short enough that a genuinely repeated call later in the
// meeting is still spoken.
const saidWindow = 20 * time.Second

type soundMatcher struct {
	Key SoundKey
	RE  *regexp.Regexp

	// literal is how much fixed text the template carries, used to prefer the
	// most specific match. "Lap {lap} {pilot} {time}" and "{pilot} {time}" can
	// both match a line; the one with more of its own words is the better read.
	literal int
}

// derivedSounds are the calls racecalls.go builds from the timing data.
//
// This list is the contract between the two paths: a sound named here is
// dd-pits's to say (and to improve), and its logged counterpart is dropped.
// Everything absent is relayed as FPVTrackside worded it.
//
// Getting it wrong is asymmetric, and worth being deliberate about. Naming a
// sound dd-pits does NOT derive drops the logged call and puts nothing in its
// place — a call that simply never happens, which is far harder to notice than a
// duplicate and much worse on race day. So a sound belongs here only once
// racecalls.go genuinely emits it; TestDerivedSoundsMatchesTheCaller holds the
// two together.
var derivedSounds = []SoundKey{
	SoundRaceLap,
	SoundTimeTrialEveryLap,
	SoundRaceDone,
	SoundRaceStart,
	SoundRaceOver,
	SoundNewLapRecord,
}

// supersededSounds are relayed calls that a DERIVED call already covers.
//
// The bridge's usual test is "is this the same sound?", which misses the case
// where FPVTrackside and dd-pits describe the same moment with DIFFERENT
// sounds. On a time trial every crossing produced two calls: dd-pits derives
// TimeTrialEveryLap ("Fizz 15.08") and FPVTrackside announces
// TimeTrialTargetLaps ("Fizz 4 laps in 61.20") for the same lap.
//
// Both are true, and together they are twice the audio the bus can carry —
// measured, calls queued from 151ms to 4384ms and were then dropped as stale.
// The derived one wins because it is the one dd-pits can improve.
var supersededSounds = map[SoundKey]SoundKey{
	SoundTimeTrialTargetLaps: SoundTimeTrialEveryLap,
}

// oncePerPilot are calls that describe a pilot FINISHING, which happens once.
//
// FPVTrackside re-announces them on every crossing after a pilot passes the
// target laps, so a time trial where pilots keep flying produces a stream of
// "bob9 finished 4 laps in …, Please land" — measured at eleven in ninety
// seconds, which saturated the bus and pushed real lap calls past their
// freshness window until they were dropped.
//
// Suppressed per pilot per heat rather than globally: the same pilot finishing
// the NEXT heat is a new call.
var oncePerPilot = map[SoundKey]bool{
	SoundTimeTrialDone: true,
	SoundRaceDone:      true,
}

func newSpokenBridge(sounds *SoundSet) *spokenBridge {
	b := &spokenBridge{
		owned:    map[SoundKey]bool{},
		enabled:  map[SoundKey]bool{},
		finished: map[string]bool{},
		said:     map[string]time.Time{},
	}
	for _, k := range derivedSounds {
		b.owned[k] = true
	}

	if sounds == nil {
		return b
	}

	// Build a matcher from every template, not just the owned ones. Knowing a
	// line is SoundHurryUp is as useful as knowing it is SoundRaceLap — the
	// first is relayed, the second dropped, and both decisions need the line
	// identified first.
	for _, key := range sounds.Keys() {
		text, ok := sounds.Template(key)
		if !ok {
			continue
		}
		b.enabled[key] = true
		if m, ok := buildSoundMatcher(key, text); ok {
			b.matchers = append(b.matchers, m)
		}
	}

	// Most specific first, so a template that is nearly all placeholders never
	// steals a line from one that spells the call out.
	sort.SliceStable(b.matchers, func(i, j int) bool {
		return b.matchers[i].literal > b.matchers[j].literal
	})

	return b
}

// Identify reports which sound a logged line is, if it can tell.
func (b *spokenBridge) Identify(text string) (SoundKey, bool) {
	keys, _ := b.identifyAll(text)
	if len(keys) == 0 {
		return "", false
	}
	return keys[0], true
}

// identifyAll returns every equally-specific candidate, and whether they agree
// on ownership.
//
// Returning the tie rather than picking from it is deliberate: an arbitrary
// choice between two identical templates decides, silently and by map order,
// whether a call is spoken twice or not at all.
func (b *spokenBridge) identifyAll(text string) (keys []SoundKey, agree bool) {
	line := normaliseSpoken(text)
	if line == "" {
		return nil, true
	}

	best := -1
	for _, m := range b.matchers {
		if !m.RE.MatchString(line) {
			continue
		}
		if best == -1 {
			best = m.literal
		}
		if m.literal < best {
			break // sorted most-specific first; the rest are worse
		}
		keys = append(keys, m.Key)
	}

	if len(keys) == 0 {
		return nil, true
	}

	agree = true
	for _, k := range keys[1:] {
		if b.owned[k] != b.owned[keys[0]] {
			agree = false
			break
		}
	}
	return keys, agree
}

// ShouldRelay reports whether a logged line still needs speaking.
//
// An UNRECOGNISED line is relayed. That is the safe direction: a sound dd-pits
// does not model going unsaid is a silent race day, whereas the worst case of
// relaying something it also derives is hearing a lap twice — annoying, and
// obvious enough to fix.
func (b *spokenBridge) ShouldRelay(text string) bool {
	keys, agree := b.identifyAll(text)

	// A finishing call for a pilot who has already been announced as finished.
	if len(keys) > 0 && oncePerPilot[keys[0]] {
		who := firstWord(text)
		mark := string(keys[0]) + "\x00" + who
		b.mu.Lock()
		seen := b.finished[mark]
		b.finished[mark] = true
		b.mu.Unlock()
		if seen {
			return false
		}
	}

	// A relayed call that a derived one already covers. Dropped only when
	// dd-pits is actually making the call that supersedes it — if that sound is
	// switched off, this one is all there is and must still be heard.
	if len(keys) > 0 {
		if by, ok := supersededSounds[keys[0]]; ok && b.owned[by] && b.enabled[by] {
			return false
		}
	}

	// Recognised as a sound dd-pits derives, with no disagreement among equally
	// specific candidates. The normal path, and the only one that drops a line
	// outright.
	if len(keys) > 0 && agree && b.owned[keys[0]] {
		return false
	}

	// Everything else — unrecognised, unowned, or an ambiguous tie between an
	// owned sound and an unowned one — falls through to asking whether dd-pits
	// has in fact just said this.
	//
	// Applied universally rather than only to ties, because it answers directly
	// what the template match only ever approximated. It is also the net that
	// catches derivedSounds drifting out of step with racecalls.go: a call the
	// caller emits without declaring is still suppressed here, so the mistake
	// costs a stale declaration rather than a duplicate on air.
	return !b.recentlySaid(text)
}

// NewHeat clears the per-heat state. A pilot finishing the next heat is a new
// call, not a repeat of the last one.
func (b *spokenBridge) NewHeat() {
	b.mu.Lock()
	b.finished = map[string]bool{}
	b.mu.Unlock()
}

// firstWord is the pilot's name in every finishing template FPVTrackside ships
// ("{pilot} finished …"), which is enough to tell one pilot's call from another.
func firstWord(s string) string {
	f := strings.Fields(normaliseSpoken(s))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// NoteSpoken records a call dd-pits made itself.
func (b *spokenBridge) NoteSpoken(text string) {
	sig := figureSignature(text)
	if sig == "" {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.said[sig] = time.Now()

	if len(b.said) > 256 {
		cutoff := time.Now().Add(-saidWindow)
		for k, at := range b.said {
			if at.Before(cutoff) {
				delete(b.said, k)
			}
		}
	}
}

func (b *spokenBridge) recentlySaid(text string) bool {
	sig := figureSignature(text)
	if sig == "" {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	at, ok := b.said[sig]
	return ok && time.Since(at) <= saidWindow
}

// numberRE picks the figures out of a call.
var numberRE = regexp.MustCompile(`\d+(?:\.\d+)?`)

// figureSignature reduces a call to the numbers in it.
//
// Comparing full text would defeat the purpose: dd-pits derives a call precisely
// so it can add words FPVTrackside would not, and the added words are exactly
// what would break an equality check. The FIGURES are what both versions must
// agree on — the same lap, the same time, the same position — so they identify
// the underlying event while leaving the wording free to differ.
// The figures are compared as NUMBERS, not as text.
//
// dd-pits formats a lap time to two decimals and FPVTrackside drops a trailing
// zero, so the same 3.3 second lap is written "3.30" by one and "3.3" by the
// other. A string comparison calls those different events, and the duplicate
// goes to air: heard as dd-pits announcing the same lap twice, once derived and
// once relayed. Worse than the repetition, the relayed copy costs a full
// generation and the next real lap call queues behind it — measured at 185ms
// for the derived call, 2800ms for its duplicate, and 3533ms for the lap after.
func figureSignature(text string) string {
	// The FIRST WORD rides along with the figures.
	//
	// Figures alone are not specific enough for a call whose only number is a
	// channel: "Fizz on R2" and "Willman on F2" both reduce to "2", so the
	// second was suppressed as a repeat of the first and a pilot never had his
	// channel announced. The pilot's name is what distinguishes them, and it is
	// the first word of every such template.
	//
	// It stays out of the way of the case this exists for — dd-pits and
	// FPVTrackside wording the SAME event differently — because both versions
	// of a call name the same pilot first.
	nums := numberRE.FindAllString(text, -1)
	if len(nums) == 0 {
		return ""
	}
	who := firstWord(text)
	canon := make([]string, 0, len(nums))
	for _, n := range nums {
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			canon = append(canon, n)
			continue
		}
		// Shortest form that round-trips: 3.30 and 3.3 both become "3.3",
		// 3.00 and 3 both become "3".
		canon = append(canon, strconv.FormatFloat(f, 'f', -1, 64))
	}
	return who + "\x00" + strings.Join(canon, "|")
}

// buildSoundMatcher turns a template into a pattern that recognises what
// FPVTrackside renders from it.
//
// The literal words are what survive rendering, so they are what identifies the
// call; each placeholder becomes a permissive gap. A placeholder can render
// EMPTY — an optional bracket name, a missing lap count — so the gap must be
// allowed to match nothing, which is why it is `.*` rather than `.+`.
func buildSoundMatcher(key SoundKey, template string) (soundMatcher, bool) {
	tmpl := normaliseSpoken(template)
	if tmpl == "" {
		return soundMatcher{}, false
	}

	var pattern strings.Builder
	pattern.WriteString(`^\s*`)

	literal := 0
	rest := tmpl
	for {
		loc := placeholderRE.FindStringIndex(rest)
		if loc == nil {
			break
		}
		lit := rest[:loc[0]]
		literal += len(strings.TrimSpace(lit))
		pattern.WriteString(regexp.QuoteMeta(strings.TrimSpace(lit)))
		pattern.WriteString(`\s*.*?\s*`)
		rest = rest[loc[1]:]
	}
	literal += len(strings.TrimSpace(rest))
	pattern.WriteString(regexp.QuoteMeta(strings.TrimSpace(rest)))
	pattern.WriteString(`\s*$`)

	// A template of nothing but placeholders matches every line, so it can only
	// mis-identify. Better to leave those lines unrecognised and relayed.
	if literal == 0 {
		return soundMatcher{}, false
	}

	re, err := regexp.Compile(`(?i)` + pattern.String())
	if err != nil {
		return soundMatcher{}, false
	}
	return soundMatcher{Key: key, RE: re, literal: literal}, true
}

// normaliseSpoken flattens the differences between a template and its rendered
// form that carry no meaning — spacing, quoting, trailing punctuation left by an
// empty field.
func normaliseSpoken(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{"'", "‘", "’", "\""} {
		s = strings.ReplaceAll(s, q, "")
	}
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, " .,;")
}
