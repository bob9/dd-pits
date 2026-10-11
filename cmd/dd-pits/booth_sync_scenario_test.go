package main

import (
	"fmt"
	"testing"
)

// TestBoothSyncLiveStreamScenarios drives the pure sync math with realistic
// per-call commentary generation latencies (script gen + multi-speaker TTS,
// measured event-moment → audio-ready) and reports the video delay B it settles
// on, plus how that B is realised as stacked OBS render-delay filters.
//
// Run: go test ./cmd/dd-pits/ -run LiveStream -v
func TestBoothSyncLiveStreamScenarios(t *testing.T) {
	type scenario struct {
		name    string
		floorMs int64   // operator -booth-min-delay / website delay_seconds (0 = default 12s)
		latMs   []int64 // per-call generation latency in ms, in the order calls complete
	}

	scenarios := []scenario{
		{
			name: "Paid Gemini key — fast, steady (typical club stream)",
			// script ~2-4s + TTS ~5-9s + queue; occasional slower call.
			latMs: []int64{9000, 11000, 8000, 10000, 12000, 9000, 14000, 10000, 11000, 9000},
		},
		{
			name:  "Paid key but a couple of slow calls (TTS spike)",
			latMs: []int64{10000, 12000, 24000, 11000, 9000, 28000, 10000, 12000},
		},
		{
			name: "Free-tier key — 3 RPM TTS pacing (~21s), lumpy",
			// segments paced ~21s for the free tier; latency stacks toward 30s+.
			latMs: []int64{18000, 26000, 31000, 22000, 34000, 25000, 29000, 33000},
		},
		{
			name:    "Paid key + operator -booth-min-delay 40s (your setup)",
			floorMs: 40000,
			latMs:   []int64{9000, 12000, 10000, 22000, 11000, 43000, 12000, 9000},
		},
	}

	for _, sc := range scenarios {
		s := newBoothSync()
		if sc.floorMs > 0 {
			s.setFloor(sc.floorMs)
		}
		t.Logf("\n=== %s ===", sc.name)
		t.Logf("start buffer B = %5.1fs (floor)", float64(s.buffer())/1000)
		var late int
		for i, lat := range sc.latMs {
			b := s.observe(lat)
			flag := ""
			if lat > b {
				late++
				flag = "  <-- AIRED LATE (latency > B before ratchet caught up)"
			}
			t.Logf("call %2d: latency %5.1fs -> B %5.1fs%s", i+1, float64(lat)/1000, float64(b)/1000, flag)
		}
		st := s.stats()
		final := s.buffer()
		filters := splitDelay(final, obsFilterCapMs)
		t.Logf("SETTLED: video delay B = %.1fs   (worst call %.1fs + %.0fs margin, rounded)",
			float64(final)/1000, float64(st.MaxLatMs)/1000, float64(boothSyncMarginMs)/1000)
		t.Logf("OBS: %s", describeFilters(filters))
		t.Logf("mic / other-app audio in OBS shifted by %.1fs to stay with the delayed video", float64(final)/1000)
		if late > 0 {
			t.Logf("NOTE: %d call(s) aired late before B grew to cover them (shown once on /booth status)", late)
		}
	}
}

func describeFilters(delays []int64) string {
	if len(delays) == 0 {
		return "no delay"
	}
	parts := make([]string, len(delays))
	for i, d := range delays {
		parts[i] = fmt.Sprintf("ddsync-%d=%.0fs", i, float64(d)/1000)
	}
	return fmt.Sprintf("%d stacked gpu_delay filter(s) per scene [%s] (20s cap each)",
		len(delays), joinComma(parts))
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
