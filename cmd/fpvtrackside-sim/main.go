// fpvtrackside-sim serves a simulated FPVTrackside race meeting over the same
// HTTP API dd-pits reads, so dd-pits can be developed, demonstrated and
// screenshotted without a timing system.
//
// Heats run back to back in real time: each one is staged, raced over three
// laps, and finished, then the next heat is cued. After a set number of rounds
// the meeting starts over as a new event.
//
// Usage:
//
//	fpvtrackside-sim -addr 127.0.0.1:8080
//	dd-pits -fpvtrackside http://127.0.0.1:8080
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

var pilotNames = []string{"Vortex", "Kestrel", "NitroNova", "Blitz", "Hornet", "Zephyr",
	"Glitch", "Raptor", "Comet", "Torque", "Pixel", "Ranger"}

var channels = []struct {
	ID, Short, Color string
	Num, Freq        int
}{
	{"ch-r1", "R", "#ff3b30", 1, 5658}, {"ch-r2", "R", "#34c759", 2, 5695},
	{"ch-r3", "R", "#0a84ff", 3, 5732}, {"ch-r4", "R", "#ffd60a", 4, 5769},
}

const (
	heatsPerRound = 3
	pilotsPerHeat = 4
	targetLaps    = 3
)

// sim derives the whole meeting from the clock, so there is no state to keep
// consistent between requests.
type sim struct {
	stage, gap time.Duration // cued-but-not-started time, and the pause after a finish
	rounds     int           // rounds before the meeting restarts
	skill      []float64     // each pilot's typical lap, seconds
	epoch      time.Time
}

type heat struct {
	idx      int
	round    int
	num      int
	pilots   []int
	cueAt    time.Time
	start    time.Time
	laps     [][]float64 // per pilot: holeshot then each lap, seconds
	finishAt time.Time
}

func newSim(rounds int, stage, gap time.Duration) *sim {
	r := rand.New(rand.NewSource(42))
	s := &sim{stage: stage, gap: gap, rounds: rounds}
	for range pilotNames {
		s.skill = append(s.skill, 12.8+r.Float64()*3.4)
	}
	// Start a few heats in, so the board opens with history on it.
	s.epoch = time.Now().Add(-s.heatStart(4) - stage/2)
	return s
}

func (s *sim) totalHeats() int { return s.rounds * heatsPerRound }

// cycle is the meeting number and how far into it we are; the meeting repeats.
func (s *sim) cycle(now time.Time) (int, time.Duration) {
	length := s.heatStart(s.totalHeats())
	el := now.Sub(s.epoch)
	return int(el / length), el % length
}

// heatStart is the offset of heat i's cue from the start of a meeting.
// Every heat slot is the same length: staging, the longest possible race, gap.
func (s *sim) heatStart(i int) time.Duration {
	return time.Duration(i) * (s.stage + 75*time.Second + s.gap)
}

func (s *sim) heat(meeting, i int, base time.Time) heat {
	round, num := i/heatsPerRound+1, i%heatsPerRound+1
	r := rand.New(rand.NewSource(int64(meeting*1000 + i)))
	order := rand.New(rand.NewSource(int64(meeting*100 + round))).Perm(len(pilotNames))
	h := heat{idx: i, round: round, num: num}
	for k := 0; k < pilotsPerHeat; k++ {
		h.pilots = append(h.pilots, order[(num-1)*pilotsPerHeat+k])
	}
	h.cueAt = base.Add(s.heatStart(i))
	h.start = h.cueAt.Add(s.stage)
	var longest float64
	for _, p := range h.pilots {
		lt := []float64{1.9 + r.Float64()*1.6}
		total := lt[0]
		for l := 0; l < targetLaps; l++ {
			v := s.skill[p] + r.NormFloat64()*0.7
			if r.Float64() < 0.06 {
				v += 3 + r.Float64()*4 // a clipped gate
			}
			lt = append(lt, v)
			total += v
		}
		h.laps = append(h.laps, lt)
		if total > longest {
			longest = total
		}
	}
	h.finishAt = h.start.Add(time.Duration((longest + 1.5) * float64(time.Second)))
	return h
}

