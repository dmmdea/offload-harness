# Patch: `internal/mcp/platform_gate.go`

**Wave:** LS-2 (live-smoke findings — MCP code-orchestration outage)

**Upstream status:** **Not yet upstreamed.** This is a *generator-template*
defect, not a llamaswap one — the emitted `platform_gate.go` carries the wrong
branch on every printed CLI whose spec declares `auth: none`. Filed upstream on
`mvanhorn/cli-printing-press`; until that lands, a reprint of this tree silently
reverts the fix and takes the whole MCP orchestration surface down with it.

**Twin:** identical fix already shipped for `comfyui-pp-cli` in
`dmmdea/offload-harness` PR #109 (`0004-mcp-code-orch-gate-and-binary-budget.patch`,
0.54.1). The two files must stay semantically identical.

## What was changed

One branch inside `requireFreshTenantGate`. Where a `nil` session with a `nil`
error was rejected:

```go
if session == nil {
    return mcplib.NewToolResultError("MCP tenant gate is not configured"), nil
}
```

it now continues, ungated, with the reason recorded inline:

```go
if session == nil {
    // A nil session with a nil error means this CLI registers no
    // tenant-gated platform source, so there is no tenant to gate on.
    // ...
    return next(ctx, request)
}
```

Nothing else in the file was touched. The `err != nil` branch still denies, and
the `pp:tenant-gate=child-cli` bypass for Cobra mirrors is unchanged.

## Why

`cli.VerifyMCPInvocation` returns `(nil, nil)` on exactly one branch — its very
first statement:

```go
if registeredPlatformSource == nil {
    return nil, nil
}
```

That pair means *"this CLI registers no tenant-gated platform source, so there
is nothing to gate"*. Once a source **is** registered the function returns
either a verified session or an error, never `(nil, nil)`, so continuing on that
pair cannot skip a gate that exists. `cli.BindMCPClient` already reads the pair
that way for the typed client path; the middleware read it as a
misconfiguration and denied.

`llamaswap`'s spec is `auth: type: none`, so `registeredPlatformSource` is nil
by construction and **every** in-process tool hit that branch:
`llamaswap_search`, `llamaswap_get`, `llamaswap_execute` and both intents — the
entire code-orchestration surface — returned `MCP tenant gate is not configured`
instead of a payload. The 67 Cobra-mirror tools carry
`pp:tenant-gate=child-cli` and bypass the wrapper, so they kept working: the
outage presented as a partial failure, which is why no static gate caught it.

## What a regen must preserve

1. The `return next(ctx, request)` on the `session == nil` branch, with the
   comment explaining the `(nil, nil)` contract. A regen that restores the error
   return takes the MCP surface down again.
2. Guarded by two tests in `internal/mcp/platform_gate_test.go`:
   - `TestMCPFreshTenantGateAllowsCLIWithoutPlatformSource` — calls the **real**
     `cli.VerifyMCPInvocation` and drives the wrapper. This is the branch the
     two pre-existing gate tests structurally cannot reach: both replace
     `verifyFreshMCPInvocation` with a stub returning an error or a session, so
     neither ever sees `(nil, nil)`.
   - `TestMCPInProcessToolsReachTheirHandlersUngated` — drives all six **real**
     in-process handlers through the **real** wrapper, with nothing stubbed,
     and fails if any comes back as the gate's own refusal. Every tool is
     called with empty arguments so each short-circuits before issuing a
     request; the test is identical in duration against a dead base URL, so it
     needs no live server and is safe on a CI runner.

   Both were verified to FAIL with the fix reverted (`in-process tool blocked on
   a CLI with no platform source` / `context: in-process tool refused by the
   tenant gate`), so they are real guards rather than tests that merely pass.

## Not ported from the twin: the binary-budget half

Twin patch `0004` carries a second, independent fix — `codeOrchBinaryOversize`,
which refuses an oversized `_pp_binary` envelope in `handleCodeOrchExecute`
instead of letting the generic bounding truncate base64 mid-value.

**Not applicable here.** That fix exists because ComfyUI's `/view` serves file
bytes. `llamaswap`'s surface has no binary-payload path: all 27 code-orch
endpoints answer JSON or `text/*` (Prometheus exposition for
`activity.prometheus` and `upstream.metrics`, plain text for `logs.get`), and
`isBinaryResponseContentType` in `internal/client/client.go` returns `false` for
every JSON and `text/*` media type. No `_pp_binary` envelope can be produced by
this CLI, so `internal/mcp/bound` never sees one. Porting the guard would add
unreachable code and a test that could only assert against a fabricated
response. Re-evaluate if a future spec revision adds an endpoint that serves
bytes.

## Verification

- `go test -count=1 ./internal/...` green (whole tree).
- Live, against llama-swap v249 on `127.0.0.1:11436` with `embeddinggemma`
  already resident (no unloads): the rebuilt MCP binary answered
  `llamaswap_search` (found `upstream.props`), `llamaswap_get`
  (`server.version`), and `llamaswap_execute` on
  `GET /upstream/{model}/props` with `model=embeddinggemma` — the last also
  re-proving the positional path-placeholder binding on the wire. The same
  three calls against a binary built from the pre-fix source returned
  `isError=true`, `MCP tenant gate is not configured`, for all three.
