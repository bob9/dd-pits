package qwentts

// Voice cloning: enrolled voices live on the tenant's own Model Studio account,
// so they are listed at runtime rather than shipped as a static catalogue.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// enrollModel is the fixed model id of the voice-enrollment service — not a
// model that synthesises. The target_model given at enrollment must match the
// model used to speak, which is why clones are pinned to DefaultVCModel.
const enrollModel = "qwen-voice-enrollment"

// ClonedVoice is a voice enrolled on the account behind the API key.
//
// The list response names the id field "voice" (there is no "voice_id"), gives
// timestamps as formatted strings rather than epochs, and carries no display
// name — the enrollment name is baked into the id, which ClonedVoiceName
// recovers.
type ClonedVoice struct {
	ID       string `json:"voice"`
	Model    string `json:"target_model"`
	Language string `json:"language"`
	Created  string `json:"gmt_create"`
}

// Name is the enrollment name, for showing in a picker.
func (v ClonedVoice) Name() string { return ClonedVoiceName(v.ID) }

// ListVoices returns the cloned voices enrolled on this account, newest first
// as DashScope orders them. An account with none returns an empty slice.
func (c *Client) ListVoices(ctx context.Context) ([]ClonedVoice, error) {
	body := map[string]any{
		"model": enrollModel,
		"input": map[string]any{"action": "list", "page_size": 50},
	}
	var resp struct {
		Output struct {
			VoiceList []ClonedVoice `json:"voice_list"`
		} `json:"output"`
	}
	if err := c.customization(ctx, body, &resp); err != nil {
		return nil, err
	}
	out := make([]ClonedVoice, 0, len(resp.Output.VoiceList))
	for _, v := range resp.Output.VoiceList {
		if strings.TrimSpace(v.ID) != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// customization POSTs to the voice-management endpoint (a different path from
// synthesis, and not an SSE stream).
func (c *Client) customization(ctx context.Context, body map[string]any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("qwen-tts: marshal voice request: %w", err)
	}
	url := c.baseURL + "/api/v1/services/audio/tts/customization"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("qwen-tts: build voice request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qwen-tts: voice request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("qwen-tts: decode voice response: %w", err)
	}
	return nil
}

// EnrollVoice clones a recording into a reusable voice on the account behind
// the API key, returning the new voice id (qwen-tts-vc-…).
//
// This is the second half of design→clone: render a sample with the character
// baked in (an instructed render is the design step — there is no separate
// design API), then enroll that sample once. The result is a STABLE voice:
// the character no longer depends on re-sending the instruction, and per-call
// instructions ride on top as expression, because the VC model accepts them.
//
// The audio travels as a base64 data URI — discovered by probing, not
// documented: the service accepts {"audio":{"data":"data:audio/wav;base64,…"}}
// and answers Audio.PreprocessError for a decodable file with no speech in it,
// which is how we know it decodes inline data rather than demanding a URL.
func (c *Client) EnrollVoice(ctx context.Context, name string, wav []byte) (string, error) {
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return -1
	}, name)
	if clean == "" {
		clean = "custom"
	}
	if len(clean) > 24 {
		clean = clean[:24]
	}
	body := map[string]any{
		"model": enrollModel,
		"input": map[string]any{
			"action":         "create",
			"target_model":   DefaultVCModel,
			"preferred_name": clean,
			"audio": map[string]any{
				"data": "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(wav),
			},
		},
	}
	var resp struct {
		Output struct {
			Voice string `json:"voice"`
		} `json:"output"`
	}
	if err := c.customization(ctx, body, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Output.Voice) == "" {
		return "", fmt.Errorf("qwen-tts: enrollment returned no voice id")
	}
	return resp.Output.Voice, nil
}
