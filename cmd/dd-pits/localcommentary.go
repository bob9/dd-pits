package main

// Generating a heat's commentary after it finishes, on this machine.
//
// The live booth calls the racing as it happens and its audio is gone once it
// has aired. This is the other thing: once a heat is over, the whole heat is
// known — every lap, the finishing order, who set a personal best — so a call
// can be written about it as a piece, and then played back over the video.
//
// It is the local counterpart of the platform's replay commentary, and it
// exists here for the same reason everything else in local mode does: a club
// with no account still streams their race day and still wants commentary on
// it.
//
// The audio is stored beside the settings so it survives a restart. Generating
// it costs real money and minutes, and losing a day's worth to a restart would
// be unforgivable.

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/ddapi"
	"github.com/bob9/dd-pits/internal/tts"
)

// commentaryGenTimeout bounds one heat's generation.
//
// Generous, because a heat produces several segments and each is a separate
// TTS call paced against a rate limit — but bounded, so a wedged request cannot
// hold a slot for the rest of the meeting.
const commentaryGenTimeout = 6 * time.Minute

// commentaryPace is the gap between TTS calls.
//
// The free tiers of both backends throttle aggressively — the platform settled
// on this for the same reason — and a burst that trips a 429 costs more time
// than pacing does.
const commentaryPace = 3 * time.Second

// generateRaceCommentary writes and voices a finished heat's commentary.
//
// Runs on its own goroutine: it takes minutes, and the timing loop must never
// wait on it. Only ONE runs at a time per agent (guarded by the caller), so a
// day's heats are worked through in order rather than all at once — which would
// exceed every rate limit and starve the live booth of the GPU.
func (a *agent) generateRaceCommentary(raceID string) {
	store := a.commentary
	if store == nil {
		return
	}
	if c, ok := store.get(raceID); ok && c.Status == "generating" {
		return // already under way
	}

	mv, _, ok := a.local.meeting()
	if !ok || mv == nil {
		return
	}
	var race *ddapi.Race
	for i := range mv.Races {
		if mv.Races[i].ID == raceID {
			race = &mv.Races[i]
			break
		}
	}
	if race == nil {
		store.put(&localCommentary{RaceID: raceID, Status: "failed",
			Error: "that heat is not in the current meeting"})
		return
	}
	if race.State != "ended" {
		store.put(&localCommentary{RaceID: raceID, Status: "failed",
			Error: "the heat has not finished yet"})
		return
	}
	// A heat with no laps has nothing to commentate.
	//
	// Caught here rather than left to the model, because a model given no data
	// does not say "there is no data" — it writes a plausible race. Observed:
	// a heat with four pilots and zero laps produced a complete call quoting
	// lap times copied out of this prompt's own format example. Commentary that
	// is confident and invented is far worse than none, because nobody
	// listening can tell.
	if countRecordedLaps(*race) == 0 {
		store.put(&localCommentary{RaceID: raceID, Status: "failed",
			Error: "no lap times were recorded for this heat, so there is nothing to commentate"})
		return
	}

	store.put(&localCommentary{RaceID: raceID, Status: "generating"})
	log.Printf("commentary: writing the call for %s", chapterLabel(*race))

	ctx, cancel := context.WithTimeout(context.Background(), commentaryGenTimeout)
	defer cancel()

	segs, err := a.buildRaceCommentary(ctx, mv, *race)
	if err != nil {
		log.Printf("commentary: %s failed: %v", chapterLabel(*race), err)
		store.put(&localCommentary{RaceID: raceID, Status: "failed", Error: err.Error()})
		return
	}
	store.put(&localCommentary{
		RaceID: raceID, Status: "ready", Segments: segs, MadeAt: time.Now().Unix(),
	})
	log.Printf("commentary: %s ready — %d segment(s)", chapterLabel(*race), len(segs))
}

// buildRaceCommentary writes the script and renders each line.
func (a *agent) buildRaceCommentary(ctx context.Context, mv *meetingView, race ddapi.Race) ([]commentarySegment, error) {
	b := a.booth
	if b == nil {
		return nil, fmt.Errorf("the commentary booth is not running")
	}
	b.mu.Lock()
	script, speech, style, voiceStyle := b.mainScript, b.speech, b.style, b.voiceStyle
	voice := b.voicePBP
	b.mu.Unlock()

	if script == nil {
		return nil, fmt.Errorf("no writer configured — set one on the Commentary page")
	}
	if speech == nil {
		return nil, fmt.Errorf("no voice configured — set one on the Commentary page")
	}

	prompt := replayPrompt(mv, race, style)
	raw, err := script.GenerateText(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("writing: %w", err)
	}

	cues := parseReplayCues(raw, race)
	if len(cues) == 0 {
		return nil, fmt.Errorf("the writer returned nothing usable")
	}

	dir := a.commentary.dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	var out []commentarySegment
	for i, cue := range cues {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(commentaryPace):
			}
		}
		pcm, rate, serr := speech.Speak(ctx, tts.Request{
			Text:        cue.Text,
			Instruction: boothpkg.TTSInstructionStyled(cue.Emotion, voiceStyle, true, style, "finish"),
			Voice:       voice,
			Language:    style.Language,
		})
		if serr != nil {
			// A line that will not render is SKIPPED, not fatal. Losing one
			// call is a gap; losing the whole heat's commentary because of it
			// is a wasted generation.
			log.Printf("commentary: skipping a line (%v)", serr)
			continue
		}
		if rate <= 0 {
			rate = 24000
		}
		name := fmt.Sprintf("%s-%02d.wav", safeRaceFile(race.ID), i)
		if err := os.WriteFile(dir+"/"+name, wavFromPCM(pcm, rate), 0o644); err != nil {
			return nil, err
		}
		out = append(out, commentarySegment{AtMs: cue.AtMs, File: name, Text: cue.Text})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("nothing rendered — check the voice settings")
	}
	return out, nil
}

