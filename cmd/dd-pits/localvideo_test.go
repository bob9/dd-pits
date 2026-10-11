package main

import (
	"strings"
	"testing"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/ddapi"
)

// One anchor fills the whole field — that is the entire point of the feature.
// Find ONE heat in the video, and every other heat's link follows from the
// timing data, because the gaps between heats are already known.
func TestOneAnchorPlacesEveryHeat(t *testing.T) {
	v := localVideo{StreamURL: "https://youtu.be/abc123"}

	// Heat two starts 600s into the meeting and the operator finds it at
	// 1:12:30 in the video, so the stream began 4350s before that heat.
	v.anchorFrom(1_000_600, 4350)

	// The heat they anchored on lands where they said it did.
	if got := v.offsetFor("r2", 1_000_600); got != 4350 {
		t.Errorf("the anchored heat should sit at its stated offset, got %d", got)
	}
	// And a heat 600s earlier in the meeting is 600s earlier in the video.
	if got := v.offsetFor("r1", 1_000_000); got != 3750 {
		t.Errorf("an earlier heat should be earlier in the video, got %d", got)
	}

	u := v.watchURLFor("r1", 1_000_000)
	if !strings.Contains(u, "3750") {
		t.Errorf("the watch link should carry the offset: %q", u)
	}
}

// A heat that ran BEFORE the stream started has nothing to link to. Clamping it
// to zero would point at the wrong moment while looking like it worked, which
// is worse than no link at all.
func TestHeatsBeforeTheStreamGetNoLink(t *testing.T) {
	v := localVideo{StreamURL: "https://youtu.be/abc123", StreamStartedAt: 1_000_000}
	if u := v.watchURLFor("early", 999_000); u != "" {
		t.Errorf("a heat before the stream must have no link, got %q", u)
	}
}

// A hand-set link always wins: it means somebody looked at the video and
// corrected it, which is better evidence than arithmetic.
func TestHandSetLinkBeatsTheDerivedOne(t *testing.T) {
	v := localVideo{
		StreamURL:       "https://youtu.be/abc123",
		StreamStartedAt: 1_000_000,
		PerRace:         map[string]string{"r1": "https://youtu.be/abc123?t=99"},
	}
	if got := v.watchURLFor("r1", 1_000_500); !strings.Contains(got, "99") {
		t.Errorf("the hand-set link must win, got %q", got)
	}
	if got := v.watchURLFor("r2", 1_000_500); !strings.Contains(got, "500") {
		t.Errorf("other heats still derive, got %q", got)
	}
}

// YouTube ignores a chapter list entirely unless the first entry is at
// 00:00:00 — the single most common reason a pasted list "does nothing".
func TestChaptersStartAtZeroAndRunInVideoOrder(t *testing.T) {
	v := localVideo{StreamURL: "https://youtu.be/x", StreamStartedAt: 1_000_000}
	races := []ddapi.Race{
		{ID: "r2", Round: 1, Number: 2, StartMs: 1_000_900_000, TypeLabel: "Race"},
		{ID: "r1", Round: 1, Number: 1, StartMs: 1_000_300_000, TypeLabel: "Race"},
	}
	lines := buildChapters(v, races)

	if len(lines) != 3 || lines[0].Offset != 0 {
		t.Fatalf("the list must open at 00:00:00, got %v", lines)
	}
	if !strings.HasPrefix(lines[0].String(), "00:00:00") {
		t.Errorf("first line = %q", lines[0].String())
	}
	// Ordered by position in the VIDEO. A rerun heat happens later in the
	// stream than its race number suggests, and an out-of-order list is
	// rejected.
	if lines[1].Offset >= lines[2].Offset {
		t.Errorf("chapters must ascend: %v", lines)
	}
	if !strings.Contains(lines[1].String(), "Race 1-1") {
		t.Errorf("chapter should be named like the dashboard: %q", lines[1].String())
	}
}

// A heat with no laps must be REFUSED, not commentated.
//
// This is not defensive tidiness: it was observed. A heat with four pilots and
// zero recorded laps produced a complete, confident call — quoting lap times
// copied out of the prompt's own format example as though they were real.
// A model given no data does not say "there is no data"; it writes a plausible
// race. Commentary that is invented and authoritative is worse than none,
// because nobody listening can tell the difference.
func TestHeatWithNoLapsHasNothingToCommentate(t *testing.T) {
	empty := ddapi.Race{ID: "r1", State: "ended", Pilots: []ddapi.Pilot{
		{Name: "bob9"}, {Name: "Fizz"}, {Name: "Willman"}, {Name: "Wizz"},
	}}
	if countRecordedLaps(empty) != 0 {
		t.Fatal("this fixture is supposed to have no laps")
	}

	flown := ddapi.Race{ID: "r2", State: "ended", Pilots: []ddapi.Pilot{
		{Name: "bob9", Laps: []float64{11.42, 11.31}},
		{Name: "Fizz", Laps: []float64{12.28}},
	}}
	if countRecordedLaps(flown) != 3 {
		t.Errorf("a flown heat should count its laps, got %d", countRecordedLaps(flown))
	}
}

// The prompt's format example must not look like data.
//
// A realistic example is an invitation to copy it, and that is exactly what
// happened: the model reproduced the example's lap times verbatim. Placeholders
// cannot be mistaken for the meeting's own numbers.
func TestPromptExampleCannotBeMistakenForData(t *testing.T) {
	race := ddapi.Race{ID: "r1", Round: 1, Number: 1, TypeLabel: "Race",
		Pilots: []ddapi.Pilot{{Name: "bob9", Laps: []float64{11.42}, Position: 1}}}
	p := replayPrompt(nil, race, boothpkg.Style{})

	// The old example's numbers were spoken as words; if the example ever
	// carries realistic-looking times again, they will be copied.
	for _, banned := range []string{"eleven forty-two", "eleven thirty-one"} {
		if strings.Contains(strings.ToLower(p), banned) {
			t.Errorf("the format example must not contain plausible lap times: %q", banned)
		}
	}
	if !strings.Contains(p, "<seconds>") {
		t.Error("the format example should use obvious placeholders")
	}
	// And the real data must be in there.
	if !strings.Contains(p, "11.42") {
		t.Error("the heat's actual lap times must reach the prompt")
	}
	if !strings.Contains(strings.ToLower(p), "never invent") {
		t.Error("the prompt must forbid inventing")
	}
}
