package main

import (
	"fmt"
	"testing"
)

// TestBoothOBSVRAM estimates the GPU memory OBS's Render Delay (gpu_delay)
// filter needs to realise a given video delay B. The filter holds every
// rendered frame for the delay window at CANVAS resolution:
//
//	VRAM ≈ width × height × 4 bytes × fps × delaySeconds   (per delayed source)
//
// This is the hard limit on a small card. dd-pits stacks filters to exceed the
// 20s-per-filter cap, but they all buffer into the same VRAM pool.
//
// Run: go test ./cmd/dd-pits/ -run VRAM -v
func TestBoothOBSVRAM(t *testing.T) {
	type res struct {
		name      string
		w, h, fps int
	}
	resolutions := []res{
		{"1080p60", 1920, 1080, 60},
		{"1080p30", 1920, 1080, 30},
		{"720p60", 1280, 720, 60},
		{"720p30", 1280, 720, 30},
	}
	// The B values our sync scenarios settled on.
	delays := []int64{19, 33, 39, 48}

	perSecMB := func(r res) float64 {
		return float64(r.w*r.h*4) * float64(r.fps) / (1024 * 1024)
	}

	t.Logf("Per-second-of-delay VRAM cost of the Render Delay filter:")
	for _, r := range resolutions {
		t.Logf("  %-9s %6.0f MB per second of delay", r.name, perSecMB(r))
	}

	t.Logf("\nVRAM needed for each settled buffer B (single delayed source):")
	t.Logf("%-9s | %s", "canvas", "  19s     33s     39s     48s")
	for _, r := range resolutions {
		line := ""
		for _, d := range delays {
			line += formatGB(perSecMB(r) * float64(d))
		}
		t.Logf("%-9s |%s", r.name, line)
	}

	// A 4GB card in practice: OS + OBS + the encoder + your other sources take a
	// chunk, so ~2.8GB is a realistic ceiling for the delay filter alone.
	const budgetMB = 2800
	t.Logf("\nLargest B that fits a 4GB card (~%.1fGB usable for the filter):", float64(budgetMB)/1024)
	for _, r := range resolutions {
		maxS := float64(budgetMB) / perSecMB(r)
		t.Logf("  %-9s ~%4.1fs of Render Delay before VRAM is exhausted", r.name, maxS)
	}
}

func formatGB(mb float64) string {
	var s string
	if mb >= 1024 {
		s = fmt.Sprintf("%.1fGB", mb/1024)
	} else {
		s = fmt.Sprintf("%.0fMB", mb)
	}
	for len(s) < 8 {
		s = " " + s
	}
	return s
}
