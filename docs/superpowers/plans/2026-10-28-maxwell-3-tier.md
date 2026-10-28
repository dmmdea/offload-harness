# maxwell-3 tier Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. No test-writing steps: the check of every task is one real run, read back (AGENTS.md, Testing rule).

**Goal:** A 3 GB NVIDIA card classifies as its own tier, `maxwell-3`, and renders only seats measured to fit on the reference box `<node-g>`, each the best quality that fits.

**Architecture:** One new VRAM band in the classifier and its PowerShell twin; one new inverted-polarity profile flag (`omit_e4b`) so the renderer and the installer drop the E4B workhorse on a tier that cannot load it; one `profiles.json` entry whose values are PROJECTED until the box's bakes replace them; generated tier docs; refreshed E2B/E4B pins. Measurements on the box decide the agent, vision, STT and image seats and are recorded in the profile notes and the benchmarks folder.

**Tech Stack:** Go 1.26 (harness, `internal/hwdetect`, `internal/servingtmpl`, `install_render.go`), PowerShell 5.1/7 (`setup/detect.ps1`, `setup/install.ps1`), llama.cpp `b11490` win-cuda-12.4 (pin), llama-swap v236.

**Spec:** `docs/specs/2026-10-28-maxwell-3-tier-design.md`

## Global Constraints

- Band: NVIDIA, `0 < VRAMGb < 4`, decided in the default (non-Blackwell, non-Volta) branch BEFORE the `ampere-6` fall-through; arch label `maxwell` never decides the band.
- `omit_e4b` is inverted-polarity: absent/false renders exactly today's output; the goldens change only where the token literal is substituted.
- No real machine, person or path names in any tracked file: the box is `<node-g>`; the leak gate scans every file.
- No new tests. Existing tables that enumerate tiers gain a row; red tests are fixed by rows, never by edits to the assertion.
- Every pin is size + LFS sha256 read from the Hugging Face tree API; no model bytes are downloaded to pin.
- Every seat ttl 300; `fleet_max_concurrent_jobs: 1`; `escalation_model` and `reasoning_model` blank.

---

### Task 1: Classification band and arch label

**Files:**
- Modify: `internal/hwdetect/arch.go` (insert after line 38), `internal/hwdetect/classify.go:25-26,180-189`, `internal/hwdetect/classify_test.go:19-40,47-82`
- Modify: `setup/detect.ps1:6,49,161,240-245,313-316,349-359,412`
- Modify: `internal/fleetnode/gpuinfo.go:246-267,276-289`, `internal/fleetnode/gpuinfo_test.go:12-24,33-47`
- Modify: `setup/tests/install-cuda-build.test.ps1:76`

**Interfaces:**
- Produces: profile id `"maxwell-3"` from `hwdetect.Classify` and `detect.ps1 Get-Profile`; arch value `"maxwell"` from `ArchFromName` / `Get-GpuArch`; `fleetnode.VendorArchFromProfile("maxwell-3") == ("nvidia","maxwell")`.

