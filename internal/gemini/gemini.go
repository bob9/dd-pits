// Package gemini provides a client for extracting structured data from receipt
// images and PDFs, generating text, and styling images using Google's Gemini models.
//
// Supports two backends:
//   - Google AI Studio (free tier) — set GEMINI_API_KEY
//   - Vertex AI (paid)             — set GEMINI_PROJECT_ID + credentials
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// Vertex AI endpoints
	vertexEndpointFmt       = "https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent"
	vertexGlobalEndpointFmt = "https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:generateContent"

	// Google AI Studio endpoint
	aiStudioEndpointFmt = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

	defaultModel    = "gemini-3-flash-preview"
	defaultLocation = "global"

	// fallbackModels are tried in order when the primary model returns 503 (overloaded).
	fallbackModels = "gemini-3.1-flash-lite-preview,gemini-2.5-flash,gemini-2.5-flash-lite"

	// imageGenModel is the Gemini model used for image generation (style photos).
	imageGenModel = "gemini-2.0-flash-exp"

	// ttsModel is the Gemini model used for text-to-speech (race commentary
	// audio). Available on the free AI Studio tier; style ("say excitedly…")
	// is controlled with natural language in the prompt itself.
	ttsModel = "gemini-2.5-flash-preview-tts"

	// requestTimeout is the maximum time to wait for Gemini to respond.
	requestTimeout = 60 * time.Second

	// vertexScope is the OAuth2 scope required for Vertex AI.
	vertexScope = "https://www.googleapis.com/auth/cloud-platform"
)

// Config holds configuration for the Gemini client.
type Config struct {
	// APIKey is the Google AI Studio API key (free tier).
	// If set, the client uses the AI Studio endpoint and ignores Vertex AI settings.
	APIKey string

	// ProjectID is the GCP project ID for Vertex AI (paid tier).
	ProjectID string
	// Location is the GCP region (default: "global").
	Location string
	// Model is the Gemini model to use (default: "gemini-3-flash-preview").
	Model string
	// CredentialsFile is the path to the service account JSON key file.
	// If empty, Application Default Credentials are used.
	CredentialsFile string
}

// LineItem is a single line item extracted from a receipt.
// Qty and UnitPrice are 0 when not present (JSON null maps to 0).
type LineItem struct {
	Description string  `json:"description"`
	Qty         float64 `json:"qty"`
	UnitPrice   float64 `json:"unit_price"`
	Amount      float64 `json:"amount"`
}

// ExtractedReceipt holds structured data extracted from a receipt by Gemini.
// All fields are pointers — nil means the value was not found on the receipt.
type ExtractedReceipt struct {
	Vendor    *string
	ABN       *string
	Invoice   *string
	Date      *string // ISO 8601 date string: "2024-03-15"
	Currency  *string // ISO 4217 code only when the receipt makes it clear
	LineItems []LineItem
	Subtotal  *float64
	GST       *float64
	Total     *float64
}

// Client calls the Gemini API to extract receipt data, generate text, etc.
type Client struct {
	cfg         Config
	useAIStudio bool
	tokenSrc    oauth2.TokenSource // nil when using AI Studio
	httpClient  *http.Client

	// OnUsage, when set (before first use — not synchronised), receives the
	// token usage of every text ("text") and speech ("tts") generation, so
	// callers can keep a running cost total (e.g. the dd-pits booth page).
	OnUsage func(kind string, inputTokens, outputTokens int)
}

