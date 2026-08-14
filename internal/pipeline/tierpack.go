// tierpack.go — TO-3 (plan 2026-08-07): tier-aware repacking at the
// escalation boundary.
//
// WHY. Run() packs the input ONCE at entry — GCF compaction + a char-budget
// head/tail trim sized by the GLOBAL cfg.MaxInputChars — and every tier in the
// cascade, including the escalation and terminal reasoning tiers, inherited
// that entry cut. Forwarding a small tier's lossy view up the ladder is a
// correctness bug class: the larger tier has a larger served window and could
// have read MORE of the source, but it was never offered more. (The plan also
// names the recursion half of TO-3: escalation must not recurse. That is
// structural here — the chain walk in Run is bounded, the confhead gate
// excludes the escalation tier, and attemptReasoning has no escalation gate —
// there is no "escalate tool" in this codebase to remove; verified by grep,
// recorded in the nightshift-4 notes.)
//
// WHAT. When a request climbs past the entry tier, the CALLEE tier's input is
// re-packed FROM THE ORIGINAL SOURCE against that tier's own budget:
//
//	n_ctx(callee) − MaxTokens(task) − tierReserveTokens − tokenized(scaffold)
//
// with n_ctx probed live from the serving endpoint (the same /props probe the
// agent loop trusts — window.go's rationale applies verbatim) and the token
// counts measured by the callee's OWN served tokenizer (internal/tokclient —
// the house was burned by chars/4 estimates; see ADR 0017 and the
// truncation-needs-context-check lesson). When the original fits, the tier
// sees the WHOLE source. When it does not, the input is cut token-exact,
// head+tail, on piece boundaries backed off to rune boundaries — never
// mid-rune (the LO-13 mojibake class), never by a chars/4 guess.
//
// FAIL-OPEN CONTRACT. Any probe/tokenize/build failure falls back to the
// ENTRY packing — byte-identical to the pre-TO-3 behavior — and the reason is
// recorded per model (sticky for probeTTL, so the escalation path does not pay
// a doomed round-trip per climb) and surfaced in the ledger row's `tier_pack`
// field. Fail-open, never fail-unobservable (the TO-4 review rule).
package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/tokclient"
)

// tierReserveTokens is the fixed headroom reserved on top of the measured
// scaffold and the task's completion budget: chat-template framing (BOS/role
// tokens /tokenize with add_special=false cannot see) plus a small safety
// margin. Deliberately modest — the scaffold and input are MEASURED here, so
// this only has to absorb template framing, not estimator error (contrast the
// agent loop's compactionMargin, which also absorbs chars/4 drift).
const tierReserveTokens = 128

// probeTTL bounds how long a per-model probe result (n_ctx, or a recorded
// failure) is trusted. Serving configs change only on operator edits, but the
// MCP server is a long-lived process — a TTL keeps a restarted llama-swap
// from being mis-budgeted (or mis-degraded) forever.
const probeTTL = 10 * time.Minute

// repackMarker replaces the elided middle of an over-budget source. Same
// shape as contextbudget.Trim's marker, named for this boundary.
const repackMarker = "\n\n[...content elided to fit the escalation tier's context window...]\n\n"

// minRepackTokens is the smallest input allowance worth paying the repack's
// tokenize round-trips for — a floor against degenerate windows. (Preventing
// a shrink below the ENTRY view is the separate tokEntry guard on the cut
// path; this constant does not carry that job.)
const minRepackTokens = 256

// tierPackState is the per-Pipeline cache behind packForTier. All access goes
// through the mutex: escalations are rare and never on the entry hot path, so
// contention is a non-issue.
type tierPackState struct {
	mu     sync.Mutex
	probes map[string]tierProbe
	toks   map[string]*tokclient.Client
	// tokFails caches per-model TOKENIZE failures for probeTTL, mirroring the
	// probe-failure cache: without it every over-window climb re-paid up to
	// two dead Count round-trips (60s timeouts) against a route that already
	// failed — the doomed-round-trip class the file's own contract forbids
	// (round-1 review finding 2026-08-14).
	tokFails map[string]tierProbe // nCtx unused; why + at only
}

type tierProbe struct {
	nCtx int    // probed served window; 0 when the probe failed
	why  string // failure reason when nCtx == 0
	at   time.Time
}

