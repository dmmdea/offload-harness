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
	"time"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// FallbackContextTokens is the window assumed when the probe cannot answer —
// the CONSERVATIVE choice (the smallest window any of our serving tiers runs),
// matching the loop's own built-in default: a too-small assumption wastes some
// budget headroom; a too-large one kills runs with server 400s.
const FallbackContextTokens = 8192

// coldStartWait bounds the per-model passthrough probes TOGETHER. llama-swap
// holds a request for a model that is not loaded until that model's health
// check passes, so these probes are also the wait for a cold start — and a vLLM
// seat's cold start is minutes. Measured 2026-09-16 on the Qube agent-pool seat:
// `starting` at 3 s, `ready` at 222 s. The old 60 s per-request timeout gave up
// at 60 s on /props and again at 120 s on /v1/models, the bare-root /props
// answered 404, and the run budgeted 8,192 against a 114,688-token window. Ten
// minutes is llama-swap's healthCheckTimeout on the reference boxes (600): past
// it, llama-swap has abandoned the load itself. A var so tests can shrink it.
var coldStartWait = 10 * time.Minute

// probeRequestTimeout bounds the bare-root /props probe, which never loads a
// model: it answers for whatever is already running, or not at all.
var probeRequestTimeout = 60 * time.Second

// ProbeServedWindow asks the serving endpoint for model's live context window
// (n_ctx). It tries, in order:
//
//  1. {base}/upstream/{model}/props — llama-swap's per-model passthrough (the
//     production topology; may cold-start the model, which is acceptable: the
//     caller is about to use exactly that model — EXCEPT under a GPU-lease
//     fence, where modelaffinity.AwaitUpstream holds the request until the
//     card frees or the caller's deadline ends, and it is never sent onto a
//     card a render or an exclusive hold owns);
//  2. {base}/props — a bare llama-server.
//
// A trailing /v1 on base is stripped first (props lives at the server root), via
// the one endpoint-normalization rule in internal/swapclient.
// Returns (n_ctx, true) on success; (0, false) on any failure — callers fall
// back, never fail, on an unanswerable probe (a generic OpenAI endpoint has no
// /props and that is fine).
//
// This is the ONE llama-swap call the harness does not route through
// pkg/llamaswap, and the reason is the auto-start above: [llamaswap.Client.Props]
// checks /running first and returns ErrNotLoaded rather than triggering a
// multi-GB load — the right default for an operator probe, and exactly wrong
// here. The planner seat is normally COLD at this point, so a refusing probe
// would silently drop every cold run back to FallbackContextTokens and re-open
// finding F4. Alias resolution, the package's other reason to exist, buys nothing
// here: llama-swap resolves aliases on /upstream itself (verified live: both
// /upstream/embeddinggemma/props and /upstream/local-embed/props answer 200).
func ProbeServedWindow(ctx context.Context, base, model string) (int, bool) {
	n, ok, _ := probeWindow(ctx, base, model, false, true)
	return n, ok
}

// ProbeServedWindowChecked is ProbeServedWindow that also says WHY it did not
// answer when the reason is the GPU-lease fence (2026-09-22): a render, or an
// exclusive hold, owns the card and the seat is not resident, so the probe —
// whose route would LOAD the seat — was never sent. err is then the fence's
// *modelaffinity.LeaseError (modelaffinity.IsLeaseRefusal), and nil on every
// other outcome, which keeps the fail-open contract for a broken endpoint. The
// run launchers use it to defer `capacity` instead of starting a run whose
// every request would wait behind the same fence.
//
// The fence waits for the card inside ctx's deadline (and coldStartWait), so a
// render that ends within the caller's admission budget costs a wait, not a
// defer.
func ProbeServedWindowChecked(ctx context.Context, base, model string) (int, bool, error) {
	return probeWindow(ctx, base, model, false, true)
}

// ProbeUpstreamWindow is ProbeServedWindow restricted to the llama-swap
// per-model passthrough — no bare-root fallback. The root /props answers for
// WHATEVER model is currently loaded, so a multi-model caller (the cascade's
// TO-3 repack) falling through to it would budget one tier against another
// tier's window (review finding 2026-08-14). Single-model callers keep
// ProbeServedWindow's fallback.
func ProbeUpstreamWindow(ctx context.Context, base, model string) (int, bool) {
	n, ok, _ := probeWindow(ctx, base, model, true, true)
	return n, ok
}

// ProbeUpstreamWindowNow is ProbeUpstreamWindow that does NOT wait behind a
// GPU-lease fence (2026-09-22): under a render or an exclusive hold over a model
// that is not resident it returns at once with the fence's *LeaseError, and a
// cold start is still absorbed when nothing fences the card. For a caller whose
// NEXT request is a generation that waits at modelaffinity.Admit anyway (the
// cascade's per-tier re-pack): waiting here too would spend the caller's time
// twice, and a fenced answer must not be cached as "this tier has no window".
func ProbeUpstreamWindowNow(ctx context.Context, base, model string) (int, bool, error) {
	return probeWindow(ctx, base, model, true, false)
}

