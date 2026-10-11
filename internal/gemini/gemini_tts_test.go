package gemini

import "testing"

func TestParseAudioRate(t *testing.T) {
	cases := map[string]int{
		"audio/L16;codec=pcm;rate=24000": 24000,
		"audio/L16; rate=16000":          16000,
		"audio/L16;codec=pcm":            24000, // unstated → default
		"":                               24000,
		"audio/L16;rate=abc":             24000, // malformed → default
	}
	for mime, want := range cases {
		if got := ParseAudioRate(mime); got != want {
			t.Errorf("ParseAudioRate(%q) = %d, want %d", mime, got, want)
		}
	}
}
