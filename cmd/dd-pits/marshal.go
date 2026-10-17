package main

// Race marshalling: fix a race's laps from dd-pits.
//
// Simple on purpose: per pilot, the lap-line crossings in order, each one
// counted or not, and a box to add a missed one. Removing a false crossing
// merges the laps either side of it; adding one splits a lap. The timer does
// the work — RotorHazard through the same save its Marshal page uses,
// FPVTrackside through its REST API — and results, the dashboard and any
// bracket follow on the next read.
//
//	/marshal              finished races to choose from
//	/marshal?race=<id>    one race's crossings, per pilot

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
)

// marshaller is implemented by the stage timers: they already hold the
// timer's address and credentials.
type marshaller interface {
	// marshal changes one pilot's crossings in one race: "invalidate" or
	// "validate" a crossing (lap), or "add" one at raceMs from the start.
	marshal(race, pilot, action, lap string, raceMs int64) error
}

func (t *rhStageTimer) marshal(race, pilot, action, lap string, raceMs int64) error {
	err := postJSON(strings.TrimSuffix(t.base, "/")+"/dd-pits/api/marshal", map[string]any{
		"race": race, "pilot": pilot, "action": action, "lap": lap, "race_time_ms": raceMs,
	}, t.token, nil)
	if errors.Is(err, errNoStageAPI) {
		return errors.New("the dd-pits RotorHazard plugin is out of date; update it to marshal races")
	}
	return err
}

func (t *ftStageTimer) marshal(race, pilot, action, lap string, raceMs int64) error {
	api := strings.TrimSuffix(t.base, "/") + "/api/v1/races/" + url.PathEscape(race)
	var err error
	switch action {
	case "invalidate", "validate":
		err = sendJSON(http.MethodPatch, api+"/detections/"+url.PathEscape(lap),
			map[string]any{"valid": action == "validate", "validityType": "Marshall"})
	case "add":
		err = sendJSON(http.MethodPost, api+"/laps", map[string]any{"pilot": pilot, "raceTimeMs": raceMs})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if errors.Is(err, errNoStageAPI) {
		return errors.New("this FPVTrackside has no REST API; marshal the race in FPVTrackside")
	}
	return err
}

// sendJSON is postJSON for any method, with FPVTrackside's token.
func sendJSON(method, u string, body any) error {
	if method == http.MethodPost {
		return postJSON(u, body, ftToken(), nil)
	}
	req, err := newJSONRequest(method, u, body)
	if err != nil {
		return err
	}
	resp, err := stageHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return notFound(data)
	}
	if resp.StatusCode >= 300 {
		return readAPIError(resp)
	}
	return nil
}

// ── page ─────────────────────────────────────────────────────────────────────

func (s *liveServer) registerMarshal(mux *http.ServeMux) {
	mux.HandleFunc("/marshal", s.marshalPage)
	s.registerSignalMarshal(mux)
}

// marshalTimer is the timer to send corrections to, if there is one.
func (s *liveServer) marshalTimer() marshaller {
	if s.stages == nil {
		return nil
	}
	m, _ := s.stages.timer.(marshaller)
	return m
}

type crossing struct {
	ID     string
	RaceMs int64
	LapSec float64
	Num    int
	Valid  bool
}

// parseRaceTime reads "1:02.5", "62.5" or "62500ms" as milliseconds.
func parseRaceTime(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if strings.HasSuffix(v, "ms") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(v, "ms"), 64)
		return int64(n), err
	}
	mins := 0.0
	if i := strings.Index(v, ":"); i >= 0 {
		m, err := strconv.ParseFloat(v[:i], 64)
		if err != nil {
			return 0, err
		}
		mins, v = m, v[i+1:]
	}
	sec, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, errors.New("give the time from the race start, like 1:02.5")
	}
	return int64((mins*60 + sec) * 1000), nil
}

func fmtRaceMs(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%d:%06.3f", ms/60000, float64(ms%60000)/1000)
}

