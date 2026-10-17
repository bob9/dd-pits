package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/dronedash"
)

// The meeting editor's side of fakeFT: the routes, fields and refusals of
// FPVTracksideCore's Webb/Api for events, rounds, races, line-ups and pilots.

var fakeChannels = []string{"R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8"}

func fakeChanID(label string) string { return "ch-" + label }

// chanRef reads a channel the way the API does: a label or an ID.
func chanRef(v string) string {
	for _, c := range fakeChannels {
		if v == c || v == fakeChanID(c) {
			return fakeChanID(c)
		}
	}
	return ""
}

func chanDto(id string) any {
	if id == "" {
		return nil
	}
	return map[string]any{"id": id, "label": strings.TrimPrefix(id, "ch-")}
}

func (f *fakeFT) editRoutes() []ftRoute {
	return []ftRoute{
		{"GET", "meeting", f.getMeeting},
		{"PUT", "event/channels", f.setChannels},
		{"POST", "rounds", f.createRound},
		{"PATCH", "rounds/{roundId}", f.patchRound},
		{"DELETE", "rounds/{roundId}", f.deleteRound},
		{"POST", "races", f.createRace},
		{"PATCH", "races/{raceId}", f.patchRace},
		{"PUT", "races/{raceId}/pilots", f.setLineup},
		{"POST", "pilots", f.createPilot},
		{"PATCH", "pilots/{pilotId}", f.patchPilot},
		{"DELETE", "pilots/{pilotId}", f.deletePilot},
	}
}

func (f *fakeFT) getMeeting(w http.ResponseWriter, _ map[string]string, _ []byte) {
	var chans []any
	for _, c := range fakeChannels {
		chans = append(chans, chanDto(fakeChanID(c)))
	}
	var pilots, rounds, races []any
	for _, p := range f.pilots {
		pilots = append(pilots, map[string]any{"id": p.ID, "name": p.Name, "firstName": p.FirstName,
			"lastName": p.LastName, "channel": chanDto(p.Channel)})
	}
	for i, rd := range f.rounds {
		n := 0
		for _, rc := range f.races {
			if rc.Round == rd.ID {
				n++
			}
		}
		rounds = append(rounds, map[string]any{"id": rd.ID, "number": i + 1, "name": rd.Name,
			"eventType": rd.EventType, "order": i * 100, "raceCount": n, "externalId": rd.ExternalID})
	}
	for _, rc := range f.races {
		ri := 0
		for i, rd := range f.rounds {
			if rd.ID == rc.Round {
				ri = i + 1
			}
		}
		state := "pending"
		if rc.running() {
			state = "running"
		} else if !rc.End.IsZero() {
			state = "finished"
		}
		var seats []any
		for _, pid := range rc.Pilots {
			name := ""
			for _, p := range f.pilots {
				if p.ID == pid {
					name = p.Name
				}
			}
			seats = append(seats, map[string]any{"pilotId": pid, "name": name, "channel": chanDto(rc.Chans[pid])})
		}
		races = append(races, map[string]any{"id": rc.ID, "roundId": rc.Round, "roundNumber": ri,
			"raceNumber": rc.Number, "name": fmt.Sprintf("Race %d-%d", ri, rc.Number), "eventType": "Race",
			"state": state, "isCurrent": f.cur == rc, "targetLaps": rc.TargetLaps, "externalId": rc.ExternalID,
			"pilots": seats})
	}
	writeJSON(w, map[string]any{
		"event": map[string]any{"id": f.event, "name": "Club day", "eventType": "Race", "laps": f.laps,
			"pbLaps": 2, "raceLengthSeconds": float64(f.length), "rulesLocked": false, "channels": chans},
		"pilots": pilots, "rounds": rounds, "races": races,
	})
}

func (f *fakeFT) setChannels(w http.ResponseWriter, _ map[string]string, body []byte) {
	m, err := fields(body, "channels")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	var list []string
	if json.Unmarshal(m["channels"], &list) != nil || len(list) == 0 {
		apiError(w, 400, "bad_request", "channels is required")
		return
	}
	for _, c := range list {
		if chanRef(c) == "" {
			apiError(w, 404, "not_found", "channel not found")
			return
		}
	}
	f.getMeeting(w, nil, nil)
}

func (f *fakeFT) roundRef(w http.ResponseWriter, id string) *ftRoundRec {
	for _, r := range f.rounds {
		if r.ID == id {
			return r
		}
	}
	apiError(w, 404, "not_found", "round not found")
	return nil
}

func (f *fakeFT) createRound(w http.ResponseWriter, _ map[string]string, body []byte) {
	m, err := fields(body, "eventType", "name", "externalId")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	rd := &ftRoundRec{ID: f.guid(), EventType: "Race"}
	_ = json.Unmarshal(m["name"], &rd.Name)
	_ = json.Unmarshal(m["eventType"], &rd.EventType)
	f.rounds = append(f.rounds, rd)
	w.WriteHeader(201)
	writeJSON(w, map[string]any{"id": rd.ID})
}