func probeWindow(ctx context.Context, base, model string, upstreamOnly, waitFence bool) (int, bool, error) {
	b := swapclient.BaseURL(base)
	if b == "" {
		return 0, false, nil
	}
	// Per-model passthrough first, in two shapes: llama-server's /props (n_ctx),
	// then the backend's own /v1/models — a vLLM seat behind llama-swap has NO
	// /props (404) but reports max_model_len there, and before 0.113.14 every run
	// on such a seat silently budgeted FallbackContextTokens (8,192) against a
	// 163,840-token window. llama-server's /v1/models carries no max_model_len,
	// so the order is safe: the second probe answers only where the first cannot.
	upstream := []struct {
		path  string
		fetch func(context.Context, *http.Client, string) (int, bool)
	}{
		{"/props", fetchNCtx},
		{"/v1/models", func(ctx context.Context, c *http.Client, u string) (int, bool) {
			return fetchMaxModelLen(ctx, c, u, model)
		}},
	}
	// One budget for both passthrough probes, carried by context rather than a
	// per-request client timeout: whichever probe lands on the cold seat absorbs
	// the load, and the other then answers from a loaded seat in milliseconds.
	uctx, cancel := context.WithTimeout(ctx, coldStartWait)
	defer cancel()
	deadline, _ := uctx.Deadline()
	if !waitFence {
		deadline = time.Now()
	}
	coldClient := &http.Client{}
	for _, c := range upstream {
		// EVERY request passes the GPU-lease fence, not the probe once: a render
		// can take the card while the first request is absorbing a cold start,
		// and the second would then start the seat again on top of it.
		u, ferr := modelaffinity.AwaitUpstream(uctx, base, model, c.path, deadline)
		if ferr != nil {
			if modelaffinity.IsLeaseRefusal(ferr) {
				// No bare-root fallback under a fence: the root answers for
				// whatever is loaded, and the honest answer is "the card is held".
				return 0, false, ferr
			}
			return 0, false, nil
		}
		if n, ok := c.fetch(uctx, coldClient, u); ok {
			return n, true, nil
		}
	}
	if upstreamOnly {
		return 0, false, nil
	}
	n, ok := fetchNCtx(ctx, &http.Client{Timeout: probeRequestTimeout}, b+"/props")
	return n, ok, nil
}

// fetchMaxModelLen GETs a per-model /v1/models URL and extracts the served
// window from data[].max_model_len (vLLM's field; absent on llama-server).
// The entry whose id equals model wins; otherwise the list must agree on ONE
// positive value (vLLM lists every --served-model-name alias with the same
// window) — a list that disagrees answers nothing rather than guessing.
func fetchMaxModelLen(ctx context.Context, client *http.Client, u, model string) (int, bool) {
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
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	agreed := 0
	for _, d := range payload.Data {
		if d.MaxModelLen <= 0 {
			continue
		}
		if d.ID == model {
			return d.MaxModelLen, true
		}
		switch {
		case agreed == 0:
			agreed = d.MaxModelLen
		case agreed != d.MaxModelLen:
			return 0, false
		}
	}
	return agreed, agreed > 0
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
//   - flag <= 0 (auto, the default): the probed window when the probe answers;
//     when the probe FAILS, the seat's CONFIGURED window (config
//     agent_ctx_tokens) when one is known, and FallbackContextTokens only when
//     nothing better is known — never a hardcoded per-tier assumption. The probe
//     now waits out a cold start (coldStartWait), so the configured window is
//     reached only when the endpoint cannot answer inside the caller's budget;
//     before both fixes a cold seat silently ran a 114,688-token window at 8,192
//     for the WHOLE task (the same agent_run measured 8,192 cold and 114,688
//     warm, minutes apart). When the probe answers, the SERVED window wins over
//     the configured one — live beats written — and a disagreement is named in
//     the note, because a stale config is exactly what would mislead the
//     fallback path the next time the probe fails;
//   - flag > 0 (operator override): the flag wins, but when the probe answered
//     with LESS than the flag a warning names the gap — that exact mismatch
//     (assumed 16384, served 8192) killed real runs before it was measured.
//
// The returned note is "" or a human-readable line for stderr; this function
// stays pure (no logging) so every drive mode reports identically.
func ResolveContextTokens(flag, probed, configured int, probeOK bool) (int, string) {
	if flag <= 0 {
		if probeOK {
			if configured > 0 && configured != probed {
				return probed, fmt.Sprintf("context window: %d (probed from the serving endpoint; config agent_ctx_tokens %d disagrees — the served window wins, correct the config)", probed, configured)
			}
			return probed, fmt.Sprintf("context window: %d (probed from the serving endpoint)", probed)
		}
		if configured > 0 {
			return configured, fmt.Sprintf("context window: %d (probe unanswered — using the seat's configured window; set --ctx-tokens to override)", configured)
		}
		return FallbackContextTokens, fmt.Sprintf("context window: %d (probe unanswered — conservative fallback; set --ctx-tokens to override)", FallbackContextTokens)
	}
	if probeOK && probed < flag {
		return flag, fmt.Sprintf("WARNING: --ctx-tokens %d exceeds the SERVED window %d — requests may be rejected with exceed_context_size; drop the flag to auto-probe", flag, probed)
	}
	return flag, ""
}
