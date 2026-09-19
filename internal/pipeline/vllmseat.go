// vllmseat.go answers ONE question for the two structured-output send sites:
// is the seat this call is bound for served by vLLM?
//
// It has to be asked at all because the answer decides which constraint field
// the request carries. vLLM's request model allows unknown extras, so it
// ACCEPTS llama.cpp's top-level `grammar` member, discards it, and answers
// unconstrained — the defect register D-129 names, measured over nine days of
// the delegation log as 1,018 structured re-packs on vLLM seats against 506 on
// every other seat, with 3-attempt exhaustion at 19.7 % against 12.6 %. A vLLM
// seat gets `structured_outputs` instead (ADR 0002, amendment 2026-09-18); a
// llama.cpp seat keeps the raw GBNF, byte-identically.
package pipeline

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// vllmSeatTTL is how long one name's answer is reused. The roster only changes
// when llama-swap's config does, so a short memo is enough to keep a per-call
// decision off the network without ever going stale across an operator edit.
const vllmSeatTTL = 60 * time.Second

// vllmSeatRosterTimeout bounds the ONE roster read this decision can cost. It
// is the same 2 s the admission pre-flight spends on the same endpoint: the
// answer is an optimization, and a slow box must not pay for it on the request
// path.
const vllmSeatRosterTimeout = 2 * time.Second

// vllmSeatAnswer is one memoized decision.
type vllmSeatAnswer struct {
	isVLLM bool
	at     time.Time
}

// isVLLMSeat reports whether model names a seat THIS box declares as vLLM,
// resolving an alias through the live llama-swap roster when the name is not a
// declared id itself.
//
// The alias step is not defensive padding — it is the common case on the
// reference boxes. The Qube's agent seat is bound as `agent-pool-3card`, an
// alias of `qwen3.8-27b-vllm-3card`, and it is the CANONICAL id that appears
// in `vllm_seats`. An exact-match-only gate would therefore have left the
// three-card box, the single largest consumer of the re-pack path, on the
// grammar field vLLM throws away: the fix would have shipped and measured as
// no change at all.
//
// A roster that cannot be read resolves to FALSE — keep sending the grammar,
// which is the behaviour every seat had before this existed. Failing the other
// way would strip the constraint from a llama.cpp seat on a transient probe
// failure and turn a working call into a parse error, so the fail-open
// direction is the one that can only cost what the defect already costs. It is
// logged once per name: a silent fail-open here reads downstream as "the fix
// does nothing on this box", which is precisely the report nobody can act on.
func (p *Pipeline) isVLLMSeat(ctx context.Context, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	// The declared roster is free to consult and needs no memo.
	if p.cfg.DeclaresVLLMSeat(model) {
		return true
	}
	// Nothing is declared: there is no alias that could resolve INTO an empty
	// roster, so do not spend a roster read to learn it.
	if len(p.cfg.VLLMSeats) == 0 {
		return false
	}
	now := time.Now()
	if p.nowFn != nil {
		now = p.nowFn()
	}
	p.vllmSeatMu.Lock()
	if a, ok := p.vllmSeatCache[model]; ok && now.Sub(a.at) < vllmSeatTTL {
		p.vllmSeatMu.Unlock()
		return a.isVLLM
	}
	p.vllmSeatMu.Unlock()

	answer := false
	roster, rerr := swapclient.FetchRoster(ctx, p.cfg.Endpoint, vllmSeatRosterTimeout)
	if rerr != nil {
		p.vllmSeatWarn(model, rerr)
	} else if canonical, ok := roster.Canonical(model); ok {
		answer = p.cfg.DeclaresVLLMSeat(canonical)
	}

	p.vllmSeatMu.Lock()
	if p.vllmSeatCache == nil {
		p.vllmSeatCache = map[string]vllmSeatAnswer{}
	}
	p.vllmSeatCache[model] = vllmSeatAnswer{isVLLM: answer, at: now}
	p.vllmSeatMu.Unlock()
	return answer
}

// vllmSeatWarn logs a roster-probe failure ONCE per seat name, so a box whose
// llama-swap is down does not turn a per-call decision into a log flood while
// still saying, exactly once, why every call to that seat is on the legacy
// grammar path.
func (p *Pipeline) vllmSeatWarn(model string, err error) {
	p.vllmSeatMu.Lock()
	if p.vllmSeatWarned == nil {
		p.vllmSeatWarned = map[string]bool{}
	}
	first := !p.vllmSeatWarned[model]
	p.vllmSeatWarned[model] = true
	p.vllmSeatMu.Unlock()
	if first {
		log.Printf("pipeline: could not read %s's roster to resolve seat %q against vllm_seats; this seat keeps the raw GBNF grammar (the pre-D-129 behaviour): %v", p.cfg.Endpoint, model, err)
	}
}
