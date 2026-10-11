package qwentts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSplitTurnsKeepsTheBoothTwoPeople pins the core of the Qwen path: the
// model has no multi-speaker call, so a duo script is rendered a turn at a
// time. Consecutive lines by one speaker must MERGE (fewer calls, and the
// model gets a whole thought to pace).
func TestSplitTurnsKeepsTheBoothTwoPeople(t *testing.T) {
	voices := []SpeakerVoice{{Speaker: "Ace", Voice: "Ethan"}, {Speaker: "Torque", Voice: "Cherry"}}
	script := "Ace: They're away!\n" +
		"Ace: DEMON gets the jump.\n" +
		"Torque: And that's the holeshot he needed.\n" +
		"Ace: Two laps to go."
	got := SplitTurns(script, voices)
	if len(got) != 3 {
		t.Fatalf("want 3 turns (Ace, Torque, Ace), got %d: %+v", len(got), got)
	}
	if got[0].Voice != "Ethan" || !strings.Contains(got[0].Text, "They're away") ||
		!strings.Contains(got[0].Text, "DEMON gets the jump") {
		t.Errorf("consecutive Ace lines must merge into one turn: %+v", got[0])
	}
	if got[1].Voice != "Cherry" || strings.Contains(got[1].Text, "Torque:") {
		t.Errorf("analyst turn wrong (tag must be stripped): %+v", got[1])
	}
	if got[2].Voice != "Ethan" {
		t.Errorf("turn 3 should return to the caller: %+v", got[2])
	}
}

// TestSplitTurnsNeverDropsWords pins that unlabelled or oddly-labelled lines
// are still spoken — silence is a worse failure than the wrong voice.
func TestSplitTurnsNeverDropsWords(t *testing.T) {
	voices := []SpeakerVoice{{Speaker: "Ace", Voice: "Ethan"}, {Speaker: "Torque", Voice: "Cherry"}}
	for _, c := range []struct{ name, script, want string }{
		{"untagged", "They're away and gone!", "They're away and gone!"},
		{"unknown speaker", "Pitlane: big crowd tonight", "Pitlane: big crowd tonight"},
		{"colon mid-sentence", "Ace: one thing is clear: he is quick", "one thing is clear: he is quick"},
	} {
		turns := SplitTurns(c.script, voices)
		if len(turns) == 0 {
			t.Fatalf("%s: everything was dropped", c.name)
		}
		var all []string
		for _, tn := range turns {
			all = append(all, tn.Text)
		}
		if joined := strings.Join(all, " "); !strings.Contains(joined, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, joined, c.want)
		}
	}
}

// TestResolveVoiceSurvivesABackendSwitch: a tenant's stored voice is a Gemini
// name until they pick a Qwen one. That must degrade to a default, not to an
// API error on the first race of the day.
func TestResolveVoiceSurvivesABackendSwitch(t *testing.T) {
	if got := ResolveVoice("Puck", DefaultVoicePBP); got != DefaultVoicePBP {
		t.Errorf("a Gemini voice name should fall back, got %q", got)
	}
	if got := ResolveVoice("", DefaultVoiceColor); got != DefaultVoiceColor {
		t.Errorf("blank should fall back, got %q", got)
	}
	if got := ResolveVoice("Ethan", DefaultVoiceColor); got != "Ethan" {
		t.Errorf("a valid voice must be kept, got %q", got)
	}
}

// TestLanguageTypeOnlyClaimsWhatQwenSpeaks — sending an unsupported
// language_type is an API error, so an unlisted language sends no hint at all.
func TestLanguageTypeOnlyClaimsWhatQwenSpeaks(t *testing.T) {
	if got := LanguageType("German"); got != "German" {
		t.Errorf("German should map, got %q", got)
	}
	if got := LanguageType("mandarin chinese"); got != "Chinese" {
		t.Errorf("platform name should map case-insensitively, got %q", got)
	}
	for _, unsupported := range []string{"Dutch", "Māori", "Thai", "Afrikaans", ""} {
		if got := LanguageType(unsupported); got != "" {
			t.Errorf("%q is not a Qwen language — want no hint, got %q", unsupported, got)
		}
	}
}

// sseBody builds a DashScope-shaped streaming response.
func sseBody(chunks ...string) string {
	var b strings.Builder
	for i, c := range chunks {
		ev := map[string]interface{}{
			"output": map[string]interface{}{
				"audio": map[string]interface{}{"data": base64.StdEncoding.EncodeToString([]byte(c))},
			},
		}
		if i == len(chunks)-1 {
			ev["output"].(map[string]interface{})["finish_reason"] = "stop"
			ev["usage"] = map[string]int{"input_tokens": 11, "output_tokens": 22}
		}
		j, _ := json.Marshal(ev)
		// No space after "data:" — DashScope's actual wire format.
		fmt.Fprintf(&b, "id:%d\nevent:result\ndata:%s\n\n", i+1, j)
	}
	return b.String()
}

