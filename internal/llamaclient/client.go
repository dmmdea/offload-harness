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
	"net"
	"net/http"
	"strings"
	"time"
)

// connectTimeout bounds only the TCP dial (LO-9): a dead/unreachable endpoint
// fails in ~2s instead of consuming the whole request budget, while the total
// budget stays at the caller's timeout — a llama-swap COLD SWAP holds the
// request open for the entire model load, so the end-to-end budget must
// survive it.
const connectTimeout = 2 * time.Second

type Client struct {
	base  string
	path  string
	model string
	http  *http.Client
	// seatEndpoints maps a model id/alias to a remote base URL and safeHTTP is
	// the tailnet-guarded client every overridden request rides (endpoints.go,
	// Phase A delegation). Both stay nil unless WithSeatEndpoints installs
	// overrides — the nil path is byte-identical to a pre-seat client.
	seatEndpoints map[string]string
	safeHTTP      *http.Client
	// remoteLanes maps a model id/alias to a busy-aware failover base
	// (lanes.go, roast delta 7); laneBusy and laneResident are its two
	// per-call gates. All three stay nil unless WithRemoteLanes installs
	// them — the nil path is byte-identical to a pre-lanes client.
	remoteLanes  map[string]string
	laneBusy     func() bool
	laneResident func(base, model string) bool
}

// New builds a client. path is the generation route (default
// /v1/chat/completions); model is the llama-swap alias ("" = dedicated server).
// The HTTP budget is SPLIT: connect gets connectTimeout, the whole request
// keeps `timeout`. The transport clones http.DefaultTransport so proxy/TLS
// defaults are preserved.
func New(base, path, model string, timeout time.Duration) *Client {
	if path == "" {
		path = "/v1/chat/completions"
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		path:  path,
		model: model,
		http:  &http.Client{Timeout: timeout, Transport: newTransport()},
	}
}

// newTransport builds the split-budget transport New has always used: connect
// gets connectTimeout, everything else rides the client Timeout. Extracted so
// WithSeatEndpoints wraps the SAME transport shape in the tailnet dial gate
// rather than drifting a second hand-rolled copy.
func newTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	return tr
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
// different tiers (e2b / e4b / 26b-a4b) per call. The resolved model also
// picks the BASE: a seat-endpoint override (endpoints.go) routes the request
// to that seat's remote tailnet base through the dial-guarded client, and a
// busy-aware cascade remote lane (lanes.go) does the same only while the
// local GPU lease is held — the vision paths below thread the same pair.
// When topLogprobs > 0 the
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
	base, hc := c.resolveEndpoint(model) // ONE decision: base and client must never split (lanes.go)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+c.path, bytes.NewReader(buf))
	if err != nil {
		return GenResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
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
	base, hc := c.resolveEndpoint(model) // ONE decision: base and client must never split (lanes.go)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+c.path, bytes.NewReader(buf))
	if err != nil {
		return GenResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return GenResult{}, err
	}
	return decodeGenResult(resp, start)
}

// GenerateVisionInterleaved sends a multimodal chat request whose user content
// INTERLEAVES a text label before each image (frameLabels[i] then image[i]),
// then appends trailingUser as the final text part. This matches Qwen3-VL's
// trained "<timestamp> frame" interleaved format (its MRoPE uses the timestamp
// tokens for temporal localization). frameLabels and imageDataURIs pair by index;
// a missing/empty label is skipped for that image. Shares decodeGenResult; like
// GenerateVision, CachePrompt is forced OFF (llama.cpp #17200).
func (c *Client) GenerateVisionInterleaved(ctx context.Context, model, system string, frameLabels, imageDataURIs []string, trailingUser, grammar string, maxTokens int, temperature float64, topLogprobs int) (GenResult, error) {
	if model == "" {
		model = c.model
	}
	userParts := make([]contentPart, 0, 2*len(imageDataURIs)+1)
	for i, uri := range imageDataURIs {
		if i < len(frameLabels) && frameLabels[i] != "" {
			userParts = append(userParts, contentPart{Type: "text", Text: frameLabels[i]})
		}
		userParts = append(userParts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: uri}})
	}
	if trailingUser != "" {
		userParts = append(userParts, contentPart{Type: "text", Text: trailingUser})
	}
	body := mmChatReq{
		Model:       model,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Grammar:     grammar,
		CachePrompt: false,
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
	base, hc := c.resolveEndpoint(model) // ONE decision: base and client must never split (lanes.go)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+c.path, bytes.NewReader(buf))
	if err != nil {
		return GenResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return GenResult{}, err
	}
	return decodeGenResult(resp, start)
}

// StatusError is a non-200 ANSWER from the server — as opposed to a failure to
// reach it at all, which surfaces as the transport's own *url.Error. The status
// is carried as a field, not just formatted into the text, because callers have
// to branch on the CLASS of refusal: a 5xx says the box is in trouble, a 4xx
// says the box is fine and rejected THIS request (context length exceeded, a
// grammar it cannot compile). Filing both as "unreachable" is how a contract
// mistake came to be reported as broken infrastructure.
//
// Error() keeps the exact historical text — `llama-server <code>: <body>` — a
// string other packages already match on (internal/gpugen's "llama-server 5"
// OOM classifier).
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llama-server %d: %s", e.StatusCode, e.Body)
}

// decodeGenResult turns a llama-server chat response into a GenResult. It owns
// status handling, body decode, and per-call telemetry (incl. raw logprobs), so
// both Generate (text) and GenerateVision (multimodal) share one decode path.
// It closes resp.Body.
func decodeGenResult(resp *http.Response, start time.Time) (GenResult, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return GenResult{}, &StatusError{StatusCode: resp.StatusCode, Body: truncate(string(b), 300)}
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

// Health reports whether the server answers /health with 200. It probes the
// DEFAULT base on purpose — health is a property of this client's own
// endpoint, not of any per-model seat override (a remote seat is health-checked
// by its dispatcher via its own roster fetch, never through this client).
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
