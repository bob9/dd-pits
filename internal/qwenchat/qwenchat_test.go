package qwenchat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no key returned a client; want an error at resolve time")
	}
	if _, err := New(Config{APIKey: "   "}); err == nil {
		t.Error("New with a blank key returned a client")
	}
	c, err := New(Config{APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Model() != DefaultModel {
		t.Errorf("Model = %q, want %q", c.Model(), DefaultModel)
	}
}

// Thinking must be OFF on every request — it is the entire reason this client
// exists. Reasoning tokens, not output length, are what put a live call seconds
// behind the action.
func TestGenerateTextDisablesThinking(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":"Ace: They're away!"}}],
		                    "usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	text, err := c.GenerateText(context.Background(), "write a call")
	if err != nil {
		t.Fatal(err)
	}
	if text != "Ace: They're away!" {
		t.Errorf("text = %q", text)
	}
	if v, ok := got["enable_thinking"]; !ok || v != false {
		t.Errorf("enable_thinking = %v (present=%v), want false", v, ok)
	}
	if got["model"] != DefaultModel {
		t.Errorf("model = %v, want %q", got["model"], DefaultModel)
	}
}

func TestGenerateTextReportsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"line"}}],
		                    "usage":{"prompt_tokens":322,"completion_tokens":70}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	var kind string
	var in, out int
	c.OnUsage = func(k string, i, o int) { kind, in, out = k, i, o }
	if _, err := c.GenerateText(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if kind != "text" || in != 322 || out != 70 {
		t.Errorf("usage = (%q, %d, %d), want (text, 322, 70)", kind, in, out)
	}
}

// An empty or choice-less reply must be an error, not an empty script that
// would render as silence on air.
func TestGenerateTextRejectsEmptyReplies(t *testing.T) {
	for name, body := range map[string]string{
		"no choices":    `{"choices":[]}`,
		"blank content": `{"choices":[{"message":{"content":"   "}}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		c, _ := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
		if _, err := c.GenerateText(context.Background(), "p"); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
		srv.Close()
	}
}

func TestGenerateTextSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"InvalidParameter","message":"bad model"}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	_, err := c.GenerateText(context.Background(), "p")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("error %q does not carry the provider message", err)
	}
}

func TestGenerateTextRejectsEmptyPrompt(t *testing.T) {
	c, _ := New(Config{APIKey: "sk-test"})
	if _, err := c.GenerateText(context.Background(), "  "); err == nil {
		t.Error("expected an error for an empty prompt")
	}
}

// TestLiveSnappyScript checks the real model against a race-state prompt and
// reports its latency — the number this client exists to improve.
// Opt-in: RUN_NET_TESTS=1 QWEN_API_KEY=… go test -run LiveSnappyScript -v
func TestLiveSnappyScript(t *testing.T) {
	if os.Getenv("RUN_NET_TESTS") != "1" || os.Getenv("QWEN_API_KEY") == "" {
		t.Skip("set RUN_NET_TESTS=1 and QWEN_API_KEY to run live tests")
	}
	c, err := New(Config{APIKey: os.Getenv("QWEN_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prompt := "You are a two-person FPV drone-racing booth: Ace (play-by-play) and Torque (analyst).\n" +
		"CURRENT HEAT: lap 2 of 3. DEMON leads, last lap 20.95. VORTEX 0.8s back.\n" +
		"BEAT: progress — a mid-heat running-order update.\n" +
		"Return exactly two lines, tagged 'Ace:' and 'Torque:'."

	start := time.Now()
	text, err := c.GenerateText(ctx, prompt)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("live script: %v", err)
	}
	t.Logf("%s produced %d chars in %.2fs", c.Model(), len(text), took.Seconds())
	if !strings.Contains(text, "Ace") {
		t.Errorf("script has no speaker tags:\n%s", text)
	}
	// Generous: this is a network test, not a benchmark gate. It only catches a
	// regression to the multi-second thinking behaviour.
	if took > 8*time.Second {
		t.Errorf("script took %.2fs — thinking may be back on", took.Seconds())
	}
}
