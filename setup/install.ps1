# setup/install.ps1 - idempotent cross-vendor installer for the offload-harness stack.
# Populates $OFFLOAD_HOME with llama.cpp, llama-swap, Gemma-4 models, rendered llama-swap
# config, and the built Go harness+agent; installs the harness config to ~/.local-offload.
# Every step is idempotent: satisfied steps print SKIP and do no work. Re-running is safe.
# PowerShell 5.1 compatible (no ternary, no ?? operator). Fails LOUD on any hard error.
#
# Env overrides:  OFFLOAD_HOME (default $HOME\offload-stack) | OFFLOAD_WITH_FAMILY (default 1)
#                 OFFLOAD_BACKEND (override detect.ps1: cuda|vulkan|cpu)
#
# installed.json (version manifest, written at $OFFLOAD_HOME\installed.json): each component's
# SKIP test requires BOTH the artifact to exist on disk AND the manifest to record the pinned
# version that produced it. Bumping a tag/sha in $PINNED therefore forces a re-download/re-extract
# of exactly that component on the next run, even though the old artifact is still sitting there.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is ~10x faster without the progress UI
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot  = Split-Path -Parent $scriptDir

# ---------------------------------------------------------------------------
# PINNED assets - update tags/hashes HERE in one place. Verified live 2026-07-08.
# llama.cpp release b9934 ; llama-swap v236. Model SHA256 = Hugging Face LFS oid
# (fetched from the HF tree API at pin time - no model download needed to pin).
# ---------------------------------------------------------------------------
$LLAMA_TAG = 'b9934'
$SWAP_TAG  = 'v236'
$PINNED = @{
  # llama.cpp backend binaries (SHA256 from the GitHub release API; verified by download).
  'llama-vulkan' = @{
    url  = "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/llama-$LLAMA_TAG-bin-win-vulkan-x64.zip"
    sha  = '20ea5f484c0ae373affd5c5032b718bf3b9e15a31db5c93bfbbb6d9323824a23'
    size = 32895710
    version = $LLAMA_TAG
  }
  'llama-cuda' = @{
    url  = "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/llama-$LLAMA_TAG-bin-win-cuda-12.4-x64.zip"
    sha  = '31086784613cc4b250fa820762c812bb77cff2f98322e5b76ba62488780bd293'
    size = 267009784
    version = $LLAMA_TAG
  }
  'llama-cudart' = @{
    url  = "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/cudart-llama-bin-win-cuda-12.4-x64.zip"
    sha  = '8c79a9b226de4b3cacfd1f83d24f962d0773be79f1e7b75c6af4ded7e32ae1d6'
    size = 391443627
    version = $LLAMA_TAG
  }
  'llama-cpu' = @{
    url  = "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/llama-$LLAMA_TAG-bin-win-cpu-x64.zip"
    sha  = 'dba3a85a954c14ea69f03d0f7c5c805b4b3e5387940e5543dbdaf55a12a4c385'
    size = 18206912
    version = $LLAMA_TAG
  }
  # llama-swap release (windows_amd64 zip).
  'llama-swap' = @{
    url  = "https://github.com/mostlygeek/llama-swap/releases/download/$SWAP_TAG/llama-swap_236_windows_amd64.zip"
    sha  = '276a00765f072c8f5a276861906b2fea4c69b0620d5f3aeb9288206cda4ef421'
    size = 14830229
    version = $SWAP_TAG
  }
  # Models - Hugging Face resolve links; sha = real LFS oid (sha256) fetched from the HF tree
  # API at pin time, e.g. https://huggingface.co/api/models/<repo>/tree/main?recursive=true
  # (no model bytes downloaded to pin). 'name' = local filename (the template contract); for
  # embeddinggemma the source file is capital-M but we save it lowercase-m to match.
  'model-e4b' = @{
    url  = 'https://huggingface.co/unsloth/gemma-4-E4B-it-qat-GGUF/resolve/main/gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf'
    name = 'gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf'
    size = 4215693760
    sha  = 'b3052f962d6449b4eb2075733c068bdec1c51eadb7b237e6c3157bfbb7b1dae0'
    version = 'b3052f96'
  }
  'model-e2b' = @{
    url  = 'https://huggingface.co/unsloth/gemma-4-E2B-it-qat-GGUF/resolve/main/gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf'
    name = 'gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf'
    size = 2620368960
    sha  = 'cd4526493dccbfd6791bee8822e37e30340074d1d4d9aada52ce09afefd6a33a'
    version = 'cd452649'
  }
  'model-26b' = @{
    url  = 'https://huggingface.co/unsloth/gemma-4-26B-A4B-it-qat-GGUF/resolve/main/gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf'
    name = 'gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf'
    size = 14249045120
    sha  = 'dcf179a91153e3a7ece792e48ef872180d9d6ef9b7677f0a0bd3e83cfe624d5e'
    version = 'dcf179a9'
  }
  'model-embed' = @{
    url  = 'https://huggingface.co/unsloth/embeddinggemma-300m-GGUF/resolve/main/embeddinggemma-300M-Q8_0.gguf'
    name = 'embeddinggemma-300m-Q8_0.gguf'
    size = 328577056
    sha  = 'a0f7b4e13c397a6e1b32c2de75b1f65a14c92ec524d5f674d94a4290a1c4969b'
    version = 'a0f7b4e1'
  }
}

