package raceformat

import (
	"errors"
	"strings"
)

// trimNames trims whitespace and drops empty pilot names, preserving order.
func trimNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if t := strings.TrimSpace(n); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// samePilotSet checks that placement is a permutation of pilots (same multiset).
// Returns a descriptive error when it isn't.
func samePilotSet(pilots, placement []string) error {
	if len(placement) != len(pilots) {
		return errors.New("must rank every pilot in the race")
	}
	want := map[string]int{}
	for _, p := range pilots {
		want[p]++
	}
	for _, p := range placement {
		if want[p] == 0 {
			return errors.New("contains a pilot not in the race: " + p)
		}
		want[p]--
	}
	return nil
}

// removeAll returns src with every element present in drop removed (all copies).
func removeAll(src, drop []string) []string {
	if len(drop) == 0 {
		return src
	}
	rm := map[string]int{}
	for _, d := range drop {
		rm[d]++
	}
	out := src[:0:0]
	for _, s := range src {
		if rm[s] > 0 {
			rm[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}
