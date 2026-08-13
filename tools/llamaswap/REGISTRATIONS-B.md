# REGISTRATIONS-B — Wave B (config surface)

What Wave B added, how it wires itself in, and what the other waves need to
know. Nothing here requires an edit to `internal/cli/root.go`.

---

## 1. Command registration — no root.go edit required

`root.go` exposes `registerNovelCommand(func(root *cobra.Command, flags *rootFlags))`,
a hook slice drained inside `newRootCmd` before the generated
`addNovelCommandIfAbsent` calls. Wave B registers both of its top-level
families from an `init()` in `internal/cli/config_root.go`:

```go
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newConfigCmd(flags))
		addNovelCommandIfAbsent(root, newBindCmd(flags))
	})
}
```

A `printing-press generate --force` regen therefore refreshes `root.go`
without dropping these commands.

`seat` was already registered by the generator
(`addNovelCommandIfAbsent(rootCmd, newNovelSeatCmd(flags))`, root.go:402).
Wave B filled in `internal/cli/seat.go` to attach three subcommands instead of
one.

### Command tree added

| Path | File | data-source | MCP annotation | Typed exits |
|---|---|---|---|---|
| `config` | config_root.go | local | `mcp:read-only` | 0,2 |
| `config validate [file]` | config_validate.go | local | `mcp:read-only` | 0,2,24 |
| `config lint [file]` | config_lint.go | local | `mcp:read-only` | 0,2,24 |
| `config explain <model>` | config_explain.go | local | `mcp:read-only` | 0,2,3,24 |
| `config diff <a> [b]` | config_diff.go | local | `mcp:read-only` | 0,2,24 |
| `config drift` | config_drift.go | **live** | `mcp:read-only` | 0,2,4,24,25 |
| `config backup` | config_backup.go | local | **`mcp:local-write`** | 0,2,24 |
| `config testinstance [file]` (alias `test`) | config_testinstance.go | computed | **`mcp:local-write`** | 0,1,2,23,24 |
| `config apply <file>` | config_apply.go | local | `mcp:read-only` | 0,2,24 |
| `seat log <model>` | seat_log.go | local | `mcp:read-only` | 0,2,3,24 |
| `seat show <model>` | seat_show.go | **live** | `mcp:read-only` | 0,2,3,4,25 |
| `seat try <model>` | seat_try.go | computed | `mcp:read-only` | 0,2,3 |
| `bind check` | bind_check.go | **live** | `mcp:read-only` | 0,2,3,4 |

`config backup` and `config testinstance` are the only two that are not
`mcp:read-only`, because they are the only two with side effects: new files and
a spawned process. Both also short-circuit on `PRINTING_PRESS_VERIFY=1` and on
`--dry-run`, so a verify pass never writes into an operator's config directory
and never spawns anything.

---

## 2. New package: `internal/lsconfig`

Read-only config intelligence. Files: `doc.go`, `parse.go`, `expand.go`,
`classify.go`, `corpus.go`, `validate.go`, `lint.go`, `diff.go`,
`schema/config-schema.json`, `schema/README.md`, `lsconfig_test.go`.

Exported surface other waves can rely on:

```go
lsconfig.DefaultConfigPath() (string, error)   // LLAMASWAP_YAML, then candidates
lsconfig.Load(path, LoadOptions) (*File, error)
lsconfig.ParseBytes(raw, path, LoadOptions) (*File, error)

(*File).Resolve(name) (*Model, bool)           // id OR alias
(*File).Models []*Model, .ModelIndex, .Macros, .Matrix, .StartPort, .Sha256
(*Model).CmdRaw / .CmdExpanded / .Seat / .Binary / .TTL / .Aliases
(*Model).RawBlock() string                     // verbatim source, comments included

lsconfig.ClassifySeat(binary) SeatKind         // SeatLlamaServer | SeatNonLlamaServer
lsconfig.ParseCmd(cmd) CmdSpec                 // .Binary, .Flags, .Get("-ngl")
lsconfig.TokenizeCmd(cmd) []string
lsconfig.IsFlagToken(tok) bool
lsconfig.DiffCmds(a, b) []FlagDelta            // port-normalized
lsconfig.DiffConfigs(a, b) *ConfigDiff
lsconfig.UnifiedDiff(aName, bName, a, b, ctx) string
lsconfig.Validate(*File) (*ValidationResult, error)
lsconfig.Lint(*File, LintOptions) *LintReport
lsconfig.DiscoverCorpus(configPath, DiscoverOptions) (*Corpus, error)
```

