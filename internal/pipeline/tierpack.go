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

// minRepackTokens is the smallest input allowance worth re-packing for. Below
// this the entry packing is at least as informative, and a degenerate cut
// could shrink the escalated view BELOW the entry view.
const minRepackTokens = 256

// tierPackState is the per-Pipeline cache behind packForTier. All access goes
// through the mutex: escalations are rare and never on the entry hot path, so
// contention is a non-issue.
type tierPackState struct {
	mu     sync.Mutex
	probes map[string]tierProbe
	toks   map[string]*tokclient.Client
}

type tierProbe struct {
	nCtx int    // probed served window; 0 when the probe failed
	why  string // failure reason when nCtx == 0
	at   time.Time
}

// packForTier returns the input the CALLEE tier should see and a `tier_pack`
// disposition string for the ledger row:
//
//	"token-exact (full source)"        — the original fits the callee's window
//	"token-exact (cut K/N tokens)"     — token-exact head+tail repack
//	"entry-inherited (<why>)"          — fail-open to the entry packing
//
// orig is the original pre-trim input; entryPacked is what the entry tier saw
// (today's packing — the fail-open result). req carries the task/params so the
// scaffold can be rebuilt exactly as attempt() will build it.
func (p *Pipeline) packForTier(ctx context.Context, model string, orig, entryPacked string, req core.Request) (string, string) {
	if model == "" {
		return entryPacked, "entry-inherited (no callee model)"
	}
	if orig == entryPacked {
		// Nothing was cut at entry — the callee's view cannot improve.
		return entryPacked, "token-exact (full source)"
	}

	nCtx, why := p.tierNCtx(ctx, model)
	if nCtx <= 0 {
		return entryPacked, "entry-inherited (" + why + ")"
	}
	tok := p.tierTok(model)

	// Measure the FULL prompt this task would build from the original, then
	// derive the scaffold cost as full − input. Two Count calls, no scaffold
	// reconstruction, no estimate.
	fullReq := req
	fullReq.Input = orig
	built, err := tasks.Build(fullReq)
	if err != nil {
		return entryPacked, "entry-inherited (build: " + err.Error() + ")"
	}
	prompt := built.System + "\n" + built.User
	tokFull, ok := tok.Count(ctx, prompt)
	if !ok {
		return entryPacked, "entry-inherited (tokenize: " + tok.LastErr() + ")"
	}
	allowance := nCtx - built.MaxTokens - tierReserveTokens
	if tokFull <= allowance {
		return orig, "token-exact (full source)"
	}

	tokOrig, ok := tok.Count(ctx, orig)
	if !ok {
		return entryPacked, "entry-inherited (tokenize: " + tok.LastErr() + ")"
	}
	inputAllowance := allowance - (tokFull - tokOrig)
	if inputAllowance < minRepackTokens {
		return entryPacked, fmt.Sprintf("entry-inherited (degenerate allowance %d)", inputAllowance)
	}

	packed, kept, ok := cutTokenExact(ctx, tok, orig, inputAllowance)
	if !ok {
		return entryPacked, "entry-inherited (cut: " + tok.LastErr() + ")"
	}
	return packed, fmt.Sprintf("token-exact (cut %d/%d tokens)", kept, tokOrig)
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
	// ProbeServedWindow is the agent loop's own /props probe (window.go): it
	// may cold-start the model, which is acceptable for the same reason there
	// — the caller is about to run exactly that model.
	n, ok := agent.ProbeServedWindow(ctx, p.cfg.Endpoint, model)
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
	c := tokclient.New(p.cfg.Endpoint, model, 0)
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
