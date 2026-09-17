// coherence.go — the post-warm seat coherence probe (register D-118).
//
// Why it exists. On 2026-09-16/17 a vLLM seat (Qwen3.8-27B GSQ, fp8_e5m2 KV on
// an RTX 5060 Ti under vLLM 0.29 / FlashInfer) came up HEALTHY by every gate
// the harness had — /health, /v1/models, the speed probe and the READY smoke
// all passed — and every completion it produced was numerically broken:
// `<tool_call>!!!!!!!!!!!!!!!!!!!!…` to the cap, or hallucinated prose. Each
// contract spent 126–336 s generating that and was filed `unparsed_tool_call`;
// two GPU leases went on parser hypotheses before anyone read a raw completion.
//
// The fix is one cheap question asked at the ONE moment a seat's numerics can
// have just changed — after its cold load, before the contract's wall starts:
// "call read_file, then say DONE". A seat that parses the tool call is
// coherent. A seat that answers twenty identical bytes in a row, or emits a
// tool-call marker its own server cannot parse, is broken, and the contract
// defers as `infrastructure` having spent seconds instead of the whole wall.
//
// Fail-open is the rule everywhere the seat does not ANSWER: a transport
// error, a timeout or an empty choice list proceeds with a note, exactly as
// warmSeat does. The probe exists to catch a seat that answers WRONGLY; a seat
// that does not answer at all is the loop's first chat call to diagnose, with
// far more detail than this one completion carries.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

const (
	// coherenceProbeMaxTokens bounds the probe completion. 96 is enough for a
	// read_file tool call plus DONE and small enough that a NaN seat spamming
	// token 0 stops within a second or two instead of running to a 1,024-token
	// cap — the probe's whole point is that it is cheap even when it fails.
	//
	// Deliberately NOT raised for thinking seats (reviewer finding, D-118): a
	// think block runs to hundreds or thousands of tokens, so no cap this probe
	// can afford would let one finish, and a bigger cap only multiplies what a
	// degenerate seat costs. A seat cut inside its think block is handled where
	// it belongs — in the verdict, as a proceeding note, never as a defer.
	coherenceProbeMaxTokens = 96
	// coherenceProbeCap is the hard ceiling on the probe, whatever the
	// admission budget still holds. A seat that needs more than 90 s to emit 96
	// tokens has a problem the probe is not the right instrument for.
	coherenceProbeCap = 90 * time.Second
	// coherenceProbeMinBudget is the floor under which the probe is skipped
	// rather than run against a deadline it cannot meet: a probe that times out
	// because it was given two seconds proves nothing and costs the note.
	coherenceProbeMinBudget = 3 * time.Second
	// coherenceProbeGoal is the request. It names a file the contract's own
	// context dir carries by convention and, more importantly, it is a shape
	// EVERY seat can satisfy in one turn: call the tool, or say the word.
	coherenceProbeGoal = "Read the file notes.md with the read_file tool, then answer with the single word DONE."
)

// coherenceProbeTool is the one tool the probe advertises: read_file with a
// single string `path`, the same name and parameter the loop's own read_file
// carries (internal/agent/tools.go), so a seat whose chat template or tool
// parser is mismatched fails HERE in the same way it would fail in the loop.
// Deliberately trimmed to one required property: the probe tests whether a tool
// call round-trips at all, not whether the seat handles offset/limit.
var coherenceProbeTool = agent.ToolSpec{
	Name:        "read_file",
	Description: "Read a file within the workspace. path is relative to the workspace root.",
	Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the workspace root"}},"required":["path"]}`),
}

// CoherenceVerdict is what one probe learned.
//
// Ran false means nothing was asked (policy, or no budget) — Broken is then
// meaningless and Note says why. Ran true with Broken false covers BOTH "the
// seat answered sanely" and "the seat could not be reached": the probe never
// converts silence into a defer, so the two share the proceeding branch and are
// told apart by the Note.
type CoherenceVerdict struct {
	Ran    bool
	Broken bool
	Note   string
	Spent  time.Duration
}