# ---------------------------------------------------------------------------
# Step helper - enforces idempotency + logging. $test returns $true when already
# satisfied (=> SKIP). Otherwise $action runs; a throw inside fails the install loud.
# ---------------------------------------------------------------------------
function Step {
  param([string]$Name, [scriptblock]$Test, [scriptblock]$Action)
  if (& $Test) { Write-Host "SKIP  $Name" -ForegroundColor DarkGray; return }
  Write-Host "DO    $Name" -ForegroundColor Cyan
  & $Action
  Write-Host "OK    $Name" -ForegroundColor Green
}

# ---------------------------------------------------------------------------
# R3.1: refresh $env:Path in-process from the Machine+User registry values after a winget
# install. An autonomous agent cannot open a new shell to pick up PATH changes, so we must
# re-read the two registry sources winget itself wrote and splice them into this process.
# ---------------------------------------------------------------------------
function Update-SessionPath {
  $machine = [System.Environment]::GetEnvironmentVariable('Path', 'Machine')
  $user    = [System.Environment]::GetEnvironmentVariable('Path', 'User')
  $combined = (@($machine, $user) | Where-Object { $_ }) -join ';'
  if ($combined) { $env:Path = $combined }
}

# ---------------------------------------------------------------------------
# R3.3: download with retry x3 (each attempt restarts from zero - BITS/IWR do not resume partial
# downloads here), size + SHA256 verification, and periodic progress lines (>=1 line per <=60s)
# so a large pull never reads as a hung process.
# ---------------------------------------------------------------------------
function Get-Verified {
  param([string]$Url, [string]$Dest, [long]$ExpectedSize, [string]$Sha)
  if ((Test-Path $Dest) -and ((Get-Item $Dest).Length -eq $ExpectedSize)) { return }  # already complete
  $tmp = "$Dest.part"
  for ($attempt = 1; $attempt -le 3; $attempt++) {
    try {
      $bits = Get-Command Start-BitsTransfer -ErrorAction SilentlyContinue
      if ($bits) {
        try {
          # BITS runs as an async job; poll it every <=60s and print bytes/percent so the
          # transcript never reads as a hang on a large (multi-GB) file.
          $job = Start-BitsTransfer -Source $Url -Destination $tmp -Asynchronous -DisplayName 'offload-download'
          while ($job.JobState -in @('Connecting','Transferring','Transient Error')) {
            Start-Sleep -Seconds 20
            $job = Get-BitsTransfer -JobId $job.JobId -ErrorAction SilentlyContinue
            if (-not $job) { break }
            if ($job.BytesTotal -gt 0) {
              $pct = [math]::Round(100 * $job.BytesTransferred / $job.BytesTotal, 1)
              Write-Host "      ... $($job.BytesTransferred)/$($job.BytesTotal) bytes ($pct%)" -ForegroundColor DarkGray
            }
          }
          if ($job -and $job.JobState -eq 'Transferred') {
            Complete-BitsTransfer -BitsJob $job
          } elseif ($job -and $job.JobState -eq 'Error') {
            Remove-BitsTransfer -BitsJob $job -ErrorAction SilentlyContinue
            throw "BITS job errored"
          } else {
            throw "BITS job did not complete (state: $(if($job){$job.JobState}else{'unknown'}))"
          }
        } catch {
          Get-BitsTransfer -DisplayName 'offload-download' -ErrorAction SilentlyContinue | Remove-BitsTransfer -ErrorAction SilentlyContinue
          Invoke-WebRequestWithProgress -Url $Url -Dest $tmp
        }
      } else {
        Invoke-WebRequestWithProgress -Url $Url -Dest $tmp
      }
      $got = (Get-Item $tmp).Length
      if ($ExpectedSize -gt 0 -and $got -ne $ExpectedSize) {
        throw "size mismatch: got $got bytes, expected $ExpectedSize"
      }
      if ($Sha) {
        $actual = (Get-FileHash -Path $tmp -Algorithm SHA256).Hash.ToLower()
        if ($actual -ne $Sha.ToLower()) { throw "SHA256 mismatch: got $actual, expected $Sha" }
      }
      Move-Item -Force $tmp $Dest
      return
    } catch {
      Write-Host "      download attempt $attempt/3 failed: $($_.Exception.Message)" -ForegroundColor Yellow
      Remove-Item $tmp -ErrorAction SilentlyContinue
      if ($attempt -eq 3) { throw "download failed after 3 attempts: $Url`n$($_.Exception.Message)" }
      Start-Sleep -Seconds ([math]::Pow(2, $attempt))
    }
  }
}

