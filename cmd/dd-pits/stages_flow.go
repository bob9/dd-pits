package main

// Where each seat in a bracket comes from.
//
// The bracket engines decide who goes where as results arrive; they don't
// say in advance which finishing place of which race fills a seat. dd-pits
// works it out by playing the rest of the bracket forward on a copy, with
// stand-in results, and noting the race and place each pilot arrived from.
// Seats already filled come from the real results the same way. The bracket
// page shows it on every empty seat ("1st · Winners R1 · Race 2") and draws
// the lines between races.

import (
	"sort"
	"strings"

	"github.com/bob9/dd-pits/internal/stages/raceformat"
)

// seatSource is the race and finishing place a seat is filled from; Rank 0
// with no race is a seed.
type seatSource struct {
	From  string `json:"from"`
	Rank  int    `json:"rank"`
	Pilot string `json:"-"` // the real pilot, once known
}

// raceFlow is a bracket's progression: per race, where its seats come from.
type raceFlow struct {
	// Known holds the sources of seats with a pilot in them, by pilot.
	Known map[string]map[string]seatSource
	// Open lists, per race, the sources of seats still to be filled, best
	// place first.
	Open map[string][]seatSource
}

// resultOrder is a completed race's result: advancing pilots first.
func resultOrder(rv raceformat.RaceView) []string {
	if !rv.IsComplete {
		return nil
	}
	return append(append([]string{}, rv.Winners...), rv.Losers...)
}

// stageFlow works out a bracket's progression. Points brackets race the same
// pilots every round, so they have none.
func stageFlow(st stageRecord) raceFlow {
	flow := raceFlow{Known: map[string]map[string]seatSource{}, Open: map[string][]seatSource{}}
	if st.Format == raceformat.KindMains {
		return flow
	}
	f, err := st.engine()
	if err != nil {
		return flow
	}
	real := f.Races()

	// Seats already filled: each pilot came from the last finished race
	// before this one that they flew in.
	for i, rv := range real {
		for _, p := range rv.Pilots {
			for j := i - 1; j >= 0; j-- {
				if pos := indexOf(resultOrder(real[j]), p); pos >= 0 {
					if flow.Known[rv.ID] == nil {
						flow.Known[rv.ID] = map[string]seatSource{}
					}
					flow.Known[rv.ID][p] = seatSource{From: real[j].ID, Rank: pos + 1, Pilot: p}
					break
				}
			}
		}
	}

	// Seats to come: run the rest on a copy, each race finishing in the
	// order its pilots are listed, and see who lands where.
	sim, err := raceformat.FromState(st.Format, st.config(), st.State)
	if err != nil {
		return flow
	}
	last := map[string]seatSource{} // pilot -> latest stand-in finish
	seated := map[string]map[string]bool{}
	for _, rv := range real {
		seated[rv.ID] = map[string]bool{}
		for _, p := range rv.Pilots {
			seated[rv.ID][p] = true
		}
	}
	for step := 0; step < 1000; step++ {
		races := sim.Races()
		applied := false
		for _, rv := range races {
			if rv.IsComplete || rv.IsVoid || rv.Locked || len(rv.Pilots) == 0 || !ready(rv, races) {
				continue
			}
			if sim.ApplyPlacement(rv.ID, rv.Pilots) != nil {
				continue
			}
			for i, p := range rv.Pilots {
				last[p] = seatSource{From: rv.ID, Rank: i + 1}
			}
			applied = true
			break
		}
		if !applied {
			break
		}
		for _, rv := range sim.Races() {
			if seated[rv.ID] == nil {
				seated[rv.ID] = map[string]bool{}
			}
			for _, p := range rv.Pilots {
				if seated[rv.ID][p] {
					continue
				}
				seated[rv.ID][p] = true
				if src, ok := last[p]; ok && src.From != rv.ID {
					flow.Open[rv.ID] = append(flow.Open[rv.ID], src)
				}
			}
		}
	}
	return flow
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// shortRaceTitle fits a race's title on a seat: "Winners Round 1 — Race 2"
// becomes "Winners R1 · Race 2".
func shortRaceTitle(t string) string {
	t = strings.ReplaceAll(t, "Round ", "R")
	return strings.ReplaceAll(t, " — ", " · ")
}

// flowEdge is a line on the bracket page between two races.
type flowEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Ranks []int  `json:"ranks"`
	Drop  bool   `json:"drop"` // winners' bracket into the losers'
}

// flowEdges lists the lines to draw, one per pair of races, with the places
// that travel along each.
func flowEdges(flow raceFlow, titles map[string]string) []flowEdge {
	idx := map[[2]string]int{}
	var out []flowEdge
	add := func(from, to string, rank int) {
		if from == "" || from == to {
			return
		}
		// Chase the ace re-races the same finalists: one line into its first
		// race, not one into every race of the series.
		if strings.HasPrefix(titles[to], "Chase the Ace") && !strings.HasSuffix(titles[to], "Race 1") {
			return
		}
		k := [2]string{from, to}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, flowEdge{From: from, To: to,
				Drop: strings.HasPrefix(titles[from], "Winners") && strings.HasPrefix(titles[to], "Losers")})
		}
		for _, r := range out[i].Ranks {
			if r == rank {
				return
			}
		}
		out[i].Ranks = append(out[i].Ranks, rank)
	}
	for to, seats := range flow.Known {
		for _, s := range seats {
			add(s.From, to, s.Rank)
		}
	}
	for to, seats := range flow.Open {
		for _, s := range seats {
			add(s.From, to, s.Rank)
		}
	}
	// In a fixed order, so an unchanged bracket renders identically and the
	// page doesn't redraw for nothing.
	for i := range out {
		sort.Ints(out[i].Ranks)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}
