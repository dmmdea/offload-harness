# Patch: `internal/cli/verify.go`

**Wave:** C (measurement)

## What was changed

The generated scaffold's body was implemented in place, and the scaffold's lone
`--probe` flag was joined by `--init`, `--expect-models`, `--keepset`,
`--probe-each`, `--embed-model`, `--rerank-model`, `--tolerance`, `--cosine-min`.

`verify` now: counts the roster, asserts every keep-set member is ANSWERING (not
merely listed), and optionally embeds a fixed string and reranks a fixed pair,
asserting both against baselines stored in `probe_baselines`.

## Why

Counting models after a restart proves nothing. The failure that has actually
occurred here is a seat that starts, lists, and answers — with a dropped flag
(`--pooling mean`) that silently degrades the embeddings. Only a calibrated
probe detects that class.

## What a regen must preserve

1. The whole body and all nine flags.
2. **"Listed" is not "resident" is not "answering".** The keep-set check must
   assert the third.
3. Exit 26 (`ExitProbeFailed`) for a probe outside tolerance, and exit 24 for an
   `--expect-models` mismatch. These are the codes an unattended post-restart
   check branches on.
4. Baselines are RECORDED by `--probe --init` and only compared otherwise; a
   regen must not make comparison auto-seed its own baseline, which would make
   every run pass.