# R3.3 Invoke-WebRequest fallback path: chunked stream read with a periodic (<=60s) progress
# line, since a plain Invoke-WebRequest -OutFile gives no feedback on a multi-GB file.
function Invoke-WebRequestWithProgress {
  param([string]$Url, [string]$Dest)
  $req = [System.Net.HttpWebRequest]::Create($Url)
  $req.Method = 'GET'
  $resp = $req.GetResponse()
  $total = $resp.ContentLength
  $inStream = $resp.GetResponseStream()
  $outStream = [System.IO.File]::Open($Dest, [System.IO.FileMode]::Create)
  try {
    $buffer = New-Object byte[] 1MB
    $readTotal = 0L
    $lastReport = Get-Date
    while ($true) {
      $read = $inStream.Read($buffer, 0, $buffer.Length)
      if ($read -le 0) { break }
      $outStream.Write($buffer, 0, $read)
      $readTotal += $read
      if (((Get-Date) - $lastReport).TotalSeconds -ge 20) {
        if ($total -gt 0) {
          $pct = [math]::Round(100 * $readTotal / $total, 1)
          Write-Host "      ... $readTotal/$total bytes ($pct%)" -ForegroundColor DarkGray
        } else {
          Write-Host "      ... $readTotal bytes" -ForegroundColor DarkGray
        }
        $lastReport = Get-Date
      }
    }
  } finally {
    $outStream.Close()
    $inStream.Close()
    $resp.Close()
  }
}

# Download a pinned zip (by $PINNED key) into $stage and expand it into $dest.
function Install-Zip {
  param([string]$Key, [string]$Stage, [string]$Dest)
  $p = $PINNED[$Key]
  $zip = Join-Path $Stage (Split-Path -Leaf $p.url)
  Get-Verified -Url $p.url -Dest $zip -ExpectedSize $p.size -Sha $p.sha
  New-Item -ItemType Directory -Force -Path $Dest | Out-Null
  Expand-Archive -Path $zip -DestinationPath $Dest -Force
}

