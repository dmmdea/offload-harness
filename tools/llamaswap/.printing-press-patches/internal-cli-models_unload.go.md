# Patch: `internal/cli/models_unload.go`

**Wave:** A (spine)

## What was changed

The generated endpoint mirror for `POST /api/models/unload/{model}` was an
unguarded fire-and-forget call. It was replaced with:

- alias resolution against `meta.llamaswap.aliases` from `/v1/models`;
- keep-set refusal (default on, `--force-keepset` to override);
- `--drain` / `--drain-timeout`, which fail CLOSED;
- an `unload_provenance` ledger row per attempt;
- typed exit codes 3 / 20 / 21 / 22 / 4.

## Why

`models unload local-embed` on the generated command took the mem0 memory stack
down without a word. Every guard here corresponds to a failure that has really
happened on this class of deployment.

## What a regen must preserve

1. **Keep-set refusal, matched by ALIAS as well as canonical id.** The memory
   stack is routinely addressed as `text-embedding`, `local-embed`,
   `reranker-v2-m3`, `v0.12-reranker`. An id-only check lets every one of those
   through.
2. **The refusal fires even when the target resolves nowhere in the roster.**
   Protection must not depend on the roster being readable.
3. **The drain check fails CLOSED.** Unreadable slot state (timeout or 5xx)
   unloads NOTHING and returns exit 22. A 404 from `/slots` is endpoint-ABSENT,
   not unobservable, and takes the documented activity-ring fallback instead.
4. **No `/upstream` probe for a model absent from `/running`** — that would
   auto-start it, i.e. loading a model in order to decide whether to unload it.
5. The typed exit codes, and validation living in `RunE` rather than in
   `cobra.Args` / `MarkFlagRequired` so `--dry-run` can short-circuit first.