// reportUsage forwards a response's usageMetadata to OnUsage when set.
func (c *Client) reportUsage(kind string, body []byte) {
	if c.OnUsage == nil {
		return
	}
	var env struct {
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(body, &env) == nil {
		c.OnUsage(kind, env.UsageMetadata.PromptTokenCount, env.UsageMetadata.CandidatesTokenCount)
	}
}

// New creates a new Gemini client.
// If cfg.APIKey is set, uses the free Google AI Studio API.
// Otherwise, uses Vertex AI with GCP credentials.
func New(cfg Config) (*Client, error) {
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.Location == "" {
		cfg.Location = defaultLocation
	}

	// AI Studio mode — API key auth, no GCP credentials needed
	if cfg.APIKey != "" {
		return &Client{
			cfg:         cfg,
			useAIStudio: true,
			httpClient:  &http.Client{Timeout: requestTimeout},
		}, nil
	}

	// Vertex AI mode — OAuth2 credentials
	var tokenSrc oauth2.TokenSource

	if cfg.CredentialsFile != "" {
		data, readErr := os.ReadFile(cfg.CredentialsFile)
		if readErr != nil {
			return nil, fmt.Errorf("gemini: read credentials file: %w", readErr)
		}
		creds, credsErr := google.CredentialsFromJSON(context.Background(), data, vertexScope)
		if credsErr != nil {
			return nil, fmt.Errorf("gemini: parse credentials: %w", credsErr)
		}
		tokenSrc = creds.TokenSource
	} else {
		// Fall back to Application Default Credentials
		creds, credsErr := google.FindDefaultCredentials(context.Background(), vertexScope)
		if credsErr != nil {
			return nil, fmt.Errorf("gemini: find default credentials: %w", credsErr)
		}
		tokenSrc = creds.TokenSource
	}

	return &Client{
		cfg:      cfg,
		tokenSrc: tokenSrc,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}, nil
}

// endpointForModel returns the API URL for the given model.
func (c *Client) endpointForModel(model string) string {
	if c.useAIStudio {
		return fmt.Sprintf(aiStudioEndpointFmt, model, c.cfg.APIKey)
	}
	if c.cfg.Location == "global" {
		return fmt.Sprintf(vertexGlobalEndpointFmt, c.cfg.ProjectID, model)
	}
	return fmt.Sprintf(vertexEndpointFmt, c.cfg.Location, c.cfg.ProjectID, c.cfg.Location, model)
}

// doRequest sends a JSON request body to the given endpoint with appropriate auth.
// Returns the response body, HTTP status code, and any error.
func (c *Client) doRequest(ctx context.Context, endpoint string, bodyBytes []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("gemini: build HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Vertex AI uses Bearer token auth; AI Studio uses the API key in the URL
	if !c.useAIStudio {
		token, tokenErr := c.tokenSrc.Token()
		if tokenErr != nil {
			return nil, 0, fmt.Errorf("gemini: get auth token: %w", tokenErr)
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gemini: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("gemini: read response body: %w", err)
	}

	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("gemini: API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, resp.StatusCode, nil
}

// doRequestWithFallback tries the primary model, then falls back to alternative
// models if the primary returns 503 (overloaded).
func (c *Client) doRequestWithFallback(ctx context.Context, model string, bodyBytes []byte) ([]byte, error) {
	endpoint := c.endpointForModel(model)
	respBody, status, err := c.doRequest(ctx, endpoint, bodyBytes)
	if err == nil {
		return respBody, nil
	}

	// Only try fallbacks on 503 (model overloaded) or 429 (rate limited)
	if status != 503 && status != 429 {
		return nil, err
	}

	for _, fb := range strings.Split(fallbackModels, ",") {
		fb = strings.TrimSpace(fb)
		if fb == "" || fb == model {
			continue
		}
		log.Printf("gemini: model %s returned %d, trying fallback %s", model, status, fb)
		endpoint = c.endpointForModel(fb)
		respBody, _, fbErr := c.doRequest(ctx, endpoint, bodyBytes)
		if fbErr == nil {
			return respBody, nil
		}
		log.Printf("gemini: fallback %s also failed: %v", fb, fbErr)
	}

	return nil, err // return the original error
}

// ExtractReceiptData sends a receipt file to Gemini and returns extracted fields.
// filePath is the path on disk; mimeType should be "image/jpeg", "image/png",
// "image/webp", or "application/pdf".
func (c *Client) ExtractReceiptData(ctx context.Context, filePath, mimeType string) (*ExtractedReceipt, error) {
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("gemini: read file %q: %w", filePath, err)
	}
	encoded := base64.StdEncoding.EncodeToString(fileData)

	prompt := `You are a precise receipt data extractor. Analyse this receipt image or PDF and extract the following fields.

Return ONLY a single valid JSON object with these exact keys. Use null for any field not clearly present on the receipt.
Do NOT guess, estimate, or invent values. Do NOT include any text, explanation, or markdown outside the JSON object.

{
  "vendor": "<business or store name, null if not found>",
  "abn": "<Australian Business Number digits only, null if not found>",
  "invoice": "<invoice or receipt number, null if not found>",
  "date": "<purchase date in YYYY-MM-DD format, null if not found>",
  "currency": "<ISO 4217 currency code such as AUD or USD, null if the receipt only uses an ambiguous symbol such as $>",
  "line_items": [
    {"description": "<item name or description>", "qty": <quantity as a number or null>, "unit_price": <unit price as a number or null>, "amount": <line total as a number>}
  ],
  "subtotal": <pre-tax subtotal as a number, null if not shown>,
  "gst": <GST amount as a number. If the receipt says "GST included" or "incl. GST" but does not show the amount separately, calculate it as total / 11. null only if no GST applies at all>,
  "total": <total amount paid as a number, null if not found>
}

If there are no individual line items visible on the receipt, return an empty array [] for line_items.`

	reqBody := buildRequest(prompt, mimeType, encoded)
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w", err)
	}

	endpoint := c.endpointForModel(c.cfg.Model)
	log.Printf("gemini: endpoint=%s", endpoint)
	log.Printf("gemini: model=%s file=%s mime=%s", c.cfg.Model, filePath, mimeType)

	respBody, _, err := c.doRequest(ctx, endpoint, bodyBytes)
	if err != nil {
		return nil, err
	}

	extracted, err := parseGeminiResponse(respBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: parse response: %w", err)
	}

	log.Printf("gemini: extraction complete — vendor=%v invoice=%v total=%v",
		ptrStr(extracted.Vendor), ptrStr(extracted.Invoice), ptrFloat(extracted.Total))

	return extracted, nil
}

// ExtractBankStatementJSON transcribes a bank-statement PDF into a candidate
// ledger. Callers must validate every balance and identity field before import.
func (c *Client) ExtractBankStatementJSON(ctx context.Context, filePath string) ([]byte, error) {
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("gemini: read bank statement %q: %w", filePath, err)
	}
	prompt := `You are a bank-statement transcription engine. Transcribe every transaction from every page of this PDF exactly as printed.

Return ONLY one valid JSON object using this schema. Monetary values MUST be decimal strings without currency symbols or thousands separators. Use null when a field is not clearly printed. Never infer, estimate, merge, categorise, omit, or invent transactions.

{
  "institution": "<bank name or null>",
  "account_name": "<printed account name or null>",
  "account_last_four": "<last four account digits or null>",
  "period_start": "YYYY-MM-DD",
  "period_end": "YYYY-MM-DD",
  "opening_balance": "0.00",
  "closing_balance": "0.00",
  "total_debits": "<printed statement debit/withdrawal total or null>",
  "total_credits": "<printed statement credit/deposit total or null>",
  "declared_transaction_count": <printed transaction count or null>,
  "transactions": [{
    "source_row_number": 1,
    "date": "YYYY-MM-DD",
    "description": "<complete description exactly as printed>",
    "payee": "<printed counterparty or null>",
    "reference": "<printed reference or null>",
    "cheque_number": "<printed cheque number or null>",
    "amount": "<-12.34 for money out, 12.34 for money in, or null when separate debit/credit columns are used>",
    "debit": "<positive magnitude or null>",
    "credit": "<positive magnitude or null>",
    "balance": "<running balance or null>"
  }]
}

The amount representation is strictly mutually exclusive. For a statement with separate withdrawal/debit and deposit/credit columns, amount MUST be null and exactly one of debit or credit MUST contain the printed positive magnitude. Never copy the same value into amount and debit or credit.

Keep transactions in printed order and number them sequentially across pages. Exclude headers, carried-forward lines, opening/closing labels, subtotals and page totals. Preserve repeated transactions as separate rows. If the PDF is unreadable, password protected, incomplete, or any required date/amount is unclear, return {"error":"a precise reason"} instead of guessing.`

	reqBody := buildFileRequest(prompt, "application/pdf", base64.StdEncoding.EncodeToString(fileData), 65536)
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal bank statement request: %w", err)
	}
	respBody, err := c.doRequestWithFallback(ctx, c.cfg.Model, bodyBytes)
	if err != nil {
		return nil, err
	}
	raw, err := extractJSONResponseText(respBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: parse bank statement response: %w", err)
	}
	return []byte(raw), nil
}

