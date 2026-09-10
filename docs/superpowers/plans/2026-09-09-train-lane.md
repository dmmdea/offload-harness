# Train Lane (P4: axolotl QLoRA as a harnessed fleet task) Implementation Plan — v2 (after the council)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Task 0 is hand-run by the orchestrator BEFORE any Go task starts; its decision rule decides whether Tasks 8–10.5 run at all.

**Goal:** Make `train` a fleet task type: a delegator dispatches a typed training contract to a Linux node that owns an axolotl venv; the node validates the rendered YAML with `axolotl preprocess`, takes the GPU media lease through the shipped `gpu reserve` wrapper (which drains and unloads the agent seat), runs `axolotl train` as a user-scope transient systemd unit under an ENFORCED timeout, gates success on the adapter artifact, warms the seat back on every exit path including a kill, and returns the adapter's node-resident path. A dataset helper renders the delegation corpus's VERIFIED rows in the SERVED shape (system + goal + the seeded `read_file` tool turns → the prose answer) with a normalised-goal-hash holdout, and the plan ends with the earn-its-keep gate: eval loss on the holdout before vs after. Serving the adapter and the fidelity gate (A5, "bind only on pass") are the next slice (P4b), justified only if Task 10.5 passes.

**Architecture:** The `accel` task (0.115.0, ADR 0038) is the precedent for every seam: task-type constant + `Valid()`, `fleetTaskOrder` + `taskConfiguredFor`, `BuildRequest` arm with a strict payload decoder and a job dir under `BaseDir()/pipeline-jobs`, a `Pipeline.Run` branch, a concurrency-cap exemption, a forwarder package on the delegator, an MCP tool gated by config, a CLI verb. New: a `Launcher` seam (`internal/trainlaunch`) whose Linux implementation is `systemd-run --user --wait --collect -p RuntimeMaxSec=…` + a two-stage `systemctl --user kill` (SIGINT to the main process, then cgroup SIGKILL), a fail-closed stub elsewhere; and the seat eviction, which reuses the shipped `local-offload gpu reserve --class media --drain --unload-seat --wait 2m --config <cfg> -- <cmd>` wrapper INSIDE the unit so the tested drain/unload/warm-back code in `gpu_drain.go` is not lifted or duplicated. The node's config is threaded into the unit explicitly (argv AND env), because a user unit inherits the user manager's environment, not the node's.

**Tech Stack:** Go 1.26; axolotl 0.18.0 (uv venv on the Lenovo node: torch 2.12 cu130, peft 0.19.1, transformers 5.14.1); systemd user manager (`<node-user>`, Linger=yes); llama-swap; vLLM 0.28 (P4b only, but its applied LoRA module set is verified in Task 2).

**Design:** `G:/My Drive/AI Ecosystem/Ecosystem/2026-09-07-envharness-axolotl-integration/DESIGN.md` §P4, reshaped 2026-09-07 11:1x by the council (acceptance = plumbing under contention on the 30-step synthetic smoke; `preprocess` is the gate; the hand-run precedes the Go — done 2026-09-07 14:5x, artifacts under `axolotl-env/smoke`) and reshaped again 2026-09-10 01:0x (this v2; see the next section). The v1 plan is commit 5fb87d5 on this branch.

## What the council changed (2026-09-10 01:0x — five personas, verdict RESHAPE, confidence high; scores Contrarian 4 · Expansionist 8 · Logician 5 · Researcher 7 · Operator 5)

Each R-item, what it found, and WHERE it landed in this plan. Every seam named below was re-read in the repo at b12f77e (0.115.3) while writing v2; where an item says a seam does not exist, the plan does not invent one.

