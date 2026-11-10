---
status: Proposed
date: "2026-11-08"
---

# The memory wall, mapped onto the fleet: what we store, where we keep it, how much work we reuse

## Context

NVIDIA's "5 Ways to Tackle the AI Memory Wall" (NVIDIA Developer, 2026-11-05, with a companion deep dive on
the NVIDIA technical blog) frames inference memory as three questions: what we store, where we keep it, and
how much work we reuse. It ranks five answers and draws one tier ladder:

| tier | on screen | role |
|---|---|---|
| G1 | GPU HBM | active model work |
| G2 | host memory | staging |
| G3 | local SSDs | node-local retention |
| G3.5 | NVIDIA CMX | pod-level, Ethernet-attached flash, shared KV cache |
| G4 | shared storage | files and results |

The five, in the video's order (5 to 1):

5. **NVFP4**: 4-bit E2M1 values with one FP8 E4M3 scale per 16 values (MXFP4: one E8M0 scale per 32).
   Near-FP8 accuracy is NVIDIA's published result on DeepSeek-R1-0528. Weights shrink about 72 % against BF16
   (Kimi-K3, 2.8T parameters: 5.6 TB to 1.575 TB plus scales), and KV in NVFP4 needs model and serving support.
6. **WideEP + NVLink**: spread a MoE layer's experts (DeepSeek-R1-0528: 256 routed, 8 per token, 1 shared)
   over more GPUs, so each GPU holds a smaller share and has room for context. The price is the
   dispatch-and-combine traffic, and the slide states the trade as "local capacity gains vs communication cost".
7. **Dynamo**:
   - Disaggregated prefill and decode, joined by a KV transfer.
   - Reuse of a retained shared prefix when the model matches and the state is compatible.
   - Engine-local offload, HBM to host to storage, to "retain context locally".
   - A KV-aware router whose hints let worker B retrieve worker A's context, with NIXL moving the bytes.
8. **Vera**: an agent session waiting on a tool holds HBM next to other requests. If the context is
   discarded, the next turn pays a prefill recompute. Instead the serving runtime can move the session's KV to
   coherent host memory (NVLink-C2C, LPDDR5X) and restore it when needed. The slide's response budget is a
   three-way choice: keep in HBM, move out and restore, or rebuild by compute.
9. **CMX**: a server with no local match retrieves the stored KV from a shared Ethernet flash tier
   (BlueField-4 STX) instead of processing the input again. It checks a matching model, compatible state and
   permitted access, and prefetches in software. "The goal: retrieve faster than reprocessing." The 5x
   throughput and 5x power figures are NVIDIA-reported against traditional storage.

This fleet has none of that hardware: no NVLink, no P2P, PCIe Gen3 on the pair boxes, 16 GB consumer cards
(Blackwell sm_120 and Ampere), and commodity Ethernet. The question this ADR answers is which of the five
LESSONS carry over to that hardware, where each one already lives in the harness, and what is worth building.

## Decision

### How much work we reuse: measure it first

The agent loop's prefill instrument (`internal/agent/prefillstats.go`) read only llama.cpp's `timings`.
vLLM seats answer with `usage` and no `timings`, so every vLLM step counted as an observed step with zero
cached and zero prefilled tokens. Each run then said `basis: "measured"` with a null reuse ratio, and the
seat that carries the pair boxes' agent work published no reuse at all. The instrument now falls back to
`usage.prompt_tokens_details.cached_tokens` when the server reports it. When the server does not, the step
is unmeasured, and the report names its source. Every other decision below is judged by that number.

### Where we keep it: G2 and G3 on every llama.cpp seat

- **G2 already ships.** It is `--cache-ram` sized by RAM tier (ADR 0056 Layer 1), together with llama.cpp's
  `--cache-idle-slots` parking of an evicted slot. A session's KV that loses its slot to another request goes
  to host RAM inside the same process and comes back on a prefix match. That is the video's "move out and
  restore", within one process, and it covers every tool wait shorter than the 300-second idle unload.
