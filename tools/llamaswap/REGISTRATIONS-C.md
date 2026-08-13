# REGISTRATIONS-C — wave C (measurement)

Everything below wires itself through the generated extension points, so **no
edit to `internal/cli/root.go` is required**. The explicit `AddCommand` lines
are recorded anyway in case the project prefers making the wiring visible.

## New root commands

Registered from `internal/cli/measure_common.go` via the generated
novel-command hook (hooks run inside `newRootCmd` before the generated
`addNovelCommandIfAbsent` block, and registration is additive and
name-guarded, so this cannot collide with a sibling wave's hook):

```go
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newMeasureGgufCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureVramCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureFitCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureCtxCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureBenchCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureScratchCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureGateCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureBuildCmd(flags))
	})
}
```

If explicit registration is preferred instead, delete that `init()` and add
these beside the other novel commands in `newRootCmd`:

```go
	addNovelCommandIfAbsent(rootCmd, newMeasureGgufCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureVramCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureFitCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureCtxCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureBenchCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureScratchCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureGateCmd(flags))
	addNovelCommandIfAbsent(rootCmd, newMeasureBuildCmd(flags))
```

Subcommands are attached by their own parents, not by root:

| Parent | Child | Constructor |
|---|---|---|
| `bench` | `bench aux` | `newMeasureBenchAuxCmd` |
| `gate` | `gate grammar` | `newMeasureGateGrammarCmd` |
| `gate` | `gate tools` | `newMeasureGateToolsCmd` |
| `build` | `build check` | `newMeasureBuildCheckCmd` |

`verify` needed **no** registration change: root.go already calls
`addNovelCommandIfAbsent(rootCmd, newNovelVerifyCmd(flags))`. Wave C filled in
the generated scaffold's body in place (same file, same constructor name) and
added `--init`, `--expect-models`, `--keepset`, `--probe-each`,
`--embed-model`, `--rerank-model`, `--tolerance`, `--cosine-min` alongside the
scaffold's `--probe`.

No `.printing-press-patches/` entry is required: no generated file was
modified except the `verify.go` scaffold, whose header states the body is meant
to be implemented before shipping.

## Files added

| Path | Role |
|---|---|
| `internal/gguf/gguf.go` | Pure-Go GGUF header/metadata reader (never reads tensor data) |
| `internal/gguf/ftype.go` | `general.file_type` → quantization label table |
| `internal/gguf/gguf_test.go` | Asserted against real files on `V:\models` |
| `internal/measure/gpu.go` | nvidia-smi wrapper: per-UUID readings, role labels, deltas |
| `internal/measure/kv.go` | KV-cache arithmetic and the interval fit verdict |
| `internal/measure/hide_windows.go` / `hide_other.go` | Hidden-window child spawn (Windows `CREATE_NO_WINDOW`) |
| `internal/measure/gpu_test.go`, `kv_test.go` | Unit tests incl. the measured-reference check |
| `internal/cli/measure_common.go` | Shared plumbing + the registration hook (not a command) |
| `internal/cli/gguf.go` | `gguf` |
| `internal/cli/vram.go` | `vram` |
| `internal/cli/fit.go` | `fit` |
| `internal/cli/ctx.go` | `ctx` |
| `internal/cli/bench.go` | `bench` |
| `internal/cli/bench_aux.go` | `bench aux` |
| `internal/cli/gate.go` | `gate grammar`, `gate tools` |
| `internal/cli/scratch.go` | `scratch` |
| `internal/cli/build_check.go` | `build check` |
| `internal/cli/verify.go` | `verify` (scaffold implemented) |
| `internal/cli/measure_common_test.go`, `bench_test.go`, `scratch_test.go`, `verify_test.go` | Tests |

Nothing outside these files was touched: `root.go`, `go.mod`,
`internal/store/*.go`, `internal/lsconfig`, `internal/mirror`, and
`internal/fakeswap` are untouched by this wave. Stdlib only — no new
dependencies.

## Annotations

| Command | `mcp:read-only` | Why |
|---|---|---|
| `gguf`, `vram`, `fit`, `ctx`, `build check`, `verify` | `true` | read files / read endpoints; never load a model |
| `bench`, `bench aux`, `gate *`, `scratch` | *(absent)* | these load models or spawn a server, i.e. they mutate GPU state |

Every state-mutating command also short-circuits on `cliutil.IsVerifyEnv()`
(printing a plan instead of acting) in addition to honoring `--dry-run`.

`pp:data-source` markers: `auto` for `gguf` and `fit` (a path is a local read,
a model name needs live `/running`); `live` for the rest. Each command calls
`validateDataSourceStrategy` (or the equivalent explicit check) so an
incompatible `--data-source` is a clear usage error rather than a silent
mismatch.

## Typed exit codes used (all from `internal/cli/exitcodes.go`)

| Code | Const | Emitted by |
|---|---|---|
| 2 | usage | `scratch` refusing a non-allowlisted `--set` |
| 3 | `ExitModelNotFound` | `gguf`/`fit`/`ctx`/`scratch` on an unknown or unloaded model |
| 4 | `ExitServerUnreachable` | any command when the proxy does not answer |
| 23 | `ExitPortConflict` | `scratch` on a busy port or a port outside 18796-18799 |
| 24 | `ExitConfigInvalid` | `verify --expect-models` mismatch |
| 25 | `ExitDrift` | `build check` when loaded seats are on different llama.cpp builds |
| 26 | `ExitProbeFailed` | `verify --probe` outside tolerance; a keep-set member listed but not answering |
| 27 | `ExitUpstream5xx` | upstream 5xx surfaced through `mcClassify` |
| 28 | `ExitFitRefusal` | `fit` when the interval straddles capacity, or context is unknown |

## Store tables written (wave C owns these; no other table is written)

| Table | Written by |
|---|---|
| `bench_runs` | `bench` (one row per model per invocation, with `config_sha`/`llamaswap_version`/`build_info`), `bench aux` (rows tagged in `notes`) |
| `vram_snapshots` | `vram` (snapshot + `--baseline` rows, the latter tagged `model = '__baseline__'`), and per-UUID deltas measured around a `bench` |
| `ctx_probes` | `ctx` |
| `probe_baselines` | `verify --probe --init` (upsert on `kind+model+input_sha`) |

`store.EnsureDomainSchema` is called lazily on open; persistence failures warn
on stderr and never fail a measurement.

## CLI config keys consumed

Read from the CLI's own `config.json` (the same file `--config` selects), parsed
into a wave-local struct so the generated `internal/config` type stays
generator-owned:

```json
{
  "base_url": "http://127.0.0.1:11436",
  "gpu_roles": {
    "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7": "fast-card",
    "GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97": "utility-card"
  },
  "keep_set": ["embeddinggemma", "bge-reranker-v2-m3"],
  "probe_tolerance": 0.05
}
```

`gpu_roles` is optional — cards fall back to `name (short-uuid)` and the
command says so. `keep_set` is the default for `verify --keepset`; it is read
from config only, never from the server's `ttl` field (which reports `0` for a
`ttl:-1` model on this deployment). Any `base_url` spelled `localhost` or
`[::1]` is rewritten to `127.0.0.1` before use.
