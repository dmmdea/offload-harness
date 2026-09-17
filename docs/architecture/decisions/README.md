# Architecture Decision Records

Proposed, active, and historical technical decisions — their context, tradeoffs, consequences, and
rejected alternatives.

**Only ADRs with `Accepted` status are current guidance.** When reviewing, planning, or changing
code, treat everything else as background.

## Index

| ADR | Status | Title |
|---|---|---|
| [0001](0001-defer-never-cloud-fallback.md) | Accepted | Defer instead of falling back to a cloud model |
| [0002](0002-grammar-reliable-serving-flags.md) | Accepted | Grammar-constrained output via a raw GBNF field, not a schema parameter |
| [0003](0003-policy-broker-and-capability-flags-off-by-default.md) | Accepted | Single policy broker; capability flags off by default |
| [0004](0004-worktree-confinement-audit-outside.md) | Accepted | Worktree confinement with the audit trail stored outside it |
| [0005](0005-loopback-only-serve.md) | Accepted | Loopback-only serving unless explicitly opted out |
| [0006](0006-private-canonical-public-squash-mirror.md) | Superseded | Private canonical repository with a public squash-published mirror → [0012](0012-public-canonical-repository.md) |
| [0007](0007-host-torch-pinned-additive-provisioning.md) | Accepted | Pinned-additive provisioning that never moves host torch |
| [0008](0008-pdh-primary-vram-sampling.md) | Accepted | Per-process PDH counters as the primary footprint source |
| [0009](0009-zero-warm-gpu-lifecycle.md) | Accepted | Zero-warm GPU lifecycle for media generation |
| [0010](0010-tier-optimization-before-latency-defer.md) | Accepted | Fix the tier binding before adding latency-based defers |
| [0011](0011-flux-family-license-prohibition.md) | Accepted | FLUX-family models are prohibited |
| [0012](0012-public-canonical-repository.md) | Accepted | Public repository is canonical; private is the development and moat repository |
| [0013](0013-nodes-advertise-raw-footprint.md) | Accepted | Nodes advertise the raw observed footprint; the dispatcher owns all margin |
| [0014](0014-gpu-memory-provider-and-uma-sampling.md) | Accepted | GPU memory is a resolved provider (nvidia-smi else windows-generic WDDM); UMA samples Dedicated+Shared |
| [0015](0015-compaction-defaults-on-served-window.md) | Accepted | Compaction rungs default ON (measured flip decision); the budget probes the SERVED window; emergency shrink on the overflow retry |
| [0016](0016-phase-c-dedupe-pinning-force-preserve.md) | Accepted | Phase C: content-addressed dedupe rung, re-request pinning (H8), FORCE_PRESERVE elide/drop guards, fit=false telemetry, monotonicity invariant |
| [0017](0017-kv-reuse-is-binary-and-how-we-measure-it.md) | Accepted | KV reuse is append-only (any edit costs the whole cache); the `kvbench` mode measures it, brackets itself with controls, and fails closed |
| [0018](0018-machine-wide-fenced-gpu-lease.md) | Accepted | Arbitration moves below the render path: a machine-wide fenced GPU lease |
| [0019](0019-alias-backed-media-is-declared-per-tier.md) | Accepted | Alias-backed media is declared per tier, and the binding is derived from the seat |
| [0020](0020-residency-is-declared-with-matrix.md) | Accepted | Residency is declared with `matrix:`, not `groups:` |
| [0021](0021-one-renderer-the-installers-are-wrappers.md) | Accepted | One renderer: the installers are wrappers, not renderers |
| [0022](0022-escalation-repacks-from-the-original.md) | Accepted | The escalation boundary repacks from the original; the cache key is the logical request |
| [0023](0023-agent-lane-tailnet-auth-and-locality.md) | Accepted | The agent lane is tailnet-only, bearer-gated, quality-first-placed, and hop-limited |
| [0024](0024-accelerators-are-additive-to-the-gpu-tier.md) | Accepted | Accelerators are additive to the GPU tier |
| [0025](0025-model-residency-is-arbitrated-in-process-by-base.md) | Accepted | Model residency is arbitrated in process, keyed on the resolved base |
| [0026](0026-text-load-admissions-wait-for-the-media-lease.md) | Accepted | Text admissions that would load a model wait for the media lease |
| [0027](0027-freetoken-is-the-big-moe-opt-in-engine.md) | Accepted | FreeToken is the big-MoE opt-in engine on the blackwell-2x16 tier |
| [0032](0032-a-peer-held-seat-is-waited-for-not-deferred.md) | Accepted | A peer-held seat is waited for, not deferred |
| [0033](0033-cache-server-is-an-optional-second-device-tier.md) | Accepted | A second device's RAM is an optional KV tier, scored on capacity at parity cost |
| [0034](0034-fleet-overview-is-a-read-only-page-on-the-delegator.md) | Accepted | Fleet overview is a read-only page served by the delegator, from data nodes already publish |
| [0035](0035-persistent-vllm-seat-behind-llama-swap.md) | Accepted (residency **amended 2026-09-08**) | A vLLM agent seat lives behind llama-swap as a systemd unit the entry starts and stops. It is **not** persistent: `ttl: 300`, no group, no preload, no `[Install]` section |
| [0036](0036-the-agent-lane-is-a-harnessed-environment.md) | Accepted | The agent lane is a harnessed environment: rules are data, the trace is telemetry, the rigger proposes |
| [0037](0037-a-capability-name-has-one-owner-per-box.md) | Accepted | A capability name has one owner per box: the first listed accelerator |
| [0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md) | Accepted | Accelerator work travels to the box that has the device, bytes included: fleet task `accel`, `fleet_accelerators`, local device wins a shared name |
| [0039](0039-a-box-is-the-union-of-its-tiers-and-placement-is-a-per-task-decision.md) | Accepted | A box is the union of its tiers, and placement is a per-task decision: `composes`/`layers`, one placement table, display-card guards that fail closed, `placed` on every result |
| [0039](0039-a-held-card-is-a-place-in-line.md) | Accepted | A held card is a place in line, and a cleared seat stays cleared: `gpu reserve --wait` queues (default 8h), `--unload-seat` stamps the text lease exclusive so loads are gated like a render, `offload_status.gpu_lease.queue_with` |
| [0040](0040-vision-work-travels-to-a-node-with-an-idle-card.md) | Accepted | Vision work travels to a node with an idle card: fleet task `vision` on `POST /fleet/vision` (body capped from `vision_max_image_bytes`, then the shared admission path), the full `core.Result` as job data, `route: local|auto|remote` on the three image tools |
| [0043](0043-serving-config-provenance.md) | Accepted | The rendered serving config carries its own provenance: `install render` stamps `spec_sha256` (a closed input set: tier, render params, template, tier entry, harness version) + `body_sha256`, `audit-yaml --against-render` re-derives and reports MATCH / STALE(keys) / UNSTAMPED / HAND-EDITED, and `/fleet/health` publishes `serving_config_spec_sha256` + `serving_config_state` |
| [0041](0041-the-drain-waits-for-runs-inside-the-queue.md) | Accepted | The drain waits for runs, inside the queue budget: `--drain-timeout` defaults to the rest of `--wait`, the lease is `draining` during the drain and `exclusive` after it (`Restamp`), agent runs register in `<state root>/gpu/activity/`, and `gpu status` / `offload_status.gpu_lease` carry a `verdict` + `activity` saying what the cards are doing |
| [0042](0042-no-bare-http-client-on-a-caller-named-host.md) | Accepted | No bare HTTP client on a caller-named host: `internal/netguard/publicnet.go` holds the tree's one public-IP predicate (`CheckPublicIP`), one resolution seam (`LookupIP`) and the pinned `PublicDialContext`/`PublicTransport`; the research lane dials the address it validated, on the first hop and every redirect |
| [0049](0049-ampere-16-vllm-seat-is-the-3bit-gsq-27b.md) | Accepted | The ampere-16 vLLM seat is Qwen3.8-27B **3-bit GSQ** (ISTA-DASLab, 11.85 GB — transformer 3-bit g128, embedding+LM head 4-bit RTN g64), the only published vLLM-loadable 27B that fits 16 GB where every W4A16 build is 19.45-19.56 GB; chosen for **3.7x concurrency** (19.53 vs 5.26 tok/s at 4 streams, single-stream a tie) and for being the tier's only path to the cache server (ADR 0045), NOT on blind quality; requires vLLM >= 0.29.0 plus the checkpoint's carried embedding patch (`engine_min_version` / `engine_patch` are seat fields); the BOUND agent lane is unchanged; amended 2026-09-16: the matched-window blind result (8.35 vs 9.29 at 49,152, gap 0.94, 21/24) did not close the gap and stands as the seat's recorded quality cost; deployed live the same night on vLLM 0.29.0 + the carried patch (loads, serves, digest-8 8/8 at 900 s / 3/8 at 300 s) — the BOUND lane stays the 4B because at util 0.92 the seat takes mem0's embedder off the card (HTTP 500) and at 0.90 the window no longer fits (est. max 32,928); binding it is an operator decision (32,768 @ 0.90, or move the embedder); Amendment 3: the operator chose 32,768 @ 0.90, measured first (KV 43,690 tok, embedder 200 under 4-stream load, 7.17 / 20.36 tok/s) and wired — the seat declaration now carries its bound-lane settings (4,096 / thinking off / vendor sampling / 900 s / 7.17 tok/s) |
| [0048](0048-vllm-is-a-first-class-engine-on-every-tier.md) | Accepted | vLLM is a first-class engine on every tier, equal to llama.cpp, and a tier never loses it as a side effect: ADR 0047's ship deleted ampere-16's whole `vllm_seat` to change a MODEL, which also removed the tier's LMCache capability (a binding is per vLLM seat, ADR 0045) while both existing guards passed vacuously because they `continue` on a nil declaration; the seat is restored, `TestEveryTierCanSeatAModelUnderVLLM` asserts the SET of tiers (regression floor + a countable `vllmSeatDebt`, today 3 of 16), and `TestEveryDeclaredVLLMSeatValidates` fails instead of skipping when nothing is declared |
| [0047](0047-ampere-16-agent-seat-reaudit.md) | Accepted | The ampere-16 agent seat is re-audited blind and the 4B verdict behind ADR 0029/0035 is void: Qwen3.8-27B UD-IQ3_S with its embedded MTP head wins 24 of 24 blind judgements (9.32 vs the incumbent 4B's 5.39, zero firsts and fourteen lasts for the 4B); the entry renders and is callable by name with `timeout_sec` up to the 900 s cap, but the DEFAULT binding is HELD on the fallback because a delegated contract's wall is stamped at 300 s by the delegator and no node-side setting extends it (register D-03) |
| [0046](0046-ledger-rows-carry-their-origin-and-job.md) | Accepted | Every ledger row names the session that asked and the job behind it: `ledger.Record` stamps `origin_session` (read once per process from `LOCAL_OFFLOAD_ORIGIN` / `CLAUDE_CODE_SESSION_ID`, the variable Claude Code exports to its MCP servers), `origin_pid` / `origin_ppid`, and one token figure `cards_tokens` (always written — its presence marks the schema); delegate rows add `job_id`, `route`, `placement`, `steps`, `stop_reason`, `repack_ms`, `acceptance_result`; pre-0.124.0 rows read as unattributed, and the share gate enforces the session figure only with evidence the writers stamp |
| [0045](0045-a-cache-server-binding-per-vllm-seat.md) | Accepted | `kv_cache_server` is a LIST of per-seat bindings (the pre-0.121 single object still loads, as one element); `vllm_seats` declares the box's vLLM roster; validation refuses two bindings for one seat and a `key_prefix` shared across stack generations; `doctor` fails a vLLM seat with no binding unless it carries `storeless: true` with a `reason` |
| [0044](0044-write-capable-delegation-door.md) | Accepted | A write-capable delegation door, default off: a contract may carry `write_root` (RELATIVE to the run read root), a node opts in with `agent_allow_write` (false by default; a refusal is an ACK 400 on the fleet path and `defer_class: "write"` on the local one), the write set comes back as a unified diff the HARNESS NEVER APPLIES, and the door grants create+overwrite in one directory with no delete/shell/run/fetch/github, capped at 8 files / 64 KiB and confined by `os.Root` |


## Lifecycle

An ADR starts as `Proposed` while the decision is under discussion, and becomes `Accepted` once
approved. A decision that later changes does **not** get rewritten: write a new ADR, set the old one
to `Superseded`, and link it forward with `superseded_by`. Accepted ADRs may still receive small
corrections and added links that do not change the recorded decision.

Statuses:

- `Proposed` — under discussion, not current guidance.
- `Accepted` — the current decision.
- `Superseded` — replaced by a newer ADR (must carry `superseded_by`).
- `Deprecated` — discouraged, still historically relevant.
- `Rejected` — considered and intentionally not adopted.

## Ownership

Architectural decisions are human-owned. Agents may draft ADR text from decisions that have already
been made, and keep existing ADRs aligned with the code — but an agent does not decide architecture,
and does not move an ADR to `Accepted` on its own.

## Writing a new one

Copy [../../templates/adr.md](../../templates/adr.md), name it `NNNN-<slug>.md` with the next free
number, and add a row to the index above. The frontmatter schema is fixed — see
[../../STYLE.md](../../STYLE.md).
