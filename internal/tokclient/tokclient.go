// Package tokclient is the harness's REAL-tokenizer path: it asks the serving
// endpoint's own /tokenize route how a text actually tokenizes, instead of
// estimating with chars/4. The house has been burned by estimates before —
// three real transcripts were rejected with exceed_context_size while the
// chars/4 ladder declined to compact (ADR 0017), and a truncation
// misdiagnosis cost a full debugging session (truncation-needs-context-check,
// 2026-08-08). Estimates stay acceptable for BUDGET headroom; any decision
// that CUTS content at an exact boundary must use this package.
//
// Endpoint shape mirrors internal/agent's /props probe (window.go): llama.cpp
// serves POST /tokenize at the server root, and llama-swap proxies it
// per model under /upstream/{model}/tokenize (which may cold-start the model —
// acceptable for the same reason as the window probe: the caller is about to
// use exactly that model). A trailing /v1 on base is stripped via the one
// endpoint-normalization rule in internal/swapclient.
//
// FAIL-OPEN CONTRACT: every method returns ok=false on any failure (transport,
// non-200, malformed payload, a piece accounting that does not reconstruct the
// input). Callers fall back to their estimate-driven path and MUST NOT fail the
// run because tokenization was unanswerable — a generic OpenAI endpoint has no
// /tokenize and that is fine.
package tokclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// DefaultTimeout bounds one /tokenize round-trip. Warm, tokenizing tens of KB
// is milliseconds; the generous bound exists because /upstream/{model}/ may
// cold-start the model first (same rationale as the window probe's 60s).
const DefaultTimeout = 60 * time.Second

// Client tokenizes text with one served model's own tokenizer.
type Client struct {
	base  string // normalized server root ("" = unusable; every call fails open)
	model string
	http  *http.Client
}

// New builds a client for the model served at base (any /v1 suffix is
// stripped). timeout <= 0 uses DefaultTimeout.
func New(base, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{base: swapclient.BaseURL(base), model: model, http: &http.Client{Timeout: timeout}}
}

// tokenizeReq is llama.cpp's /tokenize request body. add_special is always
// false here: callers count/cut CONTENT tokens and account for template
// framing separately.
type tokenizeReq struct {
	Content    string `json:"content"`
	AddSpecial bool   `json:"add_special"`
	WithPieces bool   `json:"with_pieces,omitempty"`
}

// piece decodes one entry of a with_pieces response. llama.cpp returns the
// piece as a STRING when it is valid UTF-8 and as an ARRAY OF BYTES when it is
// not (byte-fallback tokens mid-rune) — both shapes carry the byte length,
// which is all the cut needs.
type piece struct {
	Piece json.RawMessage `json:"piece"`
}

// byteLen returns the byte length of the piece under either wire shape.
func (p piece) byteLen() (int, bool) {
	var s string
	if err := json.Unmarshal(p.Piece, &s); err == nil {
		return len(s), true
	}
	var b []int
	if err := json.Unmarshal(p.Piece, &b); err == nil {
		return len(b), true
	}
	return 0, false
}

// Pieces tokenizes text and returns the BYTE LENGTH of every token's piece, in
// token order. sum(lengths) == len(text) is verified here — if the pieces do
// not reconstruct the input exactly, the byte→token mapping a caller would
// build from them is unreliable, so the whole call fails open instead of
// handing back a mapping that could cut mid-message.
func (c *Client) Pieces(ctx context.Context, text string) ([]int, bool) {
	body, ok := c.post(ctx, tokenizeReq{Content: text, AddSpecial: false, WithPieces: true})
	if !ok {
		return nil, false
	}
	var payload struct {
		Tokens []piece `json:"tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	lens := make([]int, 0, len(payload.Tokens))
	total := 0
	for _, p := range payload.Tokens {
		n, ok := p.byteLen()
		if !ok {
			return nil, false
		}
		lens = append(lens, n)
		total += n
	}
	if total != len(text) {
		return nil, false // pieces do not reconstruct the input — mapping unusable
	}
	return lens, true
}

// Count returns how many tokens text costs under the served model's tokenizer
// (content tokens only — add_special false). The light call for budget
// arithmetic: no pieces on the wire.
func (c *Client) Count(ctx context.Context, text string) (int, bool) {
	body, ok := c.post(ctx, tokenizeReq{Content: text, AddSpecial: false})
	if !ok {
		return 0, false
	}
	var payload struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	return len(payload.Tokens), true
}

// post tries the llama-swap per-model passthrough first, then a bare
// llama-server root — the same candidate order as the window probe.
func (c *Client) post(ctx context.Context, req tokenizeReq) ([]byte, bool) {
	if c.base == "" {
		return nil, false
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return nil, false
	}
	candidates := []string{
		c.base + "/upstream/" + url.PathEscape(c.model) + "/tokenize",
		c.base + "/tokenize",
	}
	for _, u := range candidates {
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
		if err != nil {
			continue
		}
		hr.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(hr)
		if err != nil {
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if rerr != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		return body, true
	}
	return nil, false
}
