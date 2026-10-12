package main

// Sending stage races to the timer.
//
// RotorHazard: the dd-pits plugin creates each race as a heat in a race class
// named after the stage (POST /dd-pits/api/stage-heat), seating the pilots in
// order. The link is "rh-h<heat>", and the heat's first run is the result.
//
// FPVTrackside: the REST API in bob9's FPVTrackside (POST /api/v1/meeting/sync)
// creates the races in rounds named after the stage's rounds, keyed by
// externalId so a resend changes nothing. Stock FPVTrackside has no way to
// create races from outside, so there the race director adds them by hand and
// enters the results on the stage page.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type stageTimer interface {
	name() string
	// send creates races on the timer and returns stage race id -> link.
	send(st stageRecord, races []stageRace, makeCurrent bool) (map[string]string, error)
	// control starts, stops ("stop" saves the result), restarts or discards
	// the race behind a link.
	control(link, action string) error
	// applySettings brings the laps and race time to races already on the
	// timer and not yet flown.
	applySettings(st stageRecord, sent []stageRace) error
}

var stageHTTP = &http.Client{Timeout: 20 * time.Second}

func postJSON(url string, body any, token string, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := stageHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		return errNoStageAPI
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

var errNoStageAPI = errors.New("this timer can't take races from dd-pits")

// ── RotorHazard ──────────────────────────────────────────────────────────────

type rhStageTimer struct {
	base  string // RotorHazard server
	token string // shared with the plugin, which started dd-pits
}

func (t *rhStageTimer) name() string { return "RotorHazard" }

func (t *rhStageTimer) send(st stageRecord, races []stageRace, makeCurrent bool) (map[string]string, error) {
	links := map[string]string{}
	for i, r := range races {
		var out struct {
			Heat int `json:"heat"`
		}
		heat := 0
		if l := st.Links[r.ID]; strings.HasPrefix(l, "rh-h") {
			fmt.Sscanf(l, "rh-h%d", &heat)
		}
		err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/stage-heat", map[string]any{
			"class":     st.Name,
			"name":      r.Title,
			"pilots":    r.Pilots,
			"heat":      heat,
			"current":   makeCurrent && i == 0,
			"laps":      st.Setup.Laps,
			"race_time": st.Setup.RaceSeconds,
		}, t.token, &out)
		if errors.Is(err, errNoStageAPI) {
			return links, errors.New("the dd-pits RotorHazard plugin is out of date; update it to send races")
		}
		if err != nil {
			return links, err
		}
		links[r.ID] = fmt.Sprintf("rh-h%d", out.Heat)
	}
	return links, nil
}

func (t *rhStageTimer) control(link, action string) error {
	heat := 0
	if _, err := fmt.Sscanf(link, "rh-h%d", &heat); err != nil || heat == 0 {
		return errors.New("that race isn't on RotorHazard yet")
	}
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/race-control",
		map[string]any{"action": action, "heat": heat}, t.token, nil)
	if errors.Is(err, errNoStageAPI) {
		return errors.New("the dd-pits RotorHazard plugin is out of date; update it to start races")
	}
	return err
}

// applySettings sets the bracket's race class format on RotorHazard; every
// heat in the class runs with it, so nothing per heat is needed.
func (t *rhStageTimer) applySettings(st stageRecord, _ []stageRace) error {
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/stage-format", map[string]any{
		"class": st.Name, "laps": st.Setup.Laps, "race_time": st.Setup.RaceSeconds,
	}, t.token, nil)
	if errors.Is(err, errNoStageAPI) {
		return errors.New("the dd-pits RotorHazard plugin is out of date; update it to set laps and race time")
	}
	return err
}

// ── FPVTrackside ─────────────────────────────────────────────────────────────

type ftStageTimer struct {
	base string // FPVTrackside web server
}

func (t *ftStageTimer) name() string { return "FPVTrackside" }

