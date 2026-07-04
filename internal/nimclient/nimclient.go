// Package nimclient calls an OpenAI-compatible NVIDIA NIM endpoint — either
// NVIDIA's hosted build.nvidia.com API (https://integrate.api.nvidia.com/v1,
// authenticated with an nvapi- key) or a self-hosted NIM container
// (http://host:8000/v1, no key). It is the harness's EXPLICIT remote-model tool:
// the local Gemma cascade and its sacred GBNF grammar path are untouched, and
// NIM calls never enter the savings ledger (they are deliberate experiments /
// escalations, not defer-avoidance). Pure net/http; no SDK.
package nimclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// APIKeyFromEnv reads the NIM key from env ONLY (never from a config file, so a
// secret never lands in a tracked file): NVIDIA_API_KEY first, then NGC_API_KEY
// (the name NVIDIA's own NIM docs use). Empty = no key (a self-hosted NIM is
// keyless). The single source of truth for both the CLI and MCP paths.
func APIKeyFromEnv() string {
	if k := strings.TrimSpace(os.Getenv("NVIDIA_API_KEY")); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv("NGC_API_KEY"))
}

// IsHostedNVIDIA reports whether base targets NVIDIA's hosted API (which requires
// a key), as opposed to a self-hosted NIM container (which is keyless).
func IsHostedNVIDIA(base string) bool {
	return strings.Contains(base, "api.nvidia.com") || strings.Contains(base, "integrate.api")
}

// KeyForBase returns the API key to transmit to base: the env key for NVIDIA's
// hosted API, and "" for ANY other base. This is a security boundary, not just a
// convenience — the NVIDIA key is only valid on NVIDIA's endpoints, so silently
// sending it to a user-supplied --base (a typo, a self-hosted NIM, or a third
// party) would leak the secret over the wire for no benefit. A self-hosted NIM is
// keyless by design; both the CLI and MCP paths resolve the key through here.
func KeyForBase(base string) string {
	if IsHostedNVIDIA(base) {
		return APIKeyFromEnv()
	}
	return ""
}

// Client targets one OpenAI-compatible base (".../v1"). apiKey may be empty for a
// keyless self-hosted NIM; when set it is sent as a Bearer token.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// New builds a client. base should include the API version segment (e.g.
// "https://integrate.api.nvidia.com/v1"). A trailing slash is trimmed.
func New(base, apiKey string, timeout time.Duration) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: timeout},
	}
}

// ChatResult holds the model output and per-call telemetry. ReasoningContent is
// populated for reasoning models that emit a separate reasoning_content field;
// Content is the user-facing answer (which can be empty if the response was cut
// at max_tokens mid-thought — Truncated then signals it).
type ChatResult struct {
	Content          string
	ReasoningContent string
	Model            string
	TokensIn         int
	TokensOut        int
	Truncated        bool
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Model       string    `json:"model"`
	Messages    []chatMsg `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Chat sends system (optional) + user to model and returns the answer + telemetry.
func (c *Client) Chat(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (ChatResult, error) {
	body := chatReq{
		Model:       model,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Stream:      false,
		Messages:    []chatMsg{},
	}
	if system != "" {
		body.Messages = append(body.Messages, chatMsg{Role: "system", Content: system})
	}
	body.Messages = append(body.Messages, chatMsg{Role: "user", Content: user})

	buf, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return ChatResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return ChatResult{}, fmt.Errorf("NIM %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var cr chatResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return ChatResult{}, err
	}
	if len(cr.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("NIM returned no choices")
	}
	ch := cr.Choices[0]
	return ChatResult{
		Content:          ch.Message.Content,
		ReasoningContent: ch.Message.ReasoningContent,
		Model:            cr.Model,
		TokensIn:         cr.Usage.PromptTokens,
		TokensOut:        cr.Usage.CompletionTokens,
		Truncated:        ch.FinishReason == "length",
	}, nil
}

type modelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels returns the sorted model ids the endpoint advertises at /models.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return nil, fmt.Errorf("NIM %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var mr modelsResp
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(mr.Data))
	for _, m := range mr.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
