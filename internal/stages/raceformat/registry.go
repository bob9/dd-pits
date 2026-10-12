package raceformat

import (
	"encoding/json"
	"fmt"
)

// Config is the superset of setup parameters across every format. Each format
// reads only the fields it needs: elimination uses the EliminationType/Finals…
// fields, ladder reuses PilotsPerRace/PilotsToAdvance, mains uses the Mains…
// fields. Format-specific params are decoded from race_brackets.format_config.
type Config struct {
	PilotCount int
	PilotNames []string // qualifying order — seed 1 first

	// Elimination + ladder.
	EliminationType string
	PilotsPerRace   int
	PilotsToAdvance int
	FinalsFormat    string
	AceWinsRequired int

	// Mains (decoded from format_config).
	Mains       []string
	Rounds      int
	Direction   string
	PointsTable map[int]int
	DropWorst   int
	// MainsRosters is an optional explicit pilot→main assignment from the setup
	// form's drag-drop. When empty, pilots are chunked into mains by seed.
	MainsRosters map[string][]string
	// MainsDivisions optionally groups the mains into independent competitions.
	// Empty = single competition over all mains.
	MainsDivisions []MainsDivision

	// Qualifier: run the 4-pilot final as a chase-the-ace mini-series.
	QualifierCTA bool
	// QualifierPhotoSeed selects the 16-pilot round-1 seeding (true = photo's
	// exact layout, false/default = MultiGP snake). Ignored for other sizes.
	QualifierPhotoSeed bool

	// Mains progression (a stack of qualifiers). PilotsPerMain is each main's
	// running size; BumpUp is how many of each main's finishers move up into the
	// next. Reuses QualifierCTA/AceWinsRequired/QualifierPhotoSeed per main.
	PilotsPerMain int
	BumpUp        int
	// CTAMains selects which mains run their 4-pilot final as chase-the-ace (when
	// QualifierCTA is on). Empty = the A Main only; ["*"] = every main.
	CTAMains []string
}

// New generates a fresh format of the given kind from cfg.
func New(kind FormatKind, cfg Config) (Format, error) {
	switch kind {
	case KindElimination, "":
		return newElimination(cfg)
	case KindLadder:
		return newLadder(cfg)
	case KindMains:
		return newMains(cfg)
	case KindQualifier:
		return newQualifier(cfg)
	case KindMainsProgression:
		return newMainsProgression(cfg)
	default:
		return nil, fmt.Errorf("raceformat: unknown format %q", kind)
	}
}

// FromState rehydrates a format of the given kind from a persisted state blob.
func FromState(kind FormatKind, cfg Config, raw json.RawMessage) (Format, error) {
	switch kind {
	case KindElimination, "":
		return eliminationFromState(cfg, raw)
	case KindLadder:
		return ladderFromState(cfg, raw)
	case KindMains:
		return mainsFromState(cfg, raw)
	case KindQualifier:
		return qualifierFromState(cfg, raw)
	case KindMainsProgression:
		return mainsProgressionFromState(cfg, raw)
	default:
		return nil, fmt.Errorf("raceformat: unknown format %q", kind)
	}
}
