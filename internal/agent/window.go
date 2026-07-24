// window.go — served-context-window discovery. The compaction budget is only
// as honest as the window it targets: two real runs died with
// `exceed_context_size` 400s because `--ctx-tokens` assumed 16384 while the
// serving tier ran `-c 8192` — the budget never engaged before the server
// refused (flip-decision report 2026-07-24, finding F4). The fix is to ASK the
// endpoint instead of assuming: llama.cpp's `/props` reports the live n_ctx,
// and llama-swap proxies it per model under `/upstream/{model}/`.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FallbackContextTokens is the window assumed when the probe cannot answer —
// the CONSERVATIVE choice (the smallest window any of our serving tiers runs),
// matching the loop's own built-in default: a too-small assumption wastes some
// budget headroom; a too-large one kills runs with server 400s.
const FallbackContextTokens = 8192

// ProbeServedWindow asks the serving endpoint for model's live context window
// (n_ctx). It tries, in order:
//
//  1. {base}/upstream/{model}/props — llama-swap's per-model passthrough (the
//     production topology; may cold-start the model, which is acceptable: the
//     caller is about to use exactly that model);
//  2. {base}/props — a bare llama-server.
//
// A trailing /v1 on base is stripped first (props lives at the server root).
// Returns (n_ctx, true) on success; (0, false) on any failure — callers fall
// back, never fail, on an unanswerable probe (a generic OpenAI endpoint has no
// /props and that is fine).
func ProbeServedWindow(ctx context.Context, base, model string) (int, bool) {
	b := strings.TrimRight(base, "/")
	b = strings.TrimSuffix(b, "/v1")
	if b == "" {
		return 0, false
	}
	candidates := []string{b + "/upstream/" + url.PathEscape(model) + "/props", b + "/props"}
	client := &http.Client{Timeout: 60 * time.Second} // cold model swap can take tens of seconds
	for _, u := range candidates {
		if n, ok := fetchNCtx(ctx, client, u); ok {
			return n, true
		}
	}
	return 0, false
}

// fetchNCtx GETs a /props URL and extracts default_generation_settings.n_ctx.
func fetchNCtx(ctx context.Context, client *http.Client, u string) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false
	}
	var payload struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	if n := payload.DefaultGenerationSettings.NCtx; n > 0 {
		return n, true
	}
	return 0, false
}

// ResolveContextTokens turns the --ctx-tokens knob + a probe result into the
// window the loop budgets against, with one honest rule per case:
//
//   - flag <= 0 (auto, the default): the probed window when the probe answers,
//     else FallbackContextTokens — never a hardcoded per-tier assumption;
//   - flag > 0 (operator override): the flag wins, but when the probe answered
//     with LESS than the flag a warning names the gap — that exact mismatch
//     (assumed 16384, served 8192) killed real runs before it was measured.
//
// The returned note is "" or a human-readable line for stderr; this function
// stays pure (no logging) so every drive mode reports identically.
func ResolveContextTokens(flag, probed int, probeOK bool) (int, string) {
	if flag <= 0 {
		if probeOK {
			return probed, fmt.Sprintf("context window: %d (probed from the serving endpoint)", probed)
		}
		return FallbackContextTokens, fmt.Sprintf("context window: %d (probe unanswered — conservative fallback; set --ctx-tokens to override)", FallbackContextTokens)
	}
	if probeOK && probed < flag {
		return flag, fmt.Sprintf("WARNING: --ctx-tokens %d exceeds the SERVED window %d — requests may be rejected with exceed_context_size; drop the flag to auto-probe", flag, probed)
	}
	return flag, ""
}
