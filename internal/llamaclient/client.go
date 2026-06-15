// Package llamaclient calls a llama.cpp server to generate grammar-constrained
// output. It uses /v1/chat/completions (so the Gemma chat template is applied
// by the server via --jinja) plus a raw "grammar" field — which avoids the
// json_schema crash path (#22396) while still constraining structure.
package llamaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	base    string
	path    string
	model   string
	http    *http.Client
}

// New builds a client. path is the generation route (default
// /v1/chat/completions); model is the llama-swap alias ("" = dedicated server).
func New(base, path, model string, timeout time.Duration) *Client {
	if path == "" {
		path = "/v1/chat/completions"
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		path:  path,
		model: model,
		http:  &http.Client{Timeout: timeout},
	}
}

// AltToken is one candidate at a generated position (raw, pre-grammar-mask).
type AltToken struct {
	Token   string
	Logprob float64
}

// TokenLogprob is the chosen token at one output position plus the top
// alternatives. NOTE: with a grammar active, these are the model's RAW
// distribution (pre-mask) — grammar-illegal tokens can appear, and the chosen
// token's logprob may be low if the grammar forced a non-preferred spelling.
// Confidence metrics must aggregate by legal class, not trust raw logprobs.
type TokenLogprob struct {
	Token string
	Top   []AltToken
}

// GenResult holds the model output and per-call telemetry.
type GenResult struct {
	Content   string
	TokensIn  int
	TokensOut int
	TokPerSec float64
	Truncated bool           // hit max_tokens before finishing (finish_reason == "length")
	Logprobs  []TokenLogprob // per-output-token, only when top_logprobs was requested
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Model       string    `json:"model,omitempty"`
	Messages    []chatMsg `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Grammar     string    `json:"grammar,omitempty"`
	Logprobs    bool      `json:"logprobs,omitempty"`
	TopLogprobs int       `json:"top_logprobs,omitempty"`
	CachePrompt bool      `json:"cache_prompt"`
	Stream      bool      `json:"stream"`
}

// --- multimodal (vision) request types ---
// The vision path sends OpenAI-style array content (text + image_url parts) so a
// VLM (e.g. qwen3vl-4b) can attach images. Text Generate keeps its plain-string
// content; these types are vision-only and never touch the text path.
type imageURL struct {
	URL string `json:"url"`
}
type contentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}
type mmMsg struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}
type mmChatReq struct {
	Model       string  `json:"model,omitempty"`
	Messages    []mmMsg `json:"messages"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Grammar     string  `json:"grammar,omitempty"`
	Logprobs    bool    `json:"logprobs,omitempty"`
	TopLogprobs int     `json:"top_logprobs,omitempty"`
	CachePrompt bool    `json:"cache_prompt"`
	Stream      bool    `json:"stream"`
}

