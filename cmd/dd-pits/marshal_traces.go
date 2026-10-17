package main

// Signal traces for marshalling: where they come from.
//
// A RotorHazard timer keeps every pilot's signal (RSSI) trace with each saved
// race. dd-pits reads them two ways:
//
//   - the dd-pits plugin on the RotorHazard (/dd-pits/api/marshal-traces),
//     whether dd-pits runs on that RotorHazard or beside an FPVTrackside that
//     times with it;
//   - otherwise, beside FPVTrackside, the FPVTrackside connector plugin's own
//     marshal messages (ts_race_marshal_waveform / ts_race_marshal_update),
//     the same ones FPVTrackside's laps screen uses.
//
// Corrections are saved through RotorHazard's own marshal save either way, so
// results, brackets and (through the connector) FPVTrackside all follow.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bob9/dd-pits/internal/dronedash"
	"github.com/bob9/dd-pits/internal/marshal"
)

// savedLap is one lap-line crossing as the timer stores it.
type savedLap struct {
	ID      int64   `json:"id,omitempty"`
	StampMs float64 `json:"lap_time_stamp"`
	Peak    float64 `json:"peak_rssi"`
	Deleted bool    `json:"deleted"`
	Source  int     `json:"source"`
}

// pilotTrace is one pilot's run: their signal, the lines in force and laps.
type pilotTrace struct {
	Pilot   string     `json:"pilot"` // the timer's reference for saving
	Name    string     `json:"callsign"`
	Node    int        `json:"node"`
	Freq    int        `json:"frequency"`
	Enter   float64    `json:"enter_at"`
	Exit    float64    `json:"exit_at"`
	Times   []float64  `json:"history_times"`
	Values  []float64  `json:"history_values"`
	Laps    []savedLap `json:"laps"`
	TSPilot string     `json:"ts_pilot_id"`
}

// raceTraces is a whole race, ready to marshal.
type raceTraces struct {
	Race   string       `json:"race"`
	Start  float64      `json:"start_time"`
	Rules  traceRules   `json:"rules"`
	Pilots []pilotTrace `json:"pilots"`
}

type traceRules struct {
	MinLapMs           float64 `json:"min_lap_ms"`
	MinLapDiscard      bool    `json:"min_lap_discard"`
	MinFirstCrossingMs float64 `json:"min_first_crossing_ms"`
	StartLowerPct      float64 `json:"start_lower_pct"`
	StartLowerSecs     float64 `json:"start_lower_secs"`
	StartDelayMax      float64 `json:"start_delay_max"`
}

func (r raceTraces) options(targetLaps int, holeshot bool) marshal.Options {
	return marshal.Options{
		MinLapMs: r.Rules.MinLapMs, MinLapDiscard: r.Rules.MinLapDiscard,
		MinFirstCrossingMs: r.Rules.MinFirstCrossingMs,
		StartLowerPct:      r.Rules.StartLowerPct, StartLowerSecs: r.Rules.StartLowerSecs,
		StartDelayMax: r.Rules.StartDelayMax, TargetLaps: targetLaps, HoleshotCounts: holeshot,
	}
}

func (p pilotTrace) trace(start float64) marshal.Trace {
	return marshal.Trace{Times: p.Times, Values: p.Values, Start: start}
}

// traceSource gives a timer's traces and saves corrections.
type traceSource interface {
	// traces reads a saved race; race is the dd-pits race ID.
	traces(race string) (*raceTraces, error)
	// replaceLaps saves a pilot's whole corrected lap list and the lines
	// that produced it.
	replaceLaps(race string, p pilotTrace, laps []savedLap, enter, exit float64) error
	// calibrate sets a node's lines for the races to come.
	calibrate(node int, enter, exit float64) error
	where() string
}

var errNoTraces = errors.New("this timer doesn't keep signal traces")

// ── the dd-pits plugin on a RotorHazard ──────────────────────────────────────

