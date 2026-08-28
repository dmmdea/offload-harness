---
status: Accepted
date: "2026-08-28"
---

# 0029 — The Lenovo agent lane stays on the 4B seat; FreeToken is parked there after a disqualifying live finding

## Context

The 2026-08-27 nightshift measured gpt-oss-20b under FreeToken beating the
ampere-6 tier's qwen3.5-4b agent seat 36/36 @712 s vs 32/36 @1368 s on a
calibrated exact-answer instrument, and the operator approved adopting it for
the agent/delegation lane. The same measurement carried a caveat: the agentic
tier itself was instrument-invalid, and a production-representative A/B
"needs the real delegation path". This ADR records what the real path showed.

## What was built (and stays)

- FreeToken 0.1.2 installed on the ZFS pool
  (`/srv/ecosystem_backup/apps/freetoken`) with a **pool-local Python** —
  under `llama-swap.service`'s hardening (`ProtectSystem=strict`,
  `ProtectHome=yes`) a uv-managed venv is unusable until the interpreter is
  copied local and BOTH `_sysconfigdata_*.py` and `pyvenv.cfg`'s `home=` are
  repointed; `HOME`/`XDG_CACHE_HOME`/`TRITON_CACHE_DIR` pinned to the pool.
- A llama-swap-managed seat entry `gpt-oss-20b` (heavy group, ttl 300, cmd =
  `serve-agent.sh`): llama-swap owns lifecycle and the measured mutual
  exclusion (warm ft = 5511 MiB resident on the 5.67 GiB card; eviction by
  another heavy member verified). `--num-tokens 16384` — 32768 OOMs the 3050
  by ~150 MiB. Cold start ~120 s; completions honestly 503 until ready.
- Harness fixes the wiring surfaced, valuable for ANY OpenAI-only seat:
  0.108.1 grammar-free chat-repack fallback, 0.108.2 schema-guided scalar
  coercion. Both live fleet-wide.

## The disqualifying finding

On the real delegation path the seat failed 4/4 trivial extraction contracts
— and the fourth returned an answer belonging to a DIFFERENT conversation
(a "latest stable Go version" write-up against an inventory-manifest
contract). That is cross-request response contamination somewhere in the
FreeToken serving path (`max_running_req=4`, radix cache) while the same
llama-swap instance serves other clients. Answers that can belong to someone
else's request are disqualifying for a delegation seat regardless of
benchmark scores, and data-leakage-shaped besides.

## Decision

`agent_model` on the Lenovo REVERTED to `qwen3.5-4b-agent` (verified: the
same contract passes on-seat in 18 s). The `gpt-oss-20b` llama-swap entry
STAYS as an explicitly-addressed experimental seat — never the agent lane.

## Re-eval triggers

1. Isolate the contamination with a direct-`:1920`-vs-proxied differential
   (attribute engine vs proxy), then file it upstream with that evidence.
2. An upstream fix for the contamination PLUS a passing
   production-representative A/B (the real delegation path, ≥20 contracts,
   content-anchored acceptance) — the same bar this attempt failed.