- [ ] **Step 1: Add the arch rule** in `arch.go` after the ampere rules and before the V100 rule (ordering: the `\b7\d{2}M\b` RDNA rule must not see "750M"): regexes `(?i)\bGTX\s*(9[5-8]0|965|750|745)(\s*Ti)?M?\b`, `(?i)\bGTX\s*8[3-6]0M\b`, `(?i)\bQuadro\s+M\d{3,4}M?\b` → `"maxwell"`. Mirror in `detect.ps1 Get-GpuArch` after line 49. Update the value lists in the comments (`classify.go:25-26`, `detect.ps1:6,161`).
- [ ] **Step 2: Add the band** in `classifyProfile` default NVIDIA branch: `case f.VRAMGb > 0 && f.VRAMGb < 4: return band("maxwell-3", "nvidia "+orUnknown(arch)+", VRAM < 4 GB (3 GB band)")` placed before the `ampere-6` return; mirror between `detect.ps1:243` and `244`.
- [ ] **Step 3: Rows** — `classify_test.go`: arch rows ("NVIDIA GeForce GTX 970M" → maxwell, "NVIDIA GeForce GTX 750M" → maxwell, "NVIDIA GeForce GTX 980 Ti" → maxwell) and band rows (maxwell 3 → maxwell-3; maxwell 3.9 → maxwell-3; maxwell 4 → ampere-6; other 0 → ampere-6 unchanged; maxwell 6 → ampere-6). `detect.ps1` self-test: Assert-Arch rows after 316, Assert-Profile '970M 3GB' row after 358, "recognised maxwell does NOT warn" after 412. `gpuinfo.go`: `maxwell-` prefix in both switches; rows in `gpuinfo_test.go`. `install-cuda-build.test.ps1:76`: add `'maxwell-3'`.
- [ ] **Step 4: Run it once for real**
  Run: `go test ./internal/hwdetect/ ./internal/fleetnode/ && pwsh -NoProfile -File setup/detect.tests.ps1`
  Expected: PASS for both packages; the detect self-test prints its ok lines including the three new rows.
- [ ] **Step 5: Commit** `feat(hwdetect): maxwell-3 band for <4 GB NVIDIA cards + maxwell arch label`

### Task 2: `omit_e4b` profile flag

**Files:**
- Modify: `setup/templates/profiles.json` (`_fields`), `install_render.go:81-95,676,766`, `internal/servingtmpl/servingtmpl.go` (Params ~64, drop ~306-311, token map ~562-575, validate ~674), `internal/servingtmpl/provenance.go:157,193,210`, `altcpu.go:32`
- Modify: the eight templates' set expressions: `llama-swap.win-cuda.yaml:249`, `linux-cuda:321`, `win-vulkan:163`, `linux-vulkan:224`, `win-cpu:90`, `linux-cpu:99` (`e4b | ` → `__E4B_LEAD__`), `win-cuda-resident:109`, `win-dual-cuda:121` (`& e4b` → `__E4B_AND__`)
- Modify: `setup/install.ps1:1581` (model key list), `Resolve-ProfileParams` (read the flag), `seat_closure_test.go:98-131` (`renderParams` helper gains the field)
- Re-pin: `internal/servingtmpl/testdata/eg2bin-unset-render.golden` via `OFFLOAD_UPDATE_GOLDEN=1`

**Interfaces:**
- Produces: `servingProfile.OmitE4B bool \`json:"omit_e4b"\``, `servingtmpl.Params.OmitE4B bool`, `ParamsBasis.OmitE4B`, tokens `__E4B_LEAD__` (renders `e4b | ` or empty) and `__E4B_AND__` (renders ` & e4b` or empty); `dropE4B` removes the `offload-e4b` entry and the `e4b` var when set.

- [ ] **Step 1: Document the key** in `_fields`: `omit_e4b`: "omits the offload-e4b entry, its matrix var and set membership, and the installer's model-e4b download; for a card the E4B cannot load (3,100 MiB at 16K on a 6 GB card). Absent = false = render as before. Requires resident_tier gemma4-e2b."
- [ ] **Step 2: Renderer** — add the field through `servingProfile` → `Params` → `ParamsBasis`; implement `dropE4B(yaml) string` beside `dropQ354B`; substitute both tokens in the token map; `validate` refuses `OmitE4B && ResidentTier == offload-e4b`-equivalent (the seed names E2B) and refuses a template that carries no token when the flag is set (same shape as the seats directive refusal).
- [ ] **Step 3: Templates** — replace the literal `e4b | ` / ` & e4b` with the tokens in all eight files; keep a one-line comment at each.
- [ ] **Step 4: Installer** — `Resolve-ProfileParams` reads `omit_e4b` (strict bool); Step 5 starts `$modelKeys` from `@('model-embed')` plus `model-e4b` only when not omitted; the Step 9 hint names the resident entry from `resident_tier`.
- [ ] **Step 5: Run it once for real**
  Run: `OFFLOAD_UPDATE_GOLDEN=1 go test ./internal/servingtmpl/ && go test ./internal/servingtmpl/ . && go run . install render --os windows --profile ampere-6 --llama-bin X --models Y --home Z --out /tmp/a6.yaml && grep -c offload-e4b /tmp/a6.yaml`
  Expected: tests PASS; the ampere-6 render still contains `offload-e4b` (unchanged behaviour); a render with a profile JSON that sets `omit_e4b: true` contains no `offload-e4b` and the interactive set reads `(e2b__M26_ALT__…`.
