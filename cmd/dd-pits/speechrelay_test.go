package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestRelayPathEndToEnd runs real log lines through the watcher and the bridge
// together, which is the combination that decides what a venue actually hears.
// Each piece passing alone does not establish that.
func TestRelayPathEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Log.txt")
	if err := os.WriteFile(path, []byte("start of the day\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewSpeechLogWatcher(path)
	if _, err := w.Poll(); err != nil { // prime past the history
		t.Fatal(err)
	}

	b := bridgeFor(t)

	// dd-pits derives the lap from the timing data a moment before FPVTrackside
	// logs its own version, exactly as it happens at a meeting.
	b.NoteSpoken("'bob9' lap 3 in 2.41 in 1, quickest of the heat")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"SOUNDLOG (Notice) - SpeechManager: TTS ('bob9' lap 3 in 2.41 in 1)",
		"SOUNDLOG (Notice) - SpeechManager: TTS (Hurry up 'Wizz')",
		"RACE (Notice) - some unrelated logging",
		"SOUNDLOG (Notice) - SpeechManager: TTS (Arm your quads. Starting on the tone in less than 5)",
	} {
		fmt.Fprintln(f, line)
	}
	f.Close()

	lines, err := w.Poll()
	if err != nil {
		t.Fatal(err)
	}

	var spoken []string
	for _, l := range lines {
		if b.ShouldRelay(l.Text) {
			spoken = append(spoken, l.Text)
		}
	}

	want := []string{
		"Hurry up Wizz",
		"Arm your quads. Starting on the tone in less than 5",
	}
	if len(spoken) != len(want) {
		t.Fatalf("relayed %q, want %q", spoken, want)
	}
	for i := range want {
		if spoken[i] != want[i] {
			t.Errorf("relayed[%d] = %q, want %q", i, spoken[i], want[i])
		}
	}
}

// TestRelayPrioritisesActionableCues pins the one ordering guarantee that
// matters: a cue people physically act on must not queue behind colour.
func TestRelayPrioritisesActionableCues(t *testing.T) {
	if SpokenPriority("Arm your quads. Starting on the tone in less than 5") <= callPriorityCommentary {
		t.Fatal("a start cue must outrank commentary")
	}
	if SpokenPriority("Hurry up Wizz") <= callPriorityCommentary {
		t.Fatal("anything FPVTrackside says must outrank commentary")
	}
}
