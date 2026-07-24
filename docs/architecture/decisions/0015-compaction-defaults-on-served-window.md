---
status: Accepted
date: "2026-07-24"
---

# Compaction rungs default ON; the budget targets the SERVED window

## Context

The skeleton (`--skeleton-prune`, v0.22.17) and GCF (`--gcf-compact` / `gcf_compact`, v0.22.18)
compaction rungs shipped default-OFF, gated on measurement by the compaction eval harness
([Phase B](../../systems/coding-agent.md), v0.22.23). The measurement now exists: a real replay
corpus (17 harvested agent transcripts, production-mirrored replay knobs) plus two
control-pair-gated live A/Bs showed compaction at production pressure does not cost task outcome
(base delta +0.119 CI[+0.047,+0.194]; skeleton +0.090 CI[+0.029,+0.165] — compacted transcripts
scored BETTER), skeleton's entity retention is never worse than base and better where the ladders
differ, and GCF is a no-op on agent transcripts while keeping its measured pipeline win. The
operator approved the flip from that report (flip decision 2026-07-24).

The same measurement exposed two window bugs. (a) `--ctx-tokens` defaulted to 16384 — a stale
per-tier assumption — while both serving tiers ran `-c 8192`: two real runs died with
`exceed_context_size` 400s before the compaction budget ever engaged. (b) When the oversized body
is the transcript's NEWEST tool result, every ladder rung is forbidden (keep-recent), so the
reactive harder-compaction retry re-sent the same overflow byte-for-byte and the run died anyway.

## Decision

1. **Skeleton and GCF default ON everywhere the ladder runs**: the `local-agent` CLI flags, the
   MCP `agent_run` path (previously wired to neither), and the pipeline's `gcf_compact` config
   default. An explicit `false`/flag still disables; the eval verbs keep explicit flags.
2. **The compaction budget targets the SERVED window, never an assumption.** `--ctx-tokens`
   defaults to 0 = auto: probe the endpoint's live `n_ctx` (`/upstream/{model}/props` for
   llama-swap, `/props` for a bare llama-server), falling back to the conservative 8192 when
   unanswerable. An explicit flag overrides the probe but warns when it exceeds the served window.
3. **The reactive-overflow retry gets a last resort**: when the harder compact cannot fit the
   budget (typically the huge-newest-body case), `emergencyShrink` reduces tool BODIES —
   skeleton first, then elision markers, oldest-first, finally trimming the one body that still
   overflows — never touching the preamble and never dropping turns. Recency protection yields
   only at the point where the alternative is a dead run.

## Consequences

- Default agent behavior changes for the first time on measured evidence, per the Phase-B gate's
  whole purpose; the flip is reversible per-surface (flags/config) without code.
- A serving-config change (larger or smaller `-c`) is picked up automatically at run start; the
  16384-assumption class of failure cannot recur where the probe answers.
- The probe may cold-start the planner model on llama-swap — acceptable, the run is about to use
  exactly that model.
- `emergencyShrink` can destroy recent-tool-body detail on the overflow path; the alternative it
  replaces is the run aborting with the same information lost and nothing produced.
