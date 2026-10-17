package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/elrs"
)

// Every pilot in the race goes to their race channel, Lowband included; a
// pilot without goggles set up is reported, not silently skipped.
func TestSetRaceChannels(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()
	race := &vmRace{ID: "r1", Pilots: []vmPilot{
		{Name: "Alice", ChannelLabel: "R1"},
		{Name: "Bob", ChannelLabel: "L3"},
		{Name: "Dave", ChannelLabel: "F4"},
	}}
	sent, failed := c.setRaceChannels(race)
	if len(sent) != 2 || failed["Dave"] == nil {
		t.Fatalf("sent %v, failed %v", sent, failed)
	}
	want := map[string]byte{"Alice": 4*8 + 0, "Bob": 5*8 + 2} // R1 = 32, L3 = 42
	deadline := time.Now().Add(2 * time.Second)
	for name, idx := range want {
		uid := elrs.UIDFromPhrase(name)
		for {
			if got, ok := fake.Channel(uid); ok && got == idx {
				break
			}
			if time.Now().After(deadline) {
				got, _ := fake.Channel(uid)
				t.Fatalf("%s's goggles on index %d, want %d", name, got, idx)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// The switch decides whether cueing a race sends channels.
func TestRaceChannelsOnStageFollowsTheSwitch(t *testing.T) {
	c, fake, cancel := elrsTestRig(t)
	defer cancel()
	cfg, _ := loadLocalConfig(filepath.Join(t.TempDir(), "cfg.json"))
	c.localCfg = cfg
	race := &vmRace{ID: "r2", Pilots: []vmPilot{{Name: "Carol", ChannelLabel: "R5"}}}
	uid := elrs.UIDFromPhrase("Carol")

	c.raceChannelsOnStage(race)
	time.Sleep(50 * time.Millisecond)
	if _, ok := fake.Channel(uid); ok {
		t.Fatal("switch off: no channel should be sent")
	}
	e := cfg.ELRSSettings()
	e.RaceChannels = true
	cfg.SetELRS(e)
	c.raceChannelsOnStage(race)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, ok := fake.Channel(uid); ok && got == 4*8+4 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("switch on: Carol never got R5")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
