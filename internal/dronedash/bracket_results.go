package dronedash

import "sort"

// BracketFinisher is one pilot's finish in a completed heat, carrying the
// external pilot id needed to map back to a race-bracket pilot.
type BracketFinisher struct {
	ExternalPilotID int    // external system's pilot id (race_bracket_pilots.id); 0 when unset
	PilotName       string // FPVTrackside pilot name (fallback when id is blank)
	Position        int    // 1-indexed finishing position
}

// BracketRaceResult is a finished heat that carries an external race id, with
// its finishers ordered by finishing position.
type BracketRaceResult struct {
	ExternalRaceID int // a bracket_races.id; resolves to {bracket, bracket race string}
	Finishers      []BracketFinisher
}

// BracketResults extracts, from this snapshot, every finished heat that carries
// an ExternalRaceID — so the caller can route results back into a race bracket.
// A heat qualifies when it has a non-zero ExternalRaceID, an End time
// (finished), and at least one positioned result.
//
// Finishers are ordered by the heat's actual finish-line order, computed from
// the lap timing (most racing laps up to the target, then lowest total time
// INCLUDING the holeshot). FPVTrackside's reported Position is NOT trusted as
// the primary signal: it can arrive time-trial-ranked (ordered by fastest laps,
// ignoring a slow start) or provisional on a not-yet-finalised heat. Both bit
// us — a chase-the-ace final was handed to the pilot with the fastest laps who
// actually crossed the line LAST, ending the series early on the wrong champion.
// Bracket heats are always race-scored (first across the line wins), so the lap
// finish-line order is authoritative; the reported Position is used only as a
// last resort when the heat carries no usable lap timing.
// Races are iterated in snapshot order for determinism.
func (s Snapshot) BracketResults() []BracketRaceResult {
	pilotByID := make(map[string]ftPilot, len(s.Pilots))
	for _, p := range s.Pilots {
		pilotByID[p.ID] = p
	}
	resultsByRace := map[string][]ftResult{}
	for _, res := range s.Results {
		resultsByRace[res.Race] = append(resultsByRace[res.Race], res)
	}

	var out []BracketRaceResult
	for _, race := range s.Races {
		if race.ExternalID == 0 || parseFTTime(race.End).IsZero() {
			continue // unlinked or not finished
		}
		results := resultsByRace[race.ID]
		if len(results) == 0 {
			continue
		}
		fin := bracketFinishersByLaps(race, results, pilotByID)
		if fin == nil {
			// No usable lap timing — fall back to the reported positions.
			fin = bracketFinishersByPosition(results, pilotByID)
		}
		if len(fin) == 0 {
			continue
		}
		out = append(out, BracketRaceResult{ExternalRaceID: race.ExternalID, Finishers: fin})
	}
	return out
}

// bracketFinishersByPosition returns the finishers ordered by reported Position,
// but ONLY when those positions form a clean distinct ranking (every result has
// a positive, non-duplicated position). Returns nil when the positions are
// degenerate (missing or duplicated), signalling the caller to fall back.
func bracketFinishersByPosition(results []ftResult, pilotByID map[string]ftPilot) []BracketFinisher {
	sorted := append([]ftResult(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Position < sorted[j].Position })
	fin := make([]BracketFinisher, 0, len(sorted))
	seen := map[int]bool{}
	for _, res := range sorted {
		if res.Position <= 0 || seen[res.Position] {
			return nil // not a clean ranking — caller falls back to laps
		}
		seen[res.Position] = true
		p := pilotByID[res.Pilot]
		fin = append(fin, BracketFinisher{
			ExternalPilotID: p.ExternalID,
			PilotName:       p.Name,
			Position:        res.Position,
		})
	}
	return fin
}

// bracketFinishersByLaps ranks the heat's pilots by their actual finish-line
// order: most racing laps completed up to the target, then lowest total time.
// This is the race result — who reaches the target lap first — so it:
//   - caps laps at race.TargetLaps (a pilot who kept flying extra laps past the
//     finish doesn't outrank one who stopped on the target), and
//   - includes the holeshot (LapNumber 0) in the total time (a slow start costs
//     you the race; excluding it would reward a fast-lap / slow-start pilot who
//     actually crossed the line last — the exact CTA-final bug).
//
// Laps are attributed to pilots via detections (lap.Detection → detection.Pilot);
// only valid detections count and a double-detected lap number is counted once.
// Returns nil when no pilot has any racing lap, so the caller can fall back to
// the reported positions.
func bracketFinishersByLaps(race ftRace, results []ftResult, pilotByID map[string]ftPilot) []BracketFinisher {
	detByID := make(map[string]ftDetection, len(race.Detections))
	for _, d := range race.Detections {
		detByID[d.ID] = d
	}
	type agg struct {
		laps  int
		total float64
		seen  map[int]bool // dedup double-detected laps by lap number
	}
	stat := map[string]*agg{}
	anyLaps := false
	for _, l := range race.Laps {
		// Ignore laps beyond the target — the race is decided on the first
		// TargetLaps laps. TargetLaps <= 0 means "no cap" (count every lap).
		if race.TargetLaps > 0 && l.LapNumber > race.TargetLaps {
			continue
		}
		d, ok := detByID[l.Detection]
		if !ok || !d.Valid || d.Pilot == "" {
			continue
		}
		a := stat[d.Pilot]
		if a == nil {
			a = &agg{seen: map[int]bool{}}
			stat[d.Pilot] = a
		}
		if a.seen[l.LapNumber] {
			continue
		}
		a.seen[l.LapNumber] = true
		a.total += l.LengthSeconds // holeshot (lap 0) included in elapsed time
		if l.LapNumber >= 1 {
			a.laps++ // a racing lap toward the target (holeshot is lap 0)
			anyLaps = true
		}
	}
	if !anyLaps {
		return nil
	}
	// One row per pilot that has a result in this heat (dedup by pilot src id).
	type row struct {
		pilotSrc string
		laps     int
		total    float64
	}
	var rows []row
	seen := map[string]bool{}
	for _, res := range results {
		if res.Pilot == "" || seen[res.Pilot] {
			continue
		}
		seen[res.Pilot] = true
		r := row{pilotSrc: res.Pilot}
		if a := stat[res.Pilot]; a != nil {
			r.laps, r.total = a.laps, a.total
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].laps != rows[j].laps {
			return rows[i].laps > rows[j].laps
		}
		if rows[i].total > 0 && rows[j].total > 0 {
			return rows[i].total < rows[j].total
		}
		return rows[i].total > 0 // a pilot with a recorded time ranks ahead of one without
	})
	fin := make([]BracketFinisher, 0, len(rows))
	for i, r := range rows {
		p := pilotByID[r.pilotSrc]
		fin = append(fin, BracketFinisher{
			ExternalPilotID: p.ExternalID,
			PilotName:       p.Name,
			Position:        i + 1,
		})
	}
	return fin
}
