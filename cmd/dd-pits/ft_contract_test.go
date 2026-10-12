package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/stages/raceformat"
)

// fakeFT is bob9's FPVTrackside as far as dd-pits uses it: the REST API under
// /api/v1 (Webb/Api on feature/brackets) and the event JSON files the
// snapshot reads. It is strict where the real one would be: unknown paths are
// 404 "no such endpoint", a wrong method is 405, changes need the API token,
// and request fields must be the API's camelCase names.
type fakeFT struct {
	t      *testing.T
	token  string
	mu     sync.Mutex
	nextID int
	event  string
	laps   int
	length int
	pilots []*ftPilotRec
	rounds []*ftRoundRec
	races  []*ftRaceRec
	cur    *ftRaceRec
	calls  []string
}

type ftPilotRec struct{ ID, Name string }

type ftRoundRec struct {
	ID, Name, EventType string
	ExternalID          int
}

type ftDet struct {
	ID, Pilot, ValidityType string
	At                      time.Time
	Valid                   bool
}

type ftRaceRec struct {
	ID, Round          string
	ExternalID, Number int
	TargetLaps         int
	Pilots             []string // pilot ids
	Start, End         time.Time
	Dets               []*ftDet
}

func (r *ftRaceRec) running() bool { return !r.Start.IsZero() && r.End.IsZero() }

func newFakeFT(t *testing.T, token string) *fakeFT {
	f := &fakeFT{t: t, token: token, laps: 4}
	f.event = f.guid()
	return f
}

