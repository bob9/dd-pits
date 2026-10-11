package booth

import (
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/ddapi"
)

// The holeshot glossary must reach the model on EVERY beat, solo and duo —
// without it the booth narrates the blocks-to-gate-one sprint as a lap.
func TestPromptAlwaysCarriesHoleshotGlossary(t *testing.T) {
	for _, kind := range BeatKinds {
		for _, solo := range []bool{false, true} {
			p := Prompt(kind, "DATA", "", solo, DefaultTheme)
			if !strings.Contains(p, "NOT a lap") || !strings.Contains(p, "Never add the holeshot to a lap count") {
				t.Fatalf("%s (solo=%v) prompt missing the holeshot glossary", kind, solo)
			}
			if !strings.Contains(p, BeatBriefs[kind]) || !strings.Contains(p, "DATA") {
				t.Fatalf("%s prompt missing brief or data", kind)
			}
		}
	}
}

// A theme re-flavours the persona and injects its deployment guidance into
// every beat, while an unknown key falls back to classic (no guidance leak).
func TestThemeFlavoursPrompt(t *testing.T) {
	comedy := Theme("comedy")
	if comedy.Key != "comedy" || comedy.guidance == "" {
		t.Fatal("comedy theme should resolve with guidance")
	}
	if Theme("nope").Key != "classic" || Theme("").Key != "classic" {
		t.Fatal("unknown/blank theme must fall back to classic")
	}
	p := Prompt("staged", "DATA", "", false, "comedy")
	if !strings.Contains(p, comedy.guidance) {
		t.Fatal("comedy prompt missing its personality guidance")
	}
	// Classic stays clean — no personality guidance block.
	if strings.Contains(Prompt("staged", "DATA", "", false, DefaultTheme), "PERSONALITY:") {
		t.Fatal("classic prompt must not carry a PERSONALITY guidance block")
	}
}

func TestRaceContextLabelsHoleshotDistinctly(t *testing.T) {
	mv := &ddapi.Meeting{Name: "Cup"}
	r := ddapi.Race{Label: "Race 1", TypeLabel: "Race", TargetLaps: 3, State: "running",
		Pilots: []ddapi.Pilot{{Name: "A", ChannelLabel: "R1", Holeshot: 2.31, Laps: []float64{9.1}}}}
	ctx := RaceContext(r, mv, false)
	if !strings.Contains(ctx, "holeshot (blocks → gate one, not a lap) 2.31s") {
		t.Fatalf("holeshot should be labelled as not-a-lap: %s", ctx)
	}
	if !strings.Contains(ctx, "LATEST LAP (lap 1): 9.10") {
		t.Fatalf("the newest lap should be named: %s", ctx)
	}
	// A post-finish junk crossing (in Laps but not DispLaps) never reaches
	// the commentary — DispLaps is the counted set.
	r.Pilots[0].Laps = []float64{9.1, 8.0}
	r.Pilots[0].DispLaps = []float64{9.1}
	junk := RaceContext(r, mv, false)
	if strings.Contains(junk, "8.00") || !strings.Contains(junk, "9.10") {
		t.Fatalf("junk lap leaked into commentary context: %s", junk)
	}
	// Lineup-only view carries neither timing block.
	if lu := RaceContext(r, mv, true); strings.Contains(lu, "holeshot") || strings.Contains(lu, "LATEST LAP") {
		t.Fatalf("lineup view should omit timing: %s", lu)
	}
}

func TestHeatIsTimeTrialAndFormatLine(t *testing.T) {
	// A meeting board where B leads on 22.00 and A is P2 on 22.40.
	mv := &ddapi.Meeting{Name: "Cup", Window: 3, Board: []ddapi.Standing{
		{Name: "B", Pos: 1, BestConsec: 22.00},
		{Name: "A", Pos: 2, BestConsec: 22.40},
	}}
	tt := ddapi.Race{Label: "TT 1", TypeLabel: "Time Trial", ConsecWindow: 3, State: "running",
		Pilots: []ddapi.Pilot{{Name: "A", ChannelLabel: "R1", Holeshot: 2.31}}}
	if !HeatIsTimeTrial(tt) {
		t.Fatal("Time Trial TypeLabel should be detected as a TT")
	}
	ctx := RaceContext(tt, mv, false)
	if !strings.Contains(ctx, "QUALIFYING") || !strings.Contains(ctx, "wheel-to-wheel") || !strings.Contains(ctx, "3 CONSECUTIVE") {
		t.Fatalf("TT context missing qualifying/ladder guidance: %s", ctx)
	}
	// A is P2, 0.40 off B — the ladder gap to climb must be spelled out.
	if !strings.Contains(ctx, "needs 0.40s to pass B for P1") {
		t.Fatalf("TT context missing the ladder-gap line: %s", ctx)
	}
	// Qualifying starts are staggered: the format line must say so, and the
	// pilot's own holeshot time must never reach the booth (it isn't scored
	// and isn't comparable between pilots).
	if !strings.Contains(ctx, "STAGGERED start") {
		t.Fatalf("TT context must explain the staggered start: %s", ctx)
	}
	if strings.Contains(ctx, "2.31") {
		t.Fatalf("TT context must not carry a per-pilot holeshot time: %s", ctx)
	}
	// The launch brief is format-aware too, so a TT launch call isn't teed up
	// as the start of a holeshot battle.
	if !strings.Contains(BeatBriefs["launch"], "TIME TRIAL") {
		t.Fatal("launch brief must tell the booth a time-trial start is staggered")
	}
	race := ddapi.Race{Label: "Race 1", TypeLabel: "Race", TargetLaps: 3, State: "running",
		Pilots: []ddapi.Pilot{{Name: "A", ChannelLabel: "R1"}}}
	if HeatIsTimeTrial(race) {
		t.Fatal("Race TypeLabel should NOT be a TT")
	}
	if rc := RaceContext(race, mv, false); !strings.Contains(rc, "FORMAT: RACE") || !strings.Contains(rc, "overtakes") {
		t.Fatalf("race context missing the race format line: %s", rc)
	}
	// Empty TypeLabel falls back to the IsTT flag.
	if !HeatIsTimeTrial(ddapi.Race{IsTT: true}) || HeatIsTimeTrial(ddapi.Race{}) {
		t.Fatal("IsTT fallback wrong for an empty TypeLabel")
	}
}

