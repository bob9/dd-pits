package main

// Pronunciations: how the voice SAYS a name, without changing what anything
// else sees.
//
// Callsigns are made up, and a TTS model can only guess — "Bola" came out
// wrong on air, and this local-only setup has no reach into the website's
// per-pilot pronunciation cards (those ride the cloud booth context). So the
// local agent carries its own small map, edited on the Voices page as one
// rule per line:
//
//	Bola = Boh-lah
//	9dtr = niner detter
//
// Applied ONLY at the last moment before text reaches a TTS engine, via
// sayable(). Everything upstream — call text, dedup signatures, render
// caches, the booth transcript, journals — keeps the pilot's real name, so a
// rule changes the sound and nothing else. Rules are held in a package-level
// atomic and swapped whole at save time: a pronunciation edit takes effect on
// the next call, no restart, and readers never lock.

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// pronRule says one name a particular way. The pattern is whole-word and
// case-insensitive, so "Bola" matches at a sentence start and inside "Bola
// finished in 1st" but never inside another pilot's "Bolander".
type pronRule struct {
	re  *regexp.Regexp
	say string
}

var pronRules atomic.Pointer[[]pronRule]

// parsePronunciations turns the textarea's lines into rules.
//
// One rule per line, "name = how to say it". Blank lines and lines without a
// "=" are skipped rather than errors — this is venue configuration typed at a
// race, not a file format to validate.
func parsePronunciations(text string) []pronRule {
	var rules []pronRule
	for line := range strings.SplitSeq(text, "\n") {
		name, say, ok := strings.Cut(line, "=")
		name, say = strings.TrimSpace(name), strings.TrimSpace(say)
		if !ok || name == "" || say == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
		if err != nil {
			continue
		}
		rules = append(rules, pronRule{re: re, say: say})
	}
	return rules
}

// setPronunciations swaps the live rule set. Called at startup and on every
// Voices-page save, so an edit reaches the very next call.
func setPronunciations(text string) {
	rules := parsePronunciations(text)
	pronRules.Store(&rules)
}

// sayable rewrites text the way the voice should SAY it. Applied only at the
// TTS boundary — the returned string must never be logged, cached, deduped or
// journalled in place of the original.
func sayable(text string) string {
	p := pronRules.Load()
	if p == nil || len(*p) == 0 {
		return text
	}
	for _, r := range *p {
		text = r.re.ReplaceAllString(text, r.say)
	}
	return text
}