// CoherenceProbeWanted answers whether this box's policy asks for a probe on a
// run whose warm-up did (coldLoaded) or did not attempt a load. One resolver
// for both agent doors, so the delegation lane and agent_run can never disagree
// about when the probe fires.
//
// coldLoaded is a BOOLEAN and not "the cold load took more than zero": warmSeat
// measures with the monotonic clock, whose granularity on Windows is the system
// timer tick, so a sub-tick warm-up on a fake endpoint reports exactly 0 while
// having plainly loaded the seat. The callers pass warmSeat's own "did anything
// happen" condition (a non-zero duration OR a note), which is the same test the
// admission block already uses to decide there was a warm-up at all.
func CoherenceProbeWanted(cfg config.Config, coldLoaded bool) bool {
	switch cfg.CoherenceProbe() {
	case "off":
		return false
	case "always":
		return true
	default: // "cold"
		return coldLoaded
	}
}

// ProbeSeatCoherence asks the seat one bounded question and judges the raw
// completion. It uses the SAME client the loop uses (agent.LLMClient) with the
// SAME non-thinking render and the SAME sampling policy, because a probe that
// spoke to the seat differently from the loop could pass on a seat the loop
// then fails on — which is the defect it exists to prevent, one layer up.
//
// budget is what is left of the ADMISSION budget. The caller runs this before
// the wall context exists; nothing here may be charged to the contract's wall.
func ProbeSeatCoherence(ctx context.Context, cfg config.Config, seat string, budget time.Duration) CoherenceVerdict {
	if budget < coherenceProbeMinBudget {
		return CoherenceVerdict{Note: fmt.Sprintf(
			"coherence probe skipped: %.0fs of admission budget left, under the %.0fs floor",
			budget.Seconds(), coherenceProbeMinBudget.Seconds())}
	}
	if budget > coherenceProbeCap {
		budget = coherenceProbeCap
	}
	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// Thinking OFF and the node's own sampling: a think block would eat the
	// 96-token cap and report every healthy thinking seat as silent, and a
	// decoding policy other than the loop's would test a seat the loop never
	// speaks to (D-95b).
	pctx = agent.ContextWithoutThinking(pctx)
	pctx = agent.ContextWithSampling(pctx, cfg.AgentSampling)

	// Keyless local planner — the same construction agent.Build gives the loop.
	client := agent.NewLLMClient(cfg.Endpoint, seat, "", budget)
	start := time.Now()
	comp, err := client.Chat(pctx,
		[]agent.Msg{{Role: "user", Content: coherenceProbeGoal}},
		[]agent.ToolSpec{coherenceProbeTool}, coherenceProbeMaxTokens)
	spent := time.Since(start)
	if err != nil {
		return CoherenceVerdict{Ran: true, Note: "coherence probe inconclusive (" + err.Error() + "); proceeding", Spent: spent}
	}
	v := JudgeCoherence(comp, spent)
	v.Spent = spent
	return v
}