// GenerateText sends a text-only prompt to Gemini and returns the raw text response.
// This is used for general-purpose text generation (e.g. answering questions).
func (c *Client) GenerateText(ctx context.Context, prompt string) (string, error) {
	return c.generateText(ctx, prompt, 2048)
}

// GenerateTextLong sends a text-only prompt to Gemini with a higher output token limit.
// Use this for tasks that require returning large documents (e.g. AI-assisted editing).
func (c *Client) GenerateTextLong(ctx context.Context, prompt string) (string, error) {
	return c.generateText(ctx, prompt, 65536)
}

func (c *Client) generateText(ctx context.Context, prompt string, maxOutputTokens int) (string, error) {
	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": prompt},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"temperature":     0.2,
			"maxOutputTokens": maxOutputTokens,
			"thinkingConfig": map[string]interface{}{
				"thinkingBudget": 0,
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("gemini: marshal request: %w", err)
	}

	respBody, err := c.doRequestWithFallback(ctx, c.cfg.Model, bodyBytes)
	if err != nil {
		return "", err
	}
	c.reportUsage("text", respBody)

	// Parse the response envelope
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return "", fmt.Errorf("gemini: decode envelope: %w", err)
	}
	if len(envelope.Candidates) == 0 || len(envelope.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini: empty candidates in response")
	}

	return strings.TrimSpace(envelope.Candidates[0].Content.Parts[0].Text), nil
}