# ---------------------------------------------------------------------------
# R3.4: hash a file once and cache the result beside it as <file>.sha-ok, stamped with the
# sha it matched. A later call with the SAME expected sha reuses the sentinel (fast path);
# a different expected sha (pin bump) forces a real re-hash.
# ---------------------------------------------------------------------------
function Test-CachedSha {
  param([string]$Path, [string]$ExpectedSha)
  if (-not (Test-Path $Path)) { return $false }
  $sentinel = "$Path.sha-ok"
  if (Test-Path $sentinel) {
    $cached = (Get-Content -Raw $sentinel).Trim()
    if ($cached -eq $ExpectedSha.ToLower()) { return $true }
  }
  $actual = (Get-FileHash -Path $Path -Algorithm SHA256).Hash.ToLower()
  if ($actual -ne $ExpectedSha.ToLower()) { return $false }
  Set-Content -Path $sentinel -Value $actual -NoNewline
  return $true
}

# ---------------------------------------------------------------------------
# R3.5: version manifest helpers. $manifestOld = what a PRIOR run recorded (read once, up
# front); $manifestNew = what THIS run will record (built up as components complete, written
# at the very end). A component's Step test checks $manifestOld for a matching version -
# this is what makes a $PINNED tag/sha bump force a real re-do even though old bytes remain.
# ---------------------------------------------------------------------------
function Get-OldVersion {
  param([string]$Component)
  if ($manifestOld -and $manifestOld.components -and $manifestOld.components.PSObject.Properties[$Component]) {
    return $manifestOld.components.$Component
  }
  return $null
}

# ---------------------------------------------------------------------------
# Resolve config
# ---------------------------------------------------------------------------
if ($env:OFFLOAD_HOME) { $HOME_DIR = $env:OFFLOAD_HOME } else { $HOME_DIR = Join-Path $HOME 'offload-stack' }
if ($null -ne $env:OFFLOAD_WITH_FAMILY) { $withFamily = ($env:OFFLOAD_WITH_FAMILY -ne '0') } else { $withFamily = $true }
$llamaDir = Join-Path $HOME_DIR 'llama'
$swapDir  = Join-Path $HOME_DIR 'llama-swap'
$modelDir = Join-Path $HOME_DIR 'models'
$harnessDir = Join-Path $HOME_DIR 'harness'
$stageDir = Join-Path $HOME_DIR '.stage'
foreach ($d in @($HOME_DIR, $llamaDir, $swapDir, $modelDir, $harnessDir, $stageDir)) {
  New-Item -ItemType Directory -Force -Path $d | Out-Null
}

# R3.3: transcript everything to $OFFLOAD_HOME\install.log from here on.
$logPath = Join-Path $HOME_DIR 'install.log'
try { Stop-Transcript -ErrorAction SilentlyContinue | Out-Null } catch {}
Start-Transcript -Path $logPath -Append | Out-Null

# R3.5: load the prior manifest (if any) before doing any work.
$manifestPath = Join-Path $HOME_DIR 'installed.json'
$manifestOld = $null
if (Test-Path $manifestPath) {
  try { $manifestOld = Get-Content -Raw $manifestPath | ConvertFrom-Json } catch { $manifestOld = $null }
}
$manifestComponents = @{}   # populated as each component completes/confirms this run

Write-Host "==> offload-harness installer | home=$HOME_DIR | with_family=$withFamily" -ForegroundColor White
Write-Host "    log: $logPath" -ForegroundColor White