// JudgeCoherence is the verdict rule over ONE completion, split out so both
// agent doors and the tests read the same judgement rather than a copy of it.
func JudgeCoherence(comp agent.Completion, spent time.Duration) CoherenceVerdict {
	// A parsed tool call is the strongest possible pass: the seat decoded, the
	// template rendered, and the server's tool-call parser agreed with it. That
	// is the whole chain the live failure broke.
	if len(comp.Msg.ToolCalls) > 0 {
		return CoherenceVerdict{Ran: true, Note: fmt.Sprintf("coherence probe: tool call parsed in %.1fs", spent.Seconds())}
	}
	content := comp.Msg.Content
	if b, n := agent.DegenerateRun(content); n > 0 {
		return CoherenceVerdict{Ran: true, Broken: true, Note: fmt.Sprintf(
			"%sthe completion repeats %q %d times (finish_reason %q, %d chars)",
			core.IncoherentSeatReason, string(b), n, comp.FinishReason, len(content))}
	}
	// A 96-token completion that produced nothing printable and stopped at the
	// cap is the same NaN shape without the punctuation: the seat generated to
	// its limit and emitted no content. A finished-and-empty turn is NOT judged
	// broken — that is an ordinary (if useless) answer, and the loop is the
	// right place to deal with it.
	//
	// EXCEPT when the seat reported hidden reasoning (reviewer finding, D-118).
	// A thinking seat cut inside its think block arrives in EXACTLY this shape
	// and is perfectly healthy: the client refuses to fold the reasoning
	// channel into Content at finish_reason "length" on purpose (client.go — "a
	// think block cut by max_tokens is not an answer"), so content is "" and
	// finish is "length" for a seat that is only slower to the point than 96
	// tokens allow. ContextWithoutThinking does not save us: it sends
	// `chat_template_kwargs:{"enable_thinking":false}`, which a DeepSeek-R1 /
	// gpt-oss / llama.cpp `--reasoning-format` template ignores. The loop's own
	// classifier calls this same completion recoverable reasoning starvation
	// (agent.Completion.Starvation), and two rules in one repo must not
	// disagree about one completion — so the probe proceeds and says why.
	if content == "" && comp.FinishReason == "length" {
		if rChars, rTok := len(strings.TrimSpace(comp.Reasoning)), reasoningTokens(comp); rChars > 0 || rTok > 0 {
			return CoherenceVerdict{Ran: true, Note: fmt.Sprintf(
				"coherence probe: cut inside the think block at the %d-token cap (%s; proceeding)",
				coherenceProbeMaxTokens, hiddenReasoningBasis(rChars, rTok, comp.ReasoningKey))}
		}
		return CoherenceVerdict{Ran: true, Broken: true, Note: fmt.Sprintf(
			"%sthe completion is empty at the %d-token cap (finish_reason %q, no reasoning channel reported)",
			core.IncoherentSeatReason, coherenceProbeMaxTokens, comp.FinishReason)}
	}
	// A tool-call marker in plain TEXT with no parsed tool calls is the seat's
	// server failing to parse its own model's tool syntax — the 2026-09-04 Qube
	// hermes-parser-on-a-Qwen3-template defect, which the loop already names
	// (agent.ErrUnparsedToolCall) after it has spent the wall.
	if marker := agent.UnparsedToolCallMarker(content); marker != "" {
		return CoherenceVerdict{Ran: true, Broken: true, Note: fmt.Sprintf(
			"%sthe completion carries %s with no parsed tool call (finish_reason %q, %d chars)",
			core.IncoherentSeatReason, marker, comp.FinishReason, len(content))}
	}
	// Plain text and no tool call: the seat is decoding fine and simply chose
	// to answer rather than call. Not a pass of the tool-call chain, so the
	// note says what it is; never a defer — plenty of healthy small seats
	// answer a one-line instruction in words.
	return CoherenceVerdict{Ran: true, Note: fmt.Sprintf(
		"coherence probe: answered in text without a tool call in %.1fs (proceeding)", spent.Seconds())}
}

// reasoningTokens is the seat's own count of tokens spent in the hidden
// reasoning channel, or 0 when this backend reports no usage accounting at all
// — nil Serve means "unmeasured", never "zero reasoning".
func reasoningTokens(comp agent.Completion) int {
	if comp.Serve == nil {
		return 0
	}
	return comp.Serve.UsageReasoningTokens
}

// hiddenReasoningBasis is the one-line evidence behind a "cut inside the think
// block" verdict: whichever of the two signals the seat actually gave, under
// the key it answered with, so an operator reading the note can tell a
// reasoning seat from a silent one without re-running the probe.
func hiddenReasoningBasis(chars, tokens int, key string) string {
	if key == "" {
		key = "reasoning"
	}
	switch {
	case tokens > 0 && chars > 0:
		return fmt.Sprintf("%d reasoning tokens, %d chars under %q", tokens, chars, key)
	case tokens > 0:
		return fmt.Sprintf("%d reasoning tokens reported under %q", tokens, key)
	default:
		return fmt.Sprintf("%d chars of hidden reasoning under %q", chars, key)
	}
}

