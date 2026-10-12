package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseRaceTime(t *testing.T) {
	for in, want := range map[string]int64{"1:02.5": 62500, "62.5": 62500, "62500ms": 62500, " 0:03.25 ": 3250} {
		if got, err := parseRaceTime(in); err != nil || got != want {
			t.Errorf("parseRaceTime(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseRaceTime("soon"); err == nil {
		t.Error("nonsense must be refused")
	}
}

// FPVTrackside marshalling uses the REST API's crossing endpoints.
func TestFTMarshalRequests(t *testing.T) {
	type call struct {
		Method, Path string
		Body         map[string]any
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, call{r.Method, r.URL.Path, body})
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	ft := &ftStageTimer{base: srv.URL}
	if err := ft.marshal("race-1", "pilot-9", "invalidate", "det-3", 0); err != nil {
		t.Fatal(err)
	}
	if err := ft.marshal("race-1", "pilot-9", "add", "", 52300); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if c := calls[0]; c.Method != "PATCH" || c.Path != "/api/v1/races/race-1/detections/det-3" || c.Body["valid"] != false || c.Body["validityType"] != "Marshall" {
		t.Fatalf("invalidate sent %+v", c)
	}
	if c := calls[1]; c.Method != "POST" || c.Path != "/api/v1/races/race-1/laps" || c.Body["pilot"] != "pilot-9" || c.Body["raceTimeMs"] != float64(52300) {
		t.Fatalf("add sent %+v", c)
	}
}
