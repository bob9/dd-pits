package main

// FPVTrackside's own sound configuration, read straight off the venue laptop.
//
// The point of this file is that dd-pits does NOT define its own list of race
// calls. FPVTrackside already has one — 60-odd sounds, each with its spoken
// text, its enabled flag and its category — configured in a UI the race
// director already knows. Duplicating that here would mean two places to edit
// and a guarantee they drift.
//
// So the agent reads Sounds.xml, and the race director keeps configuring sound
// exactly where they always have. Muting FPVTrackside's own audio output then
// moves the VOICE to dd-pits without moving the SETTINGS.

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// SoundKey identifies one configurable sound, matching FPVTrackside's own enum.
type SoundKey string

const (
	SoundRaceLap             SoundKey = "RaceLap"
	SoundRaceDone            SoundKey = "RaceDone"
	SoundRaceStart           SoundKey = "RaceStart"
	SoundRaceOver            SoundKey = "RaceOver"
	SoundRaceResumed         SoundKey = "RaceResumed"
	SoundStartRaceIn         SoundKey = "StartRaceIn"
	SoundTimesUp             SoundKey = "TimesUp"
	SoundTimeRemaining       SoundKey = "TimeRemaining"
	SoundAfterTimesUp        SoundKey = "AfterTimesUp"
	SoundRaceAnnounce        SoundKey = "RaceAnnounce"
	SoundHurryUp             SoundKey = "HurryUp"
	SoundInTheHole           SoundKey = "InTheHole"
	SoundSector              SoundKey = "Sector"
	SoundNewLapRecord        SoundKey = "NewLapRecord"
	SoundNewHoleshotRecord   SoundKey = "NewHoleshotRecord"
	SoundTimeTrialEveryLap   SoundKey = "TimeTrialEveryLap"
	SoundTimeTrialTargetLaps SoundKey = "TimeTrialTargetLaps"
	SoundTimeTrialDone       SoundKey = "TimeTrialDone"
	SoundPracticeLap         SoundKey = "PracticeLap"
	SoundCasualLap           SoundKey = "CasualLap"
	SoundNameTest            SoundKey = "NameTest"

	// The build-up. Named because the bus drops these the moment a heat goes
	// green — see preRaceCall. FPVTrackside carries ~60 sounds and only the
	// ones dd-pits reasons about need a constant; the rest are relayed by name.
	SoundRaceAnnounceResults  SoundKey = "RaceAnnounceResults"
	SoundUntilRaceStart       SoundKey = "UntilRaceStart"
	SoundAnnouncePilotChannel SoundKey = "AnnouncePilotChannel"
	SoundPilotsEnableVideo    SoundKey = "PilotsEnableVideo"
	SoundHoleshot             SoundKey = "Holeshot"
)

// Sound is one entry from FPVTrackside's configuration.
type Sound struct {
	Key      SoundKey `xml:"Key"`
	Text     string   `xml:"TextToSpeech"`
	Enabled  bool     `xml:"Enabled"`
	Category string   `xml:"Category"`
}

type soundsFile struct {
	XMLName xml.Name `xml:"ArrayOfSound"`
	Sounds  []Sound  `xml:"Sound"`
}

// SoundSet is the venue's configured sounds, looked up by key.
type SoundSet struct {
	mu     sync.RWMutex
	byKey  map[SoundKey]Sound
	path   string
	loaded bool
}

// LoadSoundSet reads Sounds.xml for a profile.
//
// A missing or unreadable file is NOT an error: the agent still runs, it simply
// has nothing to say. Failing to start because a config file moved would be a
// poor trade for audio that is an enhancement.
func LoadSoundSet(path string) (*SoundSet, error) {
	s := &SoundSet{byKey: map[SoundKey]Sound{}, path: path}

	raw, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("read sounds: %w", err)
	}

	var f soundsFile
	if err := xml.Unmarshal(raw, &f); err != nil {
		return s, fmt.Errorf("parse sounds: %w", err)
	}

	for _, snd := range f.Sounds {
		if snd.Key == "" {
			continue
		}
		s.byKey[snd.Key] = snd
	}
	s.loaded = true
	return s, nil
}

// Count reports how many sounds were loaded.
func (s *SoundSet) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byKey)
}

// Loaded reports whether a configuration was actually read.
func (s *SoundSet) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// Template returns the spoken text for a key, and whether it is enabled.
//
// A sound the race director has switched OFF returns ok=false, so dd-pits stays
// silent for exactly the sounds FPVTrackside would have stayed silent for. That
// is the whole point of reading their file rather than keeping our own list.
func (s *SoundSet) Template(key SoundKey) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snd, ok := s.byKey[key]
	if !ok || !snd.Enabled || strings.TrimSpace(snd.Text) == "" {
		return "", false
	}
	return snd.Text, true
}

// Reload re-reads the file, so a change in FPVTrackside's settings takes effect
// without restarting the agent.
func (s *SoundSet) Reload() error {
	fresh, err := LoadSoundSet(s.path)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.byKey = fresh.byKey
	s.loaded = fresh.loaded
	s.mu.Unlock()
	return nil
}

// Keys lists every configured sound.
func (s *SoundSet) Keys() []SoundKey {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SoundKey, 0, len(s.byKey))
	for k := range s.byKey {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var placeholderRE = regexp.MustCompile(`\{[a-zA-Z]+\}`)

// Fill substitutes {placeholders} in a template.
//
// An unfilled placeholder is DROPPED rather than spoken. FPVTrackside's
// templates carry optional fields — "Next up Round {round} {type} {race}
// {bracket}" has no bracket in a plain race — and reading the literal word
// "bracket" aloud is worse than leaving the gap.
func Fill(template string, values map[string]string) string {
	out := placeholderRE.ReplaceAllStringFunc(template, func(m string) string {
		name := strings.ToLower(strings.Trim(m, "{}"))
		if v, ok := values[name]; ok {
			return v
		}
		return ""
	})

	// Collapse the whitespace and stray punctuation left by a dropped field.
	out = strings.Join(strings.Fields(out), " ")
	out = strings.TrimSpace(strings.TrimRight(out, " ."))

	return out
}

// DefaultSoundsPath is where FPVTrackside keeps a profile's sound settings.
//
// Same machine as the agent by definition — dd-pits reads the timing system's
// API on localhost — so this is a local file read, not a network call.
func DefaultSoundsPath(profile string) string {
	if profile == "" {
		profile = "Profile 1"
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	switch runtime.GOOS {
	case "darwin", "linux":
		return filepath.Join(home, "Documents", "FPVTrackside", "data", profile, "Sounds.xml")
	case "windows":
		return filepath.Join(home, "Documents", "FPVTrackside", "data", profile, "Sounds.xml")
	default:
		return ""
	}
}