type respAlt struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Logprobs     *struct {
			Content []struct {
				Token       string    `json:"token"`
				Logprob     float64   `json:"logprob"`
				TopLogprobs []respAlt `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings *struct {
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"`
}

// Generate sends system+user as a chat request constrained by grammar (may be
// empty) and returns the content plus telemetry. model overrides the client's
// default (empty = use the default); this is how the family cascade routes to
// different tiers (e2b / e4b / 26b-a4b) per call. When topLogprobs > 0 the
// server returns per-token raw (pre-grammar-mask) logprobs in GenResult.Logprobs
// — used by the confidence gate to detect a genuinely uncertain decision.
func (c *Client) Generate(ctx context.Context, model, system, user, grammar string, maxTokens int, temperature float64, topLogprobs int) (GenResult, error) {
	if model == "" {
		model = c.model
	}
	body := chatReq{
		Model:       model,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Grammar:     grammar,
		CachePrompt: true,
		Messages:    []chatMsg{},
	}
	if topLogprobs > 0 {
		body.Logprobs = true
		body.TopLogprobs = topLogprobs
	}
	if system != "" {
		body.Messages = append(body.Messages, chatMsg{Role: "system", Content: system})
	}
	body.Messages = append(body.Messages, chatMsg{Role: "user", Content: user})

	buf, err := json.Marshal(body)
	if err != nil {
		return GenResult{}, err
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+c.path, bytes.NewReader(buf))
	if err != nil {
		return GenResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return GenResult{}, err
	}
	return decodeGenResult(resp, start)
}

// GenerateVision sends a multimodal chat request: the user message carries the
// prompt text plus one image_url part per data URI in imageDataURIs (each a full
// data:image/...;base64,... URI). model overrides the client default ("" = use
// it); grammar may be empty. It shares decodeGenResult with Generate, so
// telemetry/logprob handling is identical. CachePrompt is forced OFF on the
// vision path (llama.cpp #17200: consecutive-image KV reuse can corrupt output).
func (c *Client) GenerateVision(ctx context.Context, model, system, user string, imageDataURIs []string, grammar string, maxTokens int, temperature float64, topLogprobs int) (GenResult, error) {
	if model == "" {
		model = c.model
	}
	userParts := make([]contentPart, 0, 1+len(imageDataURIs))
	userParts = append(userParts, contentPart{Type: "text", Text: user})
	for _, uri := range imageDataURIs {
		userParts = append(userParts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: uri}})
	}
	body := mmChatReq{
		Model:       model,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Grammar:     grammar,
		CachePrompt: false, // vision: KV reuse across images can corrupt (llama.cpp #17200)
		Messages:    []mmMsg{},
	}
	if topLogprobs > 0 {
		body.Logprobs = true
		body.TopLogprobs = topLogprobs
	}
	if system != "" {
		body.Messages = append(body.Messages, mmMsg{Role: "system", Content: []contentPart{{Type: "text", Text: system}}})
	}
	body.Messages = append(body.Messages, mmMsg{Role: "user", Content: userParts})

	buf, err := json.Marshal(body)
	if err != nil {
		return GenResult{}, err
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+c.path, bytes.NewReader(buf))
	if err != nil {
		return GenResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return GenResult{}, err
	}
	return decodeGenResult(resp, start)
}

// decodeGenResult turns a llama-server chat response into a GenResult. It owns
// status handling, body decode, and per-call telemetry (incl. raw logprobs), so
// both Generate (text) and GenerateVision (multimodal) share one decode path.
// It closes resp.Body.
func decodeGenResult(resp *http.Response, start time.Time) (GenResult, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return GenResult{}, fmt.Errorf("llama-server %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var cr chatResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return GenResult{}, err
	}
	if len(cr.Choices) == 0 {
		return GenResult{}, fmt.Errorf("llama-server returned no choices")
	}
	elapsed := time.Since(start)
	out := GenResult{
		Content:   cr.Choices[0].Message.Content,
		TokensIn:  cr.Usage.PromptTokens,
		TokensOut: cr.Usage.CompletionTokens,
		Truncated: cr.Choices[0].FinishReason == "length",
	}
	if cr.Timings != nil && cr.Timings.PredictedPerSecond > 0 {
		out.TokPerSec = cr.Timings.PredictedPerSecond
	} else if out.TokensOut > 0 && elapsed > 0 {
		out.TokPerSec = float64(out.TokensOut) / elapsed.Seconds()
	}
	if lp := cr.Choices[0].Logprobs; lp != nil {
		out.Logprobs = make([]TokenLogprob, 0, len(lp.Content))
		for _, t := range lp.Content {
			tl := TokenLogprob{Token: t.Token, Top: make([]AltToken, 0, len(t.TopLogprobs))}
			for _, a := range t.TopLogprobs {
				tl.Top = append(tl.Top, AltToken{Token: a.Token, Logprob: a.Logprob})
			}
			out.Logprobs = append(out.Logprobs, tl)
		}
	}
	return out, nil
}

// Health reports whether the server answers /health with 200.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