### Two contracts other waves must not break

**1. The YAML is read-only, forever.** There is no `Marshal` path in
`lsconfig` and no writer that points at a config file. If a later wave needs to
change the file, it does NOT add `yaml.Marshal` — it builds a byte-range splice
engine that refuses on comment-count or untouched-region change. The reason is
in `doc.go`.

**2. `SeatKind` is the escape hatch — check it before any llama-server
assumption.** Any check that assumes llama.cpp semantics (GGUF headers, `-ngl`,
`-c`/`--ctx-size`, KV cache types, `--jinja`) must call
`ClassifySeat`/read `Model.Seat` first and SKIP a `SeatNonLlamaServer` seat with
an explicit note. Helpers exist so this is easy:

```go
lsconfig.FileFlagsFor(kind, binary) []string   // -m/--mmproj/-md vs -m/-vm
lsconfig.ContextFlagsFor(kind) []string        // nil for non-llama-server
```

On the reference deployment `whisper-stt` runs `whisper-server.exe` with a
`.bin` model. Wave C's `fit`/`ctx`/`gguf` commands and any `doctor` check must
skip it rather than report it as broken. Verified: `config lint` on the real
live config produces **zero** errors and **zero** warnings on that seat, only
two explicit `skipped` notes.

---

## 3. Store tables written (Wave B owns these two only)

| Table | Written by | Idempotence |
|---|---|---|
| `seat_config_history` | `seat log` | `ON CONFLICT(content_sha, model) DO NOTHING` — re-running does not duplicate the timeline |
| `bindings_audit` | `bind check` | append-only, one row per binding per run |

Both call `store.EnsureDomainSchema(ctx, s.DB())` before writing and open via
`store.OpenWithContext(ctx, defaultDBPath("llamaswap-pp-cli"))`. Neither touches
any other wave's table. `--no-record` and verify mode suppress both.

`seat_config_history` is the join key Wave C wants: `cmd_sha` is a
sha256-16 of the seat's expanded command line, so a bench row recording the
same value can be joined to the exact seat configuration that produced it.

---

## 4. Dependencies added to `go.mod` (Wave B is the only wave permitted to)

```
gopkg.in/yaml.v3 v3.0.1                        // Node API: comments + line positions
github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // was already in go.sum, now direct
```

