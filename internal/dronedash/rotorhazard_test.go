package dronedash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The plugin's snapshot decodes into the same Snapshot an FPVTrackside read
// produces, with its UTC times left as they are.
func TestRHClientReadsPluginSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/dd-pits/api/version":
			_, _ = w.Write([]byte(`{"version":"3.abc"}`))
		case "/dd-pits/api/snapshot":
			_, _ = w.Write([]byte(`{
  "event": {"ID": "rh-1", "Name": "Club night", "Races": ["rh-h1-r1"], "Laps": 3},
  "pilots": [{"ID": "rh-p1", "Name": "Vortex"}],
  "channels": [{"ID": "rh-f5658", "DisplayName": "R1", "Frequency": 5658}],
  "rounds": [{"ID": "rh-c1-r1", "Name": "Open Round 1", "RoundNumber": 1, "Order": 1, "Valid": true}],
  "races": [{"ID": "rh-h1-r1", "Start": "2026-10-12T07:39:16.637Z", "End": "", "Valid": true,
    "RaceNumber": 1, "Round": "rh-c1-r1",
    "PilotChannels": [{"ID": "pc", "Pilot": "rh-p1", "Channel": "rh-f5658"}],
    "Detections": [{"ID": "d0", "Pilot": "rh-p1", "LapNumber": 0, "Valid": true, "Time": "2026-10-12T07:39:18.757Z"}],
    "Laps": [{"ID": "l0", "Detection": "d0", "LapNumber": 0, "LengthSeconds": 2.12,
      "StartTime": "2026-10-12T07:39:16.637Z", "EndTime": "2026-10-12T07:39:18.757Z"}]}],
  "results": [],
  "selected_race_id": "rh-h1-r1"
}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewRHClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.FetchVersion(context.Background())
	if err != nil || v != "3.abc" {
		t.Fatalf("version = %q, %v", v, err)
	}
	snap, err := c.FetchSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Event.ID != "rh-1" || snap.SelectedRaceID != "rh-h1-r1" || len(snap.Races) != 1 {
		t.Fatalf("snapshot mis-decoded: %+v", snap)
	}
	if got := snap.Races[0].Start; got != "2026-10-12T07:39:16.637Z" {
		t.Fatalf("UTC start rewritten to %q", got)
	}
	if !snap.HasRunningRace() {
		t.Fatal("a started, unended race should read as running")
	}
}

func TestRHClientNeedsPlugin(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c, _ := NewRHClient(srv.URL)
	if _, err := c.FetchVersion(context.Background()); err == nil {
		t.Fatal("a RotorHazard without the plugin must report an error")
	}
}
