package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The one value that decides FPVTrackside's startup mute is edited in place,
// and nothing else in the file moves.
func TestFTSetSpeechVolumeEditsOnlyTheVolume(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "data", "Profile 1")
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "<Config>\n  <Voice>Microsoft Zira Desktop</Voice>\n" +
		"  <TextToSpeechVolume>100</TextToSpeechVolume>\n  <Volume>100</Volume>\n</Config>\n"
	path := filepath.Join(prof, "ProfileSettings.xml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ftWriteSpeechVolume(path, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "<Config>\n  <Voice>Microsoft Zira Desktop</Voice>\n" +
		"  <TextToSpeechVolume>0</TextToSpeechVolume>\n  <Volume>100</Volume>\n</Config>\n"
	if string(got) != want {
		t.Errorf("file after mute:\n%s\nwant:\n%s", got, want)
	}
	// And back on: the master Volume must stay untouched — it also covers the
	// beeps, and muting those was never asked for.
	if err := ftWriteSpeechVolume(path, 100); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != body {
		t.Errorf("round-trip did not restore the original:\n%s", got)
	}
}
