package main

// Meeting editor: change FPVTrackside's meeting from dd-pits.
//
// Everything goes through FPVTrackside's REST API, so the change is made in
// FPVTrackside itself and the dashboard, brackets and calls follow on the next
// read: the event's name, rules and channels, its rounds (add, rename,
// delete), races (add, delete, reset, laps to finish), each race's line-up and
// channels, and the event's pilots.
//
// FPVTrackside has no race names (a race is always "Race 1-2", from its round
// and number), so a race's name is kept by dd-pits (raceNames) and shown on
// every dd-pits page in place of the round-and-number label.
//
//	/meeting   the editor (race director only, FPVTrackside only)

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── race names (kept by dd-pits) ─────────────────────────────────────────────

type raceNameStore struct {
	mu    sync.RWMutex
	path  string
	names map[string]string // timer race ID -> name
}

// raceNames is read by buildMeeting; empty (no file) until main loads one.
var raceNames = &raceNameStore{names: map[string]string{}}

func raceNamesPath(configPath string) string {
	if configPath == "" {
		configPath = "dd-pits-local.json"
	}
	return filepath.Join(filepath.Dir(configPath), "dd-pits-race-names.json")
}

func loadRaceNames(path string) *raceNameStore {
	s := &raceNameStore{path: path, names: map[string]string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(raw, &s.names); err != nil {
		log.Printf("race names: %s is unreadable (%v) — starting with none", path, err)
		s.names = map[string]string{}
	}
	return s
}

func (s *raceNameStore) get(raceID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[raceID]
}

// set names a race; an empty name goes back to the timer's label.
func (s *raceNameStore) set(raceID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		delete(s.names, raceID)
	} else {
		s.names[raceID] = name
	}
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.names, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ── FPVTrackside's meeting, as its REST API gives it ─────────────────────────

type ftmChannel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type ftmPilot struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	FirstName string      `json:"firstName"`
	LastName  string      `json:"lastName"`
	Channel   *ftmChannel `json:"channel"`
}

type ftmRound struct {
	ID        string `json:"id"`
	Number    int    `json:"number"`
	Name      string `json:"name"`
	EventType string `json:"eventType"`
	Order     int    `json:"order"`
	Stage     string `json:"stage"`
}

type ftmSeat struct {
	PilotID string      `json:"pilotId"`
	Name    string      `json:"name"`
	Channel *ftmChannel `json:"channel"`
}