# ---------------------------------------------------------------------------
# Step 1: backend
# ---------------------------------------------------------------------------
if ($env:OFFLOAD_BACKEND) {
  $backend = $env:OFFLOAD_BACKEND.Trim().ToLower()
  Write-Host "DO    backend: OFFLOAD_BACKEND override = $backend" -ForegroundColor Cyan
} else {
  Write-Host "DO    backend: running detect.ps1" -ForegroundColor Cyan
  # Run detect.ps1 in a child process so its real exit code is authoritative (a hard blocker
  # exits 1). Use the same PowerShell host running this script (5.1 powershell.exe or 7 pwsh.exe).
  $psHost = (Get-Process -Id $PID).Path
  $detectPs1 = Join-Path $scriptDir 'detect.ps1'
  $detectOut = & $psHost -NoProfile -File $detectPs1
  if ($LASTEXITCODE -ne 0) { throw "detect.ps1 signalled a hard blocker (exit $LASTEXITCODE) - see its stderr above" }
  $jsonLine = ($detectOut | Where-Object { $_ -match '^\s*\{.*\}\s*$' } | Select-Object -Last 1)
  if (-not $jsonLine) { throw "detect.ps1 produced no JSON verdict line" }
  $backend = ($jsonLine | ConvertFrom-Json).backend
}
if ($backend -notin @('cuda','vulkan','cpu')) { throw "unsupported backend '$backend' (expected cuda|vulkan|cpu)" }
Write-Host "OK    backend = $backend" -ForegroundColor Green

# ---------------------------------------------------------------------------
# Step 2: prerequisites via winget (Git + Go >=1.26). Never install CUDA/ROCm.
# R3.1: refresh $env:Path in-process after any install (no new shell available).
# R3.2: winget installs are silent and pre-accept both agreement prompts.
# ---------------------------------------------------------------------------
function Test-Go126 {
  $g = Get-Command go -ErrorAction SilentlyContinue
  if (-not $g) { return $false }
  $v = (& go version) -replace '.*go(\d+)\.(\d+).*', '$1.$2'
  $parts = $v.Split('.')
  if ($parts.Count -lt 2) { return $false }
  return ([int]$parts[0] -gt 1) -or ([int]$parts[0] -eq 1 -and [int]$parts[1] -ge 26)
}
$wingetFlags = @('--accept-package-agreements', '--accept-source-agreements', '--silent')
Step 'prereq: Git' `
  { [bool](Get-Command git -ErrorAction SilentlyContinue) } `
  {
    if (-not (Get-Command winget -ErrorAction SilentlyContinue)) { throw "git missing and winget unavailable - install Git manually" }
    & winget install --id Git.Git -e --source winget @wingetFlags
    if ($LASTEXITCODE -ne 0) { throw "winget install Git.Git failed (exit $LASTEXITCODE)" }
    Update-SessionPath
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) { throw "git still missing after winget install + PATH refresh" }
  }
Step 'prereq: Go >=1.26' `
  { Test-Go126 } `
  {
    if (-not (Get-Command winget -ErrorAction SilentlyContinue)) { throw "Go >=1.26 missing and winget unavailable - install Go manually" }
    & winget install --id GoLang.Go -e --source winget @wingetFlags
    if ($LASTEXITCODE -ne 0) { throw "winget install GoLang.Go failed (exit $LASTEXITCODE)" }
    Update-SessionPath
    if (-not (Test-Go126)) { throw "Go still <1.26 after winget install + PATH refresh" }
  }

# ---------------------------------------------------------------------------
# Step 3: llama.cpp binaries for the backend (+ cudart for CUDA)
# SKIP requires: artifact present AND manifest records the currently-pinned tag.
# ---------------------------------------------------------------------------
$llamaExe = Join-Path $llamaDir 'llama-server.exe'
$llamaComponentKey = "llama-$backend"
Step "llama.cpp ($backend) -> $llamaDir" `
  { (Test-Path $llamaExe) -and ((Get-OldVersion $llamaComponentKey) -eq $LLAMA_TAG) } `
  {
    if ($backend -eq 'cuda') {
      Install-Zip -Key 'llama-cuda'   -Stage $stageDir -Dest $llamaDir
      Install-Zip -Key 'llama-cudart' -Stage $stageDir -Dest $llamaDir   # CUDA runtime DLLs alongside
    } elseif ($backend -eq 'vulkan') {
      Install-Zip -Key 'llama-vulkan' -Stage $stageDir -Dest $llamaDir
    } else {
      Install-Zip -Key 'llama-cpu'    -Stage $stageDir -Dest $llamaDir
    }
    if (-not (Test-Path $llamaExe)) { throw "llama-server.exe not found in $llamaDir after unzip" }
  }