func TestPilotIncidentBriefsAvoidCrash(t *testing.T) {
	out := BeatBriefs["pilot_out"]
	// The brief must instruct the "looks like we've lost them" tone and
	// explicitly forbid the word "crashed".
	if out == "" || !strings.Contains(strings.ToLower(out), "lost") || !strings.Contains(out, "NEVER use the word") {
		t.Fatalf("pilot_out brief must exist, cue the 'lost them' tone and forbid 'crashed': %q", out)
	}
	if BeatBriefs["pilot_back"] == "" {
		t.Fatal("pilot_back brief must exist")
	}
}

func TestLaunchAndHoleshotBriefsAreImmediate(t *testing.T) {
	if !strings.Contains(BeatBriefs["launch"], "ONE short") {
		t.Fatal("launch must be a single short line so the holeshot call isn't stuck behind it")
	}
	if !strings.Contains(BeatBriefs["holeshot"], "INSTANT") {
		t.Fatal("holeshot brief must demand immediacy")
	}
}

func TestShortBeatCoversActionNotAnalysis(t *testing.T) {
	// Snappy mode shortens the in-heat action beats (they must sync to the video)
	// but leaves the between-race/analysis beats full-length.
	short := map[string]bool{"launch": true, "holeshot": true, "progress": true, "pilot_out": true, "pilot_back": true}
	// finish is post-race (a full standings/bracket wrap), so it stays long.
	full := []string{"meeting_open", "staged", "meeting_talk", "filler", "qual_move", "finish"}
	for k := range short {
		if !ShortBeat(k) {
			t.Errorf("ShortBeat(%q) = false, want true", k)
		}
	}
	for _, k := range full {
		if ShortBeat(k) {
			t.Errorf("ShortBeat(%q) = true, want false (analysis beats stay full-length)", k)
		}
	}
	// Every ShortBeat kind must be a real beat.
	for _, k := range BeatKinds {
		_ = ShortBeat(k) // must not panic / all kinds classified
	}
}

func TestBeatPhaseClassifiesEveryBeat(t *testing.T) {
	want := map[string]string{
		"meeting_open": "pre", "staged": "pre", "filler": "pre",
		"launch": "in", "holeshot": "in", "progress": "in", "meeting_talk": "in",
		"pilot_out": "in", "pilot_back": "in", "qual_move": "in",
		"finish": "post",
	}
	for _, k := range BeatKinds {
		got := BeatPhase(k)
		if got != "pre" && got != "in" && got != "post" {
			t.Errorf("BeatPhase(%q) = %q, not a valid phase", k, got)
		}
		if w, ok := want[k]; ok && got != w {
			t.Errorf("BeatPhase(%q) = %q, want %q", k, got, w)
		}
	}
}

func TestCapShort(t *testing.T) {
	if got := CapShort([]string{"a", "b", "c", "d"}); len(got) != 2 {
		t.Fatalf("CapShort should trim to 2 lines, got %d", len(got))
	}
	if got := CapShort([]string{"a"}); len(got) != 1 {
		t.Fatalf("CapShort must not pad a short exchange, got %d", len(got))
	}
	if BrevitySuffix == "" || !strings.Contains(BrevitySuffix, "BREVITY") {
		t.Fatal("BrevitySuffix should carry the brevity directive")
	}
}

