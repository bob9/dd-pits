package main

// Signal marshalling: the race director sees every pilot's signal trace with
// the enter/exit lines, drags the lines (or lets dd-pits find them), sees the
// laps recalculated as they go with the odd ones flagged, and saves.
//
// All the arithmetic is internal/marshal's; this file serves it to the page.

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bob9/dd-pits/internal/marshal"
)

// traceSource for this venue's timer, or nil when it keeps no traces.
func (s *liveServer) traceSource() traceSource {
	if s.stages == nil {
		return nil
	}
	switch t := s.stages.timer.(type) {
	case *rhStageTimer:
		return rhPluginTraces{base: t.base, token: t.token, raceRef: func(id string) string { return id }, label: "RotorHazard"}
	case *ftStageTimer:
		if src, err := s.ftTraceSource(); err == nil {
			return src
		}
	}
	return nil
}

// A race's traces are read once and kept while it's being marshalled.
type traceCache struct {
	mu sync.Mutex
	m  map[string]cachedTraces
}

type cachedTraces struct {
	at time.Time
	rt *raceTraces
}

var marshalTraces = &traceCache{m: map[string]cachedTraces{}}

func (s *liveServer) loadTraces(race string, fresh bool) (*raceTraces, traceSource, error) {
	src := s.traceSource()
	if src == nil {
		return nil, nil, errNoTraces
	}
	marshalTraces.mu.Lock()
	c, ok := marshalTraces.m[race]
	marshalTraces.mu.Unlock()
	if ok && !fresh && time.Since(c.at) < 10*time.Minute {
		return c.rt, src, nil
	}
	rt, err := src.traces(race)
	if err != nil {
		return nil, src, err
	}
	marshalTraces.mu.Lock()
	marshalTraces.m[race] = cachedTraces{at: time.Now(), rt: rt}
	marshalTraces.mu.Unlock()
	return rt, src, nil
}

// raceRules: the race's lap count and whether the first pass is a holeshot.
func (s *liveServer) raceRules(race string) (targetLaps int, holeshot bool) {
	snap := s.currentSnap()
	holeshot = true
	if timingSystem == "FPVTrackside" {
		holeshot = strings.EqualFold(snap.Event.PrimaryTimingSystemLocation, "Holeshot")
	}
	for _, r := range snap.Races {
		if r.ID == race {
			targetLaps = r.TargetLaps
		}
	}
	if targetLaps == 0 {
		targetLaps = snap.Event.Laps
	}
	return
}

// ── JSON for the page ────────────────────────────────────────────────────────

type sigCrossing struct {
	StartS  float64 `json:"s"`
	EndS    float64 `json:"e"`
	StampMs float64 `json:"t"`
	Peak    float64 `json:"p"`
	Deleted bool    `json:"x,omitempty"`
	Why     string  `json:"why,omitempty"`
}

type sigPilot struct {
	Pilot      string              `json:"pilot"`
	Name       string              `json:"name"`
	Node       int                 `json:"node"`
	Freq       int                 `json:"freq"`
	T          []float64           `json:"t"` // seconds from the race start
	V          []float64           `json:"v"`
	SavedEnter float64             `json:"savedEnter"`
	SavedExit  float64             `json:"savedExit"`
	Saved      []float64           `json:"saved"` // counted laps as saved now (stamps, ms)
	Suggest    *marshal.Suggestion `json:"suggest,omitempty"`
}

type sigResult struct {
	Enter     float64         `json:"enter"`
	Exit      float64         `json:"exit"`
	Crossings []sigCrossing   `json:"crossings"`
	Laps      []float64       `json:"laps"` // lap times, ms
	Issues    []marshal.Issue `json:"issues"`
}

func toSig(cs []marshal.Crossing) []sigCrossing {
	out := make([]sigCrossing, len(cs))
	for i, c := range cs {
		out[i] = sigCrossing{StartS: c.StartS, EndS: c.EndS, StampMs: c.StampMs, Peak: c.Peak, Deleted: c.Deleted, Why: c.Why}
	}
	return out
}

