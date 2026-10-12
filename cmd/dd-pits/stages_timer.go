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
	// the race behind a link. A timer that re-runs a race as a new one (a
	// RotorHazard heat group) returns its new link; otherwise "".
	control(link, action string) (string, error)
	// applySettings brings the laps and race time to races already on the
	// timer and not yet flown.
	applySettings(st stageRecord, sent []stageRace) error
	// removeRaces takes races that were never flown off the timer, for a
	// deleted bracket. Flown races and their results are left alone.
	removeRaces(links []string) (int, error)
}

// eventRules is a timer whose laps and race time belong to the event rather
// than each race (FPVTrackside): they are set before a stage race starts.
type eventRules interface {
	setEventRules(laps, secs int) error
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
		return notFound(data)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, shortBody(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

var errNoStageAPI = errors.New("this timer can't take races from dd-pits")

// notFound tells a timer without the endpoint (errNoStageAPI) from
// FPVTrackside's API answering that a race, pilot or crossing isn't there:
// both are a 404, but only the API says what is missing.
func notFound(data []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" && !strings.HasPrefix(e.Error, "no such endpoint") {
		return errors.New(e.Error)
	}
	return errNoStageAPI
}

// ── RotorHazard ──────────────────────────────────────────────────────────────

type rhStageTimer struct {
	base  string // RotorHazard server
	token string // shared with the plugin, which started dd-pits
}

func (t *rhStageTimer) name() string { return "RotorHazard" }

// classLinkKey, in the links send returns, carries the RotorHazard race
// class the bracket's heats went in ("rh-c<id>"), not a race.
const classLinkKey = "#class"

func (t *rhStageTimer) send(st stageRecord, races []stageRace, makeCurrent bool) (map[string]string, error) {
	links := map[string]string{}
	classID := st.ClassID
	// A bracket from before dd-pits remembered its class: any heat it already
	// has tells the plugin which class that is.
	classHeat := 0
	for _, l := range st.Links {
		if h := 0; strings.HasPrefix(l, "rh-h") {
			if fmt.Sscanf(l, "rh-h%d", &h); h > classHeat {
				classHeat = h
			}
		}
	}
	// Every race in one request: RotorHazard looks up the class and pilots
	// once and refreshes its pages once, not per heat. A plugin from before
	// that falls back to one request per race.
	if len(races) > 1 {
		batch := make([]map[string]any, len(races))
		for i, r := range races {
			heat := 0
			if l := st.Links[r.ID]; strings.HasPrefix(l, "rh-h") {
				fmt.Sscanf(l, "rh-h%d", &heat)
			}
			batch[i] = map[string]any{"name": r.Title, "pilots": r.Pilots, "heat": heat,
				"current": makeCurrent && i == 0, "round": r.Round}
		}
		var out struct {
			Heats   []int  `json:"heats"`
			ClassID int    `json:"class_id"`
			Error   string `json:"error"`
		}
		err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/stage-heats", map[string]any{
			"class": st.Name, "class_id": classID, "class_heat": classHeat,
			"laps": st.Setup.Laps, "race_time": st.Setup.RaceSeconds, "races": batch,
		}, t.token, &out)
		if !errors.Is(err, errNoStageAPI) {
			if err != nil {
				return links, err
			}
			for i, h := range out.Heats {
				if i < len(races) && h != 0 {
					links[races[i].ID] = fmt.Sprintf("rh-h%d", h)
				}
			}
			if out.ClassID != 0 && out.ClassID != classID {
				links[classLinkKey] = fmt.Sprintf("rh-c%d", out.ClassID)
			}
			if out.Error != "" {
				return links, errors.New(out.Error)
			}
			return links, nil
		}
	}
	for i, r := range races {
		var out struct {
			Heat    int `json:"heat"`
			ClassID int `json:"class_id"`
		}
		heat := 0
		if l := st.Links[r.ID]; strings.HasPrefix(l, "rh-h") {
			fmt.Sscanf(l, "rh-h%d", &heat)
		}
		err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/stage-heat", map[string]any{
			"class":      st.Name,
			"class_id":   classID,
			"class_heat": classHeat,
			"name":       r.Title,
			"pilots":     r.Pilots,
			"heat":       heat,
			"current":    makeCurrent && i == 0,
			"laps":       st.Setup.Laps,
			"race_time":  st.Setup.RaceSeconds,
			"round":      r.Round, // its heat group, on RotorHazard 4.2+
		}, t.token, &out)
		if errors.Is(err, errNoStageAPI) {
			return links, errors.New("the dd-pits RotorHazard plugin is out of date; update it to send races")
		}
		if err != nil {
			return links, err
		}
		links[r.ID] = fmt.Sprintf("rh-h%d", out.Heat)
		if out.ClassID != 0 && out.ClassID != classID {
			classID = out.ClassID
			links[classLinkKey] = fmt.Sprintf("rh-c%d", classID)
		}
	}
	return links, nil
}