// TestPilotWithNoLapsIsNotGivenAPosition covers a call heard on air: "Willman
// holds the lead" for a pilot who had not flown a single lap.
//
// FPVTrackside assigns a position to every pilot in a heat, including ones who
// never crossed the line. Printing it made a lapless pilot read as "position 1",
// and the model called them the leader — reading exactly what it was given.
func TestPilotWithNoLapsIsNotGivenAPosition(t *testing.T) {
	race := ddapi.Race{
		ID: "r1", Label: "Round 3 — Race 5", TargetLaps: 4, State: "running",
		Pilots: []ddapi.Pilot{
			{Name: "bob9", ChannelLabel: "R1", Position: 2, Laps: []float64{5.77, 4.86}},
			{Name: "Willman", ChannelLabel: "F2", Position: 1}, // no laps at all
		},
	}
	ctx := RaceContext(race, &ddapi.Meeting{}, false) // full detail, not lineup-only

	// The lapless pilot must be flagged, not positioned.
	wi := strings.Index(ctx, "Willman")
	if wi < 0 {
		t.Fatal("the pilot should still appear in the line-up")
	}
	line := ctx[wi:]
	if e := strings.Index(line, "\n"); e > 0 {
		line = line[:e]
	}
	if strings.Contains(line, "position 1") {
		t.Errorf("a pilot with no laps was given a position: %q", line)
	}
	if !strings.Contains(line, "NO LAPS YET") {
		t.Errorf("a pilot with no laps must be flagged as not racing: %q", line)
	}

	// A pilot who IS racing keeps their position.
	bi := strings.Index(ctx, "bob9")
	bline := ctx[bi:]
	if e := strings.Index(bline, "\n"); e > 0 {
		bline = bline[:e]
	}
	if !strings.Contains(bline, "position 2") {
		t.Errorf("a racing pilot must keep their position: %q", bline)
	}
}

// TestStripSpeakerTagsHandlesSeveralTurnsOnOneLine covers a solo-mode fault
// heard on air: the commentator read the word "Ace" out mid-sentence.
//
// Only the tag at the START of a line was removed, so a line carrying more than
// one turn kept the rest — and with a single voice there is nobody for a
// speaker name to distinguish, so it is pure noise.
func TestStripSpeakerTagsHandlesSeveralTurnsOnOneLine(t *testing.T) {
	got := StripSpeakerTags([]string{
		"Ace: Fizz closes the gap. Ace: Wizz holds third. Torque: and that is the story.",
	})[0]
	for _, name := range []string{SpeakerPBP + ":", SpeakerColor + ":"} {
		if strings.Contains(got, name) {
			t.Errorf("a speaker tag survived and would be read aloud: %q", got)
		}
	}
	if !strings.Contains(got, "Wizz holds third") {
		t.Errorf("the commentary itself must survive: %q", got)
	}
}

// TestStripSpeakerTagsKeepsOrdinaryColons guards the other direction: the
// commentary's own punctuation must not be eaten.
func TestStripSpeakerTagsKeepsOrdinaryColons(t *testing.T) {
	for _, line := range []string{
		"The gap: nine tenths of a second.",
		"Willman: the fastest qualifier so far.",
	} {
		if got := StripSpeakerTags([]string{line})[0]; got != line {
			t.Errorf("StripSpeakerTags(%q) = %q — only the booth's own speaker names are tags", line, got)
		}
	}
}

// TestParseLinesPutsEachTurnOnItsOwnLine covers the DUO half of the same fault.
//
// Everything downstream assumes one line is one speaker. When the model runs
// several turns onto a line, solo read the tag aloud (fixed separately) and duo
// did something worse: the multi-speaker renderer takes only a LEADING label,
// so the whole line became one turn in the play-by-play voice — the analyst's
// words in the caller's voice, with "Torque" spoken out loud.
func TestParseLinesPutsEachTurnOnItsOwnLine(t *testing.T) {
	got := ParseLines(SpeakerPBP + ": Fizz closes the gap. " +
		SpeakerColor + ": and that is the story of the heat.")

	if len(got) != 2 {
		t.Fatalf("expected one line per turn, got %d: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], SpeakerPBP+": ") {
		t.Errorf("first turn should be %s's: %q", SpeakerPBP, got[0])
	}
	if !strings.HasPrefix(got[1], SpeakerColor+": ") {
		t.Errorf("second turn should be %s's — in duo this is what puts it in the "+
			"right voice: %q", SpeakerColor, got[1])
	}
	if !strings.Contains(got[1], "that is the story") {
		t.Errorf("the analyst's words must survive the split: %q", got[1])
	}
}

// TestParseLinesLeavesOrdinaryPunctuationAlone guards the other direction: a
// colon in the commentary itself, or a pilot's name before one, is not a tag.
func TestParseLinesLeavesOrdinaryPunctuationAlone(t *testing.T) {
	got := ParseLines(SpeakerPBP + ": The gap: nine tenths. Willman: the fastest so far.")
	if len(got) != 1 {
		t.Fatalf("only real speaker tags start a new turn, got %d: %q", len(got), got)
	}
	for _, want := range []string{"The gap: nine tenths", "Willman: the fastest so far"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("commentary was mangled, %q missing from %q", want, got[0])
		}
	}
}
