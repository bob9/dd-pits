package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bob9/dd-pits/internal/marshal"
)

// FPVTrackside's TimingSystemSettings.xml, as it saves a RotorHazard timer.
func TestRotorHazardInFTSettings(t *testing.T) {
	f := filepath.Join(t.TempDir(), "TimingSystemSettings.xml")
	os.WriteFile(f, []byte(`<?xml version="1.0" encoding="utf-8"?>
<ArrayOfTimingSystemSettings xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <TimingSystemSettings xsi:type="DummySettings"><Role>Primary</Role></TimingSystemSettings>
  <TimingSystemSettings xsi:type="RotorHazardSettings"><Role>Primary</Role><HostName>10.1.1.207</HostName><Port>5000</Port></TimingSystemSettings>
</ArrayOfTimingSystemSettings>`), 0o644)
	if got := rotorHazardInSettings(f); got != "http://10.1.1.207:5000" {
		t.Fatalf("got %q", got)
	}
	os.WriteFile(f, []byte(`<ArrayOfTimingSystemSettings xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" />`), 0o644)
	if got := rotorHazardInSettings(f); got != "" {
		t.Fatalf("no RotorHazard, got %q", got)
	}
}

// The race director's edits: a removed crossing is marked, an added lap
// takes its strength from the trace and isn't second-guessed.
func TestApplyEdits(t *testing.T) {
	tr := marshal.Trace{Start: 10, Times: []float64{10, 12.9, 13, 13.1, 20}, Values: []float64{50, 80, 96, 81, 50}}
	cs := []marshal.Crossing{{StampMs: 1000, Peak: 120}, {StampMs: 6000, Peak: 118}}
	out := applyEdits(tr, cs, []float64{6020}, []float64{3000})
	if len(out) != 3 || out[1].StampMs != 3000 || !out[1].Manual || out[1].Peak != 96 {
		t.Fatalf("added lap wrong: %+v", out)
	}
	if !out[2].Deleted || out[0].Deleted {
		t.Fatalf("removal wrong: %+v", out)
	}
}

// The plugin's marshal-traces answer decodes into what the page needs.
func TestPluginTracesDecode(t *testing.T) {
	raw := `{"race":"rh-h1-r1","start_time":152960.6,"rules":{"min_lap_ms":3000,"min_lap_discard":false,
"min_first_crossing_ms":0,"start_lower_pct":0,"start_lower_secs":0,"start_delay_max":2},
"pilots":[{"pilot":"rh-p5","ts_pilot_id":"","callsign":"Blitz","node":0,"frequency":5658,"enter_at":90,"exit_at":80,
"history_times":[152960.6,152961.6],"history_values":[50,130],"laps":[{"id":7,"lap_time_stamp":2000,"lap_time":2000,"source":0,"deleted":false,"peak_rssi":130}]}]}`
	var rt raceTraces
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		t.Fatal(err)
	}
	p := rt.Pilots[0]
	if p.Name != "Blitz" || p.Enter != 90 || len(p.Values) != 2 || p.Laps[0].StampMs != 2000 || rt.Rules.MinLapMs != 3000 {
		t.Fatalf("decoded wrong: %+v", rt)
	}
}
