# Patch: `internal/cli/helpers.go`

**Wave:** LS-1 (perfection wave — agent contracts, backlog row 3)

## What was changed

Two functions, both minimal:

**1. `writeAPIErrorEnvelope` now delegates.** The generated body was:

```go
if flags == nil || !flags.asJSON { return }
_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error(), "code": code})
```

It is now a one-line call to `emitAgentErrorEnvelopeOnce(flags, err, code)`
(hand-authored, `internal/cli/agent_errors.go`).

**2. `printJSONFiltered` marks stdout as claimed** by calling
`markStdoutDocument(flags)` before it writes.

## Why

**(1)** The generated envelope was wrong in both shape and coverage. Shape: an
agent got `{error, code}` — a sentence and a number, with the remediation glued
into the prose and no way to tell a retryable outage from a deliberate refusal.
Coverage: it fired on the HTTP-409 branch of `classifyAPIError` and nowhere
else, so the overwhelming majority of failures emitted no machine output at
all. Delegating keeps the symbol used (so the generated call site needs no
edit) while routing it through the single structured contract, and shares one
emission marker with the process-wide handler so exactly one envelope is
written per invocation.

**(2)** A command that already printed a result document and THEN exits
non-zero — `bench compare`'s not-comparable refusal is the canonical case —
would otherwise append a second top-level JSON document to stdout, and a
consumer doing one `json.Unmarshal` of the stream gets `Extra data`. Marking
the claim moves the envelope to stderr in that case. stdout is then always
exactly one document. This matches the semantics the comfyui twin shipped, so
the two CLIs behave identically for a shared agent.

## What a regen must preserve

1. `writeAPIErrorEnvelope` delegating to `emitAgentErrorEnvelopeOnce`.
   Guarded by `TestAgentEnvelopeIsEmittedExactlyOnce`, which calls it and the
   process-wide handler and asserts a single document.
2. The `markStdoutDocument(flags)` call in `printJSONFiltered`. Guarded by
   `TestEnvelopeMovesToStderrOnceAResultClaimedStdout`.
3. The envelope field names are a cross-CLI contract shared with the comfyui
   twin; `TestEnvelopeFieldNamesAreTheSharedContract` pins them.
