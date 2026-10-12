// Package raceformat is a pluggable abstraction over drone-racing event formats.
//
// The existing single/double elimination engine (internal/platform/bracket) is
// the first format, wrapped whole by EliminationFormat so its proven generation
// and advancement logic stays untouched. New formats (seeded ladder, points
// mains) are clean-room implementations of the Format interface.
//
// Every format persists its state as a JSON blob in race_brackets.bracket_state.
// A discriminator column (race_brackets.format) records which FormatKind a row
// uses, so FromState knows which concrete state struct to rehydrate.
package raceformat

import "encoding/json"

// FormatKind is the discriminator persisted on race_brackets.format.
type FormatKind string

const (
	// KindElimination is the legacy single/double elimination engine with a
	// grand-final or chase-the-ace finals. Default for existing rows.
	KindElimination FormatKind = "elimination"
	// KindLadder is the MultiGP-style seeded ladder (added in PR 2).
	KindLadder FormatKind = "ladder"
	// KindMains is the points-based A/B/C mains format (added in PR 3).
	KindMains FormatKind = "mains"
	// KindQualifier is the "Double Elimination — Top 4 Advance" qualifier: a
	// double-elim that ends in a 4-pilot final and outputs a ranked top-4 that
	// qualify onward (no single champion). Backed by internal/platform/qualbracket.
	KindQualifier FormatKind = "qualifier"
	// KindMainsProgression is a bump-up stack of qualifiers: one "Top 4 Advance"
	// bracket per Main (A = championship … lowest letter); the top finishers of
	// each main bump up into the next. Backed by internal/platform/mainsbracket.
	KindMainsProgression FormatKind = "mainsprogression"
)

// RaceView is the format-neutral projection of one heat that handlers, the
// ingest, and copy payloads consume. It is a superset of bracket.Race; GroupLabel
// generalises the hard-coded winners/losers/grand-final/chase-the-ace lanes into
// a free-form label ("Winners", "A Main", "Ladder Bout 2").
type RaceView struct {
	ID         string
	Round      int
	GroupLabel string
	// Title is the full human-friendly heading for the race, format-specific and
	// self-contained (e.g. "A Main — Round 1", "Winners Round 2 — Race 3",
	// "Chase the Ace — Heat 2", "Final"). Unlike GroupLabel it already folds in
	// the round/race/heat, so callers display it verbatim. Consumed by the
	// DroneDash race history to name bracket-linked heats.
	Title         string
	Pilots        []string
	Winners       []string
	Losers        []string
	IsComplete    bool
	FullPlacement bool
	IsVoid        bool
	// ExpectedWinners is how many pilots advance (elimination/ladder) or how
	// many must be ranked for the result to count (mains rank everyone).
	ExpectedWinners int
	// CanEdit reports whether a completed race may still be re-run.
	CanEdit bool
	// Locked marks a race that can't be run yet (e.g. a CTA heat awaiting the
	// previous heat); LockedReason names the blocker.
	Locked       bool
	LockedReason string
}

// Standing is one row in a format's running order.
type Standing struct {
	Rank  int
	Pilot string
	// Detail is a format-specific summary: "2 wins", "47 pts (R1:12 R2:18)", or
	// "" for pure single-result elimination.
	Detail string
	// Tied flags a pilot whose rank is unresolved on placements alone (the race
	// director may choose to run a tie-break). Formats never auto-create one.
	Tied bool
}

// Format is the strategy every race format implements. The lifecycle methods
// (State/RestoreState) operate on the format's own JSON-serialisable state blob
// persisted in race_brackets.bracket_state.
type Format interface {
	Kind() FormatKind

	// State returns the JSON-serialisable state blob to persist. RestoreState
	// rehydrates a format from a previously persisted blob.
	State() any
	RestoreState(raw json.RawMessage) error

	// Races projects the format's heats for the UI and ingest.
	Races() []RaceView

	// ApplyPlacement records a finish order for one race and advances state.
	ApplyPlacement(raceID string, placement []string) error
	// CanEditRace reports whether a completed race may still be re-run, and why
	// not when it can't.
	CanEditRace(raceID string) (ok bool, reason string)
	// EditRace re-runs a completed race, rewinding any clean downstream state.
	EditRace(raceID string, placement []string) error
	// ResetRace un-runs a completed race back to pending, rewinding any clean
	// downstream state. Subject to the same CanEditRace guard as EditRace; a
	// no-op on a race that hasn't been run.
	ResetRace(raceID string) error

	// Champion is the overall winner, or "" while undecided.
	Champion() string
	// IsComplete reports whether the format has crowned a champion / finished.
	IsComplete() bool
	// Standings is the full running order. Elimination returns a champion-only
	// (or empty) list; mains and ladder return the complete ranking.
	Standings() []Standing

	// LayoutKind selects the runner/public template partial:
	// "bracket-grid" | "ladder-grid" | "mains-table".
	LayoutKind() string
}