type ftmRace struct {
	ID         string    `json:"id"`
	RoundID    string    `json:"roundId"`
	RaceNumber int       `json:"raceNumber"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	IsCurrent  bool      `json:"isCurrent"`
	TargetLaps int       `json:"targetLaps"`
	ExternalID int       `json:"externalId"`
	Pilots     []ftmSeat `json:"pilots"`
}

type ftmEvent struct {
	Name              string       `json:"name"`
	EventType         string       `json:"eventType"`
	Laps              int          `json:"laps"`
	PbLaps            int          `json:"pbLaps"`
	RaceLengthSeconds float64      `json:"raceLengthSeconds"`
	MinLapTimeSeconds float64      `json:"minLapTimeSeconds"`
	PackLimit         int          `json:"packLimit"`
	RulesLocked       bool         `json:"rulesLocked"`
	Channels          []ftmChannel `json:"channels"`
}

type ftMeeting struct {
	Event  ftmEvent   `json:"event"`
	Pilots []ftmPilot `json:"pilots"`
	Rounds []ftmRound `json:"rounds"`
	Races  []ftmRace  `json:"races"`
}

// ftEventTypes are FPVTrackside's race types, for new rounds and the event.
var ftEventTypes = []string{"Race", "TimeTrial", "Practice", "Freestyle", "Endurance", "CasualPractice", "AggregateLaps", "Game"}

// ftEditor is the FPVTrackside the meeting editor changes.
type ftEditor struct{ base string }

func (e ftEditor) api(path string) string {
	return strings.TrimSuffix(e.base, "/") + "/api/v1/" + strings.TrimPrefix(path, "/")
}

func (e ftEditor) meeting() (*ftMeeting, error) {
	req, err := http.NewRequest(http.MethodGet, e.api("meeting?laps=false"), nil)
	if err != nil {
		return nil, err
	}
	if tok := ftToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := stageHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach FPVTrackside: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("this FPVTrackside has no REST API. Update FPVTrackside to a version with the REST API to edit the meeting from dd-pits")
	}
	if resp.StatusCode >= 300 {
		return nil, readAPIError(resp)
	}
	var m ftMeeting
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (e ftEditor) call(method, path string, body any) error {
	if body == nil {
		body = map[string]any{}
	}
	err := sendJSON(method, e.api(path), body)
	if errors.Is(err, errNoStageAPI) {
		return errors.New("this FPVTrackside has no REST API")
	}
	return err
}

func pathID(id string) string { return url.PathEscape(strings.TrimSpace(id)) }

// ── page ─────────────────────────────────────────────────────────────────────

func (s *liveServer) registerMeeting(mux *http.ServeMux) {
	mux.HandleFunc("/meeting", s.meetingPage)
}

// meetingEditor is the FPVTrackside to edit, or nil on another timer.
func (s *liveServer) meetingEditor() *ftEditor {
	if s.stages == nil {
		return nil
	}
	t, ok := s.stages.timer.(*ftStageTimer)
	if !ok || t.base == "" {
		return nil
	}
	return &ftEditor{base: t.base}
}

func (s *liveServer) meetingPage(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	ed := s.meetingEditor()
	if ed == nil {
		writeSetup(w, r, "Meeting", `<div class="card"><h2>The meeting editor is for FPVTrackside</h2>
<p>On RotorHazard, change heats, classes and pilots in RotorHazard itself; dd-pits follows on its own.</p></div>`)
		return
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		m := "Saved."
		if err := s.meetingChange(*ed, r); err != nil {
			m = "Not changed: " + err.Error()
		} else {
			time.Sleep(500 * time.Millisecond) // let the next read pick it up
			if s.stages != nil {
				s.stages.syncAll(false)
			}
		}
		back := "/meeting?msg=" + url.QueryEscape(m)
		if a := r.FormValue("at"); a != "" {
			back += "#" + url.QueryEscape(a)
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	b := &strings.Builder{}
	if msg := r.FormValue("msg"); msg != "" {
		cls := "ok"
		if strings.HasPrefix(msg, "Not") {
			cls = "warn"
		}
		fmt.Fprintf(b, `<div class="card"><p class="%s">%s</p></div>`, cls, esc(msg))
	}
	mt, err := ed.meeting()
	if err != nil {
		fmt.Fprintf(b, `<div class="card"><h2>Can't read the meeting</h2><p class="warn">%s</p></div>`, esc(err.Error()))
		writeSetup(w, r, "Meeting", b.String())
		return
	}
	renderMeetingEditor(b, mt)
	b.WriteString(meetingCSS + ajaxForms)
	writeSetup(w, r, "Meeting", b.String())
}