// SpeakerVoice binds a speaker label used in a dialogue script (e.g. "Ace:")
// to a prebuilt Gemini voice, for multi-speaker TTS.
type SpeakerVoice struct {
	Speaker string
	Voice   string
}

// GenerateSpeech renders text to speech with the Gemini TTS model and returns
// raw PCM sample bytes (16-bit little-endian mono) plus the sample rate parsed
// from the response MIME (24000 when unstated). Delivery/emotion is steered by
// the prompt text itself — prefix the script with a natural-language style
// instruction (e.g. "Read this like an over-excited race commentator: …").
// voiceName picks a prebuilt Gemini voice (e.g. "Puck"); empty = model default.
func (c *Client) GenerateSpeech(ctx context.Context, text, voiceName string) ([]byte, int, error) {
	var speechCfg map[string]interface{}
	if voiceName != "" {
		speechCfg = map[string]interface{}{
			"voiceConfig": map[string]interface{}{
				"prebuiltVoiceConfig": map[string]interface{}{"voiceName": voiceName},
			},
		}
	}
	return c.generateSpeech(ctx, text, speechCfg)
}

// GenerateSpeechMulti renders a dialogue to speech with one distinct voice per
// speaker (Gemini multi-speaker TTS, max two speakers). The text must tag each
// line with the speaker label ("Ace: …" / "Torque: …") matching the configs.
func (c *Client) GenerateSpeechMulti(ctx context.Context, text string, speakers []SpeakerVoice) ([]byte, int, error) {
	cfgs := make([]map[string]interface{}, len(speakers))
	for i, s := range speakers {
		cfgs[i] = map[string]interface{}{
			"speaker": s.Speaker,
			"voiceConfig": map[string]interface{}{
				"prebuiltVoiceConfig": map[string]interface{}{"voiceName": s.Voice},
			},
		}
	}
	return c.generateSpeech(ctx, text, map[string]interface{}{
		"multiSpeakerVoiceConfig": map[string]interface{}{"speakerVoiceConfigs": cfgs},
	})
}

