package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bob9/dd-pits/internal/qwentts"
)

// TestGeminiPromptIsUnchanged pins that introducing this seam did not change a
// single byte of what Gemini receives: it has always taken the delivery
// direction as a preamble glued to the front of the script, and the call sites
// used to build exactly "instruction\n\ntext".
func TestGeminiPromptIsUnchanged(t *testing.T) {
	if got, want := GeminiPrompt("Call it urgently.", "They're away!"), "Call it urgently.\n\nThey're away!"; got != want {
		t.Errorf("gemini prompt = %q, want %q", got, want)
	}
	// No direction: the script alone, with no leading blank lines.
	if got := GeminiPrompt("", "They're away!"); got != "They're away!" {
		t.Errorf("undirected prompt = %q", got)
	}
}

// TestQwenKeepsInstructionOutOfTheSpokenText is the same contract from the
// other side: Qwen's text field is literally what gets said, so the direction
// must travel separately or the commentator reads the stage notes aloud.
func TestQwenKeepsInstructionOutOfTheSpokenText(t *testing.T) {
	var input map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		input, _ = body["input"].(map[string]interface{})
		w.Write([]byte("data:{\"output\":{\"audio\":{\"data\":\"AAA=\"},\"finish_reason\":\"stop\"}}\n\n"))
	}))
	defer srv.Close()

	c, err := qwentts.New(qwentts.Config{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	eng := FromQwen(c)
	if eng.Provider() != ProviderQwen {
		t.Errorf("provider = %q", eng.Provider())
	}
	if _, _, err := eng.Speak(context.Background(), Request{
		Text: "They're away!", Instruction: "Call it urgently.", Voice: "Ethan",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := input["text"].(string); got != "They're away!" {
		t.Errorf("spoken text = %q — the direction must not be in it", got)
	}
	if got, _ := input["instructions"].(string); !strings.Contains(got, "urgently") {
		t.Errorf("instructions = %q", got)
	}
}

// Times reach the model as "21.23s" and it echoes that into the script, where
// TTS reads the unit as the LETTER — "twenty-one twenty-three ess".
func TestSpokenTextDropsBareSecondsUnit(t *testing.T) {
	cases := map[string]string{
		"That's a 21.23s lap":                  "That's a 21.23 lap",
		"t=4.5s DEMON through the first gate":  "t=4.5 DEMON through the first gate",
		"best 3-consec 63.41s, best lap 20.9s": "best 3-consec 63.41, best lap 20.9",
		"needs 0.42s to pass VORTEX":           "needs 0.42 to pass VORTEX",
	}
	for in, want := range cases {
		if got := SpokenText(in); got != want {
			t.Errorf("SpokenText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Integer+s is real FPV vocabulary and must survive — a "4s pack" is a battery,
// not a time. Only decimals carry the seconds suffix in our prompts.
func TestSpokenTextLeavesNonTimesAlone(t *testing.T) {
	for _, s := range []string{
		"flying a 4s pack tonight",
		"stepped up to a 6s build",
		"the 1990s",
		"three laps to go",
		"DEMON leads by half a length",
		"P3 on the board",
	} {
		if got := SpokenText(s); got != s {
			t.Errorf("SpokenText(%q) altered it to %q", s, got)
		}
	}
}

// Speaker tags and structure must be untouched — SplitTurns runs on this text.
func TestSpokenTextPreservesSpeakerTags(t *testing.T) {
	in := "Ace: And that's a 21.23s lap!\nTorque: His best of the day by 0.40s."
	want := "Ace: And that's a 21.23 lap!\nTorque: His best of the day by 0.40."
	if got := SpokenText(in); got != want {
		t.Errorf("SpokenText(%q) = %q, want %q", in, got, want)
	}
}
