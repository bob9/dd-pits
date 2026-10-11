package dronedash

// Sector (split-gate) times for the Live 3D Track View. FPVTrackside venues can
// run extra timing systems in the Split role; each mid-lap crossing arrives as
// a detection with TimingSystemIndex >= 1 and IsLapEnd=false. Laps stay in
// dd_laps (the lap pipeline is untouched) — this file stores only the split
// crossings, mirroring the dd_laps idempotency + reconcile machinery so
// FPVTrackside invalidations/re-detections propagate the same way.

import (
	"encoding/json"
	"sort"
	"strings"
)

// isSplitDetection reports whether d is a physical split-timer crossing worth
// storing: valid (not noise/double-trigger), from a split system (index >= 1),
// and not a lap end (a patched FT edge case can flag a split-index detection
// as a lap end — that one belongs to the lap pipeline, not here).
func isSplitDetection(d ftDetection) bool {
	return d.Valid && d.TimingSystemIndex >= 1 && !d.IsLapEnd
}

// detectionLapNumber resolves the lap a crossing belongs to. LapNumber is
// authoritative when set; some FT builds only populate RaceSector
// (= lap*100 + timing index), so fall back to decoding it.
func detectionLapNumber(d ftDetection) int {
	if d.LapNumber != 0 {
		return d.LapNumber
	}
	if d.RaceSector >= 100 {
		return d.RaceSector / 100
	}
	return 0
}

// trackSector is one entry of dd_tracks.sectors — the shape the editor saves
// and track.json serves. fromGate/toGate are gate sequence indices;
// TimingIndex is the physical split timer (FPVTrackside TimingSystemIndex).
type trackSector struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	FromGate    int    `json:"fromGate"`
	ToGate      int    `json:"toGate"`
	TimingIndex int    `json:"timingIndex"`
}

// parseSectorMap turns a track's sectors JSON into timingIndex -> sector index.
// Malformed JSON or entries yield an empty/partial map — a bad stored config
// degrades to identity mapping at the publish site, never an error.
func parseSectorMap(sectorsJSON string) map[int]int {
	m := map[int]int{}
	sectorsJSON = strings.TrimSpace(sectorsJSON)
	if sectorsJSON == "" {
		return m
	}
	var secs []trackSector
	if err := json.Unmarshal([]byte(sectorsJSON), &secs); err != nil {
		return m
	}
	for _, s := range secs {
		if s.TimingIndex >= 1 && s.Index >= 1 {
			m[s.TimingIndex] = s.Index
		}
	}
	return m
}

// orderCrossings returns d sorted by crossing time (stable on equal times by
// detection id). Race.json's Detections array order is NOT guaranteed
// chronological — never trust it for anything temporal.
func orderCrossings(ds []ftDetection) []ftDetection {
	out := make([]ftDetection, len(ds))
	copy(out, ds)
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := parseFTTime(out[i].Time), parseFTTime(out[j].Time)
		if ti.Equal(tj) {
			return out[i].ID < out[j].ID
		}
		return ti.Before(tj)
	})
	return out
}