- [ ] **Step 6: Commit** `feat(render): omit_e4b profile flag drops the E4B entry and its download`

### Task 3: `maxwell-3` profile entry, tables, docs

**Files:**
- Modify: `setup/templates/profiles.json` (new entry after `ampere-6`), `vllm_coverage_test.go:41-55`, `projected_agent_seats_test.go:47-97`, `setup/render.tests.ps1` (block modelled on 262-335), `docs/systems/setup-installer.md:66-72`, `setup/SETUP-AGENT.md:136-137`, `docs/OPERATOR-GUIDE.md:1554-1555`, `docs/glossary.md:344-349` (row for `<node-g>`)
- Generate: `docs/tiers/maxwell-3.md`, `docs/tiers/README.md` via `go generate ./...`

**Interfaces:**
- Consumes: Task 1's profile id, Task 2's flag.
- Produces: the profile entry: `backend cuda`, `ctx_size 32768`, `kv_type f16`, `flash_attn on`, `agent_ctx_tokens 32768`, `resident_tier gemma4-e2b`, `omit_e4b true`, `include_26b false`, `moe_26b drop`, `include_embeddinggemma2 false`, `disable_cuda_graphs true`, `agent_budget_policy loop-default-chosen`, `config_seed` {model gemma4-e2b, agent_model (PROJECTED: gemma4-e2b until Task 6), memory_stack [embeddinggemma, bge-reranker-v2-m3], agent_profile research, fleet_max_concurrent_jobs 1, escalation_model "", reasoning_model "", compose_script render/compose-hyperframes.mjs, the five script blanks}, `media_seats` [] until Task 7, `notes` starting "PROJECTED from the <node-g> smoke of 2026-10-28 …".

- [ ] **Step 1: Write the entry** with the values above; `notes` carries the measured smoke-1 numbers (E2B pp/tg, fits, JIT, FA, app-clock pin) and marks every unmeasured value PROJECTED.
- [ ] **Step 2: Rows** — `vllmSeatDebt` gets `maxwell-3` ("3 GB: no vLLM seat; tracked"); `projectedAgentSeats` gets the row; `render.tests.ps1` gets a maxwell-3 block asserting no `offload-e4b`, `gemma4-e2b` present, `--cache-type-k f16`, no `--n-cpu-moe`.
- [ ] **Step 3: Docs** — band row in `setup-installer.md` (and "Sixteen bands"), matrix rows in `SETUP-AGENT.md` and `OPERATOR-GUIDE.md` (fix the stale amd-gcn `gemma4-e2b` cell while there: it is `offload-e4b` at `profiles.json:1863`), glossary row `| maxwell-3 | <node-g>, the 2015 gaming laptop (GTX 970M 3 GB) |`; `go generate ./...`.
- [ ] **Step 4: Run it once for real**
  Run: `go generate ./... && go build ./... && go vet ./... && go test ./... 2>&1 | tail -30 && pwsh -NoProfile -File setup/render.tests.ps1`
  Expected: all PASS; `docs/tiers/maxwell-3.md` exists and `docs_tiers_test` is green; the render test's maxwell-3 block prints ok lines.
- [ ] **Step 5: Commit** `feat(tiers): maxwell-3 profile (projected), tables, generated docs`

### Task 4: Refresh the stale E2B/E4B pins

