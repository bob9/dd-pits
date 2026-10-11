package elrs

import "testing"

func TestChannelIndex(t *testing.T) {
	cases := []struct {
		band string
		ch   int
		want byte
	}{
		{"A", 1, 0},
		{"A", 8, 7},
		{"B", 1, 8},
		{"E", 3, 18},
		{"F", 4, 27},
		{"Fatshark", 4, 27},
		{"R", 1, 32},
		{"R", 2, 33},
		{"Raceband", 8, 39},
		{"L", 1, 40},
		{"lowband", 8, 47},
	}
	for _, tc := range cases {
		got, err := ChannelIndex(tc.band, tc.ch)
		if err != nil {
			t.Errorf("ChannelIndex(%q, %d): %v", tc.band, tc.ch, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ChannelIndex(%q, %d) = %d, want %d", tc.band, tc.ch, got, tc.want)
		}
	}

	if _, err := ChannelIndex("Q", 1); err == nil {
		t.Error("unknown band accepted")
	}
	if _, err := ChannelIndex("R", 0); err == nil {
		t.Error("channel 0 accepted")
	}
	if _, err := ChannelIndex("R", 9); err == nil {
		t.Error("channel 9 accepted")
	}
}

func TestChannelIndexFromLabel(t *testing.T) {
	got, err := ChannelIndexFromLabel("R2")
	if err != nil || got != 33 {
		t.Errorf("ChannelIndexFromLabel(R2) = %d, %v; want 33", got, err)
	}
	if _, err := ChannelIndexFromLabel("R"); err == nil {
		t.Error("bad label accepted")
	}
}

func TestChannelLabel(t *testing.T) {
	if got := ChannelLabel(33); got != "R2" {
		t.Errorf("ChannelLabel(33) = %s, want R2", got)
	}
	if got := ChannelLabel(0); got != "A1" {
		t.Errorf("ChannelLabel(0) = %s, want A1", got)
	}
}