R1. **Thread the node's config into the unit.** A `systemd-run --user` unit inherits the user manager's environment, not the fleet node's, so `LOCAL_OFFLOAD_CONFIG` never reaches the wrapper; with `HOME=BaseDir()` (v1) the wrapper resolves built-in defaults (drain/unload error → lease released → axolotl never starts), and without the HOME override it reads the decoy `~/.local-offload/config.json` and opens a DIFFERENT lease dir → trains beside the seat it promised to evict (the measured OOM). Verified: `main.go` `resolveCfgPath` precedence is `--config` > `$LOCAL_OFFLOAD_CONFIG` > `./config.json` > `~/.local-offload/config.json` > defaults; `gpu_cmd.go` `openLease` → `loadCfg(fs)` honours `--config`; `config.Source.Path` carries the resolved path but `runFleetServe` calls `loadCfg`, which drops it. **Landed:** Global Constraints (config-threading invariant); Task 1 (`Config.ConfigPath` stamped by the loader; the route refuses to start on defaults); Task 4 (`--config <cfg>` on the wrapper argv AND `LOCAL_OFFLOAD_CONFIG` in `Spec.Env`; `HF_HOME` = the real hf-cache; `HOME` never overridden; `TestTrainUnitCarriesTheConfigTriple`).
R2. **Enforce the bound — nothing kills a trainer at the timeout today.** Verified: the wrapper form ignores `--for` as a deadline (it renews every 15 s until the command exits — `gpu_cmd.go` `runGPUReserve`); `Jobs` has no per-job deadline (`jobs.go`: one store-wide ctx cancelled by `DrainAndStop`); the delegator's poll merely stops watching; `gpu reserve --wait` defaults to 8 h (`defaultReserveWait`, 0.115.2). **Landed:** Global Constraints (enforced-timeout invariant); Task 4 (`context.WithTimeout(TrainTimeout())` → `Kill()`; `TestTrainTimeoutKillsTheUnit` with the fake launcher); Task 5 (`-p RuntimeMaxSec=<timeout+grace>` on the unit argv, `TestUnitArgvCarriesRuntimeMaxSec`); the inner wrapper gets `--wait 2m` (Task 4 argv pin); the default timeout is Task 0's measured need, not 3600; Task 8 docs say a node deploy kills a train in flight by design.
R3. **Warm-back after a kill.** Verified: `Kill()` in v1 SIGKILLed the whole cgroup including the wrapper, whose `finish()` (`gpu_cmd.go`) is the ONLY warm-back; the lease self-heals (dead pid) but the seat stays cold; `DrainAndStop` gives a cancelled run 5 s (`killDeliveryGrace`) — too short for a warm (a model load). **Landed:** Global Constraints (warm-after-kill invariant); Task 5 (`Kill` = SIGINT `--kill-whom=main` first — the wrapper's `sigc` path kills the child and runs `finish()` — then cgroup SIGKILL after a grace); Task 4 (`gpu release --warm-seat --config <cfg>` after EVERY `Kill()+Wait()`, `TestKillIsFollowedByWarmBack`); Task 3/5 (startup adopt-and-warm heals the `DrainAndStop` path, which is stated plainly as unable to warm).
R4. **No cancel door exists.** Verified: `Jobs` has Accept/AcceptAgent/Admit/Get/Recent/DrainAndStop — no cancel; `server.go` `Handler()` routes health/dispatch/jobs/{id}/jobs/media only. **Landed:** Task 10 acceptance #3 is "node restart mid-train: zero orphans, adapter dir cleaned, seat warm within N s of the node coming back" (no cancel door is added); Task 3 + Task 5 (`trainlaunch.AdoptOrKill` runs in `runFleetServe` BEFORE `SweepOrphanedPipelineJobs`, which `RemoveAll`s every `pipeline-jobs/*` entry — the live trainer's `train.yaml`/dataset/`train.log` — verified at `tasks.go:989` and `main.go:2224`).
R5. **Dataset in the SERVED shape.** Verified: at serve time a grounded contract's context docs reach the seat as replayed `read_file` TOOL RESULTS (`agenttask.go` `setupActionsFor` → `core.SeedContextReads`; `agent/setup.go` `replaySetup` appends ONE assistant turn with the `setup-N` tool calls and one `tool` message per doc, the content being `read_file`'s numbered-line rendering trimmed by `contextbudget.Trim(content, toolResultCapChars())`), the answer is prose (`res.Output`), and `result.structured` comes from a separate `repackStructured` call. Also verified: the delegation-log row (`delegate/run.go` `delegationLogLine`) persists `contract`, `result` (with `trace[].obs_chars` and `setup` per step, `setup_ran`) and `acceptance_failures` — NOT the transcript and NOT the intake lint. So "via the actual transcript" is implemented as a deterministic RE-RENDER through the loop's own code (`agent.ServedTranscript`, a thin exported builder pinned byte-for-byte against a real `Loop.Run` transcript in a test), with the row's `obs_chars` as the per-doc pin, and the lint re-run at render time through the exported `delegate.LintAcceptance`. `cutMiddleTurns` is unexported; `CompactReplay` (the elide rung) is exported for the compaction eval — Task 7 exports a `CutMiddleReplay` wrapper with no logic change. **Landed:** Task 7 entirely (served-shape render, cutmiddle reuse, lint filter, normalised-goal-hash holdout, manifest per-schema/per-rigger-axis counts, ONE `seq` constant 1024, preprocess sample-count floor, epochs not `max_steps 30`).
R6. **Servability is decided in P4.** Qwen3.5 is a Gated-DeltaNet hybrid: the smoke's q/k/v/o + gate/up/down targets train only the full-attention quarter, and vLLM 0.28 silently ignores modules outside its applied set (#38085). **Landed:** Global Constraints (target-module pin); Task 2 (`lora_target_modules` = axolotl's Qwen3.5 hybrid set ∩ vLLM 0.28's applied set, with the VERIFICATION step written out — the final list is not guessed here; the manifest records the served base's exact snapshot + quantization; `method: lora` without `load_in_4bit` is expressible).
R7. **Task 10.5 = the earn-its-keep gate, INSIDE this plan.** **Landed:** Task 0 (the 48-hour hand-run version, before any Go) and Task 10.5 (the same measurement through the lane, with an explicit pass/fail rule; P4b is justified only on pass).
R8. **Task 9 pre-check runs inside the sandbox.** **Landed:** Task 9 (the `systemd-run --pipe --uid=<node-user> -p ProtectSystem=strict -p ProtectHome=yes -E XDG_RUNTIME_DIR=/run/user/<uid> -E DBUS_SESSION_BUS_ADDRESS=… systemd-run --user --wait /bin/true` probe; a `sudo -u` shell proves nothing).
R9. **Acceptance asserts RE-ROUTE, never "or wait".** Verified: the gate is `delegate/gate.go` `remoteEligible` (`!r.LeaseBusy`, joined 2026-09-07; `NodeView.LeaseBusy` decodes health `lease.busy`), and `leasedLanes` names it "(long GPU lease held)". **Landed:** Task 10 acceptance #4 names the gate and asserts placement elsewhere.
R10. **Corpus transport.** Verified: v1's helper wrote `--out` on the delegator; only the ≤512 KiB inline slice could ever train. **Landed:** Task 7 (the documented push step to `train_dataset_root` — `scp` to the node's dataset root over the tailnet — and `dataset_path` in the contract), Task 10.5 uses it.
**The 48-hour test** (the operator's own condition for "yes today"): **Landed:** Task 0, with the decision rule.

## Global Constraints

- Version: the next free number AFTER the composite-tier merge (0.116.0 is claimed by `docs/composite-tier-design`); all four carriers move together (`VERSION`, `internal/buildinfo/buildinfo.go`, `.printing-press.json`, CHANGELOG). Never hardcode the number in code; the bump is Task 8's last step; do not write a number anywhere in this plan.
- NEVER-CLOUD: the trainer runs with `HF_HUB_OFFLINE=1` against the node-local `hf-cache`; no `axolotl fetch` at run time; the delegator reaches only `delegate_remotes` on the tailnet.
- NO DAEMONS: the trainer is a transient unit (`--collect`) that exists for the job; no timer, service or scheduler; the production seat's residency (ttl 300, no group, no preload, not boot-enabled) is untouched.
- THE LEASE EVICTS THE SEAT: the job takes `ClassMedia` through the wrapper, which drains and unloads the agent seat through llama-swap BEFORE the trainer starts (the 4B QLoRA OOMs beside the resident 4B seat: measured 2026-09-07), and warms it back on every exit path. `GPU_LEASE_DIR/EPOCH/CLASS` go into the trainer child's environment only (the wrapper sets them on ITS child — `gpu_cmd.go` `cmd.Env`), never into the fleet node's own process (ADR 0026: the node's own agent loads must WAIT on the lease mid-train).
- LEASE TTL = the training timeout, exactly (`--for <timeout>` → `Options.TTL`, renewed every 15 s by the wrapper). The TTL is a DECLARATION for waiters, never the bound (R2).
- **ENFORCED-TIMEOUT INVARIANT (R2):** three independent bounds, all equal to `TrainTimeout()` plus a fixed grace: (1) the route runs the unit under `context.WithTimeout(ctx, TrainTimeout())` and calls `Launcher.Handle.Kill()` when it fires; (2) the unit carries `-p RuntimeMaxSec=<TrainTimeout()+killGrace>` so the bound survives a node crash (the user manager enforces it with no node process alive); (3) the inner wrapper is invoked with `--wait 2m`, NEVER the 8 h `defaultReserveWait` — a re-dispatch must fail in 2 min, not queue 8 h behind an orphan. The default timeout is Task 0's measured 1-epoch wall × 2 rounded up to 15 min, recorded in `measurements/train-lane-<date>/README.md`; 3600 is not a default anywhere.
- **CONFIG-THREADING INVARIANT (R1):** the wrapper and every `gpu` verb the route spawns get `--config <cfg>` on argv AND `LOCAL_OFFLOAD_CONFIG=<cfg>` in `Spec.Env`, where `<cfg>` = `cfg.ConfigPath` (the path `config.LoadWithSource` resolved — stamped in Task 1); a node running on built-in defaults (`ConfigPath == ""`) never starts a train (defer, not a guess). `HF_HOME` = the real `<offload-stack>/hf-cache` (config key `train_hf_home`, no derived default). `HOME` is NEVER overridden (Triton/inductor caches live under it; the decoy `~/.local-offload/config.json` is neutralised by the explicit `--config`, not by moving HOME).
- **WARM-AFTER-KILL INVARIANT (R3):** every path that ends the unit other than the wrapped command's own exit runs `local-offload gpu release --warm-seat --config <cfg>` after `Wait()` returns: the timeout kill, the ctx-cancel kill, and the node's startup adopt-or-kill. `Kill()` is two-stage (SIGINT to the main process → the wrapper's `sigc` path kills axolotl and runs `finish()`, which warms then releases; then cgroup SIGKILL after `killGrace`), so the explicit warm-back is a second, idempotent call on the good path and the ONLY warm on the bad one. Stated plainly: `Jobs.DrainAndStop`'s 5 s `killDeliveryGrace` cannot warm a seat (a warm is a model load); after a node stop the seat is warmed by the next start's adopt-or-kill step, not by the stopping process.
- **A NODE DEPLOY KILLS A TRAIN IN FLIGHT, BY DESIGN** (documented in Task 8): `DrainAndStop(30 s)` marks the job `interrupted`, cancels its ctx, the route's Kill lands inside the grace; the restarted node adopts nothing — it kills any surviving `offload-train-*` unit, warms the seat, then sweeps the job dirs. There is no resume; the operator re-dispatches.
- **SERVED-SHAPE DATASET INVARIANT (R5):** a training row is the transcript the seat actually saw at serve time — system prompt (profile or default), the goal as the user turn, the setup head + one `tool` message per context doc rendered by the loop's own `read_file` formatter and trimmed by the loop's own cap — followed by the prose answer (`result.output`) as the assistant target. Never a fenced-block user message, never `result.structured` as the target (that is the re-pack's shape, produced by a separate call). Rows whose `setup_ran` < number of context docs cannot be reconstructed and are dropped and counted.
- **ONE `seq` CONSTANT: `traindata.Seq = 1024`** (measured on the 16 GB card; 4096 is an unmeasured cliff). The helper's budget, the contract's `sequence_len` default and the eval config all read this one constant; a test pins that no other literal `sequence_len` exists in the repo outside `testdata/`.
- **TARGET-MODULE PIN (R6):** `trainyaml.TargetModulesQwen35Hybrid` = axolotl 0.18's Qwen3.5 hybrid module set ∩ vLLM 0.28's applied LoRA module set, established by the verification step in Task 2 (both lists read from the installed packages, the intersection checked in with the two version strings). Not guessed here.
- FAIL-LOUD, ARTIFACT-GATED: success = `systemd-run --wait` exit 0 AND non-empty `adapter_model.safetensors` + `adapter_config.json` at the declared output path; anything else is `core.Result{OK:false, Reason + a 400-byte log tail}` → job `error`. `--collect` garbage-collects a failed unit, so never read unit properties after exit.
- axolotl SILENTLY DROPS unknown YAML keys (measured 2026-09-09: `lora_rr: 8` → `lora_r=None` under `strict: true`): the Go renderer emits only keys from a pinned allowlist checked against a checked-in `config-schema` property list.
- ADVERTISEMENT == ADMISSION: `taskConfiguredFor("train") = cfg.FleetTrainEnabled && trainlaunch.Available() && the axolotl binary is present` — Windows nodes never advertise it; a Linux node without the venv never advertises it.
- Concurrency-cap EXEMPT (`concurrencyCapped` returns false for `train`, beside `accel`).
- Bearer-gated like the agent lane (a train job executes rendered configuration on the node's GPU): dispatch requires the bearer on a tokenised listener, and the job record carries the poll-auth marker `handleJob` keys on (`JobView.Agent` today — Task 3 widens the marker's meaning to "privileged lane" without renaming the wire field).
- Payload ≤ 1 MiB (dispatch body cap `maxDispatchBody`): inline dataset ≤ 512 KiB; larger sets are node-resident paths under `train_dataset_root`. The adapter (42.5 MB) is returned as a node path, never inline.
- Byte-identity: a node without `fleet_train_enabled` and a delegator without `train_enabled` show nothing new (health, tools/list, status).
- Docs + ADR in the same PR; every exported symbol has a WHY comment; `go build ./... && go vet ./... && go test ./...` before every commit; commits end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Never touch the Lenovo node's live units, config or llama-swap yaml in Tasks 1–8 (Task 0 and Task 9 do, with backups and the operator-approved order).
- This file and every doc it produces stay free of machine identity strings (the node's Linux user name, LAN/tailnet IPs, hostnames, the ZFS pool name): write `<node-user>`, `<offload-stack>`, `<uid>`, "the Lenovo node". The repo's pre-push leak scan blocks the rest.

---

## File map

| Path | Responsibility |
|---|---|
| `internal/core/types.go` | `TaskTrain` + `Valid()` |
| `internal/config/config.go`, `resolve.go` | `fleet_train_enabled`, `train_axolotl_bin`, `train_workdir`, `train_hf_home`, `train_timeout_sec`, `train_output_dir`, `train_dataset_root`, `train_enabled` (delegator); `Config.ConfigPath` (`json:"-"`, stamped by `LoadWithSource`) |
| `internal/trainlaunch/` (new) | `Launcher` interface; `launch_linux.go` (systemd-run --user, two-stage kill, `AdoptOrKill`); `launch_other.go` stub; `Available()` |
| `internal/trainyaml/` (new) | `Contract` → YAML render from the pinned key allowlist; `schema_keys.json` (checked in); `targets.go` (the R6 pin + `testdata/qwen35-targets.json`); preprocess gate runner with the sample-count parse |
| `internal/fleetnode/train_task.go` (+test) | `TrainPayload`, `buildTrain`, job dir, dataset materialisation |
| `internal/fleetnode/tasks.go`, `server.go` | order + predicate + BuildRequest arm; cap exemption; auth marker |
| `main.go` (`runFleetServe`) | `loadCfgWithSource` (keeps `ConfigPath`); `trainlaunch.AdoptOrKill` BEFORE `SweepOrphanedPipelineJobs` |
| `internal/pipeline/traintask.go` (+test) | the route: preprocess gate → wrapper-leased unit under `WithTimeout` → artifact gate → warm-back after any kill |
| `internal/trainremote/` (new) | delegator forwarder: node pick by `supported_task_types`, dispatch, poll under `timeout+60 s` |
| `internal/delegate/nodeview.go` | `NodeView.SupportedTaskTypes` (additive) |
| `internal/mcpserver/mcpserver.go`, `traintools.go` | `offload_train {action: dispatch\|status}` gated by `train_enabled`; status row |
| `train_cmd.go` | CLI `train run\|status\|dataset` |
| `internal/agent/served.go` (+test) | `ServedTranscript` (the served prefix built by the loop's own code) + `CutMiddleReplay` (export of `cutMiddleTurns`) |
| `internal/traindata/` (new) | dataset helper over `rig.ReadShards` (VERIFIED rows, teacher seats, served-shape render, lint filter, normalised-goal-hash holdout, manifest) |
| docs: `docs/systems/train-lane.md`, ADR (next free after 0039), `docs/FLEET-NODE.md`, `docs/systems/fleet-node.md`, `docs/flows/fleet-job-lifecycle.md`, `docs/OPERATOR-GUIDE.md`, `setup/SETUP-AGENT.md`, `docs/systems/mcp-server.md`, `README.md`, CHANGELOG | D8 |
| `<offload-stack>/measurements/train-lane-<date>/` (on the node, not in the repo) | Task 0 and Task 10/10.5 artifacts: README, logs, eval-loss table, nvidia-smi captures |

---

### Task 0: The 48-hour test (orchestrator; hand-run on the Lenovo node; BEFORE any Go)

**Purpose:** answer the council's question before a line of Go is written: does one epoch of QLoRA on ~200 served-shape verified pairs move eval loss on a held-out slice? Everything in Tasks 1–10 is plumbing for a result this task can show or refute in two days.

**Touches production:** the Lenovo node's GPU and its agent seat (evicted for the training window through the SHIPPED wrapper), read-only access to the delegator's `delegation-log` shards. No node config, unit or llama-swap yaml is changed. Backups are taken anyway (the node's config.json and the llama-swap yaml, to `<offload-stack>/backups/<date>/`).

**Artifacts:** `<offload-stack>/measurements/train-lane-<date>/` with `README.md` (the decision, the numbers, the exact commands), `render.py`, `manifest.json`, `train.jsonl`, `holdout.jsonl`, `preprocess.log`, `train.log`, `eval-before.json`, `eval-after.json`, `nvidia-smi-{before,during,after}.txt`, `axolotl-config-schema.json`, `targets.json`.

- [ ] **Step 1: Freeze the inputs and take the backups.** On the node: `cp <node config.json> <offload-stack>/backups/<date>/config.json`; `cp <llama-swap yaml> <offload-stack>/backups/<date>/llama-swap.yaml`. On the delegator: copy the last 30 days of `BaseDir()/delegation-log/*.jsonl` to `measurements/train-lane-<date>/shards/` (read-only copies; the live directory is never opened for write).
- [ ] **Step 2: Capture the schema and the module sets (R6 inputs).** In the node's venv: `axolotl config-schema --format json > axolotl-config-schema.json` (strip the stdout banner; note the exact axolotl version string at the top of the README). Then, in the same venv, list the module names the Qwen3.5 base exposes: `python -c "from transformers import AutoModelForCausalLM; m=AutoModelForCausalLM.from_pretrained('<base snapshot path>', device_map='meta'); print(sorted({n.split('.')[-1] for n,_ in m.named_modules() if 'proj' in n or n.endswith(('gate','up','down'))}))"` → `targets-model.json`. In the vLLM 0.28 venv on the SAME node (the seat's), print its applied LoRA module set for the served architecture: `python -c "import vllm.lora.utils as u, inspect; print(inspect.getsource(u))" | grep -n "packed_modules_mapping\|supported_lora_modules"` and the model class's `packed_modules_mapping`/`supported_lora_modules` for `Qwen3_5` → `targets-vllm.json`. Record the intersection as `targets.json` with both version strings. If vLLM's applied set does not include the `linear_attn.*` projections, record that: it means only the full-attention quarter is servable, and Task 2 pins the intersection — not the wish.
- [ ] **Step 3: Render ~200 verified pairs in the served shape** (a throwaway `render.py` over the shard copies is acceptable for Task 0 ONLY; Task 7 replaces it with the Go helper). Selection: `acceptance_pass && !deferred && error == "" && result.structured != null && result.setup_ran == len(contract.context)`, seat ∈ the teacher seats (`agent-pool`, `qwen3.8-27b`), `arm == ""`. Row shape (`{"messages":[…]}`), in this order: `system` = the profile's `System` when `contract.profile` names one (read `internal/agent/profiles.go`), else the text `agent.SystemPrompt(false,false,false,false,false,false,false)` produces (copy it from `internal/agent/prompt.go` into the script and note the commit); `user` = `contract.goal` verbatim; `assistant` with `tool_calls` = one `read_file` call per context doc, ids `setup-1..N`, `arguments {"path": <doc name>}`, content `"setup: N action(s) replayed from the contract before the first turn; their results follow. Use them — the documents they return are already in front of you."` (verbatim from `internal/agent/setup.go`); one `tool` message per doc, `tool_call_id: setup-i`, content = the doc rendered as `"<lineno>: <line>"` joined by `\n` (the `read_file` shape in `internal/agent/tools.go`), truncated to the row's `trace[i].obs_chars` (the cap the seat actually saw — if the rendered text is SHORTER than `obs_chars` the row is dropped and counted `obs_mismatch`); `assistant` = `result.output` verbatim. Budget: `Seq = 1024` tokens; count with the seat's tokenizer (`AutoTokenizer.from_pretrained(<base snapshot>)`, `apply_chat_template(messages, tokenize=True)`); a row over 1024 is dropped and counted `over_seq` (Task 0 does not elide — the count is the finding). Holdout: `sha256(normalised goal)` where normalised = lower-case, whitespace collapsed, digit runs → `#`, ISO timestamps removed; the first 10 % of the hash space is `holdout.jsonl`; at most 3 rows per contract hash in `train.jsonl`. Write `manifest.json`: counts (selected, dropped by reason, train, holdout), per output-schema hash, per rigger axis of the SAME contracts' failed siblings (`internal/rig` `Classify` precedence, replicated in the script for Task 0 only), seats, window, `seq: 1024`, `shape: "served/seeded-reads"`. Stop and report if train < 150 rows: the corpus is the finding.
- [ ] **Step 4: Verify the chat template renders the tool turns.** `axolotl preprocess train.yaml --debug` (config from Step 5, dataset `type: chat_template`, `field_messages: messages`, `roles_to_train: [assistant]`) and READ the debug print of one sample: the `tool` turns and the tool-call head must appear in the tokenised text. If the template drops them, the served-shape claim is void for this base — record it in the README and STOP (the decision rule below says what that means).
- [ ] **Step 5: Write `train.yaml` by hand** from the proven `axolotl-env/smoke/qlora-smoke.yaml`: `sequence_len: 1024`, `num_epochs: 1`, no `max_steps`, `lora_target_modules:` = `targets.json`'s intersection, `datasets: [{path: train.jsonl, ds_type: json, type: chat_template, field_messages: messages, roles_to_train: [assistant]}]`, `test_datasets: [{path: holdout.jsonl, ds_type: json, type: chat_template, field_messages: messages, split: train}]`, `val_set_size: 0`, `evals_per_epoch: 4`, `eval_on_start: true` IF `axolotl-config-schema.json` lists that key (grep it — do not assume), otherwise `eval_steps: 1` and treat the step-1 eval as the "before" (say so in the README). Absolute `dataset_prepared_path` and `output_dir`. Run `axolotl preprocess train.yaml | tee preprocess.log`; copy the line that reports the sample/token totals into the README verbatim — Task 2's parser is pinned to THAT line.
- [ ] **Step 6: Train under the shipped wrapper from the NODE's config.** `nvidia-smi > nvidia-smi-before.txt`. Then, as `<node-user>`: `local-offload --config <node config.json> gpu reserve --class media --for <2h> --wait 2m --reason "train-lane task0" --origin task0 --drain --unload-seat -- <venv>/bin/axolotl train train.yaml 2>&1 | tee train.log` with `HF_HOME=<offload-stack>/hf-cache HF_HUB_OFFLINE=1 AXOLOTL_DO_NOT_TRACK=1` exported in that shell. Mid-run: `nvidia-smi > nvidia-smi-during.txt` (the seat must be ABSENT from the process list; if it is present the eviction did not happen — stop, this is the R1 trap reproduced by hand). After exit: `nvidia-smi > nvidia-smi-after.txt`; `curl -s <llama-swap>/running` must list the seat (warm-back verified); `local-offload --config <cfg> gpu status` must print `free`. Record wall time from `train.log` (`train_runtime`) — this is the measured need R2 asks for.
- [ ] **Step 7: Read the eval losses.** From `train.log` / the trainer state JSON in `output_dir`: `eval_loss` at start (or step 1) → `eval-before.json`; the final `eval_loss` → `eval-after.json`. Write the table into the README with the relative drop.
- [ ] **Step 8: Decision rule (write the verdict in the README and in the DESIGN.md status line).** CONTINUE to Tasks 1–10.5 when eval loss on the holdout drops by ≥ 10 % relative (e.g. 1.80 → ≤ 1.62) AND the adapter trained in ≤ 2 h on the 16 GB card. "No meaningful drop" (< 10 % relative, or the Step 4 template check failed, or fewer than 150 train rows exist) → the plan STOPS AFTER TASK 7: Tasks 1–7 still ship (the lane and the helper are cheap and the corpus keeps growing; a later corpus can re-run Task 0 through the lane), Tasks 8–10.5 do not, and the README records why with the numbers. Either way: mem0 (`tier: evidence`, source `train-lane task0`) gets the verdict and the measured wall.

---

### Task 1: Declare and gate — `TaskTrain`, config keys, `Config.ConfigPath`, `trainlaunch.Available()`

**Files:**
- Modify: `internal/core/types.go` (`TaskTrain` beside `TaskAccel`, `Valid()`), `internal/config/config.go` (keys + accessors; `go generate .`), `internal/config/resolve.go` (`LoadWithSource` stamps `ConfigPath`), `internal/fleetnode/tasks.go` (`fleetTaskOrder`, `taskConfiguredFor`), `main.go` (`runFleetServe` uses `loadCfgWithSource`)
- Create: `internal/trainlaunch/launch.go`, `launch_linux.go` (`//go:build linux`), `launch_other.go` (`//go:build !linux`)
- Test: `internal/config/resolve_test.go`, `internal/trainlaunch/launch_test.go`, `internal/fleetnode/train_task_test.go` (first test only)

**Interfaces (produces):**

```go
// core
const TaskTrain TaskType = "train"   // + in Valid()

// config (all omitempty, zero defaults)
FleetTrainEnabled bool   `json:"fleet_train_enabled,omitempty"` // node opt-in
TrainAxolotlBin   string `json:"train_axolotl_bin,omitempty"`   // <venv>/bin/axolotl
TrainWorkdir      string `json:"train_workdir,omitempty"`       // the axolotl-env dir (cwd for preprocess/train)
TrainHFHome       string `json:"train_hf_home,omitempty"`       // the REAL hf-cache; required when fleet_train_enabled (R1: never derived from HOME)
TrainTimeoutSec   int    `json:"train_timeout_sec,omitempty"`   // default trainyaml.DefaultTimeoutSec (Task 0's measured need); = the lease TTL AND the enforced bound
TrainOutputDir    string `json:"train_output_dir,omitempty"`    // default BaseDir()/models/adapters via accessor
TrainDatasetRoot  string `json:"train_dataset_root,omitempty"`  // node-resident datasets may only come from here
TrainEnabled      bool   `json:"train_enabled,omitempty"`       // delegator: registers offload_train + the CLI door
ConfigPath        string `json:"-"`                             // the file this Config was loaded from ("" = built-in defaults); stamped by LoadWithSource, never serialised
func (c Config) TrainTimeout() time.Duration
func (c Config) TrainOutput() string

// trainlaunch
type Spec struct { Unit, Workdir, LogPath string; Args []string; Env map[string]string; RuntimeMax time.Duration }
type Launcher interface {
	Start(ctx context.Context, s Spec) (Handle, error)
}
type Handle interface {
	Wait() (exitCode int, err error) // from systemd-run --wait; never from unit properties
	Kill() error                     // two-stage: SIGINT --kill-whom=main, then cgroup SIGKILL after killGrace (Task 5)
}
const killGrace = 30 * time.Second   // the wrapper's finish() (warm + release) must fit inside it
func Available() (ok bool, reason string) // linux: XDG_RUNTIME_DIR resolvable + `systemctl --user --version` answers; other: false, "train lane is linux-only"
func New() Launcher
func AdoptOrKill(ctx context.Context, unitPrefix string) (killed []string, err error) // Task 5; a no-op that returns nil on !linux
```

`taskConfiguredFor("train")` = `cfg.FleetTrainEnabled && trainlaunch.Available() && cfg.TrainAxolotlBin != "" && os.Stat(cfg.TrainAxolotlBin) ok`; `fleetTaskOrder` gains `"train"` last.

- [ ] **Step 1: Write the failing tests.** `internal/config/resolve_test.go`: `TestLoadWithSourceStampsConfigPath` (a temp config file → `cfg.ConfigPath == path`; a missing explicit path → `""`; defaults → `""`) and `TestConfigPathNeverSerialises` (marshal a Config with `ConfigPath` set; the JSON has no `config_path`/`ConfigPath` key). `internal/fleetnode/train_task_test.go`: `TestTrainIsAdvertisedExactlyWhenEnabledAvailableAndBound` (table over enabled/available/bin-exists; a non-linux build is always false — use a package-level `trainAvailable = trainlaunch.Available` seam swapped in tests), and assert `TestSupportedTasksDerivation` + `TestHealthGoldenShape` stay unchanged (train predicate false on `fullCfg`/`imageCfg`). `internal/trainlaunch/launch_test.go`: `TestAvailableIsFalseOffLinux` (build-tagged) and `TestNewReturnsAFailClosedLauncherOffLinux` (`Start` returns an error naming "linux-only").
- [ ] **Step 2: Run them, see them fail** (`go test ./internal/config ./internal/fleetnode ./internal/trainlaunch` — compile errors on the missing symbols count).
- [ ] **Step 3: Implement.** Keys + accessors (`TrainTimeout()` returns `time.Duration(TrainTimeoutSec)*time.Second`, falling back to `trainyaml.DefaultTimeoutSec` — declare that constant in Task 2's package now as a placeholder-free value read from Task 0's README, e.g. `measured_wall_s × 2` rounded up to 15 min; the Task 1 commit message names the measurement file); `ConfigPath` stamped in `LoadWithSource` when `src.Loaded()`; `runFleetServe` switches `cfg := loadCfg(fs)` to `cfg, _ := loadCfgWithSource(fs)`; `go generate .`; the launcher package with the stub.
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...`.
- [ ] **Step 5: Commit** `"train lane: task type, config keys, ConfigPath stamping and the linux-only launcher gate"` with the Co-Authored-By line.

---

### Task 2: YAML render from a pinned allowlist + the R6 target-module pin + the preprocess gate with a sample-count floor

**Files:**
- Create: `internal/trainyaml/render.go`, `schema_keys.json` (the property names from Task 0's `axolotl-config-schema.json`, checked in with the axolotl version that produced it), `targets.go` + `testdata/qwen35-targets.json` (Task 0's `targets.json`: `axolotl_version`, `vllm_version`, `axolotl_hybrid_set`, `vllm_applied_set`, `intersection`), `preprocess.go`, `testdata/qlora-smoke.golden.yaml`, `testdata/served-epoch.golden.yaml`
- Test: `internal/trainyaml/render_test.go`, `targets_test.go`, `preprocess_test.go`

**Interfaces (produces):**

```go
const Seq = 1024                 // the ONE sequence-length constant (R5); traindata and the contract default read it
const DefaultTimeoutSec = <from Task 0's README: measured 1-epoch wall × 2, rounded up to 15 min>  // written as an integer literal in code, with the measurement file named in its WHY comment
var TargetModulesQwen35Hybrid []string // = testdata/qwen35-targets.json "intersection"; TestTargetPinMatchesTheCapturedIntersection keeps them equal

type Contract struct {
	Name          string   `json:"name"`                          // adapter name, ^[a-z0-9][a-z0-9._-]{1,63}$
	BaseModel     string   `json:"base_model"`                    // HF repo id or node-local snapshot path
	BaseSnapshot  string   `json:"base_snapshot,omitempty"`       // the served base's exact snapshot hash (manifest provenance, R6)
	BaseQuant     string   `json:"base_quant,omitempty"`          // what the SEAT serves (e.g. "w4a16 compressed-tensors"); recorded, never used to configure the trainer
	Method        string   `json:"method"`                        // "qlora" (bnb nf4, load_in_4bit) | "lora" (bf16 base, NO load_in_4bit — the merge→requantise path)
	LoraR         int      `json:"lora_r,omitempty"`              // default 8
	LoraAlpha     int      `json:"lora_alpha,omitempty"`          // default 16
	LoraDropout   float64  `json:"lora_dropout,omitempty"`        // default 0.05
	SequenceLen   int      `json:"sequence_len,omitempty"`        // default Seq; any other value must be ≤ Seq (a larger one is refused: unmeasured)
	MaxSteps      int      `json:"max_steps,omitempty"`           // the SMOKE only (30); mutually exclusive with Epochs
	Epochs        int      `json:"epochs,omitempty"`              // real runs; default 1 when MaxSteps == 0
	MicroBatch    int      `json:"micro_batch_size,omitempty"`    // default 1
	LearningRate  float64  `json:"learning_rate,omitempty"`       // default 2e-4
	TargetModules []string `json:"lora_target_modules,omitempty"` // default TargetModulesQwen35Hybrid; the smoke golden passes the attention-only set explicitly
	DatasetInline string   `json:"dataset_inline,omitempty"`      // JSONL, ≤ 512 KiB, {"messages":[...]} rows
	DatasetPath   string   `json:"dataset_path,omitempty"`        // node-resident, must be under train_dataset_root
	EvalPath      string   `json:"eval_path,omitempty"`           // node-resident holdout.jsonl under train_dataset_root → test_datasets (Task 10.5)
	EvalOnStart   bool     `json:"eval_on_start,omitempty"`       // emitted only when schema_keys.json has the key (Task 0 decided which)
	TimeoutSec    int      `json:"timeout_sec,omitempty"`         // default cfg.TrainTimeout()
	MinSamples    int      `json:"min_samples,omitempty"`         // preprocess floor; default 1 for the smoke, the helper's manifest count for real runs
}
func (c Contract) Validate() error
// Render writes <jobDir>/train.yaml with ABSOLUTE dataset_prepared_path=<jobDir>/prepared and output_dir=<outputRoot>/<name>; every emitted key is in schema_keys.json; datasets use type chat_template / field_messages messages / roles_to_train [assistant]; eval_path renders test_datasets + val_set_size 0 + evals_per_epoch 4.
func Render(c Contract, jobDir, datasetFile, evalFile, outputRoot string) (yamlPath string, err error)
// Preprocess runs `<axolotl> preprocess <yaml>` in workdir with the same env the launcher uses, no GPU, bounded by 10 min; pass = exit 0 AND stdout contains "Success! Preprocessed data path" AND the parsed sample count ≥ minSamples.
func Preprocess(ctx context.Context, axolotlBin, workdir, yamlPath string, env map[string]string, minSamples int) (Report, error)
type Report struct { Log string; Samples int }
```

- [ ] **Step 1: Write the failing tests.** `TestRenderEmitsOnlyAllowlistedKeysAndMatchesTheSmokeGolden` (the smoke contract → byte-equal to `qlora-smoke.golden.yaml`, the proven 30-step recipe with `max_steps: 30` and the attention-only targets passed explicitly); `TestRenderServedEpochMatchesGolden` (a real-run contract: `Epochs: 1`, default targets, `EvalPath` set → `served-epoch.golden.yaml`, which carries `num_epochs: 1`, NO `max_steps`, `test_datasets`, `val_set_size: 0`, `lora_target_modules` = the pinned intersection, `sequence_len: 1024`); `TestRenderLoraWithoutLoadIn4bit` (`Method: "lora"` → no `load_in_4bit`, no `adapter: qlora`, `adapter: lora`); `TestRenderRefusesADatasetPathOutsideTheRoot`; `TestContractValidateRejectsBadNamesMethodsOversizedInlineAndSeqAboveTheConstant`; `TestAllowlistIsASubsetOfTheSchema` (every key `Render` can emit ∈ `schema_keys.json`); `TestTargetPinMatchesTheCapturedIntersection` (`TargetModulesQwen35Hybrid` == `testdata/qwen35-targets.json` `intersection`, and the file's `vllm_version` starts with `0.28`); `TestPreprocessPassIsTheSuccessLineAndTheFloor` (fake axolotl script: exit 0 without the line = fail; the line with `Samples < minSamples` = fail naming both numbers; the sample-count regex is pinned to the exact line Task 0 copied into its README — paste it into the test as the fixture). `TestOnlyOneSequenceLenLiteral`: `grep -rn "sequence_len" --include=*.go` outside `testdata/` finds only `trainyaml.Seq`'s definition and its uses (a Go test that walks the module with `go/parser`, or a pinned `rg` invocation in a `//go:build linux` test — pick one; the point is the constant is unique).
- [ ] **Step 2: Run them, see them fail.**
- [ ] **Step 3: Implement** render, targets, preprocess. Check in `schema_keys.json` and `qwen35-targets.json` from Task 0's artifacts (copy, do not retype).
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...`.
- [ ] **Step 5: Commit** `"trainyaml: contract → axolotl YAML from a pinned allowlist; the Qwen3.5 hybrid ∩ vLLM 0.28 target pin; preprocess is the gate with a sample floor"`.

---

### Task 3: Node — `buildTrain`, job dir, cap exemption, auth marker, and startup adopt-or-kill BEFORE the sweep (R4)

**Files:**
- Create: `internal/fleetnode/train_task.go` (+ `train_task_test.go`, mirroring `accel_task_test.go` `TestAccelTaskIsServedExactlyWhenADeviceIsListed` / `TestBuildAccelWritesTheShippedImageIntoTheJobDir` / `TestBuildAccelRefusals` / `TestAccelIsNotConcurrencyCapped`)
- Modify: `internal/fleetnode/tasks.go` (BuildRequest arm `case "train": return buildTrain(cfg, payload)`), `server.go` (`concurrencyCapped` `case "train": return false` beside `accel`; the bearer gate + poll marker for `train` exactly as for `agent` — the record's `Agent` marker is set by `AcceptAgent`; add a one-line WHY that the marker means "privileged lane" and covers train), `queue_test.go` `TestConcurrencyCappedRule` + `busylease_test.go` `TestAnimateIsExemptFromTheConcurrencyCap` rows, `main.go` `runFleetServe`
- Test: `internal/fleetnode/train_task_test.go`, `main_fleetserve_train_test.go` (ordering pin)

`TrainPayload` = `trainyaml.Contract` decoded with `DisallowUnknownFields`; job dir `MkdirTemp(BaseDir()/pipeline-jobs, "train-*")`; the inline dataset is written to `<jobDir>/dataset.jsonl` (cap enforced before write); `dataset_path`/`eval_path` are verified under `cfg.TrainDatasetRoot` (no symlink escape: `filepath.EvalSymlinks` + prefix check); `Render` produces `<jobDir>/train.yaml`; `core.Request{Task: TaskTrain, Params{"job_dir", "yaml", "contract", "output_dir": <TrainOutput()>/<name>}}`; cleanup removes the job dir only (the output dir persists).

**Startup order (R4), in `runFleetServe`:** `cfg, _ := loadCfgWithSource(fs)` → IF `cfg.FleetTrainEnabled`: `killed, err := trainlaunch.AdoptOrKill(ctx, "offload-train-")` (kills every surviving `offload-train-*` user unit — two-stage — and then runs `local-offload gpu release --warm-seat --config <cfg.ConfigPath>`; logs each unit killed) → THEN `fleetnode.SweepOrphanedPipelineJobs(cfg)`. The order is the whole point: the sweep `RemoveAll`s `pipeline-jobs/train-*` (the live trainer's `train.yaml`, dataset and `train.log`); killing first means the sweep only ever removes a dead job's files. Adoption is NOT attempted (no job record survives a restart; the delegator's poll reads `interrupted`/404 and re-dispatches) — "adopt" in the name is the seam's future, the shipped behaviour is kill+warm.

- [ ] **Step 1: Write the failing tests.** `TestTrainTaskIsServedExactlyWhenEnabledAndBound`; `TestBuildTrainMaterialisesInlineDatasetAndCleansUp`; `TestBuildTrainRefusalsLeaveNoDirs` (unknown field, oversized inline, `dataset_path` outside the root, symlink escape, bad name, `max_steps` + `epochs` both set); `TestTrainIsNotConcurrencyCapped`; `TestTrainReDispatchReAcks`; `TestTrainDispatchRequiresBearerOnATokenlessNonLoopbackListener` and `TestTrainJobPollRequiresBearer` (the job record's marker set); `TestFleetServeKillsTrainUnitsBeforeTheSweep`: with a fake `trainlaunch` seam and a fake sweep recorder (package-level function variables in `main.go`, swapped in the test), assert the call order is `AdoptOrKill` then `SweepOrphanedPipelineJobs`, and that with `FleetTrainEnabled=false` `AdoptOrKill` is never called (byte-identity for non-train nodes).
- [ ] **Step 2: Run them, see them fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...`.
- [ ] **Step 5: Commit** `"fleet node: the train task — strict payload, job dir, cap exemption, bearer gate; kill surviving train units BEFORE the job-dir sweep"`.

---

### Task 4: Pipeline route — preprocess gate → wrapper-leased unit under an enforced timeout → artifact gate → warm-back after any kill (R1, R2, R3)

**Files:**
- Create: `internal/pipeline/traintask.go` (+ `traintask_test.go` with a fake `Launcher`, a fake llama-swap, and a fake `gpuVerb` runner)
- Modify: `internal/pipeline/pipeline.go` (`Run` branch after `TaskAccel` at the `req.Task == core.TaskAccel` arm), `internal/pipeline/gpulease_coverage_test.go` (add the train case by hand: the route takes NO in-process lease — the wrapper does — and the test pins that the node's own env never gains `GPU_LEASE_*`)

**Interfaces (produces):**

```go
// package pipeline
type trainDeps struct {
	launcher trainlaunch.Launcher
	gpuVerb  func(ctx context.Context, args ...string) (string, error) // runs `<self> gpu <args...>`; the test fakes it
	self     string                                                   // os.Executable() at construction
}
func (p *Pipeline) runTrainTask(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result
```

Route (`runTrainTask`), in order:
1. **Config guard (R1):** `p.cfg.ConfigPath == ""` → `Deferf("train: this node runs on built-in defaults (no config file resolved); refusing to start a trainer that would evict a seat from a different lease dir")`. `p.cfg.TrainHFHome == ""` → `Deferf("train: train_hf_home is not set")`.
2. `trainyaml.Preprocess(ctx, bin, workdir, yaml, env, contract.MinSamples)` (no lease; the seat may be resident). Failure → `Deferf("train: preprocess refused the config: <tail>")` → job `error`. `env` here is the SAME map the unit gets (step 4), so the gate and the run resolve the same cache.
3. **Build the unit command (the argv pin):** `[<self>, "--config", <cfg>, "gpu", "reserve", "--class", "media", "--for", <timeout>, "--wait", "2m", "--reason", "train:"+jobID, "--origin", "train", "--drain", "--unload-seat", "--", <axolotl>, "train", <yaml>]`. The `--config` is the GLOBAL flag (`main.go` `hoistGlobalConfig` accepts it before the verb). `--wait 2m` because the shipped default is 8 h (`defaultReserveWait`): a re-dispatch behind an orphan must fail in two minutes. `--unload-seat` implies `--exclusive` (`gpu_cmd.go`), so the delegator's `LeaseBusy`/`LeasedText` gates route contracts elsewhere for the window.
4. `launcher.Start(unitCtx, Spec{Unit: "offload-train-"+jobID, Workdir: cfg.TrainWorkdir, LogPath: <jobDir>/train.log, Args: <step 3>, RuntimeMax: TrainTimeout()+killGrace, Env: {"PATH": <venv>/bin + ":" + os.Getenv("PATH"), "LOCAL_OFFLOAD_CONFIG": <cfg>, "HF_HOME": cfg.TrainHFHome, "HF_HUB_OFFLINE": "1", "AXOLOTL_DO_NOT_TRACK": "1"}})` — **no `HOME` key** (a test asserts its absence). `unitCtx, cancel := context.WithTimeout(ctx, TrainTimeout())`; a goroutine calls `handle.Kill()` when `unitCtx.Done()` fires (timeout OR parent cancel — `DrainAndStop` cancels the store ctx after 30 s and gives 5 s: the SIGINT stage lands inside that; the cgroup SIGKILL may land after the process exits, which is fine because `RuntimeMaxSec` and the next start's adopt-or-kill bound the survivor).
5. `exit, werr := handle.Wait()`. **Warm-after-kill (R3):** if `Kill()` was called (timeout, cancel) → `gpuVerb(bg, "--config", <cfg>, "gpu", "release", "--warm-seat")` with a fresh 15-min background ctx (never the cancelled one), then return `Deferf("train: killed at the <timeout> bound"/"train: interrupted")`. The wrapper's own `finish()` warms on the SIGINT path too; the explicit call is idempotent (`warmSeat` = GET `/upstream/<seat>/health`).
6. Artifact gate: `output_dir/adapter_model.safetensors` non-empty AND `adapter_config.json` present. Success → `core.Result{OK: true, Data: {"adapter_dir", "adapter_bytes", "train_runtime_s" (from train.log if present), "eval_loss_first", "eval_loss_last" (parsed from train.log when `eval_path` was set; absent otherwise), "unit", "log_tail"}}`; else `OK:false` with the 400-byte log tail.
7. Never set `GPU_LEASE_*` in the node's own process.

- [ ] **Step 1: Write the failing tests** (fake launcher records `Spec` and exposes `killed bool`; fake `gpuVerb` records calls; fake llama-swap unused here — the wrapper is not run in unit tests): `TestTrainUnitCarriesTheConfigTriple` (argv has `--config <cfg>` BEFORE `gpu`; `Env["LOCAL_OFFLOAD_CONFIG"] == cfg`; `Env["HF_HOME"] == cfg.TrainHFHome`; `_, hasHome := Env["HOME"]; !hasHome`); `TestTrainUnitArgvIsTheWrapperFormWithWait2m` (exact argv equality against a literal slice); `TestTrainRefusesOnDefaultsConfig` (`ConfigPath == ""` → deferred, launcher never started); `TestPreprocessFailureNeverStartsTheUnit`; `TestExitZeroWithoutTheArtifactIsNotOK`; `TestTrainTimeoutKillsTheUnit` (`TrainTimeoutSec: 1`, fake `Wait` blocks until `Kill`; assert `killed` and the deferred reason names the bound); `TestCtxCancelKillsWithinTheGrace` (cancel the parent ctx; `Kill` observed within 1 s); `TestKillIsFollowedByWarmBack` (after either kill, `gpuVerb` was called with `["--config", cfg, "gpu", "release", "--warm-seat"]` and with a ctx that is NOT done); `TestSuccessPathDoesNotCallWarmBackTwice` (exit 0, no kill → `gpuVerb` never called: the wrapper's `finish()` already warmed); `TestNodeEnvNeverGainsGPULease` (`os.Getenv("GPU_LEASE_EPOCH") == ""` after the route); `TestJobsDeferredResultBecomesErrorWithReason` shape holds for the defers; the `gpulease_coverage_test.go` train row.
- [ ] **Step 2: Run them, see them fail.**
- [ ] **Step 3: Implement** `traintask.go` + the `Run` branch.
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...`.
- [ ] **Step 5: Commit** `"pipeline: the train route — config-threaded wrapper unit under an enforced timeout, artifact-gated success, warm-back after every kill"`.

---

### Task 5: Linux launcher — `systemd-run` with `RuntimeMaxSec`, the two-stage kill, `AdoptOrKill` (R2, R3, R4)

**Files:**
- Modify: `internal/trainlaunch/launch_linux.go`
- Test: `internal/trainlaunch/argv_test.go` (pure, every platform — builds the argv without running anything), `internal/trainlaunch/launch_linux_test.go` (`//go:build linux`, skipped when `systemctl --user --version` fails)

**Interfaces (produces):** `func unitArgv(s Spec) []string` (exported for the test as `UnitArgv`), `func killArgv(unit string, stage int) []string`.

`Start` = `systemd-run --user --unit <unit> --wait --collect --working-directory <workdir> -p RuntimeMaxSec=<int(s.RuntimeMax.Seconds())> -p StandardOutput=append:<log> -p StandardError=append:<log> -E K=V… -- <args>`. `Wait` returns the exit code of `systemd-run --wait` (non-zero = the unit failed; its text carries `Main processes terminated with: code=exited/status=N` or `code=killed`). `Kill` = stage 1 `systemctl --user kill --kill-whom=main -s SIGINT <unit>` → poll `systemctl --user is-active <unit>` every 500 ms until inactive/failed or `killGrace` (30 s) elapses → stage 2 `systemctl --user kill -s SIGKILL <unit>` (whole cgroup) → poll until gone inside 5 s more; returns an error naming the unit if it is still active. `Available` requires `XDG_RUNTIME_DIR` (from env, else `/run/user/<uid>`) to exist and `systemctl --user --version` to answer. `AdoptOrKill(ctx, prefix)` = `systemctl --user list-units --plain --no-legend '<prefix>*'` → for each, `Kill` (two-stage) → returns the names; the caller (Task 3) runs the warm-back.

- [ ] **Step 1: Write the failing tests.** `TestUnitArgvCarriesRuntimeMaxSec` (`Spec{RuntimeMax: 90*time.Minute + 30*time.Second}` → argv contains `-p RuntimeMaxSec=5430` as one element pair, exactly once); `TestUnitArgvNeverEmitsHOME` (a `Spec.Env` with a `HOME` key is REFUSED by `Start` with an error, so the R1 invariant is enforced at the seam, not only at the caller); `TestKillArgvStages` (stage 1 has `--kill-whom=main -s SIGINT`, stage 2 has `-s SIGKILL` and no `--kill-whom`); linux integration: `TestStartKillLeavesNoUnit` (`Start` `/bin/sh -c 'trap "" INT; sleep 60'` — ignores SIGINT, so stage 2 must land — then `Kill`; assert `is-active` reports inactive/failed within `killGrace+5s` and `Wait` returns a killed code); `TestStartTrueExitsZero`; `TestRuntimeMaxSecKillsWithoutTheParent` (`Spec{RuntimeMax: 2s}` running `sleep 30`; `Wait` returns non-zero within ~3 s with no `Kill` call); `TestAdoptOrKillFindsThePrefix` (start two units with the prefix, one without; `AdoptOrKill` returns exactly the two).
- [ ] **Step 2: Run them, see them fail** (argv tests fail on every platform; the linux ones on the Lenovo node's user session or any Linux box with a user manager — record where they ran in the commit body).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...`; on Linux additionally `go test ./internal/trainlaunch -run 'TestStart|TestRuntime|TestAdopt' -v`.
- [ ] **Step 5: Commit** `"trainlaunch: systemd-run --user transient units with RuntimeMaxSec, a two-stage kill (SIGINT main, then cgroup SIGKILL) and startup adopt-or-kill"`.

---

### Task 6: Delegator doors — `trainremote`, `NodeView.SupportedTaskTypes`, CLI `train run|status`, MCP `offload_train`

**Files:**
- Create: `internal/trainremote/trainremote.go` (+ `trainremote_test.go` using the `accelremote_test.go` `newFakeNode` fixture pattern), `train_cmd.go`, `internal/mcpserver/traintools.go`
- Modify: `internal/delegate/nodeview.go` (`healthWire.SupportedTaskTypes []string \`json:"supported_task_types"\`` → `NodeView.SupportedTaskTypes`; pin absent = nil), `internal/mcpserver/mcpserver.go` (literal `Name: "offload_train"` inside `if s.p != nil && s.p.Cfg().TrainEnabled`), `.printing-press.json` (`mcp.tools` gains `offload_train`), `internal/mcpserver/badargs_test.go` table row, `main.go` (verb + usage line), `offload_status.fleet.nodes[].supported_task_types` (additive, only when published)
- Test: `internal/trainremote/trainremote_test.go`, `internal/delegate/nodeview_test.go` (`TestFetchNodeViewDecodesSupportedTaskTypes`), `internal/mcpserver/traintools_test.go` (`TestOffloadTrainRegistrationGated` — differential tools/list)

`trainremote.Dispatch(ctx, cfg, contract) (jobID, node, base string, err)` — picks the first `delegate_remotes` node whose health `supported_task_types` contains `train` (2 s probe), POSTs the envelope `{job_id: "train-"+16hex, task_type: "train", payload}` with the bearer, requires 202. `trainremote.Status(ctx, cfg, base, jobID) (state, data, err)` polls once; `Wait` polls every 3 s under `timeout_sec + 60 s` and, on expiry, returns an error that says the NODE's bound (`RuntimeMaxSec`) will end the unit — the delegator never assumes a stopped poll stopped the trainer (R2). The MCP tool is async: `{action: "dispatch", contract…}` → `{job_id, node, base}`; `{action: "status", job_id, base}` → the job wire; the CLI `train run --contract f.json [--wait]` and `train status --job <id> --base <url>` mirror it.

- [ ] **Step 1: Write the failing tests** as listed above plus `TestDispatchPicksOnlyANodeAdvertisingTrain` and `TestWaitExpiryNamesTheNodeBound`.
- [ ] **Step 2: Run them, see them fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...` (`TestPrintingPressManifestListsEveryTool` must stay green).
- [ ] **Step 5: Commit** `"train lane: delegator doors — forwarder, CLI, gated MCP tool, node capability decode"`.

---

### Task 7: Dataset helper in the SERVED shape (R5, R10) — independent; can run in parallel with Tasks 3–6

**Files:**
- Create: `internal/agent/served.go` (+ `served_test.go`), `internal/traindata/` (`render.go`, `select.go`, `holdout.go`, `manifest.go`, `write.go`, tests on synthetic shards), CLI `train dataset` in `train_cmd.go`
- Modify: nothing else in `internal/agent` (the new file only calls package-private code)

**Interfaces (produces):**

```go
// package agent — served.go
// ServedTranscript builds the transcript prefix a grounded, seeded contract
// puts in front of the seat: system (profile System or the default
// SystemPrompt with every capability off), the goal as the user turn, the
// setup head + one tool message per doc — built by the SAME code paths
// Loop.Run uses (replaySetup's head text, scope.readFile's numbered
// rendering, contextbudget.Trim at toolResultCapChars for ctxTokens). It runs
// the real read_file tool over a temp dir holding the docs, so the rendering
// cannot drift from production. obsCaps, when non-nil, are the per-doc
// obs_chars the corpus row recorded: a rendered doc whose length differs is
// an error (the row is not the transcript the seat saw).
func ServedTranscript(profile, goal string, docs []core.ContextDoc, ctxTokens int, obsCaps []int) ([]Msg, error)
// CutMiddleReplay exposes cutMiddleTurns unchanged for the dataset helper (the
// compaction eval's CompactReplay is the precedent). fits=false means the forced
// keeps alone exceed the budget.
func CutMiddleReplay(ctx context.Context, tok Tokenizer, msgs []Msg, realBudget, protectedPrefix, keepRecent int) (out []Msg, fits, ok bool)

// package traindata
const Seq = trainyaml.Seq
type Options struct {
	LogDir, Out       string
	TeacherSeats      []string
	Since, Until      time.Time
	HoldoutFrac       float64 // default 0.10
	PerContract       int     // default 3
	IncludeArms       bool
	TokenizeBase      string  // the SERVED seat's llama-swap base (tailnet) — required; vetted by netguard like seat_endpoints
	Seat              string  // the alias /tokenize is asked under
}
func Build(ctx context.Context, o Options) (Manifest, error)
type Manifest struct {
	Shape        string         `json:"shape"`          // "served/seeded-reads"
	Seq          int            `json:"seq"`            // 1024
	Seats        []string       `json:"teacher_seats"`
	Window       [2]string      `json:"window"`
	Selected     int            `json:"selected"`
	Train        int            `json:"train"`
	Holdout      int            `json:"holdout"`
	Dropped      map[string]int `json:"dropped"`        // not_verified, arm, seat, setup_incomplete, obs_mismatch, lint_parrot, lint_ungrounded, over_seq_after_cut, ungrounded_by_cut, pii, per_contract_cap
	PerSchema    map[string]int `json:"per_schema"`     // post-filter train rows per sha256(output_schema)
	PerRigAxis   map[string]int `json:"per_rig_axis"`   // the failed SIBLINGS (same normalised goal hash) of kept contracts, by rig.Classify axis — the P4b gate's input
	MinSamples   int            `json:"min_samples"`    // = Train; the contract's preprocess floor
	Tokenizer    string         `json:"tokenizer"`      // base + seat
	SystemSource map[string]int `json:"system_source"`  // "profile:<name>" / "default"
}
```

Rules:
- Rows via `rig.ReadShards(BaseDir()/delegation-log, since, until)`. VERIFIED = `acceptance_pass && !deferred && error == "" && result != nil && len(result.structured) > 0` (the structured re-pack succeeding is the proof the prose answer was schema-complete; the TARGET is still `result.output`). Exact seat-name match against `TeacherSeats`. `arm != ""` dropped unless `IncludeArms`.
- **Served-shape gate:** `result.setup_ran == len(contract.context)` else dropped `setup_incomplete` (the seat read the docs itself at unknown offsets — not reconstructible). `obsCaps` = the `obs_chars` of the trace steps with `setup: true`, in order.
- **Lint filter (R5):** `delegate.LintAcceptance(row.Contract)` re-run at render time (the intake lint is not persisted); any warning containing `PARROT-PASSABLE` → `lint_parrot`, `UNGROUNDED` → `lint_ungrounded`; both dropped.
- **Render:** `agent.ServedTranscript(contract.Profile, contract.Goal, contract.Context, ctxTokens, obsCaps)` + the assistant target `{Role: "assistant", Content: result.output}`. `ctxTokens` = the seat's served window as the node resolved it — read from `result.seat_config_*` when present, else the `Seq`-derived default; recorded per row.
- **Truncation, production's own:** budget = `Seq − answerTokens − 16`; first `agent.CompactReplay(prefix, budget, agent.DefaultKeepRecent, protectedPrefix, ReplayOpts{})` (the elide rung — the elision marker the model sees in production), then `agent.CutMiddleReplay` with `tokclient.New(TokenizeBase, Seat, 0)`; `ok=false` (no /tokenize) is a hard error — a dataset is never rendered on the chars/4 estimate. A row whose `tool` messages were all dropped by the cut is `ungrounded_by_cut` (the answer would be trained without its evidence); a row still over budget after the cut is `over_seq_after_cut`. Never touch the goal or the answer.
- **Holdout (R5):** `normGoal` = lower-case, `\s+` → one space, `\d+` → `#` (the rigger's `nonAlnumDigits` precedent), RFC3339/ISO timestamps removed; `key = sha256(normGoal + "\n" + sorted doc names)`; holdout = the first `HoldoutFrac` of the key space (`key[0] < 256*HoldoutFrac`); a key's rows never split. `PerContract` cap on the train side, counted.
- `VetPII` (reuse `compeval.VetPII` over an `Entry` built from the row's turns) before write; a finding drops the row (`pii`) and is listed in the manifest by class, never by content.
- Atomic temp+rename; write `train.jsonl`, `holdout.jsonl` (each holdout line keeps the FULL contract under `"_contract"` for a later replay — axolotl ignores unknown top-level keys in `chat_template` rows; verify in the Task 0 preprocess debug print that it does), `manifest.json`.
- **Push step (R10), documented in the CLI help and `docs/OPERATOR-GUIDE.md`:** `scp <out>/train.jsonl <out>/holdout.jsonl <out>/manifest.json <node-user>@<lenovo tailnet name>:<train_dataset_root>/<set-name>/` then dispatch with `dataset_path: <train_dataset_root>/<set-name>/train.jsonl`, `eval_path: …/holdout.jsonl`, `min_samples: <manifest.train>`. The inline slice (≤ 512 KiB) stays for the smoke only.

- [ ] **Step 1: Write the failing tests.** `internal/agent/served_test.go`: `TestServedTranscriptEqualsARealLoopPrefix` — run a real `Loop` (fake planner that answers immediately) with `SetupActions = core.SeedContextReads(docs)` over the same docs and assert `Result.Transcript[:len(prefix)]` equals `ServedTranscript(...)` message-for-message (Role, ToolCallID, Content, ToolCalls); `TestServedTranscriptRefusesAnObsMismatch`; `TestCutMiddleReplayMatchesTheUnexportedRung`. `internal/traindata`: `TestVerifiedPredicate`; `TestSetupIncompleteRowsDropped`; `TestLintFilterDropsParrotAndUngrounded` (contracts whose acceptance is satisfied by the goal text); `TestHoldoutGroupsByNormalisedGoal` (the same goal with a different timestamp and different digits lands on the SAME side; a contract repeated 56× lands entirely on one side); `TestPerContractCap`; `TestUngroundedByCutIsDroppedNotTrained`; `TestManifestCountsPerSchemaAndPerRigAxis` (synthetic siblings failing on `schema-miss` / `loop` show up under those axes); `TestPIIRefusal`; `TestTokenizerUnreachableIsAnError`.
- [ ] **Step 2: Run them, see them fail.**
- [ ] **Step 3: Implement** `served.go`, then `traindata`, then the CLI verb `train dataset --teacher-seats agent-pool,qwen3.8-27b --tokenize-base <url> --seat <alias> --out <dir> [--since 30d] [--holdout 0.1] [--per-contract 3] [--include-arms]` (no `--seq` flag: the constant is the constant).
- [ ] **Step 4: Run the tests, see them pass;** `go build ./... && go vet ./... && go test ./...`.
- [ ] **Step 5: Commit** `"traindata: the delegation corpus in the SERVED shape — seeded read_file turns, production truncation, lint filter, normalised-goal holdout, per-axis manifest; documented push to train_dataset_root"`.

---

### Task 8: Docs, ADR, CHANGELOG, version (only if Task 0's rule said CONTINUE)

- `docs/systems/train-lane.md` (`## Purpose`, the flow, the unit/privilege model, the three timeout bounds, the config-threading rule, the eviction rule, the warm-after-kill rule, "a node deploy kills a train in flight by design", the artifact gate, the served-shape dataset and the push step, what P4b adds, `## Source map`); ADR "Training is a lane and adapters are gated" (next free number after 0039; Accepted, with the operator's 2026-09-07 order and the council's 2026-09-10 RESHAPE as provenance); `docs/FLEET-NODE.md` task table (`train` row — note the missing `accel` row as a separate proposal, not fixed silently unless trivial), config keys table; `docs/systems/fleet-node.md` "The train task" + routes row + the startup order (adopt-or-kill before the sweep); `docs/flows/fleet-job-lifecycle.md`; `docs/OPERATOR-GUIDE.md` (dataset section with the scp push; node unit drop-in for `XDG_RUNTIME_DIR`/`DBUS_SESSION_BUS_ADDRESS`; the R8 sandbox probe); `setup/SETUP-AGENT.md` (the keys incl. `train_hf_home`, the adapter dir, the drop-in); `docs/systems/mcp-server.md` (`offload_train`); `README.md` tool table; CHANGELOG entry; version carriers bumped to the next free number.
- [ ] `go test ./...` + `TestDocsLint` + `TestVersionSourcesAgree` + `TestPrintingPressManifestListsEveryTool` green; commit `"<next version>: the train lane — axolotl as a harnessed fleet task"`.

---

### Task 9: Lenovo node deploy prep (orchestrator; production node; backups; R8)

- [ ] Build `GOOS=linux`; copy beside the running binary (never over it until the probe passes).
- [ ] **Sandbox probe (R8) — from INSIDE the node's sandbox, not a `sudo -u` shell:** `sudo systemd-run --pipe --wait --uid=<node-user> -p ProtectSystem=strict -p ProtectHome=yes -E XDG_RUNTIME_DIR=/run/user/<uid> -E DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus -- systemd-run --user --wait /bin/true` must exit 0. Then the same probe with `-- <new binary> --config <node config> gpu status` must print the lease state (proves the config resolves from the sandbox). Only then write the drop-in for `offload-fleet-node`: `Environment=XDG_RUNTIME_DIR=/run/user/<uid>` and `Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` (ProtectHome=yes does not cover `/run/user`).
- [ ] Config keys (backup first): `fleet_train_enabled: true`, `train_axolotl_bin: <offload-stack>/axolotl-env/.venv/bin/axolotl`, `train_workdir: <offload-stack>/axolotl-env`, `train_hf_home: <offload-stack>/hf-cache`, `train_output_dir: <offload-stack>/models/adapters`, `train_dataset_root: <offload-stack>/datasets`; `mkdir` both dirs; `train_timeout_sec` left unset (the measured default).
- [ ] Restart the node (30 s drain); health shows `supported_task_types` containing `train`; the Qube/Aorus never advertise it; `gpu status` from the node's config prints `free`.

### Task 10: Live acceptance on the Lenovo node (LAST; artifacts under `<offload-stack>/measurements/train-lane-<date>/`)

1. Preprocess gate with the seat RESIDENT (no eviction): a contract with `min_samples` above the inline slice's row count → job `error` naming both numbers; the seat never moved.
2. Dispatch the 30-step smoke (the 10-row inline dataset) with the seat RESIDENT → expect drain → unload → 30/30 → adapter at `<output>/<name>/adapter_model.safetensors` (≈42.5 MB) → seat warmed back; journal excerpt showing `-p RuntimeMaxSec=`, `nvidia-smi` before/during/after, health lease block during (`class: media`, `busy: true`), job data.
3. **Node restart mid-train (R4 rewrite):** dispatch again; 60 s in, `systemctl restart offload-fleet-node`. Expect: the stopping node marks the job `error: interrupted`; within `killGrace` there is no `axolotl.cli.train` process (`ps -eo cmd | grep "[a]xolotl.cli.train"` empty), `nvidia-smi --query-compute-apps` empty, `gpu status` free; the starting node logs `killed 0 or 1 offload-train-* unit(s)` then the sweep; the seat is warm (`/running` lists it) within N s of the node coming back, N = the seat's measured load time + 30 s (record the measurement); the job dir is gone; the adapter dir holds no partial checkpoint that the artifact gate would accept.
4. **Re-route (R9):** during a train, `local-offload delegate` a grounded contract from the Qube with the Lenovo node in `delegate_remotes`: the run's placement reason names another node, and `offload_status`/the run report lists the Lenovo lane as "(long GPU lease held)" — the `remoteEligible` `!LeaseBusy` gate. Never "or waits". After warm-back, `fleet-smoke` PASS on every node.
5. Timeout bound: dispatch with `timeout_sec: 120` and `epochs: 1` on the real set → job `error` naming the bound at ~120 s; the unit is gone; the seat is warm.
6. Records: gates NOTES, measurement README, mem0, the DESIGN.md status line.

### Task 10.5: The earn-its-keep gate (R7) — through the lane, on the real corpus

- [ ] `train dataset` on the Qube (Task 7) → `scp` push (Task 7's documented step) → dispatch `{method: qlora, epochs: 1, dataset_path, eval_path, min_samples: <manifest.train>}` → job data carries `eval_loss_first` and `eval_loss_last`.
- [ ] Compare with Task 0's table (same holdout construction; the numbers should agree within noise — a divergence is a rendering bug in Task 7, not a result).
- [ ] **Pass/fail rule:** PASS = `eval_loss_last ≤ 0.90 × eval_loss_first` on the holdout (≥ 10 % relative drop) AND `manifest.train ≥ 150` AND the wall ≤ `TrainTimeout()`. On PASS, P4b (serving, the fidelity gate, "bind only on pass") is justified and the ADR's status line says so. On FAIL the lane stays shipped as plumbing, P4b is NOT opened, and the README records the numbers and the most likely cause from the manifest (few rows per schema, rows lost to `over_seq_after_cut`, or an axis the corpus never covered).
- [ ] Records: measurement README table (before/after, rows, wall, targets pinned), mem0 (`tier: evidence`), the DESIGN.md status line.

## Out of this plan (P4b, next — only on a Task 10.5 PASS)
Serving the adapter: one seat restart to add `--enable-lora --max-lora-rank 16` + `VLLM_ALLOW_RUNTIME_LORA_UPDATING=1` (dynamic loads afterwards), a distinct llama-swap CANDIDATE entry (`useModelName: <adapter>` — the base entry's rewrite would silently measure the base), the fidelity gate (needle + the 8-digest set before/after, blind-judged), and "bind only on pass". If Task 2's pin shows vLLM 0.28 applies only the full-attention projections, P4b also decides whether the bf16 `method: lora` → merge → requantise path replaces adapter serving.
