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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
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
	// lastErr remembers WHY the most recent call failed. Fail-open callers
	// discard the per-call error by contract, so without this a permanently
	// downgraded tokenizer is undiagnosable — the operator could not even say
	// whether the route 404'd, timed out, or answered garbage. atomic: one
	// Client may be shared across --serve handlers.
	lastErr atomic.Value // string
	// lastDefinitive marks the most recent failure as DEFINITIVE route absence
	// (every candidate answered HTTP 404/405): the endpoint positively said the
	// route does not exist, so retrying cannot help. Transient classes —
	// timeouts, resets, 5xx during a model swap, malformed bodies — are NOT
	// definitive; the sticky wrapper gives those a second chance before
	// downgrading (review finding 2026-08-14: one cold-start timeout or one
	// 503 mid-swap must not degrade a healthy endpoint for the process life).
	lastDefinitive atomic.Bool
	// upstreamOnly restricts post() to the per-model passthrough route (see
	// NewUpstreamOnly).
	upstreamOnly bool
}

// LastFailDefinitive reports whether the most recent failure was a definitive
// route absence (all candidates 404/405) rather than a transient failure.
func (c *Client) LastFailDefinitive() bool { return c.lastDefinitive.Load() }

// LastErr reports why the most recent failing call failed ("" = none yet).
// Consulted by the agent loop's sticky wrapper at downgrade time so the
// degradation is reportable, not just observable-by-absence.
func (c *Client) LastErr() string {
	s, _ := c.lastErr.Load().(string)
	return s
}

func (c *Client) fail(format string, args ...any) {
	c.lastDefinitive.Store(false) // only post()'s all-404/405 branch marks definitive, after this
	c.lastErr.Store(fmt.Sprintf(format, args...))
}

// New builds a client for the model served at base (any /v1 suffix is
// stripped). timeout <= 0 uses DefaultTimeout.
func New(base, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{base: swapclient.BaseURL(base), model: model, http: &http.Client{Timeout: timeout}}
}

// NewUpstreamOnly builds a client that tries ONLY the llama-swap per-model
// passthrough — never the bare-server root fallback. The root route answers
// for WHATEVER model is currently loaded, so in a multi-model process (the
// cascade) a stale per-model alias falling through to the root would count
// and cut with the PREVIOUS tier's tokenizer — silently, cached under the
// callee's key (TO-3 round-1 review finding 2026-08-14: the real cross-model
// contamination channel). Single-model agent paths keep New's fallback.
func NewUpstreamOnly(base, model string, timeout time.Duration) *Client {
	c := New(base, model, timeout)
	c.upstreamOnly = true
	return c
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
		Tokens *[]piece `json:"tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		c.fail("tokenize response is not the expected JSON shape: %v", err)
		return nil, false
	}
	if payload.Tokens == nil {
		c.fail("tokenize 200 response carries no tokens array")
		return nil, false
	}
	lens := make([]int, 0, len(*payload.Tokens))
	total := 0
	for _, p := range *payload.Tokens {
		n, ok := p.byteLen()
		if !ok {
			c.fail("tokenize piece is neither a string nor a byte array")
			return nil, false
		}
		lens = append(lens, n)
		total += n
	}
	if total != len(text) {
		// The mapping a caller would build from these pieces is unusable. A
		// too-large response truncated by the read cap lands here too.
		c.fail("tokenize pieces sum to %d bytes for a %d-byte input — mapping unreliable (oversized/truncated response, or a non-reconstructing tokenizer)", total, len(text))
		return nil, false
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
		// A pointer distinguishes "tokens present and empty" from "no tokens
		// key at all": a 200 JSON body WITHOUT the array (a proxy's error
		// object, say) must fail, not read as a confident zero-token success.
		Tokens *[]json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		c.fail("tokenize response is not the expected JSON shape: %v", err)
		return 0, false
	}
	if payload.Tokens == nil {
		c.fail("tokenize 200 response carries no tokens array")
		return 0, false
	}
	return len(*payload.Tokens), true
}

// post tries the llama-swap per-model passthrough first, then a bare
// llama-server root — the same candidate order as the window probe.
func (c *Client) post(ctx context.Context, req tokenizeReq) ([]byte, bool) {
	if c.base == "" {
		c.fail("no endpoint base configured")
		return nil, false
	}
	buf, err := json.Marshal(req)
	if err != nil {
		c.fail("marshaling tokenize request: %v", err)
		return nil, false
	}
	// Read cap SCALED to the input: a flat cap silently truncates exactly the
	// large-transcript with_pieces responses the feature exists for — truncated
	// JSON then reads as "not a tokenize response" and helps trip the sticky
	// downgrade with a reason blaming the server (review finding 2026-08-14).
	// The bound must hold in the WORST legitimate regime, ~1 token/byte
	// (CJK/base64/byte-fallback): one wire entry `{"id":N,"piece":"a"},` runs
	// ≤ ~27 bytes and token count never exceeds the content's byte count, so
	// 32× input + 1 MiB is a true ceiling (round-3 finding: 16× truncated
	// 1-byte/token responses and re-opened the self-disable it claimed to
	// close). A hostile/looping server is still bounded.
	limit := int64(len(req.Content))*32 + (1 << 20)
	candidates := []string{
		c.base + "/upstream/" + url.PathEscape(c.model) + "/tokenize",
		c.base + "/tokenize",
	}
	if c.upstreamOnly {
		candidates = candidates[:1]
	}
	// Per-candidate failures are collected, not swallowed: when BOTH routes
	// fail the recorded reason names each one, so a permanent downgrade is
	// diagnosable after the fact (which route 404'd vs timed out).
	// definitive stays true only while every observed failure is an HTTP
	// 404/405 — a positive "this route does not exist" from the server.
	var reasons []string
	definitive := true
	for _, u := range candidates {
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", u, err))
			definitive = false
			continue
		}
		hr.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(hr)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", u, err))
			definitive = false
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, limit))
		resp.Body.Close()
		if rerr != nil {
			reasons = append(reasons, fmt.Sprintf("%s: reading body: %v", u, rerr))
			definitive = false
			continue
		}
		if resp.StatusCode != http.StatusOK {
			reasons = append(reasons, fmt.Sprintf("%s: HTTP %d", u, resp.StatusCode))
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
				definitive = false
			}
			continue
		}
		// Shape sniff BEFORE accepting: a 200 whose body is not a tokenize
		// response (a proxy's HTML error page, a gateway interposing) must be
		// treated as THIS candidate failing — with the URL in the reason — and
		// must not mask the fallback route, which might answer correctly
		// (review finding 2026-08-14: the ambiguous case lost route attribution
		// AND dead-coded the second candidate).
		var probe struct {
			Tokens json.RawMessage `json:"tokens"`
		}
		if json.Unmarshal(body, &probe) != nil || probe.Tokens == nil {
			reasons = append(reasons, fmt.Sprintf("%s: 200 but not a tokenize response (no tokens array)", u))
			definitive = false
			continue
		}
		return body, true
	}
	c.fail("no /tokenize route answered: %s", strings.Join(reasons, "; "))
	c.lastDefinitive.Store(definitive)
	return nil, false
}
