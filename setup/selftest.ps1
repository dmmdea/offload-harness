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
# The agent-driving tier: the model an autonomous coding/edit agent actually steers through. Default
# offload-e4b (present in every install); override with OFFLOAD_AGENT_TIER to bench a different alias.
if ($env:OFFLOAD_AGENT_TIER) { $AGENT_TIER = $env:OFFLOAD_AGENT_TIER } else { $AGENT_TIER = 'offload-e4b' }
# Agent context accounting: an agent's usable INPUT budget = served ctx minus the output reservation
# (max_tokens the agent may generate) minus a safety margin for the chat template + tool scaffolding.
$AGENT_OUTPUT_RESERVE = 4096
$AGENT_SAFETY_MARGIN  = 512
$AGENT_LARGER_CTX     = 16384   # target ctx we attempt to load at, to prove headroom beyond served 8192
$swapExe    = Join-Path $HOME_DIR 'llama-swap\llama-swap.exe'
$yamlPath   = Join-Path $HOME_DIR 'llama-swap.yaml'
$modelDir   = Join-Path $HOME_DIR 'models'
$llamaDir   = Join-Path $HOME_DIR 'llama'
$harnessExe = Join-Path $HOME_DIR 'harness\local-offload.exe'
$agentExe   = Join-Path $HOME_DIR 'harness\local-agent.exe'
$manifest   = Join-Path $HOME_DIR 'installed.json'
$swapBase   = "http://127.0.0.1:$SWAP_PORT"
# H3: the PROJECTED profile map (profiles.json) lives beside this script under templates/.
# The selftest reads it to know the profile's projected ctx/KV/moe so it can MEASURE against
# them. OFFLOAD_PROFILES_JSON overrides (tests point at a fixture / a synthetic profile map).
$scriptDir  = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
if ($env:OFFLOAD_PROFILES_JSON) { $profilesJsonPath = $env:OFFLOAD_PROFILES_JSON }
else { $profilesJsonPath = Join-Path $scriptDir 'templates\profiles.json' }
# Optional: a captured detect.ps1 last-line JSON (fallback profile source when installed.json
# predates H2). OFFLOAD_DETECT_JSON points at a file holding that JSON; absent by default.
$detectJsonPath = if ($env:OFFLOAD_DETECT_JSON) { $env:OFFLOAD_DETECT_JSON } else { $null }

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
    $pt  = 0
    if ($r.usage -and $r.usage.prompt_tokens) { $pt = [int]$r.usage.prompt_tokens }
    $toks = $null
    if ($ct -gt 0 -and $sw.Elapsed.TotalSeconds -gt 0) { $toks = [math]::Round($ct / $sw.Elapsed.TotalSeconds, 1) }
    $text = ''
    if ($r.choices -and $r.choices[0].message) { $text = [string]$r.choices[0].message.content }
    return @{ ok = $true; latency_s = $lat; tokens = $ct; prompt_tokens = $pt; tok_s = $toks; text = $text; error = $null }
  } catch {
    $sw.Stop()
    return @{ ok = $false; latency_s = [math]::Round($sw.Elapsed.TotalSeconds, 2); tokens = 0; prompt_tokens = 0; tok_s = $null; text = ''; error = $_.Exception.Message }
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

# Read the served chat-tier --ctx-size from the ACTIVE rendered yaml (ground truth: what actually
# launches). The chat tiers share the 'common' macro whose first '--ctx-size N' is the served window;
# the embedding model has its own smaller '--ctx-size 2048' further down, so we take the FIRST match.
# Returns [int] on success, $null if the file is unreadable / has no match (caller falls back to 8192).
function Get-ServedCtx {
  param([string]$ConfigPath)
  try {
    if (-not (Test-Path $ConfigPath)) { return $null }
    $raw = Get-Content -Raw -Path $ConfigPath -Encoding UTF8   # explicit UTF8: 5.1 defaults BOM-less to ANSI
    $m = [regex]::Match($raw, '--ctx-size\s+(\d+)')
    if ($m.Success) { return [int]$m.Groups[1].Value }
  } catch { }
  return $null
}

# ---------------------------------------------------------------------------
# J1 canary pure helpers (dot-source testable via OFFLOAD_SELFTEST_DOT_SOURCE).
# ---------------------------------------------------------------------------

# Jaccard word-set overlap of two generations, 0.0-1.0 rounded to 2 decimals.
# Used by the FA+q8_0-KV correctness canary: at temp 0 an f16-KV and a q8_0-KV run
# of the same prompt should produce near-identical text; a positional diff would
# over-penalize a benign mid-sequence divergence, so we compare word SETS. Pure fn.
function Get-WordOverlap {
  param([string]$A, [string]$B)
  if ([string]::IsNullOrWhiteSpace($A) -or [string]::IsNullOrWhiteSpace($B)) { return 0.0 }
  # @(...) around EVERY use: PS 5.1 unwraps a single-element pipeline result to a bare
  # string, and 'string + string' is CONCATENATION - without the array forcing, a
  # degenerate single-unique-word generation made union=1 and overlap read 1.0
  # (found in adversarial review; exactly the failure mode this canary guards).
  $wa = @(@(($A.ToLowerInvariant() -split '[^\w]+') | Where-Object { $_ }) | Select-Object -Unique)
  $wb = @(@(($B.ToLowerInvariant() -split '[^\w]+') | Where-Object { $_ }) | Select-Object -Unique)
  if ($wa.Count -eq 0 -or $wb.Count -eq 0) { return 0.0 }
  $inter = @($wa | Where-Object { $wb -contains $_ }).Count
  $union = @(@($wa) + @($wb) | Select-Object -Unique).Count
  if ($union -eq 0) { return 0.0 }
  return [math]::Round($inter / $union, 2)
}

# Scan a llama-server log for the flash-attention state. Returns 'on'|'off'|$null
# (unknown). Tolerant of the several formats llama.cpp has used ("flash_attn = 1",
# "flash_attn : enabled", auto-disable notices). The FA canary treats 'off' as a
# HARD fail (the silent-FA-disable mode then breaks quantized-V loads on Gemma) and
# $null as a soft unknown (recorded honestly, never claimed confirmed). Pure fn.
function Get-FaStateFromLog {
  param([string]$LogText)
  if ([string]::IsNullOrWhiteSpace($LogText)) { return $null }
  if ($LogText -match '(?im)(disabling flash attention|flash attention.*not supported|flash_attn[^\r\n]*(=|:)\s*(0|off|disabled|false)\b)') { return 'off' }
  if ($LogText -match '(?im)flash_attn[^\r\n]*(=|:)\s*(1|on|enabled|true|auto)\b') {
    # 'auto' alone does not prove ON — but the negative patterns above did not match,
    # so an explicit '-fa on' launch that reached here is serving with FA. Distinguish:
    if ($LogText -match '(?im)flash_attn[^\r\n]*(=|:)\s*auto\b' -and $LogText -notmatch '(?im)flash_attn[^\r\n]*(=|:)\s*(1|on|enabled|true)\b') { return $null }
    return 'on'
  }
  return $null
}

# J2: read the active profile's config_seed from profiles.json and expand the
# __OFFLOAD_HOME__ token — the selftest's view of the media bindings mirrors what
# install.ps1 Step 8 seeded, keeping model names in ONE place (profiles.json).
# Returns a hashtable of the seed's keys (token-expanded) or $null. Pure of hardware.
function Get-MediaSeed {
  param([string]$ProfileId, [string]$ProfilesJsonPath, [string]$OffloadHome)
  if (-not $ProfileId -or -not (Test-Path $ProfilesJsonPath)) { return $null }
  try { $doc = Get-Content -Raw -Path $ProfilesJsonPath -Encoding UTF8 | ConvertFrom-Json } catch { return $null }
  if (-not $doc.profiles.PSObject.Properties[$ProfileId]) { return $null }
  $seed = $doc.profiles.$ProfileId.config_seed
  if (-not $seed) { return $null }
  $homeFwd = $OffloadHome.Replace('\', '/')
  $r = @{}
  foreach ($p in $seed.PSObject.Properties) {
    $v = $p.Value
    if ($v -is [string]) { $v = $v.Replace('__OFFLOAD_HOME__', $homeFwd) }
    elseif ($v -is [System.Array]) { $v = @($v | ForEach-Object { if ($_ -is [string]) { $_.Replace('__OFFLOAD_HOME__', $homeFwd) } else { $_ } }) }
    $r[$p.Name] = $v
  }
  return $r
}

# J2: count distinct colors on a sparse sample grid of a PNG (System.Drawing).
# The non-blank gate for a rendered image: a blank/solid/failed decode samples
# 1-2 distinct colors; any real render samples dozens. Returns [int] or $null
# (unreadable file — caller records the failure, never fakes a count).
function Get-PngDistinctColors {
  param([string]$Path, [int]$Grid = 16)
  try {
    Add-Type -AssemblyName System.Drawing -ErrorAction Stop
    $bmp = [System.Drawing.Bitmap]::FromFile($Path)
    try {
      $colors = New-Object System.Collections.Generic.HashSet[int]
      for ($gy = 0; $gy -lt $Grid; $gy++) {
        for ($gx = 0; $gx -lt $Grid; $gx++) {
          $x = [int]([math]::Floor(($gx + 0.5) * $bmp.Width / $Grid))
          $y = [int]([math]::Floor(($gy + 0.5) * $bmp.Height / $Grid))
          [void]$colors.Add($bmp.GetPixel($x, $y).ToArgb())
        }
      }
      return $colors.Count
    } finally { $bmp.Dispose() }
  } catch { return $null }
}

# Cosine similarity of two equal-length vectors, rounded to 4 decimals. Pure fn.
function Get-CosineSim {
  param([double[]]$A, [double[]]$B)
  if (-not $A -or -not $B -or $A.Count -eq 0 -or $A.Count -ne $B.Count) { return $null }
  $dot = 0.0; $na = 0.0; $nb = 0.0
  for ($i = 0; $i -lt $A.Count; $i++) { $dot += $A[$i] * $B[$i]; $na += $A[$i] * $A[$i]; $nb += $B[$i] * $B[$i] }
  if ($na -eq 0 -or $nb -eq 0) { return $null }
  return [math]::Round($dot / ([math]::Sqrt($na) * [math]::Sqrt($nb)), 4)
}

# Approx KV-cache size in MB for gemma-4 E4B at a given ctx. f16 KV (2 bytes/elem), K+V (x2).
# gemma-4 E4B geometry: ~34 layers, GQA num_kv_heads ~4, head_dim 256 -> kv_dim = 4*256 = 1024.
# bytes = ctx * layers * kv_dim * 2(K+V) * 2(f16). This is a documented ESTIMATE, not a measurement.
function Get-KvMbApprox {
  param([int]$Ctx, [int]$Layers = 34, [int]$KvDim = 1024)
  $bytes = [double]$Ctx * $Layers * $KvDim * 2 * 2
  return [math]::Round($bytes / 1MB, 0)
}

# Attempt to load+serve the agent tier at a LARGER ctx than served, purely to measure headroom.
# We do NOT touch the running swap or its yaml: we spin a standalone llama-server on a spare port,
# probe /health, then kill it. Returns {ok=$bool; detail=<string>}. A failure here is informational
# (the served 8192 config is unaffected); we degrade to computed-only if we can't even try.
function Test-LargerCtxLoad {
  param([int]$Ctx, [int]$TimeoutSec = 120)
  $llamaServer = Join-Path $llamaDir 'llama-server.exe'
  $tierFile = ($TIER_SPEC | Where-Object { $_.id -eq $AGENT_TIER } | Select-Object -First 1).file
  if (-not $tierFile) { $tierFile = ($TIER_SPEC | Where-Object { $_.id -eq 'offload-e4b' }).file }
  $modelPath = Join-Path $modelDir $tierFile
  if (-not (Test-Path $llamaServer)) { return @{ ok = $false; detail = 'llama-server.exe absent' } }
  if (-not (Test-Path $modelPath))   { return @{ ok = $false; detail = "model gguf absent: $tierFile" } }
  $probePort = 18803
  if (-not (Test-PortFree $probePort)) { return @{ ok = $false; detail = "probe port $probePort busy" } }
  $ngl = if ($backend -eq 'cpu') { 0 } else { 999 }
  $logFile = Join-Path $env:TEMP ("offload-selftest-ctxprobe-{0}.log" -f $PID)
  $args = @('-m', $modelPath, '-c', "$Ctx", '-ngl', "$ngl", '--flash-attn', 'on',
            '--cache-type-k', 'f16', '--cache-type-v', 'f16', '--jinja', '--no-webui',
            '--port', "$probePort", '--host', '127.0.0.1')
  $proc = $null
  try {
    $proc = Start-Process -FilePath $llamaServer -ArgumentList $args -PassThru -NoNewWindow `
              -RedirectStandardError $logFile -RedirectStandardOutput "$logFile.out"
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
      if ($proc.HasExited) {
        $tail = ''
        try { $tail = (Get-Content -Path $logFile -Tail 4 -ErrorAction SilentlyContinue) -join ' | ' } catch { }
        return @{ ok = $false; detail = ("server exited early (code {0}) {1}" -f $proc.ExitCode, $tail) }
      }
      $h = Invoke-JsonGet "http://127.0.0.1:$probePort/health" 4
      if ($h) { return @{ ok = $true; detail = "loaded+healthy at ctx=$Ctx on a standalone probe server" } }
      Start-Sleep -Milliseconds 750
    }
    return @{ ok = $false; detail = "no /health within ${TimeoutSec}s at ctx=$Ctx" }
  } catch {
    return @{ ok = $false; detail = "probe threw: $($_.Exception.Message)" }
  } finally {
    if ($proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue } catch { } }
    # Kill any lingering llama-server bound to our probe port (child processes can outlive the parent handle).
    try {
      $kids = Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" -ErrorAction SilentlyContinue |
                Where-Object { $_.CommandLine -and ($_.CommandLine -match [regex]::Escape("127.0.0.1:$probePort")) }
      foreach ($k in $kids) { try { Stop-Process -Id $k.ProcessId -Force -ErrorAction SilentlyContinue } catch { } }
    } catch { }
    # Wait briefly for the probe port to free so a later section never collides with it.
    $pd = (Get-Date).AddSeconds(8)
    while ((Get-Date) -lt $pd) { if (Test-PortFree $probePort) { break }; Start-Sleep -Milliseconds 400 }
    Get-ChildItem $env:TEMP -Filter ("offload-selftest-ctxprobe-{0}*" -f $PID) -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  }
}

# ===========================================================================
# H3: measure-at-install helpers. These turn the H2 PROJECTED profile numbers
# (setup/templates/profiles.json) into MEASUREMENTS on THIS box, honestly marking
# anything not measurable here as projected/skipped/measure-on-target. All are pure
# or side-effect-scoped (standalone probe servers on spare ports, torn down here) and
# NEVER touch the running transient swap, its yaml, or the operator's live :11436.
# ===========================================================================

# Read the ACTIVE profile the way install.ps1 (H2) resolved it: installed.json wins
# (it carries profile + agent_ctx_tokens + ram_tier + big_ram written at install), else
# detect.ps1's last-line JSON, else the OFFLOAD_PROFILE env override. Pure of hardware
# (only reads files/env), so a test can point it at a synthetic OFFLOAD_HOME. Returns a
# hashtable; profile=$null when nothing resolves (an off-matrix / pre-H2 stack).
#   profile, profile_src ('installed.json'|'detect.ps1'|'OFFLOAD_PROFILE'|'none'),
#   ram_tier, big_ram, agent_ctx_tokens (int or $null)
function Read-ActiveProfile {
  param([string]$ManifestPath, [string]$DetectJsonPath = $null)
  $r = @{ profile = $null; profile_src = 'none'; ram_tier = $null; big_ram = $false; agent_ctx_tokens = $null }
  # 1) installed.json (the H2 install manifest is the authority).
  if ($ManifestPath -and (Test-Path $ManifestPath)) {
    try {
      $m = Get-Content -Raw -Path $ManifestPath -Encoding UTF8 | ConvertFrom-Json
      if ($m.PSObject.Properties['profile'] -and $m.profile) {
        $r.profile = [string]$m.profile
        $r.profile_src = 'installed.json'
        if ($m.PSObject.Properties['ram_tier']) { $r.ram_tier = [string]$m.ram_tier }
        if ($m.PSObject.Properties['big_ram'])  { $r.big_ram = [bool]$m.big_ram }
        if ($m.PSObject.Properties['agent_ctx_tokens'] -and $m.agent_ctx_tokens) { $r.agent_ctx_tokens = [int]$m.agent_ctx_tokens }
        return $r
      }
    } catch { }
  }
  # 2) detect.ps1 JSON (last-line verdict), if a caller captured one to a file.
  if ($DetectJsonPath -and (Test-Path $DetectJsonPath)) {
    try {
      $d = Get-Content -Raw -Path $DetectJsonPath -Encoding UTF8 | ConvertFrom-Json
      if ($d.PSObject.Properties['profile'] -and $d.profile) {
        $r.profile = [string]$d.profile
        $r.profile_src = 'detect.ps1'
        if ($d.PSObject.Properties['ram_tier']) { $r.ram_tier = [string]$d.ram_tier }
        if ($d.PSObject.Properties['big_ram'])  { $r.big_ram = [bool]$d.big_ram }
        return $r
      }
    } catch { }
  }
  # 3) OFFLOAD_PROFILE env override (last resort; no ram_tier signal).
  if ($env:OFFLOAD_PROFILE) {
    $r.profile = $env:OFFLOAD_PROFILE.Trim()
    $r.profile_src = 'OFFLOAD_PROFILE'
    if ($env:OFFLOAD_RAM_TIER) { $r.ram_tier = $env:OFFLOAD_RAM_TIER.Trim() }
    return $r
  }
  return $r
}

# Read the PROJECTED serving params for a profile id from profiles.json. Pure fn of the
# on-disk file + the id. Returns $null when the file/profile is absent (caller records
# 'profile not in profiles.json' honestly). Keys mirror the profiles.json schema:
#   ctx_size(int), kv_type, moe_26b, flash_attn, resident_tier, include_26b(bool),
#   dual_resident(bool), backend, notes
function Get-ProjectedProfile {
  param([string]$ProfileId, [string]$ProfilesJsonPath)
  if (-not $ProfileId) { return $null }
  if (-not (Test-Path $ProfilesJsonPath)) { return $null }
  try {
    $doc = Get-Content -Raw -Path $ProfilesJsonPath -Encoding UTF8 | ConvertFrom-Json
  } catch { return $null }
  if (-not $doc.profiles.PSObject.Properties[$ProfileId]) { return $null }
  $p = $doc.profiles.$ProfileId
  $dual = $false
  if ($p.PSObject.Properties['dual_resident']) { $dual = [bool]$p.dual_resident }
  return @{
    ctx_size      = [int]$p.ctx_size
    kv_type       = [string]$p.kv_type
    moe_26b       = [string]$p.moe_26b
    flash_attn    = [string]$p.flash_attn
    resident_tier = [string]$p.resident_tier
    include_26b   = [bool]$p.include_26b
    dual_resident = $dual
    backend       = [string]$p.backend
    notes         = [string]$p.notes
  }
}

# Load+serve a given model gguf on a standalone probe server at a specific ctx + KV type,
# probe /health for readiness (records cold-load wall time), then OPTIONALLY run one warm
# generation through it to measure decode tok/s, then tear it down. This is the H3 workhorse
# for: the profile ctx-ceiling check (step 2), the 26B --cpu-moe decode tok/s (step 3), and
# per-tier cold-swap time (step 4). It NEVER touches the running swap/yaml (own port).
# Returns {ok=$bool; detail; cold_load_s; tps} (tps=$null unless -WarmDecode and it succeeds).
#   -ModelFile   gguf filename under $modelDir
#   -Ctx         --ctx-size to load at
#   -KvType      q8_0|f16 (kept symmetric across K and V)
#   -CpuMoe      add --cpu-moe (experts in RAM) - the 26B reduce path
#   -FlashAttn   on|off (default on; the cpu backend template omits it, handled by caller)
#   -WarmDecode  after /health, issue one chat and time the generation for tok/s
#   -GenPrompt   the warm-decode prompt (J1: the FA+q8-KV canary needs a FIXED prompt
#                whose temp-0 text it compares across KV types)
# J1: the result also carries text (the warm generation, '' unless -WarmDecode) and
# fa_state ('on'|'off'|$null) scanned from the server log — see Get-FaStateFromLog.
function Invoke-ProbeLoad {
  param(
    [string]$ModelFile,
    [int]$Ctx,
    [string]$KvType = 'q8_0',
    [switch]$CpuMoe,
    [string]$FlashAttn = 'on',
    [switch]$WarmDecode,
    [string]$GenPrompt = 'Count from one to forty in words, comma-separated, then stop.',
    [int]$TimeoutSec = 180
  )
  $llamaServer = Join-Path $llamaDir 'llama-server.exe'
  if (-not $ModelFile) { return @{ ok = $false; detail = 'no model file specified'; cold_load_s = $null; tps = $null } }
  $modelPath = Join-Path $modelDir $ModelFile
  if (-not (Test-Path $llamaServer)) { return @{ ok = $false; detail = 'llama-server.exe absent'; cold_load_s = $null; tps = $null } }
  if (-not (Test-Path $modelPath))   { return @{ ok = $false; detail = "model gguf absent: $ModelFile"; cold_load_s = $null; tps = $null } }
  $probePort = 18804
  if (-not (Test-PortFree $probePort)) { return @{ ok = $false; detail = "probe port $probePort busy"; cold_load_s = $null; tps = $null } }
  # -ngl: cpu backend has none; otherwise offload all non-expert layers (99/999 both mean "all").
  $ngl = if ($backend -eq 'cpu') { 0 } else { 999 }
  $logFile = Join-Path $env:TEMP ("offload-selftest-probeload-{0}.log" -f $PID)
  $svArgs = New-Object System.Collections.Generic.List[string]
  $svArgs.AddRange([string[]]@('-m', $modelPath, '-c', "$Ctx"))
  if ($backend -ne 'cpu') { $svArgs.AddRange([string[]]@('-ngl', "$ngl")) }
  if ($CpuMoe)            { $svArgs.Add('--cpu-moe') }
  if ($FlashAttn -eq 'on' -and $backend -ne 'cpu') { $svArgs.AddRange([string[]]@('--flash-attn', 'on')) }
  $svArgs.AddRange([string[]]@('--cache-type-k', $KvType, '--cache-type-v', $KvType,
                             '--jinja', '--no-webui', '--parallel', '1',
                             '--port', "$probePort", '--host', '127.0.0.1',
                             '-lv', '10'))   # J1: default verbosity omits the flash_attn state line;
                                             # -lv 10 prints "llama_context: flash_attn = enabled|disabled"
                                             # (verified live on a Jul-2026 build) for Get-FaStateFromLog
  $proc = $null
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  try {
    $proc = Start-Process -FilePath $llamaServer -ArgumentList ([string[]]$svArgs) -PassThru -NoNewWindow `
              -RedirectStandardError $logFile -RedirectStandardOutput "$logFile.out"
    $healthy = $false
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
      if ($proc.HasExited) {
        $sw.Stop()
        $tail = ''
        try { $tail = (Get-Content -Path $logFile -Tail 4 -ErrorAction SilentlyContinue) -join ' | ' } catch { }
        # J1: at -lv 10 the alloc/OOM line can scroll out of the 4-line tail behind debug
        # chatter, which would make Test-AllocFailure misclassify a real OOM as non-alloc
        # (silently skipping the ctx-downshift remediation). Scan the WHOLE log for the
        # alloc-class marker and surface that line in the detail.
        try {
          $whole = Get-Content -Raw -Path $logFile -ErrorAction SilentlyContinue
          if ($whole) {
            $am = [regex]::Match($whole, '(?im)^.*(VK_ERROR_OUT_OF_DEVICE_MEMORY|out of device memory|CUDA (error )?out of memory|cudaErrorMemoryAllocation|device[- ]lost|VK_ERROR_DEVICE_LOST|failed to allocate|unable to allocate).*$')
            if ($am.Success) { $tail = ($am.Value.Trim() + ' | ' + $tail) }
          }
        } catch { }
        return @{ ok = $false; detail = ("server exited early (code {0}) {1}" -f $proc.ExitCode, $tail); cold_load_s = $null; tps = $null; text = ''; fa_state = $null }
      }
      $h = Invoke-JsonGet "http://127.0.0.1:$probePort/health" 4
      if ($h) { $healthy = $true; break }
      Start-Sleep -Milliseconds 500
    }
    $sw.Stop()
    if (-not $healthy) { return @{ ok = $false; detail = "no /health within ${TimeoutSec}s at ctx=$Ctx kv=$KvType"; cold_load_s = [math]::Round($sw.Elapsed.TotalSeconds, 2); tps = $null; text = ''; fa_state = $null } }
    $coldS = [math]::Round($sw.Elapsed.TotalSeconds, 2)
    # J1: scan the server log for the flash-attention state (see Get-FaStateFromLog).
    # llama-server logs to stderr ($logFile); scan stdout too, defensively.
    $faState = $null
    try {
      $logTxt = ''
      foreach ($lf in @($logFile, "$logFile.out")) {
        if (Test-Path $lf) { $logTxt += (Get-Content -Raw -Path $lf -ErrorAction SilentlyContinue) + "`n" }
      }
      $faState = Get-FaStateFromLog -LogText $logTxt
    } catch { $faState = $null }
    $tps = $null
    $genText = ''
    $detail = "loaded+healthy at ctx=$Ctx kv=$KvType$(if ($CpuMoe){' --cpu-moe'}) on a standalone probe server"
    if ($WarmDecode) {
      # One warm generation directly against the probe server's OpenAI endpoint; time the decode.
      $body = @{ model = 'probe'; messages = @(@{ role = 'user'; content = $GenPrompt });
                 max_tokens = 160; temperature = 0; stream = $false } | ConvertTo-Json -Depth 6 -Compress
      $dsw = [System.Diagnostics.Stopwatch]::StartNew()
      try {
        $rr = Invoke-RestMethod -Uri "http://127.0.0.1:$probePort/v1/chat/completions" -Method Post `
                -ContentType 'application/json' -Body $body -TimeoutSec 300
        $dsw.Stop()
        $ct = 0
        if ($rr.usage -and $rr.usage.completion_tokens) { $ct = [int]$rr.usage.completion_tokens }
        if ($ct -gt 0 -and $dsw.Elapsed.TotalSeconds -gt 0) { $tps = [math]::Round($ct / $dsw.Elapsed.TotalSeconds, 1) }
        if ($rr.choices -and $rr.choices[0].message) { $genText = [string]$rr.choices[0].message.content }
        $detail = "$detail; warm decode $ct tok in $([math]::Round($dsw.Elapsed.TotalSeconds,2))s"
      } catch {
        $dsw.Stop()
        $detail = "$detail; warm decode FAILED: $($_.Exception.Message)"
      }
    }
    return @{ ok = $true; detail = $detail; cold_load_s = $coldS; tps = $tps; text = $genText; fa_state = $faState }
  } catch {
    return @{ ok = $false; detail = "probe threw: $($_.Exception.Message)"; cold_load_s = $null; tps = $null; text = ''; fa_state = $null }
  } finally {
    if ($proc -and -not $proc.HasExited) { try { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue } catch { } }
    try {
      $kids = Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" -ErrorAction SilentlyContinue |
                Where-Object { $_.CommandLine -and ($_.CommandLine -match [regex]::Escape("127.0.0.1:$probePort")) }
      foreach ($k in $kids) { try { Stop-Process -Id $k.ProcessId -Force -ErrorAction SilentlyContinue } catch { } }
    } catch { }
    $pd = (Get-Date).AddSeconds(8)
    while ((Get-Date) -lt $pd) { if (Test-PortFree $probePort) { break }; Start-Sleep -Milliseconds 400 }
    Get-ChildItem $env:TEMP -Filter ("offload-selftest-probeload-{0}*" -f $PID) -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  }
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
  agent           = [ordered]@{
    tier            = $AGENT_TIER
    served_ctx      = $null      # int; measured from the rendered yaml if readable, else computed default
    served_ctx_src  = 'computed' # 'measured' (read from yaml) | 'computed' (fell back to template default 8192)
    input_budget    = $null      # int; served_ctx - output-reserve - safety-margin
    larger_ctx      = $AGENT_LARGER_CTX
    larger_ctx_ok   = $null      # $true|$false measured load at larger ctx; $null = not attempted this run
    larger_ctx_src  = 'not-attempted' # 'measured' | 'computed' | 'not-attempted'
    kv_mb_approx    = $null      # approx KV-cache MB for larger_ctx (computed from ctx*layers*heads est.)
    microbench_tps  = $null      # generation tok/s of the one warm edit-style call
    microbench_pp_tps = $null    # prompt-eval (prefill) tok/s if the server reports it, else $null
    microbench_ok   = $false     # $true iff the call returned a non-empty, well-formed structured answer
    skipped         = $false     # $true iff the agent tier was absent / unreachable this run
    notes           = $null
  }
  # H3: measure-at-install ledger. Turns the PROJECTED profile (profiles.json) into
  # MEASUREMENTS on THIS box; anything this host cannot measure is marked projected/
  # skipped/measure-on-target (NEVER reported as measured). The 'tuned' sub-block holds
  # measured values the operator/installer should apply OVER the projected profile.
  profile_measure = [ordered]@{
    profile         = $null       # active profile id (from installed.json / detect / OFFLOAD_PROFILE)
    profile_src     = 'none'      # where the profile came from
    ram_tier        = $null       # high|mid|low|min (drives the 26B cpu-moe RAM gate)
    projected       = [ordered]@{ # the profiles.json PROJECTED values for this profile (unmeasured)
      ctx_size      = $null
      kv_type       = $null
      moe_26b       = $null
      flash_attn    = $null
      resident_tier = $null
      include_26b   = $null
      dual_resident = $null
    }
    ctx             = [ordered]@{ # step 2: does the profile's projected ctx actually load (no OOM)?
      projected_ctx = $null       # profiles.json ctx_size
      measured_ctx  = $null       # ctx that actually loaded (== projected, or the downshifted value)
      measured_ctx_ok = $null     # $true measured OK | $false OOM even after downshift | $null not-attempted
      downshifted   = $false      # $true iff the projected ctx OOM'd and we halved+retried
      src           = 'not-attempted' # 'measured' | 'not-attempted'
      detail        = $null
    }
    moe26b          = [ordered]@{ # step 3: 26B --cpu-moe decode tok/s ("reduce not enable" number)
      applicable    = $false      # profile includes 26B via cpu_moe AND the ram gate passed
      installed     = $false      # the 26B gguf is on disk
      moe26b_tps    = $null       # measured decode tok/s; $null if skipped
      src           = 'skipped'   # 'measured' | 'skipped'
      detail        = $null
    }
    cold_swap       = @()         # step 4: per installed tier {tier, cold_swap_s} (load-from-cold latency)
    q8_kv           = [ordered]@{ # step 5: q8_0 KV outcome (esp. AMD/Vulkan f16-conservative note)
      projected_kv  = $null
      measured_ok   = $null       # $true iff a q8_0 load succeeded this run (from step 2) | $null n/a
      note          = $null
    }
    dual_gpu        = [ordered]@{ # step 6: needs >=2 GPUs - recorded not-applicable on a single-GPU host
      applicable    = $false
      status        = 'not-applicable-on-this-host'
      detail        = $null
    }
    optane          = [ordered]@{ # step 6: needs config-#4 Optane hardware - measure-on-target only
      applicable    = $false
      status        = 'measure-on-target'
      detail        = $null
    }
    tuned           = [ordered]@{ # step 7: measured overrides the installer should apply (else projected)
      ctx_size      = $null       # downshifted ctx if OOM, else the projected ctx (confirmed loadable)
      kv_type       = $null       # confirmed KV type
      moe26b_tps    = $null       # measured 26B decode tok/s (or $null)
      source        = 'projected' # 'measured' (at least one value measured this box) | 'projected'
      notes         = $null
    }
  }
  # J1: the H3 canary ledger — the promotion gates for the AMD/Vulkan tiers (and any
  # box forcing OFFLOAD_SELFTEST_CANARIES=1). Every entry starts 'skipped' and is only
  # upgraded by an actual measurement; the SETUP-AGENT.md amd-rdna3 chapter tells the
  # installing agent which config promotions each PASS authorizes (ctx 16K->32K,
  # f16->q8_0 KV, 26B cpu-moe->full-offload). Nothing here is auto-applied.
  canaries        = [ordered]@{
    ran              = $false
    gate             = $null      # why the suite ran / was skipped
    fa_q8kv          = [ordered]@{ status = 'skipped'; fa_confirmed = $null; overlap = $null; detail = $null }
    moe_full_offload = [ordered]@{ status = 'skipped'; full_tps = $null; cpu_moe_tps = $null; promote = $null; detail = $null }
    ctx_sweep        = [ordered]@{ status = 'skipped'; results = @(); max_ok_ctx = $null; detail = $null }
    bench            = [ordered]@{ status = 'skipped'; pp512_tps = $null; tg128_tps = $null; detail = $null }
    swap_leak        = [ordered]@{ status = 'skipped'; servers_after = $null; detail = $null }
    embedder         = [ordered]@{ status = 'skipped'; cos_related = $null; cos_unrelated = $null; reranker = 'skipped: no reranker model is installed by this stack (memory_stack rerank seat is operator-provisioned)'; detail = $null }
    whisper          = [ordered]@{ status = 'skipped'; detail = 'no whisper seat is installed by this stack; when binding one on an AMD iGPU, whisper.cpp >= 1.8.3 is the FLOOR (earlier builds are not viable on AMD iGPUs; ~3-4x realtime expected)' }
  }
  # J2: the first media selftest leg. Runs when the sd.cpp media tier is installed
  # (install.ps1 Step 5b); reference render = the install-integrity gate, gpu_vae =
  # the promotion trial (drop the CPU-VAE workaround only on a measured clean+faster
  # run). OFFLOAD_SELFTEST_MEDIA=0 skips, =1 forces the attempt.
  media           = [ordered]@{
    ran     = $false
    gate    = $null
    render  = [ordered]@{ status = 'skipped'; seconds = $null; bytes = $null; distinct_colors = $null; detail = $null }
    gpu_vae = [ordered]@{ status = 'skipped'; seconds = $null; promote = $null; detail = $null }
  }
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
  # J1: on Vulkan, pin the SAME device the rendered template pins (GGML_VK_VISIBLE_DEVICES=0)
  # for every child this script spawns (probe servers, llama-bench) - otherwise on a
  # multi-ICD box the canaries could measure a different adapter than production serving.
  # Process-scoped env; dies with this script. An operator override already set wins.
  if ($backend -eq 'vulkan' -and -not $env:GGML_VK_VISIBLE_DEVICES) { $env:GGML_VK_VISIBLE_DEVICES = '0' }

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

  # --- Agent telemetry: context headroom + one agentic micro-bench (independent section) -----
  # NEW telemetry (does not alter eviction/canary/remediation above). Records, for the agent-driving
  # tier: the served ctx (measured from the rendered yaml when readable), the computed agent INPUT
  # budget, an OPTIONAL larger-ctx load probe (measured on a standalone server, never touching the
  # running swap/yaml), and ONE warm edit-style micro-bench through the same llama-swap the tiers use.
  LogStep "agent telemetry: context headroom + one agentic micro-bench (tier=$AGENT_TIER)"
  try {
    # 1a. Served ctx: prefer the value actually in the rendered yaml (measured), else template default.
    $servedCtx = Get-ServedCtx -ConfigPath $yamlPath
    if ($null -ne $servedCtx) {
      $receipt.agent.served_ctx = $servedCtx
      $receipt.agent.served_ctx_src = 'measured'
    } else {
      $servedCtx = 8192
      $receipt.agent.served_ctx = $servedCtx
      $receipt.agent.served_ctx_src = 'computed'
    }
    # 1b. Agent INPUT budget = served ctx - output reservation - safety margin (always computed).
    $inputBudget = $servedCtx - $AGENT_OUTPUT_RESERVE - $AGENT_SAFETY_MARGIN
    if ($inputBudget -lt 0) { $inputBudget = 0 }
    $receipt.agent.input_budget = $inputBudget
    LogOk ("served_ctx={0} ({1}) | input_budget={2} = {0}-{3}-{4} (computed)" -f `
           $servedCtx, $receipt.agent.served_ctx_src, $inputBudget, $AGENT_OUTPUT_RESERVE, $AGENT_SAFETY_MARGIN)

    # Is the agent tier actually installed this run? (file existence, same rule as the tier loop)
    $agentTierSpec = $TIER_SPEC | Where-Object { $_.id -eq $AGENT_TIER } | Select-Object -First 1
    $agentTierPresent = $false
    if ($agentTierSpec) { $agentTierPresent = Test-Path (Join-Path $modelDir $agentTierSpec.file) }

    # 1c. Larger-ctx headroom probe (optional; only meaningful with a GPU backend + the tier present).
    # OFFLOAD_SELFTEST_SKIP_CTXPROBE=1 skips the load (keeps the run fast); we then record kv_mb as
    # a pure computation with larger_ctx_ok left $null (not-attempted) - honest about measured vs computed.
    $kvApprox = Get-KvMbApprox -Ctx $AGENT_LARGER_CTX
    $receipt.agent.kv_mb_approx = $kvApprox
    if ($env:OFFLOAD_SELFTEST_SKIP_CTXPROBE -eq '1' -or -not $agentTierPresent -or $backend -eq 'cpu') {
      $receipt.agent.larger_ctx_ok = $null
      $receipt.agent.larger_ctx_src = 'computed'
      $why = if (-not $agentTierPresent) { "$AGENT_TIER gguf absent" } elseif ($backend -eq 'cpu') { 'cpu backend' } else { 'skip flag set' }
      LogWarn ("larger-ctx load NOT attempted ({0}); kv_mb~{1}MB@{2} is COMPUTED only" -f $why, $kvApprox, $AGENT_LARGER_CTX)
    } else {
      $probe = Test-LargerCtxLoad -Ctx $AGENT_LARGER_CTX -TimeoutSec 120
      $receipt.agent.larger_ctx_ok = [bool]$probe.ok
      $receipt.agent.larger_ctx_src = 'measured'
      if ($probe.ok) { LogOk ("larger-ctx load MEASURED ok at {0} (kv~{1}MB approx): {2}" -f $AGENT_LARGER_CTX, $kvApprox, $probe.detail) }
      else { LogWarn ("larger-ctx load MEASURED fail at {0}: {1} (served {2} unaffected)" -f $AGENT_LARGER_CTX, $probe.detail, $servedCtx) }
    }

    # 2. Agentic micro-bench: ONE warm edit-style call through llama-swap. The tier loop already
    # cold-loaded offload-e4b, so this hits a resident model (warm tok/s). Well-formed = the model
    # returned the corrected line containing the fix, non-empty. Skip honestly if the tier is absent.
    if (-not $agentTierPresent) {
      $receipt.agent.microbench_ok = $false
      $receipt.agent.skipped = $true
      $receipt.agent.notes = "micro-bench skipped: agent tier '$AGENT_TIER' gguf not installed in this stack"
      LogWarn "agentic micro-bench SKIPPED: $($receipt.agent.notes)"
    } else {
      $editPrompt = "You are a code-editing agent. Here is one line of Python with a bug:`n" +
                    "    resutl = a + b`n" +
                    "Reply with ONLY the corrected line, no explanation, no code fence."
      $mb = Invoke-Chat -Model $AGENT_TIER -UserContent $editPrompt -MaxTokens 64 -TimeoutSec 180
      if ($mb.ok -and $mb.text -and $mb.text.Trim().Length -gt 0) {
        $receipt.agent.microbench_tps = $mb.tok_s
        # pp (prefill) tok/s is NOT separable from the OpenAI usage payload (no per-phase timings),
        # so we leave microbench_pp_tps null and say so in notes rather than fabricate a number.
        $receipt.agent.microbench_pp_tps = $null
        # Well-formed = the reply actually applied the fix (contains the corrected identifier 'result').
        $wellFormed = [bool]($mb.text -match '(?i)result')
        $receipt.agent.microbench_ok = $wellFormed
        $receipt.agent.skipped = $false
        $receipt.agent.notes = ("edit-bench: gen {0} tok in {1}s ({2} tok/s), prompt_tokens={3}; well_formed={4}; pp_tps=null (llama-server OpenAI usage has no per-phase timing)" -f `
                                 $mb.tokens, $mb.latency_s, $mb.tok_s, $mb.prompt_tokens, $wellFormed)
        if ($wellFormed) { LogOk ("agentic micro-bench PASS: {0} tok/s warm, well-formed edit" -f $mb.tok_s) }
        else { LogWarn ("agentic micro-bench ran ({0} tok/s) but answer not well-formed (no 'result' token): $($mb.text.Trim())" -f $mb.tok_s) }
      } else {
        $receipt.agent.microbench_ok = $false
        $receipt.agent.skipped = $true
        $receipt.agent.notes = "micro-bench call failed: $($mb.error)"
        LogFail "agentic micro-bench FAILED: $($mb.error)"
      }
    }
  } catch {
    $receipt.agent.microbench_ok = $false
    $receipt.agent.skipped = $true
    $receipt.agent.notes = "agent telemetry section threw: $($_.Exception.Message)"
    LogFail "agent telemetry section threw: $($_.Exception.Message) - continuing to verdict"
  }

  # --- H3: profile_measure - turn PROJECTED profile numbers into MEASUREMENTS ----------------
  # Independent section (own try/catch; records its own status; never affects the verdict rule).
  # Reads the active profile (installed.json -> detect -> OFFLOAD_PROFILE), then measures ONLY
  # what THIS box supports and honestly marks the rest projected/skipped/measure-on-target.
  LogStep "H3 profile_measure: measure projected profile numbers on this box"
  $pm = $receipt.profile_measure
  try {
    # 1. Read the active profile + its PROJECTED serving params.
    $active = Read-ActiveProfile -ManifestPath $manifest -DetectJsonPath $detectJsonPath
    $pm.profile     = $active.profile
    $pm.profile_src = $active.profile_src
    $pm.ram_tier    = $active.ram_tier
    $proj = $null
    if ($active.profile) { $proj = Get-ProjectedProfile -ProfileId $active.profile -ProfilesJsonPath $profilesJsonPath }
    if ($proj) {
      $pm.projected.ctx_size      = $proj.ctx_size
      $pm.projected.kv_type       = $proj.kv_type
      $pm.projected.moe_26b       = $proj.moe_26b
      $pm.projected.flash_attn    = $proj.flash_attn
      $pm.projected.resident_tier = $proj.resident_tier
      $pm.projected.include_26b   = $proj.include_26b
      $pm.projected.dual_resident = $proj.dual_resident
      LogOk ("profile={0} (src={1}, ram_tier={2}) | PROJECTED ctx={3} kv={4} moe_26b={5} resident={6}" -f `
             $active.profile, $active.profile_src, $active.ram_tier, $proj.ctx_size, $proj.kv_type, $proj.moe_26b, $proj.resident_tier)
    } else {
      LogWarn ("no PROJECTED profile resolved (profile='{0}' src='{1}') - profile_measure records measure-on-target only" -f $active.profile, $active.profile_src)
    }

    # gpu_count: live NVIDIA+AMD discrete-adapter count (matches detect.ps1), for the dual-GPU gate.
    $gpuCount = 0
    try {
      $gpuCount = @(Get-CimInstance Win32_VideoController -ErrorAction SilentlyContinue |
                    Where-Object { $_.Name -match 'NVIDIA|AMD|Radeon' }).Count
    } catch { $gpuCount = 0 }

    # Which resident-tier gguf to load for the ctx probe (the profile's resident tier, mapped to a file).
    $residentId = if ($proj) { $proj.resident_tier } else { $AGENT_TIER }
    $residentSpec = $TIER_SPEC | Where-Object { $_.id -eq $residentId } | Select-Object -First 1
    if (-not $residentSpec) { $residentSpec = $TIER_SPEC | Where-Object { $_.id -eq 'offload-e4b' } | Select-Object -First 1 }
    $residentPresent = $false
    if ($residentSpec) { $residentPresent = Test-Path (Join-Path $modelDir $residentSpec.file) }

    # 2. Measure the projected ctx actually loads (no OOM); DOWNSHIFT (halve, retry once) if it OOMs.
    $skipProbe = ($env:OFFLOAD_SELFTEST_SKIP_CTXPROBE -eq '1') -or ($backend -eq 'cpu') -or (-not $residentPresent) -or (-not $proj)
    if ($skipProbe) {
      $why = if (-not $proj) { 'no projected profile' } elseif (-not $residentPresent) { "$residentId gguf absent" } elseif ($backend -eq 'cpu') { 'cpu backend' } else { 'skip flag set' }
      $pm.ctx.projected_ctx = if ($proj) { $proj.ctx_size } else { $null }
      $pm.ctx.src = 'not-attempted'
      $pm.ctx.detail = "ctx load NOT attempted ($why); projected ctx recorded as measure-on-target"
      LogWarn $pm.ctx.detail
    } else {
      $pm.ctx.projected_ctx = $proj.ctx_size
      $kv = $proj.kv_type
      $fa = $proj.flash_attn
      $tryCtx = $proj.ctx_size
      $residentIsMoe = ($residentId -eq 'gemma4-26b-a4b')  # if a profile ever made 26B the resident tier
      LogStep ("  ctx probe: loading {0} at projected ctx={1} kv={2}" -f $residentId, $tryCtx, $kv)
      $r1 = Invoke-ProbeLoad -ModelFile $residentSpec.file -Ctx $tryCtx -KvType $kv -FlashAttn $fa -CpuMoe:$residentIsMoe -TimeoutSec 180
      if ($r1.ok) {
        $pm.ctx.measured_ctx = $tryCtx
        $pm.ctx.measured_ctx_ok = $true
        $pm.ctx.downshifted = $false
        $pm.ctx.src = 'measured'
        $pm.ctx.detail = $r1.detail
        LogOk ("ctx MEASURED ok at projected {0} (kv={1}): {2}" -f $tryCtx, $kv, $r1.detail)
      } elseif (Test-AllocFailure $r1.detail) {
        # DOWNSHIFT: halve ctx (floor 4096) and retry once, mirroring the Invoke-Remediate26B pattern.
        $downCtx = [int][math]::Max(4096, [math]::Floor($tryCtx / 2))
        LogWarn ("projected ctx {0} OOM'd ({1}); DOWNSHIFTING to {2} and retrying once" -f $tryCtx, $r1.detail, $downCtx)
        $r2 = Invoke-ProbeLoad -ModelFile $residentSpec.file -Ctx $downCtx -KvType $kv -FlashAttn $fa -CpuMoe:$residentIsMoe -TimeoutSec 180
        $pm.ctx.downshifted = $true
        $pm.ctx.src = 'measured'
        $receipt.remediations += ,([ordered]@{ tier = $residentId; action = ("downshift ctx {0}->{1}" -f $tryCtx, $downCtx); outcome = (if ($r2.ok) { 'pass' } else { 'fail' }) })
        if ($r2.ok) {
          $pm.ctx.measured_ctx = $downCtx
          $pm.ctx.measured_ctx_ok = $true
          $pm.ctx.detail = ("projected {0} OOM'd; downshifted ctx={1} loads OK - operator should use {1}. {2}" -f $tryCtx, $downCtx, $r2.detail)
          LogOk ("ctx DOWNSHIFTED and MEASURED ok at {0} (operator should use {0}, not projected {1})" -f $downCtx, $tryCtx)
        } else {
          $pm.ctx.measured_ctx = $null
          $pm.ctx.measured_ctx_ok = $false
          $pm.ctx.detail = ("projected {0} AND downshifted {1} both failed to load: {2}" -f $tryCtx, $downCtx, $r2.detail)
          LogFail $pm.ctx.detail
        }
      } else {
        # Non-alloc failure (probe port busy, exe absent, model gguf absent, or the probe threw) -
        # the load NEVER started, so NOTHING was measured. Marking this 'measured' would be dishonest:
        # it would (a) skip the does_not_prove gate and (b) let tuned.source read 'measured' while
        # tuned.ctx_size falls back to the PROJECTED value. Record 'not-attempted' - no OOM, no downshift.
        $pm.ctx.measured_ctx_ok = $false
        $pm.ctx.src = 'not-attempted'
        $pm.ctx.detail = ("ctx probe at {0} failed (non-alloc): {1}" -f $tryCtx, $r1.detail)
        LogWarn $pm.ctx.detail
      }
    }

    # 3. 26B --cpu-moe decode tok/s (the "reduce not enable" number). Applicable iff the profile
    # INCLUDES the 26B via cpu_moe AND the ram gate passed (ram_tier mid|high). Measured only if the
    # 26B gguf is installed. This box (ampere-8/mid) IS in the cpu_moe-with-RAM-path case.
    $moe26bId   = 'gemma4-26b-a4b'
    $moe26bSpec = $TIER_SPEC | Where-Object { $_.id -eq $moe26bId } | Select-Object -First 1
    $moe26bInstalled = $false
    if ($moe26bSpec) { $moe26bInstalled = Test-Path (Join-Path $modelDir $moe26bSpec.file) }
    $pm.moe26b.installed = $moe26bInstalled
    $ramOk = ($active.ram_tier -in @('mid','high'))
    $moeApplicable = [bool]($proj -and $proj.include_26b -and ($proj.moe_26b -eq 'cpu_moe') -and $ramOk)
    $pm.moe26b.applicable = $moeApplicable
    if ($moeApplicable -and $moe26bInstalled -and $backend -ne 'cpu' -and -not $skipProbe) {
      LogStep "  26B --cpu-moe decode probe (warm generation for tok/s)"
      $rm = Invoke-ProbeLoad -ModelFile $moe26bSpec.file -Ctx ([int]$pm.projected.ctx_size) -KvType $proj.kv_type -CpuMoe -FlashAttn $proj.flash_attn -WarmDecode -TimeoutSec 300
      if ($rm.ok -and $null -ne $rm.tps) {
        $pm.moe26b.moe26b_tps = $rm.tps
        $pm.moe26b.src = 'measured'
        $pm.moe26b.detail = "26B --cpu-moe measured $($rm.tps) tok/s (RAM-speed-bound; this is why 26B is REDUCE not ENABLE on cpu-moe). $($rm.detail)"
        LogOk ("26B --cpu-moe MEASURED {0} tok/s decode" -f $rm.tps)
      } else {
        $pm.moe26b.src = 'skipped'
        $pm.moe26b.detail = "26B --cpu-moe load/decode did not complete: $($rm.detail)"
        LogWarn $pm.moe26b.detail
      }
    } else {
      $reason = if (-not $moeApplicable) {
        if (-not $proj) { 'no projected profile' }
        elseif (-not $proj.include_26b) { 'profile does not include 26B' }
        elseif ($proj.moe_26b -ne 'cpu_moe') { "26B placement is '$($proj.moe_26b)', not cpu_moe (measure GPU-resident 26B on the target rig)" }
        elseif (-not $ramOk) { "ram_tier=$($active.ram_tier) has no RAM path for cpu-moe (needs mid|high)" }
        else { 'not applicable' }
      } elseif (-not $moe26bInstalled) { '26B gguf not installed in this stack' }
      elseif ($skipProbe) {
        # Honest labeling: name the ACTUAL cause of the skip. The moe-tps probe is currently
        # coupled to $skipProbe, so OFFLOAD_SELFTEST_SKIP_CTXPROBE=1 skips it as collateral even on
        # a box that HAS the 26B installed - say so explicitly rather than blaming a generic 'ctx probe'.
        if ($env:OFFLOAD_SELFTEST_SKIP_CTXPROBE -eq '1') { 'moe-tps probe skipped as collateral of OFFLOAD_SELFTEST_SKIP_CTXPROBE=1 (26B is installed; unset the flag to measure decode tok/s)' }
        elseif ($backend -eq 'cpu') { 'cpu backend (no GPU cpu-moe measurement)' }
        elseif (-not $residentPresent) { 'resident-tier gguf absent (ctx probe not attempted)' }
        else { 'ctx probe not attempted' }
      }
      else { 'cpu backend' }
      $pm.moe26b.src = 'skipped'
      $pm.moe26b.detail = "26B cpu-moe tok/s SKIPPED: $reason"
      LogWarn $pm.moe26b.detail
    }

    # 4. Cold-swap time per installed tier (load-from-cold latency) - feeds the two-tier one-swap cost.
    if ($skipProbe) {
      LogWarn "cold-swap timing NOT attempted (ctx probe skipped / cpu backend / no projected profile)"
    } else {
      LogStep "  cold-swap timing per installed tier (cold load-from-disk latency)"
      foreach ($tier in $installedTiers) {
        $isMoe = ($tier.id -eq 'gemma4-26b-a4b')
        # 26B loads with the profile's cpu_moe path only if applicable; else plain (GPU) - it just
        # needs to load to time the cold swap, we don't decode here.
        $useCpuMoe = ($isMoe -and $moeApplicable)
        $kvForTier = if ($proj) { $proj.kv_type } else { 'q8_0' }
        $faForTier = if ($proj) { $proj.flash_attn } else { 'on' }
        $ctxForTier = if ($proj) { [int]$proj.ctx_size } else { 8192 }
        $rc = Invoke-ProbeLoad -ModelFile $tier.file -Ctx $ctxForTier -KvType $kvForTier -FlashAttn $faForTier -CpuMoe:$useCpuMoe -TimeoutSec 240
        $cs = if ($rc.ok) { $rc.cold_load_s } else { $null }
        $pm.cold_swap += ,([ordered]@{ tier = $tier.id; cold_swap_s = $cs; ok = [bool]$rc.ok })
        if ($rc.ok) { LogOk ("cold-swap {0}: {1}s (load-from-cold)" -f $tier.id, $cs) }
        else { LogWarn ("cold-swap {0}: load failed ({1})" -f $tier.id, $rc.detail) }
      }
    }

    # 5. q8_0 KV outcome. If the profile projects q8_0, its q8 load was already exercised in step 2
    # (measured_ctx_ok). For AMD (rdna3/gcn) where profiles.json set f16 conservatively, note that
    # q8_0 is worth TRYING at install (do NOT force it here).
    if ($proj) {
      $pm.q8_kv.projected_kv = $proj.kv_type
      if ($proj.kv_type -eq 'q8_0') {
        $pm.q8_kv.measured_ok = $pm.ctx.measured_ctx_ok   # $true iff the q8 load in step 2 succeeded
        $pm.q8_kv.note = if ($pm.ctx.measured_ctx_ok -eq $true) { 'q8_0 KV load confirmed via the ctx probe (step 2)' }
                         elseif ($pm.ctx.measured_ctx_ok -eq $false) { 'q8_0 KV load did NOT confirm - see ctx.detail' }
                         else { 'q8_0 KV load not attempted this run' }
      } elseif ($proj.kv_type -eq 'f16' -and $active.profile -match '^amd-') {
        $pm.q8_kv.measured_ok = $null
        $pm.q8_kv.note = 'profiles.json set f16 CONSERVATIVELY for this AMD/Vulkan profile; q8_0 KV is worth TRYING at install if the Vulkan FA + q8-KV canary is stable (NOT forced here).'
      } else {
        $pm.q8_kv.measured_ok = $null
        $pm.q8_kv.note = "projected kv_type=$($proj.kv_type); no q8 change indicated"
      }
      LogOk ("q8_kv: projected={0} note={1}" -f $proj.kv_type, $pm.q8_kv.note)
    }

    # 6. dual-GPU residency + Optane: this host lacks the hardware. Provide the code PATH (guarded)
    # but NEVER claim measured. dual-gpu applicable iff >=2 discrete GPUs AND the dual-gpu profile.
    $dualApplicable = [bool]($gpuCount -ge 2 -and $proj -and $proj.dual_resident)
    $pm.dual_gpu.applicable = $dualApplicable
    if ($dualApplicable) {
      # CODE PATH for the real dual-GPU rig (configs #3/#4): pin 26B to device 0, E4B to device 1,
      # confirm BOTH stay resident (two standalone servers, CUDA_VISIBLE_DEVICES per server, both
      # /health simultaneously). Not executed here because this host has <2 GPUs (guarded above).
      $pm.dual_gpu.status = 'measure-on-target'
      $pm.dual_gpu.detail = "dual-gpu profile on a >=2-GPU host: run the two-resident residency check (26B->CUDA_VISIBLE_DEVICES=0, E4B->=1, both -ngl 99, both /health at once). NOT measured here."
      LogWarn "dual-gpu applicable but NOT measured in this pass (code path present; run on the two-card rig)"
    } else {
      $pm.dual_gpu.status = 'not-applicable-on-this-host'
      $pm.dual_gpu.detail = "single-GPU host (gpu_count=$gpuCount) / non-dual profile: two-model residency is measure-on-target (configs #3/#4)."
      LogOk ("dual-gpu: not-applicable-on-this-host (gpu_count={0})" -f $gpuCount)
    }
    # Optane (config #4 staging/mmap): detect cannot see an Optane drive; NEVER claimed measured here.
    $pm.optane.applicable = $false
    $pm.optane.status = 'measure-on-target'
    $pm.optane.detail = 'Optane cold-load/mmap latency vs RAM is config-#4-only hardware; measure on the target rig (staging/mmap, never a resident inference tier).'
    LogOk "optane: measure-on-target (config #4 hardware absent here)"

    # 7. tuned summary: measured values that should OVERRIDE the projected profile.
    $anyMeasured = ($pm.ctx.src -eq 'measured') -or ($pm.moe26b.src -eq 'measured')
    if ($proj) {
      # ctx: the downshifted value if we downshifted, else the projected ctx CONFIRMED loadable, else projected (unconfirmed).
      if ($pm.ctx.measured_ctx_ok -eq $true -and $null -ne $pm.ctx.measured_ctx) { $pm.tuned.ctx_size = $pm.ctx.measured_ctx }
      else { $pm.tuned.ctx_size = $proj.ctx_size }
      $pm.tuned.kv_type    = $proj.kv_type
      $pm.tuned.moe26b_tps = $pm.moe26b.moe26b_tps
      $pm.tuned.source     = if ($anyMeasured) { 'measured' } else { 'projected' }
      $tunedNotes = New-Object System.Collections.Generic.List[string]
      if ($pm.ctx.downshifted -and $pm.ctx.measured_ctx_ok -eq $true) { $tunedNotes.Add("apply ctx_size=$($pm.ctx.measured_ctx) (projected $($proj.ctx_size) OOM'd)") }
      elseif ($pm.ctx.measured_ctx_ok -eq $true) { $tunedNotes.Add("projected ctx_size=$($proj.ctx_size) confirmed loadable") }
      if ($null -ne $pm.moe26b.moe26b_tps) { $tunedNotes.Add("26B cpu-moe measured $($pm.moe26b.moe26b_tps) tok/s") }
      if ($tunedNotes.Count -eq 0) { $tunedNotes.Add('nothing measured this run; projected values stand') }
      $pm.tuned.notes = ($tunedNotes -join '; ')
      LogOk ("tuned (source={0}): ctx_size={1} kv_type={2} moe26b_tps={3} | {4}" -f `
             $pm.tuned.source, $pm.tuned.ctx_size, $pm.tuned.kv_type, $(if ($null -ne $pm.tuned.moe26b_tps) { $pm.tuned.moe26b_tps } else { 'null' }), $pm.tuned.notes)
    } else {
      $pm.tuned.source = 'projected'
      $pm.tuned.notes = 'no projected profile resolved; nothing to tune'
    }
  } catch {
    $pm.tuned.source = 'projected'
    $pm.tuned.notes = "profile_measure section threw: $($_.Exception.Message)"
    LogFail "profile_measure section threw: $($_.Exception.Message) - continuing to verdict"
  }

  # --- J1: H3 canary suite - the AMD/Vulkan promotion gates ----------------------------------
  # Independent section (own try/catch; never changes the verdict rule). These canaries turn
  # the amd-rdna3 profile's SAFE FLOOR into evidence-backed promotions: each PASS authorizes
  # the installing agent to apply ONE config change per SETUP-AGENT.md (amd-rdna3 chapter) -
  # ctx 16K->32K (ctx_sweep), f16->q8_0 KV (fa_q8kv), 26B cpu-moe->full-offload
  # (moe_full_offload). Nothing is auto-applied here. Default gate: vulkan backend;
  # OFFLOAD_SELFTEST_CANARIES=1 forces on any box, =0 forces off.
  LogStep "H3 canaries: promotion gates (fa_q8kv / moe_full_offload / ctx_sweep / bench / swap_leak / embedder / whisper)"
  $cn = $receipt.canaries
  try {
    $canaryOn = $false
    if ($env:OFFLOAD_SELFTEST_CANARIES -eq '1') { $canaryOn = $true; $cn.gate = 'forced: OFFLOAD_SELFTEST_CANARIES=1' }
    elseif ($env:OFFLOAD_SELFTEST_CANARIES -eq '0') { $cn.gate = 'skipped: OFFLOAD_SELFTEST_CANARIES=0' }
    elseif ($backend -eq 'vulkan') { $canaryOn = $true; $cn.gate = 'vulkan backend (AMD-tier promotion gates run by default)' }
    else { $cn.gate = "skipped: backend=$backend (set OFFLOAD_SELFTEST_CANARIES=1 to force)" }

    if (-not $canaryOn) {
      LogWarn "canary suite $($cn.gate)"
    } else {
      $cn.ran = $true
      Log "  gate: $($cn.gate)"
      # Shared inputs. $proj/$residentSpec come from the profile_measure section; guard
      # partial state (that section has its own try/catch and may have thrown early).
      $cnResidentSpec = $residentSpec
      if (-not $cnResidentSpec) { $cnResidentSpec = $TIER_SPEC | Where-Object { $_.id -eq 'offload-e4b' } | Select-Object -First 1 }
      $cnResidentOk = Test-Path (Join-Path $modelDir $cnResidentSpec.file)
      $cnCtx = 16384; $cnKv = 'f16'; $cnFa = 'on'
      if ($proj) { $cnCtx = [int]$proj.ctx_size; $cnKv = [string]$proj.kv_type; $cnFa = [string]$proj.flash_attn }
      # VRAM-contention mitigation (review finding): the transient swap still holds the
      # LAST-SERVED model resident (ttl 300) while canary probes load standalone on :18804.
      # Harmless on a UMA iGPU (shared memory), but on a discrete card (amd-rdna3-dgpu:
      # 26B ~14.2GB resident of 16GB) the probes would OOM against the resident. Park the
      # swap on the SMALLEST installed tier before probing.
      try {
        $parkTier = $installedTiers | Sort-Object { (Get-Item (Join-Path $modelDir $_.file)).Length } | Select-Object -First 1
        if ($parkTier -and $installedTiers.Count -ge 2) {
          Log "  parking the swap on the smallest tier ($($parkTier.id)) to free memory for the probe servers"
          [void](Invoke-Chat -Model $parkTier.id -UserContent 'Reply with exactly the single word: ok' -MaxTokens 8 -TimeoutSec 300)
        }
      } catch { }
      # fa_q8kv correctness needs a KV-type comparison, not a ctx stress test - and the f16
      # baseline at a 32K profile ctx needs ~2x the KV of the q8_0 target and can OOM where
      # the real config fits. Cap the comparison ctx at 16K (ctx_sweep owns the ctx question).
      $faCtx = [math]::Min($cnCtx, 16384)

      # (1) fa_q8kv - FA + q8_0-KV correctness: FIXED prompt at temp 0, f16-KV baseline vs
      # q8_0-KV run, word-set overlap >= 0.80, AND server-log proof FA is actually ON
      # (the silent-FA-disable mode then fails quantized-V loads on Gemma - hard fail).
      if (-not $cnResidentOk) {
        $cn.fa_q8kv.detail = "resident-tier gguf absent"; LogWarn "fa_q8kv SKIPPED: $($cn.fa_q8kv.detail)"
      } elseif ($backend -eq 'cpu') {
        $cn.fa_q8kv.detail = 'cpu backend (no FA / no GPU KV)'; LogWarn "fa_q8kv SKIPPED: $($cn.fa_q8kv.detail)"
      } else {
        $faPrompt = 'List the first twelve prime numbers in ascending order, comma-separated, then stop.'
        LogStep "  fa_q8kv: f16-KV baseline (fixed prompt, temp 0, ctx=$faCtx)"
        $rf = Invoke-ProbeLoad -ModelFile $cnResidentSpec.file -Ctx $faCtx -KvType 'f16' -FlashAttn 'on' -WarmDecode -GenPrompt $faPrompt -TimeoutSec 240
        LogStep "  fa_q8kv: q8_0-KV run (same prompt)"
        $rq = Invoke-ProbeLoad -ModelFile $cnResidentSpec.file -Ctx $faCtx -KvType 'q8_0' -FlashAttn 'on' -WarmDecode -GenPrompt $faPrompt -TimeoutSec 240
        $cn.fa_q8kv.fa_confirmed = $rq.fa_state
        if (-not $rq.ok) {
          $cn.fa_q8kv.status = 'fail'
          $cn.fa_q8kv.detail = "q8_0-KV load FAILED: $($rq.detail) - stay on the f16 floor"
          LogFail "fa_q8kv: $($cn.fa_q8kv.detail)"
        } elseif ($rq.fa_state -eq 'off') {
          $cn.fa_q8kv.status = 'fail'
          $cn.fa_q8kv.detail = 'server log shows flash-attn OFF despite -fa on (the silent-disable mode) - q8_0 KV is NOT safe; stay on f16 and investigate the driver/build'
          LogFail "fa_q8kv: $($cn.fa_q8kv.detail)"
        } elseif (-not $rf.ok) {
          $cn.fa_q8kv.status = 'warn'
          $cn.fa_q8kv.detail = "q8_0-KV loaded+generated but the f16 baseline failed ($($rf.detail)) - overlap not comparable this run"
          LogWarn "fa_q8kv: $($cn.fa_q8kv.detail)"
        } else {
          $ov = Get-WordOverlap -A $rf.text -B $rq.text
          $cn.fa_q8kv.overlap = $ov
          if ($ov -ge 0.8 -and $rq.text -and $rq.text.Trim().Length -gt 0) {
            if ($rq.fa_state -eq 'on') {
              $cn.fa_q8kv.status = 'pass'
              $cn.fa_q8kv.detail = "q8_0-KV matches f16 at temp 0 (word overlap $ov) with FA confirmed ON in the server log - q8_0 KV promotion authorized"
              LogOk "fa_q8kv PASS: overlap=$ov, FA=on"
            } else {
              $cn.fa_q8kv.status = 'warn'
              $cn.fa_q8kv.detail = "overlap $ov passes but the FA state was NOT found in the server log (unknown, not confirmed) - treat as unconfirmed"
              LogWarn "fa_q8kv: $($cn.fa_q8kv.detail)"
            }
          } else {
            $cn.fa_q8kv.status = 'fail'
            $cn.fa_q8kv.detail = "q8_0-KV text diverged from f16 (word overlap $ov < 0.80) - stay on the f16 floor"
            LogFail "fa_q8kv: $($cn.fa_q8kv.detail)"
          }
        }
      }

      # (2) moe_full_offload - the 26B -ngl 99 FULL-OFFLOAD trial: the upward mirror of the
      # standing Invoke-Remediate26B downshift. On UMA boxes the weights live in GTT/shared
      # memory; research (2026-07-23) measured ~20-25 tok/s tg on a 780M + dual-channel DDR5 -
      # FASTER than cpu-moe. pass+promote -> the agent may flip the 26B to -ngl 99;
      # fail -> the cpu-moe floor stands.
      $moeSpec26 = $TIER_SPEC | Where-Object { $_.id -eq 'gemma4-26b-a4b' } | Select-Object -First 1
      $moeInstalled26 = Test-Path (Join-Path $modelDir $moeSpec26.file)
      if ($proj -and $proj.moe_26b -eq 'gpu') {
        $cn.moe_full_offload.detail = 'profile already projects full-offload (moe_26b=gpu) - nothing to trial'; LogWarn "moe_full_offload SKIPPED: $($cn.moe_full_offload.detail)"
      } elseif ($proj -and -not $proj.include_26b) {
        $cn.moe_full_offload.detail = 'profile excludes the 26B'; LogWarn "moe_full_offload SKIPPED: $($cn.moe_full_offload.detail)"
      } elseif (-not $moeInstalled26) {
        $cn.moe_full_offload.detail = '26B gguf not installed in this stack'; LogWarn "moe_full_offload SKIPPED: $($cn.moe_full_offload.detail)"
      } elseif ($backend -eq 'cpu') {
        $cn.moe_full_offload.detail = 'cpu backend'; LogWarn "moe_full_offload SKIPPED: $($cn.moe_full_offload.detail)"
      } else {
        LogStep "  moe_full_offload: 26B -ngl 99 trial (weights via GTT on UMA; may take minutes)"
        $rmf = Invoke-ProbeLoad -ModelFile $moeSpec26.file -Ctx $cnCtx -KvType $cnKv -FlashAttn $cnFa -WarmDecode -TimeoutSec 600
        $cn.moe_full_offload.cpu_moe_tps = $pm.moe26b.moe26b_tps
        if ($rmf.ok -and $null -ne $rmf.tps) {
          $cn.moe_full_offload.full_tps = $rmf.tps
          $promote = ($null -eq $pm.moe26b.moe26b_tps) -or ($rmf.tps -gt $pm.moe26b.moe26b_tps)
          $cn.moe_full_offload.promote = [bool]$promote
          $cn.moe_full_offload.status = 'pass'
          $cmLabel = if ($null -ne $pm.moe26b.moe26b_tps) { "$($pm.moe26b.moe26b_tps)" } else { 'unmeasured' }
          $cn.moe_full_offload.detail = "26B full-offload measured $($rmf.tps) tok/s decode vs cpu-moe $cmLabel - $(if ($promote) { 'PROMOTE to -ngl 99' } else { 'keep cpu-moe (full-offload is not faster here)' })"
          LogOk "moe_full_offload: $($cn.moe_full_offload.detail)"
        } elseif ($rmf.ok) {
          $cn.moe_full_offload.status = 'warn'
          $cn.moe_full_offload.promote = $false
          $cn.moe_full_offload.detail = "26B full-offload LOADED but decode tok/s was unmeasured ($($rmf.detail)) - no promotion without a number"
          LogWarn "moe_full_offload: $($cn.moe_full_offload.detail)"
        } else {
          $cn.moe_full_offload.status = 'fail'
          $cn.moe_full_offload.promote = $false
          $cn.moe_full_offload.detail = "26B full-offload did not load ($($rmf.detail)) - the cpu-moe floor stands (a partial --n-cpu-moe N split is the manual middle path)"
          LogWarn "moe_full_offload: $($cn.moe_full_offload.detail)"
        }
      }

      # (3) ctx_sweep - long-context SWA sweep 8K/16K/32K on the resident tier. KV type:
      # q8_0 if fa_q8kv PASSED this run, else the profile's projected KV. Stops at the
      # first failing size (larger would fail too). max_ok_ctx >= 32768 authorizes the
      # ctx 16K->32K promotion.
      if (-not $cnResidentOk) {
        $cn.ctx_sweep.detail = 'resident-tier gguf absent'; LogWarn "ctx_sweep SKIPPED: $($cn.ctx_sweep.detail)"
      } elseif ($backend -eq 'cpu') {
        $cn.ctx_sweep.detail = 'cpu backend'; LogWarn "ctx_sweep SKIPPED: $($cn.ctx_sweep.detail)"
      } else {
        $sweepKv = if ($cn.fa_q8kv.status -eq 'pass') { 'q8_0' } else { $cnKv }
        LogStep "  ctx_sweep: 8K/16K/32K loads+gen on $($cnResidentSpec.id) (kv=$sweepKv)"
        $sweepRes = @()
        $maxOk = $null
        foreach ($sc in @(8192, 16384, 32768)) {
          $rs = Invoke-ProbeLoad -ModelFile $cnResidentSpec.file -Ctx $sc -KvType $sweepKv -FlashAttn $cnFa -WarmDecode -TimeoutSec 240
          $sweepRes += ,([ordered]@{ ctx = $sc; ok = [bool]$rs.ok; tps = $rs.tps })
          if ($rs.ok) { $maxOk = $sc; LogOk "ctx_sweep: ctx=$sc ok ($(if ($null -ne $rs.tps) { "$($rs.tps) tok/s" } else { 'tps n/a' }))" }
          else { LogWarn "ctx_sweep: ctx=$sc FAILED ($($rs.detail)) - stopping the sweep"; break }
        }
        $cn.ctx_sweep.results = [object[]]@($sweepRes)
        $cn.ctx_sweep.max_ok_ctx = $maxOk
        if ($null -ne $maxOk -and $maxOk -ge $cnCtx) {
          $cn.ctx_sweep.status = 'pass'
          $cn.ctx_sweep.detail = "max loading+generating ctx=$maxOk (kv=$sweepKv)$(if ($maxOk -ge 32768) { ' - 32K promotion authorized' })"
        } elseif ($null -ne $maxOk) {
          $cn.ctx_sweep.status = 'warn'
          $cn.ctx_sweep.detail = "max ok ctx=$maxOk is BELOW the profile's projected $cnCtx - the ctx probe/downshift result governs"
        } else {
          $cn.ctx_sweep.status = 'fail'
          $cn.ctx_sweep.detail = "no sweep size loaded (kv=$sweepKv)"
        }
        Log "  ctx_sweep: $($cn.ctx_sweep.detail)"
      }

      # (4) bench - llama-bench pp512/tg128 on the resident tier: the regression-gate
      # numbers (recorded in the receipt so a driver/build change has a before/after).
      $benchExe = Join-Path $llamaDir 'llama-bench.exe'
      if (-not (Test-Path $benchExe)) {
        $cn.bench.detail = 'llama-bench.exe absent from the llama.cpp install'; LogWarn "bench SKIPPED: $($cn.bench.detail)"
      } elseif (-not $cnResidentOk) {
        $cn.bench.detail = 'resident-tier gguf absent'; LogWarn "bench SKIPPED: $($cn.bench.detail)"
      } else {
        LogStep "  bench: llama-bench -p 512 -n 128 on $($cnResidentSpec.id)"
        $benchRaw = Invoke-WithEapContinue { & $benchExe -m (Join-Path $modelDir $cnResidentSpec.file) -p 512 -n 128 -o json 2>$null | Out-String }
        $benchDoc = $null
        try { $benchDoc = $benchRaw | ConvertFrom-Json } catch { $benchDoc = $null }
        if ($benchDoc) {
          foreach ($row in @($benchDoc)) {
            if ($row.n_prompt -gt 0 -and $row.n_gen -eq 0 -and $row.avg_ts) { $cn.bench.pp512_tps = [math]::Round([double]$row.avg_ts, 1) }
            if ($row.n_gen -gt 0 -and $row.avg_ts) { $cn.bench.tg128_tps = [math]::Round([double]$row.avg_ts, 1) }
          }
        }
        if ($null -ne $cn.bench.tg128_tps -or $null -ne $cn.bench.pp512_tps) {
          $cn.bench.status = if ($null -ne $cn.bench.tg128_tps -and $cn.bench.tg128_tps -lt 8) { 'warn' } else { 'pass' }
          $cn.bench.detail = "pp512=$($cn.bench.pp512_tps) tok/s, tg128=$($cn.bench.tg128_tps) tok/s$(if ($cn.bench.status -eq 'warn') { ' (tg is CPU-class - check offload)' })"
          if ($cn.bench.status -eq 'pass') { LogOk "bench: $($cn.bench.detail)" } else { LogWarn "bench: $($cn.bench.detail)" }
        } else {
          $cn.bench.status = 'fail'
          $cn.bench.detail = 'llama-bench produced no parseable pp/tg numbers'
          LogWarn "bench: $($cn.bench.detail)"
        }
      }

      # (5) swap_leak - cycle tiers through the transient swap, then verify llama-swap did
      # not leak llama-server processes (>=2 tiers exercises a real eviction cycle).
      try {
        if ($installedTiers.Count -ge 2) {
          LogStep "  swap_leak: eviction cycle $($installedTiers[0].id) -> $($installedTiers[1].id) -> $($installedTiers[0].id)"
          foreach ($t in @($installedTiers[0], $installedTiers[1], $installedTiers[0])) {
            [void](Invoke-Chat -Model $t.id -UserContent 'Reply with exactly the single word: ok' -MaxTokens 8 -TimeoutSec 300)
          }
        } else {
          LogStep "  swap_leak: single tier installed - process-count check only (no eviction cycle)"
        }
        Start-Sleep -Seconds 5
        $mineProcs = @()
        try {
          $mineProcs = @(Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" -ErrorAction SilentlyContinue |
            Where-Object { $_.CommandLine -and ($_.CommandLine -match [regex]::Escape($HOME_DIR.Replace('\','/')) -or $_.CommandLine -match [regex]::Escape($HOME_DIR)) })
        } catch { $mineProcs = @() }
        $cn.swap_leak.servers_after = $mineProcs.Count
        $stillUp = [bool](Invoke-JsonGet "$swapBase/v1/models" 5)
        $cycleLabel = if ($installedTiers.Count -ge 2) { 'after the eviction cycle' } else { '(single tier - process-count check only, no eviction cycle ran)' }
        if ($mineProcs.Count -le 1 -and $stillUp) {
          $cn.swap_leak.status = 'pass'
          $cn.swap_leak.detail = "$($mineProcs.Count) llama-server process(es) $cycleLabel; swap proxy still healthy"
          LogOk "swap_leak: $($cn.swap_leak.detail)"
        } else {
          $cn.swap_leak.status = 'fail'
          $cn.swap_leak.detail = "$($mineProcs.Count) llama-server processes $cycleLabel (expected <=1); swap healthy=$stillUp"
          LogWarn "swap_leak: $($cn.swap_leak.detail)"
        }
      } catch {
        $cn.swap_leak.status = 'warn'
        $cn.swap_leak.detail = "swap_leak check threw: $($_.Exception.Message)"
        LogWarn $cn.swap_leak.detail
      }

      # (6) embedder - cosine sanity through the transient swap: two related texts must
      # score closer than an unrelated pair. (Reranker: not installed by this stack -
      # the receipt's preset note stands.)
      $embedFile = 'embeddinggemma-300m-Q8_0.gguf'
      if (-not (Test-Path (Join-Path $modelDir $embedFile))) {
        $cn.embedder.detail = 'embeddinggemma gguf absent'; LogWarn "embedder SKIPPED: $($cn.embedder.detail)"
      } else {
        LogStep "  embedder: cosine sanity (related pair vs unrelated pair)"
        try {
          $eBody = @{ model = 'embeddinggemma'; input = @(
            'The cat sat quietly on the warm mat.',
            'A kitten rested calmly on the soft rug.',
            'Quarterly GPU shipment revenue grew nine percent year over year.') } | ConvertTo-Json -Depth 4 -Compress
          $er = Invoke-RestMethod -Uri "$swapBase/v1/embeddings" -Method Post -ContentType 'application/json' -Body $eBody -TimeoutSec 120
          $vecs = @($er.data | Sort-Object index | ForEach-Object { ,([double[]]$_.embedding) })
          if ($vecs.Count -eq 3) {
            $cn.embedder.cos_related   = Get-CosineSim -A $vecs[0] -B $vecs[1]
            $cn.embedder.cos_unrelated = Get-CosineSim -A $vecs[0] -B $vecs[2]
            if ($null -ne $cn.embedder.cos_related -and $null -ne $cn.embedder.cos_unrelated -and
                ($cn.embedder.cos_related -gt ($cn.embedder.cos_unrelated + 0.05))) {
              $cn.embedder.status = 'pass'
              $cn.embedder.detail = "related=$($cn.embedder.cos_related) > unrelated=$($cn.embedder.cos_unrelated) (+0.05 margin)"
              LogOk "embedder: $($cn.embedder.detail)"
            } else {
              $cn.embedder.status = 'fail'
              $cn.embedder.detail = "cosine ordering wrong or unmeasurable: related=$($cn.embedder.cos_related) unrelated=$($cn.embedder.cos_unrelated)"
              LogWarn "embedder: $($cn.embedder.detail)"
            }
          } else {
            $cn.embedder.status = 'fail'
            $cn.embedder.detail = "embeddings endpoint returned $($vecs.Count) vectors (expected 3)"
            LogWarn "embedder: $($cn.embedder.detail)"
          }
        } catch {
          $cn.embedder.status = 'fail'
          $cn.embedder.detail = "embeddings call failed: $($_.Exception.Message)"
          LogWarn "embedder: $($cn.embedder.detail)"
        }
      }

      # (7) whisper - no seat installed by this stack; the receipt's preset detail carries
      # the version FLOOR (whisper.cpp >= 1.8.3 on AMD iGPUs) for when one is bound.
      LogWarn "whisper: $($cn.whisper.detail)"
    }
  } catch {
    $cn.gate = "canary section threw: $($_.Exception.Message)"
    LogFail "canary section threw: $($_.Exception.Message) - continuing to verdict"
  }

  # --- J2: media selftest leg - sd.cpp reference render + gpu-vae promotion trial ------------
  # Independent section (own try/catch; never changes the verdict rule). Gate: the sd.cpp
  # media tier is installed (install.ps1 Step 5b lays down sdcpp/sd-cli.exe + the roster).
  # The bindings come from the active profile's config_seed (token-expanded) so model
  # names live in ONE place; a non-seeded box (e.g. forced OFFLOAD_WITH_MEDIA=1 on
  # NVIDIA) falls back to the installer's fixed artifact names.
  LogStep "J2 media: sd.cpp reference render + gpu-vae trial"
  $md = $receipt.media
  try {
    $mseed = $null
    if ($env:OFFLOAD_SELFTEST_MEDIA -ne '0') {
      $mseed = Get-MediaSeed -ProfileId $active.profile -ProfilesJsonPath $profilesJsonPath -OffloadHome $HOME_DIR
      if ($mseed -and $mseed['imagegen_engine'] -ne 'sdcpp') { $mseed = $null }
    }
    $homeFwd  = $HOME_DIR.Replace('\', '/')
    $sdBin    = if ($mseed -and $mseed['sdcpp_bin'])   { $mseed['sdcpp_bin'] }   else { "$homeFwd/sdcpp/sd-cli.exe" }
    $sdModel  = if ($mseed -and $mseed['sdcpp_model']) { $mseed['sdcpp_model'] } else { "$homeFwd/models/z_image_turbo-Q8_0.gguf" }
    $sdVae    = if ($mseed) { $mseed['sdcpp_vae'] }  else { "$homeFwd/models/zimage_ae.safetensors" }
    $sdLlm    = if ($mseed) { $mseed['sdcpp_llm'] }  else { "$homeFwd/models/Qwen3-4B-Instruct-2507-Q4_K_M.gguf" }
    $sdKind   = if ($mseed -and $mseed['sdcpp_model_kind']) { $mseed['sdcpp_model_kind'] } else { 'diffusion' }
    $sdSteps  = if ($mseed -and $mseed['imagegen_steps']) { [int]$mseed['imagegen_steps'] } else { 8 }
    $sdCfg    = if ($mseed -and $mseed['imagegen_cfg'])   { $mseed['imagegen_cfg'] } else { 1 }
    $sdExtra  = @()
    if ($mseed -and $mseed['sdcpp_extra_args']) { $sdExtra = @($mseed['sdcpp_extra_args']) }
    $sdTimeout = if ($mseed -and $mseed['imagegen_timeout_sec']) { [int]$mseed['imagegen_timeout_sec'] } else { 900 }

    if ($env:OFFLOAD_SELFTEST_MEDIA -eq '0') {
      $md.gate = 'skipped: OFFLOAD_SELFTEST_MEDIA=0'
      LogWarn "media leg $($md.gate)"
    } elseif (-not (Test-Path $sdBin)) {
      $md.gate = "skipped: sd-cli.exe not installed ($sdBin) - the media tier is not on this box (install with OFFLOAD_WITH_MEDIA=1)"
      LogWarn "media leg $($md.gate)"
    } elseif (-not (Test-Path $sdModel)) {
      $md.gate = "skipped: image model not installed ($sdModel)"
      LogWarn "media leg $($md.gate)"
    } else {
      $md.ran = $true
      $md.gate = "sd.cpp tier present (bin=$sdBin, model=$([System.IO.Path]::GetFileName($sdModel)))"
      Log "  gate: $($md.gate)"
      $modelFlag = if ($sdKind -eq 'diffusion') { '--diffusion-model' } else { '-m' }
      # One sd-cli render on a spare output; fixed prompt/seed so receipts compare
      # across boxes and driver updates. 512x512 keeps the reference fast (the ZLUDA
      # anchors for scale: SD1.5 512^2 ~12.7s, SDXL-class ~117s on a 780M).
      function Invoke-SdcppRender {
        param([string]$OutPng, [string[]]$Extras, [int]$TimeoutSec)
        $rArgs = New-Object System.Collections.Generic.List[string]
        $rArgs.AddRange([string[]]@($modelFlag, $sdModel))
        if ($sdVae -and (Test-Path $sdVae)) { $rArgs.AddRange([string[]]@('--vae', $sdVae)) }
        if ($sdLlm -and (Test-Path $sdLlm)) { $rArgs.AddRange([string[]]@('--llm', $sdLlm)) }
        $rArgs.AddRange([string[]]@('-p', 'a lighthouse on a rocky coast at golden hour, photorealistic',
                                    '--cfg-scale', "$sdCfg", '--steps', "$sdSteps",
                                    '-W', '512', '-H', '512', '-s', '42', '-o', $OutPng))
        foreach ($e in $Extras) { if ($e) { $rArgs.Add($e) } }
        $logF = Join-Path $env:TEMP ("offload-selftest-sdcpp-{0}.log" -f $PID)
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $proc = $null
        try {
          $proc = Start-Process -FilePath $sdBin -ArgumentList ([string[]]$rArgs) -PassThru -NoNewWindow `
                    -RedirectStandardError $logF -RedirectStandardOutput "$logF.out"
          $deadline = (Get-Date).AddSeconds($TimeoutSec)
          while ((Get-Date) -lt $deadline -and -not $proc.HasExited) { Start-Sleep -Milliseconds 500 }
          $sw.Stop()
          if (-not $proc.HasExited) {
            try { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue } catch { }
            return @{ ok = $false; seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1); detail = "render exceeded ${TimeoutSec}s - killed" }
          }
          $tail = ''
          try { $tail = (Get-Content -Path $logF -Tail 3 -ErrorAction SilentlyContinue) -join ' | ' } catch { }
          if ($proc.ExitCode -ne 0) { return @{ ok = $false; seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1); detail = "sd-cli exited $($proc.ExitCode): $tail" } }
          return @{ ok = $true; seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1); detail = 'ok' }
        } catch {
          $sw.Stop()
          return @{ ok = $false; seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1); detail = "render threw: $($_.Exception.Message)" }
        } finally {
          Get-ChildItem $env:TEMP -Filter ("offload-selftest-sdcpp-{0}*" -f $PID) -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        }
      }

      # (1) Reference render with the SEEDED (safe) config - the install-integrity gate.
      $refPng = Join-Path $env:TEMP ("offload-selftest-media-ref-{0}.png" -f $PID)
      Remove-Item $refPng -Force -ErrorAction SilentlyContinue
      LogStep "  media render: reference prompt, seeded config (steps=$sdSteps cfg=$sdCfg extras: $($sdExtra -join ' '))"
      $r1 = Invoke-SdcppRender -OutPng $refPng -Extras $sdExtra -TimeoutSec $sdTimeout
      $md.render.seconds = $r1.seconds
      if ($r1.ok -and (Test-Path $refPng)) {
        $md.render.bytes = (Get-Item $refPng).Length
        $md.render.distinct_colors = Get-PngDistinctColors -Path $refPng
        if ($md.render.bytes -gt 20000 -and $null -ne $md.render.distinct_colors -and $md.render.distinct_colors -ge 8) {
          $md.render.status = 'pass'
          $md.render.detail = "reference render OK in $($r1.seconds)s ($($md.render.bytes) bytes, $($md.render.distinct_colors) sampled colors) - non-blank"
          LogOk "media render: $($md.render.detail)"
        } else {
          $md.render.status = 'fail'
          $md.render.detail = "render produced a suspect image ($($md.render.bytes) bytes, distinct_colors=$($md.render.distinct_colors)) - the blank/solid-output class (wrong quant build or VAE fault); see the runbook's media canary table"
          LogFail "media render: $($md.render.detail)"
        }
      } else {
        $md.render.status = 'fail'
        $md.render.detail = "reference render FAILED: $($r1.detail)"
        LogFail "media render: $($md.render.detail)"
      }

      # (2) gpu_vae trial - the promotion mirror: re-render WITHOUT the CPU-VAE
      # workaround; promote only on a clean, non-blank, faster run.
      $vaeWorkarounds = @($sdExtra | Where-Object { $_ -in @('--vae-on-cpu', '--vae-tiling') })
      if ($md.render.status -ne 'pass') {
        $md.gpu_vae.detail = 'skipped: reference render did not pass'
        LogWarn "gpu_vae trial $($md.gpu_vae.detail)"
      } elseif ($vaeWorkarounds.Count -eq 0) {
        $md.gpu_vae.detail = 'skipped: no CPU-VAE workaround in the seeded extras - nothing to trial'
        LogWarn "gpu_vae trial $($md.gpu_vae.detail)"
      } else {
        $trialExtras = @($sdExtra | Where-Object { $_ -notin @('--vae-on-cpu', '--vae-tiling') })
        $trialPng = Join-Path $env:TEMP ("offload-selftest-media-trial-{0}.png" -f $PID)
        Remove-Item $trialPng -Force -ErrorAction SilentlyContinue
        LogStep "  gpu_vae trial: same render without $($vaeWorkarounds -join '/')"
        $r2 = Invoke-SdcppRender -OutPng $trialPng -Extras $trialExtras -TimeoutSec $sdTimeout
        $md.gpu_vae.seconds = $r2.seconds
        $trialColors = if ($r2.ok -and (Test-Path $trialPng)) { Get-PngDistinctColors -Path $trialPng } else { $null }
        if ($r2.ok -and $null -ne $trialColors -and $trialColors -ge 8 -and $r2.seconds -lt $r1.seconds) {
          $md.gpu_vae.status = 'pass'
          $md.gpu_vae.promote = $true
          $md.gpu_vae.detail = "GPU VAE decode clean + faster ($($r2.seconds)s vs $($r1.seconds)s) - dropping the CPU-VAE workaround is authorized"
          LogOk "gpu_vae: $($md.gpu_vae.detail)"
        } elseif ($r2.ok -and $null -ne $trialColors -and $trialColors -ge 8) {
          $md.gpu_vae.status = 'pass'
          $md.gpu_vae.promote = $false
          $md.gpu_vae.detail = "GPU VAE decode clean but not faster ($($r2.seconds)s vs $($r1.seconds)s) - keep the seeded workaround"
          LogOk "gpu_vae: $($md.gpu_vae.detail)"
        } else {
          $md.gpu_vae.status = 'fail'
          $md.gpu_vae.promote = $false
          $md.gpu_vae.detail = "GPU VAE decode failed or blank ($($r2.detail); distinct_colors=$trialColors) - the CPU-VAE workaround STAYS (sd.cpp #563/#1621 class)"
          LogWarn "gpu_vae: $($md.gpu_vae.detail)"
        }
        Remove-Item $trialPng -Force -ErrorAction SilentlyContinue
      }
      Remove-Item $refPng -Force -ErrorAction SilentlyContinue
    }
  } catch {
    $md.gate = "media section threw: $($_.Exception.Message)"
    LogFail "media section threw: $($_.Exception.Message) - continuing to verdict"
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
# Honesty: if the agentic micro-bench did not actually run, the receipt must NOT imply agent-tier
# throughput/well-formedness was proven. Append a does_not_prove line (deduped) saying so.
if ($receipt.agent.skipped -eq $true) {
  $dnp = "agent-driving tier ($($receipt.agent.tier)) throughput/well-formedness: micro-bench skipped ($($receipt.agent.notes))"
  if (-not ($receipt.does_not_prove -contains $dnp)) { $receipt.does_not_prove += $dnp }
}
# Same discipline as tiers/remediations: if the larger-ctx headroom was computed-only (not loaded),
# say the receipt does not prove a larger window actually loads on this GPU.
if ($receipt.agent.larger_ctx_src -ne 'measured') {
  $dnp2 = "a larger context window ($($receipt.agent.larger_ctx)) actually loads on this GPU: recorded as computed-only ($($receipt.agent.larger_ctx_src))"
  if (-not ($receipt.does_not_prove -contains $dnp2)) { $receipt.does_not_prove += $dnp2 }
}
# H3 honesty: append a does_not_prove line for every profile_measure item NOT measured on THIS box.
# Anything projected/skipped/measure-on-target must never read as measured.
$pmf = $receipt.profile_measure
if (-not $pmf.profile) {
  $d = 'active hardware profile: none resolved (installed.json pre-H2 / no detect JSON / no OFFLOAD_PROFILE) - profile_measure values are unmeasured'
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
}
if ($pmf.ctx.src -ne 'measured') {
  $d = "the profile's projected ctx ($($pmf.ctx.projected_ctx)) actually loads on this box: $($pmf.ctx.src) ($($pmf.ctx.detail))"
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
}
if ($pmf.moe26b.src -ne 'measured') {
  $d = "26B --cpu-moe decode tok/s on this box: skipped ($($pmf.moe26b.detail))"
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
}
# dual-GPU two-model residency + Optane always need hardware this single-GPU laptop lacks.
if ($pmf.dual_gpu.status -ne 'measured') {
  $d = "dual-GPU two-model residency (configs #3/#4): $($pmf.dual_gpu.status) - $($pmf.dual_gpu.detail)"
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
}
if ($pmf.optane.status -ne 'measured') {
  $d = "Optane cold-load/mmap latency (config #4): $($pmf.optane.status) - $($pmf.optane.detail)"
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
}
# J1 honesty: when the H3 promotion canaries did not run, no promotion (ctx/KV/26B
# placement) is authorized - the floor stands and the receipt must say why.
if (-not $receipt.canaries.ran) {
  $d = "the H3 promotion canaries (fa_q8kv / moe_full_offload / ctx_sweep / bench / swap_leak / embedder): not run this pass ($($receipt.canaries.gate)) - no config promotion is authorized"
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
} else {
  # Individually skipped canaries prove nothing either - each lands its own line.
  foreach ($cnName in @('fa_q8kv','moe_full_offload','ctx_sweep','bench','swap_leak','embedder')) {
    $cnEntry = $receipt.canaries[$cnName]
    if ($cnEntry -and $cnEntry.status -eq 'skipped') {
      $d = "canary ${cnName}: skipped ($($cnEntry.detail)) - its promotion/verification is not authorized this pass"
      if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
    }
  }
}
# J2 honesty: a media tier that was not rendered this pass is unproven.
if (-not $receipt.media.ran) {
  $d = "the sd.cpp media tier (reference render + gpu-vae trial): not exercised this pass ($($receipt.media.gate)) - image generation on this box is unproven"
  if (-not ($receipt.does_not_prove -contains $d)) { $receipt.does_not_prove += $d }
}

Log ""
LogStep "verdict: $($receipt.verdict)"
# Code-enforce the receipt's array-typed fields at serialization time. PS 5.1's ConvertTo-Json
# has known unwrap/null hazards around empty and 1-element collections depending on how the
# value was built; forcing [object[]] here makes "tiers":[{...}] (array-of-one, never a bare
# object) and "remediations":[] (empty array, never null) structural rather than runtime luck.
$receipt.tiers          = [object[]]@($receipt.tiers)
$receipt.remediations   = [object[]]@($receipt.remediations)
$receipt.proves         = [object[]]@($receipt.proves)
$receipt.does_not_prove = [object[]]@($receipt.does_not_prove)
# H3: profile_measure.cold_swap is an array of per-tier records - same [object[]] force so a
# one-tier install serializes "cold_swap":[{...}] (never a bare object) and an empty run "[]".
$receipt.profile_measure.cold_swap = [object[]]@($receipt.profile_measure.cold_swap)
# J1: same force for the ctx_sweep results array.
$receipt.canaries.ctx_sweep.results = [object[]]@($receipt.canaries.ctx_sweep.results)
$json = ([pscustomobject]$receipt) | ConvertTo-Json -Depth 8 -Compress
Write-Output $json
if ($receipt.verdict -eq 'fail') { exit 1 } else { exit 0 }