// applyEdits marks crossings the race director removed (by stamp) and adds
// the laps they added by hand.
func applyEdits(t marshal.Trace, cs []marshal.Crossing, removed, added []float64) []marshal.Crossing {
	out := append([]marshal.Crossing(nil), cs...)
	for _, r := range removed {
		for i := range out {
			if math.Abs(out[i].StampMs-r) < 60 {
				out[i].Deleted, out[i].Why = true, "removed by the race director"
			}
		}
	}
	for _, a := range added {
		out = append(out, marshal.Crossing{StartS: a / 1000, EndS: a / 1000, StampMs: a, Peak: t.PeakNear(a), Manual: true, Why: "added by the race director"})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StampMs < out[j].StampMs })
	return out
}

func (s *liveServer) analyse(rt *raceTraces, race, pilot string, enter, exit float64, removed, added []float64) (sigResult, *pilotTrace, error) {
	target, holeshot := s.raceRules(race)
	o := rt.options(target, holeshot)
	var me *pilotTrace
	var others []marshal.Other
	for i := range rt.Pilots {
		p := &rt.Pilots[i]
		if p.Pilot == pilot {
			me = p
			continue
		}
		// Their crossings at their own saved lines, for spotting bleed-over.
		others = append(others, marshal.Other{Name: p.Name, Trace: p.trace(rt.Start)})
	}
	if me == nil {
		return sigResult{}, nil, errors.New("that pilot didn't fly this race")
	}
	cs := applyEdits(me.trace(rt.Start), marshal.Recalc(me.trace(rt.Start), enter, exit, o), removed, added)
	return sigResult{
		Enter: enter, Exit: exit, Crossings: toSig(cs),
		Laps:   marshal.LapTimes(cs, holeshot),
		Issues: marshal.Issues(me.trace(rt.Start), cs, enter, o, others),
	}, me, nil
}

func writeJSONResp(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (s *liveServer) registerSignalMarshal(mux *http.ServeMux) {
	mux.HandleFunc("/marshal/api/race", s.sigRace)
	mux.HandleFunc("/marshal/api/analyse", s.sigAnalyse)
	mux.HandleFunc("/marshal/api/auto", s.sigAuto)
	mux.HandleFunc("/marshal/api/apply", s.sigApply)
	mux.HandleFunc("/marshal/api/calibrate", s.sigCalibrate)
}

// sigRace: the race's traces, each pilot's saved laps and lines, and what
// auto-detect makes of each.
func (s *liveServer) sigRace(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	race := r.FormValue("race")
	rt, src, err := s.loadTraces(race, r.FormValue("fresh") == "1")
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	target, holeshot := s.raceRules(race)
	o := rt.options(target, holeshot)
	out := struct {
		Race     string     `json:"race"`
		Where    string     `json:"where"`
		Holeshot bool       `json:"holeshot"`
		Target   int        `json:"target"`
		MinLapMs float64    `json:"minLapMs"`
		Pilots   []sigPilot `json:"pilots"`
	}{Race: race, Where: src.where(), Holeshot: holeshot, Target: target, MinLapMs: rt.Rules.MinLapMs}
	for _, p := range rt.Pilots {
		sp := sigPilot{Pilot: p.Pilot, Name: p.Name, Node: p.Node, Freq: p.Freq, SavedEnter: p.Enter, SavedExit: p.Exit}
		n := len(p.Times)
		if len(p.Values) < n {
			n = len(p.Values)
		}
		for i := 0; i < n; i++ {
			sp.T = append(sp.T, math.Round((p.Times[i]-rt.Start)*1000)/1000)
			sp.V = append(sp.V, p.Values[i])
		}
		for _, l := range p.Laps {
			if !l.Deleted {
				sp.Saved = append(sp.Saved, l.StampMs)
			}
		}
		if sg, ok := marshal.AutoDetect(p.trace(rt.Start), o); ok {
			sp.Suggest = &sg
		}
		out.Pilots = append(out.Pilots, sp)
	}
	writeJSONResp(w, out, nil)
}

type sigReq struct {
	Race    string    `json:"race"`
	Pilot   string    `json:"pilot"`
	Enter   float64   `json:"enter"`
	Exit    float64   `json:"exit"`
	Removed []float64 `json:"removed"`
	Added   []float64 `json:"added"`
}

func readSigReq(r *http.Request) (sigReq, error) {
	var q sigReq
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&q)
	if err == nil && q.Enter > 0 && q.Exit >= q.Enter {
		err = errors.New("the exit line must be below the enter line")
	}
	return q, err
}