func (f *fakeFT) patchRound(w http.ResponseWriter, p map[string]string, body []byte) {
	rd := f.roundRef(w, p["roundId"])
	if rd == nil {
		return
	}
	m, err := fields(body, "name", "externalId")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	_ = json.Unmarshal(m["name"], &rd.Name)
	writeJSON(w, map[string]any{"id": rd.ID, "name": rd.Name})
}

func (f *fakeFT) deleteRound(w http.ResponseWriter, p map[string]string, _ []byte) {
	rd := f.roundRef(w, p["roundId"])
	if rd == nil {
		return
	}
	var keep []*ftRaceRec
	for _, rc := range f.races {
		if rc.Round == rd.ID && rc.running() {
			apiError(w, 409, "conflict", "a race in the round is running")
			return
		}
		if rc.Round != rd.ID {
			keep = append(keep, rc)
		}
	}
	f.races = keep
	for i, x := range f.rounds {
		if x == rd {
			f.rounds = append(f.rounds[:i], f.rounds[i+1:]...)
			break
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// seat reads a line-up the way FPVTrackside's Lineup does: a pilot listed
// twice or two pilots on one channel is refused.
func (f *fakeFT) seat(raw json.RawMessage) ([]string, map[string]string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("pilots must be a list")
	}
	var ids []string
	chans := map[string]string{}
	used := map[string]bool{}
	for i, it := range items {
		m, err := fields(it, "pilot", "channel")
		if err != nil {
			return nil, nil, err
		}
		var pilot, ch string
		_ = json.Unmarshal(m["pilot"], &pilot)
		_ = json.Unmarshal(m["channel"], &ch)
		if pilot == "" {
			return nil, nil, fmt.Errorf("pilot is required")
		}
		id := f.pilotByName(pilot)
		if contains(ids, id) {
			return nil, nil, fmt.Errorf("%s is listed twice", pilot)
		}
		c := chanRef(ch)
		if ch != "" && c == "" {
			return nil, nil, fmt.Errorf("channel not found")
		}
		if c == "" {
			c = fakeChanID(fakeChannels[i%len(fakeChannels)])
		}
		if used[c] {
			return nil, nil, fmt.Errorf("two pilots are on %s", strings.TrimPrefix(c, "ch-"))
		}
		used[c] = true
		ids = append(ids, id)
		chans[id] = c
	}
	return ids, chans, nil
}

func (f *fakeFT) createRace(w http.ResponseWriter, _ map[string]string, body []byte) {
	m, err := fields(body, "roundId", "eventType", "pilots", "makeCurrent", "externalId", "targetLaps")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	var roundID string
	_ = json.Unmarshal(m["roundId"], &roundID)
	rd := f.roundRef(w, roundID)
	if rd == nil {
		return
	}
	rc := &ftRaceRec{ID: f.guid(), Round: rd.ID, Chans: map[string]string{}}
	for _, x := range f.races {
		if x.Round == rd.ID && x.Number >= rc.Number {
			rc.Number = x.Number
		}
	}
	rc.Number++
	if raw, ok := m["pilots"]; ok {
		if rc.Pilots, rc.Chans, err = f.seat(raw); err != nil {
			apiError(w, 400, "bad_request", err.Error())
			return
		}
	}
	f.races = append(f.races, rc)
	w.WriteHeader(201)
	writeJSON(w, map[string]any{"id": rc.ID})
}

func (f *fakeFT) patchRace(w http.ResponseWriter, p map[string]string, body []byte) {
	rc := f.raceRef(w, p["raceId"])
	if rc == nil {
		return
	}
	m, err := fields(body, "externalId", "targetLaps")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	if rc.running() {
		apiError(w, 409, "conflict", "the race is running")
		return
	}
	_ = json.Unmarshal(m["targetLaps"], &rc.TargetLaps)
	writeJSON(w, map[string]any{"id": rc.ID})
}

func (f *fakeFT) setLineup(w http.ResponseWriter, p map[string]string, body []byte) {
	rc := f.raceRef(w, p["raceId"])
	if rc == nil {
		return
	}
	if rc.running() {
		apiError(w, 409, "conflict", "the race is running")
		return
	}
	ids, chans, err := f.seat(body)
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	rc.Pilots, rc.Chans = ids, chans
	writeJSON(w, map[string]any{"id": rc.ID})
}

func (f *fakeFT) pilotRef(w http.ResponseWriter, id string) *ftPilotRec {
	for _, p := range f.pilots {
		if p.ID == id {
			return p
		}
	}
	apiError(w, 404, "not_found", "pilot in the event not found")
	return nil
}

func (f *fakeFT) createPilot(w http.ResponseWriter, _ map[string]string, body []byte) {
	m, err := fields(body, "name", "channel", "firstName", "lastName", "externalId")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	p := &ftPilotRec{ID: f.guid()}
	_ = json.Unmarshal(m["name"], &p.Name)
	_ = json.Unmarshal(m["firstName"], &p.FirstName)
	_ = json.Unmarshal(m["lastName"], &p.LastName)
	var ch string
	_ = json.Unmarshal(m["channel"], &ch)
	p.Channel = chanRef(ch)
	f.pilots = append(f.pilots, p)
	w.WriteHeader(201)
	writeJSON(w, map[string]any{"id": p.ID})
}

func (f *fakeFT) patchPilot(w http.ResponseWriter, prm map[string]string, body []byte) {
	p := f.pilotRef(w, prm["pilotId"])
	if p == nil {
		return
	}
	m, err := fields(body, "name", "firstName", "lastName", "discordId", "aircraft", "catchPhrase",
		"practicePilot", "velocidroneUid", "channel", "externalId")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	_ = json.Unmarshal(m["name"], &p.Name)
	_ = json.Unmarshal(m["firstName"], &p.FirstName)
	_ = json.Unmarshal(m["lastName"], &p.LastName)
	if raw, ok := m["channel"]; ok {
		var ch string
		_ = json.Unmarshal(raw, &ch)
		p.Channel = chanRef(ch)
	}
	writeJSON(w, map[string]any{"id": p.ID})
}

func (f *fakeFT) deletePilot(w http.ResponseWriter, prm map[string]string, _ []byte) {
	p := f.pilotRef(w, prm["pilotId"])
	if p == nil {
		return
	}
	for _, rc := range f.races {
		if rc.running() && contains(rc.Pilots, p.ID) {
			apiError(w, 409, "conflict", "the pilot is in the running race")
			return
		}
	}
	for i, x := range f.pilots {
		if x == p {
			f.pilots = append(f.pilots[:i], f.pilots[i+1:]...)
			break
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// TestMeetingEditor drives /meeting against fakeFT: every change is a form
// post, as from the page, and must land in FPVTrackside.
func TestMeetingEditor(t *testing.T) {
	t.Setenv("FPV_API_TOKEN", "s3cret")
	ft := newFakeFT(t, "s3cret")
	srv := httptest.NewServer(ft)
	defer srv.Close()
	dir := t.TempDir()
	old := raceNames
	raceNames = loadRaceNames(filepath.Join(dir, "names.json"))
	defer func() { raceNames = old }()
	s := &liveServer{stages: newStageManager(filepath.Join(dir, "stages.json"), &ftStageTimer{base: srv.URL})}

	post := func(form url.Values) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/meeting", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "127.0.0.1:5000"
		w := httptest.NewRecorder()
		s.meetingPage(w, req)
		loc, _ := url.Parse(w.Header().Get("Location"))
		msg := loc.Query().Get("msg")
		if msg != "Saved." {
			t.Fatalf("%v: %s", form, msg)
		}
		return msg
	}
	page := func() string {
		req := httptest.NewRequest(http.MethodGet, "/meeting", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		w := httptest.NewRecorder()
		s.meetingPage(w, req)
		return w.Body.String()
	}

	post(url.Values{"do": {"pilot-add"}, "name": {"Wheelz"}, "channel": {"ch-R2"}})
	post(url.Values{"do": {"pilot-add"}, "name": {"Bob"}})
	post(url.Values{"do": {"pilot-add"}, "name": {"Zed"}})
	wheelz, bob, zed := ft.pilots[0].ID, ft.pilots[1].ID, ft.pilots[2].ID
	post(url.Values{"do": {"pilot-save"}, "pilot": {bob}, "name": {"Bobby"}, "firstName": {"Rob"}, "lastName": {"Jones"}, "channel": {"ch-R5"}})
	if p := ft.pilots[1]; p.Name != "Bobby" || p.FirstName != "Rob" || p.Channel != "ch-R5" {
		t.Fatalf("pilot not changed: %+v", p)
	}

	post(url.Values{"do": {"event"}, "name": {"Club day"}, "laps": {"5"}, "raceLength": {"2:30"}})
	if ft.laps != 5 || ft.length != 150 {
		t.Fatalf("event laps %d length %d", ft.laps, ft.length)
	}
	post(url.Values{"do": {"channels"}, "channels": {"R1 R2, R3"}})

	post(url.Values{"do": {"round-add"}, "name": {"Heats"}, "eventType": {"Race"}})
	round := ft.rounds[0].ID
	post(url.Values{"do": {"round-rename"}, "round": {round}, "name": {"Qualifying heats"}})
	if ft.rounds[0].Name != "Qualifying heats" {
		t.Fatalf("round = %+v", ft.rounds[0])
	}

	post(url.Values{"do": {"race-add"}, "round": {round},
		"seat-pilot-0": {wheelz}, "seat-chan-0": {"ch-R1"}, "seat-pilot-1": {bob}, "seat-pilot-2": {""}})
	if len(ft.races) != 1 || len(ft.races[0].Pilots) != 2 || ft.races[0].Chans[wheelz] != "ch-R1" {
		t.Fatalf("race = %+v", ft.races[0])
	}
	race := ft.races[0].ID

	// Name, laps and a new line-up in one save.
	post(url.Values{"do": {"race-save"}, "race": {race}, "name": {"Heat A"}, "targetLaps": {"4"}, "targetLapsWas": {"0"},
		"lineup": {"1"}, "lineupWas": {wheelz + "@ch-R1," + bob + "@ch-R2"},
		"seat-pilot-0": {zed}, "seat-chan-0": {"ch-R3"}, "seat-pilot-1": {wheelz}, "seat-chan-1": {"ch-R1"}})
	rc := ft.races[0]
	if rc.TargetLaps != 4 || len(rc.Pilots) != 2 || rc.Pilots[0] != zed || rc.Chans[zed] != "ch-R3" {
		t.Fatalf("race after save = %+v", rc)
	}
	if raceNames.get(race) != "Heat A" {
		t.Fatalf("race name = %q", raceNames.get(race))
	}
	if again := loadRaceNames(filepath.Join(dir, "names.json")); again.get(race) != "Heat A" {
		t.Fatal("race name not saved to disk")
	}

	// Saving only the name leaves FPVTrackside alone.
	ft.calls = nil
	post(url.Values{"do": {"race-save"}, "race": {race}, "name": {"Heat A"}, "targetLaps": {"4"}, "targetLapsWas": {"4"},
		"lineup": {"1"}, "lineupWas": {zed + "@ch-R3," + wheelz + "@ch-R1"},
		"seat-pilot-0": {zed}, "seat-chan-0": {"ch-R3"}, "seat-pilot-1": {wheelz}, "seat-chan-1": {"ch-R1"}})
	for _, c := range ft.calls {
		if !strings.HasPrefix(c, "GET") {
			t.Fatalf("an unchanged race sent %s", c)
		}
	}

	// A line-up FPVTrackside refuses is reported, not dropped.
	req := httptest.NewRequest(http.MethodPost, "/meeting", strings.NewReader(url.Values{"do": {"race-save"}, "race": {race}, "name": {"Renamed"},
		"lineup": {"1"}, "seat-pilot-0": {zed}, "seat-chan-0": {"ch-R1"}, "seat-pilot-1": {wheelz}, "seat-chan-1": {"ch-R1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	s.meetingPage(w, req)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "two+pilots+are+on+R1") {
		t.Fatalf("refusal lost: %s", loc)
	}
	if raceNames.get(race) != "Heat A" {
		t.Fatal("a refused save still renamed the race")
	}

	// The page shows it all, and the name reaches every dd-pits label.
	body := page()
	for _, want := range []string{"Qualifying heats", "Heat A", "Race 1-1", "Bobby", "Wheelz", `value="4"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("page is missing %q", want)
		}
	}
	client, err := dronedash.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := client.FetchSnapshot(context.Background(), ft.event)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, vr := range buildMeeting(snap).Races {
		if vr.ID == race {
			found = vr.Label == "Heat A"
		}
	}
	if !found {
		t.Fatal("dd-pits labels don't use the race name")
	}

	post(url.Values{"do": {"race-select"}, "race": {race}})
	if ft.cur == nil || ft.cur.ID != race {
		t.Fatal("race not selected")
	}
	post(url.Values{"do": {"race-delete"}, "race": {race}})
	if len(ft.races) != 0 || raceNames.get(race) != "" {
		t.Fatal("race not deleted")
	}
	post(url.Values{"do": {"pilot-remove"}, "pilot": {zed}})
	post(url.Values{"do": {"round-delete"}, "round": {round}})
	if len(ft.pilots) != 2 || len(ft.rounds) != 0 {
		t.Fatalf("pilots %d rounds %d", len(ft.pilots), len(ft.rounds))
	}
}

// TestMeetingEditorStockFPVTrackside: an FPVTrackside without the REST API
// says what to do instead of a bare 404.
func TestMeetingEditorStockFPVTrackside(t *testing.T) {
	stock := httptest.NewServer(http.NotFoundHandler())
	defer stock.Close()
	_, err := ftEditor{base: stock.URL}.meeting()
	if err == nil || !strings.Contains(err.Error(), "no REST API") {
		t.Fatalf("err = %v", err)
	}
}
