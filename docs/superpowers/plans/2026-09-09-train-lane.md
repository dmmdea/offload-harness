# Train Lane (P4: axolotl QLoRA as a harnessed fleet task) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `train` a fleet task type: a delegator dispatches a typed training contract to a Linux node that owns an axolotl venv; the node validates the rendered YAML with `axolotl preprocess`, takes the GPU media lease, drains and unloads its agent seat, runs `axolotl train` as a user-scope transient systemd unit with the lease in the child's environment, gates success on the adapter artifact, warms the seat back on every exit path, and returns the adapter's node-resident path. A dataset helper renders the delegation corpus's VERIFIED rows into SFT chat pairs with a contract-hash holdout. Serving the adapter and the fidelity gate (A5, "bind only on pass") are the next slice (P4b), not this plan.

**Architecture:** The `accel` task (0.115.0, ADR 0038) is the precedent for every seam: task-type constant + `Valid()`, `fleetTaskOrder` + `taskConfiguredFor`, `BuildRequest` arm with a strict payload decoder and a job dir under `BaseDir()/pipeline-jobs`, a `Pipeline.Run` branch, a concurrency-cap exemption, a forwarder package on the delegator, an MCP tool gated by config, a CLI verb. Two things are new: a `Launcher` seam (`internal/trainlaunch`) whose Linux implementation is `systemd-run --user --wait --collect` + `systemctl --user kill`, with a fail-closed stub elsewhere, and the seat eviction, which reuses the shipped `local-offload gpu reserve --class media --drain --unload-seat -- <cmd>` wrapper INSIDE the unit so the tested drain/unload/warm-back code in `gpu_drain.go` is not lifted or duplicated.

**Tech Stack:** Go 1.26; axolotl 0.18.0 (uv venv on the Lenovo: torch 2.12 cu130, peft 0.19.1, transformers 5.14.1); systemd user manager (<node-user>, Linger=yes); llama-swap; vLLM 0.28 (P4b only).

**Design:** `G:/My Drive/AI Ecosystem/Ecosystem/2026-09-07-envharness-axolotl-integration/DESIGN.md` §P4, reshaped 2026-09-07 11:1x by the council: acceptance = plumbing under contention (seat absent/present, forced kill with zero orphans, post-warm inference) on the 30-step synthetic QLoRA smoke; `preprocess` is the gate; the hand-run precedes the Go (done 2026-09-07 14:5x, artifacts under `axolotl-env/smoke`); the dataset helper filters by producing seat, keeps the answer under seq_len with a held-out slice.

## What the council said (2026-09-10 01:0x — five personas, verdict RESHAPE, confidence high; scores Contrarian 4 · Expansionist 8 · Logician 5 · Researcher 7 · Operator 5). **This plan is v1 and is NOT to be executed as written.** Rewrite to v2 with R1–R10 applied before any task starts.

