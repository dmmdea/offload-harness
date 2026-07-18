# Evaluation — can `/printing-press` improve the local-offload harness? (2026-06-16)

> Daniel asked whether `/printing-press` could improve the harness + the new Phase A.2 components. I read the full skill and assessed fit against the actual codebase. Verdict below; the actionable part is the audit at the bottom.

## What printing-press actually is
A generator that turns **an HTTP API (spec / HAR / URL)** into a ship-ready Go **API-client CLI**: research the API → absorb every competitor feature → emit a CLI with a **SQLite data layer + FTS search**, `sync`, agent-native output (`--json`/`--select`/`--compact`), an **MCP server** mirroring the Cobra tree, typed exit codes, and a scorecard/dogfood/verify shipcheck. Its whole premise is "there is a remote API; wrap it beautifully."

## Verdict: NOT a fit to generate/replace the harness — YES as a quality lens
- **The harness is not an API-client CLI.** It's a local-inference **orchestrator**: a Gemma-4 cascade with GBNF-grammar-constrained JSON, confidence/logprob gating, escalation, verbatim grounding, a self-learning ledger (bbolt + JSONL), conformal calibration, vision/video, and now STT. There is no remote API spec to absorb, and its value is the bespoke pipeline — exactly what printing-press's generic scaffold would discard. **Do not point printing-press at the harness.**
- **The API-client sub-components don't need it either.** `sttclient` (whisper-server: `/inference`,`/load`,`/health`), `llamaclient` (llama.cpp chat/grammar), and the future ComfyUI runner *are* small HTTP clients — but they're tiny, already clean, and must integrate tightly with the cascade/defer/zero-warm logic. A printing-press-generated standalone CLI for any of them would be a **separate binary** that can't plug into that pipeline. No real win.
  - *The one marginal exception:* if you ever want a **standalone exploratory CLI for the ComfyUI API** (richer surface: `/prompt`, `/history`, `/view`, queue) for poking at workflows by hand, printing-press could generate a decent one in ~30–60 min. Low value vs the Phase 2 integrated runner, but it's the only place the generator literally applies.

## The real value: printing-press's quality rubric as an audit lens
printing-press encodes a strong **agent-ergonomics bar** (its "Agent Build Checklist" + scorecard). Applying that bar to the harness surfaces a few concrete, modest, adoptable improvements — *without* running the tool. Audited against the actual code:

| printing-press principle | Harness today | Gap / opportunity |
|---|---|---|
| Structured `--json` on every command | ✅ all subcommands have `--json` | none |
| Non-interactive / CI-safe (no TTY prompts) | ✅ stdin/flags only | none |
| Defer/error as data, never crash | ✅ structured `Deferred` result | none (stronger than most generated CLIs) |
| MCP tools auto-exposed | ✅ 10 `offload_*` tools | **add `readOnlyHint: true`** — every offload tool is read-only (no external mutation); hosts (Claude Desktop) currently can't tell, so they bucket them as "write" tools. Small, correct win. |
| **`--select` field filtering / `--compact`** | ❌ none | **highest-value gap.** `offload_transcribe` returns a verbose `segments[]`; `video-describe`/`extract` return nested JSON. A `--select start,end,text` or `--compact` would let *me* (the agent) pull only what I need — which is literally the fastcontext citation-pattern the harness already espouses. Worth adding to the verbose outputs. |
| Typed exit codes (0/2/3/4/5…) | ⚠️ only 0/1/2 (ok/err/usage) | minor: a `deferred` outcome could map to a distinct non-zero code so scripts can branch on "harness punted" vs "harness failed". Low priority. |
| Progressive `--help` with realistic examples | ⚠️ usage lists commands, few examples | minor: add 1 realistic example per subcommand (e.g. `transcribe clip.mp4 --language es`). |
| `--dry-run` on side-effecting commands | n/a for most; render/transcribe write files | minor: a `--dry-run` on `transcribe`/`render` could print the planned ffmpeg/whisper/ComfyUI call without executing. Nice-to-have. |
| SQLite + FTS data layer | bbolt cache + JSONL ledger | **not a fit** — the harness's storage is purpose-built (single-writer cache, concurrent-append ledger feeding calibration). SQLite/FTS would be over-engineering. |

## Recommendation
1. **Don't run printing-press on the harness or its components** (wrong tool; would discard the bespoke pipeline). It stays a hand-built orchestrator.
2. **Adopt 2–3 of its agent-ergonomics patterns** as small, separate improvements when convenient — in priority order:
   - **`--select`/`--compact` on the verbose outputs** (`transcribe` segments, `video-describe`, `extract`) — best ROI, aligns with the harness's own citation-pattern philosophy.
   - **`readOnlyHint: true` on the MCP tools** — one-line-per-tool, improves host UX.
   - richer `--help` examples + a `deferred` exit code — minor polish.
3. These are independent, low-risk enhancements — a future small PR, not a regeneration. Each is the "smallest correct change," matched to the existing CLI idiom.

**Bottom line:** printing-press can't *build* this harness, but its quality bar is a useful checklist, and the `--select`/`--compact` idea is a genuinely good, on-philosophy improvement worth doing.
