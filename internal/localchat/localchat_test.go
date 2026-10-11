package localchat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJSONStringNeverBreaksTheLineProtocol pins the one property the hand-rolled
// quoting exists for: the protocol is line-based, so a prompt containing a
// newline would be read as several commands, with the tail of the prompt run as
// one of them. Booth prompts are multi-line by nature.
func TestJSONStringNeverBreaksTheLineProtocol(t *testing.T) {
	prompt := "You are a commentator.\nRULES:\n- Say \"go\"\n- Use a \\ backslash\ttoo\r\n"

	got := jsonString(prompt)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("quoted prompt still contains a raw newline: %q", got)
	}

	// It must also be genuinely valid JSON — the sidecar parses it.
	var round string
	if err := json.Unmarshal([]byte(got), &round); err != nil {
		t.Fatalf("not valid JSON: %v (%q)", err, got)
	}
	if round != prompt {
		t.Fatalf("round-trip changed the prompt:\n got %q\nwant %q", round, prompt)
	}
}

func TestJSONStringMatchesEncodingJSON(t *testing.T) {
	for _, s := range []string{
		"plain",
		`quotes "here"`,
		"tab\there",
		"unicode — em dash, ½, 🏁",
		"control\x01char",
	} {
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var a, b string
		if err := json.Unmarshal([]byte(jsonString(s)), &a); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if err := json.Unmarshal(want, &b); err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Errorf("%q decoded as %q, want %q", s, a, b)
		}
	}
}

// TestNilClientIsSafe covers the path that actually runs when the model failed
// to load: the booth keeps a nil writer and must fall back, not panic.
func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	if _, err := c.GenerateText(t.Context(), "anything"); err == nil {
		t.Fatal("a nil client must report an error so the caller falls back")
	}
	if c.Model() != "" {
		t.Fatal("a nil client has no model")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("closing a nil client must be harmless, got %v", err)
	}
}