**Files:**
- Modify: `setup/install.ps1:117-130` (`model-e4b`, `model-e2b`)

- [ ] **Step 1: Set** `model-e2b` size `2620370976` sha `e531007218dfab990486a5de7676a6932d6ea8dea233d1f698d7c21cf8a16889` version `e5310072`; `model-e4b` size `4215695776` sha `df0fd4ee07072c607c29a0a1cb4f98918426cca12f45a2776bdd6ee6d09a4de3` version `df0fd4ee` (both read 2026-10-28 from the HF tree API; the comment records the date).
- [ ] **Step 2: Run it once for real**
  Run: `curl -sIL <both resolve URLs> | grep -i x-linked-size` and `grep -n "sha  = 'e5310072\|sha  = 'df0fd4ee" setup/install.ps1`
  Expected: the two X-Linked-Size headers equal the pinned sizes.
- [ ] **Step 3: Commit** `fix(install): re-pin gemma-4 E2B/E4B GGUFs to the current upstream LFS oids`

### Task 5 (box): engine go/no-go and the quant/speed matrix

**Files:** none in the repo (results land in Task 8). Scripts: `smoke3.ps1` (bench driver, already on the box).

- [ ] **Step 1: VRAM ledger** — `smoke3.ps1 -Llama <b11490> -Tag ledger -Models <E2B>`: record `memory.used` at baseline, embedder resident, E2B at 32K f16; derive the WDDM/context reserve and the embedder's real cost.
- [ ] **Step 2: Matrix on b11490** — E2B UD-Q4_K_XL, MiniCPM5-2B Q4_K_M, Qwen3.5-2B UD-Q4_K_XL, Qwen3.5-4B UD-IQ3_XXS: bench (f16/q8_0 KV, pp512/pp2048/tg128) and server fit at 32K f16; grep `graph splits` and `flash` lines.
- [ ] **Step 3: Matrix on b11946** — the same plus LFM3-1.7B Q8_0/Q6_K/QAD-Q4_0; a crash or `no kernel image` on any LFM3 file drops the family (R-ENGINE).
- [ ] **Step 4: Read back** — a table (model, file MiB, on-card MiB at 32K, pp, tg, graph splits, JSON-schema answer) into `measurements.md`.

### Task 6 (box): agent seat bake and the spill proof

- [ ] **Step 1: Shape B and C** from `Benchmarks and Optimizations/2026-08-17-ampere6-seat-bakeoff/` run through `local-agent`/`agent_run` on each fitting candidate from Task 5 (fresh server per run, temperature 0, research profile), scored with the gold sets and the typed judge.
- [ ] **Step 2: Spill proof** (CPU window announced to the powertune session): `llama-bench -m Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf -ngl 99 -ncmoe 40 -fa 1 -ctk q8_0 -ctv q8_0 -ub 512 -b 512 -p 512 -n 64 -t 4,8 -nopo 0,1 -r 2`, then a server at 16K with one shape-C run; record tg, pp, on-card MiB, host RSS, MemAvailable floor, swap-in.
- [ ] **Step 3: Decide** per the design's R-AGENT: bind `agent_model`; if the spill passes the three gates, add the allow-list row in `ampere6_spill_seat_test.go:104-107`, set `include_qwen36_35b`, `n_cpu_moe_max 40`, `config_seed_ram_low_up.agent_model`, and write the ADR 0080 amendment paragraph.

### Task 7 (box): vision, STT, image smokes

