# setup/selftest.ps1 - verification + machine-readable RECEIPT gate for the offload-harness stack.
#
# This is THE product of the installer: a receipt that both an autonomous agent and a skeptical
# human can trust. It stands up a TRANSIENT llama-swap on a test port (18801) against the stack
# already installed in $OFFLOAD_HOME, exercises EACH installed chat tier through the actual
# llama-swap exclusive+swap group (so a second tier forces eviction+cold-load of the first),
# runs a deep-context canary at depth ~7000, smoke-tests the harness + agent, and prints ONE
# receipt JSON as the LAST stdout line. Human-readable progress precedes it.
#
# Honest proof-scope (R4.5): a transient :18801 test proves installation integrity, per-tier
# liveness-through-the-swap-group, and this-GPU+driver canary at depth 7000. It does NOT prove the
# long-running :11436 service behaves identically (different port, no real multi-hour idle-TTL
# eviction, etc.). The receipt's proves/does_not_prove arrays say so; do not overclaim.
#
# Env:  OFFLOAD_HOME (default $HOME\offload-stack)
# Exit: 0 for verdict pass|warn, 1 for verdict fail. Ports 18801/18802 freed on every path.
# PowerShell 5.1 AND pwsh 7 compatible (no ternary, no ?? operator, ASCII only).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# ---------------------------------------------------------------------------
# Config / paths
# ---------------------------------------------------------------------------
if ($env:OFFLOAD_HOME) { $HOME_DIR = $env:OFFLOAD_HOME } else { $HOME_DIR = Join-Path $HOME 'offload-stack' }
$SWAP_PORT  = 18801
$AGENT_PORT = 18802
$swapExe    = Join-Path $HOME_DIR 'llama-swap\llama-swap.exe'
$yamlPath   = Join-Path $HOME_DIR 'llama-swap.yaml'
$modelDir   = Join-Path $HOME_DIR 'models'
$llamaDir   = Join-Path $HOME_DIR 'llama'
$harnessExe = Join-Path $HOME_DIR 'harness\local-offload.exe'
$agentExe   = Join-Path $HOME_DIR 'harness\local-agent.exe'
$manifest   = Join-Path $HOME_DIR 'installed.json'
$swapBase   = "http://127.0.0.1:$SWAP_PORT"

# The chat tiers, in the fixed order we exercise them. Each maps a llama-swap model alias to the
# gguf filename the rendered template references. A tier is "installed" iff its gguf is on disk.
# (We check file existence, not installed.json: the Vulkan sandbox has no manifest, and the yaml
# always lists all three group members regardless of which were downloaded - file existence is
# the only ground truth for what can actually load.)
$TIER_SPEC = @(
  @{ id = 'offload-e4b';    file = 'gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf' }
  @{ id = 'gemma4-e2b';     file = 'gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf' }
  @{ id = 'gemma4-26b-a4b'; file = 'gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf' }
)

$script:swapProc  = $null
$script:agentProc = $null

# ---------------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------------
function Log        { param([string]$m) Write-Host $m }
function LogStep    { param([string]$m) Write-Host "==> $m" -ForegroundColor Cyan }
function LogOk      { param([string]$m) Write-Host "  PASS $m" -ForegroundColor Green }
function LogWarn    { param([string]$m) Write-Host "  WARN $m" -ForegroundColor Yellow }
function LogFail    { param([string]$m) Write-Host "  FAIL $m" -ForegroundColor Red }

