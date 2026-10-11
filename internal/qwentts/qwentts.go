// Package qwentts is a client for Alibaba Cloud Model Studio's Qwen3-TTS
// speech synthesis, offered as an alternative voice backend to Gemini TTS.
//
// Two differences from Gemini shape everything here:
//
//  1. Gemini steers delivery with a natural-language preamble glued to the
//     front of the script; Qwen's `input.text` is LITERALLY the words to speak,
//     so a preamble would be read out loud. Direction goes in `instructions`
//     instead, which only the -instruct- models accept — hence Request keeping
//     the words and the direction apart.
//  2. Qwen renders ONE voice per request. The two-person booth is therefore
//     rendered a turn at a time and concatenated (SplitTurns), which is safe
//     because every response is raw 24kHz/16-bit/mono PCM — the same format
//     Gemini returns and the same the mixer and pcmToWAV already expect.
//
// The streaming (SSE) path is used rather than the plain one: it hands back
// that raw PCM directly, where the non-streaming call returns a CDN URL that
// expires in 24h and would need a second round-trip and a container to parse.
package qwentts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Singapore (international) region — the right one
	// for Australian tenants. The Beijing region is the other option.
	DefaultBaseURL = "https://dashscope-intl.aliyuncs.com"

	// DefaultModel renders a plain script; DefaultInstructModel is the variant
	// that accepts a delivery instruction, which is what this platform always
	// has (emotion + the tenant's voice characteristics + any accent).
	DefaultModel         = "qwen3-tts-flash"
	DefaultInstructModel = "qwen3-tts-instruct-flash"

	// DefaultVCModel serves CLONED voices. The cloning and flash models reject
	// each other's voices outright — a prebuilt name on the VC model errors, and
	// a cloned id on flash returns "Invalid voice specified" — so picking per
	// voice is correctness, not preference. It accepts the same `instructions`
	// field, so a cloned voice still takes per-beat delivery direction.
	DefaultVCModel = "qwen3-tts-vc-2026-01-22"

	// UsageKindTTS is the meter bucket for Qwen speech. Distinct from Gemini's
	// "tts" because the unit differs — CHARACTERS, not tokens — and the two
	// cannot share a price. Must fit dd_booth_usage.kind (VARCHAR(10)).
	UsageKindTTS = "tts_qwen"

	// ClonedVoicePrefix marks a voice id minted by voice enrollment. DashScope
	// builds ids as qwen-tts-vc-{name}-voice-{stamp}, so the prefix separates a
	// cloned id from a prebuilt voice name without a network call.
	ClonedVoicePrefix = "qwen-tts-vc-"

	// SampleRate is what the model emits: 24kHz 16-bit mono PCM.
	SampleRate = 24000

	// MaxInstructionRunes guards the documented 1,600-token instruction cap
	// with a lot of headroom, since a tenant's voice characteristics are free
	// text and an over-long instruction is a hard API error.
	MaxInstructionRunes = 2000
)

// ErrNoAudio is returned when a call succeeds at the HTTP level but no audio
// came back — the mirror of gemini.ErrNoAudio, so callers can treat a silent
// response the same way whichever backend produced it.
var ErrNoAudio = errors.New("qwen-tts: no audio data in response")

// Config constructs a Client.
type Config struct {
	APIKey string
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// Model / InstructModel / VCModel default to the constants above.
	Model, InstructModel, VCModel string
	// HTTPClient defaults to a 120s client.
	HTTPClient *http.Client
}

// Client talks to Model Studio's multimodal-generation endpoint.
type Client struct {
	apiKey        string
	baseURL       string
	model         string
	instructModel string
	vcModel       string
	http          *http.Client

	// OnUsage, when set before first use (not synchronised), receives the
	// token counts a response reports, so a tenant's spend is metered exactly
	// as it is for Gemini.
	OnUsage func(kind string, inputTokens, outputTokens int)
}

// New builds a client. An empty API key is an error rather than a client that
// fails on first use, so a misconfigured tenant is caught at resolve time.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("qwen-tts: no API key configured")
	}
	c := &Client{
		apiKey:        strings.TrimSpace(cfg.APIKey),
		baseURL:       strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		model:         strings.TrimSpace(cfg.Model),
		instructModel: strings.TrimSpace(cfg.InstructModel),
		vcModel:       strings.TrimSpace(cfg.VCModel),
		http:          cfg.HTTPClient,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.vcModel == "" {
		c.vcModel = DefaultVCModel
	}
	if c.instructModel == "" {
		c.instructModel = DefaultInstructModel
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 120 * time.Second}
	}
	return c, nil
}