func (f *fakeFT) guid() string {
	f.nextID++
	return fmt.Sprintf("0000%04x-0000-4000-8000-%012x", f.nextID, f.nextID)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg, "code": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// fields decodes a JSON object, refusing any key the API doesn't have.
func fields(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the request body must be a JSON object: %v", err)
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range m {
		if !ok[k] {
			return nil, fmt.Errorf("unknown field %q", k)
		}
	}
	return m, nil
}

func (f *fakeFT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := strings.Trim(r.URL.Path, "/")
	if rest, ok := strings.CutPrefix(path, "api/v1/"); ok {
		f.calls = append(f.calls, r.Method+" /"+rest)
		f.api(w, r, strings.Split(rest, "/"), body)
		return
	}
	f.files(w, strings.Split(path, "/"))
}

type ftRoute struct {
	method, template string
	handle           func(w http.ResponseWriter, p map[string]string, body []byte)
}

func (f *fakeFT) api(w http.ResponseWriter, r *http.Request, seg []string, body []byte) {
	routes := []ftRoute{
		{"GET", "event", f.getEvent},
		{"PATCH", "event", f.patchEvent},
		{"POST", "meeting/sync", f.sync},
		{"DELETE", "races/{raceId}", f.deleteRace},
		{"POST", "races/{raceId}/reset", f.resetRace},
		{"POST", "races/{raceId}/laps", f.addLap},
		{"PATCH", "races/{raceId}/detections/{detectionId}", f.patchDetection},
		{"PUT", "race-control/current", f.setCurrent},
		{"POST", "race-control/start", f.start},
		{"POST", "race-control/stop", f.stop},
	}
	pathExists := false
	for _, rt := range routes {
		tseg := strings.Split(rt.template, "/")
		if len(tseg) != len(seg) {
			continue
		}
		p := map[string]string{}
		match := true
		for i, s := range tseg {
			if strings.HasPrefix(s, "{") {
				p[strings.Trim(s, "{}")] = seg[i]
			} else if s != seg[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		pathExists = true
		if rt.method != r.Method {
			continue
		}
		if rt.method != "GET" {
			if r.Header.Get("Authorization") != "Bearer "+f.token {
				apiError(w, 401, "unauthorized", "send the API token as 'Authorization: Bearer <token>'")
				return
			}
			if r.Header.Get("Content-Type") != "application/json" {
				f.t.Errorf("%s %s sent Content-Type %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
			}
		}
		rt.handle(w, p, body)
		return
	}
	if pathExists {
		apiError(w, 405, "method_not_allowed", r.Method+" is not supported here")
		return
	}
	apiError(w, 404, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path+" — see /api/v1/docs")
}

func (f *fakeFT) raceRef(w http.ResponseWriter, id string) *ftRaceRec {
	for _, r := range f.races {
		if r.ID == id {
			return r
		}
	}
	apiError(w, 404, "not_found", "race not found")
	return nil
}

func (f *fakeFT) getEvent(w http.ResponseWriter, _ map[string]string, _ []byte) {
	writeJSON(w, map[string]any{"id": f.event, "name": "Club day", "eventType": "Race", "laps": f.laps,
		"pbLaps": 2, "raceLengthSeconds": float64(f.length), "rulesLocked": false})
}

func (f *fakeFT) patchEvent(w http.ResponseWriter, _ map[string]string, body []byte) {
	m, err := fields(body, "name", "laps", "pbLaps", "raceLengthSeconds", "eventType", "minLapTimeSeconds", "packLimit", "start")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	if v, ok := m["laps"]; ok {
		_ = json.Unmarshal(v, &f.laps)
	}
	if v, ok := m["raceLengthSeconds"]; ok {
		_ = json.Unmarshal(v, &f.length)
	}
	f.getEvent(w, nil, nil)
}

func (f *fakeFT) sync(w http.ResponseWriter, _ map[string]string, body []byte) {
	bad := func(err error) { apiError(w, 400, "bad_request", err.Error()) }
	plan, err := fields(body, "pilots", "rounds", "removeMissing", "dryRun")
	if err != nil {
		bad(err)
		return
	}
	var rounds []json.RawMessage
	_ = json.Unmarshal(plan["rounds"], &rounds)
	type item struct {
		ExternalID int    `json:"externalId"`
		ID         string `json:"id"`
		Action     string `json:"action"`
		Message    string `json:"message,omitempty"`
	}
	var roundItems, raceItems []item
	for _, raw := range rounds {
		if _, err := fields(raw, "externalId", "eventType", "name", "races"); err != nil {
			bad(err)
			return
		}
		var pr struct {
			ExternalID int               `json:"externalId"`
			EventType  string            `json:"eventType"`
			Name       string            `json:"name"`
			Races      []json.RawMessage `json:"races"`
		}
		_ = json.Unmarshal(raw, &pr)
		if pr.ExternalID == 0 {
			bad(fmt.Errorf("every round needs a non-zero externalId"))
			return
		}
		switch pr.EventType {
		case "", "Race", "Practice", "TimeTrial", "Freestyle", "Endurance", "CasualPractice", "AggregateLaps", "Game":
		default:
			bad(fmt.Errorf("unknown eventType '%s'", pr.EventType))
			return
		}
		var round *ftRoundRec
		for _, x := range f.rounds {
			if x.ExternalID == pr.ExternalID {
				round = x
			}
		}
		action := "unchanged"
		if round == nil {
			round = &ftRoundRec{ID: f.guid(), ExternalID: pr.ExternalID, EventType: pr.EventType}
			f.rounds = append(f.rounds, round)
			action = "created"
		}
		if pr.Name != "" && pr.Name != round.Name {
			round.Name = pr.Name
			if action == "unchanged" {
				action = "updated"
			}
		}
		roundItems = append(roundItems, item{pr.ExternalID, round.ID, action, ""})
		for _, rr := range pr.Races {
			if _, err := fields(rr, "externalId", "pilots", "targetLaps"); err != nil {
				bad(err)
				return
			}
			var pc struct {
				ExternalID int               `json:"externalId"`
				Pilots     []json.RawMessage `json:"pilots"`
				TargetLaps *int              `json:"targetLaps"`
			}
			_ = json.Unmarshal(rr, &pc)
			if pc.ExternalID == 0 {
				bad(fmt.Errorf("every race needs a non-zero externalId"))
				return
			}
			var lineup []string
			for _, s := range pc.Pilots {
				if _, err := fields(s, "pilot", "channel"); err != nil {
					bad(err)
					return
				}
				var seat struct {
					Pilot string `json:"pilot"`
				}
				_ = json.Unmarshal(s, &seat)
				lineup = append(lineup, f.pilotByName(seat.Pilot))
			}
			var race *ftRaceRec
			for _, x := range f.races {
				if x.ExternalID == pc.ExternalID {
					race = x
				}
			}
			switch {
			case race != nil && !race.Start.IsZero():
				raceItems = append(raceItems, item{pc.ExternalID, race.ID, "locked", "already run; left as it is"})
				continue
			case race != nil && race.Round != round.ID:
				raceItems = append(raceItems, item{pc.ExternalID, race.ID, "conflict", "it is in another round"})
				continue
			case race == nil:
				n := 1
				for _, x := range f.races {
					if x.Round == round.ID {
						n++
					}
				}
				race = &ftRaceRec{ID: f.guid(), Round: round.ID, ExternalID: pc.ExternalID, Number: n}
				f.races = append(f.races, race)
				action = "created"
			default:
				action = "unchanged"
				if strings.Join(race.Pilots, ",") != strings.Join(lineup, ",") {
					action = "updated"
				}
			}
			race.Pilots = lineup
			if pc.TargetLaps != nil {
				race.TargetLaps = *pc.TargetLaps
			}
			raceItems = append(raceItems, item{pc.ExternalID, race.ID, action, ""})
		}
	}
	writeJSON(w, map[string]any{"dryRun": false, "changed": len(raceItems), "pilots": []item{},
		"rounds": roundItems, "races": raceItems, "warnings": []string{}})
}

// pilotByName finds or adds a pilot, as a seat naming one not in the event does.
func (f *fakeFT) pilotByName(name string) string {
	for _, p := range f.pilots {
		if p.ID == name || strings.EqualFold(p.Name, name) {
			return p.ID
		}
	}
	p := &ftPilotRec{ID: f.guid(), Name: name}
	f.pilots = append(f.pilots, p)
	return p.ID
}

func (f *fakeFT) deleteRace(w http.ResponseWriter, p map[string]string, _ []byte) {
	race := f.raceRef(w, p["raceId"])
	if race == nil {
		return
	}
	if race.running() {
		apiError(w, 409, "conflict", "the race is running")
		return
	}
	for i, x := range f.races {
		if x == race {
			f.races = append(f.races[:i], f.races[i+1:]...)
			break
		}
	}
	writeJSON(w, map[string]any{"ok": true, "message": "race deleted"})
}

func (f *fakeFT) resetRace(w http.ResponseWriter, p map[string]string, body []byte) {
	race := f.raceRef(w, p["raceId"])
	if race == nil {
		return
	}
	if race.running() {
		apiError(w, 409, "conflict", "the race is running")
		return
	}
	race.Start, race.End, race.Dets = time.Time{}, time.Time{}, nil
	writeJSON(w, map[string]any{"id": race.ID})
}

func (f *fakeFT) addLap(w http.ResponseWriter, p map[string]string, body []byte) {
	race := f.raceRef(w, p["raceId"])
	if race == nil {
		return
	}
	m, err := fields(body, "pilot", "time", "raceTimeMs", "anchorDetectionId", "offsetMs")
	if err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	var req struct {
		Pilot      string `json:"pilot"`
		RaceTimeMs *int64 `json:"raceTimeMs"`
	}
	_ = json.Unmarshal(body, &req)
	_, hasTime := m["time"]
	_, hasAnchor := m["anchorDetectionId"]
	if (req.RaceTimeMs != nil) == (hasTime || hasAnchor) || hasTime && hasAnchor {
		apiError(w, 400, "bad_request", "give exactly one of time, raceTimeMs, or anchorDetectionId + offsetMs")
		return
	}
	if req.RaceTimeMs == nil {
		f.t.Errorf("dd-pits adds laps by raceTimeMs")
		apiError(w, 400, "bad_request", "unsupported in the fake")
		return
	}
	if race.Start.IsZero() {
		apiError(w, 409, "conflict", "the race hasn't started, so raceTimeMs has nothing to count from")
		return
	}
	if !contains(race.Pilots, req.Pilot) {
		apiError(w, 400, "bad_request", req.Pilot+" is not in that race")
		return
	}
	race.Dets = append(race.Dets, &ftDet{ID: f.guid(), Pilot: req.Pilot, Valid: true, ValidityType: "ManualOverride",
		At: race.Start.Add(time.Duration(*req.RaceTimeMs) * time.Millisecond)})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"raceId": race.ID, "pilotId": req.Pilot})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeFT) patchDetection(w http.ResponseWriter, p map[string]string, body []byte) {
	race := f.raceRef(w, p["raceId"])
	if race == nil {
		return
	}
	if _, err := fields(body, "valid", "validityType"); err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	var req struct {
		Valid        *bool  `json:"valid"`
		ValidityType string `json:"validityType"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Valid == nil {
		apiError(w, 400, "bad_request", "valid is required")
		return
	}
	if req.ValidityType != "" && req.ValidityType != "ManualOverride" && req.ValidityType != "Marshall" {
		apiError(w, 400, "bad_request", "validityType must be ManualOverride or Marshall")
		return
	}
	for _, d := range race.Dets {
		if d.ID == p["detectionId"] {
			d.Valid, d.ValidityType = *req.Valid, req.ValidityType
			writeJSON(w, map[string]any{"raceId": race.ID, "pilotId": d.Pilot})
			return
		}
	}
	apiError(w, 404, "not_found", "detection not found")
}

func (f *fakeFT) setCurrent(w http.ResponseWriter, _ map[string]string, body []byte) {
	if _, err := fields(body, "raceId"); err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	var req struct {
		RaceID *string `json:"raceId"`
	}
	_ = json.Unmarshal(body, &req)
	if f.cur != nil && f.cur.running() {
		apiError(w, 409, "conflict", "the selected race is running")
		return
	}
	if req.RaceID == nil {
		f.cur = nil
		writeJSON(w, map[string]any{"state": "none"})
		return
	}
	if race := f.raceRef(w, *req.RaceID); race != nil {
		f.cur = race
		writeJSON(w, map[string]any{"state": "ready"})
	}
}

func (f *fakeFT) start(w http.ResponseWriter, _ map[string]string, body []byte) {
	if _, err := fields(body, "videoCheck"); err != nil {
		apiError(w, 400, "bad_request", err.Error())
		return
	}
	switch {
	case f.cur == nil:
		apiError(w, 409, "conflict", "no race is selected")
	case f.cur.running():
		apiError(w, 409, "conflict", "the race is already running")
	case !f.cur.End.IsZero():
		apiError(w, 409, "conflict", "the selected race has already been run; reset it first")
	default:
		// As RaceManager.CommitRaceStart: the race takes the event's laps.
		f.cur.Start, f.cur.TargetLaps = time.Now(), f.laps
		writeJSON(w, map[string]any{"state": "running"})
	}
}

func (f *fakeFT) stop(w http.ResponseWriter, _ map[string]string, _ []byte) {
	if f.cur == nil || !f.cur.running() {
		apiError(w, 409, "conflict", "no race is running")
		return
	}
	f.cur.End = time.Now()
	writeJSON(w, map[string]any{"state": "finished"})
}

// files serves the event JSON the snapshot reads, in FPVTrackside's own
// PascalCase and venue-local times.
func (f *fakeFT) files(w http.ResponseWriter, seg []string) {
	ts := func(t time.Time) string {
		if t.IsZero() {
			return "0001-01-01T00:00:00"
		}
		return t.Local().Format("2006-01-02T15:04:05.000")
	}
	switch {
	case len(seg) == 1 && seg[0] == "current":
		var cur any
		if f.cur != nil {
			cur = f.cur.ID
		}
		writeJSON(w, map[string]any{"CurrentRace": cur})
	case len(seg) == 2 && seg[0] == "httpfiles" && seg[1] == "Channels.json":
		writeJSON(w, []any{})
	case len(seg) == 3 && seg[0] == "events" && seg[1] == f.event:
		switch seg[2] {
		case "Event.json":
			var rounds, races []string
			for _, r := range f.rounds {
				rounds = append(rounds, r.ID)
			}
			for _, r := range f.races {
				races = append(races, r.ID)
			}
			writeJSON(w, []any{map[string]any{"ID": f.event, "Name": "Club day", "EventType": "Race",
				"Laps": f.laps, "PBLaps": 2, "Rounds": rounds, "Races": races}})
		case "Pilots.json":
			var out []any
			for _, p := range f.pilots {
				out = append(out, map[string]any{"ID": p.ID, "Name": p.Name})
			}
			writeJSON(w, out)
		case "Rounds.json":
			var out []any
			for i, r := range f.rounds {
				out = append(out, map[string]any{"ID": r.ID, "Name": r.Name, "RoundNumber": i + 1,
					"EventType": r.EventType, "Valid": true, "Order": i * 100})
			}
			writeJSON(w, out)
		default:
			http.NotFound(w, nil)
		}
	case len(seg) == 4 && seg[0] == "events" && seg[1] == f.event:
		var race *ftRaceRec
		for _, r := range f.races {
			if r.ID == seg[2] {
				race = r
			}
		}
		if race == nil {
			http.NotFound(w, nil)
			return
		}
		if seg[3] == "Result.json" {
			writeJSON(w, []any{})
			return
		}
		var pcs, dets, laps []any
		for _, p := range race.Pilots {
			pcs = append(pcs, map[string]any{"ID": p + "-pc", "Pilot": p, "Channel": ""})
			var mine []*ftDet
			for _, d := range race.Dets {
				if d.Pilot == p {
					mine = append(mine, d)
				}
			}
			sort.Slice(mine, func(i, j int) bool { return mine[i].At.Before(mine[j].At) })
			prev, n := race.Start, 0
			for _, d := range mine {
				dets = append(dets, map[string]any{"ID": d.ID, "Pilot": p, "Time": ts(d.At), "Valid": d.Valid,
					"ValidityType": d.ValidityType, "IsLapEnd": true, "LapNumber": n + 1})
				if !d.Valid {
					continue
				}
				n++
				laps = append(laps, map[string]any{"ID": d.ID + "-lap", "Detection": d.ID, "LapNumber": n,
					"LengthSeconds": d.At.Sub(prev).Seconds(), "StartTime": ts(prev), "EndTime": ts(d.At)})
				prev = d.At
			}
		}
		writeJSON(w, []any{map[string]any{"ID": race.ID, "Round": race.Round, "RaceNumber": race.Number,
			"Start": ts(race.Start), "End": ts(race.End), "TargetLaps": race.TargetLaps, "Valid": true,
			"ExternalID": race.ExternalID, "PilotChannels": pcs, "Detections": dets, "Laps": laps}})
	default:
		http.NotFound(w, nil)
	}
}

// A bracket run against FPVTrackside from start to finish: created and sent
// through meeting sync, flown with race control, results read from the
// timer's files, marshalled, and deleted.
func TestFPVTracksideBracketEndToEnd(t *testing.T) {
	t.Setenv("FPV_API_TOKEN", "s3cret")
	ft := newFakeFT(t, "s3cret")
	srv := httptest.NewServer(ft)
	defer srv.Close()
	timer := &ftStageTimer{base: srv.URL}
	client, err := dronedash.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	m := newStageManager(filepath.Join(t.TempDir(), "stages.json"), timer)
	meeting := func() *meetingView {
		snap, err := client.FetchSnapshot(context.Background(), ft.event)
		if err != nil {
			t.Fatal(err)
		}
		m.observe(snap)
		return buildMeeting(snap)
	}
	w11Of := func(stageID string) stageRace {
		rec, _ := m.get(stageID)
		races, _, err := m.races(rec, meeting())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range races {
			if r.ID == "W1-1" {
				return r
			}
		}
		t.Fatal("no W1-1")
		return stageRace{}
	}

	// Create: every race goes to FPVTrackside, in rounds named for the stage.
	pilots := []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8"}
	st, err := m.create("Finals", raceformat.KindElimination, pilots, stageSetup{EliminationType: "double",
		FinalsFormat: "double-elimination", PilotsPerRace: 4, PilotsToAdvance: 2, Laps: 3, RaceSeconds: 150}, true)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := m.get(st.ID)
	if rec.LastError != "" {
		t.Fatalf("send failed: %s", rec.LastError)
	}
	if len(ft.races) == 0 || len(ft.races) != len(rec.Links) {
		t.Fatalf("FPVTrackside has %d races, the bracket links %d", len(ft.races), len(rec.Links))
	}
	for id, link := range rec.Links {
		if r := ft.raceRefQuiet(link); r == nil || r.ExternalID != rec.Ext[id] || r.TargetLaps != 3 {
			t.Fatalf("%s -> %s isn't the race FPVTrackside made: %+v", id, link, r)
		}
	}
	if !strings.HasPrefix(ft.rounds[0].Name, "Finals: Winners Round 1") || ft.rounds[0].EventType != "Race" {
		t.Fatalf("round 1 = %+v", ft.rounds[0])
	}

	// Fly W1-1 from dd-pits: the event takes the bracket's laps and race
	// time (FPVTrackside starts a race with the event's), then the race is
	// selected and started.
	ft.calls = nil
	if err := m.raceControl(st.ID, "W1-1", "start", meeting()); err != nil {
		t.Fatal(err)
	}
	want := "GET /event,PATCH /event,PUT /race-control/current,POST /race-control/start"
	if got := strings.Join(ft.calls, ","); got != want {
		t.Fatalf("start called %s, want %s", got, want)
	}
	if ft.laps != 3 || ft.length != 150 {
		t.Fatalf("event laps/time = %d/%d", ft.laps, ft.length)
	}
	w11 := ft.raceRefQuiet(rec.Links["W1-1"])
	if w11 != ft.cur || !w11.running() || w11.TargetLaps != 3 {
		t.Fatalf("W1-1 isn't running: %+v", w11)
	}
	// Seats in line-up order cross the line 3, 3, 2 and 1 times; the first
	// is fastest.
	byName := map[string]string{}
	for _, p := range ft.pilots {
		byName[p.ID] = p.Name
	}
	order := w11Of(st.ID).Pilots
	for i, pid := range w11.Pilots {
		for lap := 1; lap <= 3-i/2-i/3; lap++ {
			ft.mu.Lock()
			w11.Dets = append(w11.Dets, &ftDet{ID: ft.guid(), Pilot: pid, Valid: true,
				At: w11.Start.Add(time.Duration(lap*10+i) * time.Second)})
			ft.mu.Unlock()
		}
	}
	if err := m.raceControl(st.ID, "W1-1", "stop", meeting()); err != nil {
		t.Fatal(err)
	}

	// Results come from the snapshot of FPVTrackside's files.
	meeting()
	m.syncAll(false)
	r := w11Of(st.ID)
	if !r.IsComplete || r.State != "finished" {
		t.Fatalf("W1-1 not taken from timing: %+v", r)
	}
	rec, _ = m.get(st.ID)
	if got := rec.Applied["W1-1"]; !strings.HasSuffix(got, "|"+strings.Join(order, ",")) {
		t.Fatalf("W1-1 result = %q, want %v", got, order)
	}

	// Marshal: take the winner's last crossing away and the second seat wins.
	var last *ftDet
	for _, d := range w11.Dets {
		if d.Pilot == w11.Pilots[0] {
			last = d
		}
	}
	if err := timer.marshal(w11.ID, w11.Pilots[0], "invalidate", last.ID, 0); err != nil {
		t.Fatal(err)
	}
	if last.Valid || last.ValidityType != "Marshall" {
		t.Fatalf("crossing not invalidated: %+v", last)
	}
	if err := timer.marshal(w11.ID, w11.Pilots[3], "add", "", 25000); err != nil {
		t.Fatal(err)
	}
	if n := len(w11.Dets); w11.Dets[n-1].Pilot != w11.Pilots[3] || w11.Dets[n-1].At != w11.Start.Add(25*time.Second) {
		t.Fatalf("lap not added: %+v", w11.Dets[n-1])
	}
	meeting()
	m.syncAll(false)
	rec, _ = m.get(st.ID)
	if got := rec.Applied["W1-1"]; !strings.Contains(got, "|"+byName[w11.Pilots[1]]+",") {
		t.Fatalf("marshalled result = %q, want %s first", got, byName[w11.Pilots[1]])
	}
	// FPVTrackside's "not found" is its message, not "no REST API".
	if err := timer.marshal("00000000-0000-0000-0000-000000000000", w11.Pilots[0], "add", "", 1000); err == nil || !strings.Contains(err.Error(), "race not found") {
		t.Fatalf("unknown race error = %v", err)
	}

	// Restart the flown race: reset, selected and started again.
	ft.calls = nil
	if err := m.raceControl(st.ID, "W1-1", "restart", meeting()); err != nil {
		t.Fatal(err)
	}
	want = "GET /event,POST /race-control/stop,POST /races/" + w11.ID + "/reset,PUT /race-control/current,POST /race-control/start"
	if got := strings.Join(ft.calls, ","); got != want {
		t.Fatalf("restart called %s, want %s", got, want)
	}
	if !w11.running() || len(w11.Dets) != 0 {
		t.Fatalf("W1-1 not re-run: %+v", w11)
	}
	if err := m.raceControl(st.ID, "W1-1", "stop", meeting()); err != nil {
		t.Fatal(err)
	}

	// Deleting the bracket takes every race not flown off FPVTrackside and
	// leaves the flown one.
	removed, err := m.deleteWithRaces(st.ID, meeting())
	if err != nil {
		t.Fatal(err)
	}
	if removed != len(rec.Links)-1 || len(ft.races) != 1 || ft.races[0] != w11 {
		t.Fatalf("removed %d of %d; FPVTrackside keeps %d races", removed, len(rec.Links), len(ft.races))
	}
}

func (f *fakeFT) raceRefQuiet(id string) *ftRaceRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.races {
		if r.ID == id {
			return r
		}
	}
	return nil
}
