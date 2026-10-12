package raceformat

import "sort"

// FinalOrdering returns a best-effort full finishing order for every pilot the
// format has raced, suitable for pre-filling a race director's results-review
// screen.
//
// It starts from Standings() — authoritative as far as it goes — and appends any
// pilot not already ranked, ordered by the latest round they appeared in
// (descending: pilots who survived to a deeper round finish higher), with seed as
// the tiebreak. For ladder and mains, Standings() already ranks everyone, so the
// appended tail is empty and the result equals Standings(). For elimination,
// Standings() yields only the champion (or the ordered chase-the-ace finalists),
// so this fills the knocked-out pilots in beneath them.
//
// Appended rows carry Tied=true to flag that their placement is inferred rather
// than decided on the track — the review UI surfaces this so the RD can adjust.
// seeds maps pilot name to 1-based seed; missing entries sort last within a round.
func FinalOrdering(f Format, seeds map[string]int) []Standing {
	ranked := f.Standings()
	seen := make(map[string]bool, len(ranked))
	out := make([]Standing, 0, len(ranked))
	for _, s := range ranked {
		if seen[s.Pilot] { // Standings may list a pilot twice (e.g. a 2-pilot CTA)
			continue
		}
		seen[s.Pilot] = true
		out = append(out, s)
	}

	// Latest round each pilot appeared in, plus a stable roster in first-seen order.
	lastRound := map[string]int{}
	var roster []string
	for _, rv := range f.Races() {
		for _, p := range rv.Pilots {
			if _, ok := lastRound[p]; !ok {
				roster = append(roster, p)
			}
			if rv.Round > lastRound[p] {
				lastRound[p] = rv.Round
			}
		}
	}

	rest := make([]string, 0, len(roster))
	for _, p := range roster {
		if !seen[p] {
			rest = append(rest, p)
		}
	}
	seedOf := func(p string) int {
		if s, ok := seeds[p]; ok {
			return s
		}
		return 1 << 30
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if lastRound[rest[i]] != lastRound[rest[j]] {
			return lastRound[rest[i]] > lastRound[rest[j]]
		}
		return seedOf(rest[i]) < seedOf(rest[j])
	})

	for _, p := range rest {
		out = append(out, Standing{Pilot: p, Tied: true})
	}
	// Renumber contiguously so ranks are always 1..N regardless of any gaps or
	// duplicates Standings() may have carried.
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}