func (t *ftStageTimer) send(st stageRecord, races []stageRace, makeCurrent bool) (map[string]string, error) {
	type seat struct {
		Pilot string `json:"pilot"`
	}
	type planRace struct {
		ExternalID int    `json:"externalId"`
		Pilots     []seat `json:"pilots"`
		TargetLaps int    `json:"targetLaps,omitempty"`
	}
	type planRound struct {
		ExternalID int        `json:"externalId"`
		EventType  string     `json:"eventType"`
		Name       string     `json:"name"`
		Races      []planRace `json:"races"`
	}
	rounds := map[int]*planRound{}
	for _, r := range races {
		pr := rounds[r.Round]
		if pr == nil {
			pr = &planRound{ExternalID: st.Base + r.Round, EventType: "Race", Name: stageRoundName(st, r.Round)}
			rounds[r.Round] = pr
		}
		var seats []seat
		for _, p := range r.Pilots {
			seats = append(seats, seat{Pilot: p})
		}
		pr.Races = append(pr.Races, planRace{ExternalID: st.Ext[r.ID], Pilots: seats, TargetLaps: st.Setup.Laps})
	}
	var nums []int
	for n := range rounds {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	plan := struct {
		Rounds []planRound `json:"rounds"`
	}{}
	for _, n := range nums {
		plan.Rounds = append(plan.Rounds, *rounds[n])
	}
	var out struct {
		Races []struct {
			ExternalID int    `json:"externalId"`
			ID         string `json:"id"`
			Action     string `json:"action"`
			Message    string `json:"message"`
		} `json:"races"`
	}
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/api/v1/meeting/sync", plan, os.Getenv("FPV_API_TOKEN"), &out)
	if errors.Is(err, errNoStageAPI) {
		return nil, errors.New("this FPVTrackside has no REST API. Use bob9's FPVTrackside " +
			"(github.com/bob9/FPVTracksideCore), or add the races by hand and enter results here")
	}
	if err != nil {
		return nil, err
	}
	byExt := map[int]string{}
	for _, r := range races {
		byExt[st.Ext[r.ID]] = r.ID
	}
	links := map[string]string{}
	var problems []string
	for _, r := range out.Races {
		id := byExt[r.ExternalID]
		if id == "" {
			continue
		}
		if r.Action == "conflict" {
			problems = append(problems, r.Message)
			continue
		}
		if r.ID != "" {
			links[id] = strings.ToLower(r.ID)
		}
	}
	if makeCurrent && len(races) == 1 {
		if l := links[races[0].ID]; l != "" {
			_ = putJSON(strings.TrimSuffix(t.base, "/")+"/api/v1/race-control/current", map[string]string{"raceId": l})
		}
	}
	if len(problems) > 0 {
		return links, errors.New(strings.Join(problems, "; "))
	}
	return links, nil
}

// applySettings re-sends the races not yet flown: meeting sync updates a
// pending race's target laps and leaves flown ones alone. FPVTrackside's race
// time belongs to the whole event, so it isn't set per bracket.
func (t *ftStageTimer) applySettings(st stageRecord, sent []stageRace) error {
	if len(sent) == 0 {
		return nil
	}
	_, err := t.send(st, sent, false)
	return err
}

// control drives FPVTrackside's race-director screen through its REST API.
// FPVTrackside must have "HTTP Server race controls enabled".
func (t *ftStageTimer) control(link, action string) error {
	api := strings.TrimSuffix(t.base, "/") + "/api/v1"
	tok := os.Getenv("FPV_API_TOKEN")
	call := func(path string, body any) error {
		err := postJSON(api+path, body, tok, nil)
		if errors.Is(err, errNoStageAPI) {
			return errors.New("this FPVTrackside has no REST API; start races in FPVTrackside")
		}
		return err
	}
	selectRace := func() error {
		return putJSON(api+"/race-control/current", map[string]string{"raceId": link})
	}
	switch action {
	case "start":
		if err := selectRace(); err != nil {
			return err
		}
		return call("/race-control/start", map[string]any{})
	case "stop":
		return call("/race-control/stop", map[string]any{})
	case "discard", "restart":
		_ = call("/race-control/stop", map[string]any{}) // nothing running is fine
		if err := call("/races/"+link+"/reset", map[string]any{}); err != nil {
			return err
		}
		if action == "discard" {
			return nil
		}
		if err := selectRace(); err != nil {
			return err
		}
		return call("/race-control/start", map[string]any{})
	}
	return fmt.Errorf("unknown action %q", action)
}

func putJSON(url string, body any) error {
	req, err := newJSONRequest(http.MethodPut, url, body)
	if err != nil {
		return err
	}
	resp, err := stageHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readAPIError(resp)
	}
	return nil
}

// ftToken is FPVTrackside's API token, needed only from another machine.
func ftToken() string { return os.Getenv("FPV_API_TOKEN") }

func newJSONRequest(method, url string, body any) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := ftToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req, nil
}

// readAPIError turns an error reply into the message it carries.
func readAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

// stageRoundName names a round from what is in it, the way FPVTrackside's own
// stages do: "Winners Round 2 · Losers Round 1", "Chase the Ace", "Final".
func stageRoundName(st stageRecord, round int) string {
	f, err := st.engine()
	if err != nil {
		return fmt.Sprintf("%s Round %d", st.Name, round)
	}
	var parts []string
	seen := map[string]bool{}
	for _, r := range f.Races() {
		if r.Round != round {
			continue
		}
		t := r.Title
		if i := strings.Index(t, " — "); i > 0 {
			t = t[:i]
		}
		if !seen[t] {
			seen[t] = true
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s Round %d", st.Name, round)
	}
	return st.Name + ": " + strings.Join(parts, " · ")
}