func ts(t time.Time) string { return t.Format("2006-01-02T15:04:05.000") }

func eventID(meeting int) string { return fmt.Sprintf("sim-meeting-%04d", meeting%10000) }

func raceID(meeting, i int) string { return fmt.Sprintf("m%d-race-%03d", meeting, i+1) }

func roundID(meeting, r int) string { return fmt.Sprintf("m%d-round-%d", meeting, r) }

// state is everything visible at now: the heats cued so far plus the next two.
type state struct {
	meeting int
	heats   []heat
	current int // index of the heat FPVTrackside has selected
}

func (s *sim) state(now time.Time) state {
	meeting, into := s.cycle(now)
	base := now.Add(-into)
	st := state{meeting: meeting}
	for i := 0; i < s.totalHeats(); i++ {
		h := s.heat(meeting, i, base)
		if h.cueAt.After(now) && i > st.current+2 {
			break // list two heats beyond the current one as pending
		}
		st.heats = append(st.heats, h)
		if !h.cueAt.After(now) {
			st.current = i
		}
	}
	return st
}

func (h heat) json(meeting int, now time.Time) map[string]any {
	var laps, dets, pcs []map[string]any
	id := raceID(meeting, h.idx)
	for i, p := range h.pilots {
		pid := fmt.Sprintf("pilot-%d", p)
		pcs = append(pcs, map[string]any{"ID": fmt.Sprintf("%s-pc%d", id, i), "Pilot": pid, "Channel": channels[i].ID})
		if h.start.After(now) {
			continue
		}
		t := h.start
		for ln, lt := range h.laps[i] {
			end := t.Add(time.Duration(lt * float64(time.Second)))
			if end.After(now) {
				break
			}
			did := fmt.Sprintf("%s-d%d-%d", id, i, ln)
			dets = append(dets, map[string]any{"ID": did, "Channel": channels[i].ID, "Time": ts(end), "Pilot": pid,
				"LapNumber": ln, "Valid": true, "ValidityType": "Auto", "IsLapEnd": true, "IsHoleshot": ln == 0})
			laps = append(laps, map[string]any{"ID": fmt.Sprintf("%s-l%d-%d", id, i, ln), "Detection": did,
				"LengthSeconds": lt, "LapNumber": ln, "StartTime": ts(t), "EndTime": ts(end)})
			t = end
		}
	}
	start, end := "0001-01-01T00:00:00", "0001-01-01T00:00:00"
	if !h.start.After(now) {
		start = ts(h.start)
	}
	if !h.finishAt.After(now) {
		end = ts(h.finishAt)
	}
	return map[string]any{"ID": id, "Laps": laps, "Detections": dets, "Start": start, "End": end,
		"PilotChannels": pcs, "RaceNumber": h.num, "Round": roundID(meeting, h.round), "TargetLaps": targetLaps,
		"Valid": true, "Event": eventID(meeting)}
}

func (h heat) results(meeting int, now time.Time) []map[string]any {
	out := []map[string]any{}
	if h.finishAt.After(now) {
		return out
	}
	idx := []int{0, 1, 2, 3}
	total := func(i int) float64 {
		s := 0.0
		for _, v := range h.laps[i] {
			s += v
		}
		return s
	}
	for a := range idx {
		for b := a + 1; b < len(idx); b++ {
			if total(idx[b]) < total(idx[a]) {
				idx[a], idx[b] = idx[b], idx[a]
			}
		}
	}
	points := []int{10, 7, 5, 3}
	for pos, i := range idx {
		out = append(out, map[string]any{"ID": fmt.Sprintf("%s-res%d", raceID(meeting, h.idx), pos),
			"Points": points[pos], "Position": pos + 1, "Valid": true, "Event": eventID(meeting),
			"Pilot": fmt.Sprintf("pilot-%d", h.pilots[i]), "Race": raceID(meeting, h.idx),
			"Round": roundID(meeting, h.round), "ResultType": "Race"})
	}
	return out
}

