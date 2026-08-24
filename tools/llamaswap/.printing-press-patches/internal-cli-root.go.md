# Patch: `internal/cli/root.go`

**Wave:** LS-1 (perfection wave — agent contracts, backlog row 3)

## What was changed

One deferred call added at the top of `Execute()`, immediately before the
existing `defer finalizePlatformInvocation(&flags, &retErr)`:

```go
defer finalizeAgentErrorEnvelope(&flags, &retErr)
```

Nothing else in the file was touched.

## Why

`Execute()` is the ONLY point that sees every failure mode of an invocation.
Cobra/pflag usage errors (`unknown flag`, `unknown command`, `required flag not
set`) are raised inside `rootCmd.ExecuteC()` before any user `RunE` runs, so no
command-level hook can observe them; dial failures surface from arbitrary
depths; typed refusals come back as `*cliError`. Before this, all of them
produced a bare `Error: ...` line on stderr and nothing machine-readable —
`ps --agent` against a dead port printed plain text, which is the finding the
backlog raised (row 3).

Registered FIRST so it runs LAST (defers are LIFO), which guarantees the
envelope is the final document emitted and that the journal/platform
finalizers have already run.

The handler is a no-op unless the invocation asked for machine output
(`--json`/`--agent`, or those flags present in argv when a parse failure means
`rootFlags` was never bound). Human mode is byte-identical to before.

## What a regen must preserve

1. The `defer finalizeAgentErrorEnvelope(&flags, &retErr)` line, and its
   position BEFORE `defer finalizePlatformInvocation` so it runs after it.
   Guarded by `TestExecuteEmitsEnvelopeForACobraUsageError` and
   `TestExecuteEmitsEnvelopeForADialFailure`, which drive the real `Execute()`
   rather than `RootCmd()` precisely because this wiring is what they test.
2. The implementation lives entirely in the hand-authored
   `internal/cli/agent_errors.go`.

## Wave D addition (2026-08-24) — novel analytics command registrations

Two `addNovelCommandIfAbsent` lines were added in the novel-command block,
between `newNovelSwapsCmd` and `newNovelVerifyCmd`:

```go
addNovelCommandIfAbsent(rootCmd, newNovelResidencyCmd(flags))
addNovelCommandIfAbsent(rootCmd, newNovelSaturationCmd(flags))
```

A regen re-emits the block without them (they are hand-authored novel commands,
not generated endpoints). Preserve both — their bodies live in the hand-authored
`internal/cli/residency.go`, `internal/cli/saturation.go`, and shared
`internal/cli/analytics_common.go`. Registration is covered by the wave-D
acceptance tests (`analytics_verbs_test.go`), which run `RootCmd()` and would
fail if either command were absent.
