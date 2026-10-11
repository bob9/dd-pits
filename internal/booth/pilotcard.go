package booth

// PER-PILOT COMMENTARY CARDS.
//
// The booth already gets each pilot's public profile blurb (home town, years
// flying, sponsors, build) so intros are personal. A card is the bit the pilot
// writes FOR the commentators rather than for the directory:
//
//   - PRONUNCIATION. The single highest-value field here and the least
//     glamorous. A mangled callsign is the fastest way to break the illusion
//     that anyone is actually watching the race.
//   - LANGUAGE FLOURISH. A pilot with another language in their background can
//     have the booth greet and sign them off in it. Deliberately capped at a
//     tagline in the build-up and the wrap: mixing languages mid-sentence
//     muddies the TTS, and a mid-race call has to be understood instantly by
//     everyone watching.
//   - NOTE. One line of running-joke context the booth can call back to.
//   - NOROAST. The opt-out. Club racing is a small room; some pilots love being
//     ribbed and some really don't, and that switch needs to exist before
//     someone has a bad day, not after.

import (
	"fmt"
	"strings"
)

// PilotCard is one pilot's commentary preferences. The zero value is a pilot
// who has set nothing, and renders no prompt text at all.
type PilotCard struct {
	Callsign string
	// Pronounce is a plain-English "say it like this" hint, e.g. "KLOG-vog".
	Pronounce string
	// Language is the pilot's flourish language, e.g. "Dutch". Blank = English.
	Language string
	// Accent is an optional regional variety of Language ("Brabants",
	// "Flemish") — how the pilot actually speaks it. Ignored without a Language.
	Accent string
	// Note is one line of colour for the booth.
	Note string
	// NoRoast opts the pilot out of all ribbing (see humourGuardrail).
	NoRoast bool
}

// Empty reports whether the card carries nothing worth putting in a prompt.
func (c PilotCard) Empty() bool {
	return strings.TrimSpace(c.Pronounce) == "" && strings.TrimSpace(c.Language) == "" &&
		strings.TrimSpace(c.Note) == "" && !c.NoRoast
}

// langLabel is the pilot's language with their regional variety folded in, e.g.
// "Dutch (Brabants)". An accent without a language means nothing, so it's
// dropped there.
func (c PilotCard) langLabel() string {
	lang := clampField(c.Language)
	if lang == "" {
		return ""
	}
	if a := SanitiseAccent(c.Accent); a != "" {
		return lang + " (" + a + ")"
	}
	return lang
}

// maxCardField bounds each free-text field before it reaches a prompt. These
// are pilot-authored and go straight into an LLM instruction, so they're capped
// hard — a card is a hint, not a soapbox, and not a place to smuggle in
// instructions to the model.
const maxCardField = 120

func clampField(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > maxCardField {
		s = strings.TrimSpace(s[:maxCardField])
	}
	return s
}

// PilotCardBlock renders the cards for the pilots in a heat. Empty when nobody
// has set anything, so an ordinary heat's prompt is untouched.
func PilotCardBlock(cards []PilotCard) string {
	var lines []string
	langs := map[string][]string{}
	for _, c := range cards {
		if c.Empty() {
			continue
		}
		name := clampField(c.Callsign)
		if name == "" {
			continue
		}
		var bits []string
		if p := clampField(c.Pronounce); p != "" {
			bits = append(bits, fmt.Sprintf("say it %q", p))
		}
		if n := clampField(c.Note); n != "" {
			bits = append(bits, n)
		}
		if c.NoRoast {
			bits = append(bits, "PREFERS NOT TO BE RIBBED — call them straight, never a joke at their expense")
		}
		if l := c.langLabel(); l != "" {
			bits = append(bits, "speaks "+l)
			langs[l] = append(langs[l], name)
		}
		if len(bits) > 0 {
			lines = append(lines, "- "+name+": "+strings.Join(bits, "; "))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nPILOT CARDS (what these pilots want from the booth):\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	for lang, names := range langs {
		fmt.Fprintf(&b, "For %s: drop ONE short %s phrase — a greeting on the way in or a sign-off on the way "+
			"out — then straight back to English. Build-up and wrap only, never mid-race, never a whole sentence "+
			"of the call.\n", strings.Join(names, " and "), lang)
	}
	return b.String()
}

// NoRoastSet is the callsign set that must be excluded from ribbing, used to
// filter Stewards' Room triggers before they reach the prompt.
func NoRoastSet(cards []PilotCard) map[string]bool {
	out := map[string]bool{}
	for _, c := range cards {
		if c.NoRoast && strings.TrimSpace(c.Callsign) != "" {
			out[c.Callsign] = true
		}
	}
	return out
}
