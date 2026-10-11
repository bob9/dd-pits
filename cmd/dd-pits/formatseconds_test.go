package main

import "testing"

func TestFormatSecondsNeverEmitsATrailingZero(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{2.60, "2.6"}, // "2.60" was read aloud as "two point six ZERO"
		{2.6, "2.6"},
		{4.45, "4.45"},
		{23.0, "23.0"}, // one decimal kept: "twenty three point oh", not "twenty three"
		{812.10, "812.1"},
		{9.43, "9.43"},
		{0.98, "0.98"},
		{11.0, "11.0"},
	} {
		if got := formatSeconds(c.in); got != c.want {
			t.Errorf("formatSeconds(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
