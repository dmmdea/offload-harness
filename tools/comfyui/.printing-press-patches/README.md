# Local patches to the generated tree

Deviations that live in this promoted copy of `comfyui-pp-cli` rather than upstream in the
Printing Press.

**Each entry here is a debt, not a feature.** A patch survives only until the next
`/printing-press-reprint comfyui` regenerates the tree, so every one of these must also be fixed
upstream — otherwise the reprint silently reverts it. Record the upstream status in the entry.

Keep this register byte-consistent with the vendored copy of the same CLI in
`dmmdea/offload-harness` under `tools/comfyui/.printing-press-patches/` (only the path prefixes in
the `.patch` files differ: this tree is the module root, that one is nested under `tools/comfyui/`).
Two copies drifting is how a fix gets re-lost.

| Patch | Fixes | Upstream status |
|---|---|---|
| `0001-cross-platform-host-path-handling.patch` | `internal/comfy/media` was Windows-only | **Not yet upstreamed** — see below |
| `0002-mcp-code-orchestration.patch` | MCP shipped 13 endpoint-mirror tools; no tool declared an `outputSchema` | **Not yet upstreamed** — see below |
| `0003-structured-error-envelope.patch` | `root.go` + `helpers.go` had no machine-readable failure path | **Not yet upstreamed** — see below |
| `0004-mcp-code-orch-gate-and-binary-budget.patch` | the tenant gate rejected every in-process MCP tool on a CLI with no platform source; an oversized binary body was truncated into corrupt base64 | **Not yet upstreamed** — see below |

---

## 0001 — cross-platform host-path handling

**Applied:** 2026-08-13. Originated in the vendored copy (`dmmdea/offload-harness` PR #101,
commit `b2cfd06`), back-ported here so the promoted artifact and the vendored one carry identical
bytes.

### What was wrong

`StagedName` and `ValidateComfyFilename` both handle *host* paths — foreign input that may have been
written on a different operating system than the one reading it. Both delegated to Go's `filepath`
package, which honors **only the running OS's separator**:

- `filepath.Base(` `` `D:\refs\portrait.png` `` `)` returns the whole string on Linux, so the staged
  name became `D_refs_portrait-<sha>.png` instead of `portrait-<sha>.png`.
- `filepath.ToSlash` is a no-op on Linux, so `\\server\share\x.png` kept its backslashes and the UNC
  and `..` traversal checks never fired.

### Why it mattered beyond a red test

Two real defects, not cosmetic ones:

1. **`StagedName`'s core guarantee broke across a mixed fleet.** Its own doc comment calls the
   content hash "load-bearing": identical bytes must stage under one name, or an archived run's
   provenance splits. On Linux, the same file referenced by a Windows-style path produced a
   *different* name than on Windows — the exact collision the design set out to prevent.
2. **`ValidateComfyFilename` failed OPEN on every non-Windows node.** Its whole job is to reject host
   paths (absolute, drive-qualified, UNC, `..` escapes) before they reach `LoadImage`. On Linux it
   silently stopped rejecting UNC and backslash-separated traversal.

This was invisible until the vendoring, because the CLI was generated, verified, and scored on
Windows only. Running it on an `ubuntu-latest` CI runner is what surfaced it.

### The fix

