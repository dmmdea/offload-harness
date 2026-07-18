---
status: Accepted
date: "2026-07-18"
---

# Defer instead of falling back to a cloud model

## Context

The harness exists to keep bulk, low-judgment work — summarize, classify, extract, triage — out of a
frontier model's context window and off its bill. It runs a Cascade of small local models and is
called by an agent that *is* a frontier model.

That creates an obvious temptation at the bottom of the Cascade: when every local Tier has failed or
returned a low-confidence answer, the harness could call a cloud model itself and return a good
answer anyway. Doing so would make the harness look more capable in benchmarks and remove the
caller's need to handle a second outcome shape.

It would also invert the entire point of the tool. A harness that silently reaches for a paid API is
a harness that spends money without the caller deciding to, holds credentials it has no reason to
hold, and hides its own failure rate behind purchased success.

## Decision

The Cascade never falls back to a cloud model. When it cannot complete a task confidently, it returns
a **Defer**: a structured, successful result telling the caller to do the task itself.

`core.Result` carries `Deferred bool` alongside `OK`, and every defer is built through one helper:

```go
// Deferf builds a deferred Result (harness could not complete; Claude should).
func Deferf(reason, partial string, meta Meta) Result {
	return Result{OK: false, Deferred: true, Reason: reason, Partial: partial, Meta: meta}
}
```

The harness holds no cloud credentials for this path, and the pipeline package imports no cloud
client — only the local llama-swap client and the local speech and image backends.

**One deliberate exception, outside the Cascade.** An explicit remote tool (`offload_nim`, backed by
`internal/nimclient`) exists as a user-invoked side channel in the MCP surface. It is not a fallback:
nothing in the Cascade can reach it, no defer escalates into it, and the caller must ask for it by
name. The rule this ADR fixes is about the *automatic* path — the harness never decides on its own to
spend money.

## Consequences

- **A Defer is a success, not an error.** Callers, tests, dashboards, and monitoring must treat
  `deferred: true` as a normal outcome. Alerting on defers as failures is a misreading.
- Callers must handle two result shapes. This is the cost, and it is paid deliberately: the caller is
  a capable model that can do the task, so handing it back is cheap.
- The harness's real capability stays visible. The defer rate is an honest measurement of where the
  local tiers fall short, which is what drives Tier and threshold work.
- The harness can run on a machine with no network egress and no secrets.
- Nobody can "fix" a quality problem by buying tokens. Fixing it means better routing, better
  grammars, better thresholds, or a better local model.

## Alternatives considered

- **Cloud fallback on exhaustion.** Rejected: it defeats the purpose (spends the tokens the harness
  exists to save), requires credentials, and makes failures invisible.
- **Optional cloud fallback behind a flag.** Rejected: a flag that exists gets turned on, and then
  every downstream consumer quietly depends on it. The explicit, separately-named `offload_nim` tool
  covers the genuine "I want a remote call here" case without putting cloud in the fallback path.
- **Returning an error instead of a Defer.** Rejected: an error implies something went wrong.
  Nothing did — the harness correctly judged that it should not answer. The distinct shape lets
  callers branch on "do it yourself" without error handling.

## Related code

- [`internal/core/types.go`](../../../internal/core/types.go) — `Result`, `Meta`, `Deferf`
- [`internal/pipeline/pipeline.go`](../../../internal/pipeline/pipeline.go) — the Cascade walk and
  every defer site
- [`internal/pipeline/recordless.go`](../../../internal/pipeline/recordless.go) — the agent-facing
  path, where a defer is returned as a successful tool result

## Related docs

- [../../glossary.md](../../glossary.md) — Defer, Cascade, Tier
- [0010-tier-optimization-before-latency-defer.md](0010-tier-optimization-before-latency-defer.md) —
  what to do instead when the local tiers underperform