$manifestComponents[$llamaComponentKey] = $LLAMA_TAG

# ---------------------------------------------------------------------------
# Step 4: llama-swap
# ---------------------------------------------------------------------------
$swapExe = Join-Path $swapDir 'llama-swap.exe'
Step "llama-swap -> $swapDir" `
  { (Test-Path $swapExe) -and ((Get-OldVersion 'llama-swap') -eq $SWAP_TAG) } `
  {
    Install-Zip -Key 'llama-swap' -Stage $stageDir -Dest $swapDir
    if (-not (Test-Path $swapExe)) { throw "llama-swap.exe not found in $swapDir after unzip" }
  }
$manifestComponents['llama-swap'] = $SWAP_TAG

# ---------------------------------------------------------------------------
# Step 5: models - E4B + embed always; E2B + 26B only when withFamily
# R3.4: SKIP test hashes once (cached via <file>.sha-ok) against the pinned sha; a pin bump
# (different sha) invalidates both the sentinel comparison and the manifest version check.
# ---------------------------------------------------------------------------
$modelKeys = @('model-e4b', 'model-embed')
if ($withFamily) { $modelKeys += @('model-e2b', 'model-26b') }
foreach ($key in $modelKeys) {
  $m = $PINNED[$key]
  $dest = Join-Path $modelDir $m.name
  Step "model: $($m.name)" `
    {
      (Test-Path $dest) -and ((Get-Item $dest).Length -eq $m.size) -and
      ((Get-OldVersion $key) -eq $m.version) -and (Test-CachedSha -Path $dest -ExpectedSha $m.sha)
    } `
    {
      Get-Verified -Url $m.url -Dest $dest -ExpectedSize $m.size -Sha $m.sha
      Test-CachedSha -Path $dest -ExpectedSha $m.sha | Out-Null   # seed the sentinel for the next run
    }
  $manifestComponents[$key] = $m.version
}