func (c *Client) generateSpeech(ctx context.Context, text string, speechCfg map[string]interface{}) ([]byte, int, error) {
	genCfg := map[string]interface{}{
		"responseModalities": []string{"AUDIO"},
	}
	if speechCfg != nil {
		genCfg["speechConfig"] = speechCfg
	}
	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role":  "user",
				"parts": []map[string]interface{}{{"text": text}},
			},
		},
		"generationConfig": genCfg,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("gemini: marshal tts request: %w", err)
	}

	respBody, _, err := c.doRequest(ctx, c.endpointForModel(ttsModel), bodyBytes)
	if err != nil {
		return nil, 0, err
	}
	c.reportUsage("tts", respBody)

	var envelope ttsEnvelope
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, 0, fmt.Errorf("gemini: decode tts envelope: %w", err)
	}
	for _, cand := range envelope.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData == nil || !strings.HasPrefix(part.InlineData.MimeType, "audio/") {
				continue
			}
			pcm, decErr := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if decErr != nil {
				return nil, 0, fmt.Errorf("gemini: decode tts audio: %w", decErr)
			}
			return pcm, ParseAudioRate(part.InlineData.MimeType), nil
		}
	}
	// No audio came back. Say WHY — the model reports it in finishReason, a
	// prompt block, or (when it answers instead of speaking) a text part.
	return nil, 0, fmt.Errorf("%w%s", ErrNoAudio, envelope.noAudioDetail())
}

// ttsEnvelope is the generateContent response shape for a TTS call.
type ttsEnvelope struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
		Content      struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// ErrNoAudio is returned when a TTS call succeeds at the HTTP level but the
// response carries no audio part. It is usually transient (the model
// occasionally returns an empty candidate), so callers should retry before
// giving up — see the TTS retry helpers in the handlers package.
var ErrNoAudio = errors.New("gemini: no audio data in tts response")

// noAudioDetail describes an audio-less TTS response for the error message.
// Empty when the response says nothing useful (a bare empty candidate).
func (e ttsEnvelope) noAudioDetail() string {
	var bits []string
	if len(e.Candidates) == 0 {
		bits = append(bits, "no candidates")
	} else if fr := e.Candidates[0].FinishReason; fr != "" && fr != "STOP" {
		bits = append(bits, "finishReason="+fr)
	}
	if e.PromptFeedback.BlockReason != "" {
		bits = append(bits, "blocked="+e.PromptFeedback.BlockReason)
	}
	for _, c := range e.Candidates {
		for _, p := range c.Content.Parts {
			if t := strings.TrimSpace(p.Text); t != "" {
				if len(t) > 160 {
					t = t[:160] + "…"
				}
				bits = append(bits, "model replied with text: "+t)
				break
			}
		}
	}
	if len(bits) == 0 {
		return ""
	}
	return " (" + strings.Join(bits, "; ") + ")"
}

// ParseAudioRate extracts the sample rate from a Gemini audio MIME type such
// as "audio/L16;codec=pcm;rate=24000". Falls back to 24000.
func ParseAudioRate(mime string) int {
	for _, seg := range strings.Split(mime, ";") {
		seg = strings.TrimSpace(seg)
		if v, ok := strings.CutPrefix(seg, "rate="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 24000
}

// GenerateStyledImage sends a source image to Gemini and requests a styled
// version using the provided style prompt. Returns the generated image bytes,
// the output MIME type (e.g. "image/png"), and any error.
func (c *Client) GenerateStyledImage(ctx context.Context, imagePath, mimeType, stylePrompt string) ([]byte, string, error) {
	fileData, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, "", fmt.Errorf("gemini: read image %q: %w", imagePath, err)
	}
	encoded := base64.StdEncoding.EncodeToString(fileData)

	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{
						"inlineData": map[string]string{
							"mimeType": mimeType,
							"data":     encoded,
						},
					},
					{"text": stylePrompt},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"IMAGE", "TEXT"},
			"temperature":        0.8,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("gemini: marshal request: %w", err)
	}

	endpoint := c.endpointForModel(imageGenModel)
	log.Printf("gemini/image: endpoint=%s file=%s mime=%s", endpoint, imagePath, mimeType)

	respBody, _, err := c.doRequest(ctx, endpoint, bodyBytes)
	if err != nil {
		return nil, "", err
	}

	// Parse response — look for inlineData parts with image content.
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text"`
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, "", fmt.Errorf("gemini: decode envelope: %w", err)
	}
	if len(envelope.Candidates) == 0 {
		return nil, "", fmt.Errorf("gemini: empty candidates in response")
	}

	for _, part := range envelope.Candidates[0].Content.Parts {
		if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "image/") {
			imgBytes, decErr := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if decErr != nil {
				return nil, "", fmt.Errorf("gemini: decode image data: %w", decErr)
			}
			return imgBytes, part.InlineData.MimeType, nil
		}
	}

	return nil, "", fmt.Errorf("gemini: no image data in response")
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// buildRequest constructs the generateContent request body for receipt extraction.
func buildRequest(prompt, mimeType, base64Data string) map[string]interface{} {
	return buildFileRequest(prompt, mimeType, base64Data, 4096)
}

