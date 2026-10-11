package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/localchat"
	"github.com/bob9/dd-pits/internal/tts"
)

// heatSnapshot builds a snapshot of one heat in a given state.
//
// Built by unmarshalling JSON rather than by struct literal, because the
// snapshot's inner types are unexported — and it exercises the real parsing
// path FPVTrackside's data takes, which is the more honest test anyway.
//
// Timestamps are relative to NOW, not fixed. A heat whose start is more than a
// few minutes old with no new laps is retired as STALE — the guard that stops a
// heat FPVTrackside forgot to end from counting forever — so fixed past times
// make every heat "ended" on arrival and no transition ever fires.
func heatSnapshot(t *testing.T, start, end string, laps int) dronedash.Snapshot {
	t.Helper()

	var ls []string
	for i := 1; i <= laps; i++ {
		ls = append(ls, fmt.Sprintf(`{"Detection":"d%d","LapNumber":%d,"LengthSeconds":11.4}`, i, i))
	}

	raw := fmt.Sprintf(`{
      "event":  {"ID":"e1","Name":"Test It"},
      "rounds": [{"ID":"rd1","RoundNumber":1,"EventType":"Race"}],
      "races":  [{"ID":"r1","Start":%q,"End":%q,"RaceNumber":1,"TargetLaps":4,
                  "Valid":true,"Round":"rd1","Event":"e1","Laps":[%s]}]
    }`, start, end, strings.Join(ls, ","))

	var snap dronedash.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("building the snapshot: %v", err)
	}
	return snap
}

// TestFinishBeatFiresWhenAHeatEnds is the post-race commentary path.
//
// Repeatedly hard to catch by hand — every attempt on the real interface was
// derailed by heat state (a heat left running for hours, a field cleared by
// Alt+C, a locked screen) — so it is pinned here instead, where the transition
// can be produced on demand.
//
// The beat fires on the running -> ended transition, so the booth must SEE the
// heat running first: an ended heat it has never seen running produces nothing,
// which is correct (attaching mid-meeting must not replay old finishes).
func TestFinishBeatFiresWhenAHeatEnds(t *testing.T) {
	b := &booth{
		phasePre: true, phaseIn: true, phasePost: true,
		mixer:      newBoothMixer(),
		enabled:    true,
		lastState:  map[string]string{},
		finishDone: map[string]bool{},
		stagedDone: map[string]bool{},
		beatWake:   make(chan struct{}, 1),
	}

	// Heat under way.
	b.observe(heatSnapshot(t, ftNow(-20*time.Second), "", 2))
	if !hasBeat(b, "launch") {
		t.Error("a heat going green should queue the launch beat")
	}
	b.drainBeats()

	// Heat finishes.
	b.observe(heatSnapshot(t, ftNow(-20*time.Second), ftNow(0), 4))
	if !hasBeat(b, "finish") {
		t.Fatal("a heat ending must queue the finish beat — this is the post-race wrap")
	}
}

// TestFinishBeatIsNotReplayedForAHeatAlreadyOver: attaching mid-meeting must not
// announce finishes that happened before dd-pits was watching.
func TestFinishBeatIsNotReplayedForAHeatAlreadyOver(t *testing.T) {
	b := &booth{
		phasePre: true, phaseIn: true, phasePost: true,
		mixer:      newBoothMixer(),
		enabled:    true,
		lastState:  map[string]string{},
		finishDone: map[string]bool{},
		stagedDone: map[string]bool{},
		beatWake:   make(chan struct{}, 1),
	}

	// First thing it ever sees is an ended heat.
	b.observe(heatSnapshot(t, ftNow(-20*time.Second), ftNow(0), 4))
	if hasBeat(b, "finish") {
		t.Error("a heat that ended before dd-pits was watching must not be wrapped up")
	}
}

// TestFinishBeatFiresOnlyOncePerHeat: the snapshot repeats every poll, and a
// wrap per poll would talk over everything that follows.
func TestFinishBeatFiresOnlyOncePerHeat(t *testing.T) {
	b := &booth{
		phasePre: true, phaseIn: true, phasePost: true,
		mixer:      newBoothMixer(),
		enabled:    true,
		lastState:  map[string]string{},
		finishDone: map[string]bool{},
		stagedDone: map[string]bool{},
		beatWake:   make(chan struct{}, 1),
	}

	b.observe(heatSnapshot(t, ftNow(-20*time.Second), "", 2))
	b.drainBeats()
	b.observe(heatSnapshot(t, ftNow(-20*time.Second), ftNow(0), 4))
	b.drainBeats()

	// Same ended heat again, as every subsequent poll would deliver it.
	b.observe(heatSnapshot(t, ftNow(-20*time.Second), ftNow(0), 4))
	if hasBeat(b, "finish") {
		t.Error("the wrap fired twice for one heat")
	}
}

// ftNow renders a timestamp in FPVTrackside's format, offset from now.
func ftNow(d time.Duration) string {
	return time.Now().Add(d).Format("2006/01/02 15:04:05.000")
}

func hasBeat(b *booth, kind string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, q := range b.beats {
		if q.kind == kind {
			return true
		}
	}
	return false
}

func (b *booth) drainBeats() {
	b.mu.Lock()
	b.beats = nil
	b.mu.Unlock()
	time.Sleep(time.Millisecond)
}

// TestFinishBeatReachesTheAudioBus is the post-race wrap end to end: a heat
// ends, the booth writes the wrap, renders it, and the audio lands on the bus
// that feeds the speakers.
//
// The queue tests above prove the beat FIRES; every race call proves the bus
// PLAYS. This closes the link between them, which is the part that could not be
// caught by hand — each attempt on the real interface was derailed by heat
// state, a cleared field, or a locked screen.
//
// Opt-in: it loads the real models.
//
//	RUN_TTS_BENCH=1 go test ./cmd/dd-pits/ -run TestFinishBeatReaches -v
func TestFinishBeatReachesTheAudioBus(t *testing.T) {
	if os.Getenv("RUN_TTS_BENCH") != "1" {
		t.Skip("set RUN_TTS_BENCH=1 (loads the real models)")
	}

	voice, err := tts.StartLocal(context.Background(), tts.LocalConfig{
		Python: benchPython, Script: benchScript,
		Model:          "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-bf16",
		ReferenceAudio: benchRef, ReferenceText: benchRefTxt,
	})
	if err != nil {
		t.Skipf("no local voice: %v", err)
	}
	defer voice.Close()

	writer, err := localchat.Start(context.Background(), localchat.Config{
		Python: benchPython, Script: benchScript, Model: localchat.DefaultModel,
	})
	if err != nil {
		t.Skipf("no local writer: %v", err)
	}
	defer writer.Close()

	b := newBooth(nil)
	b.enabled, b.voiceMode = true, "solo"
	b.speech, b.mainScript = voice, writer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.run(ctx)

	// A heat runs, then ends — the transition the wrap hangs on.
	b.observe(heatSnapshot(t, ftNow(-30*time.Second), "", 3))
	time.Sleep(500 * time.Millisecond)
	b.observe(heatSnapshot(t, ftNow(-30*time.Second), ftNow(0), 4))

	// Generation is a local 30B plus speech; allow real time for it.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if lines := b.mixer.recentLines(); len(lines) > 0 {
			fmt.Printf("\n  the booth said:\n")
			for _, l := range lines {
				fmt.Printf("    %s\n", l)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("no commentary reached the audio bus within three minutes")
}