type rhPluginTraces struct {
	base, token string
	// raceRef turns a dd-pits race ID into the plugin's ("rh-h…" stays,
	// an FPVTrackside race becomes "ts:<id>").
	raceRef func(string) string
	label   string
}

func (c rhPluginTraces) api(path string) string {
	return strings.TrimSuffix(c.base, "/") + "/dd-pits/api/" + path
}

func (c rhPluginTraces) traces(race string) (*raceTraces, error) {
	var out raceTraces
	err := postJSON(c.api("marshal-traces"), map[string]any{"race": c.raceRef(race)}, c.token, &out)
	if errors.Is(err, errNoStageAPI) {
		return nil, errNoTraces
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c rhPluginTraces) replaceLaps(race string, p pilotTrace, laps []savedLap, enter, exit float64) error {
	return postJSON(c.api("marshal"), map[string]any{
		"race": c.raceRef(race), "pilot": p.Pilot, "action": "replace",
		"laps": laps, "enter_at": enter, "exit_at": exit,
	}, c.token, nil)
}

func (c rhPluginTraces) calibrate(node int, enter, exit float64) error {
	return postJSON(c.api("marshal-calibrate"), map[string]any{"node": node, "enter_at": enter, "exit_at": exit}, c.token, nil)
}

func (c rhPluginTraces) where() string { return c.label }

// ── the RotorHazard behind an FPVTrackside ───────────────────────────────────

// ftRHTimer finds the RotorHazard an FPVTrackside times with: dd-pits'
// setting if given, otherwise FPVTrackside's own timer settings.
func ftRHTimer(cfg *localConfig) (url, key string) {
	t := cfg.RHTimerConfig()
	if t.URL != "" {
		return t.URL, t.Key
	}
	return detectFTRotorHazard(), t.Key
}

// detectFTRotorHazard reads FPVTrackside's TimingSystemSettings.xml for a
// RotorHazard timer's address.
func detectFTRotorHazard() string {
	for _, dir := range ftDataDirs() {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", "TimingSystemSettings.xml"))
		matches = append(matches, filepath.Join(dir, "TimingSystemSettings.xml"))
		for _, f := range matches {
			if u := rotorHazardInSettings(f); u != "" {
				return u
			}
		}
	}
	return ""
}

func ftDataDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, "Library", "Application Support", "FPVTrackside", "data"),
		filepath.Join(home, "AppData", "Roaming", "FPVTrackside", "data"),
		filepath.Join(home, ".local", "share", "FPVTrackside", "data"),
	}
	for _, d := range candidateTracksideDirs() {
		dirs = append(dirs, d, filepath.Join(d, "data"))
	}
	return dirs
}

func rotorHazardInSettings(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Items []struct {
			Type     string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
			HostName string `xml:"HostName"`
			Port     int    `xml:"Port"`
		} `xml:"TimingSystemSettings"`
	}
	if xml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	for _, it := range doc.Items {
		if strings.Contains(it.Type, "RotorHazard") && it.HostName != "" {
			port := it.Port
			if port == 0 {
				port = 5000
			}
			return fmt.Sprintf("http://%s:%d", it.HostName, port)
		}
	}
	return ""
}

// ftTraceSource is the RotorHazard behind FPVTrackside: through the dd-pits
// plugin when it's installed there, otherwise through the connector.
func (s *liveServer) ftTraceSource() (traceSource, error) {
	url, key := ftRHTimer(s.cfg)
	if url == "" {
		return nil, errNoTraces
	}
	plugin := rhPluginTraces{base: url, token: key, label: "RotorHazard at " + url,
		raceRef: func(id string) string { return "ts:" + id }}
	if pluginAnswers(url) {
		return plugin, nil
	}
	return &connectorTraces{base: url, snap: s.currentSnap}, nil
}

