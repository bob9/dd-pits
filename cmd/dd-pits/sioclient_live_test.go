package main

import (
	"os"
	"testing"
	"time"
)

// Against a real RotorHazard with the FPVTrackside connector (1.2.0):
//
//	DD_PITS_RH_URL=http://127.0.0.1:5050 DD_PITS_TS_RACE=… DD_PITS_TS_PILOT=… go test -run Connector
func TestConnectorLive(t *testing.T) {
	url := os.Getenv("DD_PITS_RH_URL")
	if url == "" {
		t.Skip("set DD_PITS_RH_URL to test against a real RotorHazard")
	}
	sio, err := dialSIO(url)
	if err != nil {
		t.Fatal(err)
	}
	defer sio.close()
	var info struct {
		PluginVersion string `json:"plugin_version"`
	}
	if err := sio.call("ts_server_info", map[string]any{}, &info, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Logf("connector version %q", info.PluginVersion)
	var w struct {
		Values []float64 `json:"history_values"`
		Times  []float64 `json:"history_times"`
		Enter  float64   `json:"enter_at"`
		Exit   float64   `json:"exit_at"`
		Start  float64   `json:"race_start_time"`
	}
	err = sio.call("ts_race_marshal_waveform", map[string]string{"race_id": os.Getenv("DD_PITS_TS_RACE"), "pilot_id": os.Getenv("DD_PITS_TS_PILOT")}, &w, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Values) < 100 || len(w.Times) != len(w.Values) || w.Start == 0 {
		t.Fatalf("bad trace: %d values, %d times, start %v", len(w.Values), len(w.Times), w.Start)
	}
	t.Logf("trace: %d samples, enter %v exit %v", len(w.Values), w.Enter, w.Exit)
}

func TestConnectorLiveSave(t *testing.T) {
	url := os.Getenv("DD_PITS_RH_URL")
	if url == "" || os.Getenv("DD_PITS_TS_SAVE") == "" {
		t.Skip("set DD_PITS_RH_URL and DD_PITS_TS_SAVE to save laps on a real RotorHazard")
	}
	c := &connectorTraces{base: url}
	laps := []savedLap{{StampMs: 2900}, {StampMs: 9200}, {StampMs: 12500, Deleted: true}, {StampMs: 15000}, {StampMs: 20900}}
	if err := c.replaceLaps(os.Getenv("DD_PITS_TS_RACE"), pilotTrace{Pilot: os.Getenv("DD_PITS_TS_PILOT")}, laps, 0, 0); err != nil {
		t.Fatal(err)
	}
}
