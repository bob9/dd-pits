package qwentts

import (
	"encoding/json"
	"testing"
)

// Qwen TTS is billed per input CHARACTER and reports no tokens at all. Reading
// only the token fields is how every Qwen audio call came to be metered as
// zero, which made the backend look free rather than cheap.
func TestUsageDecodesCharacters(t *testing.T) {
	var u usageInfo
	if err := json.Unmarshal([]byte(`{"characters":91}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.Characters != 91 {
		t.Errorf("Characters = %d, want 91 — Qwen reports this, not tokens", u.Characters)
	}
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("token fields = %d/%d, want 0 for a Qwen response", u.InputTokens, u.OutputTokens)
	}
}

// A token-shaped response (any provider that reports them) must still decode,
// so the fallback branch in stream() keeps working.
func TestUsageStillDecodesTokens(t *testing.T) {
	var u usageInfo
	if err := json.Unmarshal([]byte(`{"input_tokens":12,"output_tokens":34}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 12 || u.OutputTokens != 34 || u.Characters != 0 {
		t.Errorf("got in=%d out=%d chars=%d", u.InputTokens, u.OutputTokens, u.Characters)
	}
}

// The meter bucket must be distinct from Gemini's "tts" (different unit, so a
// different price) and must fit dd_booth_usage.kind, which is VARCHAR(10).
func TestUsageKindIsDistinctAndFitsTheColumn(t *testing.T) {
	if UsageKindTTS == "tts" {
		t.Error("Qwen speech must not share Gemini's bucket — characters would be priced as tokens")
	}
	if len(UsageKindTTS) > 10 {
		t.Errorf("UsageKindTTS %q is %d chars, over the VARCHAR(10) column", UsageKindTTS, len(UsageKindTTS))
	}
}
