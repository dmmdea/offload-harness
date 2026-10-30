# Operator Guide — running, driving & diagnosing the stack

Task-oriented walkthroughs for an agent (or human) operating an installed offload-harness stack.
Each task is **goal → commands → expected output → failure table**. Commands are given for
**PowerShell** (native Windows install) and **bash** where applicable. `$OFFLOAD_HOME` defaults to
`$HOME\offload-stack`; the harness config lives at `~/.local-offload/config.json`. If no config file resolves at all, every command warns on stderr that it is running on BUILT-IN DEFAULTS (machine bindings inactive) — `local-offload doctor` shows the resolved source on its `config:` line; never debug a binding problem without checking that line first.

> **Verified on this machine:** the commands marked ✅ below were spot-executed verbatim while
> writing this guide (NVIDIA/CUDA host, llama-swap live on :11436). Output snippets are real.

---

## 1. Start / stop / restart the whole stack

### Native Windows (the primary path)

**Start** llama-swap (loopback-only, the user's long-running service):

```powershell
& "$env:OFFLOAD_HOME\llama-swap\llama-swap.exe" --config "$env:OFFLOAD_HOME\llama-swap.yaml" --listen 127.0.0.1:11436
```

**Verify it's up** ✅:

```powershell
& "$env:OFFLOAD_HOME\harness\local-offload.exe" --config "$HOME\.local-offload\config.json" doctor
```
```
config:     ~/.local-offload/config.json
endpoint:   http://127.0.0.1:11436/v1/chat/completions
model:      offload-e4b
health:     OK
model:             OK    offload-e4b
triage_model:      OK    gemma4-e2b
escalation_model:  OK    gemma4-26b-a4b
escalation:        OK    gemma4-26b-a4b
...
```

The `escalation:` line is the semantic verdict on the cascade: `UNAVAILABLE` when `escalation_model` is empty or equals `model` (a single-model cascade, so the confidence gate is inert), `BROKEN` when the alias is not in the live roster, `OK` otherwise.

**Stop:** Ctrl-C the llama-swap window, or:

```powershell
Get-Process llama-swap,llama-server -ErrorAction SilentlyContinue | Stop-Process -Force
```

**Restart:** stop, then start. llama-swap lazy-loads a model on the first chat request, so a fresh
start shows no model resident until you call one.

### WSL / NVIDIA legacy GUI path

`scripts/openwebui-stack.sh` brings up the coding-agent server (:18800) **and** OpenWebUI (:8081) in
one shot (idempotent — skips whatever is already up):

```bash
bash scripts/openwebui-stack.sh
# -> starting agent server on :18800 ...
# -> starting OpenWebUI on :8081
# -> stack UP — open http://localhost:8081
```

Override defaults via env before running: `LOCAL_AGENT_MODEL` (default `offload-e4b`),
`LOCAL_AGENT_WORKSPACE`, `LOCAL_AGENT_CAPS`, `LOCAL_AGENT_MAX_STEPS`, `LOCAL_AGENT_MAX_SAME_TOOL`.
Logs: `/tmp/agent-server.log`, `/tmp/openwebui.log`.

| Failure | Fix |
|---|---|
| `doctor: health: DOWN` | llama-swap not running / wrong port. Start it; confirm `--listen 127.0.0.1:11436`. |
| `stack did not confirm ready` | Read the two `/tmp/*.log` files; usually OpenWebUI still installing or the agent port busy. |
| alias `FAIL — not in the live roster` | The name is served under neither a canonical id nor a `meta.llamaswap.aliases` entry — the yaml doesn't serve it, or a model file is missing. Check `llama-swap.yaml`. Vision/STT aliases are legitimately absent on a grunt-work-only install. |

### Fleet node (`fleet-serve`) — accept dispatched renders from other boxes

Full guide: `docs/FLEET-NODE.md`. The short operating loop:

**Start** (loopback for a local check; the **Tailscale address** for production — the
lanes all require the fleet bearer, so a node there needs `fleet_auth_token` in its config or it will not
start; port **18811**, **never `0.0.0.0`**):

```powershell
local-offload fleet-serve                                                    # loopback smoke
local-offload fleet-serve --listen 192.0.2.7:18811 --listen-trusted-network      # production (Tailscale addr)
```

**Verify:**

```powershell
curl http://127.0.0.1:18811/fleet/health
# {"node_id":"node-a","schema_version":1,...,"vram_total_gb":15.9,"supported_task_types":["image-gen",...],"model_footprints":[...],"queue_depth":0}
```

An empty `model_footprints` on a fresh box is expected — prime it once with
`local-offload fleet-measure` (one minimal render per configured task; prints the recorded
entries).

**Stop:** Ctrl-C. The server drains: new dispatches 503, in-flight renders get up to 30s,
survivors are marked terminal `error:"interrupted"` so the dispatcher's pollers always
resolve.

| Failure | Fix |
|---|---|
| `refusing to bind --listen` | Non-loopback address without `--listen-trusted-network`. Use loopback, or add the flag for the Tailscale bind only. |
| HTTP `421` / `403` / `415` from the node, `fleet-ui` or `local-agent --serve` | `fleet_ingress_guard` set to `enforce` refused the request: 421 a `Host` that is not loopback, a tailnet address, a dotless name or a name under a zone in `tailnet_suffix` / `tailnet_suffixes`; 403 an `Origin` header (a browser); 415 a POST without `Content-Type: application/json`. The log line names which and from where. `warn` (the default) logs the same and serves it; `off` skips the checks. |
| `GPU probe failed` at startup | `nvidia-smi` missing/broken. By design — a node that can't report VRAM would be treated as broken by the dispatcher. Fix the driver first. |
| footprints look wrong vs Afterburner | Run the PDH-vs-Afterburner validation in `docs/FLEET-NODE.md`; >15% off → set `"fleet_sampler":"global"`. |

---

## 2. Chat with a model directly

### curl (against llama-swap) ✅

```bash
curl -s http://127.0.0.1:11436/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"offload-e4b","messages":[{"role":"user","content":"Reply with one word: ready"}],"max_tokens":16,"temperature":0}'
```

Returns a standard OpenAI chat completion. Switch `model` to `gemma4-e2b` (fast) or `gemma4-26b-a4b`
(MoE) — the first request to a different alias triggers llama-swap to evict the current model and
cold-load the new one (a few seconds).

### OpenWebUI

Open `http://localhost:8081`, create your account on first launch (auth is ON by design — do not set
`WEBUI_AUTH=false`), pick the advertised model, and chat. In the GUI, the "model" is the agent
server, so each message runs a full agent loop (see §4), not a bare completion.

---

### Moving the harness to another drive ✅

```powershell
local-offload install volumes --data     # which volume should hold its data? (never a cloud-synced or FAT volume)
local-offload data status                # where is the data now, and is any of it on the OS drive?
# stop fleet-serve and every MCP door, then COPY the tree (never move it, never a junction):
local-offload data migrate --to D:/local-offload --apply --stopped
# then in config.json:  "home": "D:/local-offload"   (the verb prints the exact line)
local-offload doctor                     # every derived path now under the new root
```

`home` (or `$LOCAL_OFFLOAD_HOME`) rebases every path still at its default; anything you set
explicitly is left exactly as you typed it. The machine-wide GPU lease root is never
rebased — it must stay machine-wide.

On Windows the OS drive holds Windows and program installs, never data: a fresh `install.ps1` writes
`home` onto the data drive `install volumes --data` picks (`OFFLOAD_DATA_HOME` names another;
`OFFLOAD_ALLOW_OS_DATA=1` keeps it on the OS drive on purpose), and `doctor` FAILs a node whose data
still sits on the OS drive while a data drive could take it. `data migrate` is a dry run until
`--apply`, holds back the bbolt stores (`*.db`) until `--stopped`, verifies every copy, and leaves the
old tree for you to delete once the new home has run clean.

### Capability report — send this instead of describing your box ✅

```powershell
local-offload report --out capability-report.md      # Windows
./local-offload report --out capability-report.md    # Linux
```

One read-only Markdown document: harness version and platform, which config file is actually
loaded, the hardware tier from the installer manifest (when the config's `tier_profile` names another tier, the line shows both and says they
disagree), every configured model alias measured
against the live `/v1/models` (matched against canonical ids **and** `meta.llamaswap.aliases`),
and every media route with its derived verdict — plus a
**Needs attention** section that lists only the genuinely broken ones (a route this box never
bound is a legitimate machine, not a fault).

It loads no model, queues no GPU work, and changes nothing. Attach it when asking for help, or
when a collaborator asks "what can that machine do?" — it answers from the same code paths the
harness routes on, so it cannot claim a capability the box would defer. A report generated on
BUILT-IN DEFAULTS says so on its config line: that machine's real bindings are inactive and every
verdict below it describes defaults, not the install.

## 3. Run an offload task + read the ledger

An offload task returns a verified JSON result or a structured defer. ✅

```powershell
"The Q3 review: infra costs fell 12% after moving to reserved capacity; the support backlog cleared." |
  local-offload --config setup\templates\config.json summarize - --max-points 3 --json
```
```json
{
  "ok": true,
  "result": { "bullets": ["Infrastructure costs decreased by 12% ...", "..."], "summary": "..." },
  "meta": { "tokens_in": 115, "tokens_out": 123, "tok_per_s": 87.0, "model": "offload-e4b", "grounded": true }
}
```

Other tasks: `classify - --labels bug,feature,question`, `extract - --schema fields.json`,
`triage - --question "Is this an error?"`. A `{"deferred":true,"reason":"..."}` result is **normal**
for low-confidence / over-long input — hand it back to yourself.

**Read the ledger** (token-savings accounting) ✅:

```powershell
local-offload --config setup\templates\config.json ledger --since 1
```
```json
{"calls":11,"completed":11,"deferred":0,"tokens_saved":2920,"tokens_out":755,
 "est_value_kept_local":0.0438,"by_task":{"summarize":3,"triage":8}}
tokens kept local (est.): 2920 (~$0.04 Opus-input value — an estimate, not billed savings)
```

Every row (0.124.0, ADR 0046) also says WHO asked and WHAT ran: `origin_session` (the Claude Code
session id the MCP server inherited from its environment, or `LOCAL_OFFLOAD_ORIGIN` when a caller names
itself; on a fleet node's row, the session of the box that asked, which it sends as `X-Offload-Origin` since ADR 0087 —
absent when the asker is older or has none, and on a service's own row), `origin_pid` / `origin_ppid`, `cards_tokens`
(the one figure for the tokens the cards processed for that row — prompt work plus generation, 0 on a
cache hit), and on agent / delegate rows `job_id`, `route`, `placement`, `steps`, `stop_reason`,
`repack_ms`, `acceptance_result`. Since ADR 0064 every delegate row also carries `door`, `fleet_job_id`
(the id the fleet node knows the job by; the node's own agent row carries the same one), a closed
`reason_code` (`ok`, `queue_deadline`, `queue_withdrawn`, `stall_prefill`, ... see the
[delegation ledger row](FLEET-NODE.md#the-delegation-ledger-row); every deferred row of any other task carries one too) and the whole `reason`, and a
`phase: "started"` marker row is written when a job is handed to a seat; a marker is never a job, so a
reader that counts rows must skip it. A per-session share is one filter away:
`grep '"origin_session":"<session id>"' ~/.local-offload/ledger.jsonl`. Rows written before 0.124.0
carry none of these and must be read as unattributed, not as another session's.

**Test, bake and measurement traffic is labelled and kept off the production views** (register F48, job-error
class C23). A row carries `run_class` when the run that wrote it was not real work: `test`, `bake`,
`measurement`, or a label your tool chose. An unlabelled row is production, so every ordinary row is
byte-identical to before. `Record` settles the class on the one path every writer takes, from (first match):

1. what the writer set on the row;
2. the process: `LOCAL_OFFLOAD_RUN_CLASS=bake` (any lower-case `[a-z0-9_-]` label up to 32 bytes; `production` sets
   nothing), or an origin of the form `bake:run-7` / `test:...` / `measurement:...` in `LOCAL_OFFLOAD_ORIGIN`.
   **A bake or measurement tool sets one of these once, and everything it runs is labelled**;
3. a test binary that opened a ledger outside its own throwaway directory (`test`; this is how test rows reached
   the real ledger on 2026-10-03 and 2026-10-08);
4. what the row says itself: a call pinned with `pin_reason: measurement`, the `cli:fleet-measure` door
   (`measurement`), the `cli:fleet-smoke` door, the `go-test` origin the repo's own suites set (`test`; rows
   written before the label existed read the same way).

Nothing is guessed from a seat name. A seat that is not in the served roster is a real misconfiguration (a
mistyped `agent_model` fails exactly like a bake's fixture seat) and stays on every view; a bake that wants to be
off them says so with the variable.

`ledger`, `loupe`, `stats` and `audit-sample` leave the labelled jobs out by default and **say how many they
left out**: `ledger` adds `"excluded_non_production": {"bake": 40, "test": 3}` to its JSON and a `left out:`
line, `loupe` the same field and line, `stats` and `audit-sample` a note on stderr (their stdout keeps its
shape). `--include-test-traffic` counts them again. The inner `agent` row of a labelled job is left out with its
parent. `confhead-eval`, `confhead-calibrate`, `train-confhead`, `calibrate`, `health` and `train-router` read
production rows only, since a fixture's margin would move the live thresholds. The raw reader
(`ledger.ReadAll`) still returns every row.

On PAIR, a process with a class opens **no card** and asks no fleet node to card its jobs, and a delegation
pinned for `measurement` opens none. `LOCAL_OFFLOAD_PAIR_TEST_CARDS=1` keeps the cards of a labelled run (to test
PAIR itself). `fleet-smoke` keeps its card: it is the check that cards work.

```powershell
$env:LOCAL_OFFLOAD_RUN_CLASS = "bake"        # everything this shell runs is a bake: labelled rows, no PAIR card
local-offload ledger --since 2                # production only, with "excluded_non_production"
local-offload ledger --since 2 --include-test-traffic
```

A bake that dispatches to a fleet node labels the node's row too: the asker sends its class as `X-Offload-Run-Class`
(and its session as `X-Offload-Origin`), and a node that can authenticate the request stamps both (ADR 0087), so
`local-offload ledger` on the node leaves the job out and counts it. A node built before this ignores the header; its
row stays unlabelled.

**Where a session's tokens ran.** `local-offload ledger --by-session` (optionally `--session <id>`) adds a `sessions`
array to the JSON: per session, `remote_tokens` (card tokens of jobs that ran on a fleet node), `local_tokens` (on this
box), `remote_share`, and `served_rows` / `served_tokens` (jobs this box ran for the session as a fleet node, never
added to its own totals). `remote_share` is the card-side placement split, not the harness-share gate's
cards/(cards+cloud), which needs the session's transcript. It is a floor: a chat-lane call routed to a node reads as
local, and a `route: "queue"` delegation writes no row on the asker's ledger at all (the node that claims it holds the
row, as `served_*`), so it is in neither number. It reads this box's ledger only; run it on each box for the fleet
view.

**Why jobs failed.** `local-offload ledger --failures` (with `--since DAYS`, `--include-test-traffic` and
`--table FILE`) prints the failure-class register in place of the savings summary: one JSON object that groups the
ledger's failed jobs by reason code, with each class's count, share and kind (stack fault, capacity, caller,
ceiling, work, or unclassified for a class no kind names), the doors, seats and model aliases they came through (on a delegation row the "model" is the seat
alias), the card work they spent (`null` where no row measured it, never `0`), and the meaning, root cause and
structural fix that the table in [systems/failure-register.md](systems/failure-register.md) holds for the class
(`undocumented` for a class with no row; a class whose root cause and fix nobody has written is listed in
`uninvestigated_classes`). The share of failures that are the stack itself failing is `stack_fault_share`; a refusal
a node gave at dispatch that the delegator waited out is `refused_attempts`, not a failure; `unparsed_lines` and
`ledger_found` say when the ledger was damaged or missing. It reads the ledger and the table and nothing else; the
root cause and the fix are written by a person, not derived.

The harness-share gate and any other reader that sums the ledger itself should skip rows whose `run_class` is
set (and, for rows written before the label, whose `origin_session` is `go-test`).

| Failure | Fix |
|---|---|
| every call `deferred:true` | Run `doctor`. Usually the endpoint is down or unreachable. A defer on genuinely hard/over-long input is by design. |
| one media task always defers (`generate_image`, `generate_video`, `generate_audio`, `run_graph`, `edit_image`…) | Run `doctor` and read its **media routes** section. `BOUND-BUT-MISSING` names the exact configured path that is not on disk (relative script bindings resolve against the binary's directory, not your cwd), or what the route's graph or worker loads that is missing — a model file, a custom-node class with its pack (asked of a running ComfyUI, else read from `custom_nodes`), the TTS python/packages/weights — and exits non-zero; `NOT CONFIGURED` means this box has no such binding and the defer is by design. |
| `result cache is held by another local-offload process; using the per-process cache <path>` | Expected (0.113.21) — the bbolt cache is single-writer; this process keeps its own hits in a per-process sibling (`cache.p<pid>.db`) instead of running cache-less. Since 0.121.1 the note prints at the FIRST cacheable task rather than at startup, the sibling is deleted on exit when empty or promoted into the shared cache when the lock is free, and stale ones are swept after 1 h (was 12 h). `cache unavailable; continuing without cache: <err>` means BOTH the shared file and the sibling failed (disk, permissions, a bad `cache_path`) — not lock contention. The ledger still appends. |

---

### Image engines (J2)

`generate_image` has two per-machine backends via `imagegen_engine`: `""`/`"comfy"` (the ComfyUI
path, unchanged default) and `"sdcpp"` — stable-diffusion.cpp as a single native Vulkan binary
(`sdcpp_bin` + `sdcpp_model` + companions in config; spawn-per-job, zero-warm, no Python). The
AMD profiles seed sdcpp with the Apache-2.0 Z-Image-Turbo set at install; any Vulkan-capable box
can opt in (`OFFLOAD_WITH_MEDIA=1` at install). Warm-batch below is ComfyUI-engine only.

### Warm-batch image generation ✅

```powershell
local-offload generate-image --batch jobs.jsonl --json
```

One JSON object per line (`prompt` required; `negative`/`width`/`height`/`steps`/`seed`/`out`
optional — a missing seed is minted, a missing out defaults under the media dir). All jobs
render through **one warm ComfyUI session** — the checkpoint loads once instead of once per
image; the zero-always-warm teardown (free VRAM, kill the spawned ComfyUI, release the GPU
lock) runs at the **batch boundary**, however the batch ends. Measured on the 16GB box:
first job ~32s (absorbs the checkpoint load), warm jobs ~22s. A failed job is recorded in
its result item and does not abort the rest, unless ComfyUI itself became unusable (it stopped answering, or a CUDA fault left it answering but unable to run a prompt): the batch then stops at that job, the jobs after it fail with an error that starts `not run: ComfyUI became unusable at job N/M`, and the command exits non-zero (register C-83, 0.158.1). A full output disk (`ENOSPC`, `EDQUOT`, `EROFS`) stops it the same way (0.178.0): the jobs after it fail with `not run: the disk is full at job N/M, writing <path>` and the command exits non-zero. Every picture is written atomically, to a staged file renamed into place: a failed render leaves nothing at its `out` path (before 0.178.0 a full disk left zero-byte PNGs that a skip-existing script read as finished) and a good file already there is kept. The exit code says how a batch ended, whether you run `local-offload generate-image --batch jobs.jsonl` or the helpers directly (`node render/comfy-generate.mjs --batch jobs.jsonl`, `render/comfy-inpaint.mjs --batch`): `0` every job rendered, `4` it ran every job and some failed (0.178.0; the `"ok":false` rows of the results file, or of the `generate-image` JSON, name them: the JSON keeps reporting per-job status), `1` it stopped (an unusable ComfyUI, a full disk, a setup error), `2` usage. The table is in `docs/systems/media-generation.md`. Single (unbatched) renders keep the zero-warm
default — nothing changes unless you pass `--batch`.

**A batch across the fleet (`--family`, `--route`, `--results`, `--resume`, `--jobs`).**

```powershell
local-offload generate-image --batch jobs.jsonl --family qwen-image-2.1-fast --results results.jsonl
local-offload generate-image --batch jobs.jsonl --family qwen-image-2.1-fast --results results.jsonl --resume
```

Where the jobs run is decided once, at the start. If this machine's lane is **free**, the whole batch renders on one warm session, as above. If it
is **held** and a roster node renders the same recipe, each job becomes a ticket (ADR 0083): it starts on the first matching lane that frees, here or
on a node, at most N in flight (`--jobs`; default: this machine's lane plus one per matching node, at most 8), and the command blocks until every job
has ended, telling you on stderr where each stands. A node renders one job per ticket and pays a ComfyUI cold start for it; the picture is fetched back,
checked against the hash the node published, to the exact `out` you gave. **When no roster node publishes a matching recipe** (today krea2 and the 2512
build: the nodes hold no such weights, and a node that gains them will match), the batch is not split across the fleet: it keeps its place in line for
this machine's lane for up to the queue's 8 h ceiling (not the 90 s `gpu_wait_ms`), runs warm on it, and says which node does not match and why; Ctrl-C
ends the wait. With `--route remote`, or on a machine with no image lane, the jobs go one at a time. A place that is not there yet (every node busy, text-leased or
not answering) is waited for (a pause of 5 s growing to 5 min, up to 8 h), never turned into a `failed` row; after 8 h the job is `queued` and the jobs not
sent yet are queued at once, not held 8 h each. A fleet that cannot take the job as configured (no node serves image generation, a route is not
configured, no node carries `refine=false`, a roster entry the tailnet guard refuses) is not waited for: that job fails at once, naming each node's
miss. A node that was chosen and then cannot be reached when the job is sent, or is lost after, fails it too. The node's recipe is not compared there,
so each row carries the family and licence the node reports.

One row per job goes to the printed JSON and to `--results`: `{i, out, seed, ok, ms, error, node, family, sha256, verdict}` (kept in
`results.jsonl.partial` as jobs end and written over `results.jsonl` when the batch ends, so an earlier `results.jsonl` survives a refused or a dead
batch; a run that reopens the name sets a killed batch's `results.jsonl.partial` aside as `results.jsonl.partial.prev` first and says so; a folder, or on
Windows a read-only file, is refused before anything runs; a warm batch's rows arrive when its session ends) with
`verdict` `rendered`, `skipped-existing`, `failed` or `queued`. Give each job its own `out` and, for a batch you may have to re-run, its own `seed`.
Exit **0** every job has its picture, or a busy lane deferred the whole warm batch (its rows are `queued`: count them, a 0 is not "all done" then),
**4** some job failed (the rows with `failed` say why), **1** it stopped, was interrupted (the jobs that had not ended are `queued`), a job found no
place (the queue's 8 h ceiling passed, or a card stayed held: the job is `queued`, and so are the jobs not sent yet; the exit line says which), or was refused before it began (an empty prompt, two different pictures for one `out`, an unknown family, an unwritable
`--results`, a `--results` that is the `--batch` file). A picture a node sent without a published hash is set aside as `out.unverified` and its job is
`failed`, so `--resume` renders it again.

`--resume` skips a job only when the file at its `out` proves itself: not empty, a PNG that decodes whole, the width and height the job names.
A zero-byte file, a file cut short or of another size, and a missing file are rendered again. A job with no `out` is never skipped, and a size
off the family's grid (qwen-image-2.1 floors to 32) never matches. The first Ctrl-C cancels the tickets that have sent nothing and, on the warm
session, the render itself (its process tree is killed and the lease released); a job already on a node
cannot be recalled, and the same command adopts it if its jobs carry explicit seeds.

### Never post a graph to `:8188` directly ⛔

Every render goes through the harness — `generate-image`, `generate-video`, `run-graph`, or
the MCP tools. A raw `POST http://127.0.0.1:8188/prompt` skips the
[machine-wide GPU lease](architecture/decisions/0018-machine-wide-fenced-gpu-lease.md), so it
can start a diffusion run on top of a live llama-swap tier or a second render — the exact
collision the lease exists to prevent. The lease is a **convention**: it binds only code
paths that take it, so a bypass is not refused, it is simply unprotected.

Same class, same rule: **any tool that drives ComfyUI directly** bypasses the lease unless the
harness mediates it. That includes the official Comfy-MCP server
(`Comfy-Org/comfy-mcp`) — do not point one at this box's `:8188`.

**It also includes this repo's own `tools/comfyui` CLI (`comfyui-pp-cli`)**, named here so the
rule is not one the repo quietly violates. That binary defaults to
`http://127.0.0.1:8188` and POSTs graphs without taking the machine-wide lease (its
`graph_sha` dedupe is a submit-idempotency mechanism, not an arbitration one). Its SUPPORTED
posture is as the harness's submit backend: `render/comfy-submit.mjs` auto-upgrades to it when
the binary is present, and on that path the harness has already taken the lease, so the CLI
inherits it. Invoking `comfyui-pp-cli` **standalone** against a live box is the bypass — wrap
it in `gpu reserve --class media` if you need to.

**Host RAM is part of the lease (2026-10-09).** A lease declares the host memory it will take, in two quantities, and the grant holds
it back when that declaration would pass a line the harness chose. Two readings of the OS are compared, and a grant needs both:
`available RAM now - the lease's touched need - what running leases have yet to touch` at or above `gpu_host_ram_headroom_gib`
(default 8, a floor the harness chose and has not yet measured), and the commit line. A commit you **state** with `--ram-commit` is
held to the OS commit limit: `commit charge now + the lease's commit + what running leases have yet to be charged` at or under the
limit less the same headroom (where the OS enforces one: always on Windows, on Linux with `vm.overcommit_memory` = 2 and when that mode cannot be read, which counts as strict; the
Windows limit moves with a system-managed page file, so "other processes' allocations fail past it" holds once the file has reached
its maximum). A lease that states **no** commit (a bare `--ram`, the media admission, the seat warm-back) declares one figure,
and that figure is held to the line it always had: `commit charge now + the figure + what running leases have yet to touch` at or
under physical RAM less the headroom, on every OS (or under an enforced commit limit less the headroom, where that sits below physical RAM: strict-mode Linux with little swap). **This is a brake on declared loads, not a guarantee about
the box.** A declaration is an estimate: the size of the model files, until three measured runs of that family on the node raise
it to the resident memory they reached (the 90th percentile of the last twenty runs, so one inflated run does not stick, and never above twice the files). A lane's commit can carry its card allocations on top, so a lane can commit more
than it declared: the rule applied to a commit reading of 81.6 GiB on the 127.7 GiB reference box admits a 32.8 GiB Krea 2 bf16
declaration (114.4 GiB projected against a 119.7 GiB bound declared once, 179.7 stated), and a ComfyUI lane holding 57.7 GiB private bytes (its family is not
recorded) read 129.3 GiB committed on that box some hours later (readings measured on the reference box on
2026-10-10 from session readings that are not recorded in this repository). The rule refuses against the declared loads at the moment of the decision; it
does not bound a lane that holds more than it declared, and nothing here bounds a process that holds no lease. A lane already
running is counted in the next decision, so the next lease waits when its declaration would pass a line, but the guard does not stop the lane that took the box
close: read `gpu status` (below), not the admission, as the state of the box. `gpu reserve --ram <GiB>` states the working set the job touches (0 =
needs no host RAM) and `--ram-commit <GiB>` the commit it is charged, which can be much more (a coder-model arm touched 27.8 GiB and was
charged 58.1 GiB: `--ram 28 --ram-commit 58`; a commit equal to `--ram` is still stated, and `--ram-commit` must be finite and above 0);
unset, the commit is the `--ram` figure and is not stated (held to physical RAM, as above); `--ram` unset, it is estimated from the model files of a render helper call whose weights do not fit the card
(a Krea 2 bf16 call on a 16 GiB card is about 33 GiB), else for a media lease the largest ComfyUI render family the box binds (a
binding on the sd.cpp engine is not counted, whichever route it is on, so for `gpu reserve` a node that binds only sd.cpp declares 0
unless `--ram` says; the pipeline's own sd.cpp lanes are different: they declare the weights `--offload-to-cpu` parks in RAM,
sized from their bound files as an estimate; a file that cannot be sized is named in a log line and the figure is a lower bound), and 0 for a text lease. A lease that has to wait says `waiting for host RAM: needs N GiB, only X GiB of Y GiB physical is available, Z GiB
headroom` (or `needs N GiB, committed X of Y GiB physical, Z GiB headroom` for a commit you did not state, or `needs N GiB of commit, committed X of Y GiB commit limit, Z GiB headroom` for one you did, naming the condition that refused) once and keeps its place in the same queue; `--wait 0` refuses with that text. A lease that asks for `--unload-seat` is the one exception to when the host is asked: it takes its place in line and its cards first, the seat is drained and unloaded, and only then is the need compared (the memory it needs may be the seat's own), still holding the cards and inside the same `--wait`, by the same rule and with the same words (F82). The exception holds only while the unload reaches a model on the cards the lease holds and the host can be read; a lease whose unload reaches nothing on its cards, or on a host whose memory cannot be read, is compared in the grant like any other, and a shortage the unload does not cure leaves the cards held and the seat cold for at most the hold bound (`gpu_hostram_hold_max_sec`, default 600 seconds, a chosen figure; `gpu reserve --ram-hold-max <duration>` overrides it for one reserve, and 0 means no bound, so only `--wait` ends the wait). Past the bound the lease is released, the seat is warmed back as for any release (a `--detach` hold's automatic release warms it only if the lease owes the warm-back, so a seat that was never loaded is not loaded), the command never runs and the reserve exits non-zero with one line naming the need, the figures of the condition that refused and the headroom, and how long the cards were held. A `--detach` lease is released in the same way when `--wait` ends its host-RAM wait, and by its own hidden holder if the reserve dies (during the wait, or during the drain or the unload before it: the reserve keeps a moment of now plus the hold bound plus 60 s on the lease record, re-stamped every 30 s while it drains and unloads, so a dead reserve is covered within about the bound whatever `--wait` says; with the bound off there is no such backstop). If the hold bound cannot be written on the lease record, the detached wait is not run and the lease is released. If the holder cannot release its own lease it retries, exits non-zero, and `gpu status` says that it gave up and that `gpu release --epoch N` frees the lease. A figure that is only the media class default (a wrapped command that is not a render helper, no `--ram`) prints a `WARNING` line when it is declared, because on 2026-10-14 such a lease held three cards idle for about 50 minutes behind a figure the host could not meet: pass `--ram <GiB>` with the real figure (`--ram 0` when the weights sit on the cards). The warm-back of the agent seat after a
lease is admitted the same way against `agent_seat_host_ram_gib` (the host RAM the seat holds once loaded; unset, a chosen 21 GiB
stands in: not a measurement, and below the 44 GB the 2026-09-10 incident recorded for a seat, so set the key from the node's own
measurement before relying on the check) and never runs while another lane that declared host RAM is live. `gpu status` and `offload_status`
show the host's memory with one verdict: `OK`, `NEAR` (the available RAM, the room under physical RAM for the commit charge, or the room under an enforced commit limit, is inside the headroom) or `OVER` (one of them is gone, the commit charge above physical RAM included;
the brief status line then starts `HOST RAM OVER`). Details:
[GPU lease, "Host RAM"](systems/gpu-lease.md).

Two escape hatches, both of which keep the lease:

```powershell
# a graph the templates do not cover — run-graph takes the lease for you
local-offload run-graph --graph wf.json --out-dir out/

# you genuinely need a bare server (benchmarking) — hold the lease explicitly
local-offload gpu reserve --class media -- <your command>
```

On a host with card-scoped leases enabled, hold only the cards the job uses so the others keep working:
`gpu reserve --class media --devices 2 -- <cmd>` (named cards), `--cards 2` (the allocator picks two free cards; the display card only up to `gpu_display_cap_pct`, default 60 percent of its VRAM with the desktop's use counted, and only when you state `--vram`; set the key to `0` when you need the screen's card to yourself; `gpu status` / `gpu cards` print a `display cap:` line under the display card with the VRAM in use against that share, whoever's memory it is),
or no flag at all when `<cmd>` pins itself with `CUDA_VISIBLE_DEVICES`, `--cuda-device` or `COMFY_CUDA_DEVICE`. A command that carries a
`COMFY_CUDA_DEVICE` or `COMFY_CARD_UUID` pointing at a card the lease does not hold is never refused: the wrapper rewrites that
variable to the held card, prints one `re-pinned` line and starts the command (a `--cuda-device` on its command line is the
program's own argv and only draws a warning). `gpu cards`
shows each card and its holder; `gpu doctor --write-audit` is what turns the feature on for a host (see
[GPU lease](systems/gpu-lease.md#reserving-cards-the-card-table-and-the-reader-audit-plan-p3)). Point `gpu doctor --scan` at
every repository that carries its own copy of the harness binary, and read its "not searched" list: a green audit covers only
what it reached, and the deployed `local-agent` binary (section 4) is one of the readers it has to cover.

If you started ComfyUI by hand, check that the process owning `:8188` is the one you think
it is before rendering: a flagless instance can keep the port while a correctly-flagged one
fails to bind, and nothing reports it (its argv carries no `ComfyUI` substring, so a naive
kill-filter misses it).

**A ComfyUI a harness runner launched stays up after a good job and ends itself once idle (ADR 0097, 2026-10-24).** Before that
every render relaunched ComfyUI and killed it at the end: on the 8 GB laptop card the launch took 31 to 89 s of a 66.5 to 135 s
render. Now the runner leaves the process running after a good job (it holds about 240 MiB of the card; on the 16 GB reference
box a kept keyed instance held about 5.0 to 5.2 GiB of host RAM), the next render inside the bound reuses it, and the idle pack
exits the process after `COMFY_IDLE_EXIT_SEC` seconds with nothing queued or running and no runner holding it. Set
`COMFY_IDLE_EXIT_SEC` in the runner's environment: unset is 300, `0` or `off` restores the old behaviour (kill the ComfyUI after
every job), a whole number of seconds from 5 up is that bound, anything else is 300 with a `COMFY-IDLE-WARN` line. It is fixed when
the instance is launched. A render that fails stops the instance it used, so a stuck CUDA error does not outlive the job. A render
that needs another shape (warm against single, another `--reserve-vram`) restarts the instance once. An explicit `--keep-comfy`
keeps its old meaning: the instance is kept whatever happens and has no process bound; its lease's holder (or you) stops it.
A runner holds the instance for its whole job through `GET /offload/hold`, a response that sends one byte every 2 s and ends when
the runner exits, so a long step that is not ComfyUI's (ffmpeg, a QA pass) does not let the bound fire. `curl
http://127.0.0.1:8188/offload/idle` shows it: `exit_limit_sec` (the bound, 0 when there is none), `exit_idle_sec` (seconds
idle now) and `holds` (runners holding it now), beside the model free's `limit_sec` and `idle_sec`.

To stop a kept instance by hand, for example to hand the whole card to something else: find the process that listens on its port
(`Get-NetTCPConnection -LocalPort 8188 -State Listen` gives `OwningProcess`; a keyed instance has its own port, printed in its
launch marker `.offload-launch-<key>.json` in the ComfyUI folder) and stop it (`Stop-Process -Id <pid>`, or `kill <pid>` on
Linux). For a keyed instance under a lease, `local-offload gpu release --epoch N` stops the instances of that lease; the media
pipeline's own routine release leaves a self-bounded one running on purpose. Or just wait for the bound.

**A hand-started ComfyUI is not bounded by the harness's idle free unless you opt in.** The ComfyUI the harness keeps
(`--keep-comfy`) or runs warm (`--batch`) is launched with an idle pack that frees its models and caches after 300 s with
nothing queued or running (`COMFY_IDLE_FREE_SEC` on the runner changes it, `0` or `off` disables it; do not confuse it with
`COMFY_IDLE_EXIT_SEC`, above, which ends the process). The harness's own launch
passes `--disable-smart-memory`, so ComfyUI already unloads its models after each prompt; the pack matters for a warm session's
cache and for an instance that runs with smart memory (ComfyUI's default), which keeps its models loaded between prompts. The harness injects
nothing into an instance it did not start: a runner that finds one without the pack prints one `COMFY-IDLE-WARN` and uses it as
it is. To give your own instance the same bound, add this to its launch (the path is your checkout's):

```
python main.py ... --extra-model-paths-config <repo>/render/comfyui-ext/extra_model_paths_idle.yaml
```

and, if you start it with `--disable-all-custom-nodes`, add `offload_idle` to its one `--whitelist-custom-nodes` list (that flag
takes a single list and the last occurrence wins). `OFFLOAD_IDLE_FREE_SEC=<seconds>` in its environment changes the bound
(default 300, `0` off). Confirm it with `curl http://127.0.0.1:8188/offload/idle`. It frees models and caches only: the process stays
until you stop it. `OFFLOAD_IDLE_EXIT_SEC=<seconds>` in the same environment would also end the process after that many idle
seconds with no hold; leave it unset (the default is no process bound) unless you want your own instance to exit, because a
client that does not hold the instance through `GET /offload/hold` is not protected from the exit. The pack ran in a live ComfyUI on
2026-10-14; the live check is in [media generation](systems/media-generation.md).

### Run an arbitrary ComfyUI graph (run-graph) ✅

```powershell
local-offload run-graph --graph wf.json --manifest manifest.json --out-dir out/
```

Executes any API-format ComfyUI graph under the same GPU-lock/zero-warm lifecycle. With a fleet
(`delegate_remotes`) an `auto` graph whose lane is not free (or whose lane could not be read) is a **ticket**: the harness holds it
and starts it on the first matching lane that frees, here or on an idle node, in order of arrival, and the call answers with the
ticket, its position and, per node, why not yet (the MCP tool answers inside `wait_s`, the CLI blocks and tells you on stderr where
the ticket stands; a first Ctrl-C before anything was sent cancels it). The ticket keeps its own copy of the graph and the manifest,
so overwriting or deleting your file afterwards changes nothing, and `waiter_token=mq-...` collects the result even then. A node takes
it under three rules ([media-generation.md](systems/media-generation.md), "Graphs on the fleet"): it must hold every model file and
input file the graph names (it refuses with 503 naming what it lacks; that marks the node not-capable for this ticket alone, quoting the
node, and the ticket moves on and asks that node again only after 5 minutes); it must publish exactly **one** card, because a
whole-node graph runs in the default ComfyUI instance, which sees every card, and could land on a multi-GPU box's display card (a
single card that drives a display passes); and,
with `--min-vram-gb N`, its largest card must be at least N GB (a 16 GB card reads 15.92 GiB and counts as 16). `--model-family wan22`
with a graph height of 720 or more gets a 16 GB floor **chosen** from the film owner's stated constraint (Wan 2.2 14B at 720p needs a 16 GB
card), not measured; `--min-vram-gb 0` lifts it. The floor filters nodes only: `--route local` never reads it. With the fleet as
the film owner described it on 2026-10-09 (the 16 GB cards are the multi-card workstation's three; the single-card nodes have 8 GB
cards) the two rules together leave a default-floor Wan graph no remote node: it waits for its own lane. A graph ticket shares the
nodes with the image tickets of its machine by the tickets' own rules: a node another ticket is running on is left alone (one claim
per node, taken under a lock that holds across processes), and an idle node an older waiting ticket is first in line for is left to
that ticket. A graph that reads an input
file (`LoadImage`, `LoadVideo`, `LoadAudio`) is a ticket that is never offered to a node, because a node can match such a file only by
name; it keeps its place and its position and waits for this machine's own lane, and every answer it gives carries `fleet_note`
saying the fleet was not asked and why (a result carries no such note). A waiting graph ticket's place is the whole-node one, so
younger calls on this machine's lane queue behind it. A call that must leave this machine anyway (`--route remote`, or
`auto` on a machine with no graph lane) still sends it, with the node's check by name only. On every route that sends a graph to
a node, the node must run this machine's own release (the file check ships with it): a node on another release, or one that
publishes none, is skipped by name, so a fleet part-way through an upgrade takes graphs only on the nodes already upgraded. The **node manifest** (`node_packs[{name,repo}]` + `models[{path,source_url,sha256}]`; a `commit` on a pack is accepted and not read)
is satisfied BEFORE ComfyUI starts (a pack is taken only from a repository listed in `render/node-packs.json`, and a model only under `models/<class>/` from a listed https host; anything else is refused before the first clone or download): packs cloned at the head of their default branch (the commit each got is recorded beside it in `custom_nodes/<name>.installed.json`) with all deps
resolved **together** (uv) under **host-torch constraints** (provisioning never replaces the
installed CUDA torch — an unsatisfiable pack set defers instead), models downloaded and
verified against the digest the publisher publishes (Hugging Face LFS, GitHub release assets; a `sha256` in the manifest is only an
optional assertion, and a model whose host publishes no digest is reported in
`unverified_models[]`). Results are node-addressed (`outputs.<node_id>[]` with
`path/type/kind/width/height`); every failure is a **typed defer**
(`SATISFIER_UNAVAILABLE`, `VENV_INCOHERENT`, `SATISFIER_SPAWN_FAILED`, `NODE_CLASS_MISSING`,
`EXTERNAL_COMFY_NEEDS_PACKS`, …) so callers branch on `code`, never parse prose.
Prerequisite (provisioned by `install.ps1`): **uv** in the ComfyUI venv (the pack satisfier's
unified-resolve tool, the hard requirement) and git on PATH. ComfyUI-Manager and GitPython are not
used by run-graph and `install.ps1` no longer installs them.

### Generative inpainting (inpaint-image) ✅

```powershell
# 1. build a white-on-black mask (white = repaint) with the deterministic mask_boxes edit op
local-offload edit-image render.png --ops '[{"op":"mask_boxes","boxes":[{"x":620,"y":540,"width":820,"height":820}],"feather":6}]' --out mask.png
# 2. re-render ONLY the masked region from a prompt
local-offload inpaint-image render.png --mask mask.png --prompt "clean glossy dark watch dial, no writing" --negative "text, letters" --seed 4242 --json
```

Masked re-denoise on the local ComfyUI (core nodes only, same GPU-lock/zero-warm
lifecycle): white mask pixels are re-imagined from the prompt, black pixels stay
untouched. The route needs a per-machine **SDXL-class** binding in `config.json` —
`inpaint_script` (path to `render/comfy-inpaint.mjs`), `inpaint_ckpt` (e.g.
`RealVisXL_V5.0_fp16.safetensors`), optional `inpaint_vae` (`builtin` = the
checkpoint's own VAE) — because `VAEEncodeForInpaint` is a latent-space technique: a
pixel-space DiT (HiDream) canNOT drive it even when it is the machine's
`imagegen_ckpt`. Unbound = the task defers cleanly. Knobs: `--denoise` (default 1.0 —
full re-imagination inside the mask), `--grow-mask` (default 16 px seam feathering in
latent space), `--steps/--seed/--out`. Diffusion cannot WRITE legible text: inpaint the
region clean, then add real type with the `edit-image` `text` op.
`--auto-text` (EXPERIMENTAL) replaces `--mask` with vision-detected text boxes; it
defers whenever detection is unparseable, empty, or absurd (>60% coverage) — build the
mask with `mask_boxes` yourself when it does.

For edits that have **no drawable region** ("make it snowing heavily", "turn the leather
into fur") the third edit route is the maskless **generative instruction edit** — MCP-only
(`offload_edit_image_generative`, no CLI verb), bound per machine by the `gen_edit_*`
config keys — seeded on every ≥16 GB ComfyUI tier since 0.132.5 (Qwen-Image-Edit 2511,
preset `lightning8`), unbound elsewhere. See
[systems/media-generation.md](systems/media-generation.md).

### Add a named image or edit family (license-tagged opt-in, ADR 0058) ✅

A family is a second (third, …) binding beside the box's default one, selected per request
with `family`. It is how a model such as **Qwen-Image-2.1** is offered beside the default
without replacing it. Put the weights in the
ComfyUI class directories first (download set: `setup/SETUP-AGENT.md`), back up the config,
then add the overlay:

```json
"imagegen_families": {
  "qwen-image-2.1": {
    "license": "Qwen Research License",
    "commercial_use": false,
    "imagegen_family": "qwen-image-2.1",
    "imagegen_ckpt": "qwen_image_2.1_bf16.safetensors",
    "imagegen_clip": "qwen3vl_8b_bf16.safetensors",
    "imagegen_vae": "qwen_image_2.1_vae_bf16.safetensors",
    "imagegen_timeout_sec": 2400,
    "comfy_cuda_device": "2",
    "comfy_dynamic_vram": "on"
  }
},
"gen_edit_families": {
  "qwen-image-2.1": {
    "license": "Qwen Research License",
    "commercial_use": false,
    "gen_edit_family": "qwen-image-2.1",
    "gen_edit_unet": "qwen_image_2.1_bf16.safetensors",
    "gen_edit_clip": "qwen3vl_8b_bf16.safetensors",
    "gen_edit_vae": "qwen_image_2.1_vae_bf16.safetensors",
    "gen_edit_cache_device": "gpu",
    "comfy_cuda_device": "2",
    "comfy_dynamic_vram": "on"
  }
}
```

Rules the load enforces (a violation refuses the config by name): `license` (non-empty
string) and `commercial_use` (bool) are required; image overlays take only `imagegen_*`,
`sdcpp_*`, `comfy_*` keys, edit overlays only `gen_edit_*`, `comfy_*`, and every key must be
a real config key; a family does NOT inherit the default's checkpoint, LoRA, preset, sampler
knobs or pool — it inherits the script, engine, timeout and launch keys unless it sets them.
The values above for timeout, device and dynamic VRAM are the planned starting point for a
three-card box, not measurements — replace them with the node's own measured values. A family that sets no `imagegen_timeout_sec` inherits the top-level one, which is sized for the default binding, so a slow family needs its own, set above its measured cold render. On a 16 GB render card a Qwen-Image 2512 `full`-preset still took 480-555 s warm at 1.5 MP, and a 600 s limit killed its cold renders at 1.6 MP at 601 s (1,500 s, for example, clears that).

Verify: `local-offload doctor` shows a `generate_image:qwen-image-2.1` route (CONFIGURED only
when every model file is where the loader looks, with `license Qwen Research License` in its
detail) and the family's files in `comfyui model bindings`; `offload_status` lists it under
`media.image_families`. Render through it:

```powershell
local-offload generate-image "a neon shop sign on a rainy street at night" --family qwen-image-2.1 --json
local-offload generate-image "a glass perfume bottle" --family qwen-image-2.1 --transparent --json
```

The result carries `family`, `license` and `commercial_use: false`, and the ledger row the
license. A bare `generate-image` (no `--family`) still renders the default binding unchanged.

**FLUX 3 Image Klein** binds the same way, with its prompt upsampler (`imagegen_upsampler`; BFL's card says not to run
the model without one) and steps and cfg set together (the family is in the pairing rule). Every node names the family
`flux3-klein`; what it binds depends on the node. A ComfyUI node runs BFL's upsampler file in-graph:

```json
"imagegen_families": {
  "flux3-klein": {
    "license": "FLUX Kommunity License v1.0",
    "commercial_use": true,
    "imagegen_engine": "",
    "imagegen_family": "flux3-klein",
    "imagegen_ckpt": "flux3-image-klein-bf16.safetensors",
    "imagegen_clip": "qwen3vl_4b_bf16.safetensors",
    "imagegen_vae": "flux2-vae.safetensors",
    "imagegen_upsampler": "qwen3.5_9b_flux3_prompt_upsampler_int8_convrot.safetensors",
    "imagegen_steps": 16,
    "imagegen_cfg": 1,
    "imagegen_sampler": "euler",
    "imagegen_scheduler": "simple"
  }
}
```

or, faster, names a llama-swap seat serving BFL's PUP as a GGUF (`"imagegen_upsampler": "flux3-pup"`): the harness writes
the caption before the lease at llama.cpp speed (86 tok/s on a 5070 Ti, against 4-8 tok/s in-graph) and unloads the
seat before the render. An sd.cpp node has only the model form:

```json
"imagegen_families": {
  "flux3-klein": {
    "license": "FLUX Kommunity License v1.0",
    "commercial_use": true,
    "imagegen_engine": "sdcpp",
    "imagegen_family": "flux3-klein",
    "imagegen_steps": 16,
    "imagegen_cfg": 1,
    "imagegen_sampler": "euler",
    "sdcpp_model": "/models/flux3-klein/flux3-image-klein-Q4_K.gguf",
    "sdcpp_model_kind": "diffusion",
    "sdcpp_llm": "/models/flux3-klein/Qwen3VL-4B-Instruct-Q4_K_M.gguf",
    "sdcpp_vae": "/models/flux3-klein/flux2-vae.safetensors",
    "sdcpp_extra_args": ["--params-backend", "disk", "--flash-attn", "on", "--vae-tiling"],
    "imagegen_upsampler": "qwen3.5-4b-agent"
  }
}
```

The `ampere-6`, `amd-gcn` and `blackwell-8` seeds carry their measured bindings (`docs/tiers/`); the files are listed in
`setup/SETUP-AGENT.md`. `commercial_use: true` is the operator's ruling for this license (2026-10-24); read the FLUX Kommunity License before
copying it to another deployment. The result carries `upsampled` and `upsampled_prompt` (the caption the model rendered),
or `upsample_fallback` with the reason the raw prompt was rendered; `--refine=false` renders the prompt verbatim. Model
files, the card-only rows and the sd.cpp memory traps: [systems/media-generation.md](systems/media-generation.md).
To tag the default binding too, set `imagegen_license` + `imagegen_commercial_use` (both or
neither; edit: `gen_edit_license` + `gen_edit_commercial_use`).

### Pin a box's ComfyUI routes to a card (launch profile) ✅

`comfy_cuda_device` launches ComfyUI with `--cuda-device <n>` for the single-card routes
(un-pooled image generation, generative edit, upscale, inpaint, animate, music). The index is
in **ComfyUI's device order** — the order the `*_pool_*` `cuda:N` keys use, fastest card first
unless `CUDA_DEVICE_ORDER` is set — not nvidia-smi's PCI order. On the three-card tier
`cuda:0` is the 5070 Ti display card and must never carry a single-card route; the tier seeds
`"2"` — the 5060 Ti at PCI B5:00.0, the better-cooled of the pair (operator choice 2026-09-22:
`cuda:1`, the 5060 Ti at 17:00.0, ran at 82 °C under a Qwen-Image-2.1 render). An existing box
sets it by hand. Pooled image/video seats never take the pin (their pool keys place them) and neither does
`run-graph`.

```json
"comfy_cuda_device": "2",
"comfy_dynamic_vram": "",
"comfy_extra_args": ""
```

`comfy_dynamic_vram` `on` strips `--disable-dynamic-vram` from the launch (weight streaming for
a DiT larger than one card), `off` adds it (required by DisTorch2 pooled seats), `""` leaves
`COMFY_EXTRA_ARGS` alone; `comfy_extra_args` replaces the inherited `COMFY_EXTRA_ARGS` env when
set. Keep `on` out of a binding that pools — every config load (so every CLI verb, doctor included) warns on stderr. If a ComfyUI is already running
with other flags, a render that needs a different profile relaunches it when the harness
started it and nobody holds it, and otherwise defers with `COMFY-PROFILE-MISMATCH: …` naming
the difference — stop that instance (or start it with the binding's flags) and retry.
`--use-sage-attention` in `COMFY_EXTRA_ARGS` also changes the card-only Wan sizing (its activations are 15.2 GiB at film size, so a sage film call
passes `--wan-resident-gb 0`; see "Sage attention" in `docs/systems/media-generation.md`), and a kept ComfyUI that runs sage is not reused by a call
sized for the default attention.

### Deterministic post-production (edit-image op pack) ✅

```powershell
# grade -> resize -> finish (delivery sharpen LAST), plus a platform export matrix
local-offload edit-image render.png --ops '[{"op":"grade","levels":{"black":8,"white":248,"gamma":1.05},"wb":{"mode":"gray_world"}},{"op":"resize","width":1920},{"op":"finish"}]' --renditions '[{"width":1920,"format":"jpg","suffix":"-web"},{"width":1080,"format":"webp","suffix":"-ig"}]' --json
# apply a .cube LUT look at 60% strength
local-offload edit-image render.png --ops '[{"op":"lut_cube","path":"D:/luts/teal-orange.cube","strength":0.6}]'
# place a render onto a laptop-screen mockup (quad = UL,UR,LR,LL screen corners)
local-offload edit-image mockup.png --ops '[{"op":"perspective_composite","overlay":"render.png","quad":[[412,180],[1508,236],[1490,940],[398,905]]}]'
# GIMP layered-template factory: new copy + swapped product image, then PIL ops
local-offload edit-image template.xcf --ops '[{"op":"instantiate_design","set_text":{"Headline":"Hola Bogotá"},"replace_image":{"ProductShot":"D:/renders/watch.png"}},{"op":"resize","width":1080}]'
```

All raster ops are deterministic, CPU-only PIL (no GPU lock, run in parallel with
renders); GIMP is needed only for `flatten_design`/`instantiate_design` (headless
`gimp-console`, invocations serialized process-wide). The op pack:

- **`grade`** `{levels{black,white,gamma}?, curve{points[[in,out],…]}?, wb{mode:gray_world|scale,r,g,b}?, luminance_only?}` —
  tone/color grading. Every transform composes mathematically into ONE 256-entry LUT
  per channel and quantizes ONCE (chained 8-bit `.point()` passes band visibly); the
  alpha channel is never remapped.
- **`lut_cube`** `{path, strength?}` — a `.cube` 3D LUT "look" via Pillow's built-in
  `Color3DLUT`; `strength` 0–1 blends the graded result over the original. 1D cubes
  and non-standard domains are rejected.
- **`perspective_composite`** `{overlay, quad:[[x,y]×4]}` — warps the overlay into the
  destination quad (**UL,UR,LR,LL winding** — a mismatched winding silently mirrors)
  with a pure-Python homography solve + BICUBIC resample, then alpha-composites:
  screen/frame mockup placement. Quads live with the mockup asset; there is no
  auto-detection.
- **`finish`** `{sharpen{radius,percent,threshold}?, median 3|5?}` — delivery
  sharpening; defaults (radius 1.2 / percent 80 / threshold 3) are tuned for
  post-AI-upscale web output (Pillow's 150% default over-crisps upscaler output).
  Explicit zeros are honored (`"sharpen":{"percent":0}` = no visible sharpening —
  the way to get a median-only finish through the harness; direct worker callers
  may also pass `"sharpen":null`).
  **Ordering rule: `finish` must be the LAST op, after any `resize`** — sharpening
  before a resize is undone by resampling. `median` is only for salt-and-pepper
  speckle; real sensor/upscaler noise reduction (NLM/BM3D-class) is out of PIL's
  reach and deliberately not faked.
- **`--renditions`** `[{width/height, format png|jpg|webp, suffix}]` — after the ops
  pipeline produces the master `out`, each rendition re-runs the worker
  (resize+convert) writing `<out-stem><suffix>.<format>`; results land in
  `renditions[]`. One master, full platform matrix in one call.
- **`instantiate_design`** `{set_text{LayerName: copy}, replace_image{LayerName: path}}`
  (FIRST op only, `image` = a `.xcf`/`.psd` template) — the GIMP template factory:
  sets named **text layers'** copy, swaps named **pixel layers** for replacement
  images at the old layer's offsets, flattens, and hands the raster to the rest of
  the pipeline (e.g. `grade` → `renditions` = a one-call brand-variant factory).
  A layer-name mismatch is THE common failure — the error surfaces GIMP's stderr
  naming the failing lookup; check names with `flatten_design`'s `layers` output.

### Compose motion graphics (compose-video, HyperFrames)

```powershell
# a vetted template + typed variables (what a node's template door takes from any fleet caller)
local-offload compose-video --template title-card --variables '{"title":"Launch day","subtitle":"Everything that shipped","accent":"#22c55e","duration":6}' --json
# a transparent lower third for an editor (ProRes 4444) or the web (VP9 alpha), with two check frames
local-offload compose-video --template lower-third --format webm --variables-file lt.json --snapshots 1,2.5 --out D:/renders/lt.webm
# a stat card (opaque); the caption's emphasised phrase is its own variable
local-offload compose-video --template stat-card --variables '{"kicker":"Launch week","stat":"3.4x","label_pre":"faster search across","label_em":"every workspace","label_post":"since the update"}' --json
# a caption overlay from a transcript: group the words, then render each chunk it prints
node render/captions-groups.mjs clip.segments.json --pace conversational --out chunks.json
# your own single-file composition (trusted code only: Chrome runs it without a sandbox)
local-offload compose-video --html card.html --quality draft --workers 1
```

The vetted templates are `title-card`, `lower-third`, `stat-card`, `section-title`, `callout-label`,
`checklist-card` and `captions-bar`. `stat-card`, `section-title`, `callout-label` and `checklist-card` are
adapted from a teaching kit under its own licences, kept beside them; `captions-bar` is written for the harness.
Each README under `render/compose-templates/` lists the template's
variables and limits. `captions-bar` reads one `words_json` variable that `render/captions-groups.mjs`
builds from `offload_transcribe`'s `<base>.segments.json`; render the overlay silent to `webm` or `mov`
and lay it over the footage with ffmpeg's `overlay` filter or in an editor.

This renders an HTML/CSS composition to video with the HyperFrames CLI the installer put in place (the newest npm release, never pinned). It runs on the CPU:
software GL and CPU encode, with no GPU lock, so it runs next to every render and text seat. The
steps are `lint`, then `check`, then `render`, then an ffprobe gate. Every returned field is
measured. Any failure defers with a typed class (`BAD_INPUT`, `LINT_ERRORS`, `CHECK_FAILED`,
`RENDER_FAILED`, `BROWSER_MISSING`, `FFMPEG_MISSING`, `CLI_MISSING`, `SPAWN_EBUSY`,
`DISK_HEADROOM`, `TIMEOUT`).

**Binding the lane.** The installer's hyperframes step does this on a box with Node >= 22. It
installs the newest HyperFrames release from npm into `<home>/hyperframes` (`npm install --ignore-scripts`, each tarball verified against the registry's integrity field), runs `npm audit signatures` (fatal on failure) and
`browser ensure`, and writes three keys into a FRESH config. An existing config is never rewritten,
so the installer prints the three keys instead. To bind by hand:

```json
"compose_script": "render/compose-hyperframes.mjs",
"hyperframes_dir": "D:/offload-stack/hyperframes",
"hyperframes_browser_path": "auto"
```

`"auto"` (what the installers write) means the managed chrome-headless-shell that the installed HyperFrames
release resolves at render time under `<hyperframes_dir>/home`; `local-offload install hyperframes` and
`local-offload update` refresh it, and a render fetches it on first use if the release wants a newer one. An
explicit file path (what `node render/compose-hyperframes.mjs browser --hyperframes-dir <dir>` prints as
`browser_path`) binds that one browser and does not follow a release.

`ffmpeg_path` must name an ffmpeg with ffprobe beside it. `local-offload doctor` then reports
`compose_video CONFIGURED` with the template list, and `local-offload acceptance` runs the installed
CLI's `--version` through the runner as this identity. Optional keys: `compose_quality` (`high`
default), `compose_workers` (`auto` default; on the reference box neither 1 nor `auto` wins consistently for
a short card, but a clip longer than about two minutes needs 1: above one worker HyperFrames stores every
frame, 8.3 MB at 1080p, and defers `DISK_HEADROOM`),
`compose_timeout_sec` (1800; a 300 s caption chunk takes about 17 minutes at one worker) and `compose_cache_dir`
(work dirs and frame cache; move it to a large drive if a render defers `DISK_HEADROOM`).

**From a machine without the lane ([ADR 0071](architecture/decisions/0071-a-fleet-token-holder-may-send-a-whole-composition-project-to-render.md)).**
`route` (`--route` on the CLI) picks where a composition renders: `auto`, the default, renders here when
this machine has the lane and on a fleet node from `delegate_remotes` when it has none; `remote` always
sends it; `local` never does. A template travels by name to any node that advertises `compose-video`. A
project directory or an inline composition travels as a bundle to a node's project door, which a node
opens only with `fleet_compose_projects: true` and a `fleet_auth_token`, and the caller must hold the same
token. Both sides check the bundle: regular files only, no unsafe or reserved names, size and entry
caps (`fleet_compose_bundle_max_mb`, 64 MB by default), and no reference that leaves the project (`..`
from its file or, unless it starts with `../`, from the project root, where HyperFrames resolves it; an
absolute, root-relative, UNC or protocol-relative path; a `file:` URL; a `<base>` or `srcdoc`; or a URL
to a loopback host in any numeric form, a private, tailnet, dotless or non-ASCII host). Every file a
`data-composition-src` names is checked as a composition whatever its extension. Public `https`
references (fonts, a CDN) stay allowed, and the node fetches them. A project job's state and output are
visible only with the token. The video and its snapshots come back into this machine's media dir, or to `out`. `png-sequence`
renders only locally. A machine with no model or lane at all is a
[delegation client](systems/delegation-client.md).

**Media jobs on a fleet node ([ADR 0077](architecture/decisions/0077-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md)).**
`offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio` and
`offload_run_graph` take `route` and `remotes` (CLI `generate-image`, `generate-video`, `generate-audio`, `run-graph`:
`--route` and a repeatable `--remote`). `auto`, the default, runs here when this machine has the lane (read from the files
its route loads, not from the config binding) and on a node from `delegate_remotes` when it has none; `remote` always sends
it; `local` never does. `remotes` narrows the nodes for one call and must be a subset of `delegate_remotes`. A still, a
reference and driver, or a clone sample travels in a hash-checked bundle to the node's media-job door, which a node opens
only with `fleet_media_inputs: true` and a `fleet_auth_token` (the caller holds the same token; `fleet_media_inputs_max_mb`
caps the bundle, 256 MB by default: the node holds the base64 body and the decoded bundle together while it admits a job, about 0.58 GiB at the default, and takes one upload at a time (a second waits up to 30 s for the first, then is answered 503), so raise it only on a node with the RAM). A job with no input file, and every `run-graph` (its graph travels inline), needs no
door. The outputs come back by name, each checked against the sha256 the node published; a mismatch defers and leaves no
file. A node advertises `video-gen`, `animate`, `audio-gen` and `run-graph` only while its media route is CONFIGURED, so a
node with a missing weight drops the task and `/fleet/health` `media_routes` says which route and why; `doctor` and
`offload_status` print the same verdicts. `tts_voice`, `transformer` and `run_graph`'s `devices` cannot travel and defer on a remote
route (`refine=false` travels, to a node whose health says `refine_honoured`); `run_graph`'s `out_dir` is where the fetched outputs land on the calling machine (created if missing, never sent to the node). A node that binds a media task but whose route is not CONFIGURED refuses a job for it with a 503 naming the route and its state (a delegator re-places that), not the 400 an unbound task gets. See [media-generation.md](systems/media-generation.md#remote-routing-and-the-media-job-door-adr-0077).

**A local media call on a node whose route lost a file defers typed, before the lease (CT-54).** A node that binds a media route and
then loses a file it names (a weight deleted, the engine binary or the runner script moved) already refuses a job for it at its door
with a 503. A call made on that machine itself (`generate-video`, `animate-character`, `generate-audio`, `generate-image` on an sdcpp
box, or the same MCP tools) now answers the same way and in the same millisecond, with `err_class` `route_not_ready`, a reason naming
the route, the config key and the missing path, and the advice to place the job on another node; it takes no lease and creates no
output directory. `route auto` already sends such a call to a fleet node, `doctor` and `offload_status` show the same verdict, and the
fix is to restore the file (or unbind the route). A required key left unset (the VAE empty while the weight and the binary are set, or
nothing bound at all) is a configuration gap, not a missing file: the lane keeps its own `<key> is not configured` sentence, which
names the key, with no class. A ComfyUI that stopped answering is `comfy_unusable` in the ledger instead of `other`
([media-generation.md](systems/media-generation.md)), on every row of an image `--batch` that stopped on it (the job it stopped on and the
jobs it never ran; a job that failed on its own earlier keeps its own class). A `POST /prompt` it never answers ends after 120 s and
counts as that death (the render exits 3 and a `--batch` stops); a `GET /view` it never answers ends after 600 s as a plain `timeout`.

**A bare `--` ends the flags on the media verbs (CT-52).** `local-offload generate-audio out.wav -- "--- Intro ---"` passes the text,
where `"--- Intro ---"` alone was read as a flag and the text came out empty. Everything after the first `--` is a positional, in order,
even when it starts with a dash; a lone `-` is still stdin. The same holds for `generate-video`, `animate-character` and the verbs that
take one file, text or prompt.

**A busy image lane overflows to the fleet ([ADR 0082](architecture/decisions/0082-a-media-call-overflows-to-the-fleet.md)).** An `auto` image call on a machine that has the lane and a fleet asks its own pipeline whether the lane is free; when it is not (a lease holds the card, callers are in line, or the host-RAM guard is holding the lane back), the call goes to an idle node that renders **the same recipe**: the same weight files at the same sizes and with the same content fingerprint, and the same sampling, compared as `digest` in `/fleet/health` `image_recipes`. A match is strict and never swaps in a different quantization; it also needs the node on the same release, and the answer names the key that differs and the family of yours that would match (`send family=qwen-image-2.1-fast to use its qwen-image-2.1`). A node that holds any lease is not a candidate. When nothing admits, the call becomes a **ticket** ([ADR 0083](architecture/decisions/0083-a-busy-image-call-is-a-ticket-the-harness-holds-until-the-first-matching-lane-frees.md)): the harness holds the job and starts it on the first matching lane that frees anywhere (your own, or a node that renders the same recipe), in the order it arrived, and the answer says where it stands: its position, your own lane, and one row of `cluster[]` per node of `delegate_remotes` with the state (`busy` and by what, `in-use`, `not-capable` and the key that differs, `unreachable`, `bounced`, `refused`) and since when, and **do not call again** (a repeat of the identical request attaches to the same ticket; `waiter_token=mq-...` collects the result). A node that took the call and passed it back (another job held its card, or its own lease could not be taken) is left alone for a minute, then five, and its row says so in the node's own words (a host short of RAM reads as that, not as a held card); the MCP server remembers that across the calls of a session. A node that refused the recipe (412) because it had only not finished reading its weight files (`pending`) is left alone for a minute and never for five, and that is not counted as a bounce; one whose files or binding changed, or that cannot read a file, is bounced like the rest. `offload_generate_image` takes `wait_s` (how long the call waits before it answers "queued"; default `gpu_wait_ms`); the CLI `generate-image` blocks until the job is done, prints where the ticket stands on stderr, and the first Ctrl-C cancels a ticket that has sent nothing. `offload_status` lists the tickets as `media_queue[]`. A node added to `delegate_remotes` joins a running MCP session when the config file changes (no restart). A finished result nobody has collected is kept 6 hours (10 minutes for a failure), and for 2 minutes after a call took it, so a second call naming the ticket still gets it. A lane the pipeline could not read (its card table past 4 s and the one 10 s retry that follows a read that ran out of time, its lease directory not opening) is not taken for a free lane: the fleet is looked at first and the call runs here only if no node can take it. `route local` never leaves. A digest covers each weight file's name, byte size and a sampled content fingerprint (64 evenly spaced MiB of the file): a same-named file of the same size with other bytes no longer matches and the answer names it; a change confined to the stretch between two sampled blocks is not seen (that stretch is about a sixty-third of the file: roughly 0.3 GB of a 20 GB checkpoint and 0.65 GB of a 42 GB one, and a layer-wise merge or a baked-in VAE can sit inside it), so after a copy you doubt, or a merge you swap in, compare whole-file hashes. A node that has just started, or whose file just changed, publishes that file as `pending` for a few seconds on a warm disk (longer on a cold or network one), with no digest, and is a named miss until it has read it; a read that has not finished in a minute is given up on and the file reads `unreadable`. Every node must run the same release for digests to match. Before relying on a match, read `image_recipes` on both nodes and compare the digests.

**Naming the node that runs a video, animation or audio call ([ADR 0082](architecture/decisions/0082-a-media-call-overflows-to-the-fleet.md) Amendment 3).**
`local-offload generate-video out.mp4 still.png "a slow pan" --node node-b` (or `animate-character ... --node node-b`,
`generate-audio out.wav "text" --kind music --node node-b`; MCP `node` on the same three tools) sends the call to the node whose
`node_id` is `node-b`, or to a `delegate_remotes` URL you give instead, and to no other: not another node, and not this machine's own
lane. If that node cannot take it nothing is sent and the CLI prints `DEFERRED (<reason>)` with the node ids and file names in the
reason; the reason as a word is in `data.refusal` (with `missing[]` and `roster[]`), which the CLI prints only with `--json` and an MCP
call returns as it is: `node_unknown` (the answer lists the node ids that answered), `node_is_self`, `node_lacks_route` (it does not serve the lane, or its weights are gone: the answer names
the files), `node_lacks_media_job` (the call carries a still, a reference or a clone sample and the node has not opened that door),
`node_lacks_model` (the node's row for the family you asked for lacks a file) or `node_below_min_vram` (`--min-vram-gb N`). A node that
is down or busy with a text job is a retryable `capacity` defer instead. `--route local` with `--node` is refused. Without
`--model` the node renders its own default family, and the answer's `rendered_family` and `rendered_files` say which. Read a node's rows with
`curl -H @fleet-auth.hdr http://node-b:18811/fleet/health` (`media_recipes`; `bytes` -1 is a missing
weight), where `fleet-auth.hdr` is a file only you can read that holds one line, `Authorization: Bearer <the node's fleet_auth_token>`
(the token stays off the command line, where the process list and the shell history would keep it). A node that has a token withholds `media_recipes`
(with `image_recipes`, the store and the lease's reason) from a request without the bearer: a bare `curl` answers `200` with
the rest of the document and no `media_recipes` key, and a wrong bearer gets that same answer and leaves one line in the
node's journal. This is the named-node release only: a
busy lane still does not send a video, animation or audio call to another node by itself.

**Adding a template.** Follow the contract in
[`render/compose-templates/README.md`](../render/compose-templates/README.md): offline, deterministic,
declared variables, a duration variable, and a README with a measured render. Lint and check must
be clean, the frames must be looked at, and `framemd5` must match at 1, 2, 4 and 6 workers and at
`auto` twice (a frame has to be a pure function of time; see the contract).

**Updating HyperFrames.** There is nothing to bump: the harness installs the newest HyperFrames
release from npm, verified by the registry's integrity field, and the runner requires only the floor
in `setup/floors.json` (ADR 0099). Run `local-offload install hyperframes` (or
`local-offload update --component hyperframes`; it stages, verifies, swaps the new
tree in with the old one kept as `node_modules.prev`, and records the version in `installed.json`),
then `local-offload acceptance`, on every node with the lane. After a release, render a two-template
smoke that includes `captions-bar`: it reads the runtime's `hf-seek` event, which the package
dispatches but does not document, so render its default sample and confirm a known group is on screen
at a known time (the frame at 1.5 s shows "Captions follow the words"), and record the version you
measured in the Measured section of its README as a dated record. A release that changes a flag
the runner relies on is fixed in the runner's argument builders, never by holding the install back.
Never `npm install -g hyperframes`: a global HyperFrames self-upgrades in a detached process. A node
whose install is below the floor reads `compose_video` BOUND-BUT-MISSING and defers `CLI_MISSING`
until the same command runs.

## 4. Drive the coding agent

The agent plans with a local model and acts through tools confined to a workspace. Build it once:
`go build -o local-agent ./cmd/local-agent` (or use `$OFFLOAD_HOME\harness\local-agent.exe`). The agent reads the GPU lease
before it loads a seat, so it is one of the readers `gpu doctor` audits (it is found by name, and a running one by image): a
`local-agent` built before the per-epoch fence is reported `OLD` and keeps the host from enabling card-scoped leases until it
is rebuilt or replaced.

### One-shot CLI (read-only by default) ✅

```bash
local-agent --root . --base http://127.0.0.1:11436 --max-steps 4 \
  "List the files in the workspace and tell me what README.md contains in one sentence."
# ...final answer...
# [local-agent] steps=3 stop=done tools=6
```

### Server mode (`--serve`) — for a chat GUI ✅

```bash
local-agent --serve --listen 127.0.0.1:18808 --base http://127.0.0.1:11436
# [local-agent] OpenAI server on http://127.0.0.1:18808  (model="offload-e4b", fleet_ingress_guard=warn)
curl -s http://127.0.0.1:18808/v1/models
# {"data":[{"id":"offload-e4b","object":"model","owned_by":"local-offload"}],"object":"list"}
```

Each `/v1/chat/completions` POST runs the **full agent loop** over the last user message and returns
the final answer. The endpoint is **unauthenticated** — keep it loopback-only.

**The ingress guard in front of it (`fleet_ingress_guard`, register SF-16).** The banner prints the mode in
force. The default, `warn`, logs a request that `enforce` would refuse and serves it anyway, so a stock install is
not guarded until you set `enforce`. Under `enforce` the endpoint refuses a `Host` outside loopback, the tailnet and
every zone in `tailnet_suffix` / `tailnet_suffixes` (421), any `Origin` header (403), and a POST that is not
`Content-Type: application/json` (415). So a hand-made `curl -X POST` needs `-H 'Content-Type: application/json'`, a
GUI whose browser makes the request itself sends an `Origin` and is refused, and a GUI in a container that reaches
this host as `host.docker.internal:18808` fails the Host rule. Run `warn` until the log shows no client you use
tripping it. `--serve` refuses to start on a config that failed to load, because the guard's mode comes from it. The
guard stops a web page and a DNS-rebinding name; it is not authentication.

### Capability flags (all OFF by default)

| Flag | Grants | Notes |
|---|---|---|
| `--allow-write` | `write_file` (+ `delete`/`edit` only with the next two) | Worktree-scoped, policy-gated. |
| `--allow-overwrite` | overwrite existing files + `edit_file` | requires `--allow-write`. |
| `--allow-delete` | `delete_file` | requires `--allow-write`. |
| `--allow-fetch` | `web_fetch` | egress-allowlist gated; add hosts with repeatable `--egress-host` (bare host or `*.host`). Deny-all by default. |
| `--allow-search` | `web_search` (DuckDuckGo, keyless) | auto-allowlists the search host. |
| `--allow-run` | `run` — an allowlisted program run **directly** (no shell) in the OS sandbox | Linux **and** Windows. Allowlist + broker are the control (see "The runner" below). |
| `--allow-shell` | `run_shell` in the OS sandbox | **Linux only**; no network, FS-confined, syscall-limited. |
| `--allow-github` | `github_api` / `create_repo` / `upload_file` | token from `$GITHUB_TOKEN`, repo from `$GITHUB_REPO`. Use a least-privilege token. |
| `--allow-browse` (+ `--browse-hosts host1,host2`) | `browse` — drive the operator's own running browser (ADR 0060) | Needs the browse lane configured (see "Browse lane" below). `--browse-hosts` is REQUIRED: the CLI always builds unattended, so the grant is refused without a host list, the deny-list can never be lifted, and an audit path is needed (default `agent-audit.jsonl` under the install root: `home`, else `<HOME>/.local-offload`). `agent_run` / `agent_delegate` take `allow_browse` + `browse_hosts` and are unattended the same way; `route` must be `local` and the node needs `agent_allow_browse: true`. Only the MCP tool `offload_browse` is attended. |
| `--listen-trusted-network` | bind `--serve` beyond loopback | prints a loud warning; only on a trusted LAN. |

### Browse lane (opt-in: drive your own browser)

The lane lets `offload_browse` (MCP) and the agent `browse` tool operate your already-running,
logged-in Chromium browser toward a goal. It is off by default. Full reference:
[systems/browse-lane.md](systems/browse-lane.md); decision: ADR 0060.

**What leaves the machine.** The visible page text and element labels go to `browse_decision_url`
on every step. The harness calls only loopback and holds no key, but your endpoint may front a hosted
decision model. Do not browse pages whose text must not leave the machine.

```powershell
# 1. install the browse sidecar (floors only, newest at install); it prints the browse_python / browse_script values
pwsh setup/browse/install.ps1 -OffloadHome <OFFLOAD_HOME>
# 2. config: browse_python, browse_script, browse_decision_url (plain http on loopback only),
#    browse_cdp_url for a dedicated agent profile (e.g. http://127.0.0.1:9333; recommended),
#    or browse_browser (chrome|edge|brave|chromium), browse_timeout_sec (300),
#    browse_max_actions (30, ceiling 60), browse_capture_dir,
#    browse_activate_tab (false; set it true together with browse_cdp_url, see below)
# 3. only if your decision endpoint wants a bearer, set it in the harness's environment
$env:LOCAL_OFFLOAD_BROWSE_BEARER = '<bearer>'
# 4. CLI use (always unattended for browse: --browse-hosts is required)
local-agent --allow-browse --browse-hosts example.com "open the reports page and read the totals"
```

Then start the browser the lane will drive. **Recommended: a dedicated agent browser**, the one `browse_cdp_url` names
in step 2: start it yourself with its own port and profile directory, for example
`brave.exe --remote-debugging-port=9333 --user-data-dir=<OFFLOAD_HOME>/browse/agent-profile`, and log in once, in that
window, to the sites the agent may use. It shows no Allow prompt, so a run nobody is watching works, and the agent stays
out of your everyday profile. The alternative is your everyday browser through its toggle (`browse_browser`, or left
empty for the first browser that allows it): open
`<browser>://inspect/#remote-debugging` (`chrome://`, `edge://`, `brave://`) and tick **Allow remote debugging for this
browser instance**. Recent Chromium builds then ask you to click **Allow** on EVERY new connection, so use the toggle
only with someone at the screen. A run started while nobody is there ends `BROWSER_UNAVAILABLE` when `browse_browser`
is named (the sidecar waits only about 10 s for the click, and the reason says the browser is waiting on its Allow
prompt) and `TIMEOUT` when it is left empty (nothing times out in the handshake, so the run waits until
`browse_timeout_sec`); see [systems/browse-lane.md](systems/browse-lane.md), Error handling. The harness never starts
or restarts the browser.
Restart your MCP client so `offload_browse` appears.

**Dedicated agent browser: turn on tab activation.** The lane opens its tab in the background, and Chromium
draws almost no frames for a background tab, so a dialog that fades in can stay invisible to the model and the
run ends `blocked`. If the browser behind `browse_cdp_url` is a dedicated profile that nobody looks at, set
`"browse_activate_tab": true` in the harness config (and restart the MCP client, as for the other browse keys):
the sidecar then brings the lane's own tab to the front once per run and the page renders normally (measured on a
production web app's confirm dialog: fully open 0.4 s after the click, against 2.1 s left alone). The setting is
ignored without `browse_cdp_url`, because activating a tab switches the window's active tab, and in your
everyday browser that is the tab you are looking at. The config load warns when the setting is ignored, and
`offload_status` shows `remote.browse_activate_tab` (with a note when it is ignored). That field is what the
harness will send; it cannot see the installed sidecar, and only a sidecar from 0.154.2 or later acts on it, so
after an upgrade rerun `setup/browse/install.ps1` first (an older `runner.py` ignores the setting without a log
line). A run that activates also stops the lane's own leftover browser-harness daemon before it starts, so a
daemon left by a crashed run cannot keep the run attached to the wrong browser. For a menu-then-confirm
flow, write the goal so that it describes the dialog's question and says the task is not done and not blocked
while the dialog is open (see [systems/browse-lane.md](systems/browse-lane.md), Common pitfalls).

Controls labelled publish, send, post, delete, remove, pay, buy, checkout, subscribe, confirm, sign out
and similar are removed before the model sees them and rechecked at execution; the run then ends
`denied`. Only on the MCP door (the one attended door) can `allow_labels` exempt an exact label; the
CLI, `agent_run` and `agent_delegate` are unattended and never lift it. For `agent_run` and
`agent_delegate`, set `agent_allow_browse: true` on each node that may run one, and pass `allow_browse`
with a non-empty `browse_hosts`. `done` is the decision model's claim: check the outcome yourself.

### Tool profiles (`--profile`)

`--profile <name>` narrows the advertised tools to a curated subset and adds a tuned prompt + a couple
of worked few-shot exemplars — a small local model selects tools more reliably with fewer advertised.
A profile can only **narrow** the enabled set; it never grants a tool your `--allow-*` flags didn't
turn on.

| Profile | Use it for | Advertised tools (subject to your `--allow-*`) |
|---|---|---|
| `general` (fallback) | anything; today's full capability-gated set | all enabled tools |
| `edit` | a focused code edit in an existing repo | `list_dir`, `read_file`, `search_files`, `edit_file`, `write_file`, `update_plan` |
| `build` | edit-then-verify (needs `--allow-run` / `--allow-shell`) | edit set **+ `run` / `run_shell`** |
| `research` | find + read sources (needs `--allow-search`/`--allow-fetch`) | `web_search`, `web_fetch`, `summarize_file`, `read_file`, `list_dir` |
| `github` | prepare files then publish (needs `--allow-github`) | edit set **+ `github_api` / `github_create_repo` / `github_upload_file`** |

`general` is the FALLBACK, not necessarily your box's default: with no `--profile`, the resolution
is explicit > config `agent_profile` > `general`, so a tier that seeds `agent_profile` (today
`ampere-6` seeds `research`) uses that instead. `local-agent` prints which profile it applied and
whether it came from the flag or the config; `agent_run` reports it in the response.

`--two-tier` conflicts with any **non-default** `--profile` (two-tier picks the architect/editor
toolsets itself); the CLI rejects that combination, while `--profile general` or an empty value
coexists.

### The runner (`--allow-run`) + how to extend the allowlist

`run` executes an **allowlisted program directly — no shell**: you pass `command` (a bare executable
name) and an `args` array that is handed to the program literally (no pipes, globs, redirection, or
`&&`). The executable allowlist is the real control:

```
go, gofmt, python, python3, pytest, npm, node, cargo, git
```

A command must be a **bare name** (a path or `./go` is refused) that resolves on the **trusted system
PATH** — a `go.exe` planted inside the worktree is not resolvable and is refused. Every accepted
command is broker-gated and written to the audit log. **On native Windows, reads and network are not
contained** (Job Object + low-integrity writes only) — see [Diagnose](#6-diagnose) / README Security.

**Extend the allowlist** by editing `runAllowedExecutables` in `internal/agent/runtool.go` and
rebuilding `local-agent`. It is a compile-time list on purpose (no runtime flag) so the confinement
surface is auditable in the source.

### Two-tier (`--two-tier`) — plan once, then execute

`--two-tier` runs aider's architect/editor one-shot handoff: the **architect** (`--architect-model`,
default `gemma4-26b-a4b`) drafts one complete, standalone plan with read/search tools only, then the
**editor** (`--editor-model`, default `offload-e4b`) executes that plan as its **sole** message — it
never sees the original request or any history. On a single GPU this is **exactly one cold model
swap** (plan-once, not per-step alternation); on a dual-GPU box (profile `dual-gpu`) the two models
are resident on separate cards, so it is **zero swap**. A degenerate/empty architect plan falls back
to a single-model run of the original objective (logged as `fallback=…`). `--allow-*` flags gate the
**editor's** write tools; the architect is always read-only.

### Circuit breakers & budget

- `--max-steps` (default 12) — hard step budget, owned in code.
- `--max-same-tool` (default 8) — cap on calls to any one tool per run; the breaker for a model that
  loops (e.g. repeated reworded `web_search`). Negative disables; 0 → built-in default. `read_file`
  is the exception: it is metered in characters (the read budget, register D-103) and the cap counts
  the files it opens and any page that gave nothing new (a failed page, or one past the end of the
  file), not the pages of one that return lines — see `docs/systems/coding-agent.md`.
- `--max-tokens` (default 4096) — planner tokens per completion. Must be large enough for the biggest
  tool-call argument (e.g. a whole file's content) or the model's JSON gets cut off mid-string and
  the call fails. 4096 is the tested value; do not lower it for write-heavy runs.
- `--ctx-tokens` (**default 0 = AUTO**) — the served model context window the loop's transcript
  compaction budgets against. At 0 the loop **probes the serving endpoint** for the planner model's
  live `n_ctx` (`/upstream/{model}/props` on llama-swap, `/props` on a bare llama-server) and falls
  back to 8192 only when the endpoint will not answer — an *assumed* window killed real runs with
  `exceed_context_size` 400s (ADR 0015). An explicit value **overrides the probe** and is warned
  about when it exceeds the served window; set one only to match a tier's `--ctx-size` deliberately
  (the install prints the profile's value). The derived usable **input budget** is
  `ctx-tokens − max-tokens − 512`. Setting it too high lets the transcript overflow the real window
  (a 400); too low compacts sooner than necessary.
- `--gcf-compact` (default **ON** — measured flip decision 2026-07-24) — the compaction ladder's LOSSLESS first rung: over budget, older
  tool results that are JSON arrays of flat objects are re-encoded columnar (keys stated once,
  `internal/gcf`, round-trip proven — nothing is lost) before any lossy rung runs. The same
  transform guards the offload pipeline's context trim via the `gcf_compact` config field: an
  over-budget input's JSON is compacted losslessly before the head/tail cut, converting would-be
  truncations into full-fidelity completions.
- `--skeleton-prune` (default **ON** — same 2026-07-24 flip decision) — the next, lossy-structural rung: over budget, older tool
  results are reduced to signal-preserving **skeletons** (head/tail lines + error/failure/warning
  lines kept, elided runs replaced by `[... n lines elided ...]` markers) before the existing
  bare-marker and turn-drop rungs run. Deterministic and local — no model call, no added latency.
  Long multi-step runs keep *what went wrong earlier* visible to the model instead of losing whole
  older results at the first budget crossing.

### Unattended runs: risk rules, parking, and the advisory judge

The CLI is non-interactive, so every broker "ask" is **deny-and-queue** — deferred approvals and
parked calls land in the ask queue (`--ask-queue`, default `agent-asks.jsonl` under the install root: `home`, else `~/.local-offload`) for
your morning review. Three mechanisms bear on an unattended run, but **only the first actually
gates** — the other two report:

- `--rules <file>` — a versioned JSON array of **structural, tighten-only** policy rules
  (`{kind, glob, decision, severity, reason}`): action kind + a glob over the worktree-relative
  path (write/delete) or host (fetch); `decision` must be `deny` or `ask`, never allow. A built-in
  floor already denies secret-material paths (`.env*`, `*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`).
  A bad or missing file fails the run rather than silently deactivating the table; the audit trail
  records which rule fired at what severity.

  **With the flag empty, an unattended run loads a built-in default table** (0.55.0; embedded in
  the binary from `internal/agent/unattended-rules.json`, so no path and no installer step): every
  `delete` queues for your review, behind hard denies (write AND delete) for evidence files
  (`*.jsonl`), model weights (`*.gguf`/`*.safetensors`) and worktree-root CI workflows; lockfile
  and `go.sum` **hand-edits** hard-deny (lockfile *deletes* queue like any other delete); config
  and dependency-manifest writes (`config.json`, `settings.json`, `go.mod`, `package.json`,
  `requirements.txt`, `*.yaml`, `*.toml`, …) queue too — note this includes **creating** a new
  config file, so a task like "scaffold a compose file" produces a queued ask rather than a file.
  Ordinary source writes stay governed by your `--allow-*` posture flags, so the agent can still
  do the work you granted, and the table only sees the write/delete tools — file operations run
  *inside* the shell/run cage are governed by the OS cage, not by rules. Two explicit outs:
  `--rules <path>` **replaces** the default with your own table (replacement is what lets you
  loosen the delete catch-all — rules themselves only tighten), and `--rules off` runs ungated
  exactly as before 0.55.0. An alternative starter is `examples/agent-rules.json` — it
  additionally queues source overwrites (`*.py`/`*.go`/`*.ts`/`*.js`/`*.mjs` → ask) and every
  fetch, but covers fewer config/manifest globs than the default. It is a **repo file that no
  installer packages**: from a repo checkout, `local-agent --rules examples/agent-rules.json …`;
  from an installed binary (`$OFFLOAD_HOME\harness\local-agent.exe`), copy the file somewhere
  durable — e.g. `~/.local-offload/agent-rules.json` — and pass that path.
- **Risk parking (reporting, NOT a gate — measured 0% recall)** — every effectful tool asks the
  model to self-annotate `security_risk` (low/medium/high). A call flagged `high` (or with an
  unrecognized value — fail closed) is **parked**: never executed, recorded in the ask queue, the
  run continues without it.

  **This is a backstop, not a gate — do not rely on it.** The mechanism above is real and fires
  as described, but the *input* it depends on was measured to be a literal constant. Pooled over
  BOTH production agent seats and five arms (137 effectful calls), **83 of 83** emitted
  annotations were `low` — zero medium, zero high — *including all 81 structurally destructive
  calls*, and it held under an escalated arm (deleting an entire source tree still self-reported
  `low`). Park-gate recall was **0/81**, and the annotation was least likely to be emitted on the
  calls with the largest blast radius (`web_fetch`: 0/6 annotated). Treat the model's
  self-annotation as telemetry, never as a security control.

  The control that actually fires is the **structural rule table**, which is evaluated against
  tool + arguments rather than against the model's opinion of itself — and since 0.55.0 it is
  **on by default for unattended runs** (see `--rules` above). The ungated posture still exists,
  but only behind the explicit `--rules off`.

  **The `UNGATED` note — when you will see it, and when you will not.** It is not a general startup
  banner. It is appended by the agent builder and printed to stderr by the CLI
  (`[local-agent] UNGATED: …`) on exactly one condition: the run is **unattended**, holds at least
  one of `--allow-write` / `--allow-delete` / `--allow-overwrite` / `--allow-shell` /
  `--allow-github`, **and** you passed `--rules off`. `--allow-run` alone does not raise it (the
  table cannot gate cage execution anyway), an attended run never raises it, and an empty
  `--rules` cannot raise it any more (the default table loads instead, announcing itself with a
  `default unattended rule table ACTIVE` note when `--allow-write` is granted). It is a
  **note, never an error** — the run proceeds; you asked for exactly that.
  **Scope:** this is the CLI/queue path, the path that grants `--allow-*` and sets unattended. The
  MCP `agent_run` front door passes no write/delete/shell/fetch capability at all, so it never
  raises the note and none of this changes what an MCP caller can do.
- **Effect accounting + the judge** — every tool call gets an honest effect status
  (`committed`/`failed`/`unknown`/`none`); the CLI prints the non-committed records at the end of a
  run, and `agent_run` returns them (`effects`, `effects_flagged`). `agent_run judge=true` adds one
  end-of-run **advisory** completion grading the flagged records for your review — it never gates
  anything.
- **Environment rules (`agent_env_rules`, 0.113.22)** — a per-box table that shapes how the SEAT
  behaves inside the loop: `deny_tools` / `allow_tools` (withheld from the offered tools),
  `max_calls_per_tool` (the N+1th execution is refused with a reason and the tool is no longer
  offered), `arg_limits` (whole-number caps on numeric arguments, e.g. `{"read_file":{"limit":400}}`;
  the result tells the model what was capped), `max_observation_tokens` (one tool result bounded,
  head+tail, minimum 64), `observation_strip` (regexps removed from tool results), `rewrite_error`
  (a tool error matching a pattern becomes a short line the model can act on). Start from
  `examples/agent-env-rules.json`; a bad table fails by name (`local-agent` exits 2, `agent_run`
  defers, a fleet node defers with `defer_class: config`). Try a candidate without touching the
  config: `local-agent --env-rules candidate.json …` (`--env-rules off` runs with none). Every
  agent result reports `rules_fired` and a per-call `trace` (tool, status, bytes read, rule) — the
  same fields land in the delegation-log corpus, so a night of ordinary traffic says which rule a
  seat needs. These are NOT the risk rules (`--rules`), which gate effects; env rules never grant
  or deny an effect. ADR 0036; details in [systems/coding-agent.md](systems/coding-agent.md).

**Auditing a recipe (ADR 0036, amendment 2026-10-15).** A recipe is a proposed `agent_env_rules` table kept as a file in
the recipe store (`<data home>/recipes/<id>.json`, or `--store DIR`), strict like the `--env-rules` file and tagged with
the capability class of the seat (`seat_tag`) it was distilled for. To find out whether it earns a place on a seat,
freeze a pre-registration (`gate freeze`) with judge `{deterministic, acceptance-dsl}`, the primary on the unit interval
(the verb scores every run 1 or 0), at most tokens-class efficiency metrics (the only class it can read off a run) and no
incumbent, write a contracts file (a JSON array of `{"id": <a declared task>, "contract": <agent contract>}`, one for every
task the baseline declares, held-out ones included, where every contract carries acceptance checks), and run the audit
under a lease, because it runs the seat and never takes one itself:

```
local-offload gpu reserve --wait 8h --drain --unload-seat --for 2h --reason "recipe audit" -- \
  local-offload recipe-audit --recipe ID --seat ALIAS --seat-tag TAG \
  --contracts tasks.json --baseline baseline.json --engine-build BUILD [--attempts N] [--input-out gate-input.json]
```

Each task runs on the seat as it is and with the recipe's rules in place of the box's table. The recipe is published only
when the gate says ADOPT and the mean steps are no worse; otherwise it is demoted, and either way the store records the
audit. Both arms are stamped with the pin the harness takes of the seat that served the runs, so the gate's era is the seat
and not its alias. Exit 0 means the store holds the result (read `status` in the printed report). Exit 2 is a refusal and
records nothing. Most refusals come before the first seat call: no lease (`no_lease`), a lease this process does not hold
(`lease_not_held`), a `--seat-tag` that is not the recipe's (`seat_tag_mismatch`), or input the store or gate will not take
(`bad_input`: a baseline on another scale or with a money or memory metric, a contracts file that leaves a declared task
out, a build that cannot be hashed). Two end an audit that already ran some seat calls, so re-run it under a fresh lease:
a lease that moved on mid-audit (`lease_lost`), and a seat whose pin moved or is missing (`seat_pin`: a model swapped
behind the alias mid-audit, a served run the harness could not pin, or an audit in which nothing was served).

Details and rationale: [systems/coding-agent.md](systems/coding-agent.md).

### The timeout chain, and walls sized from the seat's own rate (0.115.21, register D-03)

One contract crosses six clocks. None of them is derived from another; this is where each one is set and what it
does NOT contain:

| clock | set by | contains | does not contain |
|---|---|---|---|
| `timeout_sec` (contract; default 300, box `agent_timeout_sec`) | the caller | the node's whole run: probe, build, loop, structured re-pack | placement wait, admission, cold load, queue time |
| delegator poll | **held open while the node reports progress** (0.131.0): the poll stays alive while the node's `progress.last_progress_ms` + its `stall_allowance_sec` + grace is in the future, bounded by the node's `ceiling_sec`; a node that reports none is polled as before, and a stale report is not extended (the deadline reason then says when the node last moved and what it was allowed). Otherwise `timeout_sec` + a grace window — for an UNSIZED (`timeout_auto`) contract, the bound the target node's advertised `seat_rate`/`seat_budget` imply, by the node's own arithmetic, plus a 300 s allowance for the node's admission (cordon, pre-flight, cold load, coherence probe — all spent in state `running`) until the node publishes a started `wall_sec`, at which point the clock is re-anchored on that wall start and the allowance drops; the 900 s cap when the node advertises no rate, or when the run is placed on a composite LAYER seat whose rate health does not publish (D-116); C-27 credits back intervals the node PROVABLY spent queued (both endpoints observed `accepted`), bounded at the queue budget, `clamp(1.5 x the node's ETA to start + 30 s, 60 s, poll bound + grace)` (ADR 0063; a node that publishes no ETA keeps `min(poll bound + grace, 5 min)`); a job that never started is a `queue deadline` FAILURE, never a `budget` defer, unless the node confirms it took the job back, which re-places the subtask (ADR 0064) | waiting for the node's answer | the capacity wait (the call's deadline less a reserve, else `agent_placement_wait_sec`, ADR 0073; `results[].capacity_wait_sec`) |
| admission + warm-up (`agent_admission_wait_sec`, default 300) | the node, BEFORE its wall starts (D-64) | another model's swap on the endpoint, then the seat's own cold load (`admission_wait_sec`, `admission_note`) | anything after the first token |
| coherence probe (`agent_coherence_probe`, default `cold`) | the node, after the warm-up and still BEFORE its wall (D-118) | one ≤ 96-token completion asking the freshly loaded seat to call `read_file` and answer DONE (`coherence_note`; its time is added to `admission_wait_sec`) | the loop, the re-pack, anything the contract asked for |
| node wall (0.131.0: the EXPECTATION, ADR 0055) | `timeout_sec` / the auto wall, reported as `wall_sec` | what the node expects the run to take — what the delegator sizes and anchors from | **it ends no run.** Until 0.130.x it was a context deadline over the loop, and a 27B seat streaming its 949th token at 984 s died at the 900 s cap exactly like a hung engine. One thing is held to it: the structured re-pack, by a token budget and a deadline check before each attempt against `min(ceiling, bound + 30 s)` at the seat's rate, where the bound is the later of the wall's end and the time the re-pack is owed (`1.5 x the re-pack's expected tokens (the answer's, or the schema's bound when that is smaller) / the seat's rate` from the moment it began, stopping at the call's deadline; register C-80 and F44/C13, ADR 0055 items 9 and 10); a request in flight is never cut, and a re-pack that the ceiling or the call's deadline leaves too little time for gets no node-side attempt at all: a `budget` defer carrying the finished answer, which the delegator re-packs on the doors that wire the rescue (`agent_delegate`, `offload_research`, the `delegate` and `research` verbs) |
| local `agent_run` wall (register D-102) | `timeout_sec`, else the box's `agent_timeout_sec`, else 180 | the loop: a HARD context deadline on this door, which never adopted the liveness monitor below — a run still producing at the wall is cut and deferred, but its forced final answer is first fitted to what is left of the wall (register D-134, below) | admission (cordon, pre-flight, warm-up, probes), and the sizing refusal, which comes before any of it |
| **stall allowance** (0.131.0) | the node, per phase, from the seat's measured rates | how long the seat may go without a progress event — a streamed token, a tool call, a phase change; on a seat listed in `vllm_seats`, every frame of generated token ids, including the ones vLLM's tool parser is still holding back (0.140.1: the loop asks for `return_token_ids`) — before the run is filed `stalled: no progress for Xs in <phase> (allowed Ys: …)` as **infrastructure**: admission → the admission budget; prefill → `prompt tokens ÷ prefill_tok_s × 1.5 + 30 s`, where the prompt is the transcript at the run's token density plus the tool-spec block every request carries (never under what the server counted for the call before), and the rate is the SLOWEST of the seat's last five solo samples (`prefill_slow_tok_s`; the average `prefill_tok_s` for a store that has no window yet) — a heat-soaked seat is sized as the slow seat it is (100 tok/s assumed until the seat-rates store has measured it — from the time to first delta, any engine); decoding → 20 deltas at the decode rate; a tool → its own cap + 30 s; the re-pack → `max(120 s × the seat's load, 1.5 × expected answer tokens ÷ the seat's decode rate + 30 s)` (register C-66: the re-pack streams, so this bounds silence, and it runs with no transport bound of its own; load 1 = `max(120 s, …)`, and the load stretches the flat 120 s only, never the answer estimate); floor 60 s; while other requests share the seat the prefill rate is divided by the load too (ADR 0066) | the ceiling; a producing seat, however slow; a seat that is LOADING (the cold-load hold below) |
| **cold-load hold** (0.140.0) | the node, while a request may be waiting for its first byte (admission, prefill, re-pack): `/running` read every 5 s and before any stall is filed | two parts in phase `cold-load`. **Loading** requires positive evidence: the seat's row is `starting`/`stopping`, or it is absent while another row is `starting`/`stopping`. It is bounded by `max(600 s, 2 × the seat's measured cold_load_sec)` from the silence, clamped to the run ceiling, and a defer reads `stalled: seat still loading after Xs in cold-load (…)`. **Post-ready** follows the load, or the admission warm-up: the first completion gets `max(120 s, 2 × the phase's own allowance)`, and a defer reads `stalled: no byte for Xs after the seat read ready, in cold-load (…)`. Both are **infrastructure**; the first byte ends the hold | a seat merely absent from `/running` with nothing loading, or an unreadable `/running` with no load seen (the prefill clock then runs as before) |
| **seat-down recovery** (ADR 0066) | the node, when the seat's engine goes down under a running call | a seat is DOWN when its engine's counters stayed flat for the flat bound while llama-swap still lists it ready (**wedged**), or when it left `/running` inside an established hold, or a call failed like a dead seat (stream cut, 5xx, refused connection) and a fresh read shows the seat starting, stopping, unlisted or refusing connections (**died**). The node cancels only that call and waits for the seat in phase `cold-load` as ONE episode, bounded by the cold-load ceiling counted from the first verdict (and the run's ceiling), then re-issues the same step; a recovery spends no step. A wedged seat must show a change (counters moving, or a restart) before the re-issue. A seat llama-swap does not list is started by the re-issue itself; if that start fails (the launcher refusing, every request an instant 500) the node waits one poll and triggers again until the bound, and a start that never succeeds is filed at the bound naming the attempts. A seat seen serving again earns exactly one re-issue: a second failure before the answer ends the run. A recovery is counted (`seat_recoveries`) when the re-issued call's first byte arrives, and `seat_down_wait_sec` books the whole episode; a run recovers at most twice. What does not recover is filed `seat down: …` as **infrastructure** (a seat lost during the structured re-pack files the same prefix, with `(during the structured re-pack)` appended and the finished answer flagged `schema_miss`: the delegator re-packs that answer itself first, and only when it cannot), and the delegator re-places that contract on another node with the wait credited back (and, after a failed rescue, the wall the rescue spent), even when the alternative node is busy (a full node answers 503 and the contract waits for room in the bounded, credited capacity wait; while this box's own run-cap line is full an untried remote is preferred to it) | a failed call on a seat that reads ready and readable (an ordinary error), a cut tool call (the engine answered), a thrash (the engine steps but produces no token: still the plain `stalled:` of ADR 0061), and absence from `/running` on the first look of a silent request (the ADR 0055 rule) |
| **ceiling** (0.131.0) | the node: `max(3 × estimate, 2 × wall, 1800 s)`, cap 4 h (`ceiling_sec`) | the safety net over everything; a run still producing when it passes is filed `ceiling Ns reached while producing (T tok at R tok/s)` as **budget** — the sizing signal | — |
| loop budgets | `agent_max_tokens` per step (default 1,024; 4,096 on a thinking seat), the final answer at 4× (cap 8,192) **narrowed to what the remaining wall can decode** (0.122.1, D-95), 12 steps (`max_steps`, cap 12 remote), the forced final step (D-89), the `read_file` character budget (D-103: sized to the steps, so it never refuses one read per step) | one completion each | the stall allowance and the ceiling — a step that generates for minutes is progress, and only silence or the ceiling ends it (0.131.0) |
| engine + lease | the client's request timeout (split into connect / first token / stream, `llamaclient`); the media lease. The generation routes (image, edit, inpaint, upscale, batch, run-graph, video, animate, audio) take the media lease, except the `tts_endpoint` voice lane (the speech server owns its card) and a `pipeline-job` (it holds only the in-process media slot; the stages it runs lease for themselves). A route that takes the lease takes it for the call's own timeout, read from that route's own key (`imagegen_timeout_sec` for image, sdcpp image, batch and run-graph calls; `videogen_timeout_sec`; `inpaint_timeout_sec`; `upscale_timeout_sec`; `gen_edit_timeout_sec`; `animategen_timeout_sec`; `audiogen_timeout_sec`), and the render is killed at that same value, so a render never outlives the lease the pipeline took for it (a batch takes the per-image timeout times its jobs). The code defaults are `imagegen_timeout_sec` 720 and `videogen_timeout_sec` 1,500; the tier seeds in `setup/templates/profiles.json` raise them, to as much as 3,600 s for an image and 7,200 s for a video (a seed that sets a video timeout of 5,400 s gives a 5,400 s window and a 5,400 s kill), so on a seeded install a wedged render holds the machine-wide media lease for that long: size a resize from the seeded figure, not the default. That window is granted whole, whatever its length (a batch of any number of images takes the per-image timeout times its jobs, whole too): `gpu_max_term_min` (default 360, six hours) does not bound it. That cap is the renewal term of a `gpu reserve` hold, renewed by that hold's own holder; a media call's lease runs no term check, so while the render is inside its window nothing labels the lease overdue or expired or asks it to yield, and it is kept by its heartbeat (renewed every 15 s while the render runs). Past the window the same timeout is the call's own deadline, so the render is killed there and the lease released; a takeover by the operator, or the death of the process that holds the lease, is what frees the card sooner. A long timeout therefore costs one thing: how long a wedged render holds the machine-wide lease. The lease library's 3,600 s window (`DefaultTTL`) is only the fallback for a lease acquired with no window, which no generation route does. `gpu_wait_ms` 90,000 (code default) is the queue wait, how long a call waits behind a holder before it defers, not a lease length. The one place a window can be shorter than the job is a `gpu reserve` wrapper: a call run inside it inherits the wrapper's lease and its `--for` window (default 45 min), and a window that ends first is renewed or labeled expired, never a release; size the wrapper's `--for` from the same key as the route it wraps (a seeded 5,400 s video wants `--for 90m`) so the lease is never labeled overdue while the render is inside its own timeout | one request / one lease | — |

**Reading a run while it runs (0.131.0).** `gpu status` and `offload_status.gpu_lease.activity.runs[]` carry one
liveness line per run — `producing 3.4 tok/s, last token 2s ago (allowed 60s in decoding)` while the seat streams,
`silent 187s of 214s allowed in prefill` while it does not — beside the holder heartbeat (they are different things:
register C-32). `/fleet/jobs/{id}` publishes the same as `progress`. A `stalled:` reason means the SEAT went quiet
inside its allowance (check the seat, not the contract); a `ceiling` reason means the run was still producing at the
safety ceiling (size the contract, or split it). The seat-rates store's `prefill_slow_tok_s`, the slowest of the last five solo
samples (the average `prefill_tok_s` for a store with no window yet), is what sizes the prefill
allowance; until a seat has measured one, 100 tok/s is assumed and the reason says `assumed`. It is one
request's rate: when other requests share the seat (this box's run registry, and the busy hold's engine gauges) the
prefill allowance divides it by the load and the reason prints it (`24000 tok / (400 tok/s / load 4) x 1.5 + 30s`),
and only a run known to have had the seat to itself updates `prefill_tok_s` and its window (a run whose load nothing could answer
for, or that waited out a seat going down, teaches it nothing). The busy hold's wedge bound follows the load only on
an engine that cannot see a prefill (`/slots`, or vLLM without a KV-usage gauge). A `seat down:` reason means the
seat's engine hung or died under the run and did not come back inside the recovery wait (see the table); a
`seat not serving:` reason means llama-swap answered a 5xx for the seat (a start that failed, a health check that
timed out) — not contention, so `concurrencyLimit` is not the knob; `seat contended:` is a 429 only. Inside an `agent_delegate` or `offload_research` call the delegator re-places a `seat down:` defer on another node, and that re-placement answers to the call's deadline (`agent_call_deadline_sec`, [ADR 0065](architecture/decisions/0065-the-whole-call-has-a-deadline-below-the-clients-abort.md)) like any other attempt: the credit for the dead seat's wait lengthens the retry's own budget, never the call's, so a re-placement still running at the deadline is cut (its own `budget` row, and the cut is in the first attempt's `retry_note`), and a `seat down:` defer that arrives after the deadline is the call's `call deadline reached` defer and is not re-placed. While the seat is
being loaded under a waiting request the line reads `… allowed in cold-load` (0.140.0), and a `stalled: seat still
loading after …` reason means the load itself outlasted the cold-load ceiling: look at the seat's load, not at its
prefill. `stalled: no byte … after the seat read ready` means the seat loaded and then answered nothing inside the
post-ready bound: a seat that wedged right after its load.

**`doctor` names a clock that has drifted.** `local-offload doctor` prints a `config findings` section — one
`FAIL` row and a non-zero exit per value that loads and then cannot do what it says — and two of its rows are
clocks from this table: `gpu_wait_ms` more than 3x `vision_gpu_wait_sec` (register C-33: a deployed 600,000 ms
against a 90 s vision wait is ten minutes of blocking on every image/video/audio/run-graph call, against that
key’s own documented 90 s design), and one row per RETIRED key the file still carries — `videogen_wait_ms`,
`audiogen_wait_ms` — which until now was a single stderr note at startup that scrolls past every command.

**Sizing rule.** A wall is worth `cold load + (thinking auto ? one think block at the step budget : 0) + (steps − 1) ×
(128 tokens + 6 s prefill) + final budget`, all at the seat's decode rate. The node computes exactly that before every
run and publishes it beside the result: `results[].wall_estimate_sec`, `min_turn_sec` (cold load + one turn at the final
budget — the least a retry is worth, D-46) and `wall_note` (the arithmetic, or why there is none). `seat_tok_s` is the
run's own measured rate (completion tokens per second of call wall over the completions that generated ≥ 1,024 tokens;
tool-call completions of 25–60 tokens are prefill-dominated and excluded), `calls[].ms` the wall of each completion. The
rate and the cold load are remembered per seat in `<state root>/seat-rates.json` (the GPU-lease root: machine-local by
design; EMA 0.3 on the rate, the slowest of the last five loads, written under an exclusive lock file so two
processes on one box never drop each other's sample); until a seat has a sample, `agent_seat_tok_s` in the
box config stands in, and with neither the note says so and no numbers are published. **The estimate never changes an explicit
wall** — a contract that names `timeout_sec` runs under it exactly as before. A contract that names NONE is sized by it
(0.126.0, register D-03): intake stamps `timeout_auto`, the executing node clamps this estimate to 300..900, runs under
that and reports it as `results[].wall_sec` (`wall_note` prefixed `auto wall`), and every retry carries an explicit
remainder instead; a seat with no rate yet runs the 300 s default. **The delegator polls it at the node's wall, not at
the cap** (register D-116, and the paid-down "accepted cost" of 0.126.0 — a node that acked and then died silently used
to be abandoned 900 s later even when its own wall had been 300 s). The delegator's BUDGET — the retry remainder, the
re-placement ledger — still holds the cap open, because the node may legitimately size anything up to it; its POLL CLOCK
is sized from that node's `/fleet/health` `seat_rate` + `seat_budget` through the same function the node sizes its wall
with (`seatrate.AutoWallFor`, clamped to the same 300..900), so the two clocks cannot drift. The node's own number
overrides the estimate the moment a running poll publishes it: `/fleet/jobs/{id}` now carries `wall_sec` while the job
RUNS, and the delegator RAISES its bound to it — it may never lower it, because abandoning a job the node is still
running inside its own wall is the one failure this must not buy. The deadline message names the bound it used (`poll
bound: sized from <node>'s seat_rate 30.0 tok/s (5 samples): 612 s` / `poll bound: the node's own wall 720 s` / `poll
bound: cap: no seat rate advertised by <node>`), and the same line rides every result as `results[].poll_note`. A
contract that names its own `timeout_sec` is polled exactly as before, with no note. The `queue` route still polls at the
cap: its claimant is not a node the delegator chose, so there is no health view to size from.
For an explicit wall, `wall X s is BELOW the estimate` in `wall_note` (and
the node log) is the caller's signal to size the contract. A contract with an `output_schema` carries one more term
(0.117.2): `+ re-pack ≤ N tok` — one final-budget completion for the structured re-pack of a prose answer, an upper
bound that a seat answering in the object shape never pays (a 285 s re-pack on the 27B sat outside every floor on
2026-09-10). The retry floor on the delegator is the RETRY seat's (0.117.2, register D-46): `max(agent_retry_min_sec,
min_turn of the seat the retry lands on)` — a remote node's `/fleet/health` `seat_rate` (its remembered rate and cold
load) at its own `seat_budget`, the local seat's `seat-rates.json`, plus the re-pack term for a schema contract; the
retry note names the publisher, and only a node that publishes no rate falls back to the first attempt's
`min_turn_sec` (the 2026-09-10 shape: a 201 s floor from the 4B cleared, the retry landed on the 27B whose own floor
was ≈ 484 s). Since 0.184.0 the cheap check made BEFORE a retry node is chosen reads `agent_retry_min_sec` alone: it used to
read the first attempt's `min_turn_sec` too, a number that belongs to a seat the retry never uses, and 7 of the 14 retries skipped in one day
were refused for it ("floor 1112s, min_turn_sec of the seat on <node>") before any node was asked. `seat_budget` is also what a caller reads to MATCH budgets across seats — a delegator's config does not
travel with the contract. Reference (2026-09-10, ledger-01 on both
seats): the <node-b> 27B TP2 seat at ~30 tok/s needs ≈ 600 s thinking off / ≈ 730 s auto INCLUDING a 210 s cold load for a
12-step, 8,192-token-final contract — a 600 s box default is at the edge and 900 s is the honest wall; the <node-c> 4B at
~30 tok/s answers the same contract in one step in 250–380 s with a 34 s cold load.

**The local `agent_run` door sizes the run before it starts (register D-102).** That door's wall is a hard context
deadline, and until register D-102 it computed no sizing at all: a caller who named a 420 s wall against an estimate of 928 s
(`min_turn_sec` 807 s) learned it from `context deadline exceeded` after ten steps. Now the door sizes the call from the
seat's remembered rate (the rate store, else `agent_seat_tok_s` for the box's own agent seat only) with the node's own arithmetic, BEFORE the fence check,
the cordon, the warm-up or any request to the seat, and every call that gets past argument validation and seat placement — answered or deferred — carries
`wall_estimate_sec`, `min_turn_sec` and `wall_note` (`wall 420 s is BELOW the estimate 928 s …`). A wall that cannot hold
even the smallest answer is REFUSED before step 1: one tool step (128 tokens plus a 6 s prefill) and a 64-token final at
the seat's rate, with no think block, no re-pack and no cold load (`seatrate.MinViableSec`). The refusal is `deferred`,
`defer_class: budget`, `steps: 0`, with nothing sent to the seat, and its reason names the wall (and whether it is
`timeout_sec` or the box default `agent_timeout_sec`), the floor, the run's estimate with `min_turn_sec`, and the
`timeout_sec` that fits. The floor is one tool step plus the final whatever `max_steps` says, so fewer steps never clear the
refusal. It is the one the delegator applies to a remote placement (`feasibleFinal`); a parity test
fails when the two drift. **It is not `min_turn_sec`.** That figure is a cold load plus the configured max final, and the
INV-5 rider (ADR 0050) forbids refusing on it: a 900 s wall on a 7 tok/s 27B seat that completes its contracts sits under
it. So a wall between the floor and the estimate runs, and `wall_note` says by how much; no rate means no floor and no
refusal. `agent_seat_tok_s` is the agent seat's rate and is never lent to another seat: a model you name with `model`, or a
seat composite placement picks, is sized from its own rate-store entry or not at all (no floor, no refusal, no published
estimate). The delegation door's node does not do that yet: its own sizing (`SeatPolicyFor`) still applies the configured
rate to any seat it runs that the store has not measured, and register C-56 (a node-side refusal) has to stop that before it
refuses on the estimate. **The row's own case is cured by the final-budget fit (register D-134, which closes what D-102 left PARTIAL):**
a 420 s wall against a `min_turn_sec` of 807 s is above the floor (about 45 s at 5 tok/s, 13 s at 30 tok/s), so it runs, and the door
installs the D-95 fit (below) on its loop. At the forced final step the answer opens at the largest budget the wall that is LEFT can
decode at the seat's rate, not at the configured worst case: a final that would have asked a 17.2 tok/s seat for 476 s of decode inside a
420 s wall asks for at most about 378 s, and for about 270 s once the tool steps have spent their share of the wall, so the run answers
instead of being cut at the deadline. The fit narrows the final answer only. It does
not shorten the tool steps, and a wall that buys less than the 1,024-token floor at the seat's rate still ends at the deadline
(`budget_note` says `the wall will be the stop`). `final_budget_fit` and `budget_note` ride on the answer and on a run that died at its
wall, only when the fit narrowed something (a run whose own final was not narrowed, because it answered early, was cut before its forced final or reached it with wall to spare, carries the fit predicted when its wall started, as the node's result does); the refusal (which starts no run), a wall with room and a seat with no rate carry neither. A final the seat ended on that budget
(`finish_reason: length`) is a partial: it is still the `output`, flagged `output_truncated: true` with the loop's `stop_note` and a `truncation_note`
naming the budget the loop's fit opened it at when it narrowed it, and a complete answer carries none of them. The
two agent doors share one copy of the arithmetic (`pipeline.InstallFinalFit`), so the node and this door size one run alike. A call
refused before a seat is chosen (a bad argument, or a composite placement guard refusing the seat) carries no numbers, and a routed call carries no sizing of this box either: a contract sent
through the fleet carries its wall to the node, where it is an expectation (ADR 0055).

**A seat only the fleet runs (register D-125).** `agent_run(model: <seat>)` used to refuse any name this box's roster lacks,
however many nodes ran it. A name the local roster lacks is now looked up on the fleet: the door reads the roster's health once
and sends ONE contract, at `route=remote` and narrowed to the nodes that match, to a node whose `agent_seat` is that name
(without regard to case; a composite node's agent seat in a named layer counts). The result names `node`, `placement`, `seat`,
`model` and `executed_on`, plus `node_reported` only when the node's own result named itself by another id, and is withheld if it came from another seat (a node that changed its agent seat after it was read; so is
the finished prose a deferral carries). The contract is a reasoned remote pin (`pin_reason: operator`), so when the delegator retries an
attempt (a seat that abstained or went down) the retry goes to another node that runs the name or is not run, and never to this box's own agent seat.
A name nobody runs keeps the old refusal and adds where the fleet looked, `config` when every node answered and `infrastructure`
when one did not; a name a node serves without running it as its agent seat is refused naming the seat it does run, because the
contract names no seat and the node runs its own. `route: "auto"`, `"spread"` and `"queue"` refuse a model (they place by
capacity, and the model used to be dropped without a word); `route: "local"` and a name this box serves never read the fleet.
Every routed `agent_run` now carries a flat `{answer}` `output_schema`: before, the delegator refused the contract on every node
as `noschema` (register C-46), so a routed call could never be placed on a node. No node needs a change; a seat that is not a node's agent seat would need
the contract to name it, which is a wire change and is not here. Detail in [mcp-server.md](systems/mcp-server.md).

**One question before the wall: the seat coherence probe (register D-118).** A seat can be HEALTHY by every gate the
harness had and still be numerically broken. On 2026-09-16/17 the blackwell-16 vLLM seat (Qwen3.8-27B GSQ, `fp8_e5m2` KV
through FlashInfer) passed `/health`, `/v1/models`, the speed probe and the READY smoke, then answered every contract
with `<tool_call>!!!!!!!!!!!!!!!!!!!!…` to the token cap — 126–336 s of degenerate output per contract, all of it filed
as `unparsed_tool_call`, and two GPU leases spent on parser hypotheses before anyone read a raw completion. So after the
warm-up and still OUTSIDE the wall, the node asks the seat one bounded question: *read `notes.md` with the `read_file`
tool, then answer DONE*, at most 96 tokens, thinking off, the node's own sampling, over the same client the loop uses.

Cost: **one ≤ 96-token completion per cold load** under the default `cold` policy (`always` probes every run, `off`
never). What comes back is reported as `coherence_note` on the wire, in one of three shapes:

| verdict | what the seat did | what happens |
|---|---|---|
| `coherence probe: tool call parsed in Ns` | the seat decoded, the template rendered, the server parsed the call | the run proceeds |
| `coherence probe: answered in text without a tool call in Ns (proceeding)` / `… inconclusive (…); proceeding` | plain prose instead of a call, or the seat could not be reached at all | the run proceeds — the probe never turns silence into a defer |
| `coherence probe: cut inside the think block at the 96-token cap (…; proceeding)` | a thinking seat spent the probe's 96 tokens in its hidden `reasoning` / `reasoning_content` channel | the run proceeds — a think block cut by the cap is not an incoherent seat |
| `seat incoherent at warm: …` | ≥ 20 identical non-whitespace bytes in a row, an unparsed tool-call marker with no parsed call, or nothing printable (empty or whitespace-only) at the cap **and no reasoning channel reported** | the contract defers `infrastructure` after SECONDS, and `agent_delegate` re-places it on another node |
| `seat incoherent at warm: … (remembered from this seat's probe Ns ago…)` | a WARM contract on the seat this process already caught, still resident | the contract defers on the remembered verdict, spending no probe at all |

The defer is the one `infrastructure` defer the delegator retries elsewhere: the fault is a property of that seat and
it was caught before the contract's WALL started. The budget is delegator wall clock, so what the node spent getting
there — cordon, pre-flight, the cold load that triggered the probe (125–250 s for a vLLM seat) and the probe itself — is
credited back to the subtask's `timeout_sec` ledger from the `admission_wait_sec` the node reports, exactly as a
capacity wait is credited; without that credit the retry floor (the alternate seat's own `min_turn`) would refuse the
retry this defer exists for. It is still a broken stack — `--route remote --pin-reason operator` exits non-zero and an operator has to fix the
box (on that seat the fix was `kv_cache_dtype: fp8` instead of `fp8_e5m2`; see ADR 0048 Amendment 1).

Note what `cold` does and does not cover: it protects the contract that LOADS the seat, and the defer unloads nothing,
so the broken seat stays resident. The node therefore REMEMBERS a broken verdict for that endpoint+seat for 10 minutes
and defers the next warm contract on it without spending a probe. The memo lives in the node process, and it is dropped
by the seat's next cold load, by any later probe that is not broken, and by its own TTL — so a seat you fix comes back
by itself. Set `agent_coherence_probe: "always"` on a box whose seat has gone NaN under it before and you get the probe
on every run instead, at one ≤ 96-token completion per contract.

**The final budget fits the wall (0.122.1, register D-95).** Sizing told the caller a contract would not fit; it did
nothing about the run in flight, which still opened its final answer at the configured 4× budget. On the <node-c> 4B seat
(~15 tok/s) a list-heavy extraction with an `output_schema` owed an 8,192-token final PLUS an 8,192-token re-pack — the
node's own `wall_note` priced that at 1,166–1,310 s against a 900 s wall, and on 2026-09-14 `METHODOLOGY.md` and
`SELF-CONTROL.md` hit the wall instead of answering (at the 4,096 budget the same contracts were CUT instead: three of
ten deferred `output_truncated`). The same arithmetic now runs backwards, before the answer is asked for:

```
fit   = (remaining_wall − other − safety) × tok_s / turns
final = min(configured final, fit), floored at 1,024, never raised above the configured cap
```

`turns` is **2** when the contract carries an `output_schema` (the final answer and its structured re-pack are both
decoded on this seat inside this wall) and 1 otherwise; `other` is everything the run still owes that is not the answer
— the estimate's cold load + think block + tool steps (`wall_estimate_sec`'s own non-final terms) at run start, and **0**
at the forced final step, where those are already spent; `safety` is a tenth of the remaining wall, never less than one
transcript prefill (6 s). It is computed at run start and again at the forced final step, off the LIVE clock. Worked
example, the METHODOLOGY case: a 900 s wall, 15 tok/s, a schema, 12 steps, 2,048-token steps — `other` = 331 s (34 s cold
load + 137 s think block + 160 s tool steps), safety 90 s, so `fit = (900 − 331 − 90) × 15 / 2 = 3,592 tokens`, and the run
then owes 331 + 2×239 = **810 s of its 900 s wall** instead of 1,423 s. Results publish `final_budget_fit` and
`budget_note` ("final 8192 → 3592 to fit 900 s at 15.0 tok/s (split with the output_schema re-pack)") on the node and the
delegator wires, and on the local `agent_run` door (register D-134), whose loop runs under a deadline that IS the wall, so the fit
at the forced final step is the one that bites there (the node's loop runs under the liveness ceiling, ADR 0055). A seat with **no measured rate**, or a wall with room, publishes neither and runs byte-for-byte as
before — the fit is a ceiling, never a raise. When even the floor does not fit, the floor stands and the note says `the
wall will be the stop`, which is today's behaviour, named.

**A cut final on a schema contract is re-issued once with list caps (0.122.1, register D-95).** 0.115.23 (D-91) refuses
to re-pack a `length`-cut final — a partial cannot be re-packed into the requested object — but abstaining there threw
away a run that had read the whole document and only over-answered: nothing had told the seat how long its lists could
be. The loop now asks ONCE more, thinking off, at the same budget, with the caps spelled out: *cap every list at N items
(keep the most important ones and drop the rest), and keep every string under 200 characters*. **N is the smallest
`maxItems` anywhere in the contract's schema, and never more than 8** — a schema the author shaped is honoured, never
broken by the retry, so set `maxItems` on your arrays when you know the bound. Two conditions gate it, so it can never
re-create the shape it fixes: an `output_schema` is set, and the wall still holds one turn at the seat's measured rate
(`min_turn`, the re-pack term included). It is bounded at ONE: a seat that cuts the capped answer too abstains exactly
as before, with `final_reissue=list_cap` and BOTH `finish_reason`s on the record (`calls[]`, and named in
`repack_note`), so a first cut and a second are never read as the same event.

**The re-issue no longer asks the partial to be JSON-shaped (0.123.3, register D-95b).** 0.122.1 gated it on the cut
answer starting with `{` or `[`, on the reasoning that "cap every list" says nothing to a truncated narrative. On the
<node-c> 4B that excluded every run it was built for: the seat answers a schema contract in its OWN prose shape
(`summary (≤100 words):` / `mechanisms:` / `- item`), which the structured re-pack reads perfectly well, so the live
readback of `METHODOLOGY.md` on 2026-09-14 still deferred at 381 s with `output_truncated` and no re-issue at all. Any
`length`-cut final on a schema contract now earns the one re-issue; the wall gate is what bounds it.

**A repetition loop is a cut answer (0.123.3, register D-95b).** The same readback showed the second half of the
failure: the answer had degenerated into a LOOP — one four-line block under `numbers:` repeated about twenty times
until the budget ran out — and the engine reported it as an ordinary completion, so nothing downstream could tell it
from an answer that merely ran long. The loop now reads the text it was handed:

- **The rule.** Take the final's non-blank lines, normalised (trimmed, inner whitespace collapsed, case-folded). If the
  TAIL of the answer is the same block of `p` lines repeated **4 or more times in a row** (`p` from 1 to 32 lines, the
  smallest period wins, and the last block may be cut off mid-way — that is how a budget ends), the answer is a
  degenerate loop. Four is the threshold: three repeats are legitimate parallel structure, and the measured failures
  repeat twenty times or more. A long list of DISTINCT items never matches — the detector keys on repetition, not on
  length.
- **What happens.** The final is treated as CUT even when the engine said `stop`: `stop_note` carries
  `repetition loop (20× "- 100 words cap for summary (enfor…")`, `output_truncated` is set, and the run re-issues ONCE
  with the list caps PLUS *"Your previous answer got stuck REPEATING the same lines … write each list item exactly once
  and then stop."* A second loop abstains exactly as a second cut does. `calls[].finish_reason` is **not** rewritten —
  it stays what the engine reported; the guard is the loop's reading of the text, not the seat's report of itself.
- **What the caller sees.** The repeated tail is trimmed off the partial that rides in `output`: one copy of the block,
  then `[repetition trimmed ×19]`. Everything before the loop survives byte for byte.

**`agent_sampling` / `agent_sampling_final` (0.123.3, register D-95b).** The agent client has always sent
`temperature: 0` and no other sampling key — right for tool calling, and precisely the greedy decoding that makes a
small seat fall into the loop above. These two box-config objects give a MEASURED seat a different policy:
`agent_sampling` applies to planner (tool) calls on the executing seat, `agent_sampling_final` to the final answer turn
and its re-issues — the thinking-off prose turn, which is where a model family's non-thinking recommendation actually
applies. Both take `temperature`, `top_p`, `top_k`, `presence_penalty` and `repetition_penalty`, all optional; a knob
you do not set is ABSENT from the request, so the seat keeps its own default, and an absent object is today's behaviour
byte for byte. An out-of-range value fails the config LOAD, named by its key, rather than as a 400 mid-run. The
effective policy of every completion is published in `calls[].sampling` (`temperature=0` for the default), so a
measurement can prove which decoding produced which answer. **There is no house default and no recommended value
here**: Qwen3.5's own non-thinking card says temperature 0.7 / top_p 0.8 / top_k 20 / presence_penalty 1.5, and that is
a number to measure on a seat, not one to inherit fleet-wide.

### What is the harness doing on the cards? (0.117.0, ADR 0041)

"Busy" is not an answer. `local-offload gpu status` (and `offload_status.gpu_lease`) now say what the cards
are DOING: a one-word `verdict` — `working` (a request or a registered agent run is in flight on the seat),
`held-working` (a lease is held and the cards are busy under it), **`held-idle`** (a lease is held and nothing
is running: seat idle, cards quiet — the holder is draining, queued, loading, or stalled), `loaded-idle`,
`busy-outside` (no lease, cards busy with work the harness does not own), `stale-holder`, `free`, and for a held
lease that is not healthy `held-stalled` (its progress file stopped moving), `held-orphaned` (its owner has been gone
past `gpu_orphan_grace_min`), `held-overdue` (its declared window ended, its holder still heartbeats; a lease whose term was not renewed is labelled
expired and says why; with `gpu_on_overdue = request` its holder is also asked to yield when someone waits for its cards, and the note says what became of the request: see "An overdue lease's holder is asked to yield" in docs/systems/gpu-lease.md) and `tree-orphan`
(the wrapper is gone, the job it started is still running, the card stays claimed until the job ends; `gpu release --epoch N` ends the claim at once and leaves the job running; `gpu reserve --die-with-wrapper` makes a Windows wrapper's death end its job instead, except an instance another lease took over, which is spared and the wrapper says so, and it is refused when the command cannot be put in the job) — see "Who asked for a lease" and "Terms" in docs/systems/gpu-lease.md — and an
`activity` block with the seat's load state and in-flight count, every registered run (kind, pid, origin,
goal excerpt, phase, step, tokens, age), a utilization/memory sample per card with the processes on them
(a display card's processes that nvidia-smi cannot size, the Windows desktop, are one count per card in
`display_card_processes_unknown`, not dozens of rows; "A display card's desktop is one count" in docs/systems/gpu-lease.md),
and the holder's command (the wrapper form stamps its argv). Every agent loop registers itself in
`<state root>/gpu/activity/` before admission and updates the record per step, so a drain or a status reader
sees a run between its steps, when the engine's own gauge reads zero.

**The seventh clock: the drain.** `gpu reserve --drain` waits until the seat's gauge AND the registry are
empty on two consecutive reads. Its deadline is the rest of `--wait` (from when the reservation began
queueing; floor 2 min) unless `--drain-timeout` is given — a fixed two minutes failed twice on 2026-09-14
under one legitimate 27B step of 3m27s. While it drains the lease is stamped `draining` (new runs hold at
the cordon for their admission budget and defer `capacity`; in-flight runs finish their steps) and turns
`exclusive` only once idle. Progress prints on change with the seat's own turn arithmetic
("one seat turn is ≈ 174 s at 23.6 tok/s"), never a line per tick.

### Context-budget guidance (why prompt shape matters)

The planner models here have a **~32K context window** and the loop **resends the full growing
transcript every step**. A wide, exploratory `web_search`-heavy prompt accumulates search results
into that transcript and can blow the window before finishing. **Prefer "edit an existing file, then
upload it" over "search the web, build, upload."** The search leg is what overflows context on broad
topics; edit+upload alone completes reliably and fast. For anything beyond a narrow task, keep the
toolset lean (no shell/delete unless needed — each tool adds schema overhead) and the same-tool cap
low so a stuck model gets its tool disabled quickly.

**Prompting rules of thumb (read before your first prompt):**

1. **ONE bounded task per message.** "Edit index.html to add a pricing section, then upload it" — good.
   A multi-goal essay ("research X, then build Y, then also refactor Z and…") — the run dies mid-way.
2. **Never paste long documents into the agent chat.** The whole paste rides in the transcript on
   every step. To digest a big file, use the harness instead (`local-offload summarize <file>`) and
   hand the agent the summary or the file *path*.
3. **Don't chain more than ~3 tool-kinds in one ask** (search → write → upload is the practical
   ceiling on a 32K model). Split bigger jobs into sequential messages — each run starts fresh.
4. **If you see `agent error: chat 400 … context`, your prompt was too big or too broad.** Nothing is
   broken — narrow the ask and send again.
5. These are guardrails, not conventions: the loop hard-caps at `--max-steps` (12) and disables any
   tool called more than `--max-same-tool` (8) times per run, so a bad prompt costs one failed run,
   never the installation.

### Per-hardware-profile serving expectations

The installer resolves a hardware **profile** (`detect.ps1` → `install.ps1`, see
`setup/SETUP-AGENT.md`) and renders the serving template + writes the profile's `agent_ctx_tokens`
to `installed.json`. The install seed also binds the agent's planner seat automatically: config
`agent_model` comes from the profile's explicit `config_seed.agent_model` when it names one (the
measured seat for that tier), and otherwise is DERIVED from `resident_tier` when that differs from
the workhorse. So running the agent no longer requires `-model <resident_tier>` — the `-model` flag
remains an override. `agent_max_tokens` (0.113.9) is the planner's completion budget per call for `agent_run`
and for delegated jobs this node serves (0 = the loop default of 1,024) — set 4096 for a THINKING seat, whose
reasoning spends the same budget (the <node-b> 27B seat used 839 reasoning tokens of 1,024 and returned nothing,
2026-09-04). A key named `max_tokens` is NOT read: the loader warns `unknown config key`. `agent_thinking`
(0.115.8) is the seat's think-block policy on planner calls: `auto` (default) thinks every step and, when a
step ends EMPTY (no content, no tool call — a think block that ate the budget, or a bare close), re-issues that
same step ONCE with thinking off at 4× the step budget (cap 8,192), then stops the run as `reasoning_starved` /
`empty`, which the node reports as a defer with the arithmetic in `stop_note` and per-call `calls[]` — never as an
empty answer; `off` renders every planner call in non-thinking mode (`chat_template_kwargs: {"enable_thinking":
false}`, the same knob the structured re-pack sends) for grounded extraction on a seat measured to starve; `on`
never sends the kwarg. A contract's own `thinking` overrides the box. `agent_sampling` and `agent_sampling_final`
(0.123.3) are this seat's decoding policy — planner calls and the final answer turn respectively — each an object of
optional `temperature` / `top_p` / `top_k` / `presence_penalty` / `repetition_penalty`; absent or empty means the
historical request (`temperature: 0`, no other key), an unset knob is absent from the body so the seat keeps its own
default, an out-of-range value fails the load by key, and the effective policy of each completion is published in
`calls[].sampling`. They carry no house default on purpose: a sampling setting is a per-seat measurement (see "The
timeout chain"). Until 0.115.8 the loop raised the budget
4× on a starved step, nudged once with a user turn and then accepted a second empty as `done` (1× + 4× + 4× the
budget for nothing; 2026-09-10 retrospective D-01). `agent_retry_min_sec` (0.115.9) is the least `timeout_sec` budget a subtask's cross-seat verification retry is
worth starting with (0 = the historical 10 s): set it to a cold load plus one turn at `max_tokens` on the retry
seat — 300 on the reference box — so a 296 s leftover no longer buys a thinking seat four minutes of think that
times out; the retry is also skipped after an empty final and never lands on a seat already running another
job (`retry_note` names each). `agent_lease_wait_sec` (0.113.14) bounds how long a local `agent_delegate` placement waits for a
foreign TEXT-class GPU lease (`gpu reserve --class text`) to clear before deferring — see the delegate section below. `agent_placement_wait_sec` (0.113.18) is the delegator's CAPACITY wait for a call that has no whole-call deadline (the `delegate` CLI verb, `agent_run`, `offload_ask` with a route): a subtask every fitting node refused for capacity (queue full, leased, draining, shed), or whose only placement is a reserved seat, waits up to this many seconds (default 120; negative = off, for every call) re-reading the fleet's health and lands on the first node that frees — the idle time is not charged to `timeout_sec`; when nothing frees it defers with class `capacity`. A call that HAS a deadline (the MCP doors `agent_delegate` and `offload_research`, `agent_call_deadline_sec` below) waits until that deadline less a 15 s reserve instead, whatever this key says (ADR 0073): band-0 work does not defer for capacity while the call still has time to place it (a subtask that holds one of the call's run slots while others have not started holds it for the larger of this key and `agent_lease_wait_sec`, then gives the slot up and keeps waiting to the deadline, so a saturated fleet cannot hold every slot for the whole call and no subtask is refused while the call has time; it takes a slot again only to dispatch), and nothing is started or dispatched inside the reserve, where a job could only be cut. To shorten the wait of those doors, shorten `agent_call_deadline_sec`; the deferral then says `bounded by the call's deadline, not by agent_placement_wait_sec`. Since ADR 0063 the same wait holds a subtask the local seat's run cap deferred, a retry whose seat is busy, and any dispatch this process's per-node gate turned away (it never holds more jobs open on a node than the node's `max_queue_depth`), and it skips a node inside its `Retry-After` cooldown (unless a health read taken after the refusal proves a free worker: ADR 0073) or whose backlog outlasts the contract's patience (which, under a call deadline, is clamped to the time the call has left). `priority` on `agent_delegate` / `--priority` on `delegate` (`-1` sheddable, `0`, `1`) is the scheduling band a node claims by; sheddable work takes idle capacity only and is shed rather than waited (docs/systems/fleet-node.md, "Bands, tenants, saturation and the capacity wait"). `agent_call_deadline_sec` (ADR 0065) is the WHOLE-CALL deadline of the MCP doors `agent_delegate` and `offload_research`: the MCP client aborts a tool call at its own limit (1,800 s in the reference setup) and drops the response with it, so at this many seconds from the call's arrival (default 1,500; 0 = the default; negative = none) the call returns the finished subtasks' results and defers every unfinished one as a budget-class "call deadline reached; N unfinished" (the outstanding work is cancelled, nothing further starts, and a remote job the node had already started keeps running there and stays open for the recovery pass). It is not a per-subtask wall (`timeout_sec` is) and the CLI verbs take none; keep it below the client's own abort with room for the answer to travel (a value at or above the abort, or a negative that was meant as a number, is a `doctor` finding and a startup warning; a subtask that first waits queued on a node or for capacity can outrun the default and be cut, so raise it for such work). `agent_spread_local_slot` (0.113.20) decides what `route=spread` does with the LOCAL rotation slot when the local seat is already busy at deal time: `skip-when-busy` (the default) deals it to the best-fit eligible remote with room, so several delegating sessions do not stack on one local seat; `always` restores the unconditional local slot (docs/systems/fleet-node.md, "The local slot under load"). A tier may also seed `agent_profile`, the box's DEFAULT agent tool profile when
a call names none (resolution: explicit `--profile`/argument > config `agent_profile` > `general`).
`ampere-6` seeds `research` because on that tier the same model scored 0% under `general` and 72%
narrowed — the profile outweighed the choice of model. `--two-tier` ignores the box default, since
it sets its own architect/editor toolsets. A seeded tier is the normal case wherever a bake-off has run: an explicit seed
is how a tier binds a planner that is neither its resident nor its workhorse. Run with `-ctx-tokens <agent_ctx_tokens>` matching the profile. These are **projected
defaults**; `selftest.ps1` measures on the real box and its `receipt.profile_measure.tuned` block
carries any measured override to apply.

| Profile | Resident/default tier | Served ctx (`-ctx-tokens`) | KV | 26B-A4B |
|---|---|---|---|---|
| `blackwell-16` / `volta-16` | `gemma4-26b-a4b` | 32768 | q8_0 | full-GPU resident |
| `blackwell-2x16` | `gemma4-26b-a4b` | 131072 | q8_0 | full-GPU resident; the served window follows the measurement (0.113.29, 32768 → 131072). Bound only for EXACTLY two Blackwell cards in the 12–23 GB band; three cards are `blackwell-3x16` (shipped 0.113.32) |
| `blackwell-3x16` | `gemma4-26b-a4b` (single layer) + the vLLM agent seat (pair layer) | 131072 (long twin 262144) | q8_0 | COMPOSITE (ADR 0052): a complete `blackwell-16` AND `blackwell-2x16` as well as itself. Placement decides per task which layer and seat run the work and records it as `placed`; the display card (device 1) is fenced by a ≥4 GiB floor, a host-RAM guard and an operator-presence guard, all failing closed, and its layer ships dormant. See [systems/composite-tier.md](systems/composite-tier.md) |
| `ampere-16` | `offload-e4b` (agent `qwen38-27b-agent` = Qwen3.8-27B UD-IQ3_S + embedded MTP head, `--ctx-size 49152`, `research`; 26B dropped) | 49152 | q8_0 | **SEAT RE-AUDITED 2026-09-16 (ADR 0047): the 4B is out.** Blind, 24 Opus judgements, 3 lenses, on a card at its accepted 40 W profile and cooled before every arm: Qwen3.8-27B UD-IQ3_S+MTP 9.32 beats the previous seat `qwen3.5-4b-vllm` (5.39) **24/24**, gemma-4-12B+MTP (6.17) 24/24 and gpt-oss-20b+EAGLE-3 (6.21) 24/24; the 4B took zero firsts and fourteen lasts. The 2026-09-14 "measured tie" is withdrawn — it ran on a stock, thermally throttled card (A-103) at a 1,024-token step budget with the 12B capped at 32k. Costs that ship with it: it pads (102 degenerate findings flagged) and decodes at 6.3 tok/s, so the tier seeds `agent_max_tokens` 4096 and `agent_timeout_sec` 900. The llama.cpp 4B entry stays as the fallback. Was: measured 2026-09-04 on an NVIDIA A2 16 GB at 40 W: the projected 26B-A4B seat ran 1/8 digests, the 4B seat 8/8 (0.113.15); 2026-09-06 the reference box's agent lane moved to a vLLM seat behind llama-swap (131,072 ctx). RENDERED by the installer since 0.113.33 and NOT persistent since 0.113.36 (`ttl: 300`, no preload, no boot-enable). Seat choice re-grounded on QUALITY 2026-09-08 (blind 4-way, 24 judgements: 4B-vLLM 7.75 > 4B-llama.cpp 7.38 > 12B-vLLM 6.50 > 12B-llama.cpp 6.21), not on wall time: ADR 0035, `setup/templates/vllm-seat/linux-systemd/` |
| `dual-gpu` | `gemma4-26b-a4b` (architect) + `offload-e4b` (editor), both resident | 32768 | q8_0 | resident (two-tier, **zero swap**) |
| `ampere-8` / `blackwell-8` | `offload-e4b` | 16384 | q8_0 | dropped (RAM is overflow only: no every-expert `--cpu-moe`) |
| `amd-rdna3` | `offload-e4b` (Vulkan) | 16384 (floor; canary → 32768) | f16 (floor; canary → q8_0) | full-offload `-ngl 99` on the unified-memory iGPU (~20–25 t/s on dual-channel DDR5) — see SETUP-AGENT.md, AMD RDNA3 chapter |
| `amd-rdna3-dgpu` | `gemma4-26b-a4b` (Vulkan, discrete RX 7900-class ≥12 GB) | 32768 | q8_0 | full-GPU `-ngl 99` resident |
| `ampere-6` | `offload-e4b` (agent seat `qwen3.5-4b-agent`; `qwen3.6-35b-a3b-agent`, a RAM-spill MoE, on a box with 28 GB or more of RAM, ADR 0080) | 32768 | q8_0 (conservative default; f16 measured viable) | dropped (architectural — see the tier page) |
| `maxwell-3` | `gemma4-e2b` (the E4B is omitted: 3,100 MiB at 16K does not fit a 3 GB card; no agent seat bound, the lane falls back to the E2B until the `<node-g>` bake) | 32768 | f16, flash-attn on (Maxwell has no fp16 math; the f32 flash-attn kernels measured faster) | dropped; the 12.4 prebuilt serves through its sm_50 PTX (one-time JIT, cached under `CUDA_CACHE_PATH`) |
| `amd-gcn` | `offload-e4b` (Vulkan; the CPU alt route was withdrawn 2026-09-24 — no model runs on CPU, ADR 0054 amendment; agent seat `mimo-9b-agent`, `qwen3.5-4b-agent` as the un-aliased rollback) | 32768 (8192 → 32768 measured 2026-09-20: 24k-token prompt in 278 s, 3.9 GiB GTT) | f16, flash-attn on (measured 2026-09-20 on <node-f>: +4 % pp, neutral on Lucienne; the lane is DDR-bandwidth-bound, every RADV/ubatch/KV knob within ±4 %) | dropped |
| `cpu` | `offload-e4b` (CPU) | 8192 | f16, flash-attn off | kept when RAM ≥ ~56 GB, else dropped (no card to overflow from: the one tier exempt from the every-expert rule) |
| `rockchip-rk3588` | `qwen3.5-2b-npu` (Qwen3.5-2B W8A8 on the NPU via RKLLM, chat and vision: `vqa` and `ocr`, never `assess_image`; its own pipeline runs `classify` and `extract` with no grammar, from a prompt plus strict validation, and the fleet text lane is not advertised until measured); no llama.cpp entry (llama.cpp Vulkan faults the Mali GPU on this kernel/Mesa, measured), no model runs on the CPU, and `uma_reserve_gib` 3 holds RAM back for the host | the NPU seat 16384 | n/a (RKLLM) | dropped |

Notes: q8_0 KV keeps the KV cache ~half the size (V-quant needs flash-attn on, which the CUDA/Vulkan
templates set); the 26B is placed full-GPU only on ≥12 GB single-card profiles and dropped where it
does not fit: RAM is overflow only, so the installer never writes the every-expert `--cpu-moe` for a box
with a card (the only expert placement in RAM it may write is a measured partial `--n-cpu-moe N`
within the tier's `n_cpu_moe_max`). A box that is not on the tier matrix gets no 26B on the CUDA family.
On the dual-GPU profile the two models sit on separate cards so `--two-tier` costs no swap.
Anything *italic/projected* is refined by the install-time measurement — trust the selftest receipt
over the projected table when they differ.

| Failure | Fix |
|---|---|
| `refusing to bind --listen` | You passed a non-loopback address. Use `127.0.0.1`, or `--listen-trusted-network` (only if authorized). |
| agent stops with `stop=step-cap` / loops | Raise `--max-steps`, or the model is stuck — lower `--max-same-tool`, narrow the prompt (edit+upload shape). |
| GitHub tool refuses | `$GITHUB_TOKEN` unset or under-scoped, or `$GITHUB_REPO` unset. See §6. |

### Parallel sessions on one llama-swap (0.111.0)

Several Claude Code sessions each run their own `local-offload mcp` process and fan out
against the SAME llama-swap (a call is as wide as its deal, [ADR 0076](architecture/decisions/0076-a-calls-width-is-sized-from-its-deal.md),
but its local legs are held to the seat's own run-cap line, `fleet_max_concurrent_jobs`, 4 by default and 1 on a single-slot tier). llama-swap answers **429** once a model's
reserved requests (queued + in-flight) reach its `concurrencyLimit` (default **10**), queues
requests *silently* while it swaps a model, answers **503 "process is not ready"** while a process
is starting, and **500** with `src:"llama-swap"` when a health check times out. While the seat is
serving, the harness treats all of those as *peers hold the seat* ([ADR 0032](architecture/decisions/0032-a-peer-held-seat-is-waited-for-not-deferred.md)).
While the seat is NOT serving (llama-swap lists it starting or stopping, does not list it, or its engine refuses
connections) a 5xx is the seat's own failure and is not waited on as contention, by the loop or by the structured
re-pack (ADR 0066); a 429 is contention by definition and is never second-guessed:

| key (`config.json`) | default | meaning |
|---|---|---|
| `seat_contention_wait_sec` | `0` → 90 s | one wait budget per agent contract, shared by every chat step and the re-pack; `-1` = never wait (first busy answer defers) |
| `agent_admission_wait_sec` | `0` → 300 s | pre-flight: wait while any model on the endpoint is mid-swap, then WARM the seat if it is not loaded (0.115.11: one passthrough GET makes llama-swap swap it in; a vLLM cold load is 125–250 s) — all BEFORE the contract's wall starts; `admission_wait_sec` / `admission_note` on the wire report it; `-1` = off |
| `vision_coldload_wait_sec` | `180` (`0` = off) | the vision door's own pre-send WARM-UP (F49): a vision call (`vqa`, `ocr`, `assess_image`, `video_describe`, `video_watch`) whose seat llama-swap does not list ready sends one passthrough request that loads it and holds for the load, up to this many seconds, and only then sends the vision request, which keeps its full `request_timeout_sec` for inference. Before this the first call after the seat's idle unload carried the load inside that timeout and failed with `Client.Timeout exceeded while awaiting headers`. A bound, not a promise: a load that outlasts it proceeds into the call. 180 is chosen (the first chat completion logged after each of the last 25 cold starts took 14–84 s, load plus its own inference, in the 21 that completed; one load outlasted 120 s) |
| `agent_warm_failure_defer` | `false` | ENFORCEMENT of a warm-up start failure (register C-76, R-05a; audit mode by default). When the warm request is refused with a 5xx and the evidence says the seat's process did not start — the answer is not one of llama-swap's busy shapes, `/running` lists no row for the seat and nothing else is mid-swap, `/running` was readable — `false` proceeds into the wall and `admission_note` says the run **would have deferred at once**; `true` defers at once as `seat warm-up failed: …` (infrastructure) and the delegator retries the contract once on another node. Count the would-be defers in `admission_note` (on every published result and in the delegation log) before turning it on |
| `audit_chain` | `false` | HASH-CHAIN each agent run's rows on the broker audit trail (register SF-08): `run_id`, `seq`, `prev_sha256` on every row and a closing `run_end` row (a run with no decision writes nothing). `local-offload agent-audit verify [--file PATH] [--json] [--strict]` exits 1 naming the run and seq of an edited, removed or reordered row inside a run; an OPEN run (no `run_end`: a crash or a cut tail) is reported, and failed with `--strict`; LATE rows (chained, after the close) are reported. Unkeyed: it does not see a whole run removed or a re-chained edit; an off-node head witness (S-08) is not built |
| `agent_evidence_reducer` | `false` | the EVIDENCE-PRESERVING REDUCER for the agent's build and test results. `false` leaves every tool result exactly as the tool printed it. `true`: a `run` or `run_shell` result that is a build or test invocation (`go test`/`build`/`vet`, `cargo build`/`test`/`check`, `npm test`/`run build`/`run test`, `pytest`, `python -m pytest`/`unittest`, and `make` through `run_shell`: the same producer gate as the log-collapse rung) and is 4 KiB to 64 KiB goes to the cascade seat the agent's own offload tools already use, which writes up to 5 claims, each over a one-line quote; the delegator then looks every quote up in the output (the research digest's quote check) and the transcript keeps a receipt in place of the log: a disclosure line, the tool's `exit=` line copied verbatim, then each claim over the output's own span. Anything else keeps the raw output unchanged: a quote that is not in the output, a seat error or defer, an answer that is not the requested JSON, a receipt that is not strictly smaller, output that looks like a credential or carries an email address (checked before the seat is shown anything), reads, searches, listings and any command not read as a build or test. A call the model repeats runs once more and that result is not reduced. Cost: one seat call (bounded at 60 s) on the loop's critical path per qualifying result. Stays `false` until a seat A/B on the agent seats passes (non-inferiority on tool-call reliability and pass rate, forwarded prefill tokens as the efficiency metric). No flag; `local-agent` reads it from the config like the other agent keys |
| `agent_read_floor` | `""` → `warn` | the READ FLOOR on every agent tool that reads file content (register SF-07): `read_file`, `summarize_file`, `search_files`, `edit_file`, `github_upload_file`. Secret material (`.env*` except `.env.example`/`.env.sample`, keys and certificates, `.npmrc`/`.netrc`/`.pypirc`, `.git-credentials`, `.claude.json`, anything under `.ssh`/`.aws`/`.gnupg`/`.kube`/`.env`) and a rule table's `read` rules, checked as given AND resolved (symlinks, Windows 8.3 short names). `warn` = the read goes through and a `warn` row is recorded (refused when an enforcing trail cannot record it); `enforce` = refused, and `search_files` never opens such a file and says nothing about it; `off` = nothing checked. `--rules off` also turns it off. Not covered: the run/shell cage, hard links, media tools reading absolute paths |
| `audit_all_doors` | `""` → `off` | the broker decision AUDIT TRAIL on every agent door (register SF-02): `off` = a trail only when browse asks for one (unchanged); `warn` = `agent_run`, `agent_delegate` and fleet contracts all write `~/.local-offload/agent-audit.jsonl`, a failed write is logged once and never changes a decision; `enforce` = a failed write denies the action and a door with no resolvable trail defers `config`. An unknown value refuses the config load (and a verb that runs on past the load error refuses the door). Rows come only from brokered actions (write, delete, fetch, shell, run, browse), so a read-only run attaches the trail and writes no broker rows (read-floor rows aside, see `agent_read_floor`). Move to `enforce` only after a clean window under `warn` |
| `fleet_ingress_guard` | `""` → `warn` | the INGRESS GUARD on the fleet node, `fleet-ui` and `local-agent --serve` (register SF-16): a `Host` outside loopback, the tailnet CGNAT range, a dotless MagicDNS name and every zone listed in `tailnet_suffix` / `tailnet_suffixes` (the rule a `seat_endpoints` URL takes; a node shared in from another tailnet is named under that tailnet's zone, so list it in `tailnet_suffixes` before you set `enforce`), any `Origin` header (`fleet-ui`: other than its own page's), and (fleet node, local-agent) a POST that is not `application/json`. `warn` logs what `enforce` would refuse and serves it (bounded: each offender once per 5 minute window, at most 64 per peer and 4096 in all, values clipped, a summary line a minute for the rest), `enforce` refuses (421 / 403 / 415; the body does not name your tailnet zones), `off` skips; `fleet-serve`, `fleet-ui` and `doctor` print `ingress guard: <mode>`, and `local-agent --serve` prints the mode in its banner; a mistyped mode fails the load, and `fleet-ui` and `local-agent --serve` refuse a config that failed to load |
| `agent_coherence_probe` | `""` → `cold` | post-warm SEAT COHERENCE probe (register D-118): after a cold load, ask the seat one ≤ 96-token question BEFORE the wall starts and defer `infrastructure` if it answers with the NaN shape. `cold` = only when this run loaded the seat, `always` = every run warm or cold, `off` = never. `coherence_note` on the wire reports the verdict |
| `gpu_hostram_hold_max_sec` | unset → `600` (`0` = no bound) | the HOLD BOUND of a `gpu reserve --unload-seat` lease (F82, 2026-10-14 incident): the most it may hold its cards, counted from the unload, while it waits for the host to have the RAM it declared. Past it the lease is released (the seat warmed back only if the lease owes it), nothing runs, and the reserve exits non-zero with one line (the need, the figures of the condition that refused and the headroom, how long the cards were held, what to do). A `--detach` hold is released the same way, and also when `--wait` ends the wait first; if the reserve dies (mid-wait, or earlier, during the drain or the unload: the live reserve keeps now plus this bound plus 60 s on the record, re-stamped every 30 s), the hidden holder releases the lease itself 30 s after the stamped moment. With the bound at 0 the outer stamp is the `--wait` deadline (a dead parent's lease ends then). `gpu reserve --ram-hold-max <duration>` overrides it for one reserve; `0` (key or flag) is a value: no bound, only `--wait` ends the wait. A negative value, or one above 7 days (604800 s, a chosen ceiling that also keeps the conversion from wrapping), is refused by name. 600 is a chosen figure, not a measurement. `gpu status` shows how long a waiting lease has held its cards and, once the wait for the host has begun, when it is released if the host still lacks the room (during the drain and the unload it says the seat is cleared first and the host asked after, and when the holder releases the lease if the reserve dies; the lease row's `host_ram_hold_phase` is `outer`, `wait` or `release`); a release that fails prints `LEASE NOT RELEASED (epoch N)` and the command that frees it (`gpu release --epoch N`, with `--warm-seat` only when a warm-back is owed), and the error the command returns ends with that failure; `gpu release` no longer says `no lease is held` over a lease that is held |
| `tools_schema_profile` | `"full"` | how much prose the MCP `tools/list` carries. `full` (the default, and what an unset key means) advertises every description as written. `compact` keeps every tool and every parameter name, type, enum, `required` entry and bound, so any call valid under `full` is valid under `compact`, and cuts each description (a tool's and each parameter's) to its opening sentence plus the sentences a reviewed per-tool keep-list names (prohibitions, trust and privacy boundaries, cost rules, the "if it cannot, it returns deferred:true and you do the work yourself" protocol of every lane, the acceptance-authoring rules, how to read `offload_status`, warnings that a result is not a verdict, the result-reading sentences that stop a misreading such as a partial result being a successful call), and drops examples. A test reads every sentence of every description, exactly as `tools/list` emits them (each accelerator alone too, as a local and as a fleet device), and fails for any sentence outside the opening ones that is neither on the keep-list nor on the reviewed dropped list (one row per sentence, with a reason class), whatever words the sentence holds: a rule reworded as "Always ...", "You should ..." or "Make sure ..." is caught as much as one with "never" in it. A dropped sentence that carries a rule word (`never`, `only`, `must`, `should`, `always`, `avoid`, `defer`, `forwarded`, `travels`, `sent`, `cloud`, ...) needs a reason of its own on its row; the acceptance-authoring sentences cannot be dropped at all; and an enum value the full list explains (in the parameter's description or the tool's) is explained in the compact list unless it is on a short reviewed list. The forwarding note of a fleet accelerator is on the keep-list. Meant for a client that sends every tool schema up front on every call and has no deferred tool search (opencode), where the advertised list is prompt tokens on every turn; the measured table is in [opencode-integration.md](systems/opencode-integration.md). Only the advertised list changes: the registered schema, the handlers and the call-time checks are untouched. A value other than `full` or `compact` fails the config load by name. The profile is read once, when the MCP server starts (the rewrite is a middleware installed at that moment): changing it takes an MCP server restart or a client reconnect |
| `delegate_remote_classes` | unset → every node `private-ok` | the DATA CLASS of a fleet node, as an object keyed by the node's base URL exactly as `delegate_remotes` lists it (register SF-40): `{"http://node-x:18811": "public"}`. A node that is not listed is `private-ok` and may be placed any contract, as before. A node declared `public` is one whose admin is not you (a node shared into your tailnet by another account), and it is placed **public-data work only**: the page digests `offload_research` and `research` build from the public web, and the `fleet-smoke` probe. Every other contract is ineligible for it on every route (`auto`, `spread`, `remote`, a retry, a replacement), whatever its capacity: a context document, a diff, a prompt or a goal typed into `agent_delegate` is readable by that admin, so a contract is private unless the door that built it is one of the public ones (the door is stamped by the surface after intake, so a caller cannot claim it). The exclusion is recorded in `results[].placement` as `<node>: private (...)`; a call pinned `route: "remote"` with only public nodes eligible defers, `defer_class: contract`, with a reason that names `delegate_remote_classes` and the two ways out (route it local, or list a `private-ok` node). `route: "queue"` is checked at `fleet_queue_holder` only. Values are `"public"` or `"private-ok"`; any other value, and any key that is not a `delegate_remotes` entry (case and a trailing slash ignored), fails the load by name, because a typo would otherwise read as `private-ok` and send private work out. Read once per process, like `delegate_remotes`: restart the MCP server after changing it. |

What you will see on the wire and in the ledger: `contention_wait_sec` and `admission_wait_sec`
on every agent result; a defer whose reason starts with **`seat contended:`** when a 429 outlived the budget
(`grep "seat contended:" ~/.local-offload/ledger.jsonl`); **`seat not serving:`** when a 5xx did (llama-swap failing
to serve the seat: `concurrencyLimit` is not the knob), and **`seat down:`** when the seat's engine hung or died
under the run and did not come back (ADR 0066); `summary.batches` on research
calls that ran more than 8 pages; `summary.quarantined` when a fleet node was blocked for 30 min
after two off-document answers that outnumber its passes of the same window (0.184.0: the fingerprint is lexical and
misfires on an honest answer now and then, so a node that passed most of its work is not quarantined by two misses; its
reason appears among `probe_errors` as "quarantined until …"). Only a FAILED fingerprint can be excused (by an honest
absence). An unknown fingerprint that the contract caused (a grounded check with no context documents, a field check the
`output_schema` does not declare) or that sits over an honest-absence digest is reported as a failed check but strikes
nothing, passes nothing and is not retried on the research door. Apart from those two cases, an unknown the result caused (an empty answer beside the
documents, a declared field missing from the result) strikes like a failure.

**Capacity is your step — the wait only makes contention honest.** The 429 exists because ten
reserved requests per model are shared by every session. Ready to paste into
`C:\llama-swap\llama-swap.yaml` (<node-b>; back it up first), on the seats the harness binds
(`agent_model` qwen3.8-27b; cascade gemma-4-e2b / e4b / 12b / 26b):

```yaml
  qwen3.8-27b:
    concurrencyLimit: 32   # was the implicit default 10; queues in llama-swap instead of 429
  gemma-4-26b:
    concurrencyLimit: 32
  gemma-4-e4b:
    concurrencyLimit: 32
```

Then the elevated restart (the `llama-swap` scheduled task is elevated; an unelevated stop is
denied while the OLD process keeps answering 200):

```powershell
Start-Process powershell -Verb RunAs -WindowStyle Hidden -ArgumentList '-NoProfile','-Command',
  'Stop-ScheduledTask llama-swap; Stop-Process -Name llama-swap -Force -ErrorAction SilentlyContinue; Start-ScheduledTask llama-swap'
# proof of apply: C:\llama-swap\llama-swap.log shows a fresh startup banner, then
Invoke-RestMethod http://127.0.0.1:11436/running
```

Raising `--parallel` on the 27B seat (4 today; the 2026-09-01 fairness arms measured ~110–120 tok/s
aggregate at 8 slots on the 5060 Ti pair) adds real throughput rather than queue depth — that is a
seat change and stays your call.

**Deploying this build:** `go build -o bin\local-offload.exe .` in the repo, then restart the
Claude Code sessions (each one spawns its own `local-offload mcp`; a running session keeps the
old binary until it restarts).

### Cache server — an optional second device holding evicted KV (0.112.0)

A vLLM seat's usable KV in VRAM is small on consumer cards (and on a Mamba-hybrid model the
"GPU KV cache size" banner overstates it: align-mode checkpoint blocks share the pool). Contexts
that leave VRAM are recomputed. With LMCache MP and a **cache server** — a store on a second
machine's RAM — they come back at parity cost instead (measured 2026-09-02, Qwen3.8-27B, 24k
tokens: 3.86 s from <node-c> over the LAN vs 24.68 s recompute, every token from the store; 20.6 s
when the store namespace was shared across layouts), the GPU
stays free while the load streams, and they survive a seat swap. Same-box RAM as the tier is faster
(0.50 s, 49.7×) but spends the serving PC's memory; the second device is the capacity route.

**Off by default; nothing depends on it.** `kv_cache_server` is a LIST of per-seat bindings, one per
vLLM seat, and `vllm_seats` is this box's vLLM roster (0.121.0 — ADR 0045). Declare both only when a
second device exists:

```json
"vllm_seats": ["qwen3.8-27b-vllm", "qwen3.8-27b-vllm-3card"],
"kv_cache_server": [
  {
    "enabled": true,
    "store": "fs_native",
    "address": "/mnt/kvcache/lmcache-seat-tp2-fp8",
    "l1_staging_gb": 8,
    "chunk_size": 1568,
    "key_prefix": "seat-tp2-fp8",
    "seat": "qwen3.8-27b-vllm",
    "kv_dtype": "fp8",
    "tensor_parallel": 2,
    "status_file": "//wsl.localhost/<distro><seat-dir>/seat-l2-qwen3.8-27b-vllm.status"
  },
  {
    "enabled": true,
    "store": "fs_native",
    "address": "/mnt/kvcache/lmcache-seat-3card-fp8",
    "l1_staging_gb": 16,
    "chunk_size": 1568,
    "key_prefix": "seat-3card-fp8",
    "seat": "qwen3.8-27b-vllm-3card",
    "kv_dtype": "fp8",
    "status_file": "//wsl.localhost/<distro><seat-dir>/seat-l2-qwen3.8-27b-vllm-3card.status"
  }
]
```

- **One binding per seat.** A binding whose `seat` is empty is the BOX DEFAULT and backs every vLLM
  seat that has no binding of its own; an exact seat match always wins over it. Two bindings for one
  seat — or two box defaults — are refused at load: that is two stores whose order in the file
  decides which one the seat gets, not a merge.
- **The pre-0.121 single object still loads**, as a one-element list bound to its own `seat`. No
  deployed config has to change. The list is the shape to write from now on.
- **`vllm_seats` is declared, not sniffed.** `/v1/models` reports model ids, not engines, and the
  cascade deliberately stays on llama.cpp (Gemma-4 hybrids crash LMCache's V2 path, upstream #4263),
  so a sniffed roster would fail every seat that must never have a store. Empty = this box runs no
  vLLM seat and the gate below is inert.
- **`local-offload doctor` FAILS a vLLM seat with no binding**, one line per seat and a non-zero
  exit — the operator rule is that the store backs every vLLM seat while the second device is online:

  ```
  cache server (one binding per vLLM seat; the store backs every seat while the second device is online):
    qwen3.8-27b-vllm:        OK    fs_native /mnt/kvcache/lmcache-seat-tp2-fp8 key_prefix=seat-tp2-fp8 l1=8GB
    qwen3.8-27b-vllm-3card:  FAIL  no kv_cache_server binding — give this seat a store, or opt out explicitly with {"seat":"qwen3.8-27b-vllm-3card","storeless":true,"reason":"<why>"}
  ```

  An explicit `"storeless": true` with a `reason` PASSES. Silence does not — neither an absent
  binding nor one merely switched off with nothing saying why.
- **`kv_dtype` + `tensor_parallel` declare the stack generation.** They are needed only when two
  seats SHARE a `key_prefix`: the load refuses a prefix shared across different generations, and
  refuses a shared prefix where any binding leaves the generation undeclared (it cannot be shown
  safe). Give each seat its own prefix and neither field is required.
- **`status_file` is the fs_native readback (0.129.1, register B-29).** An `fs_native` store is a mounted path with
  no port to dial, so `offload_status` cannot probe it — but the seat wrapper decides the fact at every seat start
  (mount + 64 MiB write probe) and writes the file `SEAT_L2_STATUS_FILE` names (`ok <stamp> mbps=<n>` or
  `degraded <stamp> reason=<why>`): `seat-l2-<seat id>.status` in a rendered seat env, `$WORK/seat-l2.status`
  only when the env sets none. A binding that still names `seat-l2.status` must be repointed after a re-render.
  Declare that file on the binding (on a WSL2 seat, the host-visible `//wsl.localhost/<distro>/…`
  path) and status publishes `reachable` true/false with `status_line` and `status_age_s`; undeclared or not yet
  written stays `null` with a note saying which.
- **Rendering a seat from the box's bindings:** `local-offload install vllm-seat --config
  <config.json> …` picks the binding FOR THAT SEAT by name and renders its adapter, directory,
  namespace, L1 size and chunk into `seat.env`; the tier keeps the mount point, the write floor, the
  prune target, the writer count and the cap, which are properties of the share rather than the
  namespace.
- **Rendering a Windows/WSL seat (`launch: windows-wsl`: the blackwell-2x16 and blackwell-3x16 tiers):** the command
  takes its templates from the seat's launch, so these tiers render `<seat id>.env` (plus `templates/<name>` when the
  tier names a shipped chat template) and the four Windows scripts (`seat-cmd.ps1`, `seat-cmdstop.ps1`, `hidden.vbs`,
  `register-seat-tasks.ps1`), and never a systemd unit or a polkit rule. The tier states the operating point; the box
  supplies its own half as flags, none of them hardened into the tier:

  | flag | meaning |
  |---|---|
  | `--distro` | the WSL distribution the engine runs in (required) |
  | `--venv`, `--hf-home` | the vLLM virtualenv and the HF cache, required, as paths INSIDE the distro (`/opt/seat/vllm-env`, `/hf`); the defaults hang off `--home` and `$HF_HOME`, which are Windows paths here, so they are refused. `--venv` is written to `SEAT_VENV`, so the seat runs the venv the check looked in |
  | `--model-path` | optional: pin the exact HF snapshot directory, a distro path (a Windows path is refused). Without it the one snapshot under the tier's `model_repo` is used, and two snapshots are an error. Either way the snapshot is written to `SEAT_MODEL`, so the seat serves the weights the check resolved and not whichever revision `seat_fg.sh`'s default repo id would pick |
  | `--wsl-seat-dir` | the seat directory inside the distro (default `/opt/seat`) |
  | `--lmcache-overlay` | the patched LMCache directory, a distro path; required for an fp8 KV seat with a cache server, because stock LMCache restores fp8 pages corrupt (upstream PR #4253, merged upstream on 2026-09-28 and contained in the v0.5.6rc5 release-candidate tags as read on 2026-10-19; the flag stays required until the seat runs an LMCache build that contains the merge). windows-wsl only: a seat that starts through systemd has no LMCache wiring, so the flag is refused there |
  | `--cache-mount-src`, `--cache-mount-opts` | the export the wrapper mounts before the MP server starts (`//store.example/kvcache`) and its mount options; empty = already mounted. Refused for a seat with no cache server (the tier declares none, or `--config` binds it storeless), where nothing would mount it |
  | `--host-prefix` | where this machine reads the distro's filesystem for the prerequisite check (`//wsl.localhost/<distro>`); never written to a file. When it is a `wsl.localhost` or `wsl$` share its name must be `--distro`; when it cannot be read the error says so instead of asking for a venv |

  ```sh
  local-offload install vllm-seat --profile blackwell-3x16 --root <repo> --home C:/llama-swap --user <account> \
    --proxy-host 127.0.0.1 --distro <distro> --venv /opt/seat/vllm-env --hf-home /hf \
    --lmcache-overlay /opt/seat/lmcache-overlay --host-prefix //wsl.localhost/<distro> [--config <config.json>]
  ```

  The command only writes files: it copies nothing into the distro and registers no task, and it says so in its
  output. Copy `<seat id>.env` (and `templates/`) with the shipped `seat_fg.sh` and `seat_stop.sh` into the distro's
  seat directory, keep the four Windows scripts in the Windows seat directory the llama-swap entry runs them from, and
  run `register-seat-tasks.ps1` once, elevated. The llama-swap entry is not written by this command (the Windows
  installer binds the tier's fallback seat); `setup/templates/vllm-seat/llama-swap-entry.yaml` is its reference, kept
  equal to what the renderer writes by `TestVLLMSeatReferenceEntryDeclaresWhatEntryRenders`: `ttl: 300`,
  `concurrencyLimit` equal to `max_num_seqs`, `useModelName`, and the two PowerShell stubs as `cmd` and `cmdStop`.
  Parity is a diff, not a belief: render the tier and compare the box's seat env with the result
  (`TestCommittedTiersRenderTheirMeasuredSeatEnv` pins what each committed tier renders, cache-server lines
  included). The rendered env always ends its lines with LF, whatever the checkout holds (bash in the distro reads
  a CR as part of every value), so a byte diff against the box's file is meaningful. It carries `SEAT_VENV` and
  `SEAT_MODEL`, which a hand-kept env usually leaves to `seat_fg.sh`'s defaults (`<seat dir>/vllm-env` and the
  hard-coded repo id): expect those two lines to show in the first diff. LMCache's fs_native page names embed the
  resolved model path (the command prints a NOTE about long `--hf-home` values for that reason), so after the first start with a rendered env confirm that
  the store still hits the pages written before (unverified here: the render has only been compared with the tier).
  `--user` and `--proxy-host` are still required (`--user` is the interactive Windows account the tasks are registered
  for). The windows-wsl-only flags (`--distro`, `--wsl-seat-dir`, `--host-prefix`, `--lmcache-overlay`,
  `--cache-mount-*`) are an error, not a silent no-op, on a seat that starts through systemd. An fp8 KV seat bound to a
  cache server cannot be rendered for systemd at all (its unit carries no LMCache wiring); the error says so.
- **A seat that names a shipped chat template (`chat_template`, e.g. the blackwell-3x16 seat's
  `qwen3-fold-system.jinja`):** the render includes `templates/<name>` beside `<seat id>.env`, and
  `SEAT_EXTRA_ARGS` passes `--chat-template <WSL seat dir>/templates/<name>`. Copy the whole render —
  `templates/` included — into the distro's seat directory. `seat_fg.sh` refuses to start and names the
  file when it is missing, before the MP server is touched; vLLM would refuse the path too, but only after
  the wrapper had restarted the MP server, with the reason in a traceback and HTTP 500 on the lane.

The measured transport of choice is `fs_native` over a network share of the store's RAM disk (0.112.1+;
<node-c> tmpfs over SMB 3.1.1: a 23.7k-token prefix back in 0.56–0.70 s vs 3.8 s through Valkey, 2026-09-04).
The block then names the mounted path, and the seat's `seat.env` names the share the wrapper mounts first:

```json
"kv_cache_server": [{
  "enabled": true,
  "store": "fs_native",
  "address": "/mnt/kvcache/lmcache-seat-tp2-v2",
  "l1_staging_gb": 8,
  "chunk_size": 784,
  "key_prefix": "seat-tp2-v2",
  "seat": "qwen3.8-27b-vllm"
}]
```

```sh
# seat.env (the wrapper mounts the share before the MP server opens base_path, and refuses to start without it)
SEAT_L2='{"type":"fs_native","base_path":"/mnt/kvcache/lmcache-seat-tp2-v2","num_workers":8,"use_odirect":false,"max_capacity_gb":38}'
SEAT_L2_MOUNT_SRC=//cache-server/kvcache   # a HOSTNAME this box resolves (tailnet MagicDNS or static DNS) — never a DHCP address
SEAT_L2_MOUNT_DIR=/mnt/kvcache
SEAT_L2_MOUNT_OPTS=credentials=/root/.smbcred,vers=3.1.1,rsize=4194304,wsize=4194304,cache=none,actimeo=1,noserverino,nobrl
SEAT_L2_MIN_MBPS=200                       # optional write floor: refuse to start on a crawling path (0 = off)
SEAT_L2_MOUNT_SRCADDR=auto                 # optional: pin the CIFS source to the lowest-metric default route (two NICs on one subnet);
                                           # with a .local store name the launcher also mounts the mDNS answer on that network (below)
SEAT_LMCACHE_PYTHONPATH=<seat-dir>/lmcache-overlay   # optional: load LMCache from an overlay (an unreleased fix), not the venv
```

- A `.local` store name with a pinned source address (`SEAT_L2_MOUNT_SRCADDR`, `cifs` only; register B-55). The system resolver
  returns ONE of the addresses the store's mDNS responders answer with, and a store host that has two responders (one pinned to
  its wired NIC, another that answers on every interface with that interface's own address: Wi-Fi, container bridges, a VPN)
  makes that one arbitrary. The mount landed on the Wi-Fi path (~24 MB/s, so the write floor degraded L2), on an unreachable
  bridge address, or on the VPN address that refuses the pinned source, and every start for three days ran without L2. The
  launcher now asks mDNS itself for all the A answers (a raw query to `224.0.0.251:5353` with the QU bit, three times in about
  a second, sent out of the pinned source address; the embedded helper runs under `timeout -s KILL 3`, so a seat start never
  hangs on it) and mounts `//<address>/<share>` at an answer inside the pinned source address's IPv4 network (the prefix
  comes from `ip -4 -o addr`). With ONE answer inside the network that is the address. With SEVERAL (the store's wired and
  Wi-Fi addresses on one network, where arrival order cannot tell them apart) the launcher pings each one from the pinned
  source address (`ping -I <source address> -c 5 -i 0.2 -q -W 1 <address>`, each ping under `timeout -s KILL 3`, one after
  the other, so at most 3 s per candidate) and mounts the one with the lowest MINIMUM round-trip time; equal minimums go to
  the earlier answer. On the reference workstation the wired address measured a minimum RTT of 0.425-0.477 ms and the Wi-Fi
  one 0.983-2.288 ms (three runs each, more than 2x apart on every run; the averages, 0.57-0.62 ms against 1.5-26 ms, were
  much noisier, so the minimum is what is compared). A candidate whose ping fails shows `no reply` and is never chosen over
  one that replied. If ping is missing, no candidate replies or no time can be read, the choice is the first answer inside
  the network by arrival order, the rule in force before this tie-break. One `seat_fg:` line names every answer, the
  candidates with their results and the rule that chose, for example `mDNS answers: A B C, inside N/P: A (min RTT 0.43 ms)
  B (min RTT 1.10 ms); chose A, the lowest round-trip time` (the fallback reads `chose A, the first inside the network in
  arrival order (round-trip times could not be compared: <why>)`), and the status file reads
  `ok <stamp> mbps=<n> addr=<address>`. Whatever else happens (no python, no answer, nothing on that network, a helper that
  fails or times out) leaves the mount by name exactly as before, and the line printed says why. A name that is not `.local`,
  an IPv4 literal, an `nfs` mount and an unset pin are never touched. **The address is chosen when the share is mounted** (once
  per WSL session). A start that finds the share already mounted does not mount again, does not unmount it (another seat may be
  using it, and a new pick at every start could trade a good mount for the Wi-Fi one) and asks no mDNS; it prints one `seat_fg:`
  line saying where the share is (source and server address, read with `findmnt`) and how to choose again, the status file then
  reads `addr=<that server address>`, and a write-floor degrade names it. **A share an earlier start mounted at the wrong address
  (for example by name, before this change) stays there** until you unmount it while no seat uses it (`umount
  <SEAT_L2_MOUNT_DIR>`) and start the seat again; restarting the WSL distro does the same. **Limit:** the round-trip time is a
  heuristic for the better path, not a throughput measurement. When the store's wired and Wi-Fi addresses sit on ONE network
  the minimum RTT separated them on the reference workstation, but a store whose fast path has a higher latency than its slow
  path would be picked wrong, and a store whose fast path drops ICMP loses to a slow path that answers it (the line shows
  `no reply` for it). Name the wired address in `SEAT_L2_MOUNT_SRC` to override all of it, as a reserved or static address of
  the store (a literal is mounted as given; a DHCP lease is what the rule above forbids) or as a name that is not `.local`
  which this box resolves to it (a static DNS entry or a hosts-file line: such a name is not looked up and nothing is pinged,
  and a `.local` name pinned in `/etc/hosts` is NOT honoured, because the lookup asks mDNS directly).
  The degrade messages keep their text and name the address when the launcher chose it or found the share mounted at it.
- `SEAT_LMCACHE_PYTHONPATH` (optional, 0.113.13): a directory prepended to `PYTHONPATH` for BOTH the LMCache
  MP server and the vLLM engine, so the seat imports LMCache from an overlay copy that carries a not-yet-released
  upstream fix, leaving the installed package untouched. Empty/unset = the installed package. The server and the
  engine must load the SAME LMCache (a mismatched serializer corrupts store/retrieve silently), which is why the
  one variable feeds both. Build the overlay from whatever LMCache is installed plus the fix, and REBUILD it after
  any `pip install -U lmcache` so the patch rides the new base; when the fix ships in a release, unset the variable.
  First use: LMCache PR #4253 (fp8-KV / FlashInfer store corruption on GDN hybrids, LMCache #4247).
  Upstream state, read from the GitHub API on 2026-10-19: #4253 was merged on 2026-09-28 (merge commit
  `1847435fa4`) and #4247 is closed. The merge is contained in the v0.5.6rc5 tags (published 2026-10-12 and
  2026-10-13), a release candidate with no final 0.5.6 seen. Keep the variable set until the seat runs an
  LMCache build that contains the merge; that seat move is tracked elsewhere.
- **A seat on WSL2 with vLLM ≥ 0.30 and LMCache 0.5.6 needs two house patches.**
  - **vLLM (start).** vLLM ≥ 0.29 backs a worker's KV cache with one allocation, and WSL2 refuses to export one that large through CUDA IPC, so the seat dies at start (`dxgkio_make_resident: Ioctl failed: -12` in the guest's kernel log).
    - Fix: apply `setup/templates/vllm-seat/vllm-patches/house-kv-split-<vllm version>.patch` to the venv.
    - The rendered WSL2 seat env already exports `HOUSE_KV_SPLIT=1`, and an unpatched vLLM never reads it; a seat env written by hand needs the line.
    - Then the seat log shows `HOUSE_KV_SPLIT=1: split the … KV cache into N region(s)`.
  - **LMCache (first store).** LMCache 0.5.6's copy-engine direct path faults on WSL2: the MP server logs an illegal memory access on its first store or retrieve, and requests then hang.
    - Fix: build the overlay from the `wsl2-0.5.6` set, `PATCHES=<seat-dir>/lmcache-patches/wsl2-0.5.6 bash lmcache-patches/repatch-lmcache-overlay.sh`.
    - The MP log then shows `HOUSE-WSL2-NO-DIRECT-COPY` once per server start.
  - **Not yet supported:** a pipeline-parallel seat stays on a 0.5.5 overlay until patches 02 and 04 are ported to 0.5.6.
  - Details, measurements and the probe are in both READMEs. Point the seat at a new L2 `base_path` when LMCache changes version.

- `address` must be private (LAN or tailnet); a public address is refused at load by key name, and so is
  a URL or host:port under `store: "fs_native"` (which takes the absolute path of the mounted export).
  Bulk KV prefers the direct LAN (WireGuard measured 6.6× slower).
- Name the store by a hostname, and re-measure the path after any network change. On 2026-09-04 a seat env
  that mounted `//<lan-ip>/kvcache` refused every start for hours after the store's DHCP lease vanished; the
  MagicDNS name mounted at once — over a Wi-Fi hop, at 4.6 MB/s, which is slower than recomputing the prefix.
  `SEAT_L2_MIN_MBPS` turns that into a refusal instead of a silently useless tier; until the path is back,
  run the same-box tier (`SEAT_L2=` empty, no mount vars, `l1_staging_gb` sized as the tier) and set
  `enabled: false` here so `offload_status` and the seat agree. The wrapper's refusal message names the
  cause (does not resolve / port unreachable / answers but the share refused).
- `chunk_size` must equal the engine's unified block size for the model (vLLM logs "Setting
  attention block size to N tokens"; 784 for Qwen3.8-27B with fp16 KV). The 784 default is that one
  model's number: status reports `chunk_size_defaulted: true` until you set it.
- `key_prefix` is one namespace per stack generation: change it (or flush the store) whenever the
  engine layout, the KV dtype or the LMCache build changes — objects written under another
  generation fail reads with "value size exceeds buffer capacity" and the tier silently pays nothing.
  `audit-config` treats `key_prefix` as node-owned when both sides list their bindings.
- `key_prefix` or `seat` is required when enabled: two seats must never share a namespace by accident.
- `offload_status.kv_cache_server` LISTS every binding (`bindings[]`, each with its seat, store,
  address, key_prefix, l1_staging_gb and declared/enabled state) plus `unbound_seats` — the same
  list `doctor` fails on, so the report and the gate cannot disagree — and, for a Valkey store named
  by an IP literal, a 1 s TCP `reachable` fact per binding; a hostname is reported as unprobed. A
  binding the load refused is reported `invalid` and never dialed. The bindings are declarative: each
  seat wrapper runs what ITS `seat.env` says — keep them in agreement (`install vllm-seat --config`
  is how you stop doing that by hand).

The seat itself: `setup/templates/vllm-seat/` has the reference `seat_fg.sh` (starts the LMCache MP
server and the engine in the foreground of the llama-swap client, so a swap-out reaps the engine
while the store keeps the pages), `seat_stop.sh`, the llama-swap entry, and the second device's
`kv-cache-server.service` (a systemd-guaranteed Valkey container; do not rely on docker's restart
policy). **After an engine crash (2026-09-30):** nothing runs a stop for a crash, so the dead generation's engine workers
and MP server are reaped by the NEXT start (`seat_fg.sh` runs `seat_stop.sh` once when no engine of its port is alive
and something of the stack is left) and, on a Windows/WSL seat, by the stub's crash exit (`seat-cmd.ps1` starts the
stop task, waits for it and logs its result; after an unload it does nothing, which `seat-cmdstop.ps1` marks with a
`seat-stop-requested-<seat>` file in the seat directory: that file is the marker, not litter). What is reaped is chosen by
rules, never by the port a process holds (engine processes with no live vLLM API server above them, and this stack's MP
server); a foreign listener on the MP HTTP port is still refused. `seat_stop.sh` writes its output to `seat.log`. To take
effect, copy `seat_fg.sh` and `seat_stop.sh` into the distro's seat directory and re-render and deploy BOTH
`seat-cmd.ps1` and `seat-cmdstop.ps1` — diff a deployed stub before replacing it, since one may have been edited by hand.
Optional knobs in the seat env: `SEAT_REAP_WAIT_SEC`, `SEAT_MP_PORT_WAIT_SEC` (both default 10, a number of seconds;
anything else is said in the log and means 10). Details: `docs/systems/cache-server.md` 4g.
**KV pool pinned from free memory (2026-09-07, `seat_fg.sh`):** `--gpu-memory-utilization` budgets a fraction of the
card whatever the co-residents hold, and the profiler lands the same config at different pool sizes on different starts; set
`SEAT_KV_HEADROOM_GIB` in the seat's env and the launcher instead computes the pool from what is actually free on the tighter seat
card at launch — free − `SEAT_NONKV_GIB` (the engine's weights + non-torch + peak activation per worker, read from the profiler's
own banner) − the headroom (what must stay free for co-residents' growth) — floored at `SEAT_KV_FLOOR_GIB` (2.0) and capped at
`SEAT_KV_CAP_GIB` (3.4), and passes it as `--kv-cache-memory-bytes` (per worker; vLLM then ignores the utilization). The banner
line names every input. Off unless the headroom knob is set; measured on the reference workstation after a util-0.90 seat stalled
(gate verdict 2026-09-07: headroom 0.5 GiB with the utility seats on CPU → the same 209,597-token pool on 10/10 starts, c32
218.9 tok/s at TTFT p95 11.1 s, a 20-minute soak of 1,624 requests with 0 errors — the run-to-run pool variance is gone) 
under daytime co-resident growth. Prove the tier with vLLM's own `vllm:external_prefix_cache_hits` counter around an
after-eviction request, and prove fidelity with a planted needle retrieved verbatim after eviction
and after a restart — hit counters alone do not prove the context came back intact.

**A pinned pool the floor raised can be too small for `SEAT_MAX_LEN` (register B-54, `SEAT_KV_NEED_GIB`):** a card holding ~2.2 GB of
desktop graphics memory gave a pool of 1.95 GiB, floored to 2.0, and vLLM 0.28 refused every start (`2.66 GiB KV cache is needed,
which is larger than the available KV cache memory (2.0 GiB)` for 163,840 tokens) while llama-swap restarted the seat on every
request (about 98 failed starts in two days). Set the optional `SEAT_KV_NEED_GIB` to that `N GiB KV cache is needed` figure,
rounded UP past the second decimal (vLLM prints it to two decimals, rounded to nearest: a need of 2.664 prints as `2.66`, and a
knob typed as `2.66` would then leave a pool of 2.663 GiB "enough" and the refusal in place; write `2.67` or `2.7`). The figure
belongs to one `SEAT_MAX_LEN`, parallelism, speculative-decoding setting and model: measure it again when any of them changes (a
stale figure that is too low leaves the refusal in place with no line from the launcher; one that is too high auto-fits a
context longer than a lowered `SEAT_MAX_LEN`, because `-1` is capped at the model's own maximum, not at `SEAT_MAX_LEN`). When the pinned pool is below it the launcher passes `--max-model-len -1` instead of `SEAT_MAX_LEN`, and vLLM picks the
largest context that fits the PINNED pool (never above the model's own maximum), so the seat serves LESS than `SEAT_MAX_LEN`; one
`seat_fg:` line says so and vLLM logs the length it chose (`Auto-fit max_model_len`). Pool at or above the knob: nothing changes.
Knob unset and the pool floored: `SEAT_MAX_LEN` is kept (an auto-fit could exceed the length you set) and a WARNING names the
knob. A non-number is a warning and is ignored. The knob only acts while the pin is on. No renderer emits the pin family
(`SEAT_KV_HEADROOM_GIB`, `SEAT_NONKV_GIB`, `SEAT_KV_FLOOR_GIB`, `SEAT_KV_CAP_GIB`, `SEAT_KV_NEED_GIB`): they are per-box measured
figures, so the template seat env lists them commented out. Proven by `setup/templates/vllm-seat/seat_fg.kv-and-store.tests.sh`,
which runs the whole launcher against stand-ins and reads the argument list vllm was started with.

**Humming kernels (2026-10-21, [ADR 0095](architecture/decisions/0095-humming-serves-the-weight-formats-marlin-cannot.md)):**
vLLM installs Humming, a JIT-compiled GEMM library for quantized weights, as a dependency (always the newest), and serves on it every
checkpoint whose weights Marlin cannot (2, 3, 5, 6 or 7 bits); 4-bit and 8-bit seats stay on Marlin. Humming compiles a layer's
kernels on its first forward. The launchers keep those kernels beside vLLM's compile cache (`humming-cache` and `humming-tmp`
under `seat_fg.sh`'s work directory, `cache/humming` under the systemd stack) and build at most eight at once, where
Humming's own default is one build per CPU. The first start of a new model or vLLM version pays the compile (291 s against 175 s
with the kernels cached, 94 kernels for the 27B INT4); every later start and every start after the idle unload reuses it.
`SEAT_HUMMING_CACHE`, `SEAT_HUMMING_TMP` and `SEAT_HUMMING_BUILD_WORKERS` move the directories and change the cap. Each start
logs `seat_fg: Humming kernel cache <dir> (at most N parallel builds)`, and the engine names its kernel in `Using
<Name>LinearKernel for CompressedTensorsWNA16`. `--linear-backend humming` moves a 4-bit seat onto Humming. Its proving run gave
the same output and footprint as Marlin, but its engine stopped during a 23.7k-token prefill that Marlin served, so no seat runs
it and a seat that opts in proves that prefill first.

**Pipeline seats and the store (measured 2026-09-03, served since 2026-09-22):** stock LMCache sizes L2
reads from one layout per model, but the stages of a pipeline-parallel seat hold different numbers of
full-attention layers — the 3-card seat's 28,13,23 split (opencode's seat since 2026-10-04, the agent seat before) of a 64-layer model with full attention every
4th layer puts **7/3/6** attention layers on its three ranks — so reads failed for every rank whose layout
differed and the evict phase got 0 external hits. With the per-rank layout overlay patch (each rank's layout
bound to its own pages; loaded through `SEAT_LMCACHE_PYTHONPATH`) the 3-card pipeline seat is bound to its
own `fs_native` store, as in the example above, and serves L2 hits. **Caveat, still open:** the FIRST
request after an MP server start gets 0 L2 hits — it recomputes — until register-time binding lands; later
requests hit. Each seat writes its own wrapper status file (`SEAT_L2_STATUS_FILE`: a rendered seat env names
`seat-l2-<seat id>.status` beside it, e.g. `seat-l2-qwen3.8-27b-vllm-3card.status`; a hand-kept env may name
another), and the binding's `status_file` must name exactly the file that seat's env names.
L1 staging for this hybrid model is 8 GB on the two-card seat and 16 GB on the three-card seat (2 GB, the
default, fails its stores; 8 GB on the three-card seat left too little free to stage an L2 hit back).
Details: [`docs/systems/cache-server.md`](systems/cache-server.md), ADR 0033 (the tier), ADR 0045 (a binding per seat).

### Delegate subtasks across fleet nodes (`agent_delegate` / `delegate`)

Fan self-contained sub-agent contracts out to this box or to fleet nodes on your tailnet
(never cloud — [ADR 0023](architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)).
Placement is **quality-first**: an idle local box runs the work up to its run-cap line
(`fleet_max_concurrent_jobs`, 4 by default; 1 on a single-slot tier, below) and the rest of a wider call goes to the nodes that have room
([ADR 0076](architecture/decisions/0076-a-calls-width-is-sized-from-its-deal.md));
a remote node is also used whenever the local GPU is busy; either way it must pass the capability gate. A box with no agent seat
(a [delegation client](systems/delegation-client.md): `agent_model` and `model` both empty) is never a
placement: route=auto and route=spread send every subtask to the fleet. Wire details:
`docs/FLEET-NODE.md`. Template contracts to start from: [`contracts/`](../contracts/README.md).

**A single-slot box publishes one worker.** `fleet_max_concurrent_jobs` is how many jobs a node runs at once, and it is
what the delegator believes: a node that publishes 4 is dealt 4 at once. That is right for a seat that serves four requests
at a time and wrong for one that serves one: over a `--parallel 1` llama.cpp seat a node publishing 4 holds three jobs that
only wait inside the seat, charged to their wall, while the delegator counts them as parallel workers. So the installer
seeds `fleet_max_concurrent_jobs: 1` on every tier whose agent seat, resolved through its aliases, is a single-slot
llama.cpp entry of the shipped serving template: `amd-gcn`, `amd-rdna3-dgpu`, `ampere-6`, `ampere-8`, `blackwell-8`,
`blackwell-32`, `blackwell-48`, `blackwell-72`, `dual-gpu` and `volta-16` today (a test derives the set from the templates, so
a tier added later cannot drift from it; a tier with a vLLM agent seat, which serves many requests at once, and a tier with
no agent seat are out of scope). `fleet_max_queue_depth` stays at its default, twice the workers: one running, one waiting.
Two things follow. The same key is this box's own run-cap line when it delegates, so a one-worker box runs ONE local job at a
time and sends the rest of a call to the fleet; that is intended, because a single-slot seat serves one request at a time.
And it governs the text-endpoint lanes (agent and text jobs): media, stt and pipeline routes are exempt. A seed reaches
FRESH configs only: on a live node, set `fleet_max_concurrent_jobs: 1` in `config.json`, restart `fleet-serve`, and read
`max_concurrent_jobs` on `/fleet/health` (`local-offload audit-config` lists the key as `SEED-ONLY` or `DIFFERENT` until then).
`doctor` prints one informational `capacity:` row on a box with an agent seat: the cap against the slots in the LIVE serving
config (`serving_config_path`; `--parallel N`, `-np N` and `LLAMA_ARG_N_PARALLEL` are read, alias-aware, and a stated `-1` or
`auto` reads as the 4 slots llama-server serves for it; an entry that is not llama.cpp, a vLLM seat, reads as its
`concurrencyLimit`, which the installer sets to the seat's `max_num_seqs`). `OK` when the cap equals or is below the slots,
`WARN` (with the value to set) when it exceeds them, `UNKNOWN` (naming why: the path is unset or unreadable, no entry answers
to the seat, a non-llama.cpp entry states no `concurrencyLimit`, or a llama.cpp entry states no slot flag) when the slots cannot
be read. The cap follows the seat the box serves NOW. A box of a vLLM tier (`ampere-16`, `blackwell-16`, `blackwell-2x16`,
`blackwell-3x16`) whose vLLM venv is not installed serves the tier's llama.cpp fallback at `--parallel 1`: the row warns, and says
which vLLM seat the tier declares, its `max_num_seqs`, and that the cap goes up to it when that seat takes over, so setting 1 now
does not strand the box at 1 after the venv is installed. Two places can name the tier, the installer's (`installed.json`) and
the config's `tier_profile`, and a tier counts only when the box's agent seat is its declared vLLM seat or that seat's fallback, so a
manifest recorded under an older layout (`dual-gpu` declares no vLLM seat, and is what three Blackwell cards classified as before
`blackwell-3x16` existed) never hides the seat the config's tier owns. When both tiers count (the alias `agent-pool` is declared by
`blackwell-16`, `blackwell-2x16` and `blackwell-3x16`), the row names the one whose declared seat is the entry of the serving config
that `agent_model` resolves to, because that entry is what the box runs; when the entry names neither, the config's `tier_profile` wins
over the manifest's. With no tier that counts, or with an agent seat that is not a tier's, the row names no tier. It never changes
doctor's exit code.

**A name, not a LAN address (`lan ip:` row).** Cross-machine reach uses tailnet names, because an IP literal works until the box it
names moves or its firewall is scoped to the tailnet, and then the endpoint is dead with nothing in the file that says why. `doctor`
prints one `lan ip:     WARN` row for each endpoint the harness dials whose host is an IP literal instead of a name: a
private-range literal (RFC 1918 or an IPv6 unique-local address), a link-local one, or a tailnet CGNAT one (the row then says to use
the MagicDNS name). Loopback is fine, every name is fine, and a public literal is left to the tailnet guard. The fields read are
`endpoint`, `seat_endpoints`, `cascade_remote_lanes`, `delegate_remotes`, `fleet_queue_holder`, the explicit entries of
`pair_workloads_relay`. `nim_endpoint` and `nim_bases` are not read, because a self-hosted NIM on the LAN is what that allowlist is
for, and neither is a `kv_cache_server` address: a literal on the direct LAN or the tailnet is a valid store address and the
preferred one (bulk KV over MagicDNS measured several times slower, and the `offload_status` reachability readback of a store dials
literals only, so a name switches it off), which a row asking for a name would undo. A store that moves is the operator's call to
rename. The row names the field, with its list index or map key, and never prints the address. It is configuration only,
so a serving layer that is down does not hide it, a config with nothing to warn about prints nothing, and it never changes
doctor's exit code. A literal outside loopback and the tailnet range was already refused when the config loads in
`seat_endpoints`, `cascade_remote_lanes` and `fleet_queue_holder`, and is a `config findings` FAIL in `delegate_remotes`; the
row is what says why, and it is the only word on a tailnet literal and on the fields that load clean (`endpoint`,
`pair_workloads_relay`). Fix the warning by writing the node's dotless MagicDNS name (or a name under a
zone listed in `tailnet_suffix` / `tailnet_suffixes`) in the field.

**A power profile applied this boot (`power:` row).** A box can apply a GPU power limit and a clock lock at boot with a separate tool, and when that step is skipped the card runs at stock limits and nothing else says so. The tool writes a state record on every run, refusals included, and `doctor` prints one `power:` row from it. `OK` says a profile was applied since this boot began, how long ago, and how many steps returned success. `WARN` says no profile was applied this boot (the newest record is older than the boot, once the box has been up ten minutes: the boot apply runs a few minutes after boot, so a box just started gets a `SKIP` that says it is not yet a finding), or the last run was a dry run (which replaces the record of the real run), a refusal, a failure, an empty plan, or skipped by the tool's crash guard (`cookie_blocked`: a cookie from an earlier apply was still on disk, so that run applied nothing; the row does not say the card is at stock limits, because a resume from sleep a few minutes after the boot apply meets the cookie of an apply that is still in force, and a cookie from this boot clears itself within ten minutes, while one from an earlier boot stays until a person clears it), or the apply was partial: a step short of the plan, a lever skipped other than by the tool's own design (a card whose power limit is owned by firmware has none to apply, and that is counted, not warned), or a driver that differs from the profile's stamp, which leaves the tuned levers waiting for a re-evaluation. `UNKNOWN` (naming why) says the file cannot be read, is not JSON, is of a schema this doctor does not read, or carries no usable time; unknown is never reported as absent. With no state file the row is one `SKIP` line saying no power profile state is recorded, which is not an error: a box that applies no profile records nothing. The file is found only by `LOCAL_OFFLOAD_POWER_PROFILE_STATE`, which holds the path of the tool's state file: the harness knows no tool's install folder, so there is no platform default, and a box that applies a profile at boot sets the variable (until it is set, and on a box whose applier keeps no state file, the row reads `SKIP`). "This boot" counts from when the operating system session began, which a Windows Fast Startup power cycle does not reset; when the platform cannot say, the row says it cannot tell. The row prints no path, host name or card id, and not the applier's free-form detail. **It reads the record, not the card.** The record says what the tool did, and "applied" in it means the writes returned success, so the row does not compare the recorded limit and lock with the driver's live ones: the harness has no reader of a card's power limit or clock lock yet, and a limit changed after the apply reads green here until it does. It never changes doctor's exit code. **The same reading rides `/fleet/health` as `power_profile`** (`ok`, `skipped` or `unknown`, a reason token, the cards' recorded power limits and whether a clock-lock step succeeded; `OK` is `ok`, `WARN` is `skipped`, `SKIP` and `UNKNOWN` are `unknown`), and `offload_status` shows one token per node in its `fleet` block, the brief form included, so the roster says which nodes' profiles took. A node with no state file, or no setting (a Linux node with no boot-apply tool), publishes `unknown` with the reason `no_state`: an answer, not an error. See [FLEET-NODE.md](FLEET-NODE.md), "Boot-time GPU power profile".

`doctor` also prints a `backends:` section after its checks, whatever they returned: one row per lane and per seat saying which
serving engine answers (`active_backend`: `llama.cpp`, `vllm`, `rkllm`, `whisper.cpp`, `unknown` (the engine is not named, a probe timed out, or another node serves the seat), or `none`; on a lane, the engine of the seat the
lane is bound to), the chain it was picked from, how deep the evidence goes (`process`: llama-swap's roster and running list;
`endpoint`: a loaded seat's own health endpoint), and why. A box whose tier prefers a vLLM agent seat while `agent_model` is bound to
the llama.cpp fallback reads `llama.cpp` on the agent lane and says in the row that the vLLM seat is available but not bound. The
default probe loads nothing and sends no completion. `doctor --probe-completion` (listed by `doctor -h` and by the usage a mistyped flag prints) adds a 1-token completion
to each seat that is already running and, on a box where nothing is running, to ONE idle seat, which it loads (a cold vLLM load takes minutes). It never
loads a seat over a running one: every other seat is reported as `not probed: would load over <seat>`, and a seat with no chat route (an embedder, a
speech-to-text seat) as `not probed: not a chat seat`. It never runs unless you pass the flag; run it deliberately, not from a script that runs every few minutes.
The section is informational and never changes the exit code. `doctor` has no JSON mode: it prints these rows as text, and the JSON
of the same four fields is `lane_backends` and `seat_backends` on `/fleet/health` (docs/systems/fleet-node.md).

**A pin needs a reason (ADR 0078).** `route: local` and `route: remote` pin a call to this box's seat or to a fleet node, and a
call that does that must say why: `pin_reason` (`--pin-reason` on the `delegate` and `research` verbs) is `privacy` (the material
may not leave this box) or `locality` (the work needs something only this box has), both with `local`; or `measurement` (the call
measures a specific place: a benchmark, a bake, a seat A/B) or `operator` (you named this placement), with `local` or `remote`. A
reason with any other route, or a value outside the set, is refused before anything is read or fetched, and the error lists the set.
**Without a reason a local or remote route is only a hint**: `local` is placed as `auto` places it (the idle seat takes its run-cap line
and the rest of a wider call goes to the nodes with room; a call that fits the line stays on the box), and `remote` is placed
fleet-first, with the idle local seat taking what no remote has room for where `remote` would have deferred. A hint never runs on a seat
a text lease reserves, a pin is authoritative to the end (a failed verification of a subtask pinned to a node is retried on another
node or not at all, never on the local seat), and a call that carries a browse grant (`allow_browse`, which drives this machine's own browser) is pinned under
the implied reason `locality`. Every result's `placement` opens by saying whether the hint was honoured or overridden, and the ledger row carries
`route_asked`, `pin_reason` (empty for a hint) and, as `route`, the route the engine applied (`auto` for a hint). `offload_status`'s
fleet block has a `pins` object with this server's counts since it started. This is the cure for a fan-out queued on one seat while
the fleet idled (2026-10-07: 32 of 78 pages `not started`). A measurement script that has to run on one seat says
`--pin-reason measurement`, as `scripts/parallel-sessions-gate.ps1` and `scripts/write-door-gate.ps1` do.

**A text-class GPU lease reserves the local seat (0.113.14).** `gpu reserve --class text` is how a
benchmark, eval or measured run keeps everyone else off its cards. Until 0.113.14 delegate placement
read the lease only to *prefer* a remote (route=auto) — and on route=spread not at all — so a
contract with no eligible remote still ran on the reserved seat (three foreign contracts loaded a
reserved two-card seat mid-measurement, 2026-09-05). Now, on route=auto and route=spread, a held
**text** lease takes the local seat out of placement: an eligible remote takes the work; with none,
the placement waits (until the call's deadline less a reserve when the call has one, ADR 0073; else up to the larger of
`agent_placement_wait_sec` (default 120 s) and `agent_lease_wait_sec` (default 0)), re-reading the
lease once a second, then defers with class `infrastructure` and a reason naming the holder (class,
pid, reason, origin, expiry) so the caller can wait, route elsewhere, or ask. A retry never runs on the
reserved seat either (register C-81), but it is not held in line for the holder as a first placement is:
a verification retry, or the re-issue of a seat-down defer, whose first attempt ran on a fleet node goes
to an untried node and, with none, is skipped at once with a `retry_note` that names the holder, and the
first attempt's result stands. A pinned `route=local` (one with a `pin_reason`) is the
caller's explicit choice and is never gated; a reasonless one is a hint, placed as `auto`, which does read the lease. A **media** lease is not a placement gate: it keeps
steering toward remotes as before and is arbitrated at the model-affinity gate (ADR 0026), so
single-box render behaviour is unchanged. The local seat's window is probed live before each run —
llama-server `/props`, else the backend's `/v1/models` `max_model_len` (a vLLM seat behind
llama-swap has no `/props`; before 0.113.14 such a seat was budgeted at the 8,192-token fallback
and `offload_status` showed `ctx_probe_error: HTTP 404` while it was warm). The per-model probes share
one cold-start budget of 10 minutes (llama-swap's `healthCheckTimeout`), because llama-swap answers them
only once a cold seat is up — a vLLM seat measured 222 s, and the old 60 s per-URL timeout returned the
fallback for the whole run. When the probe still cannot answer, the box's `agent_ctx_tokens` is the
window; when it answers and disagrees with `agent_ctx_tokens`, the served window wins and the note says so.

**Store steward (0.113.16) — `fleet_store_root`, `fleet_store_cap_gb`, `fleet_store_prune_every_jobs`, `fleet_store_prune_every_sec` (0.130.1: a time tick, default 60 s, because under a GPU lease no job completes on the node while a bench arm or the pair seat writes ~1 GB/min; negative disables it).** A node that owns a
persistent KV page store on disk (<node-c>'s LMCache fs_native dataset) keeps it under budget between its own turns:
`cap = min(fleet_store_cap_gb, 0.8 × (used + free))`, prune oldest-first from 95 % of cap down to 85 %, after every N completed
jobs (default 8) and on any health poll that finds it high. `/fleet/health` shows it under `store`. Set the root to the
store dataset's mount (for example `/srv/kvstore`) and create the marker file `.storesteward` in it once (the steward
writes it into an EMPTY root itself; a populated root without it is refused at start, so a mistyped root can never be pruned;
every removed page is one journal line), the cap to the dataset quota, and keep the seat's own
`SEAT_L2_PRUNE_GB + max_capacity_gb` under the quota too — LMCache evicts only its own pages.

**Serving-config provenance (0.123.0, ADR 0043) — `serving_config_path`.** The llama-swap config is rendered once, at
install, and nothing re-renders it: `ampere-16` served a 32768 window for weeks after the tier table said 131072, and
`audit-yaml` reported OK the whole time because a stale config breaks no operator rule. `install render` now stamps every
config it writes with `spec_sha256` (a closed input set: tier, render params, template, tier entry, harness version) and
`body_sha256` (the yaml below the stamp, so a hand edit stays distinguishable). Check a live box with
`local-offload audit-yaml --against-render <file>` — flags BEFORE the files — which re-derives from the binary's own seeds
and reports `MATCH` / `STALE(<keys>)` / `UNSTAMPED` / `HAND-EDITED`; STALE names the inputs that moved and exits 1, as does
HAND-EDITED, while UNSTAMPED (every config rendered before 0.123.0) prints as a finding and does not fail. Point
`serving_config_path` at the node's rendered config and `/fleet/health` publishes `serving_config_spec_sha256` +
`serving_config_state` as well, so a fleet-wide sweep is one poll; unset, both keys are omitted. Fix a STALE box by
re-rendering with `install render`, never by hand-editing the file.

**Lease in health (0.113.16).** `/fleet/health` carries `lease` while the node's GPU lease is held; a text lease makes the
node ineligible for new delegated work (and its dispatch answers 503, re-placeable). `gpu reserve --drain --unload-seat` and
`gpu release --warm-seat` are the maintenance verbs — see docs/systems/gpu-lease.md.

**A held card is a place in line (0.115.2).** `gpu reserve` QUEUES behind a current holder for `--wait` (default 8h;
`--wait 0` fails fast) in both the wrapper and `--detach` forms, printing one line on entry and one on acquire; the holder's
declared window is reported, never trusted — the wait runs its full length. **A free card with a waiter already in line
is that waiter's**: a fresh reserve never probes the card bare, with or without `--wait` — it registers first and only the
front of the line claims, so a recipe that chains reserves back to back queues behind an earlier waiter instead of winning
every just-freed card (`--wait 0` then fails fast naming who is ahead; a waiter for OTHER cards, a blocked text-load
admission included, which queues on its seat's cards only, is no reason to wait, and `--cards N` takes a card nobody is
queued for). `--unload-seat` (or `--exclusive`) stamps the
text lease **exclusive**, and the text-load gate then keeps models off the cleared cards for the lease's length — loads ride
a `cascade_remote_lanes` lane or wait their own budget. `offload_status` publishes the local lease under `gpu_lease` with
`queue_with`, the exact command. The rule for every session: **never refuse or defer GPU work because a card looks busy —
reserve it and the machine queues it**:
`local-offload gpu reserve --wait 8h --drain --unload-seat --for <window> --reason "<why>" -- <cmd>`.

**A reserve that loses its lease while draining queues again (register C-59).** If the lease record is released, or
another acquirer takes the card, while `--drain` is waiting, the wrapper form says `the lease was lost during the
drain`, takes its place in the line again with what remains of `--wait`, and drains and clears the seat afresh under
the new lease before it runs the command — it no longer exits at the restamp (`stamping the lease after the drain`).
`--wait 0` stays one try. The `--detach` form cannot re-queue; its error says whether the lease is still held. See
docs/systems/gpu-lease.md.


**Enable — worker node** (the box that will *execute* contracts), in its
`~/.local-offload/config.json`:

```json
{
  "fleet_agent_enabled": true,
  "fleet_auth_token": "<one shared secret, same on every node>",
  "agent_ctx_tokens": 16384
}
```

`agent_ctx_tokens` is the tier's served agent-seat window (the installer records it in
`installed.json`); `0` means the node advertises no ceiling and is **never** chosen for remote
agent work. Then serve beyond loopback on the machine's Tailscale address:

```powershell
local-offload fleet-serve --listen <tailscale-ip>:18811 --listen-trusted-network
curl http://<tailscale-ip>:18811/fleet/health
# ... "agent_enabled":true,"agent_seat":"offload-e4b","agent_ctx_tokens":16384,"agent_seat_resident":true ...
```

`agent_seat_resident` starts `false` (the roster probe is cached, background-refreshed, and
fail-closed) — give it one health request plus a few seconds before concluding anything.

**If the four `agent_*` fields are missing entirely**, the lane is not admissible and no
delegator will ever place here — by design, since a dispatch would be refused. The three
conditions are the same ones the ack-time guard applies: `fleet_agent_enabled: true`, a
resolvable agent seat (`agent_model`, else the workhorse `model`), and a safe listener —
loopback, **or** `fleet_auth_token` set for anything beyond it. Serving on a Tailscale address
with no token is the common miss: the advertisement is withheld rather than published-then-403'd.

**Enable — delegator** (the box that *places* contracts), in its config:

```json
{
  "agent_delegation_enabled": true,
  "fleet_auth_token": "<the same shared secret>"
}
```

That registers the MCP `agent_delegate` tool (tools/list is byte-identical when off) and
unlocks the CLI verb. The fleet is configuration: list the nodes in `delegate_remotes` and both surfaces
use them, with no node named per call. A call may NARROW that list, and what it names has to be tailnet
URLs (loopback, the tailnet CGNAT range, a dotless MagicDNS name, or a host under a tailnet DNS zone you listed in `tailnet_suffix` /
`tailnet_suffixes`;
anything else is refused by name). On the MCP tool `remotes: [...]` may only name nodes already in
`delegate_remotes` (a URL outside it is refused and never dialled, and a box with no `delegate_remotes`
accepts none, so a model can never add a node, the standalone box included, by naming it); a list given
there REPLACES the roster for that call rather than merging with it, so a one-node list confines every
subtask to that node. The CLI's `--remote` (repeatable) is the operator's own: it names any tailnet node,
which is how a node that has not joined the roster yet is smoke-tested (`fleet-smoke --remote`).

A node shared into your tailnet by someone else's account is not yours to trust with your files. Declare it in
`delegate_remote_classes` (`{"http://node-x:18811": "public"}`, beside `delegate_remotes`) and it is placed public-data work
only: the page digests of `offload_research`, which read the public web, and the `fleet-smoke` probe. A subtask that carries a
context document, a diff or a goal an `agent_delegate` caller typed is never placed there on any route; pinned to
`route: "remote"` with only public nodes eligible, it defers with a reason that names the rule. A node you do not list stays
`private-ok`, so a config without the key behaves exactly as before. To use a public node for a typed contract, run that
contract on a node you trust.

**`fleet_auth_token` covers every lane of a node** (a dispatch of any task type, media and `run-graph` included, the poll and
withdraw of any job, and the read of any rendered file); it used to cover only the agent lane and the doors that take a caller's
bytes. With the token set, a request without it is `401`; with none set, a node answers only on loopback (the box's own trust
boundary), refuses everything with `403` on any other listener, and `fleet-serve` will not start there. `/fleet/health` and the
jobs feed stay readable without the token, with the lease's free-text reason, the store path and the weight-file rows (and a
job's error text) left out. A run-graph manifest may name a node pack only from a listed repository
(`render/node-packs.json`; the node installs its default branch's head and records the commit beside the pack), and a model only under `models/<class>/` from an https host listed there (`model_hosts`): a token
holder can have the node render, not run code of its choosing or write files where it chooses.
**Choose a long random value** for it: it is the one secret behind every lane, and a node does not slow down repeated wrong
bearers. Generate it with `openssl rand -hex 32` (64 hex characters) and use the output as the value of `fleet_auth_token`,
the same on every node and every delegator; the installer and the config check accept any non-empty value, so a weak one is
not caught.
**Roll it out in this order: every delegator first, then every node**, each with the same value. This repo's clients already
send the token whenever it is set, but a node that gains the rule refuses three kinds of caller, and they must be put right
first. (1) A delegator with no `fleet_auth_token`, or a wrong one, a media-only box included (media, stt and compose-video
calls needed none before): every dispatch, poll and artifact fetch is `401`, the media client reports that as a `config`
defer, and the node's `/fleet/health` reaches it without the lease reason, the store and the weight-file rows, so its node view
degrades with no error. (2) A delegator older than the first release whose client sends the bearer on a lane: withdraw
0.146.0, text 0.154.0, compose 0.163.0, stt 0.165.0, the roster probe's token check 0.167.0 and media 0.173.0. (3) Software outside this repo
that sends no bearer, which stops working on every lane. Before the node change is deployed to a node, `fleet_auth_token` must
be present in the config its `fleet-serve` reads. A node that already
listens beyond loopback with no token will not start after the upgrade, so give it the token before it; its refusal names the
key and the config file it read. `setup/install.sh --token-file PATH` stores the key in a Linux node's config and refuses to
register the unit without one. `local-offload doctor`'s `fleet token` rows show a mismatch before a call is spent.

**One token goes to every node you list.** `fleet_auth_token` is a single value, and every request to a roster node
(`delegate_remotes`, and the nodes named per call) carries it as the bearer, whichever tailnet zone the entry sits
under. A node shared in from another tailnet, admitted by listing that tailnet's zone in `tailnet_suffixes`, is sent the
same token as your own nodes, so whoever administers it receives a token that opens every node on this roster. List only
nodes you trust with that token. A token per remote, so that a shared-in node is sent one that opens it alone, is not
built yet. The token probe and the single-shot lane clients never follow a redirect with the bearer (the health read
in `internal/delegate` is not changed here and still can), and a roster entry pasted with a `?token=` or
`user:password@` is printed without them everywhere (doctor, the lanes' "probed ..." lines, config findings).

**Context budget — read this before writing a contract.** The 256 KiB wire cap is a transport
bound only; what actually decides remote placement is the gate's arithmetic:
`ceil(chars/3)` over goal + context docs + schema + acceptance, **plus a 3072-token reserve**
(system prompt + tool specs + per-step transcript growth × the 12-step cap), must fit the
node's advertised `agent_ctx_tokens`. The `chars/3` estimate is a deliberately conservative
upper bound (v1 has no remote tokenizer to ask; typical prose runs ~4 chars/token), so the
practical budget is smaller than the raw window suggests:

| Advertised `agent_ctx_tokens` | Estimate budget after the reserve | ≈ contract text that fits | ≈ real prose tokens |
|---|---|---|---|
| 4096 | 1024 | ~3 KiB | ~0.8k — goal + a snippet, no real docs |
| 8192 | 5120 | ~15 KiB | **~2–4k** |
| 16384 | 13312 | ~39 KiB | ~10k |
| 32768 | 29696 | ~87 KiB | ~20k+ |

At an 8k seat, budget **~2–4k tokens of actual document content** per contract; split bigger
inputs across subtasks. A contract that does not fit is not an error — it places locally
(route `auto`) or defers (route `remote`), and the placement reason says why.

**Worked example** (MCP `agent_delegate`; the CLI takes the identical subtask object as
`--contract file.json`):

```json
{
  "subtasks": [{
    "goal": "Read the two release-notes docs and produce a migration digest: list every config key added between them, and summarize the upgrade in two sentences.",
    "context_paths": ["notes/release-a.md", "notes/release-b.md"],
    "output_schema": {
      "type": "object",
      "properties": {
        "added_keys": {"type": "array", "items": {"type": "string"}},
        "summary": {"type": "string"}
      },
      "required": ["added_keys", "summary"]
    },
    "acceptance": ["min_items:added_keys:1", "nonempty:summary", "not_contains:TODO"]
  }],
  "route": "auto",
  "read_root": "/abs/path/to/project",
  "remotes": ["http://<node-b>:18811"]
}
```

Here `http://<node-b>:18811` must already be in `delegate_remotes`. Drop `remotes` to spread over the whole
configured fleet; naming one node confines the call to it.

#### Delegating an IMPLEMENTATION leg (`write_root`, 0.122.0, register D-06)

A subtask may add `"write_root": "<dir relative to the run's read root>"`. The seat then gets
`write_file` + `edit_file` inside that directory of the node's own copy of the inlined docs, and what
comes back beside the usual result is a **unified diff**:

```json
{
  "subtasks": [{
    "goal": "util.go has an off-by-one in Last(): it returns n where it must return n - 1. Fix it, and add one table-driven case to util_test.go covering n = 1.",
    "context_paths": ["util.go", "util_test.go"],
    "write_root": ".",
    "output_schema": {"type": "object", "properties": {"change": {"type": "string"}}, "required": ["change"]},
    "acceptance": ["diff_touches:util.go", "diff_max_files:2", "contains:n - 1"]
  }],
  "route": "local",
  "read_root": "/abs/path/to/project"
}
```

Read the result's `diff` and apply it yourself (`git apply -p1`); **the harness never applies it**, on
purpose — a small local seat is worth handing an implementation leg precisely because a human reads
what it produced before it touches anything real. `diff_files` lists the touched paths and `write_note`
says what the door did when there is no diff (most often: the seat described the change instead of
making it).

Two acceptance verbs read the write set rather than the prose, and are the only checks on a write
contract a talkative seat cannot satisfy by talking: `diff_touches:<path-prefix>` and
`diff_max_files:<n>`. Both FAIL on an empty write set, `diff_max_files` included.

Requirements and limits, all of them refusals rather than surprises:

- The executing node must have `"agent_allow_write": true` (see docs/FLEET-NODE.md). Without it the
  contract is refused at ACK and re-placed on a node that has it, or — locally — deferred with
  `defer_class: "write"`.
- `write_root` must be RELATIVE and must not escape (no `..`, no absolute path, no `.git`, no reserved
  Windows device name). It is relative because the node has never seen your filesystem: the contract is
  self-contained, and an absolute path from your box would name nothing there.
- 8 files, 64 KiB written, 192 KiB of diff. Past any of them nothing is published and the subtask
  defers `write` — split the leg instead.
- No delete, no shell, no `run`, no network. This lane edits files; it does not verify them. Checking the
  result with one real run is still yours.

**Gate it before you rely on it.** `scripts/write-door-gate.ps1` (fixtures under
[`contracts/write-door/`](../contracts/README.md), section "write-door/")
sends three staged legs to one seat — `-Remote http://<node>:18811` for a fleet node's door, no
`-Remote` for this box's own seat — applies each returned diff to a fresh copy and proves it there
(`go test` red → green, a JSON parse, exact-row checks). PASS means the door on that seat produces
diffs a caller can apply; it is the measurement that preceded every door opened so far (register D-06).

`context_paths` are read and inlined **by the delegator**, confined to `read_root`
(≤ 128 KiB per file) — your session's context never pays for them, and the wire contract stays
self-contained (the remote node never reaches back into your filesystem). The node writes them as
files in the seat's read root; the seat then spends its first steps finding them (measured: 89 of
117 failed 4B rows in six days stop at `list_dir` + `read_file`). `setup_actions` (0.113.24) lets
a subtask name up to eight `{tool, args}` calls the node replays before the model's first turn —
`[{"tool":"read_file","args":{"path":"notes.md"}}]` puts the document in front of the seat at
step 0, spending no step. Simpler still: set `agent_seed_context_reads: true` in the executing
node's config and it prepends one `read_file` per context doc by itself (it knows the names).
Read `setup_ran` on the result before crediting either — a node one release behind ignores the
field silently. `acceptance` is
evaluated by the **delegator** after the result returns: a schema-valid result that fails a
check comes back `failed_verification`, never a success. Read the response's `summary` block
first — `{succeeded, deferred, failed_verification, failed, infrastructure}` — eight quiet
defers are a loud outcome, not eight green jobs. `infrastructure` counts the results whose story
is a broken stack rather than the work: defers whose `defer_class` says so (`infrastructure` /
`config`, as opposed to `abstention` / `budget`), plus a local placement taken while every
configured remote was failing its health probe. Non-zero means a node is broken or
misconfigured, not that the small model could not do the job.

`lost_to_stack` rides beside it (omitted when zero) and is the half that is **lost work** —
subtasks that delivered no usable result because the stack failed them: the contracted output
never arrived, as opposed to a local placement that succeeded while the fleet was down. It is not
a "the result is blank" count — a `structured re-pack unreachable` defer carries the finished
loop's prose in `output` with `structured` absent, and still counts, because a contract with an
`output_schema` asked for a checked deliverable. The MCP tool marks the call `isError` when
**nothing succeeded** and something failed, was lost to the stack, or was skipped
(`succeeded == 0 && (failed > 0 || lost_to_stack > 0 || skipped > 0)`), with the JSON body
unchanged; a **partial** result — at least one subtask delivered — is a successful call whose body
names what is missing, because the MCP client keeps only the head and tail of an error-flagged body
and used to cut the surviving digests out of it (C-75, ADR 0065). The CLI's exit code is the wider
`infrastructure > 0` rule below.

`defer_class: "contract"` is the one class that is **your** problem rather than a box's: the
contract carries no `output_schema` for a remote placement, is past the origin hop, or needs
more context than **every** node advertises — a node that advertises no `agent_ctx_tokens` at all
makes it a `config` verdict on that node instead, because an unadvertised ceiling is unknown, not
small. Those stay exit 0 by design. If the summary carries
`corpus_rows_lost` / `ledger_rows_lost`, the results are still complete — the harness could not
write that many telemetry rows (usually a full or read-only disk).

**Which node ran it (`results[].node`, `results[].node_reported`, 0.185.0).** `results[].node` is the node id the node publishes in
`/fleet/health` (the name on its card in `fleet-ui`), the same name `results[].placement` uses, for a run on a fleet node this
delegator dispatched to; it is also the `node` of the delegation-log row and the node part of the ledger's `model_tier`
(`<node>:<seat>`). Before 0.185.0 it was the id the node stamped on its own result, which for a node launched with `--node-id` (the
installers' form) was the OS hostname, so a result could name one node in `node` and another in `placement`.
`results[].node_reported` is that stamped id and appears only when it differs from `node`: a node on a build before 0.185.0 (it
stamps its hostname where it was launched with another `--node-id`), or a local run attributed to another box's engine (register
C-58), where it is the runner's own stamp. A node on 0.185.0 stamps the id it advertises, so it never differs there. It is absent
when the two agree and when no node took the subtask. Match a node by `node`. A `route: "queue"` answer carries the id its
claimant stamped, which is the id it claims under once that node runs 0.185.0 (the delegator has no health view of a claimant). A
run on this box's own seat (`ran_on: "local"`) carries this box's own name, or the box whose engine ran it.

`rig --node <name>` matches a row by its `node` or by the id the node stamped on its own result, and the two names do not select
the same rows. The node's card id selects the rows a 0.185.0 delegator wrote, and, for a node launched with `--node-id` and no `fleet_node_id` (the installers' form), never a row written before (that row carries
only the hostname). The OS hostname selects the rows an older delegator wrote and the rows a 0.185.0 delegator wrote for a node not
yet on 0.185.0, but not the rows of a node that runs 0.185.0, which stamps its card id in both fields. For a window that spans the
change, run `rig` once per name and read both reports.

CLI equivalent:

```powershell
local-offload delegate --contract contracts/research-digest.json --route auto --remote http://<node-b>:18811
local-offload delegate --contract contracts/research-digest.json --route local --pin-reason measurement   # a pin, with its reason
```

Exit 0 covers honest defers (`abstention` / `budget`) and failed verification — the JSON says
what happened. Non-zero is reserved for transport/config failures **and** for
`summary.infrastructure > 0`: a worker whose llama-swap is down defers every subtask, and a
scripted caller reading only the exit code must not take that for a good run. Every subtask
also writes a ledger row
(`task=agent_delegate` in `local-offload ledger`) and a full contract+result+verdict line
under the harness base dir at `delegation-log/YYYY-MM-DD.jsonl`.

Loop accounting on the wire (0.115.20): every `results[]` row carries the node's `steps`,
`stop_reason`, `stop_note` and `output_truncated` as the node reported them. Read them before
trusting a `done`: `stop_note` beginning `forced final answer` means the step budget ran out
and the last step extracted the answer without tools (0.115.19, register D-89) — a correct
answer from an incomplete exploration (since 0.184.0 it may add that the seat first answered
that step with a tool call and was re-asked once with it quoted: the answer is the second reply); `output_truncated: true` means the final completion
ended on `length` and the text is a partial. Both used to be visible only in the delegation log.

Acceptance lint (0.88.0, warn-only): every subtask's acceptance is linted at intake and the
warnings ride the response as `results[].acceptance_lint` — PARROT-PASSABLE (every content
check also matches the goal text: an echoed question passes as verified and the retry never
fires), UNGROUNDED (`contains:`/`regex:` matching nothing in the contract's own docs — fails
right answers), SHAPE-ONLY (`nonempty:`/`min_items:` alone — passes garbage). The run still
happens; fix the acceptance before reusing the contract (authoring rule: anchor ≥1 check to
content that appears in the docs but not in the goal; contracts/README.md).

Previewing a contract without running it (register D-109): `agent_delegate` takes an optional `lint_only: true`. The call
does everything intake does (context_paths read, caps, validation, the acceptance lint), then returns before any
dispatch: no job, no GPU lease, no queue entry, no seat warm-up, and no model runs. The answer is
`{lint_only: true, dispatched: false, subtasks: [{index, acceptance_lint, placement: {route, where, node, reason}}]}`; a
subtask intake refused is a contract-class defer at its own index, as in a real call. `placement` is what the joint deal
would choose from the readings taken now (the local lease file, the local seat's in-flight count, the run line, and one
health read per fleet node): `where` is `local`, `node` (named in `node`), `capacity_wait` (every eligible node is full),
`no_eligible_node` or `not_computed`. Only routes `auto`, `remote` and `local` are read ahead; `spread` and `queue` answer
`not_computed` with the reason, and the placement reads are bounded at 4 s, so a down node cannot hold the call. The pick
is a preview: a lease taken or a refusal before the real call can still move it. Use it to fix acceptance and size a
contract for free before paying for the run.

Closed-at authority: a contract whose checks all carry an authority qualifier (`@answer:`, `@field=<name>:`,
`@grounded:`; contracts/README.md) and all passed publishes the weakest of them as
`results[].acceptance_authority` (`answer`, `field` or `grounded`), and the delegation-log row carries the same
key. It is absent for every other result (an unqualified check, a failure, an unknown), so a caller reads its
presence as "every check of this contract was backed by at least this authority" and its absence as no claim.

Experiment support (0.79.0–0.81.0): setting `OFFLOAD_DELEGATE_ARM=<label>` tags every
delegation-log line **and every ledger row** the process records with `arm:<label>`, so an
experiment's rows separate cleanly from ordinary traffic after the fact. Each served run's
result also carries A1 config pins — `harness_version`/`harness_build_sha256` (which binary
constructed the requests) and `seat_config_sha256`/`seat_config_basis` (what the seat's
llama-server actually served, hashed from live `/props`) — and the log line adds
`delegator_version`/`delegator_build_sha256` for the acceptance/placement side. Paired
cross-seat comparisons must refuse rows whose pins differ or are absent; absent means
*unknown* (pre-0.81, or the seat was gone by probe time), never "same config".
A node that answers a dispatch 503 or 429 for capacity ("queue full") is a place in line, not a
failed subtask: the delegator outwaits it and re-places the subtask, and the attempt's corpus line
carries `refusal: {status, retry_after_sec}`. `rig` and the fleet page read that line as a
refusal, not a failure; the subtask's own outcome is the line that landed, or, when nothing took it,
a terminal line under a job id of its own (a capacity defer, or the "placement refused" failure).

| Failure | Fix |
|---|---|
| `403 fleet lanes require fleet_auth_token on a non-loopback listener` | The node is bound beyond loopback with no token, so it refuses every lane (a current `fleet-serve` will not start in that state and says so, naming the config file it read: `refusing to serve on ...: ... fleet_auth_token is not set (the config this node read is ...)`). Set `fleet_auth_token` (same value) in that file and on the delegators, and restart `fleet-serve`. |
| `401 unauthorized` on dispatch or poll | Token mismatch between delegator and worker configs. `local-offload doctor` shows it before a call is spent: its `fleet token` rows ask each reachable node whether it accepts this box's token (`OK`, `MISMATCH` = the node has another token, `NO-TOKEN` = the node listens beyond loopback with none, `UNKNOWN`, `UNCHECKED` = refused or unreachable, not asked), one row per `delegate_remotes` slot, never printing a token; a box with no `fleet_auth_token` prints one `NOT SET` line instead. `/fleet/health` is readable without the token (it reads the bearer to decide which private fields to leave out, and a wrong one leaves one line in the node's journal), so every other row reads healthy either way. A media call that gets the `401` is a `config` defer on the delegator, and its node view of this node loses the weight-file rows and the lease reason until the token matches. |
| `agent delegation is disabled on this box` | Set `"agent_delegation_enabled": true` in the **delegator's** config. |
| Everything places local although a remote exists | Usually correct — idle-local always wins. Force `--route remote --pin-reason operator` to surface the gate's verdict (without a reason `remote` is a hint and the idle local seat takes what no remote can): the defer reason now names the actual cause — no remotes configured, every remote failing its health probe (each error quoted), or a healthy remote failing the gate (`agent_enabled` + `agent_seat_resident` + `output_schema` + the ctx arithmetic above). |
| `remote "…": hostname … not allowed` | Non-tailnet URL. Loopback, a tailnet CGNAT-range address, a dotless MagicDNS name, or a hostname under a tailnet zone you listed only: `tailnet_suffix` for your own tailnet, `tailnet_suffixes` for another tailnet that shared a node in (it names that node under the SHARER's zone, ADR 0074). `doctor` prints the zones it read. |
| failed `queue deadline after …: the node accepted the job but never started it` | The node admitted the job but never gave it an execution slot — every poll said `accepted`. It is **saturated**, not broken. Check `jobs_running` / `jobs_queued` / `max_concurrent_jobs` on that node's `/fleet/health`; raise `fleet_max_concurrent_jobs` only if the seat serves that many requests at once (`doctor`'s `capacity:` row compares the cap with the slots in the live serving config; a single-slot tier seeds 1), or spread the fan-out across more nodes. Queued time is credited back to the budget, so this only fires after a real wait, bounded by the node's own ETA (`clamp(1.5 x etaStart + 30 s, 60 s, timeout_sec + grace)`; `min(timeout_sec + grace, 5 min)` for a node that publishes no ETA — ADR 0063). A node whose ETA to start a job already exceeds the wait the contract will give it is not sent the job at all: it is held out and re-read every few seconds, and the placement reason names it as `backlog (…)` with its arithmetic. When the message carries `the queue budget was derived from the placement snapshot`, the node's health could not be read again once the job was seen queued, so the budget was never re-checked: look at why that node stopped answering health. A job the node took back when asked is re-placed on another node and never surfaces as this failure; one whose text ends `; withdraw not confirmed: <why>` is a job the delegator asked the node to take back (ADR 0064) and the node did not, so it may still start later for nobody: `HTTP 405` is a node that predates the route (upgrade it), `HTTP 401` is a `fleet_auth_token` mismatch, `no answer` is a node that sat on the request. |
| `503 queue full (… limit N)` | `fleet_max_queue_depth` reached — default is now `2x fleet_max_concurrent_jobs` (8 with the default 4 workers, 2 with a single-slot tier's one), not a flat 32, so a busier node hits this sooner than it used to. The node publishes a `Retry-After` header sized from its own `recent_agent_wall_sec` (bounded `[5, 300]`, worded so you can tell a real estimate from the flat 30s default or a clamped-high one), and health separately publishes the same formula's RAW, unbounded value as `queue_wait_estimate_sec`. The delegator consumes both (ADR 0063): a 503 re-places the subtask on a node with room at once, its `Retry-After` becomes that node's cooldown for the capacity wait, and `queue_wait_estimate_sec` sizes the queue budget and holds out a node that cannot start the job in time. Raise `fleet_max_queue_depth` only if the box should hold a deeper backlog. Never size your own retry from `seat_rate.min_turn_sec`; that is a per-seat retry floor, not a queue-depth signal. |
| deferred `poll deadline after …: node accepted the job but did not reach a terminal state` | The node acked, STARTED the job, and outran `timeout_sec` + 60 s grace. Check the worker's serve log; the job id in `delegation-log/` reconciles it. A quoted "last poll error" means the node was also answering badly (5xx) — fix that first. A `(+… credited back for time queued …)` clause means part of the wall clock was backlog wait, which was *not* charged to the budget. |
| failed `poll deadline after …: node never answered` | The node died or became unreachable after acking: nothing came back at all, so nothing is claimed on its behalf. The quoted last error (dial refused, dropped connection) is the lead. |
| deferred `output failed schema: …` | The seat could not produce the shape. Check the schema stays inside the supported subset — a `properties` map of string / number / integer / boolean / `enum` fields, lists of strings, and objects and lists of objects nested to any depth; a type list, `anyOf`, `oneOf` or `$ref` reads as a string, and a list of numbers or booleans is written as quoted strings and converted back. A schema inside the subset that still defers is a seat that cannot hold it: read the attempt notes in the reason, and flatten it. |
| deferred `structured re-pack unreachable: …` | Not a schema problem: the worker's llama-swap could not be reached for the final grammar completion. Restart/check the worker's endpoint. Since register C-66 (PR-4) the delegator re-packs the finished answer before it reports this (`repack_note: rescued on …`), on its own seat when that is resident, else on the node that wrote the answer, else on its own seat warmed first. For THIS defer the node rung is skipped: that node's seat is the one that just stalled or could not be reached. You only see the defer when that rescue failed too. |
| deferred `seat warm-up failed: …` | Only with `agent_warm_failure_defer: true`. The seat's process did not start: llama-swap answered the warm-up with a server error that was not one of its busy shapes (429, 503 `process is not ready`, a 500 of its own, a health-check timeout, an empty 502), `/running` listed no row for the seat and nothing else was mid-swap. Read the seat's own log (the engine's exit at start), not the contract. The run was deferred before its wall, and the delegator already gave the contract one retry on another node; you see this defer when there was none to take it or it failed too. With the flag off (the default) `admission_note` says `this run would have deferred at once` and the run proceeds into its wall. |

### Busy-hour cascade failover (`cascade_remote_lanes`)

The daily lane — the `offload_summarize` / `offload_classify` / `offload_extract` /
`offload_triage` calls a session fires dozens of times a week — can fail over **per call**
to another node while this box's GPU is held by a render. Opt in on the box whose cascade
should fail over, in its `~/.local-offload/config.json`:

```json
{
  "cascade_remote_lanes": { "offload-e4b": "http://<node-b>:18811" }
}
```

Key = the exact model id or llama-swap alias a cascade call names; value = a tailnet base URL
that **serves that same model**. The value may be either shape, and the lane works out which by
probing it — you never declare it:

| lane base | when to use it | residency read from | the call rides |
|---|---|---|---|
| `http://<node>:18811` (a **fleet node**) | the normal case on this fleet | `GET /fleet/health` → `served_models` | `POST /fleet/chat` with `fleet_auth_token` |
| `http://<node>:11436` (a **llama-swap**) | only where llama-swap is reachable from this box | `GET /v1/models` | `/v1/chat/completions`, no credential |

**Use the `:18811` node form on this fleet.** <node-c> and <node-a> bind llama-swap to
`127.0.0.1:11436` and nothing else, so the `:11436` form is unreachable from <node-b> and the
lane will never engage; binding llama-swap to the tailnet instead would be a new unauthenticated
listener, which is why the node proxies the call behind its own bearer gate (the same door the
vision lane uses). The node must be running a harness with the chat lane — check
`curl http://<node-b>:18811/fleet/health` for `"chat_lane": true` and for the model in
`served_models` — and **this box's `fleet_auth_token` must match the node's**, or the lane
fails closed to local (one log line per window) rather than collecting 401s.

Semantics, in order:

- A `seat_endpoints` static pin on the same model always wins — a pinned seat is *always*
  remote; a lane is *busy-hours only*.
- A lane is taken only when this box would make the call **wait** — either the machine-wide GPU
  lease is held, or (register C-41) the local llama-swap holds ANOTHER model that is `starting`
  or is `ready` with at least one request in flight, so a swap to the cascade tier would queue
  behind it — **and** a roster probe (alias-aware, cached 30 s per lane, fail-closed) confirms
  the lane serves the model. Idle GPU with an idle (or absent) local seat, a loaded seat that is
  the requested model itself, a probe failure, or a roster miss → the call stays local, silently
  and safely. The local busy reading is cached 5 s.
- **Quality-identical by construction**: the lane must serve the SAME model — the failover
  never changes *which* model answers, only *where*. Never point a lane at a smaller tier.
- Every reroute writes one serve-log line naming the reason: `cascade remote lane: <model> ->
  <base> (local GPU lease held)` or `… (local seat busy: qwen3.8-27b-vllm, 2 in flight)`. No
  lines while the box is busy = the lane never engaged — check that the lane actually serves the
  alias, at whichever door the base is: `curl http://<node-b>:18811/fleet/health` (look for
  `chat_lane` and `served_models`) for a node base, `curl http://<node-b>:11436/v1/models` for a
  llama-swap base. A failed probe of either side logs its own line once per window, and so does a
  node that advertises no `chat_lane` or that this box has no `fleet_auth_token` for.
- **A dead lane base is cheap, and only for itself.** A base that does not answer the health GET
  at all (no dial, no route, a timeout, a reset, a body that stalls) is not asked for a roster
  either, so it costs ONE 5 s probe per 30 s window, not two; the probe runs outside the lane
  cache's lock with one probe in flight per base, so calls that need another base never wait on
  it. The log line is `nothing answered <base>/fleet/health (<why>) ... roster probe is skipped`.

Lane URLs pass the same tailnet guard as everything else here (loopback, `100.64.0.0/10`,
dotless MagicDNS, or a tailnet zone you listed in `tailnet_suffix` / `tailnet_suffixes`;
validated at config load naming the key, and again at every dial).

### A cascade call never evicts a loaded vLLM seat (`cascade_seat_guard`)

The lane rule above does not fire for a loaded seat with nothing in flight — llama-swap swaps an
idle model out at once, so it is not "busy". On a box whose cascade rungs share an exclusive
matrix set with a vLLM seat, that swap IS the damage: the reference box's llama-swap log showed
`model=gemma-4-e4b set=interactive … evict=[qwen3.8-27b-vllm-3card]`. The session using the seat
paid a 3–5 minute cold load and lost the seat's whole prefix cache.

The seat guard closes it. It is **on by default** and inert on a box that declares no
`vllm_seats`. While a seat `vllm_seats` names is loaded (`ready` or `starting` in `/running`),
every rung of a summarize / classify / extract / triage call is checked against llama-swap's own
routing. The routing is read from `serving_config_path` (matrix sets and evict costs, or groups)
and solved the way llama-swap solves it. A rung whose load would unload the seat takes the first
non-evicting door:

1. **its `cascade_remote_lanes` lane**, when the lane serves the SAME model (the rule above);
2. otherwise **the loaded seat itself**, as the rung (vLLM seats are cascade rungs since D-129:
   `structured_outputs`, non-thinking render). A busy seat queues the call, and nothing is refused;
3. the terminal reasoning tier, when neither door applies, runs its attempt on the loaded seat.
   It keeps the reasoning budget and is marked `reasoning` in the ledger. It uses the seat's own
   `structured_outputs` in place of the think-wrapped grammar, which a vLLM seat cannot take. It is
   skipped when the seat has already answered the call.

With no vLLM seat loaded the cascade is exactly what it was. The serve log carries one line per
call that the guard touched, in llama-swap's own shape, so the two logs can be read side by side:

```
cascade seat guard: model=gemma-4-e4b set=interactive evict=[qwen3.8-27b-vllm-3card] target=[bge-reranker-v2-m3 embeddinggemma gemma-4-e4b] cost=1 -> served by the loaded seat qwen3.8-27b-vllm-3card (no lane serves gemma-4-e4b, gemma-4-12b, reasoning gemma-4-26b)
cascade seat guard: model=gemma-4-e4b … -> served off this box by the cascade lane that serves each rung (…)
cascade remote lane: gemma-4-e4b -> http://<node-b>:18811/fleet/chat (cascade seat guard: model=gemma-4-e4b …)
```

On a genuine cost tie, llama-swap chooses one set by definition order and logs that set. The
guard unions the tied sets on purpose, so there its `set=` / `target=` / `evict=` can differ from
llama-swap's line (`evict=` only ever lists more). The reference config has one such tie:
`qwen3.8-27b` is in both `interactive` and `qwen_aux`.

The guard needs `serving_config_path` set. Without the file (unset, missing, or unparseable),
co-residency is unknown, and while a seat is loaded every other model is treated as evicting it.
When co-residency is unknown and several seats are loaded, the log line names all of them.

A `/running` that cannot be read is also unknown. So is any non-2xx answer, and any answer without
a `running` list, even when its JSON body looks like an empty list. The last reading that saw the
seat loaded keeps protecting it for 300 s.

**Known residual risk.** Past those 300 s, and before any good reading, the guard cannot name a seat.
The call rides its lane when one serves it; otherwise **the configured rung runs unguarded**. If a
seat really is loaded at that moment, that rung can evict it. This is deliberate: the guard never
refuses a call, and substituting a seat it cannot see would ask an unreadable llama-swap to
cold-load the agent seat. The line `-> the configured rung runs: … no loaded seat is known to
substitute` marks every such call.

**A lane that disappears between the plan and the send.** The guard reads lane residency from the
lane's 30 s cache, and the send checks it again. If the lane stopped serving the rung in between,
the call is refused before any request is made. The client logs `… was sent off this box … but its
cascade lane no longer serves it`. The rung then takes the loaded seat instead (`… lane gone at
send -> served by the loaded seat …`). With no seat known it runs as configured, the residual above.
It never falls through to this box silently.

**A model that appears in no matrix set runs alone.** That is llama-swap's rule, not the guard's.
Such a model evicts every loaded model, including the residents, so the guard protects the seat from
it and warns once per model: `… appears in no matrix set of …`. For a cascade rung this is almost
always a config gap. Add the rung to a set.

To switch it off, set `"cascade_seat_guard": false`. It is worth doing on a box whose vLLM seat is
much slower than its cascade rungs, where a cold load costs less than serving Tier-1 calls from
the seat.

---

## 5. Add / replace a model in llama-swap.yaml

1. Download the GGUF into `$OFFLOAD_HOME\models\`.
2. Edit `$OFFLOAD_HOME\llama-swap.yaml`. Copy an existing `models:` entry, set `-m` to the new file,
   keep the `${common}` macro (it carries the grammar-reliable flags), and give it an alias. To make
   it swap-exclusive with the others, add the alias to `groups.offload-family.members`.
3. **Use forward slashes** in paths inside the yaml (llama-swap on Windows chokes on backslash
   escapes; the installer already renders forward slashes).
4. Point the harness at it by editing the matching key in `~/.local-offload/config.json` (e.g.
   `escalation_model`).
5. Restart llama-swap, then verify ✅:

```powershell
local-offload --config "$HOME\.local-offload\config.json" doctor   # the new alias must show OK
```

| Failure | Fix |
|---|---|
| alias `FAIL — not in roster` | Restart llama-swap so it re-reads the yaml; confirm the name matches a model id or one of its `aliases` (the check reads both). |
| model won't load / device-lost | VRAM too small — lower `-ngl` or the window, or drop the model from the tier (the 26B: `moe_26b` `drop`); the experts stay on the card, because RAM is overflow only (no `--cpu-moe`). See §6. |

---

## 6. Diagnose

| Symptom | Cause → fix |
|---|---|
| **Everything defers** | Endpoint unreachable. Run `local-offload doctor`: `health: DOWN` → start llama-swap; an alias `FAIL` → fix the yaml. A defer on hard/over-long input is by design. |
| **Port busy** (`18801`/`18802` in selftest, or `:11436`/`:18800`) | `Get-NetTCPConnection -LocalPort <p> -State Listen` to find the owner; kill a leaked `llama-server.exe`/`local-agent.exe`, or reboot after asking. |
| **Model load fails** (OOM at load) | VRAM ceiling. For the 26B MoE: drop it (`moe_26b` `drop`, and change the `resident_tier` and agent seat if the tier's resident is the 26B) or declare a *measured* partial spill (`n_cpu_moe` with `n_cpu_moe_max`) in the profile table; never add the every-expert `--cpu-moe` to its `cmd` (INV-1: the cards do the inference, RAM is overflow only, and `audit-yaml` fails the file). For dense tiers lower `-ngl`. selftest does not remediate it: the receipt's failed `tiers[]` row carries the advice in `detail` and `llama-swap.yaml` is never edited. |
| **Vulkan driver crash** (device-lost mid-generation, AMD) | The deep-context crash class (llama.cpp #17432). Re-run `selftest.ps1` — its depth-7000 **canary** reproduces it and the receipt names your GPU + driver. Remediation: **update the AMD Adrenalin driver** (ask the human first), keep it current; interim: lower `-ngl` or the window. If it persists on a big MoE, add `nodes_per_submit: 1` to the model entry. |
| **Context overflow** (agent HTTP 400 / `context` error) | The transcript exceeded the ~32K window (see §4). Narrow the prompt to edit+upload shape, trim the toolset, or lower `--max-steps`. |
| **GitHub tool refusals** | Token/scope. Ensure `$GITHUB_TOKEN` is set (least-privilege — only the scopes the task needs) and `$GITHUB_REPO=owner/name`. Put both in a **gitignored** `~/.local-agent-github.env`, never in the repo or a logged command line. |
| **`doctor` exits non-zero with a `config findings` section** | One `FAIL` row per value that loads and then cannot do what it says: a `delegate_remotes` base not on the fleet node port `:18811`, one that is loopback (a delegate REMOTE is another box), one carrying a `/v1` suffix, or one the tailnet guard refuses (named by slot, `delegate_remotes[i] is refused by the tailnet guard`: the agent lane would fail every call on it and the single-shot lanes skip it); a `cascade_remote_lanes` base on neither shape a lane can be (a fleet node `:18811`, or a llama-swap on this box’s own `endpoint` port) or carrying `/v1`; `gpu_wait_ms` more than 3x `vision_gpu_wait_sec` (C-33); a `fleet_accelerators` entry naming a local-only device (`hailo-8l`, E-08: no node ever advertises it, so the call can only defer); one row per retired key still in the file. Nothing is refused for these — fix the file, or accept the row. |
| **A config file FAILS VALIDATION naming a key** | A configured HTTP base is refused by key and value when it is not a usable URL (a parse error, a scheme that is not http/https, or no host — `${NODE_A_HOST}:18811` and `http://node-a:$PORT` fail to parse, and `node-a:18811` parses as *scheme* `node-a` with no host), or when its port is `:0`/`:9` (the OS’s "any free port" and IANA discard; compared numerically, so `:09` is refused too). Applies to `endpoint`, `delegate_remotes[]`, `cascade_remote_lanes{}`, `seat_endpoints{}`, `fleet_queue_holder`, `tts_endpoint`, `nim_endpoint`, `hailo_endpoint`, `coral_endpoint`, `pair_workloads_endpoint`. An EMPTY value is fine; a **loopback** base on an unusual port is still allowed — that is INV-10’s bench twin. |
| **What a refused config does, per entry point** | `fleet-serve` **refuses to start** (non-zero exit): a node that cannot prove its own config must not accept other boxes’ dispatches. `mcp` **starts and says so** — `offload_status` carries `config_error` as its first key and every other tool returns `{"deferred":true,"reason":"config invalid: …"}`; it must never vanish, because an MCP server that exits takes every tool out of every session silently. `doctor` prints the refusal verbatim as its first `FAIL` row and exits non-zero. One-shot CLI verbs warn and proceed. The stderr warning and doctor’s `config:` line no longer say "BUILT-IN DEFAULTS" — a file that failed validation is still the file the process is running on. A file that could not be **parsed** is the opposite case, and says so: nothing from it is in effect, the process runs on BUILT-IN DEFAULTS, and the warning, the `config:` line and the error text (`not valid JSON, so NOTHING from the file is in effect`) all state that. A leading UTF-8 byte-order mark (Windows PowerShell 5.1 `Set-Content`/`Out-File -Encoding UTF8`) is stripped before decoding, so such a file loads as written. |
| **Empty / truncated model output** | Serving with reasoning on, or `--max-tokens` too low. Confirm `--reasoning off` on the server; raise `--max-tokens`. Never pass `--json-schema`/`response_format` — they crash the model. |

---

## 7. Update the stack

```bash
git pull                                   # get the latest harness + scripts
go build ./...                             # rebuild — must stay green
go build -o "$OFFLOAD_HOME/harness/local-offload.exe" .
go build -o "$OFFLOAD_HOME/harness/local-agent.exe" ./cmd/local-agent
```
```powershell
pwsh -NoProfile -File setup\install.ps1    # resolves the newest complete builds (idempotent; a component already at the newest SKIPs)
pwsh -NoProfile -File setup\selftest.ps1   # re-verify: expect verdict pass|warn
```

A newer published build forces a re-download of exactly that component (the installed generation in `installed.json`
is older than what the publisher serves, and the previous generation is kept for rollback) while everything else SKIPs;
a newer build already on the box is kept, never downgraded. After any update,
re-run selftest and confirm the receipt `verdict` is `pass` (or `warn` for known-soft signals).