// --- the incoherent-seat memo (reviewer finding, D-118) ------------------
//
// Under the shipped default ("cold") only the contract that LOADS the seat is
// probed, and a broken seat stays loaded: the defer path unloads nothing, and
// nothing in the fleet quarantines a local seat. So contracts 2..N landed on
// the same NaN seat the probe had already caught and each spent its wall on
// degenerate output — the very repeat the probe was written to prevent.
//
// The memo closes that: a broken verdict is remembered per endpoint+seat, and
// a WARM run that would not have probed at all defers immediately on it. It is
// process-local on purpose — the fleet node and the MCP server are the
// long-lived processes that take contract after contract, which is where the
// repeat happens; a one-shot CLI invocation has no second contract to protect.
//
// Two ways out, so a fixed seat is never stuck deferring: any later probe that
// is NOT broken (an "always" box, or the next cold load) clears the entry, and
// an entry expires on its own after coherenceMemoTTL. The TTL is longer than
// the fleet's 5-minute idle unload, so a seat that fell out of residency comes
// back through a cold load — which re-probes and overwrites the memo anyway.
const coherenceMemoTTL = 10 * time.Minute

var coherenceMemo struct {
	sync.Mutex
	broken map[string]coherenceMemoEntry
}

type coherenceMemoEntry struct {
	note string
	at   time.Time
}

func coherenceMemoKey(endpoint, seat string) string { return endpoint + " seat=" + seat }

// RememberCoherence files what one probe learned about one seat. A broken
// verdict is remembered; any other verdict the probe actually RAN clears the
// seat, because a seat that just answered sanely is evidence against whatever
// a previous residency did. A verdict that never ran teaches nothing and
// leaves the memo untouched.
func RememberCoherence(endpoint, seat string, v CoherenceVerdict) {
	if !v.Ran || seat == "" {
		return
	}
	coherenceMemo.Lock()
	defer coherenceMemo.Unlock()
	if !v.Broken {
		delete(coherenceMemo.broken, coherenceMemoKey(endpoint, seat))
		return
	}
	if coherenceMemo.broken == nil {
		coherenceMemo.broken = map[string]coherenceMemoEntry{}
	}
	coherenceMemo.broken[coherenceMemoKey(endpoint, seat)] = coherenceMemoEntry{note: v.Note, at: time.Now()}
}

// RecallIncoherentSeat reports what this process last learned about a seat it
// judged broken, and whether that verdict is still fresh. The note keeps the
// core.IncoherentSeatReason prefix the delegator's retry gate matches on, with
// the age appended so an operator can tell a remembered verdict from a probe
// that just ran.
func RecallIncoherentSeat(endpoint, seat string) (string, bool) {
	if seat == "" {
		return "", false
	}
	key := coherenceMemoKey(endpoint, seat)
	coherenceMemo.Lock()
	defer coherenceMemo.Unlock()
	e, ok := coherenceMemo.broken[key]
	if !ok {
		return "", false
	}
	if age := time.Since(e.at); age > coherenceMemoTTL {
		delete(coherenceMemo.broken, key)
		return "", false
	}
	return fmt.Sprintf("%s (remembered from this seat's probe %s ago; it has not cold-loaded since)",
		e.note, time.Since(e.at).Round(time.Second)), true
}

// forgetCoherence drops a seat's remembered verdict. Tests use it to keep one
// process's memo from leaking between cases.
func forgetCoherence(endpoint, seat string) {
	coherenceMemo.Lock()
	defer coherenceMemo.Unlock()
	delete(coherenceMemo.broken, coherenceMemoKey(endpoint, seat))
}
