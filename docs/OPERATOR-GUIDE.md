# Operator Guide — running, driving & diagnosing the stack

Task-oriented walkthroughs for an agent (or human) operating an installed offload-harness stack.
Each task is **goal → commands → expected output → failure table**. Commands are given for
**PowerShell** (native Windows install) and **bash** where applicable. `$OFFLOAD_HOME` defaults to
`$HOME\offload-stack`; the harness config lives at `~/.local-offload/config.json`.

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
endpoint:   http://127.0.0.1:11436/v1/chat/completions
model:      offload-e4b
health:     OK
model:             OK    offload-e4b
triage_model:      OK    gemma4-e2b
escalation_model:  OK    gemma4-26b-a4b
...
```

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
| alias `FAIL — not in the live roster` | The yaml doesn't serve that alias, or a model file is missing. Check `llama-swap.yaml`. Vision/STT aliases are legitimately absent on a grunt-work-only install. |

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

| Failure | Fix |
|---|---|
| every call `deferred:true` | Run `doctor`. Usually the endpoint is down or unreachable. A defer on genuinely hard/over-long input is by design. |
| `cache unavailable (held by the MCP server?)` | Expected — the bbolt cache is single-writer. The CLI continues cache-less; the ledger still appends. |

---

## 4. Drive the coding agent

The agent plans with a local model and acts through tools confined to a workspace. Build it once:
`go build -o local-agent ./cmd/local-agent` (or use `$OFFLOAD_HOME\harness\local-agent.exe`).

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
# [local-agent] OpenAI server on http://127.0.0.1:18808  (model="offload-e4b")
curl -s http://127.0.0.1:18808/v1/models
# {"data":[{"id":"offload-e4b","object":"model","owned_by":"local-offload"}],"object":"list"}
```

Each `/v1/chat/completions` POST runs the **full agent loop** over the last user message and returns
the final answer. The endpoint is **unauthenticated** — keep it loopback-only.

### Capability flags (all OFF by default)

| Flag | Grants | Notes |
|---|---|---|
| `--allow-write` | `write_file` (+ `delete`/`edit` only with the next two) | Worktree-scoped, policy-gated. |
| `--allow-overwrite` | overwrite existing files + `edit_file` | requires `--allow-write`. |
| `--allow-delete` | `delete_file` | requires `--allow-write`. |
| `--allow-fetch` | `web_fetch` | egress-allowlist gated; add hosts with repeatable `--egress-host` (bare host or `*.host`). Deny-all by default. |
| `--allow-search` | `web_search` (DuckDuckGo, keyless) | auto-allowlists the search host. |
| `--allow-shell` | `run_shell` in the OS sandbox | **Linux only**; no network, FS-confined, syscall-limited. |
| `--allow-github` | `github_api` / `create_repo` / `upload_file` | token from `$GITHUB_TOKEN`, repo from `$GITHUB_REPO`. Use a least-privilege token. |
| `--listen-trusted-network` | bind `--serve` beyond loopback | prints a loud warning; only on a trusted LAN. |

### Circuit breakers & budget

- `--max-steps` (default 12) — hard step budget, owned in code.
- `--max-same-tool` (default 3) — cap on calls to any one tool per run; the breaker for a model that
  loops (e.g. repeated reworded `web_search`). Negative disables; 0 → built-in default.
- `--max-tokens` (default 4096) — planner tokens per completion. Must be large enough for the biggest
  tool-call argument (e.g. a whole file's content) or the model's JSON gets cut off mid-string and
  the call fails. 4096 is the tested value; do not lower it for write-heavy runs.

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
   tool called more than `--max-same-tool` (3) times per run, so a bad prompt costs one failed run,
   never the installation.

| Failure | Fix |
|---|---|
| `refusing to bind --listen` | You passed a non-loopback address. Use `127.0.0.1`, or `--listen-trusted-network` (only if authorized). |
| agent stops with `stop=step-cap` / loops | Raise `--max-steps`, or the model is stuck — lower `--max-same-tool`, narrow the prompt (edit+upload shape). |
| GitHub tool refuses | `$GITHUB_TOKEN` unset or under-scoped, or `$GITHUB_REPO` unset. See §6. |

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
| alias `FAIL — not in roster` | Restart llama-swap so it re-reads the yaml; confirm the alias name matches exactly. |
| model won't load / device-lost | VRAM too small — add `--cpu-moe` (MoE) or lower `-ngl`. See §6. |

---

## 6. Diagnose

| Symptom | Cause → fix |
|---|---|
| **Everything defers** | Endpoint unreachable. Run `local-offload doctor`: `health: DOWN` → start llama-swap; an alias `FAIL` → fix the yaml. A defer on hard/over-long input is by design. |
| **Port busy** (`18801`/`18802` in selftest, or `:11436`/`:18800`) | `Get-NetTCPConnection -LocalPort <p> -State Listen` to find the owner; kill a leaked `llama-server.exe`/`local-agent.exe`, or reboot after asking. |
| **Model load fails** (OOM at load) | VRAM ceiling. For the 26B MoE add `--cpu-moe` (experts stay in RAM) to its `cmd`; for dense tiers lower `-ngl`. On AMD, selftest auto-remediates 26B with `--cpu-moe`. |
| **Vulkan driver crash** (device-lost mid-generation, AMD) | The deep-context crash class (llama.cpp #17432). Re-run `selftest.ps1` — its depth-7000 **canary** reproduces it and the receipt names your GPU + driver. Remediation: **update the AMD Adrenalin driver** (ask the human first), keep it current; interim: `--cpu-moe` / lower `-ngl`. If it persists on a big MoE, add `nodes_per_submit: 1` to the model entry. |
| **Context overflow** (agent HTTP 400 / `context` error) | The transcript exceeded the ~32K window (see §4). Narrow the prompt to edit+upload shape, trim the toolset, or lower `--max-steps`. |
| **GitHub tool refusals** | Token/scope. Ensure `$GITHUB_TOKEN` is set (least-privilege — only the scopes the task needs) and `$GITHUB_REPO=owner/name`. Put both in a **gitignored** `~/.local-agent-github.env`, never in the repo or a logged command line. |
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
pwsh -NoProfile -File setup\install.ps1    # picks up any bumped pins (idempotent; unchanged components SKIP)
pwsh -NoProfile -File setup\selftest.ps1   # re-verify: expect verdict pass|warn
```

A bumped pin in `install.ps1`'s `$PINNED` table forces a re-download of exactly that component
(the `installed.json` version check fails for it) while everything else SKIPs. After any update,
re-run selftest and confirm the receipt `verdict` is `pass` (or `warn` for known-soft signals).
