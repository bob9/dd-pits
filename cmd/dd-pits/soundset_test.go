package main

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleSounds = `<?xml version="1.0" encoding="utf-8"?>
<ArrayOfSound xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <Sound><Key>RaceLap</Key><TextToSpeech>{pilot} lap {lapnumber} in {laptime} in {position}</TextToSpeech><Enabled>true</Enabled><Category>Detection</Category></Sound>
  <Sound><Key>HurryUp</Key><TextToSpeech>Hurry Up {pilot}</TextToSpeech><Enabled>true</Enabled><Category>Announcements</Category></Sound>
  <Sound><Key>InTheHole</Key><TextToSpeech>In the hole {pilots}</TextToSpeech><Enabled>false</Enabled><Category>Announcements</Category></Sound>
  <Sound><Key>RaceAnnounce</Key><TextToSpeech>Next up Round {round} {type} {race} {bracket}. </TextToSpeech><Enabled>true</Enabled><Category>Announcements</Category></Sound>
</ArrayOfSound>`

func writeSounds(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "Sounds.xml")
	if err := os.WriteFile(p, []byte(sampleSounds), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadsFPVTracksideSounds(t *testing.T) {
	s, err := LoadSoundSet(writeSounds(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Count() != 4 {
		t.Fatalf("expected 4 sounds, got %d", s.Count())
	}
	if tpl, ok := s.Template(SoundHurryUp); !ok || tpl != "Hurry Up {pilot}" {
		t.Fatalf("hurry up template: %q ok=%v", tpl, ok)
	}
}

// A sound switched OFF in FPVTrackside must stay silent here too — that is the
// entire reason for reading their configuration rather than keeping our own.
func TestDisabledSoundsStaySilent(t *testing.T) {
	s, _ := LoadSoundSet(writeSounds(t))
	if _, ok := s.Template(SoundInTheHole); ok {
		t.Fatal("a disabled sound must not be spoken")
	}
}

// A missing file must not stop the agent. Losing audio is an inconvenience;
// failing to start is a venue that cannot race.
func TestMissingSoundsFileIsNotFatal(t *testing.T) {
	s, err := LoadSoundSet("/nonexistent/Sounds.xml")
	if err == nil {
		t.Fatal("expected an error to be reported")
	}
	if s == nil {
		t.Fatal("a usable (empty) set must still be returned")
	}
	if _, ok := s.Template(SoundRaceLap); ok {
		t.Fatal("an empty set must have nothing to say")
	}
}

func TestFillSubstitutesPlaceholders(t *testing.T) {
	got := Fill("{pilot} lap {lapnumber} in {laptime} in {position}", map[string]string{
		"pilot": "bob9", "lapnumber": "2", "laptime": "24.08", "position": "1st",
	})
	if want := "bob9 lap 2 in 24.08 in 1st"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// An unfilled placeholder is dropped, never spoken. FPVTrackside's templates
// carry optional fields — a plain race has no bracket — and reading the literal
// word "bracket" aloud is worse than leaving the gap.
func TestUnfilledPlaceholdersAreDroppedNotSpoken(t *testing.T) {
	got := Fill("Next up Round {round} {type} {race} {bracket}. ", map[string]string{
		"round": "1", "type": "Race", "race": "2",
	})
	if want := "Next up Round 1 Race 2"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFillCollapsesWhitespaceFromDroppedFields(t *testing.T) {
	got := Fill("{a} middle {b} end", map[string]string{"b": "X"})
	if want := "middle X end"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
