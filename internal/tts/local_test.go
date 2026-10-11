package tts

import "testing"

// An unpunctuated line gets a terminal full stop; a punctuated one is left be.
//
// The cloned voice clips the final word of unpunctuated lines — "in first"
// aired as "in firs" — because a short reference recording that stops hard
// teaches the model to stop hard, and a bare figure gives it no marker for
// where the utterance ends. Every race-call template ends in a figure.
func TestCloseSentence(t *testing.T) {
	cases := map[string]string{
		"Willman lap 3 in 10.93 in 1st": "Willman lap 3 in 10.93 in 1st.",
		"Race over.":                    "Race over.",
		"Holeshot bob9 3.16!":           "Holeshot bob9 3.16!",
		"who takes it?":                 "who takes it?",
		"":                              "",
	}
	for in, want := range cases {
		if got := closeSentence(in); got != want {
			t.Errorf("closeSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