func buildFileRequest(prompt, mimeType, base64Data string, maxOutputTokens int) map[string]interface{} {
	return map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": prompt},
					{
						"inlineData": map[string]string{
							"mimeType": mimeType,
							"data":     base64Data,
						},
					},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"temperature":     0,
			"maxOutputTokens": maxOutputTokens,
			"thinkingConfig": map[string]interface{}{
				"thinkingBudget": 0,
			},
		},
	}
}

func extractJSONResponseText(body []byte) (string, error) {
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Candidates) == 0 || len(envelope.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty candidates in response")
	}
	raw := strings.TrimSpace(envelope.Candidates[0].Content.Parts[0].Text)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if !json.Valid([]byte(raw)) {
		return "", fmt.Errorf("candidate did not contain valid JSON")
	}
	return raw, nil
}

// parseGeminiResponse extracts the JSON payload from the API response envelope.
func parseGeminiResponse(body []byte) (*ExtractedReceipt, error) {
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Candidates) == 0 || len(envelope.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty candidates in response")
	}

	rawText := envelope.Candidates[0].Content.Parts[0].Text
	// Strip markdown code fences that Gemini sometimes wraps around JSON
	rawText = strings.TrimPrefix(rawText, "```json")
	rawText = strings.TrimPrefix(rawText, "```")
	rawText = strings.TrimSuffix(rawText, "```")
	rawText = strings.TrimSpace(rawText)

	var parsed struct {
		Vendor    *string    `json:"vendor"`
		ABN       *string    `json:"abn"`
		Invoice   *string    `json:"invoice"`
		Date      *string    `json:"date"`
		Currency  *string    `json:"currency"`
		LineItems []LineItem `json:"line_items"`
		Subtotal  *float64   `json:"subtotal"`
		GST       *float64   `json:"gst"`
		Total     *float64   `json:"total"`
	}
	if err := json.Unmarshal([]byte(rawText), &parsed); err != nil {
		return nil, fmt.Errorf("decode extracted JSON (%q): %w", rawText, err)
	}

	// Sanitise: empty strings from Gemini should be treated as nil
	parsed.Vendor = cleanString(parsed.Vendor)
	parsed.ABN = cleanString(parsed.ABN)
	parsed.Invoice = cleanString(parsed.Invoice)
	parsed.Date = cleanString(parsed.Date)
	parsed.Currency = cleanString(parsed.Currency)
	if parsed.Currency != nil {
		currency := strings.ToUpper(*parsed.Currency)
		if len(currency) != 3 {
			parsed.Currency = nil
		} else {
			parsed.Currency = &currency
		}
	}

	if parsed.LineItems == nil {
		parsed.LineItems = []LineItem{}
	}

	return &ExtractedReceipt{
		Vendor:    parsed.Vendor,
		ABN:       parsed.ABN,
		Invoice:   parsed.Invoice,
		Date:      parsed.Date,
		Currency:  parsed.Currency,
		LineItems: parsed.LineItems,
		Subtotal:  parsed.Subtotal,
		GST:       parsed.GST,
		Total:     parsed.Total,
	}, nil
}

func cleanString(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" || v == "null" {
		return nil
	}
	return &v
}

func ptrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func ptrFloat(f *float64) string {
	if f == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%.2f", *f)
}
