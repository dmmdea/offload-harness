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
	"net/url"
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
//
// It decides who receives the key, so it matches the parsed HOST exactly: https,
// no userinfo, the default port, and a host that IS api.nvidia.com or a DNS
// subdomain of it (integrate.api.nvidia.com, ai.api.nvidia.com), compared
// case-insensitively with a trailing root dot dropped. Until 0.143.1 it was a
// substring test on the whole URL, so a caller-supplied base such as
// https://attacker.example/integrate.api/v1 or
// https://api.nvidia.com.attacker.example/v1 — one prompt-injected offload_nim
// call — received NVIDIA_API_KEY as a Bearer token.
func IsHostedNVIDIA(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == nvidiaAPIHost || strings.HasSuffix(host, "."+nvidiaAPIHost)
}

// nvidiaAPIHost is the DNS zone of NVIDIA's hosted API; only it and its
// subdomains receive the key.
const nvidiaAPIHost = "api.nvidia.com"

// BaseAllowed reports whether a caller-named base is one offload_nim may
// target (security standard L5, register S-30): NVIDIA's hosted API
// (IsHostedNVIDIA), the configured endpoint, or an entry of the operator's
// extra list. A base matches an entry when scheme, host (case-insensitive,
// trailing root dot dropped) and port are equal — the default port made
// explicit — and the entry's path is a prefix of the base's path on a segment
// boundary. why names the rule that matched, or the reason nothing did.
func BaseAllowed(base, configured string, extra []string) (ok bool, why string) {
	if IsHostedNVIDIA(base) {
		return true, "NVIDIA hosted API"
	}
	b, err := parseBase(base)
	if err != nil {
		return false, "unparseable base: " + err.Error()
	}
	if configured != "" {
		if c, cerr := parseBase(configured); cerr == nil && baseCovers(c, b) {
			return true, "nim_endpoint"
		}
	}
	for _, e := range extra {
		if c, cerr := parseBase(e); cerr == nil && baseCovers(c, b) {
			return true, "nim_bases entry " + e
		}
	}
	return false, "not NVIDIA's hosted API, nim_endpoint or a nim_bases entry"
}

type nimBase struct{ scheme, host, port, path string }

func parseBase(raw string) (nimBase, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nimBase{}, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nimBase{}, fmt.Errorf("scheme %q is not http(s)", u.Scheme)
	}
	if u.User != nil {
		return nimBase{}, fmt.Errorf("a base with userinfo is never allowlisted")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nimBase{}, fmt.Errorf("no host")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	return nimBase{scheme: strings.ToLower(u.Scheme), host: host, port: port, path: strings.TrimRight(u.EscapedPath(), "/")}, nil
}

// baseCovers: same scheme, host and port, and the entry's path is a prefix of
// the base's path on a segment boundary ("/v1" covers "/v1" and "/v1/x", never
// "/v1x").
func baseCovers(entry, b nimBase) bool {
	if entry.scheme != b.scheme || entry.host != b.host || entry.port != b.port {
		return false
	}
	if entry.path == "" || entry.path == b.path {
		return true
	}
	return strings.HasPrefix(b.path, entry.path+"/")
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
