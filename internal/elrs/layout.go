package elrs

import (
	"encoding/json"
	"fmt"
	"sort"
)

// LayoutVersion is the current OSD layout schema version.
const LayoutVersion = 1

// Element slugs — the fixed set of OSD message types a pilot can position.
const (
	ElemRaceStatus   = "race_status"
	ElemLapPosition  = "lap_position"
	ElemLapResult    = "lap_result"
	ElemGapResult    = "gap_result"
	ElemGapBehind    = "gap_behind"
	ElemHeatName     = "heat_name"
	ElemClassName    = "class_name"
	ElemEventName    = "event_name"
	ElemAnnouncement = "announcement"
	ElemResults      = "results"
	ElemRecentLaps   = "recent_laps"
)

// ElementMeta describes an element type for editors and renderers.
type ElementMeta struct {
	Slug    string `json:"slug"`
	Label   string `json:"label"`
	Preview string `json:"preview"` // representative text shown in the editor
	MaxW    int    `json:"max_w"`   // widest realistic render, for clamping
	Rows    int    `json:"rows"`    // vertical footprint (results is a block)
}

// ElementMetas lists every element in display order. The preview strings use
// the HDZero glyph convention from the reference plugin ('x'/'w' render as
// block borders on the goggles).
var ElementMetas = []ElementMeta{
	{Slug: ElemRaceStatus, Label: "Race status", Preview: "w ARM NOW x", MaxW: 20, Rows: 1},
	{Slug: ElemLapPosition, Label: "Lap & position", Preview: "POSN: 3 | LAP: 2", MaxW: 20, Rows: 1},
	{Slug: ElemLapResult, Label: "Lap time", Preview: "x 1:04.32 | 3:12.45 w", MaxW: 30, Rows: 1},
	{Slug: ElemGapResult, Label: "Gap to next", Preview: "x PILOT2 | +0.85 w", MaxW: 30, Rows: 1},
	{Slug: ElemGapBehind, Label: "Gap behind (chaser)", Preview: "x PILOT4 | -0.85 w", MaxW: 30, Rows: 1},
	{Slug: ElemHeatName, Label: "Heat name", Preview: "x HEAT 3 | ROUND 2 w", MaxW: 34, Rows: 1},
	{Slug: ElemClassName, Label: "Class name", Preview: "x OPEN CLASS w", MaxW: 34, Rows: 1},
	{Slug: ElemEventName, Label: "Event name", Preview: "x CLUB CHAMPS w", MaxW: 34, Rows: 1},
	{Slug: ElemAnnouncement, Label: "Announcements", Preview: "x TRACK CLEAR w", MaxW: 50, Rows: 1},
	{Slug: ElemResults, Label: "Race results", Preview: "PLACEMENT: 2", MaxW: 34, Rows: 2},
	{Slug: ElemRecentLaps, Label: "Recent laps (rolling)", Preview: "L2:44.23", MaxW: 12, Rows: 1}, // vertical: real footprint = NumLaps rows
}

// ElementPos is one element's placement on the 50x18 character grid.
type ElementPos struct {
	Enabled bool `json:"enabled"`
	Row     int  `json:"row"`
	Col     int  `json:"col"`
	Center  bool `json:"center"` // when true Col is ignored and text auto-centers

	// NumLaps is the rolling window size (ElemRecentLaps only); 0 = default 3, max 5.
	NumLaps int `json:"num_laps,omitempty"`
	// HoldSecs is how long the message stays on screen after each update, for
	// any element: 0 = the element's default behaviour, -1 = stays up until
	// the next update or a screen clear, >0 = cleared after that many seconds.
	HoldSecs int `json:"hold_secs,omitempty"`
}

// HoldFor resolves the user's on-screen duration against an element's default
// (def 0 = the element normally stays up). Returned 0 means "no timed clear".
func (p ElementPos) HoldFor(defSecs int) int {
	switch {
	case p.HoldSecs > 0:
		return p.HoldSecs
	case p.HoldSecs == -1:
		return 0
	default:
		return defSecs
	}
}

// RecentLapWindow resolves the rolling-laps window with its default + cap.
func (p ElementPos) RecentLapWindow() int {
	if p.NumLaps <= 0 {
		return 3
	}
	if p.NumLaps > 5 {
		return 5
	}
	return p.NumLaps
}

// Layout is a complete OSD layout: every element slug maps to a placement.
type Layout struct {
	Version  int                   `json:"version"`
	Elements map[string]ElementPos `json:"elements"`
}