func (s *sim) handler() http.Handler {
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		st := s.state(now)
		p := r.URL.Path
		switch {
		case p == "/":
			fmt.Fprintf(w, `<html><script>var eventManager = new EventManager("events/%s");</script></html>`, eventID(st.meeting))
		case p == "/current":
			writeJSON(w, map[string]string{"CurrentRace": raceID(st.meeting, st.current)})
		case p == "/httpfiles/Channels.json":
			var out []map[string]any
			for _, c := range channels {
				out = append(out, map[string]any{"ID": c.ID, "Number": c.Num, "Band": "Raceband", "ShortBand": c.Short,
					"ChannelPrefix": c.Short, "Frequency": c.Freq, "DisplayName": fmt.Sprintf("%s%d", c.Short, c.Num)})
			}
			writeJSON(w, out)
		case strings.HasSuffix(p, "/Event.json"):
			var races, rounds, chs, colors []string
			for _, h := range st.heats {
				races = append(races, raceID(st.meeting, h.idx))
			}
			for i := 1; i <= st.heats[len(st.heats)-1].round; i++ {
				rounds = append(rounds, roundID(st.meeting, i))
			}
			for _, c := range channels {
				chs, colors = append(chs, c.ID), append(colors, c.Color)
			}
			writeJSON(w, []map[string]any{{"ID": eventID(st.meeting), "Name": "Twilight Cup — Demo Meeting",
				"EventType": "Race", "Start": ts(st.heats[0].cueAt), "Laps": targetLaps, "PBLaps": targetLaps,
				"Rounds": rounds, "Races": races, "Channels": chs, "ChannelColors": colors}})
		case strings.HasSuffix(p, "/Pilots.json"):
			var out []map[string]any
			for i, n := range pilotNames {
				out = append(out, map[string]any{"ID": fmt.Sprintf("pilot-%d", i), "Name": n})
			}
			writeJSON(w, out)
		case strings.HasSuffix(p, "/Rounds.json"):
			var out []map[string]any
			for i := 1; i <= st.heats[len(st.heats)-1].round; i++ {
				out = append(out, map[string]any{"ID": roundID(st.meeting, i), "Name": fmt.Sprintf("Round %d", i),
					"RoundNumber": i, "EventType": "Race", "RoundType": "Round", "Valid": true, "Order": i})
			}
			writeJSON(w, out)
		case strings.HasSuffix(p, "/Results.json"):
			out := []map[string]any{}
			for _, h := range st.heats {
				out = append(out, h.results(st.meeting, now)...)
			}
			writeJSON(w, out)
		case strings.HasSuffix(p, "/Race.json"), strings.HasSuffix(p, "/Result.json"):
			parts := strings.Split(p, "/")
			id := parts[len(parts)-2]
			for _, h := range st.heats {
				if raceID(st.meeting, h.idx) == id {
					if strings.HasSuffix(p, "/Race.json") {
						writeJSON(w, []any{h.json(st.meeting, now)})
					} else {
						writeJSON(w, h.results(st.meeting, now))
					}
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to serve the simulated FPVTrackside API on")
	rounds := flag.Int("rounds", 8, "rounds per meeting before it starts over")
	stage := flag.Duration("stage", 20*time.Second, "how long each heat is cued before it starts")
	gap := flag.Duration("gap", 10*time.Second, "pause after a heat finishes before the next is cued")
	flag.Parse()
	s := newSim(*rounds, *stage, *gap)
	log.Printf("fpvtrackside-sim: serving a simulated meeting on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.handler()))
}