// pluginAnswers checks for the dd-pits plugin on a RotorHazard.
func pluginAnswers(base string) bool {
	resp, err := stageHTTP.Get(strings.TrimSuffix(base, "/") + "/dd-pits/api/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// currentSnap is the latest snapshot of the meeting.
func (s *liveServer) currentSnap() dronedash.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// ── through the FPVTrackside connector ───────────────────────────────────────

// connectorTraces reads traces through the FPVTrackside connector plugin on
// the RotorHazard (ts_race_marshal_waveform, connector 1.2.0 or later), with
// the laps from FPVTrackside itself. Saving goes back through the connector
// (ts_race_marshal_update), which resaves in RotorHazard and tells FPVTrackside.
type connectorTraces struct {
	base string
	snap func() dronedash.Snapshot
}

func (c *connectorTraces) where() string {
	return "RotorHazard at " + c.base + " (FPVTrackside connector)"
}

func (c *connectorTraces) traces(race string) (*raceTraces, error) {
	snap := c.snap()
	idx := -1
	for i := range snap.Races {
		if snap.Races[i].ID == race {
			idx = i
		}
	}
	if idx < 0 {
		return nil, errors.New("that race isn't in the meeting")
	}
	r := &snap.Races[idx]
	start := parseTime(r.Start)
	names := map[string]string{}
	for _, p := range snap.Pilots {
		names[p.ID] = p.Name
	}
	sio, err := dialSIO(c.base)
	if err != nil {
		return nil, fmt.Errorf("can't reach the RotorHazard timer at %s: %w", c.base, err)
	}
	defer sio.close()
	out := &raceTraces{Race: race}
	out.Rules.MinLapDiscard = false
	for _, pc := range r.PilotChannels {
		var w struct {
			Values []float64 `json:"history_values"`
			Times  []float64 `json:"history_times"`
			Enter  float64   `json:"enter_at"`
			Exit   float64   `json:"exit_at"`
			Start  float64   `json:"race_start_time"`
		}
		err := sio.call("ts_race_marshal_waveform", map[string]string{"race_id": race, "pilot_id": pc.Pilot}, &w, 15*time.Second)
		if errors.Is(err, errNoTraces) {
			continue // no trace kept for this pilot
		}
		if err != nil {
			if strings.Contains(err.Error(), "no answer") {
				return nil, errors.New("the FPVTrackside connector on that RotorHazard doesn't share signal traces yet (it needs version 1.2.0, or install the dd-pits plugin there)")
			}
			return nil, err
		}
		out.Start = w.Start
		pt := pilotTrace{Pilot: pc.Pilot, TSPilot: pc.Pilot, Name: names[pc.Pilot], Node: -1,
			Enter: w.Enter, Exit: w.Exit, Times: w.Times, Values: w.Values}
		for _, d := range r.Detections {
			if d.Pilot != pc.Pilot || !(d.IsLapEnd || d.IsHoleshot) || start.IsZero() {
				continue
			}
			at := parseTime(d.Time)
			if at.IsZero() {
				continue
			}
			pt.Laps = append(pt.Laps, savedLap{StampMs: float64(at.Sub(start).Milliseconds()), Deleted: !d.Valid})
		}
		out.Pilots = append(out.Pilots, pt)
	}
	if len(out.Pilots) == 0 {
		return nil, errNoTraces
	}
	return out, nil
}

func (c *connectorTraces) replaceLaps(race string, p pilotTrace, laps []savedLap, _, _ float64) error {
	sio, err := dialSIO(c.base)
	if err != nil {
		return err
	}
	defer sio.close()
	type lap struct {
		Deleted bool    `json:"deleted"`
		LapTime float64 `json:"lap_time"`
		Stamp   float64 `json:"lap_time_stamp"`
	}
	var out []lap
	prev := 0.0
	for _, l := range laps {
		x := lap{Deleted: l.Deleted, Stamp: l.StampMs}
		if !l.Deleted {
			x.LapTime = l.StampMs - prev
			prev = l.StampMs
		}
		out = append(out, x)
	}
	return sio.emit("ts_race_marshal_update", map[string]any{"race_id": race, "pilot_id": p.Pilot, "laps": out})
}

func (c *connectorTraces) calibrate(int, float64, float64) error {
	return errors.New("setting a node's lines needs the dd-pits plugin on that RotorHazard")
}