// meetingChange makes one change from a form on the page.
func (s *liveServer) meetingChange(ed ftEditor, r *http.Request) error {
	v := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	optInt := func(k string) (*int, error) {
		if v(k) == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(v(k))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s must be a whole number", k)
		}
		return &n, nil
	}
	race, round, pilot := pathID(v("race")), pathID(v("round")), pathID(v("pilot"))

	switch v("do") {
	// Event
	case "event":
		body := map[string]any{}
		if v("name") != "" {
			body["name"] = v("name")
		}
		if v("eventType") != "" {
			body["eventType"] = v("eventType")
		}
		for _, k := range []string{"laps", "pbLaps", "packLimit"} {
			n, err := optInt(k)
			if err != nil {
				return err
			}
			if n != nil {
				body[k] = *n
			}
		}
		if v("raceLength") != "" {
			secs, err := parseDuration(v("raceLength"))
			if err != nil {
				return err
			}
			body["raceLengthSeconds"] = secs
		}
		if v("minLap") != "" {
			f, err := strconv.ParseFloat(v("minLap"), 64)
			if err != nil || f < 0 {
				return errors.New("minimum lap time is in seconds, like 4.5")
			}
			body["minLapTimeSeconds"] = f
		}
		return ed.call(http.MethodPatch, "event", body)
	case "channels":
		list := strings.FieldsFunc(v("channels"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\t' })
		if len(list) == 0 {
			return errors.New("give at least one channel, like R1 R2 F4 F8")
		}
		return ed.call(http.MethodPut, "event/channels", map[string]any{"channels": list})

	// Rounds
	case "round-add":
		body := map[string]any{}
		if v("name") != "" {
			body["name"] = v("name")
		}
		if v("eventType") != "" {
			body["eventType"] = v("eventType")
		}
		return ed.call(http.MethodPost, "rounds", body)
	case "round-rename":
		return ed.call(http.MethodPatch, "rounds/"+round, map[string]any{"name": v("name")})
	case "round-delete":
		return ed.call(http.MethodDelete, "rounds/"+round, nil)

	// Races
	case "race-add":
		body := map[string]any{"roundId": v("round")}
		if seats := lineupFromForm(r); len(seats) > 0 {
			body["pilots"] = seats
		}
		return ed.call(http.MethodPost, "races", body)
	case "race-save":
		// FPVTrackside first: a refused line-up leaves the whole race as it was.
		if r.FormValue("lineup") == "1" && v("lineupWas") != lineupKey(lineupFromForm(r)) {
			if err := ed.call(http.MethodPut, "races/"+race+"/pilots", lineupFromForm(r)); err != nil {
				return err
			}
		}
		if n, err := optInt("targetLaps"); err != nil {
			return err
		} else if n != nil && v("targetLaps") != v("targetLapsWas") {
			if err := ed.call(http.MethodPatch, "races/"+race, map[string]any{"targetLaps": *n}); err != nil {
				return err
			}
		}
		if err := raceNames.set(v("race"), v("name")); err != nil {
			return fmt.Errorf("saving the race name: %w", err)
		}
		return nil
	case "race-reset":
		return ed.call(http.MethodPost, "races/"+race+"/reset", nil)
	case "race-delete":
		if err := ed.call(http.MethodDelete, "races/"+race, nil); err != nil {
			return err
		}
		return raceNames.set(v("race"), "")
	case "race-select":
		return putJSON(ed.api("race-control/current"), map[string]string{"raceId": v("race")})

	// Pilots
	case "pilot-add":
		if v("name") == "" {
			return errors.New("a pilot needs a name")
		}
		body := map[string]any{"name": v("name")}
		for _, k := range []string{"firstName", "lastName", "channel"} {
			if v(k) != "" {
				body[k] = v(k)
			}
		}
		return ed.call(http.MethodPost, "pilots", body)
	case "pilot-save":
		if v("name") == "" {
			return errors.New("a pilot needs a name")
		}
		body := map[string]any{"name": v("name"), "firstName": v("firstName"), "lastName": v("lastName")}
		if v("channel") != "" {
			body["channel"] = v("channel")
		}
		return ed.call(http.MethodPatch, "pilots/"+pilot, body)
	case "pilot-remove":
		return ed.call(http.MethodDelete, "pilots/"+pilot, nil)
	}
	return fmt.Errorf("unknown change %q", v("do"))
}

type seatReq struct {
	Pilot   string `json:"pilot"`
	Channel string `json:"channel,omitempty"`
}

// lineupFromForm reads the seat rows (seat-pilot-N, seat-chan-N) of a race
// form, skipping empty seats.
func lineupFromForm(r *http.Request) []seatReq {
	out := []seatReq{}
	for i := 0; i < 64; i++ {
		p := strings.TrimSpace(r.FormValue(fmt.Sprintf("seat-pilot-%d", i)))
		c := strings.TrimSpace(r.FormValue(fmt.Sprintf("seat-chan-%d", i)))
		if p == "" {
			continue
		}
		out = append(out, seatReq{Pilot: p, Channel: c})
	}
	return out
}

// lineupKey tells whether a line-up changed, so saving a race's name alone
// doesn't re-seat its pilots.
func lineupKey(seats []seatReq) string {
	parts := make([]string, len(seats))
	for i, s := range seats {
		parts[i] = s.Pilot + "@" + s.Channel
	}
	return strings.Join(parts, ",")
}

func renderMeetingEditor(b *strings.Builder, mt *ftMeeting) {
	ev := mt.Event
	sort.SliceStable(mt.Pilots, func(i, j int) bool {
		return strings.ToLower(mt.Pilots[i].Name) < strings.ToLower(mt.Pilots[j].Name)
	})
	sort.SliceStable(mt.Rounds, func(i, j int) bool { return mt.Rounds[i].Order < mt.Rounds[j].Order })

	typeOptions := func(sel string, blank string) string {
		o := &strings.Builder{}
		if blank != "" {
			fmt.Fprintf(o, `<option value="">%s</option>`, esc(blank))
		}
		for _, t := range ftEventTypes {
			s := ""
			if t == sel {
				s = " selected"
			}
			fmt.Fprintf(o, `<option value="%s"%s>%s</option>`, t, s, esc(typeName(t)))
		}
		return o.String()
	}
	chanOptions := func(sel string) string {
		o := &strings.Builder{}
		o.WriteString(`<option value="">free channel</option>`)
		for _, c := range ev.Channels {
			s := ""
			if c.ID == sel {
				s = " selected"
			}
			fmt.Fprintf(o, `<option value="%s"%s>%s</option>`, esc(c.ID), s, esc(c.Label))
		}
		return o.String()
	}
	pilotOptions := func(sel string) string {
		o := &strings.Builder{}
		o.WriteString(`<option value="">—</option>`)
		for _, p := range mt.Pilots {
			s := ""
			if p.ID == sel {
				s = " selected"
			}
			fmt.Fprintf(o, `<option value="%s"%s>%s</option>`, esc(p.ID), s, esc(p.Name))
		}
		return o.String()
	}
	// seatRows gives a race form one row per channel, so any pilot can go on
	// any channel; the race's own seats come first.
	seatRows := func(seats []ftmSeat) string {
		o := &strings.Builder{}
		n := len(ev.Channels)
		if len(seats) > n {
			n = len(seats)
		}
		if n == 0 {
			n = 4
		}
		for i := 0; i < n; i++ {
			pid, cid := "", ""
			if i < len(seats) {
				pid = seats[i].PilotID
				if seats[i].Channel != nil {
					cid = seats[i].Channel.ID
				}
			}
			fmt.Fprintf(o, `<div class="seat"><select name="seat-pilot-%d">%s</select><select name="seat-chan-%d">%s</select></div>`,
				i, pilotOptions(pid), i, chanOptions(cid))
		}
		return o.String()
	}

	// Event
	b.WriteString(`<div class="card"><h2>Meeting</h2><p class="mut">Changes go straight to FPVTrackside. Rounds, races, line-ups and pilots made in FPVTrackside show here too.</p>
<p class="mut"><a href="#event">Event</a> · <a href="#rounds">Rounds and races</a> · <a href="#pilots">Pilots</a></p></div>`)
	locked := ""
	if ev.RulesLocked {
		locked = `<p class="warn">The event's rules are locked in FPVTrackside, so laps and race length can't be changed.</p>`
	}
	fmt.Fprintf(b, `<div class="card" id="event"><h2>Event</h2>%s<form method="post" class="grid">
<input type="hidden" name="do" value="event"><input type="hidden" name="at" value="event">
<div><label>Name</label><input type="text" name="name" value="%s"></div>
<div><label>Race type for new rounds</label><select name="eventType">%s</select></div>
<div><label>Laps per race</label><input type="number" min="0" name="laps" value="%d"></div>
<div><label>Consecutive laps for PBs</label><input type="number" min="0" name="pbLaps" value="%d"></div>
<div><label>Race length (timed races)</label><input type="text" name="raceLength" value="%s" placeholder="2:00"></div>
<div><label>Ignore laps shorter than (s)</label><input type="text" name="minLap" value="%s"></div>
<div><label>Pack limit (0 = none)</label><input type="number" min="0" name="packLimit" value="%d"></div>
<div class="end"><button>Save event</button></div></form>`,
		locked, esc(ev.Name), typeOptions(ev.EventType, ""), ev.Laps, ev.PbLaps,
		fmtDuration(int(ev.RaceLengthSeconds)), strconv.FormatFloat(ev.MinLapTimeSeconds, 'f', -1, 64), ev.PackLimit)
	var labels []string
	for _, c := range ev.Channels {
		labels = append(labels, c.Label)
	}
	fmt.Fprintf(b, `<form method="post" class="row" style="margin-top:1rem;align-items:flex-end">
<input type="hidden" name="do" value="channels"><input type="hidden" name="at" value="event">
<div><label>Channels, in order</label><input type="text" name="channels" value="%s" placeholder="R1 R2 F4 F8"></div>
<div style="flex:0"><button class="sec">Set channels</button></div></form></div>`, esc(strings.Join(labels, " ")))

	// Rounds and races
	races := map[string][]ftmRace{}
	for _, rc := range mt.Races {
		races[rc.RoundID] = append(races[rc.RoundID], rc)
	}
	b.WriteString(`<div class="card" id="rounds"><h2>Rounds and races</h2>`)
	if len(mt.Rounds) == 0 {
		b.WriteString(`<p class="mut">No rounds yet.</p>`)
	}
	for _, rd := range mt.Rounds {
		rs := races[rd.ID]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].RaceNumber < rs[j].RaceNumber })
		title := rd.Name
		if title == "" {
			title = fmt.Sprintf("%s round %d", typeName(rd.EventType), rd.Number)
		}
		anchor := "round-" + rd.ID
		stage := ""
		if rd.Stage != "" {
			stage = ` <span class="tag">` + esc(rd.Stage) + `</span>`
		}
		fmt.Fprintf(b, `<details class="round" id="%s" open><summary><b>%s</b>%s <span class="mut">%s · %d race%s</span></summary>
<form method="post" class="row" style="align-items:flex-end">
<input type="hidden" name="round" value="%s"><input type="hidden" name="at" value="%s">
<div><label>Round name</label><input type="text" name="name" value="%s" placeholder="%s round %d"></div>
<div style="flex:0"><button class="sec" name="do" value="round-rename">Rename</button></div>
<div style="flex:0"><button class="sec danger" name="do" value="round-delete" onclick="return confirm('Delete this round and every race in it?')">Delete round</button></div></form>`,
			esc(anchor), esc(title), stage, esc(typeName(rd.EventType)), len(rs), plural(len(rs)),
			esc(rd.ID), esc(anchor), esc(rd.Name), esc(typeName(rd.EventType)), rd.Number)
		for _, rc := range rs {
			name := raceNames.get(rc.ID)
			state := map[string]string{"pending": "not run", "running": "racing", "finished": "finished"}[rc.State]
			cur := ""
			if rc.IsCurrent {
				cur = ` <span class="tag on">selected</span>`
			}
			ext := ""
			if rc.ExternalID != 0 {
				ext = `<p class="mut small">Sent by a bracket: a change to its line-up here may be put back when the bracket sends it again.</p>`
			}
			var who []string
			var key []seatReq
			for _, p := range rc.Pilots {
				ch := ""
				cid := ""
				if p.Channel != nil {
					ch, cid = " ("+p.Channel.Label+")", p.Channel.ID
				}
				who = append(who, p.Name+ch)
				key = append(key, seatReq{Pilot: p.PilotID, Channel: cid})
			}
			label := rc.Name
			if name != "" {
				label = esc(name) + ` <span class="mut">· ` + esc(rc.Name) + `</span>`
			} else {
				label = esc(label)
			}
			anchorR := "race-" + rc.ID
			lineup := ""
			if rc.State == "pending" {
				lineup = `<input type="hidden" name="lineup" value="1"><label>Line-up</label><div class="seats">` + seatRows(rc.Pilots) + `</div>`
			}
			fmt.Fprintf(b, `<details class="race" id="%s"><summary>%s%s <span class="mut">%s · %s</span></summary>%s
<form method="post"><input type="hidden" name="race" value="%s"><input type="hidden" name="at" value="%s">
<input type="hidden" name="targetLapsWas" value="%d"><input type="hidden" name="lineupWas" value="%s">
<div class="row"><div><label>Race name (shown by dd-pits)</label><input type="text" name="name" value="%s" placeholder="%s"></div>
<div style="flex:0 0 8rem"><label>Laps to finish</label><input type="number" min="0" name="targetLaps" value="%d"></div></div>
%s
<div class="row btns"><button name="do" value="race-save">Save race</button>
<button class="sec" name="do" value="race-select">Select in FPVTrackside</button>
<button class="sec" name="do" value="race-reset" onclick="return confirm('Clear this race\'s laps and results?')">Reset</button>
<button class="sec danger" name="do" value="race-delete" onclick="return confirm('Delete this race?')">Delete</button></div></form></details>`,
				esc(anchorR), label, cur, esc(state), esc(strings.Join(who, ", ")), ext,
				esc(rc.ID), esc(anchor), rc.TargetLaps, esc(lineupKey(key)), esc(name), esc(rc.Name), rc.TargetLaps, lineup)
		}
		fmt.Fprintf(b, `<details class="race add"><summary>+ Add a race to this round</summary><form method="post">
<input type="hidden" name="round" value="%s"><input type="hidden" name="at" value="%s"><label>Line-up</label><div class="seats">%s</div>
<div class="row btns"><button name="do" value="race-add">Add race</button></div></form></details></details>`,
			esc(rd.ID), esc(anchor), seatRows(nil))
	}
	fmt.Fprintf(b, `<form method="post" class="row" style="margin-top:1rem;align-items:flex-end">
<input type="hidden" name="at" value="rounds">
<div><label>New round name (optional)</label><input type="text" name="name"></div>
<div style="flex:0 0 11rem"><label>Race type</label><select name="eventType">%s</select></div>
<div style="flex:0"><button name="do" value="round-add">Add round</button></div></form></div>`, typeOptions("", "the event's"))

	// Pilots
	b.WriteString(`<div class="card" id="pilots"><h2>Pilots</h2><table><tr><th>Name</th><th>First name</th><th>Last name</th><th>Channel</th><th></th></tr>`)
	if len(mt.Pilots) == 0 {
		b.WriteString(`<tr><td colspan="5" class="mut">No pilots in the event.</td></tr>`)
	}
	for _, p := range mt.Pilots {
		cid := ""
		if p.Channel != nil {
			cid = p.Channel.ID
		}
		f := "pf-" + p.ID
		fmt.Fprintf(b, `<tr><td><form method="post" id="%s"><input type="hidden" name="pilot" value="%s"><input type="hidden" name="at" value="pilots"></form>
<input form="%s" type="text" name="name" value="%s" required></td>
<td><input form="%s" type="text" name="firstName" value="%s"></td><td><input form="%s" type="text" name="lastName" value="%s"></td>
<td><select form="%s" name="channel">%s</select></td>
<td class="nowrap"><button form="%s" class="sec small" name="do" value="pilot-save">Save</button>
<button form="%s" class="sec small danger" name="do" value="pilot-remove" onclick="return confirm('Remove %s from the event?')">Remove</button></td></tr>`,
			esc(f), esc(p.ID), esc(f), esc(p.Name), esc(f), esc(p.FirstName), esc(f), esc(p.LastName),
			esc(f), chanOptions(cid), esc(f), esc(f), esc(strings.ReplaceAll(p.Name, "'", "")))
	}
	fmt.Fprintf(b, `</table><form method="post" class="row" style="margin-top:1rem;align-items:flex-end">
<input type="hidden" name="at" value="pilots">
<div><label>Add a pilot (callsign)</label><input type="text" name="name" required></div>
<div><label>First name</label><input type="text" name="firstName"></div>
<div><label>Last name</label><input type="text" name="lastName"></div>
<div style="flex:0 0 9rem"><label>Channel</label><select name="channel">%s</select></div>
<div style="flex:0"><button name="do" value="pilot-add">Add pilot</button></div></form></div>`, chanOptions(""))
}