# ---------------------------------------------------------------------------
# Step 6: render llama-swap.yaml from the backend template (token substitution).
# R3.6: rendered paths use forward slashes - llama-swap on Windows chokes on backslash
# escapes inside its YAML string scalars; Windows APIs accept forward slashes natively.
# ---------------------------------------------------------------------------
$yamlDest = Join-Path $HOME_DIR 'llama-swap.yaml'
Step "render llama-swap.yaml ($backend)" `
  {
    (Test-Path $yamlDest) -and
    -not (Select-String -Path $yamlDest -Pattern '__(LLAMA_BIN|MODELS|NTHREADS)__' -Quiet) -and
    -not (Select-String -Path $yamlDest -SimpleMatch -Pattern $llamaDir -Quiet)   # backslash path = stale pre-R3.6 render
  } `
  {
    $tpl = Join-Path (Join-Path $scriptDir 'templates') "llama-swap.win-$backend.yaml"
    if (-not (Test-Path $tpl)) { throw "template not found: $tpl" }
    $text = Get-Content -Raw $tpl
    $llamaDirFwd = $llamaDir.Replace('\', '/')
    $modelDirFwd = $modelDir.Replace('\', '/')
    # Replace token+backslash together so the template's own literal '\' path separator
    # (baked in right after each token, e.g. "__LLAMA_BIN__\llama-server.exe") also becomes
    # forward-slash - a plain token substitution would leave that literal backslash behind.
    $text = $text.Replace('__LLAMA_BIN__\', "$llamaDirFwd/").Replace('__MODELS__\', "$modelDirFwd/")
    $text = $text.Replace('__LLAMA_BIN__', $llamaDirFwd).Replace('__MODELS__', $modelDirFwd)
    if ($backend -eq 'cpu') {
      $cores = (Get-CimInstance Win32_Processor | Measure-Object -Property NumberOfCores -Sum).Sum
      if (-not $cores -or $cores -lt 1) { $cores = [Environment]::ProcessorCount }
      $text = $text.Replace('__NTHREADS__', "$cores")
    }
    Set-Content -Path $yamlDest -Value $text -Encoding UTF8
  }

# ---------------------------------------------------------------------------
# Step 7: build Go harness + agent into $harnessDir
# ---------------------------------------------------------------------------
$harnessExe = Join-Path $harnessDir 'local-offload.exe'
$agentExe   = Join-Path $harnessDir 'local-agent.exe'
Step 'build local-offload.exe' `
  { Test-Path $harnessExe } `
  {
    if (-not (Test-Go126)) { throw "Go >=1.26 not on PATH - cannot build" }
    Push-Location $repoRoot
    try {
      & go build -o $harnessExe .
      if ($LASTEXITCODE -ne 0) { throw "go build (harness) failed (exit $LASTEXITCODE)" }
    } finally { Pop-Location }
    if (-not (Test-Path $harnessExe)) { throw "local-offload.exe not produced" }
  }
Step 'build local-agent.exe' `
  { Test-Path $agentExe } `
  {
    if (-not (Test-Go126)) { throw "Go >=1.26 not on PATH - cannot build" }
    Push-Location $repoRoot
    try {
      & go build -o $agentExe ./cmd/local-agent
      if ($LASTEXITCODE -ne 0) { throw "go build (agent) failed (exit $LASTEXITCODE)" }
    } finally { Pop-Location }
    if (-not (Test-Path $agentExe)) { throw "local-agent.exe not produced" }
  }

# ---------------------------------------------------------------------------
# Step 8: harness config -> ~/.local-offload/config.json (no overwrite)
# ---------------------------------------------------------------------------
$cfgDir  = Join-Path $HOME '.local-offload'
$cfgDest = Join-Path $cfgDir 'config.json'
Step 'harness config -> ~/.local-offload/config.json' `
  { Test-Path $cfgDest } `
  {
    New-Item -ItemType Directory -Force -Path $cfgDir | Out-Null
    Copy-Item (Join-Path (Join-Path $scriptDir 'templates') 'config.json') $cfgDest
  }

# ---------------------------------------------------------------------------
# R3.5: write the version manifest now that every component version is known.
# ---------------------------------------------------------------------------
$manifest = [ordered]@{
  llama_cpp_tag  = $LLAMA_TAG
  llama_swap_tag = $SWAP_TAG
  backend        = $backend
  install_date   = (Get-Date -Format 'yyyy-MM-ddTHH:mm:ssK')
  components     = $manifestComponents
  models         = @($modelKeys | ForEach-Object { @{ name = $PINNED[$_].name; sha256 = $PINNED[$_].sha } })
}
$manifest | ConvertTo-Json -Depth 6 | Set-Content -Path $manifestPath -Encoding UTF8

# ---------------------------------------------------------------------------
# Step 9: start command + JSON verdict
# ---------------------------------------------------------------------------
Write-Host ""
Write-Host "Start the stack with:" -ForegroundColor White
Write-Host "  & '$swapExe' --config '$yamlDest' --listen 127.0.0.1:11436" -ForegroundColor White
Write-Host ""
$verdict = @{ installed = $true; backend = $backend; home = $HOME_DIR; next = 'run selftest.ps1' } | ConvertTo-Json -Compress
try { Stop-Transcript | Out-Null } catch {}
$verdict