func TestSpeakStreamsPCMAndMeters(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k-123" {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("X-DashScope-SSE"); got != "enable" {
			t.Errorf("SSE header = %q — the streaming path is what returns raw PCM", got)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody("abcd", "efgh"))
	}))
	defer srv.Close()

	c, err := New(Config{APIKey: "k-123", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	var in, out int
	c.OnUsage = func(k string, i, o int) { kind, in, out = k, i, o }

	pcm, rate, err := c.Speak(context.Background(), "They're away!", "Ethan", "Call it with real urgency.", "English")
	if err != nil {
		t.Fatal(err)
	}
	if string(pcm) != "abcdefgh" {
		t.Errorf("chunks must concatenate in order, got %q", pcm)
	}
	if rate != SampleRate {
		t.Errorf("rate = %d, want %d", rate, SampleRate)
	}
	if kind != "tts" || in != 11 || out != 22 {
		t.Errorf("usage not metered: %s %d/%d", kind, in, out)
	}
	// The delivery direction must NOT be glued to the spoken text — Qwen would
	// read it out loud. It belongs in instructions, on the instruct model.
	input := gotBody["input"].(map[string]interface{})
	if input["text"] != "They're away!" {
		t.Errorf("text carried more than the words: %q", input["text"])
	}
	if input["instructions"] != "Call it with real urgency." {
		t.Errorf("instruction not sent separately: %q", input["instructions"])
	}
	if gotBody["model"] != DefaultInstructModel {
		t.Errorf("an instruction requires the instruct model, got %q", gotBody["model"])
	}
	if input["language_type"] != "English" {
		t.Errorf("language_type = %q", input["language_type"])
	}
}

// TestSpeakDialogueConcatenatesTurns: one request per turn, joined in order.
func TestSpeakDialogueConcatenatesTurns(t *testing.T) {
	var voices []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input struct{ Voice, Text string } `json:"input"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		voices = append(voices, body.Input.Voice)
		fmt.Fprint(w, sseBody(body.Input.Voice))
	}))
	defer srv.Close()

	c, _ := New(Config{APIKey: "k", BaseURL: srv.URL})
	pcm, _, err := c.SpeakDialogue(context.Background(),
		"Ace: one\nTorque: two\nAce: three",
		[]SpeakerVoice{{Speaker: "Ace", Voice: "Ethan"}, {Speaker: "Torque", Voice: "Cherry"}}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Ethan", "Cherry", "Ethan"}; strings.Join(voices, ",") != strings.Join(want, ",") {
		t.Errorf("turn voices = %v, want %v", voices, want)
	}
	if string(pcm) != "EthanCherryEthan" {
		t.Errorf("turns must concatenate in script order, got %q", pcm)
	}
}

// TestRateLimitErrorShape pins the message shape the callers' rate-limit
// detection matches on, so a Qwen 429 paces a run exactly like a Gemini one.
func TestRateLimitErrorShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded"}`)
	}))
	defer srv.Close()
	c, _ := New(Config{APIKey: "k", BaseURL: srv.URL})
	_, _, err := c.Speak(context.Background(), "hi", "Ethan", "", "")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "API error 429") {
		t.Errorf("error must carry the shared 'API error 429' marker, got %q", err)
	}
}

// TestMidStreamErrorIsNotSilentlyShortAudio — DashScope can report a failure
// inside a 200 stream; treating that as "the audio just ended" would ship a
// truncated call to air.
func TestMidStreamErrorIsNotSilentlyShortAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data:{\"code\":\"InternalError\",\"message\":\"boom\"}\n\n")
	}))
	defer srv.Close()
	c, _ := New(Config{APIKey: "k", BaseURL: srv.URL})
	if _, _, err := c.Speak(context.Background(), "hi", "Ethan", "", ""); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("mid-stream error must surface, got %v", err)
	}
}

func TestNoAudioIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data:{\"output\":{\"finish_reason\":\"stop\"}}\n\n")
	}))
	defer srv.Close()
	c, _ := New(Config{APIKey: "k", BaseURL: srv.URL})
	_, _, err := c.Speak(context.Background(), "hi", "Ethan", "", "")
	if err == nil || !strings.Contains(err.Error(), ErrNoAudio.Error()) {
		t.Fatalf("want ErrNoAudio, got %v", err)
	}
}

func TestNewRequiresAKey(t *testing.T) {
	if _, err := New(Config{APIKey: "   "}); err == nil {
		t.Fatal("a blank key must fail at construction, not at the first race of the day")
	}
}