- **G3 ships now.** ADR 0056 Layer 2 was blocked because a restored slot file buys zero prefill on b9934
  (upstream #25913). The fix is upstream PR #26004, at tag b11506. On a build carrying it, b11977 on
  `<node-b>`, with Qwen3.5-9B (hybrid) on CUDA and every arm a new process, a restore turns a 3,279-token
  prefill into 4 tokens, and a follow-on turn into 26. Arms and numbers are in the ADR 0056 amendment.
  The node now drives it:
  - **Render.** `--slot-save-path <home>/kvslots` renders only when the render knows the install home, the
    node's llama.cpp build is at or above the floor in `setup/floors.json`, and the entry runs one slot. The
    render creates the directory. A render-time audit refuses the flag beside `--parallel` above 1 and on an
    entry that states no `--parallel`; the token follows `--parallel 1` in each entry, never a shared macro.
  - **Run start.** The agent loop keys the byte-exact prefix it is about to send: the seat's config pin, the
    model, the tool specs, and every message up to the first turn, recall and setup replay included. When a
    file for that key exists, the node restores it.
  - **Admission.** Saving follows a presence rule, the write admission of Dynamo's KV block manager. A key
    is saved on its second sighting on the node and restored from the third, because most prefixes never
    recur and a file is about 80 KB per token.
  - **Hit.** A restore is judged on the completion that follows it: `hit` only when that completion took at
    least 80 % of its prompt from the cache. Any other outcome is `miss`, and the ledger cannot say `hit`
    for a prefill that happened.
- **Vera's case on this fleet is the idle unload, not HBM contention.** A `--parallel 1` seat's slot is
  preallocated, so a waiting session holds no memory another request could use. What loses the session's KV
  is the seat unloading under it: the 300-second idle rule, a media job freeing the cards, or a GPU lease.
  Before a tool call whose cap reaches 240 seconds (the browse tool's 360 s cap is past the unload), the loop
  saves the slot under a run-scoped key, concurrently with the tool. After the tool returns, if the seat went
  cold, it warms the seat through the normal path and restores. The run-scoped file is deleted when the run
  ends. Saves are not taken on every step: a save is a disk write of the whole context.
- **The node reads the slot directory from the running seat's own command line**, not from
  `OFFLOAD_HOME`, so the render and the node agree by construction. Files stay under the existing LRU cap
  (`kvslot_cap_gib`, default 8 GiB), and the sweep deletes only harness-named `k1-<sha256>.bin` files. A restore
  touches its file, so a hot key is not evicted as old.
- **Two refusals on both doors** (the loop's store and the bearer-gated HTTP lane share one core):
  - A seat whose running command does not state exactly one slot is refused. The e2b entries state no
    `--parallel`, and the server's default there is several slots.
  - A save is skipped when the slot directory's volume has under 6 GiB free, because the cap sweep runs after
    the write.
- `agent_kvslot_retention: false` turns the whole path off. It is on by default wherever the seat supports it.

### What we store: NVFP4

- **vLLM.** `--kv-cache-dtype nvfp4` reached vLLM 0.32.0 for SM12x. On two RTX 5060 Ti it decoded at 8.8 and
  17.4 tok/s against 62.3 and 60.8 for fp8, and every second request came back corrupted on SM120 (vLLM PR
  #46329, open). The fleet keeps fp8 KV on Blackwell (e5m2 returns NaN there, ADR 0048) and the per-seat
  measured fp8_e5m2 on Ampere. A seat spec that pairs nvfp4 KV with a cache-server binding is refused: the
  LMCache overlay guard keyed only on an fp8 prefix, and the restore fidelity of nvfp4 through LMCache is
  unmeasured. Storeless nvfp4 stays expressible for a measured experiment. There is no allowlist of dtypes,
  because the engine's list moves (ADR 0099).
- **llama.cpp.** Native NVFP4 matmul on sm_120 merged upstream (PR #22196). It speeds prefill and leaves
  decode flat, at 4.5 bits per weight, the size of Q4_K_M. llama.cpp has no FP4 KV type. An NVFP4 GGUF
  becomes a seat's file only when it is the best available quant of that model and a quality comparison on
  the same build says so; the fleet is not re-quantized for it.

### Rulings on the rest

- **WideEP: nothing to adopt.** Expert parallelism pays for its all-to-all with NVLink. On PCIe Gen3 without
  P2P, splitting a model that fits one card costs 20 to 33 % of generation speed: the pin-don't-split rule in
  the templates. The fleet's version of "capacity vs communication" is already encoded:
  - one card first;
  - then a measured `--n-cpu-moe N` partial spill bounded by `n_cpu_moe_max` (ADR 0080, the RAM rule);
  - the display-card partial spill measured at N = 4, 8 and 14.

  Two audit gaps are noted, not built, because no template uses either flag: `-ot`/`--override-tensor`
  could move expert tensors to RAM outside `AuditSpill`, and `--no-kv-offload` moves KV and attention off the
  card. A template that wants either flag must close the gap first.
- **Dynamo disaggregation and NIXL: not applicable.** One process per seat, no RDMA.
- **KV-aware routing: not now.** The agent prompt puts the goal ahead of the documents, and the documents
  arrive as a setup replay after the objective. Two contracts on different pages, or different questions on
  one page, therefore share only the system prompt and tool specs (about 1,300 tokens). Only an identical
  contract re-run pays, and the node-local retention above serves that on the node that ran it. Routing by
  prefix affinity waits for a documents-first prompt order, which is its own change, judged by quality and
  by the reuse number above.
- **CMX (G3.5): already shipped, for vLLM.** The optional LMCache cache-server tier (ADR 0033, ADR 0045)
  is the commodity pattern: a shared store on a second device, fs_native or Valkey over Ethernet. Its key
  carries the model name, its key prefix is one per stack generation, and its success bar is the operator's:
  a restore at parity with recompute counts. llama.cpp slot files are portable between nodes serving an
  identical seat shape, but no shared slot directory is built until a single node's retention shows reuse
  worth sharing.
- **GPU-to-host offload during a tool wait: not built.** On a one-slot seat there is no contention to relieve,
  and `--cache-ram` is already the host tier inside the process.

### The ladder on each tier

All 19 tiers in `setup/templates/profiles.json`. The llama.cpp KV type is the tier's `kv_type`. G2 is
`--cache-ram` per RAM tier (min 1,024, low 8,192, mid 16,384, high 24,576 MiB). G3 is slot retention on every
one-slot llama.cpp chat or agent seat whose node runs a build at or above the floor.

| tier | engines | G1 KV | G2 host | G3 node-local | G3.5 shared |
|---|---|---|---|---|---|
| blackwell-16 | llama.cpp + vLLM (fp8, storeless) | q8_0 / fp8 | cache-ram; vLLM none | slot files | none |
| blackwell-32 | llama.cpp (resident) | q8_0 | cache-ram | slot files | none |
| blackwell-2x16 | llama.cpp + vLLM TP2 (fp8) | q8_0 / fp8 | cache-ram; LMCache L1 | slot files (par8 twin excluded) | LMCache fs_native, optional |
| blackwell-3x16 | llama.cpp + vLLM TP2 (fp8) | q8_0 / fp8 | cache-ram; LMCache L1 | slot files (par8 twin excluded) | LMCache fs_native, optional |
| blackwell-48 | llama.cpp (resident) | f16 | cache-ram | slot files | none |
| blackwell-72 | llama.cpp (resident) | f16 | cache-ram | slot files | none |
| ampere-16 | llama.cpp + vLLM (fp8_e5m2, storeless) | q8_0 / fp8_e5m2 | cache-ram; vLLM none | slot files | none |
| ampere-2x16 | llama.cpp (pair) | q8_0 | cache-ram | slot files | none |
| volta-16 | llama.cpp | q8_0 | cache-ram | slot files | none |
| ampere-8 | llama.cpp | q8_0 | cache-ram | slot files | none |
| blackwell-8 | llama.cpp | q8_0 | cache-ram | slot files | none |
| ampere-6 | llama.cpp (35B-A3B experts spilled) | q8_0 | 0 on the spill seat, by measurement | slot files: the only tier that survives a reload there | none |
| maxwell-3 | llama.cpp | f16 | cache-ram | slot files | none |
| amd-rdna3 | llama.cpp Vulkan | f16 | cache-ram | slot files | none |
| amd-rdna3-dgpu | llama.cpp Vulkan | q8_0 | cache-ram | slot files | none |
| amd-gcn | llama.cpp Vulkan | f16 | cache-ram | slot files | none |
| dual-gpu | llama.cpp (two cards) | q8_0 | cache-ram | slot files | none |
| cpu | llama.cpp (no card) | f16 | cache-ram | slot files | none |
| rockchip-rk3588 | RKLLM on the NPU | (NPU runtime) | cache-ram 1,024 on llama.cpp twins | no llama.cpp chat seat: none | none |

Vision and OCR seats send `cache_prompt: false` (llama.cpp #17200) and take no slot path. The embedder and
reranker run four slots and hold no chat KV.

## Consequences

- The ledger now answers "how much work we reuse" on every engine, which is the input any later prompt-order or
  routing change needs.
- A one-slot llama.cpp seat keeps a session's context across the idle unload and a media or lease unload, and a
  recurring prefix costs a restore instead of a prefill. On `<node-b>`'s fastest card a 3.3k-token prompt went
  from 1,256 ms to 664 ms restore included. On a slow-prefill seat, where the prefill takes tens of seconds, the
  same restore costs the same few hundred milliseconds.
- Disk: about 80 KB per token on the 9B with q8_0 KV (268 MB for 3,279 tokens). A full 32k window is about
  2.6 GB, so the default 8 GiB cap holds about three. Per-tier caps are set from a measurement on each agent seat,
  not from this figure.
- A restore overwrites slot 0. On a node running more than one job, the other run's KV is parked in `--cache-ram`
  and comes back on its next prefix match, so the cost is that run's restore rather than its prefill.
- A pre-floor llama.cpp build renders exactly what it rendered before.

## Alternatives considered

- Delegator-side slot keys (ADR 0056's original plan): rejected. The delegator never sees the recall block or the
  setup replay, so its key could not match the prefix the node sends.
- Saving on every step, or on first sighting: rejected for disk churn against a prefix that most often never
  recurs.
- A keep-alive ping during a long tool wait to dodge the idle unload: rejected. It keeps a model loaded without
  work, against the operator's 5-minute idle rule; the slot file achieves the same reuse with the card free.
- vLLM native CPU offload (`--kv-offloading-backend native`) on the storeless seats: not adopted here. It was
  measured unusable on the Mamba-hybrid under WSL2 and pathological under pipeline parallelism (ADR 0033).
- Dynamo KVBM, llm-d, Mooncake, LMCache P2P: Kubernetes-first, RDMA-first or bound to resident peers. They do not
  fit a fleet that installs on any PC and unloads idle seats after 300 seconds.

## Related code

- `internal/agent/prefillstats.go`, `internal/agent/client.go`: the reuse instrument and its usage fallback.
- `internal/servingtmpl/servingtmpl.go` (`slotSaveFlag`), `internal/servingtmpl/audit.go`, `setup/floors.json`,
  `setup/templates/llama-swap.*.yaml`: the render gate.
- `internal/agent/kvslot.go`, `internal/agent/loop.go`: run-start restore, presence admission, the tool-wait save.
- `internal/fleetnode/kvslots.go` and the shared slot core: the node lane and the LRU sweep.
- `internal/vllmseat/vllmseat.go`: the nvfp4 and cache-server refusal.

## Related docs

- [0056](0056-llamacpp-prompt-cache-tiers.md) (Layer 1 and Layer 2; amended 2026-11-08 with the measurement
  above), [0017](0017-kv-reuse-is-binary-and-how-we-measure-it.md) (reuse is append-only),
  [0033](0033-cache-server-is-an-optional-second-device-tier.md) and
  [0045](0045-a-cache-server-binding-per-vllm-seat.md) (the shared tier), [0048](0048-vllm-is-a-first-class-engine-on-every-tier.md),
  [0080](0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md), [0099](0099-frontier-updates-floors-never-pins.md).
- Upstream: ggml-org/llama.cpp PR #26004 and issue #25913; vllm-project/vllm PR #46329; LMCache MP docs.