- [ ] **Step 1: Vision** — E2B + `mmproj-gemma-4-E2B-it-Q8_0.gguf` at ctx 8192 f16 FA with the embedder resident; one 1080p screenshot and one receipt via `/v1/chat/completions`; record peak MiB and seconds; LFM3-VL-2B Q6_K + Q8_0 mmproj on b11946 if Task 5 kept the family.
- [ ] **Step 2: STT** — `smoke2.ps1`: whisper-server q5_0 then q8_0 on the SAPI Spanish clip, FA on/off; record MiB, ms, text.
- [ ] **Step 3: Image** — `smoke4.ps1`: sd.cpp Vulkan with sdxl-turbo Q4_0, both device indices; record which index is the 970M, seconds, peak MiB, and look at the PNG.
- [ ] **Step 4: Decide** `media_seats` (vision + stt with their measured ctx/image caps) and the imagegen `config_seed` (engine sdcpp, `sdcpp_extra_args` with the measured `--backend vulkanN`, `--vae-tiling`, steps 4, cfg 1, timeout from the measured seconds x3).

### Task 8: Finalize the profile with measured values

**Files:**
- Modify: `setup/templates/profiles.json` (`maxwell-3`), `docs/specs/2026-10-28-maxwell-3-tier-design.md` (status + measured table), `docs/architecture/decisions/0080-*.md` (amendment, only if Task 6 binds the spill), `ampere6_spill_seat_test.go` (allow-list row, same condition)
- Create: `changes/unreleased/feat-maxwell-3-tier.md` (`### Added - …`, bold-lead bullets What it does / Why / Not changed / Check), the benchmark folder `<Drive>/AI Ecosystem/Ecosystem/Benchmarks and Optimizations/2026-10-28-maxwell3-tier/README.md`
- Generate: `go generate ./...`

- [ ] **Step 1: Replace** every PROJECTED value with its MEASURED one; `notes` records each measurement with its date and the reasons for rejected candidates.
- [ ] **Step 2: Run it once for real**
  Run: `go generate ./... && go test ./... 2>&1 | tail -20 && pwsh -NoProfile -File setup/render.tests.ps1`
  Expected: PASS; the tier page shows the measured rows.
- [ ] **Step 3: Commit** `feat(tiers): maxwell-3 measured on <node-g>; fragment; benchmark record`

### Task 9 (box): install end to end and the real calls

- [ ] **Step 1: Build** `GOOS=windows go build -o local-offload.exe ./cmd/local-offload` from the worktree (VERSION must match the installer's check) and copy it with `setup/` to the box.
- [ ] **Step 2: Install** — `OFFLOAD_HOME=D:\offload-stack OFFLOAD_DATA_HOME=D:\offload-data`: `detect.ps1` (expect `"profile":"maxwell-3"`), `install.ps1` (expect the rendered yaml without `offload-e4b`, no E4B download), copy whisper-server + models into place, `selftest.ps1`, `local-offload doctor`, `local-offload report --out capability-report.md`.
- [ ] **Step 3: Service** — a boot-triggered scheduled task with the hidden launcher starting llama-swap on 11436 with the CUDA cache env; reboot-safe check: `Invoke-RestMethod http://127.0.0.1:11436/v1/models`.
- [ ] **Step 4: Register** the MCP server for the box's Claude Code (`claude mcp add local-offload -s user -- <exe> mcp`), then one real call per bound lane (`offload_summarize`, `agent_run`, `offload_vqa`, `offload_transcribe`, `offload_generate_image` if bound); read each output back.
- [ ] **Step 5: Record** the outputs in the PR body and `measurements.md`; commit the checked-in capability report under `docs/tiers/reports/maxwell-3-node-g.md` (scrubbed).

### Task 10: Ship

- [ ] **Step 1:** `clean-ship`: gate pass (`go build`, `go vet`, CI), PR on the public repo with Intent / Scope / What changed / The real run / Blast radius / Risk (template re-hash: every rendered node reads STALE until re-rendered), `jev-presets.js review-needed` verdict in the body.
- [ ] **Step 2:** Babysit CI; merge when green under the ordered-plan authorization; read the merge back; worktree cleanup in its own call.

Ruling: executor = Native - ten tasks share two interfaces (the profile id and the flag) and half of them are measurements only this session can run on the box - a context compaction mid-run loses nothing because state lives in the plan, the ledger and measurements.md.
