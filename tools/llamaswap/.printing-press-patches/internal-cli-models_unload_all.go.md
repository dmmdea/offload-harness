# Patch: `internal/cli/models_unload_all.go`

**Wave:** A (spine)

## What was changed

The generated mirror for `POST /api/models/unload` called the non-selective bulk
route directly. It was replaced with a per-model unload that excludes the
keep-set, plus the legacy `GET /unload` fallback for older builds.

## Why

The bulk route is not selective: on this box it takes the mem0 memory stack down
along with everything else. "Unload all" almost always means "unload all the
things I am allowed to unload".

## What a regen must preserve

1. **Keep-set members are excluded by DEFAULT.** The bulk route may only be
   reached when the caller explicitly overrides, and the output must say so.
2. The per-model loop — do not "optimize" it back into one bulk call. The bulk
   route cannot express an exclusion.
3. The `GET /unload` fallback on 404, for llama-swap builds predating the POST
   route.
4. Alias-aware keep-set matching (see the `models_unload.go` guard).