// DefaultLayout mirrors the reference plugin's default rows.
func DefaultLayout() Layout {
	return Layout{
		Version: LayoutVersion,
		Elements: map[string]ElementPos{
			ElemRaceStatus:   {Enabled: true, Row: 5, Center: true},
			ElemLapPosition:  {Enabled: true, Row: 0, Center: true},
			ElemLapResult:    {Enabled: true, Row: 15, Center: true},
			ElemGapResult:    {Enabled: true, Row: 14, Center: true},
			ElemGapBehind:    {Enabled: false, Row: 12, Center: true},
			ElemHeatName:     {Enabled: true, Row: 2, Center: true},
			ElemClassName:    {Enabled: false, Row: 1, Center: true},
			ElemEventName:    {Enabled: false, Row: 0, Center: true},
			ElemAnnouncement: {Enabled: true, Row: 3, Center: true},
			ElemResults:      {Enabled: true, Row: 13, Center: true},
			ElemRecentLaps:   {Enabled: false, Row: 16, Center: true},
		},
	}
}

func metaFor(slug string) (ElementMeta, bool) {
	for _, m := range ElementMetas {
		if m.Slug == slug {
			return m, true
		}
	}
	return ElementMeta{}, false
}

// ValidateLayout normalizes a layout: unknown slugs are dropped, missing
// slugs are filled from the default, rows/cols are clamped to the grid and
// the element's footprint. The returned layout is always complete.
func ValidateLayout(in Layout) (Layout, error) {
	if in.Version != 0 && in.Version != LayoutVersion {
		return Layout{}, fmt.Errorf("unsupported layout version %d", in.Version)
	}
	out := DefaultLayout()
	for slug, pos := range in.Elements {
		meta, ok := metaFor(slug)
		if !ok {
			continue
		}
		rows := meta.Rows
		if slug == ElemRecentLaps {
			rows = pos.RecentLapWindow() // vertical strip: one row per lap
		}
		maxRow := OSDRows - rows
		if pos.Row < 0 {
			pos.Row = 0
		}
		if pos.Row > maxRow {
			pos.Row = maxRow
		}
		maxCol := OSDCols - meta.MaxW
		if maxCol < 0 {
			maxCol = 0
		}
		if pos.Col < 0 {
			pos.Col = 0
		}
		if pos.Col > maxCol {
			pos.Col = maxCol
		}
		if pos.Center {
			pos.Col = 0
		}
		if pos.NumLaps < 0 {
			pos.NumLaps = 0
		}
		if pos.NumLaps > 5 {
			pos.NumLaps = 5
		}
		if pos.HoldSecs < -1 {
			pos.HoldSecs = -1
		}
		if pos.HoldSecs > 60 {
			pos.HoldSecs = 60
		}
		out.Elements[slug] = pos
	}
	return out, nil
}

// ParseLayoutJSON decodes and validates a stored layout.
func ParseLayoutJSON(data []byte) (Layout, error) {
	var l Layout
	if err := json.Unmarshal(data, &l); err != nil {
		return Layout{}, fmt.Errorf("parse osd layout: %w", err)
	}
	return ValidateLayout(l)
}

// MarshalJSON renders elements in a stable order for ETag-friendly output.
func (l Layout) MarshalJSON() ([]byte, error) {
	type alias Layout
	slugs := make([]string, 0, len(l.Elements))
	for s := range l.Elements {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	ordered := make(map[string]ElementPos, len(l.Elements))
	for _, s := range slugs {
		ordered[s] = l.Elements[s]
	}
	return json.Marshal(alias{Version: l.Version, Elements: ordered})
}

// StartCol resolves the column an element's text should start at.
func (p ElementPos) StartCol(textLen int) int {
	if p.Center {
		return CenterCol(textLen)
	}
	return p.Col
}

// TestCell is one dummy element placement for the goggle "test layout" preview.
type TestCell struct {
	Row  int    `json:"row"`
	Col  int    `json:"col"`
	Text string `json:"text"`
}

// testPreviewLines returns the sample text an element shows in the test
// preview, mirroring the editor's preview strings. The results block spans two
// rows, so it returns two lines.
func testPreviewLines(m ElementMeta, pos ElementPos) []string {
	if m.Slug == ElemResults {
		return []string{"PLACEMENT: 2", "TOTAL TIME: 1:32.45"}
	}
	if m.Slug == ElemRecentLaps {
		sample := []string{"HS:2.14", "L1:45.01", "L2:44.23", "L3:43.56", "L4:44.01"}
		return sample[len(sample)-pos.RecentLapWindow():]
	}
	return []string{m.Preview}
}

// TestLayoutCells renders a layout with sample race data: every enabled
// element mapped to its (row, col, text) on the OSD grid, using the same
// preview strings the editor shows. Powers the "test layout" goggle preview so
// a pilot can check placement at the field before flying. Off-grid rows are
// dropped; column centering matches the editor via StartCol.
func TestLayoutCells(l Layout) []TestCell {
	var out []TestCell
	for _, m := range ElementMetas {
		pos, ok := l.Elements[m.Slug]
		if !ok || !pos.Enabled {
			continue
		}
		for i, line := range testPreviewLines(m, pos) {
			row := pos.Row + i
			if row < 0 || row >= OSDRows || line == "" {
				continue
			}
			out = append(out, TestCell{Row: row, Col: pos.StartCol(len(line)), Text: line})
		}
	}
	return out
}
