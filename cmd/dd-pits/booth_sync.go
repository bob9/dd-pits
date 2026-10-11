package main

// Broadcast-sync controller for the live booth.
//
// Writing + voicing each call takes the booth several seconds, so a call for an
// on-track moment is always LATE. To line the audio up with the video we delay
// the VIDEO by a buffer B and HOLD each generated call until event_time + B, so
// both land on the same frame the moment reaches the viewer.
//
// B is chosen automatically to always clear the real generation latency
// (measured per call) plus a margin, and only ever GROWS during a meeting — a
// shrinking video delay would make the stream visibly skip. dd-pits pushes B
// into OBS as a stacked video render delay (booth_obs.go); the mixer holds each
// cue to its playAt. This file owns the math; it has no I/O so it is unit-tested.

import (
	"fmt"
	"sort"
	"sync"
)

const (
	boothSyncFloorMs  = 12000  // start here; never delay the video less than this
	boothSyncCeilMs   = 120000 // sanity ceiling (realistically far under)
	boothSyncMarginMs = 5000   // headroom the buffer always keeps over latency
	boothSyncWindow   = 32     // latency samples retained
	boothSyncStepMs   = 1000   // round the buffer up to whole seconds (OBS-friendly)
)

type boothSync struct {
	mu      sync.Mutex
	samples []int64
	target  int64 // current buffer B (ms)
	lastLat int64
	maxLat  int64
	lateN   int // cues that still aired late (latency jumped past the buffer)

	floor, ceil, margin, step int64
}

func newBoothSync() *boothSync {
	return &boothSync{
		target: boothSyncFloorMs,
		floor:  boothSyncFloorMs,
		ceil:   boothSyncCeilMs,
		margin: boothSyncMarginMs,
		step:   boothSyncStepMs,
	}
}

// observe records one call's generation latency (audio-ready minus its event
// moment, ms) and returns the buffer B to use — grown if needed so B always
// clears the SLOWEST recent call plus the margin. Ratchets up only.
func (s *boothSync) observe(latencyMs int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if latencyMs < 0 {
		latencyMs = 0
	}
	s.lastLat = latencyMs
	if latencyMs > s.maxLat {
		s.maxLat = latencyMs
	}
	s.samples = append(s.samples, latencyMs)
	if len(s.samples) > boothSyncWindow {
		s.samples = s.samples[len(s.samples)-boothSyncWindow:]
	}
	need := roundUpMs(s.worstLocked()+s.margin, s.step)
	if need > s.target {
		s.target = need // ratchet up to guarantee headroom
	}
	if s.target > s.ceil {
		s.target = s.ceil
	}
	if s.target < s.floor {
		s.target = s.floor
	}
	return s.target
}

// setFloor raises the starting/minimum buffer (the operator's -booth-min-delay)
// so early calls get generous headroom and the auto video delay never dips
// below it. Lifts the ceiling too if the floor is set above it, so an explicit
// operator choice is honoured rather than silently clamped.
func (s *boothSync) setFloor(ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms <= s.floor {
		return
	}
	s.floor = ms
	if s.ceil < s.floor {
		s.ceil = s.floor
	}
	if s.target < s.floor {
		s.target = s.floor
	}
}

// buffer returns the current B (ms) without recording a sample.
func (s *boothSync) buffer() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}

// noteLate records that a cue aired after its mark (latency jumped past B before
// the ratchet caught up) — surfaced on the status page.
func (s *boothSync) noteLate() {
	s.mu.Lock()
	s.lateN++
	s.mu.Unlock()
}

// worstLocked is the slowest retained sample — the buffer must clear it.
func (s *boothSync) worstLocked() int64 {
	if len(s.samples) == 0 {
		return 0
	}
	cp := append([]int64(nil), s.samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)-1]
}

type boothSyncStats struct {
	BufferMs, LastLatMs, MaxLatMs int64
	LateCues                      int
}

func (s *boothSync) stats() boothSyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return boothSyncStats{BufferMs: s.target, LastLatMs: s.lastLat, MaxLatMs: s.maxLat, LateCues: s.lateN}
}

func roundUpMs(v, step int64) int64 {
	if step <= 0 {
		return v
	}
	return (v + step - 1) / step * step
}

// boothSyncLine renders the /booth status page's broadcast-sync summary.
func boothSyncLine(b *booth) string {
	if b.sync == nil {
		return "Broadcast sync: off"
	}
	st := b.sync.stats()
	switch {
	case b.obs != nil:
		conn, applied := b.obs.status()
		where := "connecting to OBS…"
		if conn {
			where = fmt.Sprintf("OBS video delayed %.1fs", float64(applied)/1000)
		}
		return fmt.Sprintf("Broadcast sync: AUTO — buffer %.1fs · %s · last call latency %.1fs (worst %.1fs)%s",
			float64(st.BufferMs)/1000, where, float64(st.LastLatMs)/1000, float64(st.MaxLatMs)/1000, lateSuffix(st.LateCues))
	case b.manualDelayMs > 0:
		return fmt.Sprintf("Broadcast sync: MANUAL — holding audio to %.1fs (match your OBS video delay) · latency last %.1fs / worst %.1fs%s",
			float64(b.manualDelayMs)/1000, float64(st.LastLatMs)/1000, float64(st.MaxLatMs)/1000, lateSuffix(st.LateCues))
	default:
		return fmt.Sprintf("Broadcast sync: off — measured commentary latency last %.1fs / worst %.1fs (set -obs-url or -booth-video-delay to line audio up with video)",
			float64(st.LastLatMs)/1000, float64(st.MaxLatMs)/1000)
	}
}

func lateSuffix(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" · %d aired late (buffer grew)", n)
}