// replayCue is one line and where it belongs in the heat.
type replayCue struct {
	AtMs    int64
	Text    string
	Emotion string
}

// replayPrompt asks for a call about a heat that has already run.
//
// Anchored to REAL LAP TIMES with their moments, so the commentary can be
// played back in sync with the video rather than being a summary read over the
// top of it. That is the difference between commentary and a voiceover.
func replayPrompt(mv *meetingView, race ddapi.Race, st boothpkg.Style) string {
	var b strings.Builder
	b.WriteString("You are writing the commentary for a drone race that has ALREADY FINISHED, to be " +
		"played back over the video of it.\n\n")
	b.WriteString("Write it as a live call, in the present tense, as if watching it happen.\n\n")

	// The example uses OBVIOUS placeholders, not realistic names and times.
	//
	// A realistic example is an invitation to copy it: given a heat with no
	// usable data, the model reproduced the example's lap times verbatim as
	// though they were real. Placeholders cannot be mistaken for the data.
	b.WriteString("FORMAT — one line per call, each starting with the number of seconds into the heat:\n")
	b.WriteString("  <seconds> | <what is happening at that moment>\n")
	b.WriteString("  <seconds> | <the next thing>\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("  - Use ONLY the lap times and pilots listed below. Never invent a time, a pilot, " +
		"a position or an overtake. If something is not in the data, it did not happen.\n")
	b.WriteString("  - Callsigns are names: say them exactly as written, never translate them.\n")
	b.WriteString("  - One short line per call. Six to twelve lines for the whole heat.\n")
	b.WriteString("  - The last line calls the result.\n\n")

	if lc := st.LanguageClause(); lc != "" {
		b.WriteString(lc + "\n\n")
	}
	if g := st.Guidance(); g != "" {
		b.WriteString(g + "\n\n")
	}

	fmt.Fprintf(&b, "HEAT: %s\n", chapterLabel(race))
	if mv != nil && mv.Name != "" {
		fmt.Fprintf(&b, "MEETING: %s\n", mv.Name)
	}
	b.WriteString("\nWHAT HAPPENED (seconds into the heat, then the lap):\n")

	type ev struct {
		at    float64
		line  string
		final bool
	}
	var events []ev
	for _, p := range race.Pilots {
		run := 0.0
		for i, lap := range p.Laps {
			run += lap
			events = append(events, ev{
				at:   run,
				line: fmt.Sprintf("  %5.1fs  %s lap %d in %.2f", run, p.Name, i+1, lap),
			})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at < events[j].at })
	for _, e := range events {
		b.WriteString(e.line + "\n")
	}

	b.WriteString("\nFINISHING ORDER:\n")
	fin := append([]ddapi.Pilot(nil), race.Pilots...)
	sort.Slice(fin, func(i, j int) bool { return fin[i].Position < fin[j].Position })
	for _, p := range fin {
		if p.Position <= 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s %s — %d laps\n", ordinal(p.Position), p.Name, len(p.Laps))
	}
	return b.String()
}

// parseReplayCues turns the model's lines into timed cues.
//
// Tolerant of the shapes a model actually returns — a stray bullet, a missing
// separator, a fenced block — because a whole generation is wasted when one
// formatting quirk makes it unparseable.
func parseReplayCues(raw string, race ddapi.Race) []replayCue {
	var out []replayCue
	dur := raceDurationSecs(race)

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "`"))
		line = strings.TrimLeft(line, "-*• \t")
		if line == "" {
			continue
		}
		at, text, ok := splitCue(line)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		// Clamped rather than dropped: a cue past the end of the heat is a
		// model rounding up, and the closing call is the one worth keeping.
		if dur > 0 && at > dur+20 {
			at = dur
		}
		out = append(out, replayCue{
			AtMs:    int64(at * 1000),
			Text:    strings.TrimSpace(text),
			Emotion: cueEmotion(at, dur),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	return out
}

// splitCue reads "12 | text" or "12s text".
func splitCue(line string) (float64, string, bool) {
	sep := strings.IndexAny(line, "|:")
	if sep > 0 {
		if at, ok := parseSecs(line[:sep]); ok {
			return at, line[sep+1:], true
		}
	}
	// "12s some words"
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		if at, ok := parseSecs(fields[0]); ok {
			return at, strings.Join(fields[1:], " "), true
		}
	}
	return 0, "", false
}

func parseSecs(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "s"))
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// cueEmotion picks the delivery for where in the heat a line falls: the closing
// call is the big one, the opening is the launch.
func cueEmotion(at, dur float64) string {
	switch {
	case dur > 0 && at >= dur*0.85:
		return "big-finish"
	case at < 8:
		return "excited"
	default:
		return "tense"
	}
}

// countRecordedLaps is how much of the heat was actually timed.
//
// The gate on generating anything: no laps means no race to describe, however
// many pilots were on the line.
func countRecordedLaps(race ddapi.Race) int {
	n := 0
	for _, p := range race.Pilots {
		n += len(p.Laps)
	}
	return n
}

// raceDurationSecs is how long the heat took, from the longest pilot's laps.
func raceDurationSecs(race ddapi.Race) float64 {
	var max float64
	for _, p := range race.Pilots {
		var run float64
		for _, l := range p.Laps {
			run += l
		}
		if run > max {
			max = run
		}
	}
	return max
}