R1. **Thread the node's config into the unit.** A `systemd-run --user` unit inherits the user manager's environment, not the fleet node's, so `LOCAL_OFFLOAD_CONFIG` never reaches the wrapper; with `HOME=BaseDir()` the wrapper resolves built-in defaults (drain/unload error → lease released → axolotl never starts), and without the HOME override it reads the decoy `~/.local-offload/config.json` and opens a DIFFERENT lease dir → trains beside the seat it promised to evict (the measured OOM). Fix: `--config <cfg>` on the wrapper argv AND `LOCAL_OFFLOAD_CONFIG` in `Spec.Env`; `HF_HOME` = the real `offload-stack/hf-cache`; do NOT override `HOME` (Triton/inductor caches); a test pins the argv/env/config triple.
R2. **Enforce the bound — nothing kills a trainer at the timeout today.** The wrapper form ignores `--for` as a deadline and renews every 15 s; `Jobs` has no per-job deadline; the delegator's poll merely stops watching. Fix: route `context.WithTimeout(TrainTimeout())` → `Kill()`, plus `-p RuntimeMaxSec=<timeout>` on the unit so the bound survives a node crash; the inner wrapper gets `--wait 2m` (never the 8h default: a re-dispatch would queue behind an orphan for 8 h); default timeout = the measured need, not 3600; docs say a node deploy kills a train in flight by design.
R3. **Warm-back after a kill.** `Kill()` SIGKILLs the whole cgroup including the wrapper whose `finish()` is the ONLY warm-back; the lease self-heals (dead pid) but the seat stays cold. Fix: `gpu release --warm-seat --config <cfg>` after every `Kill()+Wait()`; SIGINT `--kill-whom=main` first (the wrapper's sigc path kills the child then runs finish), cgroup SIGKILL after a grace; state plainly that the `DrainAndStop` 5 s path cannot warm.
R4. **No cancel door exists** (`Jobs` has no cancel; `server.go` handles health/dispatch/job/jobs/media). Acceptance #3 becomes "node restart mid-train: zero orphans, adapter dir cleaned, seat warm within N s of the node coming back". On startup, kill/adopt `offload-train-*` user units BEFORE `SweepOrphanedPipelineJobs` (which `RemoveAll`s the live trainer's `train.yaml`/dataset/`train.log`).
R5. **Dataset in the SERVED shape.** At serve time context docs reach the seat as replayed `read_file` TOOL RESULTS (`agenttask.go` / `core.SeedContextReads`) and the answer is prose (`res.Output`); `result.structured` comes from a separate `repackStructured` call. Render system + goal + seeded read_file tool turns → the prose answer; reuse `internal/agent/cutmiddle.go` for truncation (same elision as production); drop rows whose acceptance lint carried PARROT-PASSABLE/UNGROUNDED; holdout grouped by a NORMALISED goal hash (timestamps/re-digested logs escape the raw hash); manifest reports post-filter counts per schema and per rigger axis (the P4b gate's input); ONE `seq` constant = 1024 (measured; 4096 is an unmeasured cliff on a 16 GB card); parse the preprocess sample count and fail below a floor (axolotl drops over-length samples with only a warning); real runs use epochs, not `max_steps 30`.
R6. **Servability is decided in P4.** Qwen3.5 is a Gated-DeltaNet hybrid: the default q/k/v/o+gate/up/down targets train only the full-attention quarter, and vLLM 0.28 silently ignores modules outside its applied set (#38085). Pin `lora_target_modules` = axolotl's Qwen3.5 hybrid set (`linear_attn.in_proj_qkv/in_proj_z/out_proj` + …) ∩ what vLLM 0.28 applies, verified on 0.28 before freezing the golden; the manifest records the served base's exact snapshot + quantization (the seat is w4a16, the trainer is bnb nf4 — LoRA × compressed-tensors int4 is outside vLLM's tested matrix); the contract can express the bf16-LoRA → merge → requantize path (`method: lora`, no `load_in_4bit`).
R7. **Task 10.5 = the earn-its-keep gate, INSIDE this plan:** run the lane on the real corpus render with `holdout.jsonl` as the eval set and report eval loss on the holdout before (base) and after (adapter). axolotl computes it without serving. P4b is justified only if it drops meaningfully. (The 30/30 synthetic smoke proves the pipe, not the purpose.)
R8. **Task 9 pre-check runs inside the sandbox:** `systemd-run --pipe --uid=<node-user> -p ProtectSystem=strict -p ProtectHome=yes -E XDG_RUNTIME_DIR=/run/user/<uid> -E DBUS_SESSION_BUS_ADDRESS=… systemd-run --user --wait /bin/true` — a `sudo -u <node-user>` shell proves nothing about the node's sandbox.
R9. **Acceptance asserts RE-ROUTE, never "or wait":** during the window, agent contracts land on other nodes via the delegator's `LeaseBusy` gate; the test names the gate.
R10. **Corpus transport:** the helper's `--out` on the delegator never reaches `train_dataset_root`; the only in-plan training set is the ≤512 KiB inline slice (~40 rows at seq 4096). Add the documented push step (scp to the node's dataset root, or a node-side render) so the real set trains.
Cheapest test before any Go (48 h): hand-run on the Lenovo — render ~200 verified pairs in the served shape, `axolotl train` 1 epoch at seq 1024 with the hybrid target set and `holdout.jsonl` as eval, read eval loss before vs after. Operator's own condition for "yes today".

## Global Constraints

- Version: the next free number AFTER the composite-tier merge (0.116.0 is claimed); all four carriers move together (`VERSION`, `internal/buildinfo/buildinfo.go`, `.printing-press.json`, CHANGELOG). Never hardcode the number in code; the bump is Task 8's last step.
- NEVER-CLOUD: the trainer runs with `HF_HUB_OFFLINE=1` against the node-local `hf-cache`; no `axolotl fetch` at run time; the delegator reaches only `delegate_remotes` on the tailnet.
- NO DAEMONS: the trainer is a transient unit (`--collect`) that exists for the job; no timer, service or scheduler; the production seat's residency (ttl 300, no group, no preload, not boot-enabled) is untouched.
- THE LEASE EVICTS THE SEAT: the job takes `ClassMedia`, then drains and unloads the agent seat through llama-swap BEFORE the trainer starts (the 4B QLoRA OOMs beside the resident 4B seat: measured 2026-09-07), and warms it back on every exit path. `GPU_LEASE_DIR/EPOCH/CLASS` go into the trainer child's environment only, never into the fleet node's own process (ADR 0026: the node's own agent loads must WAIT on the lease mid-train).
- LEASE TTL = the training timeout, exactly (`Options.TTL`, renewed every 15 s by the wrapper).
- FAIL-LOUD, ARTIFACT-GATED: success = `systemd-run --wait` exit 0 AND non-empty `adapter_model.safetensors` + `adapter_config.json` at the declared output path; anything else is `core.Result{OK:false, Reason + a 400-byte log tail}` → job `error`. `--collect` garbage-collects a failed unit, so never read unit properties after exit.
- axolotl SILENTLY DROPS unknown YAML keys (measured 2026-09-09: `lora_rr: 8` → `lora_r=None` under `strict: true`): the Go renderer emits only keys from a pinned allowlist checked against a checked-in `config-schema` property list.
- ADVERTISEMENT == ADMISSION: `taskConfiguredFor("train") = cfg.FleetTrainEnabled && trainlaunch.Available() && the axolotl binary runs` — Windows nodes never advertise it; a Linux node without the venv never advertises it.
- Concurrency-cap EXEMPT (`concurrencyCapped` returns false for `train`, beside `accel`).
- Bearer-gated like the agent lane (a train job executes rendered configuration on the node's GPU).
- Payload ≤ 1 MiB (dispatch body cap): inline dataset ≤ 512 KiB; larger sets are node-resident paths under `train_dataset_root`. The adapter (42.5 MB) is returned as a node path, never inline.
- Byte-identity: a node without `fleet_train_enabled` and a delegator without `train_enabled` show nothing new (health, tools/list, status).
- Docs + ADR in the same PR; every exported symbol has a WHY comment; `go build ./... && go vet ./... && go test ./...` before every commit; commits end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Never touch the Lenovo's live units, config or llama-swap yaml in Tasks 1–8 (Task 9 does, with backups and the operator-approved order).

---

## File map

| Path | Responsibility |
|---|---|
| `internal/core/types.go` | `TaskTrain` + `Valid()` |
| `internal/config/config.go` | `fleet_train_enabled`, `train_axolotl_bin`, `train_workdir`, `train_timeout_sec`, `train_output_dir`, `train_dataset_root`, `train_enabled` (delegator), `train_remotes` (optional) |
| `internal/trainlaunch/` (new) | `Launcher` interface; `launch_linux.go` (systemd-run --user); `launch_other.go` stub; `Available()` |
| `internal/trainyaml/` (new) | `Contract` → YAML render from the pinned key allowlist; `schema_keys.json` (checked in); preprocess gate runner |
| `internal/fleetnode/train_task.go` (+test) | `TrainPayload`, `buildTrain`, job dir, dataset materialisation |
| `internal/fleetnode/tasks.go`, `server.go` | order + predicate + BuildRequest arm; cap exemption; auth marker |
| `internal/pipeline/traintask.go` (+test) | the route: preprocess gate → lease via the wrapper inside the unit → artifact gate → warm-back |
| `internal/trainremote/` (new) | delegator forwarder: node pick by `supported_task_types`, dispatch, poll under `timeout+60 s` |
| `internal/delegate/nodeview.go` | `NodeView.SupportedTaskTypes` (additive) |
| `internal/mcpserver/mcpserver.go`, `traintools.go` | `offload_train {action: dispatch|status}` gated by `train_enabled`; status row |
| `train_cmd.go` | CLI `train run|status|dataset` |
| `internal/traindata/` (new) | dataset helper over `rig.ReadShards` (VERIFIED rows, teacher seats, contract-hash holdout, middle truncation, redaction) |
| docs: `docs/systems/train-lane.md`, ADR (next free), `docs/FLEET-NODE.md`, `docs/systems/fleet-node.md`, `docs/flows/fleet-job-lifecycle.md`, `docs/OPERATOR-GUIDE.md`, `setup/SETUP-AGENT.md`, CHANGELOG | D8 |

---

### Task 1: Declare and gate — `TaskTrain`, config keys, `trainlaunch.Available()`

**Files:** `internal/core/types.go`, `internal/config/config.go` (+ `go generate .`), `internal/trainlaunch/launch.go`, `launch_linux.go` (`//go:build linux`), `launch_other.go` (`//go:build !linux`), `internal/fleetnode/tasks.go`; tests `internal/trainlaunch/launch_test.go`, `internal/fleetnode/train_task_test.go` (first test only).

**Interfaces (produces):**

```go
// core
const TaskTrain TaskType = "train"   // + in Valid()

// config (all omitempty, zero defaults)
FleetTrainEnabled bool   `json:"fleet_train_enabled,omitempty"` // node opt-in
TrainAxolotlBin   string `json:"train_axolotl_bin,omitempty"`   // <venv>/bin/axolotl
TrainWorkdir      string `json:"train_workdir,omitempty"`       // the axolotl-env dir (cwd for preprocess/train)
TrainTimeoutSec   int    `json:"train_timeout_sec,omitempty"`   // default 3600 via accessor; = the lease TTL
TrainOutputDir    string `json:"train_output_dir,omitempty"`    // default BaseDir()/models/adapters via accessor
TrainDatasetRoot  string `json:"train_dataset_root,omitempty"`  // node-resident datasets may only come from here
TrainEnabled      bool   `json:"train_enabled,omitempty"`       // delegator: registers offload_train + the CLI door

// trainlaunch
type Spec struct { Unit, Workdir, LogPath string; Args []string; Env map[string]string }
type Launcher interface {
	Start(ctx context.Context, s Spec) (Handle, error)
}
type Handle interface {
	Wait() (exitCode int, err error) // from systemd-run --wait; never from unit properties
	Kill() error                     // systemctl --user kill -s SIGKILL <unit>
}
func Available() (ok bool, reason string) // linux: XDG_RUNTIME_DIR resolvable + `systemctl --user --version` answers; other: false, "train lane is linux-only"
func New() Launcher
```

`taskConfiguredFor("train")` = `cfg.FleetTrainEnabled && trainlaunch.Available() && cfg.TrainAxolotlBin != "" && exec.LookPath-or-stat(cfg.TrainAxolotlBin) ok`; `fleetTaskOrder` gains `"train"` last.

- [ ] Tests first: `TestTrainIsAdvertisedExactlyWhenEnabledAvailableAndBound` (table over enabled/available/bin; a non-linux build always false — use a package-level `available` seam in tests), `TestSupportedTasksDerivation` unchanged (train predicate false on `fullCfg`/`imageCfg`), `TestHealthGoldenShape` unchanged; config accessors `TrainTimeout()` (3600 s default), `TrainOutput()` (BaseDir()/models/adapters).
- [ ] Implement; `go generate .`; `go test ./internal/core ./internal/config ./internal/trainlaunch ./internal/fleetnode .` green; commit `"train lane: task type, config keys and the linux-only launcher gate"`.

---

### Task 2: YAML render from a pinned allowlist + the preprocess gate

**Files:** `internal/trainyaml/render.go`, `schema_keys.json` (the property names from `axolotl config-schema --format json` with the stdout banner stripped, checked in with the axolotl version that produced it), `preprocess.go`, tests.

**Interfaces (produces):**

```go
type Contract struct {
	Name         string   `json:"name"`                    // adapter name, ^[a-z0-9][a-z0-9._-]{1,63}$
	BaseModel    string   `json:"base_model"`              // HF repo id or node-local snapshot path
	Method       string   `json:"method"`                  // "qlora" | "lora"
	LoraR        int      `json:"lora_r,omitempty"`        // default 8
	LoraAlpha    int      `json:"lora_alpha,omitempty"`    // default 16
	LoraDropout  float64  `json:"lora_dropout,omitempty"`  // default 0.05
	SequenceLen  int      `json:"sequence_len,omitempty"`  // default 1024
	MaxSteps     int      `json:"max_steps,omitempty"`     // default 30 (the smoke); 0 = epochs
	Epochs       int      `json:"epochs,omitempty"`
	MicroBatch   int      `json:"micro_batch_size,omitempty"` // default 1
	LearningRate float64  `json:"learning_rate,omitempty"`    // default 2e-4
	TargetModules []string `json:"lora_target_modules,omitempty"` // default = the smoke's attention q/k/v/o + MLP gate/up/down
	DatasetInline string  `json:"dataset_inline,omitempty"`  // JSONL, ≤ 512 KiB, {"messages":[...]} rows
	DatasetPath   string  `json:"dataset_path,omitempty"`    // node-resident, must be under train_dataset_root
	TimeoutSec    int     `json:"timeout_sec,omitempty"`     // default cfg.TrainTimeout()
}
func (c Contract) Validate() error
// Render writes <jobDir>/train.yaml from the contract with ABSOLUTE dataset_prepared_path=<jobDir>/prepared and output_dir=<outputRoot>/<name>; every emitted key is in schema_keys.json (a test pins the allowlist ⊆ schema) and the render of the smoke contract equals the checked-in golden `testdata/qlora-smoke.golden.yaml` (the proven 30-line recipe from the Lenovo's axolotl-env/smoke/qlora-smoke.yaml).
func Render(c Contract, jobDir, datasetFile, outputRoot string) (yamlPath string, err error)
// Preprocess runs `<axolotl> preprocess <yaml>` in workdir with the same env the launcher uses, no GPU, bounded by 10 min; pass = exit 0 AND stdout contains "Success! Preprocessed data path".
func Preprocess(ctx context.Context, axolotlBin, workdir, yamlPath string, env map[string]string) (log string, err error)
```

- [ ] Tests: `TestRenderEmitsOnlyAllowlistedKeysAndMatchesTheSmokeGolden`, `TestRenderRefusesADatasetPathOutsideTheRoot`, `TestContractValidateRejectsBadNamesMethodsAndOversizedInline`, `TestPreprocessPassIsTheSuccessLineNotTheExitCode` (fake axolotl script: exit 0 without the line = fail).
- [ ] Commit `"trainyaml: contract → axolotl YAML from a pinned allowlist; preprocess is the gate"`.

---

### Task 3: Node — `buildTrain`, job dir, cap exemption, auth marker

**Files:** `internal/fleetnode/train_task.go` (+test mirroring `accel_task_test.go:23/42/78/112`), `tasks.go` (BuildRequest arm), `server.go` (`concurrencyCapped` `case "train": return false`; bearer gate + poll marker keyed on `train` like `agent`), `queue_test.go:396` + `busylease_test.go:232` rows.

`TrainPayload` = `trainyaml.Contract` decoded with `DisallowUnknownFields`; job dir `MkdirTemp(BaseDir()/pipeline-jobs, "train-*")`; the inline dataset is written to `<jobDir>/dataset.jsonl` (cap enforced before write); a `dataset_path` is verified under `cfg.TrainDatasetRoot` (no symlink escape: `filepath.EvalSymlinks` + prefix check); `Render` produces `<jobDir>/train.yaml`; `core.Request{Task: TaskTrain, Params{"job_dir", "yaml", "contract", "output_dir": <TrainOutput()>/<name>}}`; cleanup removes the job dir only (the output dir persists).

- [ ] Tests: served-exactly-when, materialisation + cleanup, refusal matrix leaves no dirs (unknown field, oversized inline, path outside root, bad name), not-capped, re-dispatch re-acks, bearer required on a non-loopback tokenless listener.
- [ ] Commit `"fleet node: the train task — strict payload, job dir, cap exemption, bearer gate"`.

---

### Task 4: Pipeline route — preprocess gate → wrapper-leased unit → artifact gate → warm-back

**Files:** `internal/pipeline/traintask.go` (+test with a fake `Launcher` and a fake llama-swap), `pipeline.go` (`Run` branch after `TaskAccel`), `internal/pipeline/gpulease_coverage_test.go` (add the train case by hand).

Route (`runTrainTask`):
1. `trainyaml.Preprocess` (no lease; the seat may be resident). Failure → `Deferf("train: preprocess refused the config: <tail>")` → job `error`.
2. Build the unit command: `<local-offload> gpu reserve --class media --for <timeout> --reason train:<job> --origin train --drain --unload-seat -- <axolotl> train <yaml>` — the shipped wrapper takes the media lease, drains and unloads the agent seat (`gpu_drain.go`), exports `GPU_LEASE_DIR/EPOCH/CLASS` to its child only, renews every 15 s, and releases on exit. Confirm from `gpu_cmd.go:153-195` whether the wrapper warms the seat back after the wrapped command exits; if it does not, the route runs `<local-offload> gpu release --warm-seat` after `Wait()` returns (both paths tested with the fake).
3. `launcher.Start(ctx, Spec{Unit: "offload-train-<job>", Workdir: cfg.TrainWorkdir, LogPath: <jobDir>/train.log, Args: [...], Env: {PATH: <venv>/bin:…, HOME: BaseDir(), HF_HOME: <BaseDir()>/hf-cache, HF_HUB_OFFLINE: "1", AXOLOTL_DO_NOT_TRACK: "1"}})`; a goroutine calls `Kill()` on ctx cancel (DrainAndStop cancels after 30 s; the kill must land inside the 5 s grace — test with the fake).
4. `Wait()` → exit code; then the artifact gate: `output_dir/adapter_model.safetensors` non-empty AND `adapter_config.json` present. Success → `core.Result{OK: true, Data: {"adapter_dir", "adapter_bytes", "train_runtime_s" (from train.log if present), "unit", "log_tail"}}`; else `OK:false` with the 400-byte log tail.
5. Never set `GPU_LEASE_*` in the node's own process.

- [ ] Tests (fake launcher + fake llama-swap): the unit command line is exactly the wrapper form; env carries PATH/HOME/HF_HOME/HF_HUB_OFFLINE; preprocess failure never starts the unit; exit 0 without the artifact is `OK:false`; ctx cancel calls `Kill` within the grace; the node's own env never gains `GPU_LEASE_EPOCH`; `TestJobsDeferredResultBecomesErrorWithReason` shape holds for the defers.
- [ ] Commit `"pipeline: the train route — preprocess gate, wrapper-leased transient unit, artifact-gated success"`.

---

### Task 5: Linux launcher

**Files:** `internal/trainlaunch/launch_linux.go` (+ an integration test `//go:build linux` skipped when `systemctl --user --version` fails).

`Start` = `systemd-run --user --unit <unit> --wait --collect --working-directory <workdir> -p StandardOutput=append:<log> -p StandardError=append:<log> -E K=V… -- <args>`; `Wait` returns the exit code of `systemd-run --wait` (non-zero = the unit failed; its text carries `Main processes terminated with: code=exited/status=N` or `code=killed`); `Kill` = `systemctl --user kill -s SIGKILL <unit>` then waits for the unit to vanish (`systemctl --user is-active` → inactive/failed) inside 5 s. `Available` requires `XDG_RUNTIME_DIR` (from env, else `/run/user/<uid>`) to exist and `systemctl --user --version` to answer.

- [ ] Integration test (Linux with a user manager): `Start` `/bin/sh -c 'sleep 30'`, `Kill`, assert the unit is gone and `Wait` returns a killed code within 5 s; `Start` `/bin/true` → exit 0.
- [ ] Commit `"trainlaunch: systemd-run --user transient units with a cgroup kill"`.

---

### Task 6: Delegator doors — `trainremote`, `NodeView.SupportedTaskTypes`, CLI `train run|status`, MCP `offload_train`

**Files:** `internal/trainremote/trainremote.go` (+test with the `accelremote_test.go:32` fake-node fixture), `internal/delegate/nodeview.go` (+ `healthWire.SupportedTaskTypes`; pin absent = nil), `train_cmd.go`, `internal/mcpserver/mcpserver.go` (literal `Name: "offload_train"` inside `if s.p != nil && s.p.Cfg().TrainEnabled`), `.printing-press.json` (`mcp.tools` gains `offload_train`), `internal/mcpserver/badargs_test.go` table row, a differential tools/list pin (`TestOffloadTrainRegistrationGated`), `offload_status.fleet.nodes[].supported_task_types` (additive, only when published).

`trainremote.Dispatch(ctx, cfg, contract) (jobID, node, base string, err)` — picks the first `delegate_remotes` node whose health `supported_task_types` contains `train` (2 s probe), POSTs the envelope `{job_id: "train-"+16hex, task_type: "train", payload}` with the bearer, requires 202. `trainremote.Status(ctx, cfg, base, jobID) (state, data, err)` polls once; `Wait` polls every 3 s under `timeout_sec + 60 s`. The MCP tool is async: `{action: "dispatch", contract…}` → `{job_id, node, base}`; `{action: "status", job_id, base}` → the job wire; the CLI `train run --contract f.json [--wait]` and `train status --job <id> --base <url>` mirror it.

- [ ] Tests as listed in the file map + `TestFetchNodeViewDecodesSupportedTaskTypes` (absent → nil); commit `"train lane: delegator doors — forwarder, CLI, gated MCP tool, node capability decode"`.

---

### Task 7: Dataset helper (independent; can run in parallel with Tasks 3–6)

**Files:** `internal/traindata/` (+tests on synthetic shards), CLI `train dataset --from-corpus --teacher-seats agent-pool,qwen3.8-27b --seq 4096 --holdout 0.1 --out <dir> [--since 30d] [--exclude-arms]`.

Rules (from the corpus map): rows via `rig.ReadShards(BaseDir()/delegation-log, since, now)`; VERIFIED = `acceptance_pass && !deferred && error=="" && result.structured present`; exact seat-name match; system line = the profile's System when `contract.profile` names one, else `agent.SystemPrompt(false…)` — record the choice per row in the manifest; user = goal + each context doc as a fenced block `### <name>` (documented as the DESIGN shape; the manifest records `doc_shape: inline`); assistant = compact JSON of `result.structured` (manifest records `target: structured_json`); budget = `seq × 3 chars − system − answer − 256` (chars/3, the gate's bound), middle-truncate the largest doc with a counted elision marker, never the goal; drop rows whose answer alone exceeds the budget and count them; split by contract hash (sha256 of goal + context + output_schema) with at most `--per-contract 3` rows per contract, holdout by hash; exclude rows whose `arm` is set unless `--include-arms`; redaction + `VetPII` (reuse the compaction-eval harvest gate) before write; atomic temp+rename; write `train.jsonl`, `holdout.jsonl` (each holdout line keeps the FULL contract for a later replay), `manifest.json` (counts, choices, seats, window, dropped, axolotl dataset type).

- [ ] Tests: verified predicate; seat match; per-contract cap and hash split (a contract repeated 56× lands entirely on one side); truncation keeps head+tail and marks the cut; oversize-answer rows counted not silently dropped; arm exclusion; PII refusal.
- [ ] Commit `"traindata: the delegation corpus as SFT pairs — verified rows, teacher seats, contract-hash holdout"`.

---

### Task 8: Docs, ADR, CHANGELOG, version

- `docs/systems/train-lane.md` (`## Purpose`, the flow, the unit/privilege model, the eviction rule, the artifact gate, what P4b adds, `## Source map`); ADR "Training is a lane and adapters are gated" (next free number; Accepted with the operator's 2026-09-07 order as provenance); `docs/FLEET-NODE.md` task table (`train` row — and note the missing `accel` row as a separate proposal, not fixed silently unless trivial), config keys table; `docs/systems/fleet-node.md` "The train task" + routes row; `docs/flows/fleet-job-lifecycle.md`; `docs/OPERATOR-GUIDE.md` (dataset section; node unit drop-in for `XDG_RUNTIME_DIR`); `setup/SETUP-AGENT.md` (the keys, the adapter dir, the drop-in); `docs/systems/mcp-server.md` (`offload_train`); `README.md` tool table; CHANGELOG entry; version carriers bumped to the next free number.
- [ ] `go test ./...` + `TestDocsLint` + `TestVersionSourcesAgree` + `TestPrintingPressManifestListsEveryTool` green; commit `"<next version>: the train lane — axolotl as a harnessed fleet task"`.

---

### Task 9: Lenovo deploy prep (orchestrator; production node; backups)

- Build `GOOS=linux`; deploy beside the running binary; a unit drop-in for `offload-fleet-node` adding `Environment=XDG_RUNTIME_DIR=/run/user/1000` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus` (ProtectHome=yes does not cover `/run/user`; verify `systemd-run --user --wait /bin/true` succeeds FROM the node's identity and env before restarting the node); config keys: `fleet_train_enabled: true`, `train_axolotl_bin: <offload-stack>/axolotl-env/.venv/bin/axolotl`, `train_workdir: <offload-stack>/axolotl-env`, `train_output_dir: <offload-stack>/models/adapters`, `train_dataset_root: <offload-stack>/datasets`; `mkdir` both dirs; restart the node (30 s drain); health shows `supported_task_types` containing `train`; the Qube/Aorus never advertise it.

### Task 10: Live acceptance on the Lenovo (LAST, artifacts captured under `offload-stack/measurements/train-lane-<date>/`)

1. Preprocess gate with the seat RESIDENT (no eviction).
2. Dispatch the 30-step smoke (the 10-row inline dataset) with the seat RESIDENT → expect drain → unload → 30/30 → adapter at `<output>/<name>/adapter_model.safetensors` (≈42.5 MB) → seat warmed back; journal excerpt, `nvidia-smi` before/during/after, health lease block, job data.
3. Dispatch again and cancel mid-flight (delegator cancel → node ctx cancel → `Kill`) → job `error: interrupted`; `ps -eo cmd | grep "[a]xolotl.cli.train"` empty; `nvidia-smi --query-compute-apps` empty; memory back to the seat's; lease released; seat warmed.
4. During a train: an agent contract routed to the Lenovo WAITS on the media lease (or is placed elsewhere by the delegator's LeaseBusy gate) — never reloads the seat under the trainer; `fleet-smoke` PASS after warm-back.
5. Records: gates NOTES, measurement README, mem0, the DESIGN.md status line.

## Out of this plan (P4b, next)
Serving the adapter: one seat restart to add `--enable-lora --max-lora-rank 16` + `VLLM_ALLOW_RUNTIME_LORA_UPDATING=1` (dynamic loads afterwards), a distinct llama-swap CANDIDATE entry (`useModelName: <adapter>` — the base entry's rewrite would silently measure the base), the fidelity gate (needle + the 8-digest set before/after, blind-judged), and "bind only on pass".