`go mod tidy` promoted both from `// indirect` to a direct require block and
added `gopkg.in/check.v1` hashes to `go.sum` (yaml.v3's test dependency). No
other module lines changed.

---

## 5. Shared helpers added in `package cli` (avoid redefining these)

Defined in `config_root.go` unless noted. Other waves should reuse rather than
re-declare, or the package will not compile:

| Symbol | Purpose |
|---|---|
| `theTrustContract` | the read-only-YAML statement printed by `config --help` and the `--write` refusal |
| `resolveConfigPath(args, idx)` | positional-or-default config path |
| `loadConfigFile(path)` | parse, mapping failure to `ExitConfigInvalid` |
| `loopbackBaseURL(raw)` | rewrites `localhost` → `127.0.0.1` |
| `newLoopbackClient(flags)` | the generated client with that rewrite applied |
| `fetchRunning(ctx, flags)` | `GET /running`, cache-disabled, typed `[]runningSeat` |
| `fetchRosterAliases(ctx, flags)` | `GET /v1/models` → entries + `name→canonical id` map |
| `wantsJSON(w, flags)` | the machine-output gate |
| `severityMark`, `fprintBlock`, `dashIfEmpty` (config_diff.go) | human rendering |
| `shortSha(s)` | sha256-16 of a string |
| `errConfigInvalid/errDrift/errPortConflict/errModelNotFound` | typed exit wrappers |
| `verifyPlan(w, flags, action, plan)` | print-and-stop for side-effecting commands |
| `cliutilIsVerifyEnv` | test-overridable `cliutil.IsVerifyEnv()` |
| `hideSpawnedWindow(*exec.Cmd)` | `config_spawn_windows.go` / `config_spawn_other.go` — `HideWindow` + `CREATE_NO_WINDOW` |
| `matchedBy`, `modelIDs`, `ttlDisplay` (config_explain.go) | small shared formatters |
| `restartCommand(configPath)` (config_apply.go) | reads `register-task.ps1`; returns commands + a source string that says READ or ASSUMED |
| `postRestartVerifyPlan(*lsconfig.File)` | the verify steps, keep-set read from the config |

**Any wave spawning a child process should call `hideSpawnedWindow`** — it is
already build-tagged for both platforms.

**Any wave reading the live proxy should go through `newLoopbackClient`**, not
`flags.newClient()` directly. The generated default base URL is
`http://localhost:11436`; on a dual-stack Windows host that resolves `::1`
first and stalls for seconds per call when the proxy is IPv4-only.

---

## 6. Behaviors other waves will want to know about

- **Keep-set must be read from the config, never the server.** The live API
  reports `ttl:0` for a `ttl:-1` seat — reproduced here: `/running` returns
  `"ttl":0` for `embeddinggemma`, whose config says `ttl: -1`. `config lint`
  emits a `ttl.keep-resident` INFO on every such seat saying exactly this.
- **`config drift` never counts an unloaded seat as clean.** It lists them
  under `not_evaluated` with the reason. Any command tempted to "check every
  seat" must not load one to do it: loading evicts what is resident.
- **`${PORT}` is normalized on both sides of every command comparison** via
  `lsconfig.NormalizePortValues`. Without it every seat reports drift on the
  one field llama-swap is supposed to rewrite.
- **Filenames are labels.** `DiscoverCorpus` orders by mtime and identifies by
  sha256; a filename date that disagrees with the mtime is reported, not
  trusted. Five such disagreements exist in the reference corpus.
- **`config apply` is dry-run only, and `--write` errors on purpose.** The
  splice-and-assert write engine is a future, trust-earned change.

---

## 7. Files owned by Wave B

```
internal/lsconfig/doc.go
internal/lsconfig/parse.go
internal/lsconfig/expand.go
internal/lsconfig/classify.go
internal/lsconfig/corpus.go
internal/lsconfig/validate.go
internal/lsconfig/lint.go
internal/lsconfig/diff.go
internal/lsconfig/lsconfig_test.go
internal/lsconfig/schema/config-schema.json
internal/lsconfig/schema/README.md
internal/cli/config_root.go
internal/cli/config_validate.go
internal/cli/config_lint.go
internal/cli/config_explain.go
internal/cli/config_diff.go
internal/cli/config_drift.go
internal/cli/config_backup.go
internal/cli/config_testinstance.go
internal/cli/config_apply.go
internal/cli/config_spawn_windows.go
internal/cli/config_spawn_other.go
internal/cli/config_cli_test.go
internal/cli/seat.go
internal/cli/seat_log.go
internal/cli/seat_log_test.go
internal/cli/seat_show.go
internal/cli/seat_try.go
internal/cli/bind_check.go
go.mod, go.sum
REGISTRATIONS-B.md
```

Not touched: `root.go`, `internal/store/*.go` (schema read only), `internal/mirror`,
`internal/fakeswap`, `internal/gguf`, and every other wave's `internal/cli` file.