function Test-PortFree {
  param([int]$Port)
  return -not [bool](Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
}

# Write a file as UTF-8 WITHOUT a BOM. Windows PowerShell 5.1's `-Encoding UTF8`
# (Set-Content/Out-File) writes a BOM; pwsh 7's does not. Go rejects a BOM'd payload
# (json.Unmarshal: "invalid character ... looking for beginning of value"), so EVERY file this
# script writes for a Go consumer (harness config JSON, llama-swap yaml) must go through here.
function Write-Utf8NoBom {
  param([string]$Path, [string]$Content)
  [System.IO.File]::WriteAllText($Path, $Content, (New-Object System.Text.UTF8Encoding($false)))
}

# Run a native-exe capture under ErrorActionPreference=Continue. Under PS 5.1 with
# $ErrorActionPreference='Stop', redirecting a native command's stderr (2>&1 / 2>$null) turns
# each stderr line into a terminating NativeCommandError - so a mere harness WARNING on stderr
# would abort the whole selftest. pwsh 7 does not do this; this shim makes both engines behave
# like pwsh 7. $LASTEXITCODE is still valid after the call.
function Invoke-WithEapContinue {
  param([scriptblock]$Body)
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try { return (& $Body) } finally { $ErrorActionPreference = $prev }
}

# GET returning parsed JSON (or $null on any failure). Short timeout; never throws.
function Invoke-JsonGet {
  param([string]$Url, [int]$TimeoutSec = 10)
  try { return Invoke-RestMethod -Uri $Url -Method Get -TimeoutSec $TimeoutSec } catch { return $null }
}

# One OpenAI chat completion against a model alias on the transient swap. Returns a hashtable
# {ok, latency_s, tokens, tok_s, error}. A request to a group member is exactly what triggers
# llama-swap to evict the wrong upstream and cold-load this one (README "How does llama-swap work").
function Invoke-Chat {
  param([string]$Model, [string]$UserContent, [int]$MaxTokens = 64, [int]$TimeoutSec = 300)
  $body = @{
    model       = $Model
    messages    = @(@{ role = 'user'; content = $UserContent })
    max_tokens  = $MaxTokens
    temperature = 0
    stream      = $false
  } | ConvertTo-Json -Depth 6 -Compress
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  try {
    $r = Invoke-RestMethod -Uri "$swapBase/v1/chat/completions" -Method Post -ContentType 'application/json' `
           -Body $body -TimeoutSec $TimeoutSec
    $sw.Stop()
    $lat = [math]::Round($sw.Elapsed.TotalSeconds, 2)
    $ct  = 0
    if ($r.usage -and $r.usage.completion_tokens) { $ct = [int]$r.usage.completion_tokens }
    $toks = $null
    if ($ct -gt 0 -and $sw.Elapsed.TotalSeconds -gt 0) { $toks = [math]::Round($ct / $sw.Elapsed.TotalSeconds, 1) }
    $text = ''
    if ($r.choices -and $r.choices[0].message) { $text = [string]$r.choices[0].message.content }
    return @{ ok = $true; latency_s = $lat; tokens = $ct; tok_s = $toks; text = $text; error = $null }
  } catch {
    $sw.Stop()
    return @{ ok = $false; latency_s = [math]::Round($sw.Elapsed.TotalSeconds, 2); tokens = 0; tok_s = $null; text = ''; error = $_.Exception.Message }
  }
}

# Classify an error string as an allocation / device-lost / GPU-crash class failure (R4.2/R4.3).
function Test-AllocFailure {
  param([string]$ErrText)
  if (-not $ErrText) { return $false }
  return ($ErrText -match '(?i)(VK_ERROR_OUT_OF_DEVICE_MEMORY|out of device memory|CUDA (error )?out of memory|cudaErrorMemoryAllocation|device[- ]lost|VK_ERROR_DEVICE_LOST|failed to allocate|ggml_backend.*alloc|unable to allocate|connection.*(closed|refused|reset|aborted)|actively refused|forcibly closed|unexpectedly)')
}

# Start the transient llama-swap against $yamlPath on $SWAP_PORT. Returns the Process object.
function Start-Swap {
  param([string]$ConfigPath)
  $logFile = Join-Path $env:TEMP ("offload-selftest-swap-{0}.log" -f $PID)
  $p = Start-Process -FilePath $swapExe `
        -ArgumentList @('--config', $ConfigPath, '--listen', "127.0.0.1:$SWAP_PORT") `
        -PassThru -NoNewWindow -RedirectStandardError $logFile -RedirectStandardOutput "$logFile.out"
  return $p
}

# Poll /v1/models until the swap answers or timeout. llama-swap's own HTTP is up long before any
# model loads, so this only proves the proxy started (a model loads lazily on first chat request).
function Wait-SwapReady {
  param([int]$TimeoutSec = 120)
  $deadline = (Get-Date).AddSeconds($TimeoutSec)
  while ((Get-Date) -lt $deadline) {
    if ($script:swapProc -and $script:swapProc.HasExited) { return $false }
    $m = Invoke-JsonGet "$swapBase/v1/models" 5
    if ($m) { return $true }
    Start-Sleep -Milliseconds 750
  }
  return $false
}

# R4.2: auto-remediation for a 26B allocation/device-lost failure. Reads $yamlPath, finds the
# gemma4-26b-a4b model's cmd, inserts --cpu-moe if absent, rewrites the yaml, restarts the
# transient swap, and retries the 26B request ONCE. Returns {outcome='pass|fail', retry=<chat
# result or $null>}. Idempotent: if --cpu-moe is already present it still restarts + retries once.
function Invoke-Remediate26B {
  Log "  R4.2: auto-remediating 26B - inserting --cpu-moe and restarting the transient swap"
  # -Encoding UTF8 is REQUIRED: PS 5.1 decodes a BOM-less file as ANSI, which would mojibake any
  # non-ASCII byte (e.g. the em-dash in the rendered yaml's comments) and the rewrite below would
  # then bake that corruption into the file. Explicit UTF8 reads correctly on both engines.
  $lines = Get-Content -Path $yamlPath -Encoding UTF8
  # Find the model key line, then the first 'cmd:' at greater indent, then the continuation line
  # that carries the actual llama-server args (the '>-' block folds onto the next non-empty line).
  $keyIdx = -1
  for ($j = 0; $j -lt $lines.Count; $j++) {
    if ($lines[$j] -match '^\s*gemma4-26b-a4b\s*:') { $keyIdx = $j; break }
  }
  if ($keyIdx -lt 0) { return @{ outcome = 'fail'; retry = $null } }
  # Insert --cpu-moe on the args continuation line (the one mentioning llama-server.exe or the .gguf)
  # if the block does not already contain it anywhere before the next top-level key.
  $blockHasCpuMoe = $false
  $argsLineIdx = -1
  for ($j = $keyIdx + 1; $j -lt $lines.Count; $j++) {
    if ($lines[$j] -match '^\s{0,4}\S' -and $lines[$j] -notmatch '^\s') { break }  # dedent to a top-level key
    if ($lines[$j] -match '^\s{0,2}\w+\s*:' -and $j -gt $keyIdx + 1 -and $lines[$j] -notmatch '(cmd|env|ttl|aliases)\s*:') { break }
    if ($lines[$j] -match '--cpu-moe') { $blockHasCpuMoe = $true }
    if ($argsLineIdx -lt 0 -and $lines[$j] -match '(llama-server\.exe|\.gguf|-ngl)') { $argsLineIdx = $j }
  }
  if (-not $blockHasCpuMoe -and $argsLineIdx -ge 0) {
    # Insert right after the .gguf model path (or after llama-server.exe) so it lands in the args.
    $lines[$argsLineIdx] = $lines[$argsLineIdx] -replace '(\.gguf)', '$1 --cpu-moe'
    if ($lines[$argsLineIdx] -notmatch '--cpu-moe') { $lines[$argsLineIdx] = $lines[$argsLineIdx] + ' --cpu-moe' }
    # BOM-less write: PS 5.1's Set-Content -Encoding UTF8 would prepend a BOM the yaml parser rejects.
    Write-Utf8NoBom -Path $yamlPath -Content (($lines -join [Environment]::NewLine) + [Environment]::NewLine)
    Log "  R4.2: --cpu-moe inserted into gemma4-26b-a4b cmd"
  } elseif ($blockHasCpuMoe) {
    Log "  R4.2: --cpu-moe already present; restarting + retrying once anyway"
  }
  # Restart the transient swap so it re-reads the yaml.
  if ($script:swapProc -and -not $script:swapProc.HasExited) {
    try { Stop-Process -Id $script:swapProc.Id -Force -ErrorAction SilentlyContinue } catch { }
  }
  Start-Sleep -Seconds 2
  $script:swapProc = Start-Swap -ConfigPath $yamlPath
  if (-not (Wait-SwapReady -TimeoutSec 120)) { return @{ outcome = 'fail'; retry = $null } }
  $retry = Invoke-Chat -Model 'gemma4-26b-a4b' -UserContent 'Reply with exactly the single word: ready' -MaxTokens 48 -TimeoutSec 300
  if ($retry.ok) { return @{ outcome = 'pass'; retry = $retry } }
  return @{ outcome = 'fail'; retry = $retry }
}

# ---------------------------------------------------------------------------
# Receipt accumulator (R4.4 schema)
# ---------------------------------------------------------------------------
$receipt = [ordered]@{
  schema          = 1
  backend         = $null
  gpu             = $null
  driver_version  = $null
  tiers           = @()
  canary          = [ordered]@{ depth = 7000; status = 'fail'; detail = 'not run' }
  remediations    = @()
  harness_smoke   = 'fail'
  agent_smoke     = 'fail'
  verdict         = 'fail'
  proves          = @(
    'installation integrity',
    'each installed tier cold-loads and generates through the swap-exclusive group',
    'this GPU+driver did not crash generating to depth 7000'
  )
  does_not_prove  = @(
    'long-running :11436 service behavior under sustained multi-hour load',
    'behavior on a different driver version than the one recorded above',
    'port :11436 vs the transient test port are otherwise identical in every respect'
  )
}

# ---------------------------------------------------------------------------
# Test seam: with OFFLOAD_SELFTEST_DOT_SOURCE=1 the script defines its functions, paths, and
# receipt state, then returns BEFORE MAIN - so a test harness can dot-source it, stub the
# transport helpers (Start-Swap / Wait-SwapReady / Invoke-Chat), and drive Invoke-Remediate26B
# etc. in isolation (see setup/tests/). Zero effect on a normal run (env var absent).
# ---------------------------------------------------------------------------
if ($env:OFFLOAD_SELFTEST_DOT_SOURCE -eq '1') { return }

# ---------------------------------------------------------------------------
# MAIN - everything inside try so the finally block guarantees teardown.
# ---------------------------------------------------------------------------
try {
  Log ""
  LogStep "offload-harness selftest | home=$HOME_DIR | swap-port=$SWAP_PORT agent-port=$AGENT_PORT"

  # --- Behavior 1: preflight - artifacts present + ports free -------------------------------
  LogStep "preflight: artifacts + ports"
  foreach ($f in @($swapExe, $yamlPath, $harnessExe, $agentExe)) {
    if (-not (Test-Path $f)) { throw "missing required artifact: $f (was install.ps1 run against this OFFLOAD_HOME?)" }
  }
  if (-not (Test-PortFree $SWAP_PORT))  { throw "port $SWAP_PORT is already in use - free it before selftest" }
  if (-not (Test-PortFree $AGENT_PORT)) { throw "port $AGENT_PORT is already in use - free it before selftest" }
  LogOk "artifacts present; ports $SWAP_PORT/$AGENT_PORT free"

  # --- Backend detection: installed.json wins; else infer from the ggml backend dll ---------
  $backend = $null
  if (Test-Path $manifest) {
    try { $backend = (Get-Content -Raw $manifest -Encoding UTF8 | ConvertFrom-Json).backend } catch { $backend = $null }
  }
  if (-not $backend) {
    if     (Test-Path (Join-Path $llamaDir 'ggml-cuda.dll'))   { $backend = 'cuda' }
    elseif (Test-Path (Join-Path $llamaDir 'ggml-vulkan.dll')) { $backend = 'vulkan' }
    else   { $backend = 'cpu' }
  }
  $receipt.backend = $backend
  LogOk "backend = $backend"

  # --- GPU + driver capture (R4.3) ----------------------------------------------------------
  # Source of truth for WHICH gpu = llama.cpp itself. `llama-server --list-devices` prints the
  # backend devices in selection order; device 0 (the first "<Backend>N:" line) is what
  # llama-server uses by default. This avoids the Win32_VideoController enumeration-order trap
  # (e.g. Vulkan device 0 = discrete NVIDIA, but the iGPU may enumerate first in WMI). We then
  # match that device name back to WMI purely to read its DriverVersion. On cpu backend, skip.
  $llamaServer = Join-Path $llamaDir 'llama-server.exe'
  if ($backend -ne 'cpu' -and (Test-Path $llamaServer)) {
    $dev0 = $null
    try {
      $devOut = Invoke-WithEapContinue { & $llamaServer --list-devices 2>&1 | Out-String }
      # First "Word0:" style backend-device line, capture the human name up to the trailing "(...)".
      $m = [regex]::Match($devOut, '(?m)^\s*\w+0:\s*(.+?)\s*\(')
      if ($m.Success) { $dev0 = $m.Groups[1].Value.Trim() }
    } catch { }
    if ($dev0) { $receipt.gpu = $dev0 }
    # Driver version: match the WMI adapter whose Name best matches dev0 (or the first NVIDIA/AMD).
    try {
      $adapters = Get-CimInstance Win32_VideoController -ErrorAction Stop
      $match = $null
      if ($dev0) {
        # Reduce dev0 to distinctive words (drop "Laptop GPU", "(R)", parentheticals) for a loose match.
        $key = ($dev0 -replace '\(R\)','' -replace 'Laptop GPU','' -replace '[()]',' ').Trim()
        $match = $adapters | Where-Object { $_.Name -and ($_.Name -like "*$key*" -or $key -like "*$($_.Name)*") } | Select-Object -First 1
      }
      if (-not $match) { $match = $adapters | Where-Object { $_.Name -match 'NVIDIA|AMD|Radeon' } | Select-Object -First 1 }
      if (-not $match) { $match = $adapters | Select-Object -First 1 }
      if ($match) {
        if (-not $receipt.gpu) { $receipt.gpu = [string]$match.Name }
        $receipt.driver_version = [string]$match.DriverVersion
      }
    } catch { }
  }
  $gpuLabel = if ($receipt.gpu) { $receipt.gpu } else { '(none/cpu)' }
  $drvLabel = if ($receipt.driver_version) { $receipt.driver_version } else { 'null' }
  LogOk "gpu = $gpuLabel | driver = $drvLabel (gpu from llama-server --list-devices device 0)"

  # --- Which chat tiers are actually installed (file existence) ------------------------------
  $installedTiers = @($TIER_SPEC | Where-Object { Test-Path (Join-Path $modelDir $_.file) })
  if ($installedTiers.Count -eq 0) { throw "no chat-tier gguf found in $modelDir - nothing to exercise" }
  LogOk ("installed chat tiers: {0}" -f (($installedTiers | ForEach-Object { $_.id }) -join ', '))

  # --- Expectations table (info only) -------------------------------------------------------
  Log ""
  Log "  tok/s expectations (tg, info only): NVIDIA 3070 ~= 70-83 t/s | AMD 780M ~= 19-25 t/s | CPU-class < 8 t/s (WARN)"
  Log ""

  # --- Start the transient swap (Behavior 1 continued) --------------------------------------
  LogStep "start transient llama-swap on $SWAP_PORT"
  $script:swapProc = Start-Swap -ConfigPath $yamlPath
  if (-not (Wait-SwapReady -TimeoutSec 120)) {
    throw "llama-swap did not answer /v1/models within 120s on port $SWAP_PORT (proxy failed to start)"
  }
  LogOk "llama-swap proxy up (pid $($script:swapProc.Id))"

  # --- R4.1: exercise EACH installed tier THROUGH the exclusive+swap group -------------------
  # Hitting each alias in sequence on the SAME running swap: the exclusive group means request N+1
  # to a different member evicts member N and cold-loads N+1. With one installed tier this proves
  # cold-load+generate live; the eviction transition is proven-by-construction (the group config is
  # identical) and is exercised live only when 2+ tiers are present.
  # Sections below are INDEPENDENT: a failure in one records its own fail status and the rest
  # still run. Only preflight / llama-swap-start failures (above) abort the whole selftest.
  LogStep "R4.1: per-tier chat through the offload-family swap group"
  $tierPrompt = 'Reply with exactly the single word: ready'
  try {
  foreach ($tier in $installedTiers) {
    $isMoE = ($tier.id -eq 'gemma4-26b-a4b')
    Log "  -> tier $($tier.id): request 1 (COLD - triggers evict-prior + cold-load; time = load+first-gen)"
    $res = Invoke-Chat -Model $tier.id -UserContent $tierPrompt -MaxTokens 48 -TimeoutSec 300

    # R4.2: auto-remediation on 26B allocation failure.
    if (-not $res.ok -and $isMoE -and (Test-AllocFailure $res.error)) {
      LogWarn "26B allocation/device-lost class failure: $($res.error)"
      $remOutcome = Invoke-Remediate26B
      $receipt.remediations += ,([ordered]@{ tier = 'gemma4-26b-a4b'; action = 'added --cpu-moe'; outcome = $remOutcome.outcome })
      if ($remOutcome.retry) { $res = $remOutcome.retry }
    }

    if ($res.ok) {
      # cold_load_s = wall time of the cold request (model load + short generation). tok/s off a
      # cold, tiny-output request is meaningless (dominated by load latency), so measure real
      # throughput with a WARM second request (model now resident) generating a longer completion.
      $coldS = $res.latency_s
      $warm = Invoke-Chat -Model $tier.id -UserContent 'Count from one to forty in words, comma-separated, then stop.' -MaxTokens 192 -TimeoutSec 120
      $tokS = $null
      if ($warm.ok -and $null -ne $warm.tok_s) { $tokS = $warm.tok_s }
      $status = 'pass'
      $tokLabel = if ($null -ne $tokS) { "$tokS" } else { 'n/a' }
      if ($null -ne $tokS -and $tokS -lt 8) { $status = 'warn' }  # CPU-class throughput
      $receipt.tiers += ,([ordered]@{ id = $tier.id; cold_load_s = $coldS; tok_s = $tokS; status = $status })
      if ($status -eq 'warn') { LogWarn "$($tier.id): cold_load=${coldS}s warm-tok/s=$tokLabel (CPU-class, WARN)" }
      else { LogOk "$($tier.id): cold_load=${coldS}s warm-tok/s=$tokLabel" }
    } else {
      # 26B failing (even post-remediation) is a tier-local FAIL, not a whole-selftest FAIL.
      $msg = $res.error
      if ($isMoE) { $msg = 'auto-remediation (--cpu-moe) applied but still failed; try a lower -ngl or update the AMD Adrenalin driver' }
      $receipt.tiers += ,([ordered]@{ id = $tier.id; cold_load_s = $res.latency_s; tok_s = $null; status = 'fail' })
      LogFail "$($tier.id): $msg"
    }
  }
  if ($installedTiers.Count -ge 2) {
    LogOk "swap-group eviction path exercised LIVE across $($installedTiers.Count) tiers (each cold-load implies the prior was evicted)"
  } else {
    Log  "  NOTE: only 1 chat tier installed - cold-load+generate proven LIVE; the evict-then-reload transition is proven BY CONSTRUCTION (same exclusive+swap group), not exercised live here."
  }
  } catch {
    LogFail "tier-exercise section threw: $($_.Exception.Message) - continuing to remaining sections"
  }

  # --- R4.3: deep-context canary at depth ~7000 ---------------------------------------------
  LogStep "R4.3: deep-context canary (depth ~7000 in the 8192 window)"
  try {
  # Build a ~7000-token prompt. A GGUF token is ~4 chars of English on average; ~6.5 chars/word.
  # 7000 tokens ~= 28000 chars. We pad with numbered lorem lines (deterministic) then ask for a
  # 128-token generation, so token GENERATION happens at depth (where llama.cpp#17432 lives).
  $sb = New-Object System.Text.StringBuilder
  [void]$sb.Append("You are given a long numbered log. After reading it, write a 128-word continuation of the story of a lighthouse keeper. Log follows.`n")
  $line = 'lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. '
  # ~28000 chars total; the seed line is ~118 chars, so ~236 lines. Guard on char count directly.
  $i = 0
  while ($sb.Length -lt 28000) { [void]$sb.Append(("{0:D4}: {1}`n" -f $i, $line)); $i++ }
  [void]$sb.Append("`nNow write the 128-word continuation:")
  $canaryPrompt = $sb.ToString()
  # Canary always runs against the primary tier (offload-e4b) - the tier every install has.
  $canaryModel = $installedTiers[0].id
  $can = Invoke-Chat -Model $canaryModel -UserContent $canaryPrompt -MaxTokens 160 -TimeoutSec 300
  if ($can.ok -and $can.text -and $can.text.Trim().Length -gt 0) {
    $receipt.canary.status = 'pass'
    $receipt.canary.detail = "generated $($can.tokens) tokens at depth ~7000 on $canaryModel (this GPU=$gpuLabel driver=$drvLabel); no device-lost/crash"
    LogOk "canary generated $($can.tokens) tokens at depth ~7000 (machine-local: this GPU + driver + depth only)"
  } elseif ((Test-AllocFailure $can.error) -or $can.error) {
    $receipt.canary.status = 'fail'
    $receipt.canary.detail = "depth-7000 generation FAILED on $canaryModel : $($can.error) - update the AMD Adrenalin driver; if it persists switch 26b/e4b to --cpu-moe / lower -ngl"
    LogFail "canary crashed/failed at depth 7000: $($can.error)"
  } else {
    $receipt.canary.status = 'fail'
    $receipt.canary.detail = "depth-7000 generation returned empty completion on $canaryModel"
    LogFail "canary returned empty completion at depth 7000"
  }
  } catch {
    $receipt.canary.status = 'fail'
    $receipt.canary.detail = "canary section threw: $($_.Exception.Message)"
    LogFail "canary section threw: $($_.Exception.Message) - continuing to remaining sections"
  }

  # --- Behavior 4: harness grammar smoke (independent section) -------------------------------
  LogStep "harness smoke: doctor + one summarize against the test port"
  # Point the harness at the transient swap by writing a temp config (endpoint override). We copy
  # the installed config so all other fields (model, paths) stay real, then override endpoint+port.
  # $tmpCfg is created here but also reused by the agent smoke below (guarded by Test-Path there).
  $tmpCfg = Join-Path $env:TEMP ("offload-selftest-cfg-{0}.json" -f $PID)
  try {
    $baseCfg = Join-Path $HOME '.local-offload\config.json'
    if (Test-Path $baseCfg) {
      $cfgObj = Get-Content -Raw $baseCfg -Encoding UTF8 | ConvertFrom-Json   # explicit UTF8: 5.1 defaults BOM-less to ANSI
    } else {
      $cfgObj = [pscustomobject]@{ completion_path = '/v1/chat/completions'; model = 'offload-e4b' }
    }
    $cfgObj | Add-Member -NotePropertyName endpoint -NotePropertyValue $swapBase -Force
    # Neutralize bbolt cache/ledger contention against a possibly-running real MCP by pointing at temp.
    $cfgObj | Add-Member -NotePropertyName cache_path  -NotePropertyValue '' -Force
    $cfgObj | Add-Member -NotePropertyName ledger_path -NotePropertyValue (Join-Path $env:TEMP ("offload-selftest-ledger-{0}.jsonl" -f $PID)) -Force
    # BOM-less: PS 5.1's Set-Content -Encoding UTF8 writes a BOM and Go's json.Unmarshal rejects
    # it ("invalid character ..."), silently falling back to DEFAULT config = the REAL :11436
    # endpoint - the smoke would test the wrong service. Write-Utf8NoBom on both engines.
    Write-Utf8NoBom -Path $tmpCfg -Content ($cfgObj | ConvertTo-Json -Depth 8)

    # doctor is DIAGNOSTIC context, not the gate. On a minimal offload-family stack doctor
    # intentionally exits non-zero because it validates EVERY configured alias (escalation/vision/
    # stt = reasoning/vision/stt models, e.g. qwen3vl-4b, whisper-stt, ...) against the live roster, and those media/vision
    # models are legitimately absent from a grunt-work-only install. What matters for harness_smoke
    # is that (a) the endpoint is healthy and (b) the grammar pipeline actually returns a result -
    # so the SUMMARIZE call is the determinant (brief behavior 4), not doctor's exit code.
    $doctorOut = Invoke-WithEapContinue { & $harnessExe --config $tmpCfg doctor 2>&1 }
    $doctorCode = $LASTEXITCODE
    $doctorHealthy = [bool]((($doctorOut | ForEach-Object { "$_" }) -join "`n") -match '(?m)^\s*health:\s*OK')
    if ($doctorCode -eq 0) { LogOk "harness doctor OK (all configured aliases live)" }
    elseif ($doctorHealthy) { LogWarn "harness doctor: endpoint healthy; some non-offload aliases absent (expected on a minimal stack) - not a failure" }
    else { LogWarn "harness doctor: endpoint health line not OK (exit $doctorCode) - see summarize result below" }

    # One summarize with a here-string sample; parse the JSON result. Defer => WARN (by design).
    $sample = @"
The quarterly review covered three themes. First, infrastructure costs fell 12 percent after the
migration to reserved capacity. Second, the support backlog was cleared and median response time
dropped from nine hours to under two. Third, two new regions came online, extending coverage to the
Asia-Pacific market for the first time. Leadership asked the team to prioritize reliability work next.
"@
    $sumOut = Invoke-WithEapContinue { $sample | & $harnessExe --config $tmpCfg summarize - --json 2>$null }
    $sumCode = $LASTEXITCODE
    $parsed = $null
    if ($sumOut) { try { $parsed = (($sumOut | ForEach-Object { "$_" }) -join "`n") | ConvertFrom-Json } catch { $parsed = $null } }
    if ($sumCode -eq 0 -and $parsed -and ($parsed.deferred -eq $true)) {
      $receipt.harness_smoke = 'defer'
      $rsn = if ($parsed.reason) { $parsed.reason } else { 'model deferred' }
      LogWarn "harness summarize DEFERRED (designed behavior, not a failure): $rsn"
    } elseif ($sumCode -eq 0 -and $parsed) {
      $receipt.harness_smoke = 'ok'
      LogOk "harness summarize returned a non-deferred JSON result"
    } else {
      $receipt.harness_smoke = 'fail'
      LogFail "harness summarize produced no parseable JSON (exit $sumCode)"
    }
  } catch {
    $receipt.harness_smoke = 'fail'
    LogFail "harness smoke section threw: $($_.Exception.Message) - continuing to agent smoke"
  }

  # --- Behavior 5: agent smoke - serve on 18802, /v1/models, kill (independent section) ------
  LogStep "agent smoke: local-agent --serve on $AGENT_PORT + /v1/models"
  try {
    $agentLog = Join-Path $env:TEMP ("offload-selftest-agent-{0}.log" -f $PID)
    # --base overrides the endpoint directly, so the agent works even if the temp config write
    # failed above (it would just warn and use defaults) - pass --config only when it exists.
    $agentArgs = @('--serve', '--listen', "127.0.0.1:$AGENT_PORT", '--base', "$swapBase/v1")
    if (Test-Path $tmpCfg) { $agentArgs = @('--config', $tmpCfg) + $agentArgs }
    $script:agentProc = Start-Process -FilePath $agentExe `
        -ArgumentList $agentArgs `
        -PassThru -NoNewWindow -RedirectStandardError $agentLog -RedirectStandardOutput "$agentLog.out"
    $agentUp = $false
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
      if ($script:agentProc.HasExited) { break }
      $am = Invoke-JsonGet "http://127.0.0.1:$AGENT_PORT/v1/models" 5
      if ($am) { $agentUp = $true; break }
      Start-Sleep -Milliseconds 500
    }
    if ($agentUp) {
      $receipt.agent_smoke = 'ok'
      LogOk "local-agent server answered /v1/models on $AGENT_PORT"
    } else {
      $receipt.agent_smoke = 'fail'
      LogFail "local-agent server did not answer /v1/models on $AGENT_PORT within 30s"
    }
  } catch {
    $receipt.agent_smoke = 'fail'
    LogFail "agent smoke section threw: $($_.Exception.Message)"
  }

  # --- Verdict (R4.4 rule) ------------------------------------------------------------------
  # fail iff harness_smoke=fail OR agent_smoke=fail OR ALL tiers failed. A single non-26b tier
  # failing, or 26b failing even after remediation, is WARN (partial capability). The canary
  # failing degrades to WARN too (machine-local crash signal, not an install-integrity failure).
  $allTiersFailed = ($receipt.tiers.Count -gt 0) -and (-not ($receipt.tiers | Where-Object { $_.status -ne 'fail' }))
  $anyTierWarnOrFail = [bool]($receipt.tiers | Where-Object { $_.status -ne 'pass' })
  if ($receipt.harness_smoke -eq 'fail' -or $receipt.agent_smoke -eq 'fail' -or $allTiersFailed) {
    $receipt.verdict = 'fail'
  } elseif ($anyTierWarnOrFail -or $receipt.canary.status -ne 'pass' -or $receipt.harness_smoke -eq 'defer') {
    $receipt.verdict = 'warn'
  } else {
    $receipt.verdict = 'pass'
  }
}
catch {
  # Any hard throw (missing artifact, port taken, swap failed to start) => fail verdict, real detail.
  $receipt.verdict = 'fail'
  if ($receipt.canary.detail -eq 'not run') { $receipt.canary.detail = "selftest aborted: $($_.Exception.Message)" }
  LogFail "selftest aborted: $($_.Exception.Message)"
}
finally {
  # ---------------------------------------------------------------------------
  # Teardown - GUARANTEED. Stop both transient processes; verify both ports freed.
  # ---------------------------------------------------------------------------
  LogStep "teardown: stopping transient processes; freeing ports"
  foreach ($pv in @($script:agentProc, $script:swapProc)) {
    if ($pv -and -not $pv.HasExited) {
      try { Stop-Process -Id $pv.Id -Force -ErrorAction SilentlyContinue } catch { }
    }
  }
  # llama-swap spawns child llama-server.exe processes; kill any whose command line points at OUR
  # test port or our config, so a loaded model does not outlive the selftest and hold the port.
  try {
    $kids = Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" -ErrorAction SilentlyContinue |
              Where-Object { $_.CommandLine -and ($_.CommandLine -match [regex]::Escape("127.0.0.1")) -and ($_.CommandLine -match [regex]::Escape($HOME_DIR.Replace('\','/')) -or $_.CommandLine -match [regex]::Escape($HOME_DIR)) }
    foreach ($k in $kids) { try { Stop-Process -Id $k.ProcessId -Force -ErrorAction SilentlyContinue } catch { } }
  } catch { }
  # Wait up to 10s for both ports to actually free (process exit + socket close are not instant).
  $freeDeadline = (Get-Date).AddSeconds(10)
  while ((Get-Date) -lt $freeDeadline) {
    if ((Test-PortFree $SWAP_PORT) -and (Test-PortFree $AGENT_PORT)) { break }
    Start-Sleep -Milliseconds 500
  }
  $swapFree  = Test-PortFree $SWAP_PORT
  $agentFree = Test-PortFree $AGENT_PORT
  if ($swapFree -and $agentFree) { LogOk "ports $SWAP_PORT/$AGENT_PORT freed" }
  else { LogWarn "port(s) still bound after teardown: swap-free=$swapFree agent-free=$agentFree (leaked llama-server?)" }
  # Best-effort temp cleanup (never fatal).
  Get-ChildItem $env:TEMP -Filter ("offload-selftest-*{0}*" -f $PID) -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
}

# ---------------------------------------------------------------------------
# Print the receipt as the LAST stdout line, then exit 0 (pass|warn) or 1 (fail).
# ---------------------------------------------------------------------------
Log ""
LogStep "verdict: $($receipt.verdict)"
# Code-enforce the receipt's array-typed fields at serialization time. PS 5.1's ConvertTo-Json
# has known unwrap/null hazards around empty and 1-element collections depending on how the
# value was built; forcing [object[]] here makes "tiers":[{...}] (array-of-one, never a bare
# object) and "remediations":[] (empty array, never null) structural rather than runtime luck.
$receipt.tiers        = [object[]]@($receipt.tiers)
$receipt.remediations = [object[]]@($receipt.remediations)
$json = ([pscustomobject]$receipt) | ConvertTo-Json -Depth 8 -Compress
Write-Output $json
if ($receipt.verdict -eq 'fail') { exit 1 } else { exit 0 }