func (s *liveServer) sigAnalyse(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	q, err := readSigReq(r)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	rt, _, err := s.loadTraces(q.Race, false)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	res, _, err := s.analyse(rt, q.Race, q.Pilot, q.Enter, q.Exit, q.Removed, q.Added)
	writeJSONResp(w, res, err)
}

func (s *liveServer) sigAuto(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	q, err := readSigReq(r)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	rt, _, err := s.loadTraces(q.Race, false)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	target, holeshot := s.raceRules(q.Race)
	for _, p := range rt.Pilots {
		if p.Pilot != q.Pilot {
			continue
		}
		sg, ok := marshal.AutoDetect(p.trace(rt.Start), rt.options(target, holeshot))
		if !ok {
			writeJSONResp(w, nil, errors.New("this pilot's signal has no passes that stand clear of the background, so there are no lines to suggest"))
			return
		}
		res, _, err := s.analyse(rt, q.Race, q.Pilot, sg.Enter, sg.Exit, nil, nil)
		writeJSONResp(w, struct {
			sigResult
			Suggest marshal.Suggestion `json:"suggest"`
		}{res, sg}, err)
		return
	}
	writeJSONResp(w, nil, errors.New("that pilot didn't fly this race"))
}

// sigApply saves the pilot's laps as shown: the crossings at these lines, with
// the race director's removals and additions.
func (s *liveServer) sigApply(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	q, err := readSigReq(r)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	rt, src, err := s.loadTraces(q.Race, false)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	res, me, err := s.analyse(rt, q.Race, q.Pilot, q.Enter, q.Exit, q.Removed, q.Added)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	var laps []savedLap
	for _, c := range res.Crossings {
		src := 2 // recalculated
		if c.Why == "added by the race director" {
			src = 1 // manual
		}
		laps = append(laps, savedLap{StampMs: math.Round(c.StampMs), Peak: c.Peak, Deleted: c.Deleted, Source: src})
	}
	if err := src.replaceLaps(q.Race, *me, laps, q.Enter, q.Exit); err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	// The saved lines and laps are now these: read the race again next time.
	marshalTraces.mu.Lock()
	delete(marshalTraces.m, q.Race)
	marshalTraces.mu.Unlock()
	go func() {
		time.Sleep(700 * time.Millisecond)
		if s.stages != nil {
			s.stages.syncAll(false)
		}
	}()
	writeJSONResp(w, map[string]any{"ok": true, "laps": len(res.Laps)}, nil)
}

func (s *liveServer) sigCalibrate(w http.ResponseWriter, r *http.Request) {
	if !localSetupAllowed(r) {
		setupDenied(w)
		return
	}
	q, err := readSigReq(r)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	rt, src, err := s.loadTraces(q.Race, false)
	if err != nil {
		writeJSONResp(w, nil, err)
		return
	}
	for _, p := range rt.Pilots {
		if p.Pilot == q.Pilot {
			if p.Node < 0 {
				writeJSONResp(w, nil, errors.New("this timer didn't say which node the pilot was on"))
				return
			}
			writeJSONResp(w, map[string]any{"ok": true, "node": p.Node + 1}, src.calibrate(p.Node, q.Enter, q.Exit))
			return
		}
	}
	writeJSONResp(w, nil, errors.New("that pilot didn't fly this race"))
}