// typeName is how a race type reads: "TimeTrial" is "Time trial".
func typeName(t string) string {
	switch t {
	case "TimeTrial":
		return "Time trial"
	case "CasualPractice":
		return "Casual practice"
	case "AggregateLaps":
		return "Aggregate laps"
	case "":
		return "Race"
	}
	return t
}

const meetingCSS = `<style>
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(12rem,1fr));gap:.7rem;align-items:end}
.grid .end{align-self:end}
details.round{border-top:1px solid rgba(255,255,255,.08);padding:.7rem 0}
details.round>summary{cursor:pointer;font-size:1.02rem;padding:.2rem 0}
details.race{margin:.5rem 0 .5rem 1rem;padding:.5rem .7rem;border:1px solid rgba(255,255,255,.08);border-radius:12px}
details.race>summary{cursor:pointer}
details.race.add>summary{color:#c6ff3d}
.seats{display:grid;grid-template-columns:repeat(auto-fill,minmax(15rem,1fr));gap:.4rem;margin:.3rem 0 .6rem}
.seat{display:flex;gap:.3rem}.seat select{flex:1;min-width:0}
.row.btns{gap:.4rem;flex-wrap:wrap}.row.btns button{flex:0 0 auto}
.tag{display:inline-block;padding:.05rem .5rem;border-radius:999px;background:rgba(255,255,255,.08);font-size:.75rem;margin-left:.3rem}
.tag.on{background:rgba(198,255,61,.16);color:#c6ff3d}
button.danger{color:#ff8a80}
button.small{padding:.3rem .75rem;font-size:.8rem}
p.small{font-size:.8rem;margin:.3rem 0}
td.nowrap{white-space:nowrap}
table input,table select{width:100%;min-width:6rem}
</style>`
