package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/stages/raceformat"
)

func TestStageTermsFollowFPVTrackside(t *testing.T) {
	cases := map[string]string{
		"Redemption Round 2 — Race 1": "Losers Round 2 — Race 1",
		"A Main — Round 1":            "Bracket A — Round 1",
		"Chase the Ace — Heat 3":      "Chase the Ace — Race 3",
		"Winners bracket":             "Winners bracket",
		"bracket progression":         "stage progression",
	}
	for in, want := range cases {
		if got := stageTerms(in); got != want {
			t.Errorf("stageTerms(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFinishOrderAppendsNonFinishersBySeed(t *testing.T) {
	tr := vmRace{Pilots: []vmPilot{
		{Name: "nova", CountedLaps: 4, Position: 1},
		{Name: "Blitz", CountedLaps: 3, Position: 2},
		{Name: "Hornet"}, // never crossed
		{Name: "Someone else", CountedLaps: 4},
	}}
	seeds := map[string]int{"Vortex": 1, "Nova": 2, "Blitz": 3, "Hornet": 4}
	got := finishOrder(tr, []string{"Vortex", "Nova", "Blitz", "Hornet"}, seeds)
	want := "Nova,Blitz,Vortex,Hornet"
	if strings.Join(got, ",") != want {
		t.Fatalf("finish order = %v, want %s", got, want)
	}
}

func TestReadyWaitsForEarlierRounds(t *testing.T) {
	all := []raceformat.RaceView{
		{ID: "W1-1", Round: 1, Pilots: []string{"a", "b"}, IsComplete: true},
		{ID: "W1-2", Round: 1, Pilots: []string{"c", "d"}},
		{ID: "W2-1", Round: 2, Pilots: []string{"a"}},
		{ID: "A:W1-1", Round: 1, Pilots: []string{"x"}},
	}
	if ready(all[2], all) {
		t.Fatal("W2-1 must wait while W1-2 is unflown")
	}
	if !ready(all[1], all) {
		t.Fatal("W1-2 should be ready")
	}
	if !ready(all[3], all) {
		t.Fatal("bracket A's first round does not wait on another bracket")
	}
}

// fakeTimer flies nothing; it hands out links like RotorHazard heats.
type fakeTimer struct{ sent []string }

func (f *fakeTimer) name() string                                 { return "Fake" }
func (f *fakeTimer) control(link, action string) (string, error)  { return "", nil }
func (f *fakeTimer) applySettings(stageRecord, []stageRace) error { return nil }
func (f *fakeTimer) removeRaces([]string) (int, error)            { return 0, nil }
func (f *fakeTimer) send(st stageRecord, races []stageRace, _ bool) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range races {
		f.sent = append(f.sent, r.ID+"="+strings.Join(r.Pilots, ","))
		out[r.ID] = "rh-h" + r.ID
	}
	return out, nil
}

// A stage driven only by results from the meeting: every race flown on the
// timer is picked up, the bracket advances, and nothing is sent twice.
func TestStageRunsFromTimingResults(t *testing.T) {
	timer := &fakeTimer{}
	m := newStageManager(filepath.Join(t.TempDir(), "stages.json"), timer)
	pilots := []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8"}
	st, err := m.create("Finals", raceformat.KindElimination, pilots,
		stageSetup{EliminationType: "double", FinalsFormat: "double-elimination", PilotsPerRace: 4, PilotsToAdvance: 2}, true)
	if err != nil {
		t.Fatal(err)
	}

	var flown []vmRace
	for step := 0; step < 20; step++ {
		rec, _ := m.get(st.ID)
		races, f, err := m.races(rec, &meetingView{Races: flown})
		if err != nil {
			t.Fatal(err)
		}
		if f.Champion() != "" {
			break
		}
		// Fly every race that is on the timer: seed order wins.
		progressed := false
		for _, r := range races {
			if r.State != "sent" {
				continue
			}
			vr := vmRace{ID: r.Link + "-r1", State: "ended", StartMs: int64(step + 1)}
			for i, p := range r.Pilots {
				vr.Pilots = append(vr.Pilots, vmPilot{Name: p, CountedLaps: 4, Position: i + 1})
			}
			flown = append(flown, vr)
			progressed = true
		}
		m.syncStage(st.ID, &meetingView{Races: flown}, false)
		if !progressed && step > 0 {
			rec, _ = m.get(st.ID)
			races, _, _ = m.races(rec, &meetingView{Races: flown})
			anySent := false
			for _, r := range races {
				anySent = anySent || r.State == "sent"
			}
			if !anySent {
				t.Fatalf("stage stalled at step %d: %+v", step, races)
			}
		}
	}
	rec, _ := m.get(st.ID)
	_, f, _ := m.races(rec, nil)
	if f.Champion() != "P1" {
		t.Fatalf("champion = %q, want the top seed winning every race", f.Champion())
	}
	// Races go on the timer early and are re-seated as pilots arrive, but
	// never re-sent with an unchanged line-up.
	seen := map[string]bool{}
	for _, sent := range timer.sent {
		if seen[sent] {
			t.Fatalf("%s was sent twice", sent)
		}
		seen[sent] = true
	}
	rec, _ = m.get(st.ID)
	if len(rec.Links) < 6 {
		t.Fatalf("every race should be on the timer: %v", rec.Links)
	}
	// The stage survives a restart.
	again := newStageManager(m.store.path, timer)
	if got, ok := again.get(st.ID); !ok || got.Name != "Finals" {
		t.Fatal("stage not reloaded from disk")
	}
}

func TestSeedingRanksByBestConsecutive(t *testing.T) {
	mv := &meetingView{Board: []vmStanding{
		{Name: "Slow", BestConsec: 40, Laps: 9},
		{Name: "NoTime", Laps: 0},
		{Name: "Fast", BestConsec: 31.2, Laps: 6},
	}}
	got := strings.Join(seedingFromMeeting(mv), ",")
	if got != "Fast,Slow,NoTime" {
		t.Fatalf("seeding = %s", got)
	}
}

// Every format the page offers can be created and has races to send at once.
func TestEveryFormatStarts(t *testing.T) {
	var pilots []string
	for i := 1; i <= 16; i++ {
		pilots = append(pilots, "P"+strings.Repeat("x", i%3)+string(rune('a'+i)))
	}
	for _, f := range stageFormats {
		timer := &fakeTimer{}
		m := newStageManager(filepath.Join(t.TempDir(), "stages.json"), timer)
		setup := stageSetup{PilotsPerRace: 4, PilotsToAdvance: 2, Rounds: 3, PilotsPerMain: 8, BumpUp: 4, Mains: 2}
		if _, err := m.create("T", f.Kind, pilots, setup, true); err != nil {
			t.Errorf("%s: %v", f.Kind, err)
			continue
		}
		if len(timer.sent) == 0 {
			t.Errorf("%s: no race was ready to send", f.Kind)
		}
	}
}

// The FPVTrackside sender speaks bob9's /api/v1/meeting/sync: rounds and races
// keyed by externalId, pilots by name, and the race ids come back as links.
func TestFTStageTimerMeetingSync(t *testing.T) {
	var got struct {
		Rounds []struct {
			ExternalID int    `json:"externalId"`
			Name       string `json:"name"`
			Races      []struct {
				ExternalID int `json:"externalId"`
				Pilots     []struct {
					Pilot string `json:"pilot"`
				} `json:"pilots"`
			} `json:"races"`
		} `json:"rounds"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/meeting/sync" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changed":2,"races":[
 {"externalId":70011001,"id":"AAAA-1","action":"created"},
 {"externalId":70011002,"id":"bbbb-2","action":"created"}]}`))
	}))
	defer srv.Close()

	st := stageRecord{Name: "Finals", Format: raceformat.KindElimination, Base: 70010000,
		Pilots: []string{"A", "B", "C", "D", "E", "F", "G", "H"},
		Setup:  stageSetup{PilotsPerRace: 4, PilotsToAdvance: 2, EliminationType: "double", FinalsFormat: "double-elimination"},
		Ext:    map[string]int{"W1-1": 70011001, "W1-2": 70011002}}
	f, err := raceformat.New(st.Format, st.config())
	if err != nil {
		t.Fatal(err)
	}
	st.State, _ = json.Marshal(f.State())
	var races []stageRace
	for _, rv := range f.Races() {
		if rv.Round == 1 {
			races = append(races, stageRace{RaceView: rv})
		}
	}
	links, err := (&ftStageTimer{base: srv.URL}).send(st, races, false)
	if err != nil {
		t.Fatal(err)
	}
	if links["W1-1"] != "aaaa-1" || links["W1-2"] != "bbbb-2" {
		t.Fatalf("links = %v", links)
	}
	if len(got.Rounds) != 1 || got.Rounds[0].ExternalID != 70010001 || len(got.Rounds[0].Races) != 2 {
		t.Fatalf("plan = %+v", got)
	}
	if !strings.HasPrefix(got.Rounds[0].Name, "Finals: Winners Round 1") {
		t.Fatalf("round name = %q", got.Rounds[0].Name)
	}
	if n := len(got.Rounds[0].Races[0].Pilots); n != 4 {
		t.Fatalf("race 1 seats %d pilots", n)
	}

	// Stock FPVTrackside: no API, and the error says what to do.
	stock := httptest.NewServer(http.NotFoundHandler())
	defer stock.Close()
	if _, err := (&ftStageTimer{base: stock.URL}).send(st, races, false); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("stock FPVTrackside error = %v", err)
	}
}
