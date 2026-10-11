package elrs

import (
	"encoding/json"
	"testing"
)

func TestDefaultLayoutComplete(t *testing.T) {
	l := DefaultLayout()
	if len(l.Elements) != len(ElementMetas) {
		t.Fatalf("default layout has %d elements, want %d", len(l.Elements), len(ElementMetas))
	}
	for _, m := range ElementMetas {
		if _, ok := l.Elements[m.Slug]; !ok {
			t.Errorf("default layout missing %q", m.Slug)
		}
	}
}

func TestValidateLayoutFillsAndClamps(t *testing.T) {
	in := Layout{
		Version: LayoutVersion,
		Elements: map[string]ElementPos{
			ElemRaceStatus: {Enabled: true, Row: 99, Col: 99},
			ElemResults:    {Enabled: true, Row: -5, Col: -5},
			"bogus_slug":   {Enabled: true, Row: 1, Col: 1},
		},
	}
	out, err := ValidateLayout(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Elements["bogus_slug"]; ok {
		t.Error("unknown slug not dropped")
	}
	if len(out.Elements) != len(ElementMetas) {
		t.Errorf("validated layout has %d elements, want %d", len(out.Elements), len(ElementMetas))
	}
	rs := out.Elements[ElemRaceStatus]
	if rs.Row != OSDRows-1 {
		t.Errorf("race_status row = %d, want clamped to %d", rs.Row, OSDRows-1)
	}
	if rs.Col > OSDCols {
		t.Errorf("race_status col = %d not clamped", rs.Col)
	}
	res := out.Elements[ElemResults]
	if res.Row != 0 || res.Col != 0 {
		t.Errorf("results pos = %d,%d, want 0,0", res.Row, res.Col)
	}
}

func TestValidateLayoutRejectsBadVersion(t *testing.T) {
	if _, err := ValidateLayout(Layout{Version: 99}); err == nil {
		t.Error("version 99 accepted")
	}
}

func TestParseLayoutJSONRoundTrip(t *testing.T) {
	orig, _ := ValidateLayout(DefaultLayout())
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseLayoutJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	for slug, pos := range orig.Elements {
		if back.Elements[slug] != pos {
			t.Errorf("%s: %+v != %+v", slug, back.Elements[slug], pos)
		}
	}
}

func TestStartCol(t *testing.T) {
	centered := ElementPos{Center: true, Col: 40}
	if got := centered.StartCol(10); got != 20 {
		t.Errorf("centered StartCol(10) = %d, want 20", got)
	}
	fixed := ElementPos{Col: 7}
	if got := fixed.StartCol(10); got != 7 {
		t.Errorf("fixed StartCol = %d, want 7", got)
	}
}

func TestTestLayoutCells(t *testing.T) {
	l := Layout{Version: LayoutVersion, Elements: map[string]ElementPos{
		// Enabled, fixed column → renders at its column with its preview text.
		ElemLapPosition: {Enabled: true, Row: 4, Col: 5},
		// Disabled → dropped.
		ElemGapResult: {Enabled: false, Row: 6, Center: true},
		// Two-row results block → two cells on consecutive rows.
		ElemResults: {Enabled: true, Row: 10, Col: 0},
	}}

	cells := TestLayoutCells(l)

	byText := map[string]TestCell{}
	for _, c := range cells {
		byText[c.Text] = c
	}

	if c, ok := byText["POSN: 3 | LAP: 2"]; !ok {
		t.Fatalf("lap_position cell missing; got %+v", cells)
	} else if c.Row != 4 || c.Col != 5 {
		t.Errorf("lap_position at (%d,%d), want (4,5)", c.Row, c.Col)
	}

	if _, ok := byText["x PILOT2 | +0.85 w"]; ok {
		t.Error("disabled gap_result should not render")
	}

	// Results renders both rows.
	if c, ok := byText["PLACEMENT: 2"]; !ok || c.Row != 10 {
		t.Errorf("results line 1 = %+v, want row 10", c)
	}
	if c, ok := byText["TOTAL TIME: 1:32.45"]; !ok || c.Row != 11 {
		t.Errorf("results line 2 = %+v, want row 11", c)
	}

	// Every cell must sit on the grid.
	for _, c := range cells {
		if c.Row < 0 || c.Row >= OSDRows || c.Col < 0 || c.Col >= OSDCols {
			t.Errorf("cell off grid: %+v", c)
		}
	}
}
