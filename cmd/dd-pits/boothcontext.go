package main

// The single definition of what each beat is told.
//
// This exists because the audition and the live booth had drifted: staged and
// launch were auditioned on a lineup-only race with no meeting context, while
// live they get the full race AND the meeting. An audition on different input
// from the real thing is worse than none — it says a model is fine on data it
// will never be given.
//
// Comparing two implementations with a test would only catch drift after it
// happened. There is one implementation instead, so drift is not expressible:
// the live queue sites and the audition both call beatContext, and anything
// only the running agent knows arrives through beatExtrasCtx rather than being
// assembled twice.

import (
	"fmt"
	"strings"

	boothpkg "github.com/bob9/dd-pits/internal/booth"
	"github.com/bob9/dd-pits/internal/ddapi"
)

// beatExtrasCtx is the context only the running agent can supply.
//
// Empty is valid, and is exactly what the live booth passes for a meeting with
// no bracket — so an audition without one is not a different code path, it is
// the same path with the same empty strings.
type beatExtrasCtx struct {
	BracketRace      string // this heat's place in a bracket
	BracketStructure string // the bracket's shape and advancement rules
	PilotNotes       string // public-profile colour for the line-up
	LadderMove       string // who moved where on the qualifying board
	BoardMove        string // what this heat CHANGED on the standings
}

// rosterFence names every pilot who exists, positively.
//
// The prompt already said "never invent" four separate times, and a 4B writer
// invented a pilot anyway — announced "Mirella" in a heat of bob9, Willman and
// Wizz. A prohibition asks the model for restraint; small models do not
// reliably have any. A whitelist DEFINES THE WORLD instead: everyone who can be
// spoken about is on one short line, and a name not on it has nothing in the
// context to attach to.
//
// Built from the heat AND the meeting board, because the finish beat
// legitimately talks about pilots from earlier heats when it reads the
// standings — fencing to the heat alone would make the leaderboard unspeakable.
func rosterFence(r ddapi.Race, mv *ddapi.Meeting) string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n != "" && !seen[strings.ToLower(n)] {
			seen[strings.ToLower(n)] = true
			names = append(names, n)
		}
	}
	for _, p := range r.Pilots {
		add(p.Name)
	}
	if mv != nil {
		for _, s := range mv.Board {
			add(s.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "\nTHE ONLY PILOTS WHO EXIST: " + strings.Join(names, ", ") + ".\n" +
		"No other person may be named or referred to. If a name is not on that " +
		"list, it is not in this meeting and must not be spoken.\n"
}

// beatContext assembles what the model is given for one beat.
//
// Every case mirrors a live queue site in booth.go. The per-kind routing is the
// thing that must not be duplicated, and the tests pin it.
func beatContext(kind string, r ddapi.Race, mv *ddapi.Meeting, x beatExtrasCtx) string {
	return beatContextBody(kind, r, mv, x) + rosterFence(r, mv)
}

// beatContextBody is the per-kind routing; the roster fence rides on every beat
// because an invented pilot is wrong in all of them.
func beatContextBody(kind string, r ddapi.Race, mv *ddapi.Meeting, x beatExtrasCtx) string {
	switch kind {
	case "meeting_open", "filler":
		// Meeting-level: no single heat is the subject.
		return boothpkg.MeetingContext(mv) + x.BracketStructure + boothpkg.UpNextContext(mv)

	case "meeting_talk":
		return boothpkg.MeetingContext(mv) + boothpkg.RaceContext(r, mv, false) +
			x.BracketRace + x.BracketStructure + boothpkg.UpNextContext(mv)

	case "staged":
		// Before the flag. The race carries no lap times because it has not run —
		// a property of the data, not something to withhold.
		return boothpkg.MeetingContext(mv) + boothpkg.RaceContext(r, mv, false) +
			x.BracketRace + x.PilotNotes

	case "launch":
		return boothpkg.MeetingContext(mv) + boothpkg.RaceContext(r, mv, false) + x.BracketRace

	case "holeshot", "progress", "pilot_out", "pilot_back":
		return boothpkg.RaceContext(r, mv, false) + x.BracketRace

	case "qual_move":
		// The ladder move is the whole point of this beat. Without it the model
		// guesses who moved — and it did, announcing the slowest pilot on the
		// board leaping to second with a "blistering" time.
		return boothpkg.MeetingContext(mv) + x.LadderMove

	case "finish":
		// BoardMove is what makes this a standings UPDATE rather than a reading.
		//
		// The board is a snapshot: it says where everyone sits, not what just
		// changed. Given only that, the call lists positions — "bob9 takes the
		// top spot, Willman holds second" — which sounds like movement while
		// stating none, because the model has no before to compare against. The
		// same absence made qual_move invent a mover outright.
		return boothpkg.MeetingContext(mv) + x.BoardMove + boothpkg.RaceContext(r, mv, false) +
			x.BracketRace + x.BracketStructure + boothpkg.UpNextContext(mv)

	default:
		return boothpkg.RaceContext(r, mv, false) + x.BracketRace
	}
}

// ladderMoveLine is the one fact the qual_move beat is built around.
//
// Shared, so an audition cannot describe a move in a different shape from the
// one the live booth reports.
func ladderMoveLine(pilot, time string, from, to int) string {
	return fmt.Sprintf("\nLADDER MOVE: %s has just improved to %s and climbed from P%d to P%d "+
		"on the QUALIFYING leaderboard. Call the new personal best and the move up the ladder.\n",
		pilot, time, from, to)
}

// boardMoveLine states what a heat changed on the standings.
//
// Computed rather than left to the model, for the same reason the gaps are:
// anything needing a comparison ACROSS pilots, or against an earlier state, is
// something a model will infer plausibly and often wrongly. Here it cannot even
// infer it — the previous board is not in the data at all.
func boardMoveLine(moves []boardMove) string {
	if len(moves) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nSTANDINGS CHANGE (what THIS heat just did to the board — lead with it):\n")
	for _, m := range moves {
		switch {
		case m.From == 0:
			fmt.Fprintf(&b, "- %s enters the board at P%d\n", m.Name, m.To)
		case m.To < m.From:
			fmt.Fprintf(&b, "- %s climbs from P%d to P%d\n", m.Name, m.From, m.To)
		case m.To > m.From:
			fmt.Fprintf(&b, "- %s drops from P%d to P%d\n", m.Name, m.From, m.To)
		default:
			fmt.Fprintf(&b, "- %s holds P%d\n", m.Name, m.To)
		}
	}
	b.WriteString("Pilots not listed did not move.\n")
	return b.String()
}

// boardMove is one pilot's change of position.
type boardMove struct {
	Name     string
	From, To int // 0 = was not on the board
}

// boardMovesSince compares the board against the positions held before, and
// reports only what actually changed.
func boardMovesSince(prev map[string]int, board []ddapi.Standing) []boardMove {
	var out []boardMove
	for _, st := range board {
		was := prev[st.Name]
		if was == st.Pos {
			continue // no movement is not news
		}
		out = append(out, boardMove{Name: st.Name, From: was, To: st.Pos})
	}
	return out
}