// Speak renders one voice. text is only the words; instruction (optional) is
// the delivery direction and selects the -instruct- model. Returns raw PCM and
// its sample rate.
func (c *Client) Speak(ctx context.Context, text, voice, instruction, language string) ([]byte, int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, 0, errors.New("qwen-tts: nothing to speak")
	}
	model := c.model
	instruction = TrimInstruction(instruction)
	if instruction != "" {
		model = c.instructModel
	}
	// A cloned voice is served ONLY by the cloning model, whether or not there
	// is an instruction — it accepts `instructions` too.
	if IsClonedVoice(voice) {
		model = c.vcModel
	}
	input := map[string]interface{}{"text": text}
	if voice != "" {
		input["voice"] = voice
	}
	// language_type is a hint, not a gate: send it only for a language the
	// model lists, and let it auto-detect otherwise rather than sending a
	// value it will reject.
	if lt := LanguageType(language); lt != "" {
		input["language_type"] = lt
	}
	body := map[string]interface{}{"model": model, "input": input}
	if instruction != "" {
		body["input"].(map[string]interface{})["instructions"] = instruction
	}
	return c.stream(ctx, body)
}

// SpeakDialogue renders a speaker-tagged script ("Ace: …" / "Torque: …") by
// rendering each turn in its own voice and concatenating the PCM. Qwen has no
// multi-speaker call, so this is what keeps the two-person booth two people.
//
// Every turn is rendered at the same model and sample rate, so concatenation
// is a plain append — no resampling, no re-containering.
func (c *Client) SpeakDialogue(ctx context.Context, text string, voices []SpeakerVoice, instruction, language string) ([]byte, int, error) {
	turns := SplitTurns(text, voices)
	if len(turns) == 0 {
		return nil, 0, errors.New("qwen-tts: nothing to speak")
	}
	var out []byte
	rate := 0
	for _, t := range turns {
		pcm, r, err := c.Speak(ctx, t.Text, t.Voice, instruction, language)
		if err != nil {
			return nil, 0, err
		}
		if rate == 0 {
			rate = r
		}
		out = append(out, pcm...)
	}
	if len(out) == 0 {
		return nil, 0, ErrNoAudio
	}
	return out, rate, nil
}

// stream POSTs with SSE enabled and accumulates the base64 PCM chunks.
func (c *Client) stream(ctx context.Context, body map[string]interface{}) ([]byte, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("qwen-tts: marshal request: %w", err)
	}
	url := c.baseURL + "/api/v1/services/aigc/multimodal-generation/generation"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, fmt.Errorf("qwen-tts: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("qwen-tts: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, httpError(resp)
	}
	pcm, rate, usage, err := parseSSE(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if c.OnUsage != nil && usage != nil {
		// Qwen bills per input CHARACTER and reports no tokens, so it is
		// metered under its own kind — pushing characters through the
		// token-priced Gemini formula would produce a confidently wrong figure.
		if usage.Characters > 0 {
			c.OnUsage(UsageKindTTS, usage.Characters, 0)
		} else {
			c.OnUsage("tts", usage.InputTokens, usage.OutputTokens)
		}
	}
	if len(pcm) == 0 {
		return nil, 0, ErrNoAudio
	}
	if rate == 0 {
		rate = SampleRate
	}
	return pcm, rate, nil
}

// httpError renders a non-200 with DashScope's error body. The message keeps
// the literal "API error <code>" shape the callers' rate-limit detection
// matches on, so a Qwen 429 paces the run exactly like a Gemini one.
func httpError(resp *http.Response) error {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	detail := strings.TrimSpace(buf.String())
	if json.Unmarshal(buf.Bytes(), &e) == nil && e.Message != "" {
		detail = e.Code + ": " + e.Message
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	return fmt.Errorf("qwen-tts: API error %d: %s", resp.StatusCode, detail)
}

// IsClonedVoice reports whether a voice identifier came from voice enrollment
// rather than being one of the built-in names in Voices.
func IsClonedVoice(voice string) bool {
	return strings.HasPrefix(strings.TrimSpace(voice), ClonedVoicePrefix)
}