// packForTier returns the input the CALLEE tier should see and a `tier_pack`
// disposition string for the ledger row:
//
//	"full source (under entry cap)"    — nothing was cut at entry; NOTHING was
//	                                     probed or measured (an honest label —
//	                                     claiming "token-exact" here converted
//	                                     "unverified" into a false verification
//	                                     claim; round-1 review finding)
//	"token-exact (full source)"        — measured: the original fits the window
//	"token-exact (cut K/N tokens)"     — token-exact head+tail repack
//	"entry-inherited (<why>)"          — fail-open to the entry packing
//
// orig is the original pre-trim input; entryPacked is what the entry tier saw
// (today's packing — the fail-open result). req carries the task/params so the
// scaffold can be rebuilt exactly as attempt() will build it. genBudget is the
// CALLEE'S REAL completion request — attemptReasoning generates with
// MaxTokens+reasoningThinkBudget, and budgeting against bare MaxTokens
// overshot the served window by ~384 tokens on exactly the large inputs this
// feature exists for (round-1 CRITICAL finding). decorate re-applies any
// prompt decoration (exemplar shots) so the measured prompt is the SHIPPED
// prompt, not a bare build the injection then outgrows.
func (p *Pipeline) packForTier(ctx context.Context, model string, orig, entryPacked string, req core.Request, genBudget int, decorate func(tasks.Built) tasks.Built) (string, string) {
	if model == "" {
		return entryPacked, "entry-inherited (no callee model)"
	}
	if orig == entryPacked {
		// Nothing was cut at entry — the callee's view cannot improve. No
		// probe, no measurement: the label must not claim one.
		return entryPacked, "full source (under entry cap)"
	}

	nCtx, why := p.tierNCtx(ctx, model)
	if nCtx <= 0 {
		return entryPacked, "entry-inherited (" + why + ")"
	}
	if why, failed := p.tokFailFresh(model); failed {
		return entryPacked, "entry-inherited (tokenize (cached): " + why + ")"
	}
	tok := p.tierTok(model)

	// Measure the FULL prompt this task would build from the original —
	// decorated exactly as it will ship — then derive the scaffold cost as
	// full − input. Two Count calls, no scaffold reconstruction, no estimate.
	fullReq := req
	fullReq.Input = orig
	built, err := tasks.Build(fullReq)
	if err != nil {
		return entryPacked, "entry-inherited (build: " + err.Error() + ")"
	}
	if decorate != nil {
		built = decorate(built)
	}
	prompt := built.System + "\n" + built.User
	tokFull, ok := tok.Count(ctx, prompt)
	if !ok {
		return entryPacked, p.noteTokFail(model, tok.LastErr())
	}
	allowance := nCtx - genBudget - tierReserveTokens
	if tokFull <= allowance {
		return orig, "token-exact (full source)"
	}

	tokOrig, ok := tok.Count(ctx, orig)
	if !ok {
		return entryPacked, p.noteTokFail(model, tok.LastErr())
	}
	inputAllowance := allowance - (tokFull - tokOrig)
	if inputAllowance < minRepackTokens {
		return entryPacked, fmt.Sprintf("entry-inherited (degenerate allowance %d)", inputAllowance)
	}
	// The repack must BUY view, never shrink it: a callee served with a small
	// window (big models routinely get smaller n_ctx to fit VRAM) could pass
	// the fixed floor yet see LESS than the entry tier — the exact inversion
	// TO-3 exists to prevent (round-1 review finding, rating 8). Compare
	// against the entry view's own token count, not a constant.
	tokEntry, ok := tok.Count(ctx, entryPacked)
	if !ok {
		return entryPacked, p.noteTokFail(model, tok.LastErr())
	}
	if inputAllowance <= tokEntry {
		return entryPacked, fmt.Sprintf("entry-inherited (callee window buys no view: allowance %d <= entry view %d)", inputAllowance, tokEntry)
	}

	packed, kept, ok := cutTokenExact(ctx, tok, orig, inputAllowance)
	if !ok {
		return entryPacked, p.noteTokFail(model, tok.LastErr())
	}
	// kept under-reports the shipped size by the marker's ~16 tokens plus
	// retokenization drift at the two seams — absorbed by tierReserveTokens,
	// stated here so nobody reads K as exact-to-the-token.
	return packed, fmt.Sprintf("token-exact (cut %d/%d tokens)", kept, tokOrig)
}

// tokFailFresh reports a cached tokenize failure for model, if still inside
// probeTTL.
func (p *Pipeline) tokFailFresh(model string) (string, bool) {
	p.tierPack.mu.Lock()
	defer p.tierPack.mu.Unlock()
	e, ok := p.tierPack.tokFails[model]
	if !ok || p.now().Sub(e.at) >= probeTTL {
		return "", false
	}
	return e.why, true
}