Handle both `/` and `\` explicitly, independent of `runtime.GOOS`:

- new `hostBase()` replaces `filepath.Base(filepath.FromSlash(...))` in `StagedName`, splitting on
  either separator and stripping a `D:`-style drive qualifier;
- `ValidateComfyFilename` normalises separators with `strings.ReplaceAll(trimmed, "\\", "/")`
  instead of `filepath.ToSlash`.

Behavior on Windows is unchanged — `filepath` already did this there. Only the non-Windows path
changes, from wrong to correct.

Also in this patch: `media_test.go` had a fixture path containing a real local username, replaced
with `/home/user/...`. The vendored copy of this tree is public and its style rules forbid
filesystem paths that leak an identity — in code as well as docs. Keeping the fixture identical in
both copies is what stops the next sync from reintroducing it.

### Verification

`go test ./internal/comfy/media/ -count=1` green on Windows. The Linux half of the guarantee was
proven in the vendored copy by cross-compiling the test binary and running it under Linux
(`uname -s` = `Linux`): `TestStagedName/windows_path`, `TestValidateComfyFilename/unc_path`,
`TestValidateComfyFilename/unix_absolute`, and the identical-content-one-name assertion all pass
there. These two files are byte-identical between the copies, so that result carries.

### Upstream

**Still owed.** The durable fix belongs in the CLI Printing Press generator/templates that emitted
these two functions, so the next `/printing-press-reprint comfyui` carries it instead of reverting
it. Until that lands, re-apply this patch after any reprint and re-run the Linux check.

---

## 0002 — MCP code orchestration + declared output schemas

**Applied:** 2026-08-13. Two changes that share one file (`internal/mcp/tools.go`) and therefore one
patch. The sibling `llamaswap-pp-cli` already prints in code-orchestration mode, so the shape here
is a port of that tree's `internal/mcp/code_orch.go`, not an invention.

### What was wrong

**1. The MCP surface mirrored one tool per endpoint.** `RegisterTools` registered 13 typed
endpoint tools whose schemas an agent pays for on every single request, before it knows which one
it needs. The Printing Press already supports the alternative — a `search` / `get` / `execute`
trio over an in-binary endpoint registry, the shape Anthropic documented for MCP servers covering
thousands of endpoints — and `spec.yaml` simply never declared it.

**2. No tool declared an `outputSchema`.** Every result was untyped text, so an MCP host had to
re-parse free-form JSON to learn what came back.

### The fix

`spec.yaml` gains an `mcp:` stanza (`orchestration: code`, `endpoint_tools: hidden`) and
`tools-manifest.json` the matching `mcp` block — these are the generator inputs, so a reprint that
honours them emits this mode natively instead of reverting to endpoint mirrors. In the tree,
`internal/mcp/code_orch.go` carries the registry and the three handlers, and `RegisterTools`
calls `RegisterCodeOrchestrationTools(s)` where the 13 `s.AddTool(...)` blocks used to be. The
endpoint summaries are copied verbatim from the descriptions those blocks carried; they encode
findings that cost real render time to learn (COMBO options live at input-tuple index 1, an empty
options list means an unregistered model *class* rather than a missing file, `/history` timestamps
are the only honest duration source) and paraphrasing them away would be the actual regression.

`internal/mcp/output_schema.go` declares the three output schemas, attached with
`mcplib.WithRawOutputSchema`, and the handlers now return `mcplib.NewToolResultStructured` so the
declared schema is backed by real `structuredContent` rather than advertised and unfulfilled.

**One deliberate divergence from the sibling.** llamaswap's registry leaves `Positional` empty for
every endpoint, including the ones whose paths carry `{model}`. `handleCodeOrchExecute` substitutes
a `{name}` segment only if that name is listed in `Positional`, so those endpoints ship the literal
template to the server. This tree populates `Positional` (`prompt_id`, `class_type`, `file`) and
`QueryParams` (`max_items`, `overwrite`, `filename`/`subfolder`/`type`) for real.
`TestCodeOrchPositionalCoversEveryPathPlaceholder` pins the invariant so a reprint that reintroduces
the empty slices fails instead of 404-ing at runtime. **The sibling still carries the bug** — fixing
it there is a separate, owed change.

### Not covered, and why

The Cobra command-mirror tools (`wait`, `validate`, `nodes options`, …) get no `outputSchema`.
`cobratree.shellOutToCLI` returns the companion CLI's stdout as free text and emits no
`structuredContent` at all, and that stdout's shape depends on flags the *caller* passes
(`--json` / `--agent`), not on the tool definition. Declaring a schema there would advertise a
contract the handler cannot keep. Changing that means making the mirror force a machine-readable
mode and parse it — a change to shared generated machinery affecting every mirrored command, which
belongs upstream rather than in a local patch.

### Verification

`go build ./internal/mcp/...`, `go vet ./internal/mcp/...`, and `gofmt -l internal/mcp/` clean;
`go test ./internal/mcp/... -count=1 -shuffle=on` green. The golden tests were proven non-vacuous
by injecting drift in both directions and confirming a red test each time: renaming a property in
`output_schema.go` fails the advertised-vs-committed comparison, and renaming a key in
`codeOrchEndpointMetadata` fails the payload-conformance check against the committed schema.
`TestCommittedSchemaRejectsNonConformingPayload` is the standing negative control for the second.

`-race -shuffle=on -count=1` was subsequently run green across all 17 packages using the MinGW-w64
toolchain on the authoring box (`CGO_ENABLED=1` with `gcc` on PATH). The race detector needs cgo
and a C compiler, which is not on PATH by default here — that is a local toolchain condition, not
a property of this repo, and CI runs the race target on Linux regardless.

### Upstream

**Still owed, in two parts.** (a) The `spec.yaml` / `tools-manifest.json` `mcp` stanzas are
generator inputs — a reprint that reads them should emit `code_orch.go` and the swapped
`RegisterTools` on its own, at which point most of this patch stops being a deviation. (b) The
`Positional` fix and the output-schema wiring belong in the generator's `code_orch.go` template,
and the same `Positional` fix is owed to `llamaswap-pp-cli`. Until both land, re-apply this patch
after any reprint.

---

## 0003 — structured error envelope on every failing exit

**Applied:** 2026-08-13, as part of the CU perfection wave (backlog row 3).

### What was wrong

A failing invocation gave a machine caller nothing machine-readable. Only the HTTP-409 branch of
`classifyAPIError` emitted anything structured, and it emitted `{error, code}` — a prose string and
an int. Everything else reached the caller as a bare `Error: …` line on stderr, printed by Cobra:
every typed domain exit, every dial failure, and every usage error. An agent had to string-match
prose to decide whether to retry, fix its arguments, or give up.

The worst case was the flag-parse path. `--agent` only implies `--json` inside `PersistentPreRunE`,
which never runs when flag parsing fails — so a malformed invocation under `--agent`, the single
most common machine failure, was the one case guaranteed to return bare prose.

### The fix, and why it needs generated files

Three surgical edits, all of which must live in generated files because there is no seam for them:

1. **`root.go` — `rootFlags.structuredWritten`.** A new field; there is no way to extend a struct
   from another file in Go. Records that a structured document already went to stdout, so the error
   envelope can pick stderr instead of appending a second JSON document.
2. **`root.go` — `SilenceErrors: true` + a deferred `finalizeErrorOutput`.** Cobra prints errors
   from inside `ExecuteC`, so suppressing that is the only way to have ONE error-reporting site.
   The defer is registered FIRST so it runs LAST (LIFO) and observes the error the caller actually
   receives, after `finalizePlatformInvocation` has had its chance to replace it. The now-redundant
   explicit hint print on the unknown-flag path was removed in the same edit: the hint is already
   wrapped into `err`, so printing it separately duplicated it.
3. **`helpers.go` — the envelope shape and the two output-funnel marks.** `writeAPIErrorEnvelope`
   now emits the same document as `finalizeErrorOutput` instead of the old `{error, code}` pair.
   `printOutputWithFlagsMeta` and `wrapPlatformStructuredOutput` set `structuredWritten`; the mark
   in `wrapPlatformStructuredOutput` sits BEFORE its nil-session early return on purpose, because
   the mark tracks "a document is going to stdout", which is true either way. Over-marking is the
   safe direction — it moves the envelope to stderr — whereas under-marking puts two JSON documents
   on stdout.

`classifyAPIError` also gained two typed classifications in the same file: a statusless failure
naming a dead socket now returns `ExitServerUnreachable` (4) instead of the generic API code 5, and
a 5xx returns `ExitUpstream5xx` (26). Without those the envelope would have said
`code: "server_unreachable"` while the process exited 5 — the classification and `$?` disagreeing
is precisely the ambiguity the row set out to remove.

All the logic lives in the hand-authored `internal/cli/errenvelope.go` and
`internal/cli/exitcodes.go`; the generated files only gain the wiring.

### Verification

`gofmt -l .` clean, `go build ./...` and `go vet ./...` clean, and
`go test ./... -count=1 -race -shuffle=on` green across all 17 packages. `errenvelope_test.go`
pins the wire contract: the exact seven field names, the code tokens, and the `retryable`
judgement per exit code. Live-checked against the real binary with the server down —
`--agent nosuchcommand` → envelope with `exit_code: 2`; `queue get --agent` against a dead port →
`code: "server_unreachable"`, `retryable: true`, `exit_code: 4`; human mode still one prose line.

### Upstream

**Still owed.** The envelope is a Printing Press-wide concern, not a ComfyUI one: the same three
edits are needed in the generator's `root.go` and `helpers.go` templates, and the sibling
`llamaswap-pp-cli` ships the identical field names. Until that lands, re-apply this patch after
any reprint — and keep the field names byte-identical across the twins, because an agent that
learns one is expected to read the other without a second parser.

---

## 0004 — MCP code-orchestration: tenant gate and binary budget

**Applied:** 2026-08-13. Both defects were found by the first live run of the deferred smokes in
`LIVE-SMOKES-DEFERRED.md` against a real ComfyUI 0.32.0 server. Neither is reachable from the
fake-server suite, which is why 0002 shipped with them.

### What was wrong

**1. The tenant gate rejected the entire code-orchestration surface.**

`installFreshTenantGate` wraps every registered tool. Cobra mirrors carry `pp:tenant-gate:
child-cli` in their meta and pass straight through, so the 56 mirrored commands kept working. The
three in-process tools — `comfyui_search`, `comfyui_get`, `comfyui_execute` — do not, so each call
went through `cli.VerifyMCPInvocation`, whose first branch is:

```go
if registeredPlatformSource == nil {
    return nil, nil
}
```

That pair means "this CLI has no tenant-gated platform source, so there is nothing to gate", and
nothing ever registers a source for `comfyui-pp-cli`. The wrapper read the nil session as a
misconfiguration and returned `MCP tenant gate is not configured` — every time, for every argument.
`cli.BindMCPClient` reads the same pair the opposite way and proceeds, so the gate was stricter than
the binder it exists to protect.

The failure mode is what hid it: a partial outage. `tools/list` advertised all 59 tools with
correct `outputSchema`s, the cobra mirrors answered normally, and only the three tools 0002 was
written to add were dead.

**2. An oversized binary body was truncated into corrupt base64.**

The client layer base64-wraps non-textual bodies, so `/view` arrives as a `_pp_binary` envelope.
A real render is megabytes, and base64 adds a third: a 2.7 MB PNG becomes a 3.6 MB envelope against
a 60 000 byte budget. `bound.EndpointResponse` then applied its generic fallback and returned a
`preview` holding the base64 string cut mid-value — it no longer parses, and an agent that decoded
it anyway would write a corrupt file. The attached note advised narrowing the request "with filters,
search/sql, or a command-mirror tool with --agent/--compact/--select", none of which can shrink an
image. The typed endpoint-mirror path already refused this case with an actionable message; the
code-orchestration path silently returned damage.

### The fix

- `platform_gate.go` — `requireFreshTenantGate` continues to the handler on `(nil, nil)`. This
  cannot skip a real gate: once a platform source is registered, `VerifyMCPInvocation` returns
  either a verified session or an error, never that pair.
- `code_orch.go` — new `codeOrchBinaryOversize` runs before `bound.EndpointResponse` and refuses a
  `_pp_binary` envelope that exceeds the budget, naming the route to the bytes
  (`comfyui-pp-cli view … --deliver file:<path>`, then decode the envelope's base64 `data` field —
  the CLI writes the envelope, not raw bytes, and the message says so).

### Why the tests missed both

`TestMCPEveryRegisteredToolHasFreshTenantGate` and `TestMCPTypedInvocationUsesSingleFreshTenantGate`
both replace `verifyFreshMCPInvocation` with a stub returning an error or a session. Neither
exercises what the real function returns when no platform source is registered, which is the only
configuration this CLI ever ships in. No fake-server test served a binary body at all.

Three tests close the gap, each verified to fail with its fix reverted:
`TestMCPFreshTenantGateAllowsCLIWithoutPlatformSource` calls the real `cli.VerifyMCPInvocation`
rather than a stub; `TestCodeOrchExecuteBinaryBodyRoundTripsAsBase64` asserts a small binary
survives byte-for-byte; `TestCodeOrchExecuteOversizedBinaryRefusesInsteadOfTruncating` asserts the
oversized one is refused with a usable route instead of previewed.

### Verification

`go build ./...` and `go vet ./...` clean, `go test ./... -shuffle=on -count=1` green across all 17
packages. Live against ComfyUI 0.32.0: `comfyui_execute {endpoint_id: "system.stats"}` returns
`path: "/system_stats"` with per-device vram; `{endpoint_id: "objectinfo.get", params: {class_type:
"VAELoader"}}` returns `path: "/object_info/VAELoader"` with a populated option list, and
`{endpoint_id: "history.get", params: {prompt_id: <real>}}` returns `path: "/history/<real>"` with
`execution_start` and `execution_success` — the placeholder substitution proven on two different
parameters. `view.get` against a 4 196-byte input returns `_pp_binary: true`, `encoding: "base64"`,
and data that decodes to a sha256 identical to the staged source; against a 2.7 MB render it now
refuses with the byte counts and the CLI route.

### Upstream

**Still owed**, and the gate half is urgent for every printed CLI: any CLI generated without a
tenant-gated platform source ships a dead code-orchestration surface. Both edits belong in the
generator's `platform_gate.go` and `code_orch.go` templates. Re-apply this patch after any reprint
until that lands.
