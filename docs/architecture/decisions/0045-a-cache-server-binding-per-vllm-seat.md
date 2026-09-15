---
status: Accepted
date: "2026-09-14"
---

# 0045 — A cache-server binding per vLLM seat

Release: 0.121.0 (register B-01)

## Context

[ADR 0033](0033-cache-server-is-an-optional-second-device-tier.md) established the cache server as an
optional second-device KV tier and gave it one config block:

```json
"kv_cache_server": {
  "enabled": true, "store": "fs_native",
  "address": "/mnt/kvcache/lmcache-seat-tp2-fp8",
  "chunk_size": 1568, "key_prefix": "qube-seat-tp2-fp8",
  "seat": "qwen3.8-27b-vllm"
}
```

One block, one `seat`. That was right while a box ran one vLLM seat, and wrong the moment one did
not. The reference workstation runs the tensor-parallel pair AND an opt-in 3-card layout; the
second box runs its own small seat. A store can only be named for one of them, and the shape gives
the others nothing to be: a seat with no tier and a seat nobody thought about produce the same
config, the same `offload_status`, and the same silence.

The operator directive of 2026-09-10 removed the ambiguity in the other direction: *the second
device's store backs EVERY vLLM seat whenever that device is online* — the pair, the 3-card layout,
and the second box's own seat — and a config that binds `kv_cache_server` to a single seat name is a
DEFECT, not a configuration. The defect was recorded against the deployed Qube config, which carries
exactly the block above.

Two constraints shape what replaces it:

- **Only vLLM seats are candidates.** The cascade stays on llama.cpp: Gemma-4 hybrids crash
  LMCache's V2 runner path (upstream LMCache #4263). So the gate needs a roster of *vLLM* seats, and
  `/v1/models` cannot supply one — it reports model ids, not engines, and a sniffed roster would
  fail every llama.cpp seat that must never have a store.
- **One namespace per stack generation** (register B-45, standing practice since the 2026-09-02/03/04
  `qube-s3c` → `qube-seat` → `qube-seat-v7` → `qube-seat-tp2-v1` sequence). Pages written by another
  engine layout are not stale, they are unreadable: LMCache fails the read with `value size exceeds
  buffer capacity` and the tier serves nothing while reporting success. Two seats sharing a
  `key_prefix` across layouts is that failure, pre-installed.

## Decision

`kv_cache_server` is a LIST of per-seat bindings, and the box declares its vLLM roster.

1. **A list, with the old shape still accepted.** `KVCacheServers.UnmarshalJSON` takes either the
   list or the pre-0.121 single object; the object decodes to a one-element list bound to its own
   `seat`. Nothing already deployed has to change to keep loading. A binding whose `seat` is empty
   is the BOX DEFAULT and backs every vLLM seat that has no binding of its own; an exact seat match
   always wins over it.
2. **`vllm_seats` is the box's vLLM roster** — declared, not sniffed, for the reason above. Empty
   (the default, and every llama.cpp box in the fleet) means the gate has no subject and is inert.
3. **Validation refuses what cannot work.** Two bindings for one seat — including two box defaults —
   is not a merge but two stores whose order in the file decides the winner. A `key_prefix` shared
   by two seats must be shared deliberately: every binding on it declares the same stack generation
   (`kv_dtype` + `tensor_parallel`), and an *undeclared* generation on a shared prefix is refused
   too, because it cannot be shown to be safe and "probably fine" is how that one shipped.
4. **`doctor` fails a storeless seat**, one line per seat, non-zero exit. The escape hatch is a
   DECLARATION, never silence: `{"seat": "…", "storeless": true, "reason": "…"}` passes, an absent
   binding fails, and a binding merely switched off with no reason fails — "the tier is off here"
   and "nobody considered this seat" must not look alike. The section prints above the health probe
   because its verdicts are pure config, the same reasoning that moved the media section there.
5. **The render reads its binding by seat name.** `install vllm-seat --config <config.json>` lets
   the deployment supply the adapter, directory, namespace, L1 size and chunk for THIS seat, while
   the tier keeps what only the tier knows — the mount point, the write floor, the prune target, the
   writer count, the cap. So two seats on one box each carry their own store directory and
   namespace without either one being edited into a hardware tier.
6. **`offload_status.kv_cache_server` lists every binding** plus `unbound_seats`, computed by the
   same `UnboundSeats` the gate fails on, so the report and the gate cannot disagree.

A tier that renders a vLLM seat now seeds `vllm_seats` and a binding for it — a store when the tier
declares one, an explicit storeless opt-out when it does not — because a fresh install must not ship
a config its own `doctor` rejects.

## Consequences

- The tier stays OPTIONAL and off by default (the invariant ADR 0033 set). A box with no
  `vllm_seats` gains no section, no key and no failure.
- An operator running two vLLM seats can give each its own store without touching a tier profile.
- A seat deliberately run without a store now costs one line of config and one sentence of reason.
  That is the point: the cost of the exception is saying why.
- `key_prefix` is still not rendered into `seat.env` — for `fs_native` the store DIRECTORY is the
  live namespace, and `key_prefix` is what the config block and `offload_status` publish. The two
  must be moved together; the generation fields exist so the refusal can say when they were not.
- The live Qube and Lenovo configs still carry the old single object. They load unchanged; migrating
  them to per-seat bindings is a deployment step, taken in a GPU window with the H-24 soak any
  pair-seat env change requires.