func (s *liveServer) marshalPage(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()
	raceID := r.FormValue("race")
	msg := r.FormValue("msg")

	if r.Method == http.MethodPost {
		t := s.marshalTimer()
		var err error
		if t == nil {
			err = errors.New("no timer to send corrections to")
		} else {
			switch r.FormValue("do") {
			case "add":
				var ms int64
				if ms, err = parseRaceTime(r.FormValue("at")); err == nil {
					err = t.marshal(raceID, r.FormValue("pilot"), "add", "", ms)
				}
			case "invalidate", "validate":
				err = t.marshal(raceID, r.FormValue("pilot"), r.FormValue("do"), r.FormValue("lap"), 0)
			}
		}
		m := "Saved."
		if err != nil {
			m = "Not changed: " + err.Error()
		} else {
			time.Sleep(700 * time.Millisecond) // let the next read pick it up
			if s.stages != nil {
				s.stages.syncAll(false)
			}
		}
		http.Redirect(w, r, "/marshal?race="+url.QueryEscape(raceID)+"&msg="+url.QueryEscape(m), http.StatusSeeOther)
		return
	}

	b := &strings.Builder{}
	if msg != "" {
		cls := "ok"
		if strings.HasPrefix(msg, "Not") {
			cls = "warn"
		}
		fmt.Fprintf(b, `<div class="card"><p class="%s">%s</p></div>`, cls, esc(msg))
	}
	if raceID == "" {
		s.marshalList(b, snap)
		writeSetup(w, r, "Marshal", b.String())
		return
	}
	mv := buildMeeting(snap)
	label := raceID
	for _, vr := range mv.Races {
		if vr.ID == raceID {
			label = vr.Label
		}
	}
	// A timer that keeps signal traces (RotorHazard, or the RotorHazard
	// behind FPVTrackside) gets the trace view; ?simple=1 is the plain list.
	if r.FormValue("simple") != "1" && s.traceSource() != nil {
		s.signalMarshalPage(w, r, raceID, label)
		return
	}
	idx := -1
	for i := range snap.Races {
		if snap.Races[i].ID == raceID {
			idx = i
		}
	}
	if idx < 0 {
		fmt.Fprintf(b, `<div class="card"><p class="warn">That race isn't in the meeting any more.</p><p><a href="/marshal">Choose another race</a></p></div>`)
		writeSetup(w, r, "Marshal", b.String())
		return
	}
	rr := snap.Races[idx]
	start := parseTime(rr.Start)
	names := map[string]string{}
	for _, p := range snap.Pilots {
		names[p.ID] = p.Name
	}
	lapByDet := map[string]float64{}
	numByDet := map[string]int{}
	for _, l := range rr.Laps {
		lapByDet[l.Detection] = l.LengthSeconds
		numByDet[l.Detection] = l.LapNumber
	}
	fmt.Fprintf(b, `<div class="card"><p class="mut"><a href="/marshal">Races</a></p><h2>%s</h2>
<p class="mut">Remove a false lap and the laps either side of it join up; add a missed one and a lap splits in two.
Results, the dashboard and any bracket update straight after.</p></div>`, esc(label))
	if start.IsZero() {
		b.WriteString(`<div class="card"><p class="mut">This race hasn't started.</p></div>`)
	}
	for _, pc := range rr.PilotChannels {
		var xs []crossing
		for _, d := range rr.Detections {
			if d.Pilot != pc.Pilot || !d.IsLapEnd {
				continue
			}
			c := crossing{ID: d.ID, Valid: d.Valid, LapSec: lapByDet[d.ID], Num: numByDet[d.ID]}
			if t := parseTime(d.Time); !t.IsZero() && !start.IsZero() {
				c.RaceMs = t.Sub(start).Milliseconds()
			}
			xs = append(xs, c)
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].RaceMs < xs[j].RaceMs })
		name := names[pc.Pilot]
		if name == "" {
			name = pc.Pilot
		}
		fmt.Fprintf(b, `<div class="card"><h2>%s</h2><table><tr><th>Lap</th><th>Race time</th><th>Lap time</th><th></th></tr>`, esc(name))
		if len(xs) == 0 {
			b.WriteString(`<tr><td colspan="4" class="mut">No crossings.</td></tr>`)
		}
		for _, c := range xs {
			lap, lapTime, act, btn, cls := "—", "", "validate", "Restore", ` class="mut" style="text-decoration:line-through"`
			if c.Valid {
				lap = strconv.Itoa(c.Num)
				if c.Num == 0 {
					lap = "Holeshot"
				}
				lapTime = fmt.Sprintf("%.3f", c.LapSec)
				act, btn, cls = "invalidate", "Remove", ""
			}
			fmt.Fprintf(b, `<tr%s><td>%s</td><td>%s</td><td>%s</td><td><form method="post" style="margin:0">
<input type="hidden" name="race" value="%s"><input type="hidden" name="pilot" value="%s"><input type="hidden" name="lap" value="%s">
<button class="sec small" name="do" value="%s">%s</button></form></td></tr>`,
				cls, lap, fmtRaceMs(c.RaceMs), lapTime, esc(raceID), esc(pc.Pilot), esc(c.ID), act, btn)
		}
		fmt.Fprintf(b, `</table><form method="post" class="row" style="margin-top:.7rem;align-items:flex-end">
<input type="hidden" name="race" value="%s"><input type="hidden" name="pilot" value="%s">
<div><label>Add a missed lap at (race time)</label><input type="text" name="at" placeholder="1:02.5" required></div>
<div style="flex:0"><button class="sec" name="do" value="add">Add lap</button></div></form></div>`, esc(raceID), esc(pc.Pilot))
	}
	b.WriteString(`<style>button.small{padding:.3rem .75rem;font-size:.8rem}</style>` + ajaxForms)
	writeSetup(w, r, "Marshal", b.String())
}

// marshalList offers the races that have been flown, most recent first.
func (s *liveServer) marshalList(b *strings.Builder, snap dronedash.Snapshot) {
	mv := buildMeeting(snap)
	b.WriteString(`<div class="card"><h2>Pick a race to marshal</h2><p class="mut">Races that have been flown, most recent first.</p><table>`)
	n := 0
	var flown []vmRace
	for _, r := range mv.Races {
		if r.State != "pending" {
			flown = append(flown, r)
		}
	}
	sort.SliceStable(flown, func(i, j int) bool { return flown[i].StartMs > flown[j].StartMs })
	for _, r := range flown {
		var who []string
		for _, p := range r.Pilots {
			who = append(who, p.Name)
		}
		state := "finished"
		if r.State == "running" {
			state = "racing"
		}
		fmt.Fprintf(b, `<tr><td><a href="/marshal?race=%s">%s</a></td><td class="mut">%s</td><td class="mut">%s</td></tr>`,
			url.QueryEscape(r.ID), esc(r.Label), esc(strings.Join(who, ", ")), state)
		n++
	}
	if n == 0 {
		b.WriteString(`<tr><td class="mut">No races flown yet.</td></tr>`)
	}
	b.WriteString(`</table></div>`)
}
