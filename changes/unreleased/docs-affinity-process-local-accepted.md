
### Changed - the model-affinity gate's per-process limit is recorded as accepted in ADR 0025, with where it stops and what reopens it

- **What was open.** ADR 0025 named the gate's limit (two harness processes on one box do not see each other's in-flight
  model) but never decided it, and the comment in `internal/modelaffinity/affinity.go` said "nothing here narrows that gap"
  after two machine-wide halves had landed beside the gate.
- **The decision.** The limit stays, on purpose. ADR 0025 gains an amendment dated 2026-10-14 that says what is per process
  (the drain of a base's in-flight batch and the batching of same-model requests), what is machine-wide today (the run cap
  and its line over the run registry, and the card fence that makes a text load wait for a media or exclusive lease; the
  seat-inflight markers are counted across processes but never gate), why accepting it is right now (the cross-process harms
  the box has shown are the machine-wide ones, closing the swap costs what ADR 0018 refused for text, and no measurement of
  cross-process switches is recorded), the trigger that reopens it (a measurement of processes with in-flight requests naming
  different models on one base at the same time, set against llama-swap's own model-load events, not a process count) and the
  shape of the change if it is reopened (the in-flight model set machine-wide and keyed by base, measured against the text
  budget first).
- **Also changed.** The `CROSS-PROCESS LIMIT` paragraph in `affinity.go` now says the same thing (comment only, no code), and
  invariant 6 of `docs/systems/offload-pipeline.md` states the scope of the gate.
- **Tests.** None: documentation and a comment only. The root package's docs, changelog and gofmt-sensitive gates and the
  `internal/modelaffinity` package were run on the branch.
