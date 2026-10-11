// Package tts is the seam between the commentary pipelines and whichever
// speech backend a tenant has configured — Gemini TTS, or Qwen3-TTS when the
// club has supplied a Qwen key.
//
// The interesting part of the contract is Request keeping the WORDS apart from
// the DIRECTION. Gemini takes delivery notes as a natural-language preamble
// glued to the front of the script; Qwen takes them in a separate instructions
// field and would read a preamble out loud. Callers already build the two
// halves separately, so handing them over separately costs nothing and lets
// each backend assemble them the way its API expects.
package tts

import (
	"context"
	"regexp"

	"github.com/bob9/dd-pits/internal/gemini"
	"github.com/bob9/dd-pits/internal/qwentts"
)

// Provider names, for logs, the admin page and the spend meter.
const (
	ProviderGemini = "gemini"
	ProviderQwen   = "qwen"
)

// Speaker maps a script's speaker label to the backend voice that reads it.
type Speaker struct {
	Speaker string
	Voice   string
}

// Request is one synthesis job.
type Request struct {
	// Text is ONLY the words to be spoken. For a dialogue it carries the
	// "Ace: …" / "Torque: …" speaker tags matching Speakers.
	Text string
	// Instruction is the delivery direction (emotion, style, accent). Optional.
	Instruction string
	// Voice renders a solo take. Ignored when Speakers is set.
	Voice string
	// Speakers renders a dialogue, one voice per speaker label.
	Speakers []Speaker
	// Language is the platform's display name for the script's language
	// ("Dutch", "German"); blank = English. A backend may use it as a hint.
	Language string
}

// numberSecondsRE matches a DECIMAL number carrying a bare "s" unit — the shape
// every lap/split time takes in the prompts ("21.23s", "t=4.5s").
//
// Deliberately decimals only. Bare integer+s is real FPV vocabulary — a "4s
// pack", a "6s build" — and must survive untouched, as must words like "laps"
// and a decade like "1990s".
var numberSecondsRE = regexp.MustCompile(`(\d+\.\d+)s\b`)

// SpokenText prepares a script for synthesis.
//
// Times reach the model as "21.23s" and it echoes that shape into the script,
// where TTS reads the unit out as the LETTER — "twenty-one twenty-three ess".
// A commentator says the number alone, so the bare "s" is dropped. The value is
// left exactly as written; only the suffix goes.
func SpokenText(s string) string {
	return numberSecondsRE.ReplaceAllString(s, "$1")
}

// Engine is a speech backend. Returns raw 16-bit little-endian mono PCM and
// its sample rate — the shape pcmToWAV and the booth mixer already expect.
type Engine interface {
	Speak(ctx context.Context, req Request) ([]byte, int, error)
	// Provider is ProviderGemini or ProviderQwen.
	Provider() string
	// SetUsageHook installs the token meter. Call before first use.
	SetUsageHook(func(kind string, inputTokens, outputTokens int))
}

// ── Gemini ───────────────────────────────────────────────────────────────────

// FromGemini adapts a Gemini client to Engine. It re-joins Instruction and Text
// exactly as the call sites used to, so Gemini output is unchanged by the
// introduction of this seam.
func FromGemini(c *gemini.Client) Engine { return &geminiEngine{c: c} }

type geminiEngine struct{ c *gemini.Client }

func (g *geminiEngine) Provider() string { return ProviderGemini }

func (g *geminiEngine) SetUsageHook(fn func(string, int, int)) { g.c.OnUsage = fn }

// GeminiPrompt is how the Gemini backend merges the delivery direction into
// the script: as a preamble the TTS model reads as direction, not as words.
// Exported so the byte-for-byte shape the call sites used before this seam
// existed is pinned by a test rather than by careful reading.
func GeminiPrompt(instruction, text string) string {
	if instruction == "" {
		return text
	}
	return instruction + "\n\n" + text
}

func (g *geminiEngine) Speak(ctx context.Context, req Request) ([]byte, int, error) {
	text := GeminiPrompt(req.Instruction, SpokenText(req.Text))
	if len(req.Speakers) == 0 {
		return g.c.GenerateSpeech(ctx, text, req.Voice)
	}
	sv := make([]gemini.SpeakerVoice, len(req.Speakers))
	for i, s := range req.Speakers {
		sv[i] = gemini.SpeakerVoice{Speaker: s.Speaker, Voice: s.Voice}
	}
	return g.c.GenerateSpeechMulti(ctx, text, sv)
}

// ── Qwen ─────────────────────────────────────────────────────────────────────

// FromQwen adapts a Qwen3-TTS client to Engine.
func FromQwen(c *qwentts.Client) Engine { return &qwenEngine{c: c} }

type qwenEngine struct{ c *qwentts.Client }

func (q *qwenEngine) Provider() string { return ProviderQwen }

func (q *qwenEngine) SetUsageHook(fn func(string, int, int)) { q.c.OnUsage = fn }

func (q *qwenEngine) Speak(ctx context.Context, req Request) ([]byte, int, error) {
	if len(req.Speakers) == 0 {
		return q.c.Speak(ctx, SpokenText(req.Text), req.Voice, req.Instruction, req.Language)
	}
	sv := make([]qwentts.SpeakerVoice, len(req.Speakers))
	for i, s := range req.Speakers {
		sv[i] = qwentts.SpeakerVoice{Speaker: s.Speaker, Voice: s.Voice}
	}
	return q.c.SpeakDialogue(ctx, SpokenText(req.Text), sv, req.Instruction, req.Language)
}