// noteTokFail records a tokenize failure for model (TTL-cached) and returns
// the entry-inherited disposition naming it. Deliberately class-blind (no
// LastFailDefinitive consult, unlike the agent loop's 2-strike sticky): the
// escalation cadence is low, the fallback is the safe entry packing, and a
// transient 503 suppressing repacks for one TTL window is a bounded cost —
// simpler beats a second classifier here.
func (p *Pipeline) noteTokFail(model, why string) string {
	p.tierPack.mu.Lock()
	if p.tierPack.tokFails == nil {
		p.tierPack.tokFails = map[string]tierProbe{}
	}
	p.tierPack.tokFails[model] = tierProbe{why: why, at: p.now()}
	p.tierPack.mu.Unlock()
	return "entry-inherited (tokenize: " + why + ")"
}

// tierNCtx returns the callee's served context window, cached for probeTTL.
// Failures are cached too (with their reason) so a dead /props route costs one
// probe per TTL, not one per escalation.
func (p *Pipeline) tierNCtx(ctx context.Context, model string) (int, string) {
	p.tierPack.mu.Lock()
	if p.tierPack.probes == nil {
		p.tierPack.probes = map[string]tierProbe{}
	}
	e, cached := p.tierPack.probes[model]
	p.tierPack.mu.Unlock()
	now := p.now()
	if cached && now.Sub(e.at) < probeTTL {
		return e.nCtx, e.why
	}
	// ProbeUpstreamWindow is the agent /props probe RESTRICTED to the
	// per-model passthrough: the cascade is multi-model, and the bare-root
	// fallback answers for whatever model is currently loaded — budgeting one
	// tier against another tier's window. It may cold-start the model, which
	// is acceptable for the same reason as the agent probe: the caller is
	// about to run exactly that model. On a bare llama-server (no /upstream)
	// this probe fails and the repack stays entry-inherited — honest, and a
	// single-model server cannot meaningfully repack per-tier anyway.
	n, ok := agent.ProbeUpstreamWindow(ctx, p.cfg.Endpoint, model)
	fresh := tierProbe{nCtx: n, at: now}
	if !ok {
		fresh.nCtx = 0
		fresh.why = "no /props answer from the serving endpoint"
	}
	p.tierPack.mu.Lock()
	p.tierPack.probes[model] = fresh
	p.tierPack.mu.Unlock()
	return fresh.nCtx, fresh.why
}

// tierTok returns (building once) the callee's tokenizer client.
func (p *Pipeline) tierTok(model string) *tokclient.Client {
	p.tierPack.mu.Lock()
	defer p.tierPack.mu.Unlock()
	if p.tierPack.toks == nil {
		p.tierPack.toks = map[string]*tokclient.Client{}
	}
	if c, ok := p.tierPack.toks[model]; ok {
		return c
	}
	// Upstream-only for the same reason as the window probe: the root
	// /tokenize answers with the CURRENTLY LOADED model's tokenizer, which
	// mid-cascade is the previous tier's — counts and cuts would be computed
	// with the wrong vocabulary and cached under this model's key.
	c := tokclient.NewUpstreamOnly(p.cfg.Endpoint, model, 0)
	p.tierPack.toks[model] = c
	return c
}

// cutTokenExact keeps the first 2/3 and last 1/3 of allowance REAL tokens of
// text (mirroring contextbudget.Trim's head-biased split), cutting on piece
// boundaries backed off to rune boundaries, and joins the halves with
// repackMarker. Returns (packed, keptTokens, ok); ok=false when the tokenizer
// could not answer or its pieces do not reconstruct the text (tokclient
// verifies reconstruction itself — a false here is always accompanied by a
// recorded LastErr).
func cutTokenExact(ctx context.Context, tok *tokclient.Client, text string, allowance int) (string, int, bool) {
	lens, ok := tok.Pieces(ctx, text)
	if !ok {
		return "", 0, false
	}
	if allowance >= len(lens) {
		return text, len(lens), true // defensive: caller already checked fit
	}
	headTok := allowance * 2 / 3
	tailTok := allowance - headTok

	// Head: bytes of the first headTok pieces.
	headBytes := 0
	for i := 0; i < headTok; i++ {
		headBytes += lens[i]
	}
	// Tail: bytes of the last tailTok pieces.
	tailBytes := 0
	for i := len(lens) - tailTok; i < len(lens); i++ {
		tailBytes += lens[i]
	}

	head := text[:headBytes]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1] // back off a piece-split rune (byte-fallback tokens)
	}
	tail := text[len(text)-tailBytes:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return head + repackMarker + tail, headTok + tailTok, true
}

// now is the pipeline's injectable clock (nowFn is used by the LO-9 cold-swap
// tracking; reuse it so tests can control the probe TTL too).
func (p *Pipeline) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}