func (t *rhStageTimer) control(link, action string) (string, error) {
	heat := 0
	if _, err := fmt.Sscanf(link, "rh-h%d", &heat); err != nil || heat == 0 {
		return "", errors.New("that race isn't on RotorHazard yet")
	}
	var out struct {
		Heat int `json:"heat"`
	}
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/race-control",
		map[string]any{"action": action, "heat": heat}, t.token, &out)
	if errors.Is(err, errNoStageAPI) {
		return "", errors.New("the dd-pits RotorHazard plugin is out of date; update it to start races")
	}
	if err != nil {
		return "", err
	}
	if out.Heat != 0 && out.Heat != heat {
		return fmt.Sprintf("rh-h%d", out.Heat), nil
	}
	return "", nil
}

// applySettings sets the bracket's race class format on RotorHazard; every
// heat in the class runs with it, so nothing per heat is needed.
func (t *rhStageTimer) applySettings(st stageRecord, _ []stageRace) error {
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/stage-format", map[string]any{
		"class": st.Name, "class_id": st.ClassID, "laps": st.Setup.Laps, "race_time": st.Setup.RaceSeconds,
	}, t.token, nil)
	if errors.Is(err, errNoStageAPI) {
		return errors.New("the dd-pits RotorHazard plugin is out of date; update it to set laps and race time")
	}
	return err
}

func (t *rhStageTimer) removeRaces(links []string) (int, error) {
	var heats []int
	for _, l := range links {
		var h int
		if _, err := fmt.Sscanf(l, "rh-h%d", &h); err == nil && h > 0 {
			heats = append(heats, h)
		}
	}
	if len(heats) == 0 {
		return 0, nil
	}
	var out struct {
		Deleted []int `json:"deleted"`
	}
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/stage-delete", map[string]any{"heats": heats}, t.token, &out)
	if errors.Is(err, errNoStageAPI) {
		return 0, errors.New("the dd-pits RotorHazard plugin is out of date; update it so deleting a bracket removes its heats")
	}
	return len(out.Deleted), err
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
// pending race's target laps and leaves flown ones alone. FPVTrackside resets
// a race's laps to the event's when it starts, and its race time is the
// event's too, so both are also set on the event as dd-pits starts a race
// (setEventRules).
func (t *ftStageTimer) applySettings(st stageRecord, sent []stageRace) error {
	if len(sent) == 0 {
		return nil
	}
	_, err := t.send(st, sent, false)
	return err
}

// removeRaces deletes each race; one already gone is skipped. A refusal (a
// race running, no permission) stops it, so the caller keeps its links rather
// than sending the races again beside the ones still there.
func (t *ftStageTimer) removeRaces(links []string) (int, error) {
	n := 0
	for _, l := range links {
		req, err := newJSONRequest(http.MethodDelete, strings.TrimSuffix(t.base, "/")+"/api/v1/races/"+l, map[string]any{})
		if err != nil {
			return n, err
		}
		resp, err := stageHTTP.Do(req)
		if err != nil {
			return n, err
		}
		switch {
		case resp.StatusCode < 300:
			n++
		case resp.StatusCode != http.StatusNotFound:
			err = readAPIError(resp)
		}
		resp.Body.Close()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// setEventRules gives FPVTrackside's event the bracket's laps and race time
// (0 leaves one as it is) before dd-pits starts a race: FPVTrackside takes
// both from the event when a race starts. Nothing is sent when they already
// match, so an event with its rules locked still runs at its own settings.
func (t *ftStageTimer) setEventRules(laps, secs int) error {
	if laps == 0 && secs == 0 {
		return nil
	}
	api := strings.TrimSuffix(t.base, "/") + "/api/v1/event"
	req, err := http.NewRequest(http.MethodGet, api, nil)
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
	var ev struct {
		Laps              int     `json:"laps"`
		RaceLengthSeconds float64 `json:"raceLengthSeconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return err
	}
	change := map[string]int{}
	if laps > 0 && laps != ev.Laps {
		change["laps"] = laps
	}
	if secs > 0 && float64(secs) != ev.RaceLengthSeconds {
		change["raceLengthSeconds"] = secs
	}
	if len(change) == 0 {
		return nil
	}
	if err := sendJSON(http.MethodPatch, api, change); err != nil {
		return fmt.Errorf("setting FPVTrackside's laps and race time: %w", err)
	}
	return nil
}

// control drives FPVTrackside's race-director screen through its REST API.
// FPVTrackside must have "HTTP Server race controls enabled".
func (t *ftStageTimer) control(link, action string) (string, error) {
	return "", t.drive(link, action)
}

func (t *ftStageTimer) drive(link, action string) error {
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
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, shortBody(data))
}

// shortBody keeps an error reply readable: a crash page from the timer
// becomes a short note, not a screen of HTML.
func shortBody(data []byte) string {
	t := strings.TrimSpace(string(data))
	if strings.HasPrefix(strings.ToLower(t), "<!doctype") || strings.HasPrefix(t, "<html") {
		if i := strings.Index(t, "<title>"); i >= 0 {
			if j := strings.Index(t[i:], "</title>"); j > 0 {
				return "the timer failed: " + strings.TrimSpace(t[i+7:i+j])
			}
		}
		return "the timer failed"
	}
	if len(t) > 200 {
		t = t[:200] + "…"
	}
	return t
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
