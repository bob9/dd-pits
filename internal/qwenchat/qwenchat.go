// Package qwenchat is a minimal text-generation client for Alibaba Cloud Model
// Studio, used to write booth commentary scripts.
//
// It exists for ONE property: latency. Measured on a live `progress` beat, the
// thinking-enabled models spend far more effort deliberating than speaking —
// 700 tokens of reasoning for an 89-token call on gemini-3.6-flash, 1,980 on
// qwen3.7-flash — which puts a live call 4-14 seconds behind the action. The
// same prompt with `enable_thinking:false` returns in about a second.
//
// A race call is not a reasoning task: the context already states the running
// order, the gaps and the format, so the model only has to phrase it. Thinking
// is therefore disabled outright rather than exposed as a knob.
//
// The OpenAI-compatible endpoint is used because it accepts that flag directly.
package qwenchat

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
	// DefaultBaseURL is the Singapore (international) region, matching qwentts.
	DefaultBaseURL = "https://dashscope-intl.aliyuncs.com"

	// DefaultModel is the fastest model that stayed accurate about race state in
	// benchmarking. Its siblings either hallucinated the lap count or garbled
	// callsigns; qwen-flash-character, despite the name, was the worst of them.
	DefaultModel = "qwen-flash"

	// requestTimeout is deliberately short: a live call that takes this long has
	// already missed its moment, and failing fast lets the booth move on to the
	// next beat instead of blocking the queue.
	requestTimeout = 30 * time.Second
)

// Config constructs a Client.
type Config struct {
	APIKey  string
	BaseURL string // defaults to DefaultBaseURL
	Model   string // defaults to DefaultModel
	HTTP    *http.Client
}

// Client generates commentary scripts.
type Client struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client

	// OnUsage, when set before first use, receives token counts so booth spend
	// is metered exactly as it is for the Gemini path.
	OnUsage func(kind string, inputTokens, outputTokens int)
}

// New builds a client. An empty key is an error rather than a client that fails
// on first use, so a misconfigured tenant is caught at resolve time.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("qwen-chat: no API key configured")
	}
	c := &Client{
		apiKey:  strings.TrimSpace(cfg.APIKey),
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		model:   strings.TrimSpace(cfg.Model),
		http:    cfg.HTTP,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: requestTimeout}
	}
	return c, nil
}

// Model reports the model in use, for logging.
func (c *Client) Model() string { return c.model }

// GenerateText returns the model's reply to prompt. Mirrors
// gemini.Client.GenerateText so the two are interchangeable at the call site.
func (c *Client) GenerateText(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("qwen-chat: empty prompt")
	}
	body := map[string]any{
		"model":    c.model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		// The whole point of this client — see the package comment.
		"enable_thinking": false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("qwen-chat: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/compatible-mode/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("qwen-chat: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("qwen-chat: request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("qwen-chat: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qwen-chat: HTTP %d: %s", resp.StatusCode, apiError(payload))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("qwen-chat: decode response: %w", err)
	}
	if c.OnUsage != nil {
		c.OnUsage("text", out.Usage.PromptTokens, out.Usage.CompletionTokens)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("qwen-chat: no choices in response")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", errors.New("qwen-chat: empty script response")
	}
	return text, nil
}

// apiError pulls the provider's message out of an error body, falling back to
// the raw payload (truncated) when it isn't the documented shape.
func apiError(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Error.Message != "" {
			return strings.TrimSpace(e.Error.Code + " " + e.Error.Message)
		}
		if e.Message != "" {
			return strings.TrimSpace(e.Code + " " + e.Message)
		}
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return string(body)
}
