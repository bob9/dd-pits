package main

import "testing"

// Pronunciation rules change how a name SOUNDS and nothing else — applied
// whole-word and case-insensitively at the TTS boundary only.
func TestPronunciationsRewriteNamesWholeWord(t *testing.T) {
	setPronunciations("Bola = Boh-lah\n9dtr = niner detter\n\njunk line without equals\n = nothing\n")
	defer setPronunciations("")

	if got := sayable("Bola finished in 1st"); got != "Boh-lah finished in 1st" {
		t.Fatalf("plain rewrite failed: %q", got)
	}
	// Case-insensitive: the writer may lowercase a callsign mid-sentence.
	if got := sayable("and bola takes the top spot"); got != "and Boh-lah takes the top spot" {
		t.Fatalf("case-insensitive rewrite failed: %q", got)
	}
	// Whole word only: another pilot whose name merely CONTAINS the rule's
	// name must never be renamed.
	if got := sayable("Bolander on R2"); got != "Bolander on R2" {
		t.Fatalf("substring must not match: %q", got)
	}
	// Digit-heavy callsigns work too.
	if got := sayable("9dtr 1.97"); got != "niner detter 1.97" {
		t.Fatalf("digit callsign rewrite failed: %q", got)
	}
	// No rules = untouched text, zero cost.
	setPronunciations("")
	if got := sayable("Bola finished in 1st"); got != "Bola finished in 1st" {
		t.Fatalf("empty rules must leave text alone: %q", got)
	}
}
