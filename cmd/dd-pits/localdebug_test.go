package main

import (
	"strings"
	"testing"
)

// The debug log exists to be COPIED AND SENT to someone else, so a credential
// reaching a log line would be handed over with it. Nothing logs a key on
// purpose, but errors quote what they were given — a rejected request carries
// its own URL — so the shapes are stripped wherever they appear rather than
// trusting every present and future log call to be careful.
func TestRedactSecretsStripsCredentials(t *testing.T) {
	cases := []struct {
		name, in, mustNotContain string
	}{
		{
			"qwen / openai style key",
			`booth: filler beat failed: qwen-tts: auth with sk-abc123DEF456ghi789 rejected`,
			"sk-abc123DEF456ghi789",
		},
		{
			"gemini key",
			`booth: request failed: key AIzaSyC7xNq0pLm3RtUvWxYz12345 invalid`,
			"AIzaSyC7xNq0pLm3RtUvWxYz12345",
		},
		{
			"platform ingest token",
			`ingest: POST rejected for ddi_9f8e7d6c5b4a3210zyxw`,
			"ddi_9f8e7d6c5b4a3210zyxw",
		},
		{
			"authorization header echoed in an error",
			`http: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig`,
			"eyJhbGciOiJIUzI1NiJ9.payload.sig",
		},
		{
			"key as a query parameter, which is how it reaches a URL",
			`GET https://api.example.com/v1/tts?model=x&api_key=sk-9zYx8Wv7Ut6Sr5&rate=24000`,
			"sk-9zYx8Wv7Ut6Sr5",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactSecrets(c.in)
			if strings.Contains(got, c.mustNotContain) {
				t.Fatalf("the secret survived redaction:\n  in:  %s\n  out: %s", c.in, got)
			}
			if !strings.Contains(got, "redacted") {
				t.Errorf("nothing was marked as redacted, so the line does not\n"+
					"say a value was removed: %s", got)
			}
		})
	}
}

// A redacted line must still be diagnosable. "The key is missing" and "the key
// is wrong" are different faults with different fixes, and stripping the line
// down to nothing would lose the difference.
func TestRedactionKeepsTheLineUseful(t *testing.T) {
	in := `booth: filler beat failed: qwen-tts: <400> InvalidApiKey for sk-abc123DEF456`
	got := redactSecrets(in)
	for _, keep := range []string{"filler beat failed", "qwen-tts", "400", "InvalidApiKey"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed diagnostic detail %q: %s", keep, got)
		}
	}
}

// Ordinary log lines must pass through untouched — over-redaction would quietly
// destroy the thing the page is for.
func TestRedactionLeavesNormalLinesAlone(t *testing.T) {
	for _, line := range []string{
		`booth: airing a filler line (26.5s of audio) [preStart=false running=false gen=0/0]`,
		`audio devices:   [2] MacBook Pro Speakers  <- system default`,
		`booth: writing commentary REMOTELY with qwen-flash — no GPU used`,
		`dd-pits: serving the local dashboard on :8088`,
	} {
		if got := redactSecrets(line); got != line {
			t.Errorf("a normal line was altered:\n  in:  %s\n  out: %s", line, got)
		}
	}
}

// The ring keeps the most recent lines and reassembles a line split across two
// writes — log output arrives as whatever the writer hands over, not neatly one
// line per call.
func TestLogRingKeepsRecentLinesAndRejoinsSplits(t *testing.T) {
	r := newLogRing()
	r.Write([]byte("first line\nsecond "))
	r.Write([]byte("line continues\n"))
	got := r.Lines()
	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(got), got)
	}
	if got[1] != "second line continues" {
		t.Errorf("a line split across writes was not rejoined: %q", got[1])
	}

	// Overflow drops the oldest, keeping the newest.
	r2 := newLogRing()
	for i := 0; i < logRingSize+50; i++ {
		r2.Write([]byte("line\n"))
	}
	if n := len(r2.Lines()); n > logRingSize {
		t.Errorf("ring grew past its cap: %d > %d", n, logRingSize)
	}
}

// The tape is capped, and copies the audio.
//
// The copy matters: the mixer fades and truncates a cue's buffer IN PLACE when a
// call is cut short, so holding the same slice would mean the debug page played
// the clipped version rather than what was generated.
func TestBoothTapeCapsAndCopiesAudio(t *testing.T) {
	tp := newBoothTape()
	pcm := []byte{1, 2, 3, 4}
	tp.add("finish", "and that is the race", pcm)

	// Mutating the caller's buffer must not change what the tape holds.
	pcm[0] = 99
	got := tp.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 take, got %d", len(got))
	}
	if got[0].PCM[0] == 99 {
		t.Error("the tape kept a reference to the caller's buffer instead of a copy")
	}

	for i := 0; i < boothTapeMaxItems+10; i++ {
		tp.add("progress", "line", []byte{0, 0, 0, 0})
	}
	if n := len(tp.List()); n > boothTapeMaxItems {
		t.Errorf("tape grew past its cap: %d > %d", n, boothTapeMaxItems)
	}
	// Newest first, so the interesting one is at the top.
	if l := tp.List(); len(l) > 1 && l[0].ID < l[1].ID {
		t.Error("List should return newest first")
	}
	// An empty clip is not a take.
	before := len(tp.List())
	tp.add("filler", "nothing", nil)
	if len(tp.List()) != before {
		t.Error("an empty clip should not be recorded")
	}
}
