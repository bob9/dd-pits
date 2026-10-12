package raceformat

import (
	"encoding/json"
	"strconv"

	"github.com/bob9/dd-pits/internal/stages/mainsbracket"
)

// MainsProgressionFormat wraps the mainsbracket engine: a bump-up stack of
// "Top 4 Advance" qualifiers, one per Main. Each main is a qualbracket, so the
// runner/public/ingest/finalise reuse the qualifier paths via prefixed race ids
// ("A:W1-1"). The handler renders one qualifier SVG per built main (tabs).
type MainsProgressionFormat struct {
	m *mainsbracket.Mains
}

func mainsProgressionConfig(cfg Config) mainsbracket.Config {
	return mainsbracket.Config{
		PilotCount:      cfg.PilotCount,
		PilotNames:      cfg.PilotNames,
		PilotsPerMain:   cfg.PilotsPerMain,
		BumpUp:          cfg.BumpUp,
		CTA:             cfg.QualifierCTA,
		AceWinsRequired: cfg.AceWinsRequired,
		PhotoSeed:       cfg.QualifierPhotoSeed,
		CTAMains:        cfg.CTAMains,
		PilotsPerRace:   cfg.PilotsPerRace,
		Advance:         cfg.PilotsToAdvance,
	}
}

func newMainsProgression(cfg Config) (*MainsProgressionFormat, error) {
	m, err := mainsbracket.New(mainsProgressionConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &MainsProgressionFormat{m: m}, nil
}

func mainsProgressionFromState(cfg Config, raw json.RawMessage) (*MainsProgressionFormat, error) {
	if len(raw) == 0 {
		return newMainsProgression(cfg)
	}
	st := &mainsbracket.State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	m, err := mainsbracket.FromState(mainsProgressionConfig(cfg), st)
	if err != nil {
		return nil, err
	}
	return &MainsProgressionFormat{m: m}, nil
}

// Mains returns the underlying engine for tabbed per-main rendering.
func (f *MainsProgressionFormat) Mains() *mainsbracket.Mains { return f.m }

func (f *MainsProgressionFormat) Kind() FormatKind { return KindMainsProgression }

func (f *MainsProgressionFormat) State() any { return f.m.State() }

func (f *MainsProgressionFormat) RestoreState(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	st := &mainsbracket.State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return err
	}
	m, err := mainsbracket.FromState(f.m.Cfg(), st)
	if err != nil {
		return err
	}
	f.m = m
	return nil
}

func (f *MainsProgressionFormat) ApplyPlacement(raceID string, placement []string) error {
	return f.m.ApplyPlacement(raceID, placement)
}

func (f *MainsProgressionFormat) CanEditRace(raceID string) (bool, string) {
	return f.m.CanEditRace(raceID)
}

func (f *MainsProgressionFormat) EditRace(raceID string, placement []string) error {
	return f.m.EditRace(raceID, placement)
}

func (f *MainsProgressionFormat) ResetRace(raceID string) error {
	return f.m.ResetRace(raceID)
}

func (f *MainsProgressionFormat) Champion() string { return f.m.Champion() }

func (f *MainsProgressionFormat) IsComplete() bool { return f.m.IsComplete() }

func (f *MainsProgressionFormat) LayoutKind() string { return "mains-progression-tabs" }

// Standings is the overall competition order, A main first then B, …, with the
// pilot's main + place as the detail line.
func (f *MainsProgressionFormat) Standings() []Standing {
	rows := f.m.Ranking()
	out := make([]Standing, 0, len(rows))
	for i, r := range rows {
		out = append(out, Standing{
			Rank:   i + 1,
			Pilot:  r.Pilot,
			Detail: r.Main + " Main · " + posOrdinal(r.MainRank),
		})
	}
	return out
}

// Races projects every built main's heats with prefixed ids ("A:W1-1") and a
// main-qualified title ("A Main · Winners Round 2 — Race 3").
func (f *MainsProgressionFormat) Races() []RaceView {
	var out []RaceView
	for _, mv := range f.m.MainViews() {
		if mv.Qual == nil {
			continue // unbuilt main has no heats yet
		}
		state := mv.Qual.State()
		titles := qualRaceTitles(state)
		for _, r := range state.Races {
			prefixed := mv.Label + ":" + r.ID
			canEdit := false
			if r.IsComplete {
				canEdit, _ = f.m.CanEditRace(prefixed)
			}
			out = append(out, RaceView{
				ID:              prefixed,
				Round:           r.Round,
				GroupLabel:      mv.Label + " Main · " + qualSlotLabel(r.Bracket),
				Title:           mv.Label + " Main · " + titles[r.ID],
				Pilots:          append([]string(nil), r.Pilots...),
				Winners:         append([]string(nil), r.Winners...),
				Losers:          append([]string(nil), r.Losers...),
				IsComplete:      r.IsComplete,
				FullPlacement:   r.FullPlacement,
				IsVoid:          r.IsVoid,
				ExpectedWinners: mv.Qual.ExpectedWinners(r),
				CanEdit:         canEdit,
			})
		}
	}
	return out
}

// posOrdinal renders 1→"1st", 2→"2nd", … for standings detail.
func posOrdinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}
