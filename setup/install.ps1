# setup/install.ps1 - idempotent cross-vendor installer for the offload-harness stack.
# Populates $OFFLOAD_HOME with llama.cpp, llama-swap, Gemma-4 models, rendered llama-swap
# config, and the built Go harness+agent; installs the harness config to ~/.local-offload.
# Every step is idempotent: satisfied steps print SKIP and do no work. Re-running is safe.
# PowerShell 5.1 compatible (no ternary, no ?? operator). Fails LOUD on any hard error.
#
# Env overrides:  OFFLOAD_HOME (default $HOME\offload-stack) | OFFLOAD_WITH_FAMILY (default 1)
#                 OFFLOAD_BACKEND (override detect.ps1: cuda|vulkan|cpu)
#                 OFFLOAD_PROFILE / OFFLOAD_RAM_TIER / OFFLOAD_BIG_RAM (override the
#                 detected serving profile — used by -RenderOnly and by testing a synthetic box)
#                 OFFLOAD_CUDA_DRIVER / OFFLOAD_CUDA_TOOLKIT (H4: override detect's
#                 cuda_driver/cuda_toolkit for the CUDA build selection — synthetic-box testing)
#
# -RenderOnly (H2): resolve the profile + render llama-swap.yaml ONLY (Step 1 + Step 6),
#   then exit. No winget, no downloads, no Go build, no manifest. Requires OFFLOAD_BACKEND
#   + OFFLOAD_PROFILE (and OFFLOAD_RAM_TIER) set so no hardware detection runs. -RenderOut
#   overrides the output path (default $OFFLOAD_HOME\llama-swap.yaml). This is the render
#   self-test / dry-run path — it never touches the network or a real install.
#
# installed.json (version manifest, written at $OFFLOAD_HOME\installed.json): each component's
# SKIP test requires BOTH the artifact to exist on disk AND the manifest to record the pinned
# version that produced it. Bumping a tag/sha in $PINNED therefore forces a re-download/re-extract
# of exactly that component on the next run, even though the old artifact is still sitting there.
[CmdletBinding()]
param([switch]$RenderOnly, [string]$RenderOut)
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
  # FRONTIER FLOOR (J1, 2026-07-24): the vulkan build must be >= Mar-2026 — the AMD
  # scalar-FA Wave32 + graphics-queue tuning (Feb–Mar 2026) is what makes the RDNA3
  # tiers competitive, and FA+q8_0-KV support on Vulkan dates from the same window.
  # The pin below is a SNAPSHOT OF FRONTIER, refreshed on a cadence (bump, re-run the
  # H3 canary suite, promote) — never let it age below the floor. Show-FrontierNote
  # surfaces newer upstream releases at install time so the pin never silently rots.
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
  # H4: CUDA-13.3 build family — the Blackwell (sm_120) SERVE path. SHA256 = the GitHub
  # release API asset digest for tag b9934 (verified 2026-07-15); Get-Verified re-checks
  # size + SHA on every download.
  'llama-cuda13' = @{
    url  = "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/llama-$LLAMA_TAG-bin-win-cuda-13.3-x64.zip"
    sha  = '20e49d5c640037db1e6a1d3ad111030ed9e15c6df4d4438fc9dad622de035793'
    size = 162132387
    version = $LLAMA_TAG
  }
  'llama-cudart13' = @{
    url  = "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/cudart-llama-bin-win-cuda-13.3-x64.zip"
    sha  = '1462a050eb4c684921ba51dcc4cc488a036674c3e73e9945ee705b854808d03e'
    size = 390970417
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
  # --- J2 media tier: stable-diffusion.cpp (Vulkan) + the Apache-2.0 image roster ---
  # sd.cpp uses rolling per-master releases (no semver): the pin is tag+commit, a
  # SNAPSHOT OF FRONTIER (same rule as the llama.cpp pin — refresh via the media
  # canaries, never let it rot). SHA256 = the GitHub release asset digest; binary
  # inside is sd-cli.exe (+ sd-server.exe, the recorded warm-swap upgrade path).
  'sdcpp-vulkan' = @{
    url  = 'https://github.com/leejet/stable-diffusion.cpp/releases/download/master-789-5114672/sd-master-5114672-bin-win-vulkan-x64.zip'
    sha  = 'cb5fb173430147d83fa3439040be1e1d97906c2e8fb3a06cc8afb761ea98ba17'
    size = 37813364
    version = 'master-789-5114672'
  }
  # Lead image model: Z-Image-Turbo (Apache-2.0 end-to-end; 8-step few-step DiT).
  # jayn7's GGUF build, NOT leejet's: leejet's mixed-rule low-bit quants produce
  # blank/solid images on Vulkan+AMD (sd.cpp issue #1031); jayn7's K-quants are the
  # community-confirmed-working set. Q8_0 = quality-first within UMA reality.
  'model-zimage' = @{
    url  = 'https://huggingface.co/jayn7/Z-Image-Turbo-GGUF/resolve/main/z_image_turbo-Q8_0.gguf'
    name = 'z_image_turbo-Q8_0.gguf'
    size = 7224707136
    sha  = 'f163d60b0eb427469510b8226243d196574a18139a2e40c017409cfbda95ecfe'
    version = 'f163d60b'
  }
  # Z-Image companions: the Qwen3-4B LLM text encoder (sd.cpp --llm; the exact repo
  # named in the pinned release's docs/z_image.md) + the Flux AE VAE from the UNGATED
  # Comfy-Org mirror (the BFL original is gated and its LFS sha is API-masked).
  'model-zimage-llm' = @{
    url  = 'https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF/resolve/main/Qwen3-4B-Instruct-2507-Q4_K_M.gguf'
    name = 'Qwen3-4B-Instruct-2507-Q4_K_M.gguf'
    size = 2497281120
    sha  = '3605803b982cb64aead44f6c1b2ae36e3acdb41d8e46c8a94c6533bc4c67e597'
    version = '3605803b'
  }
  'model-zimage-vae' = @{
    url  = 'https://huggingface.co/Comfy-Org/z_image_turbo/resolve/main/split_files/vae/ae.safetensors'
    name = 'zimage_ae.safetensors'
    size = 335304388
    sha  = 'afc8e28272cd15db3919bacdb6918ce9c1ed22e96cb12c4d5ed0fba823529e38'
    version = 'afc8e282'
  }
  # Optional roster extras (OFFLOAD_MEDIA_EXTRAS=1): SD 1.5 speed floor (single-file,
  # creativeml-openrail-m; the repo carries fp32 only, 4.27GB) + SDXL base slow path
  # (openrail++) with the fp16-fix VAE (MIT; REQUIRED on AMD iGPUs - the stock fp16
  # VAE renders all-black there, sd.cpp issue #563). NOT installed by default.
  # Excluded on license/ADR grounds: SDXL-Turbo (sai-nc-community, non-commercial)
  # and the entire FLUX family (ADR 0011 - even Apache schnell; reopening = new ADR).
  'model-sd15' = @{
    url  = 'https://huggingface.co/stable-diffusion-v1-5/stable-diffusion-v1-5/resolve/main/v1-5-pruned-emaonly.safetensors'
    name = 'v1-5-pruned-emaonly.safetensors'
    size = 4265146304
    sha  = '6ce0161689b3853acaa03779ec93eafe75a02f4ced659bee03f50797806fa2fa'
    version = '6ce01616'
  }
  'model-sdxl' = @{
    url  = 'https://huggingface.co/stabilityai/stable-diffusion-xl-base-1.0/resolve/main/sd_xl_base_1.0.safetensors'
    name = 'sd_xl_base_1.0.safetensors'
    size = 6938078334
    sha  = '31e35c80fc4829d14f90153f4c74cd59c90b779f6afe05a74cd6120b893f7e5b'
    version = '31e35c80'
  }
  'model-sdxl-vae' = @{
    url  = 'https://huggingface.co/madebyollin/sdxl-vae-fp16-fix/resolve/main/sdxl_vae.safetensors'
    name = 'sdxl_vae_fp16_fix.safetensors'
    size = 334641162
    sha  = '235745af8d86bf4a4c1b5b4f529868b37019a10f7c0b2e79ad0abca3a22bc6e1'
    version = '235745af'
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
# J1 frontier surfacing: the pins are SNAPSHOTS OF FRONTIER (house rule: recency
# floors, never staleness pins). This best-effort check asks GitHub for the latest
# llama.cpp / llama-swap release tags and prints a NOTE when the pin is behind —
# it NEVER blocks or fails the install (offline boxes just skip it silently), and
# it never substitutes an asset (Hard rule 2: pins only change by a human bumping
# them and re-running the canary suite). OFFLOAD_SKIP_UPDATE_CHECK=1 disables.
# ---------------------------------------------------------------------------
function Show-FrontierNote {
  if ($env:OFFLOAD_SKIP_UPDATE_CHECK -eq '1') { return }
  $checks = @(
    @{ repo = 'ggml-org/llama.cpp';     pinned = $LLAMA_TAG; name = 'llama.cpp' }
    @{ repo = 'mostlygeek/llama-swap';  pinned = $SWAP_TAG;  name = 'llama-swap' }
  )
  foreach ($c in $checks) {
    try {
      $r = Invoke-RestMethod -Uri "https://api.github.com/repos/$($c.repo)/releases/latest" -TimeoutSec 5 `
             -Headers @{ 'User-Agent' = 'offload-harness-installer' }
      if ($r.tag_name -and ($r.tag_name -ne $c.pinned)) {
        Write-Host "NOTE  $($c.name): newer release $($r.tag_name) available (pinned $($c.pinned) = the verified frontier snapshot). Do NOT substitute mid-install; refresh the pin via the canary suite (bump + re-run selftest + promote)." -ForegroundColor Yellow
      }
    } catch { }   # offline / rate-limited / API change: silently skip — never install-fatal
  }
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
# H2: resolve profile-driven serving params. Reads setup/templates/profiles.json,
# looks up $ProfileId, and returns a hashtable of the concrete render values. An
# UNKNOWN (or null) profile returns @{ known=$false } so Step 6 falls back to the
# backend template's current per-backend defaults. Pure function of its args +
# the on-disk profiles.json (no hardware access) — unit-testable.
#
# Returned keys:
#   known       [bool]    profile was found in profiles.json
#   ctx         [string]  --ctx-size value
#   kv_k, kv_v  [string]  --cache-type-k / -v (kept symmetric = kv_type)
#   flash_attn  [string]  on|off
#   agent_ctx   [int]     the agent's -ctx-tokens (matches ctx)
#   backend     [string]  cuda|vulkan|cpu|dual-cuda (which template to render)
#   dual        [bool]    dual-gpu two-resident render
#   include_26b [bool]    keep the 26B model+group-member AFTER the ram gate
#   moe_26b     [string]  the __MOE_26B__ substitution string ('' when dropped)
#   moe_mode    [string]  gpu|cpu_moe|drop (post-gate, for logging)
#   notes       [string]  operator-facing note
# ---------------------------------------------------------------------------
function Resolve-ProfileParams {
  param([string]$ProfileId, [string]$RamTier, [bool]$BigRam, [string]$ProfilesJsonPath, [string]$Backend)
  # Sane fallback: an unknown/absent profile renders the backend's ORIGINAL baked
  # defaults (pre-H2 values), so the config is always valid even off-matrix. The
  # backend templates are now fully tokenized, so the fallback MUST supply values.
  $defaults = @{
    cuda   = @{ ctx = '16384'; kv = 'q8_0'; fa = 'on';  moe = '--cpu-moe -ngl 999'; resident = 'offload-e4b' }
    vulkan = @{ ctx = '8192';  kv = 'f16';  fa = 'on';  moe = '-ngl 999';          resident = 'offload-e4b' }
    cpu    = @{ ctx = '8192';  kv = 'f16';  fa = 'off'; moe = '';                  resident = 'offload-e4b' }
  }
  function New-Fallback {
    param([string]$B)
    $d = $defaults[$B]
    if (-not $d) { $d = $defaults['cuda'] }
    return @{ known = $false; ctx = $d.ctx; kv_k = $d.kv; kv_v = $d.kv; flash_attn = $d.fa;
      agent_ctx = [int]$d.ctx; backend = $B; dual = $false; resident_tier = $d.resident;
      include_26b = $true; moe_26b = $d.moe; moe_mode = 'template'; notes = 'fallback: backend defaults (unknown profile)' }
  }
  if (-not $ProfileId) { return (New-Fallback -B $Backend) }
  if (-not (Test-Path $ProfilesJsonPath)) { throw "profiles.json not found: $ProfilesJsonPath" }
  $doc = Get-Content -Raw $ProfilesJsonPath | ConvertFrom-Json
  if (-not $doc.profiles.PSObject.Properties[$ProfileId]) {
    return (New-Fallback -B $Backend)   # unknown profile -> backend defaults (sane fallback)
  }
  $p = $doc.profiles.$ProfileId

  # --cpu-moe needs a RAM path (>=~48GB). We keep the 26B on cpu_moe ONLY when
  # ram_tier is mid|high; on low|min there is no RAM path so the 26B is dropped.
  # 'gpu' placement never depends on RAM; 'drop' is always dropped.
  $moeMode = $p.moe_26b
  $include = [bool]$p.include_26b
  if ($moeMode -eq 'drop') { $include = $false }
  elseif ($moeMode -eq 'cpu_moe' -and $RamTier -notin @('mid','high')) {
    $include = $false; $moeMode = 'drop'   # cpu_moe requested but no RAM path -> drop
  }

  # __MOE_26B__ substitution string for the 26B llama-server cmd.
  #   gpu     -> experts on GPU, full offload
  #   cpu_moe -> experts in RAM, non-expert layers on GPU
  $moeStr = ''
  if ($include) {
    if ($moeMode -eq 'gpu') { $moeStr = '-ngl 99' }
    elseif ($moeMode -eq 'cpu_moe') { $moeStr = '--cpu-moe -ngl 999' }
  }

  return @{
    known         = $true
    ctx           = "$($p.ctx_size)"
    kv_k          = $p.kv_type
    kv_v          = $p.kv_type
    flash_attn    = $p.flash_attn
    agent_ctx     = [int]$p.ctx_size
    backend       = $p.backend
    dual          = [bool]$p.dual_resident
    resident_tier = $p.resident_tier
    include_26b   = $include
    moe_26b       = $moeStr
    moe_mode      = $moeMode
    notes         = $p.notes
  }
}

# ---------------------------------------------------------------------------
# H2: drop the gemma4-26b-a4b model block from a rendered llama-swap yaml AND
# remove it from every group's members list. Operates on the rendered text so it
# works for all four templates (cuda/vulkan/cpu/dual-cuda). Returns the edited
# text. Pure string transform — unit-testable.
#   * Model block: the top-level "  gemma4-26b-a4b:" key through the next
#     top-level 2-space model key or a 0-indent line (groups:). A leading
#     comment block immediately above it (the CPU template's OPTIONAL note) is
#     also removed so no dangling comment is left behind.
#   * Group members: strip ", gemma4-26b-a4b" / "gemma4-26b-a4b, " / a lone entry
#     from any "members: [ ... ]" inline list.
# ---------------------------------------------------------------------------
function Remove-26bFromYaml {
  param([string]$Text)
  $lines = $Text -split "`r?`n"
  $out = New-Object System.Collections.Generic.List[string]
  $i = 0
  while ($i -lt $lines.Count) {
    $line = $lines[$i]
    if ($line -match '^\s{2}gemma4-26b-a4b:\s*$') {
      # Drop any immediately-preceding comment lines we already emitted (the
      # 2-space "# ..." note that introduces this model).
      while ($out.Count -gt 0 -and $out[$out.Count - 1] -match '^\s{2}#') {
        $out.RemoveAt($out.Count - 1)
      }
      # Skip this model block: consume lines until the next 2-space top-level key
      # (another model) or a 0-indent line (e.g. "groups:") or EOF.
      $i++
      while ($i -lt $lines.Count) {
        $l = $lines[$i]
        if ($l -match '^\s{2}\S' -or $l -match '^\S' ) { break }   # next key / dedent
        $i++
      }
      continue
    }
    if ($line -match 'members:\s*\[') {
      # Remove the 26B entry from the inline members list, tidy separators.
      $new = $line
      $new = $new -replace ',\s*gemma4-26b-a4b', ''
      $new = $new -replace 'gemma4-26b-a4b\s*,\s*', ''
      $new = $new -replace 'gemma4-26b-a4b', ''
      $new = $new -replace '\[\s*,', '['
      $new = $new -replace ',\s*\]', ']'
      $out.Add($new); $i++; continue
    }
    $out.Add($line); $i++
  }
  return ($out -join "`n")
}

# ---------------------------------------------------------------------------
# H4: choose the llama.cpp CUDA build from (profile, detected CUDA) — FLEXIBLE,
# never a fixed assumption. Pure function of its args — unit-testable via the
# OFFLOAD_INSTALL_DOT_SOURCE seam (setup/tests/install-cuda-build.test.ps1).
#
# The binding constraint for a PREBUILT is the DRIVER's max CUDA (the zips ship
# their own cudart, but a CUDA-13 runtime still needs a CUDA-13 / R580+ driver);
# the TOOLKIT (nvcc) only matters for the source-build path, so it is reported
# as an opportunity, never required.
#
# Matrix (upstream reality, release b9934 — verified 2026-07-15):
#   * win-cuda-12.4 prebuilt: NO sm_120 -> will not run Blackwell at all.
#   * win-cuda-13.3 prebuilt: SERVES Blackwell (MMQ falls back to cuBLAS,
#     ~5.6x slower prefill on Q4 — functional, not peak). Needs a CUDA-13 driver.
#   * NO win-cuda-12.8 prebuilt exists -> the PEAK path (sm_120 MMQ,
#     -DCMAKE_CUDA_ARCHITECTURES=120) is a documented source-build vs a
#     12.8/12.9 toolkit. See setup/SETUP-AGENT.md, Blackwell note.
#   * dual sm_70+sm_120: no single prebuilt covers both, and the CUDA-13 toolkit
#     cannot compile sm_70 (Volta offline compilation removed in CUDA 13.0) ->
#     source-build vs a 12.8/12.9 toolkit with -DCMAKE_CUDA_ARCHITECTURES="70;120"
#     (driver branch R580+ still DRIVES the V100 — only the toolkit dropped it).
#
# Returns @{ component; keys; tier; refuse; report } where component is the
# manifest/PINNED key of the llama zip ('' when refuse), keys = zips to install,
# tier = standard|serves|refuse, report = operator-facing lines (always >=1).
# ---------------------------------------------------------------------------
function Select-CudaBuild {
  param(
    [string]$ProfileId,     # blackwell-16|blackwell-8|dual-gpu|ampere-*|volta-16|... or $null
    [string]$CudaDriver,    # driver's max CUDA "X.Y" (detect: cuda_driver), or $null
    [string]$CudaToolkit    # installed toolkit "X.Y" (detect: cuda_toolkit), or $null
  )
  $driverVer = $null
  if ($CudaDriver -match '^\d+\.\d+$') { $driverVer = [version]$CudaDriver }
  $toolkitVer = $null
  if ($CudaToolkit -match '^\d+\.\d+$') { $toolkitVer = [version]$CudaToolkit }
  $cudaDesc = "driver CUDA=$(if ($CudaDriver) { $CudaDriver } else { 'undetected' }) toolkit=$(if ($CudaToolkit) { $CudaToolkit } else { 'none' })"

  if ($ProfileId -eq 'dual-gpu') {
    return @{ component = ''; keys = @(); tier = 'refuse'; refuse = $true; report = @(
      "dual-gpu (sm_70 Volta + sm_120 Blackwell): no pinned prebuilt covers BOTH arches ($cudaDesc).",
      "The 12.4 prebuilt has no sm_120; the CUDA-13 toolkit cannot compile sm_70 (Volta offline compilation removed in CUDA 13.0).",
      "Source-build llama.cpp with a CUDA 12.8/12.9 toolkit: -DCMAKE_CUDA_ARCHITECTURES=`"70;120`" (an R580+ driver still drives the V100).",
      "See setup/SETUP-AGENT.md - Blackwell note (multi-arch build)." ) }
  }

  if ($ProfileId -match '^blackwell-') {
    if ($driverVer -and $driverVer -ge [version]'13.0') {
      $report = @(
        "Blackwell (sm_120), $cudaDesc -> CUDA-13.3 prebuilt: SERVES now (MMQ falls back to cuBLAS, ~5.6x slower prefill on Q4). Functional, not peak.",
        "Peak path: source-build vs a CUDA 12.8/12.9 toolkit with -DCMAKE_CUDA_ARCHITECTURES=120 (MMQ). See setup/SETUP-AGENT.md - Blackwell note." )
      if ($toolkitVer -and $toolkitVer -ge [version]'12.8' -and $toolkitVer -lt [version]'13.0') {
        $report += "Toolkit $CudaToolkit is already installed - the peak source-build is available on this box now."
      }
      return @{ component = 'llama-cuda13'; keys = @('llama-cuda13','llama-cudart13'); tier = 'serves'; refuse = $false; report = $report }
    }
    if ($driverVer -and $driverVer -ge [version]'12.8') {
      return @{ component = ''; keys = @(); tier = 'refuse'; refuse = $true; report = @(
        "Blackwell (sm_120), ${cudaDesc}: NO pinned prebuilt runs here - the 12.4 build has no sm_120 and the CUDA-13.3 build needs a CUDA-13 (R580+) driver.",
        "Either upgrade the NVIDIA driver to R580+ and re-run install (the 13.3 prebuilt then serves), or source-build vs a 12.8/12.9 toolkit for peak (-DCMAKE_CUDA_ARCHITECTURES=120).",
        "See setup/SETUP-AGENT.md - Blackwell note." ) }
    }
    return @{ component = ''; keys = @(); tier = 'refuse'; refuse = $true; report = @(
      "Blackwell (sm_120), ${cudaDesc}: the old 12.4 prebuilt has NO sm_120 and will not run this card.",
      "Upgrade the NVIDIA driver to R580+ (CUDA 13.x) and re-run install - detect.ps1 + this selection adapt automatically.",
      "See setup/SETUP-AGENT.md - Blackwell note." ) }
  }

  # Every other CUDA profile (ampere-*/volta-16/other/none): the pinned 12.4 build
  # is the verified status-quo path (sm_70..sm_89 covered; any 12.4+ driver runs it).
  return @{ component = 'llama-cuda'; keys = @('llama-cuda','llama-cudart'); tier = 'standard'; refuse = $false;
    report = @("CUDA build: pinned 12.4 prebuilt (profile=$(if ($ProfileId) { $ProfileId } else { '(none)' }), $cudaDesc).") }
}

# ---------------------------------------------------------------------------
# H4: inject the Blackwell runtime env into a rendered llama-swap yaml. Adds the
# given VAR=VAL entries to every model block under models: — appended into an
# existing "env: [ ... ]" inline list (e.g. the 26B's GGML_CUDA_DISABLE_GRAPHS),
# or inserted as a new "    env: [...]" line right after the model key. Entries
# already present are not duplicated (idempotent). Pure string transform.
# Why: CUDA_VISIBLE_DEVICES must be EXPLICIT (hybrid-graphics boxes can hand the
# runtime -1), and CUDA_MODULE_LOADING=LAZY avoids the eager-load cost on
# Blackwell (llama.cpp #22696).
# ---------------------------------------------------------------------------
function Add-GpuEnvToYaml {
  param([string]$Text, [string[]]$EnvVars)
  $lines = $Text -split "`r?`n"
  $out = New-Object System.Collections.Generic.List[string]
  $inModels = $false
  $i = 0
  while ($i -lt $lines.Count) {
    $line = $lines[$i]
    if ($line -match '^models:\s*$') { $inModels = $true; $out.Add($line); $i++; continue }
    elseif ($line -match '^\S') { $inModels = $false }   # any other 0-indent key ends models:
    if ($inModels -and $line -match '^\s{2}(\S+):\s*$') {
      # Model block: scan it for an existing inline env list.
      $blockEnd = $i + 1
      $envIdx = -1
      while ($blockEnd -lt $lines.Count -and $lines[$blockEnd] -notmatch '^\s{0,2}\S') {
        if ($lines[$blockEnd] -match '^\s{4}env:\s*\[(.*)\]\s*$') { $envIdx = $blockEnd }
        $blockEnd++
      }
      $out.Add($line)   # the model key line
      for ($j = $i + 1; $j -lt $blockEnd; $j++) {
        if ($j -eq $envIdx) {
          $existing = $Matches = $null
          if ($lines[$j] -match '^(\s{4}env:\s*\[)(.*)(\]\s*)$') {
            $existing = $Matches[2].Trim()
            $entries = @()
            if ($existing) { $entries = @($existing -split '\s*,\s*') }
            foreach ($v in $EnvVars) { if ($entries -notcontains $v) { $entries += $v } }
            $out.Add("    env: [" + ($entries -join ', ') + "]")
          } else { $out.Add($lines[$j]) }
        } else { $out.Add($lines[$j]) }
      }
      if ($envIdx -lt 0) { $out.Insert($out.Count - ($blockEnd - $i - 1), "    env: [" + ($EnvVars -join ', ') + "]") }
      $i = $blockEnd
      continue
    }
    $out.Add($line); $i++
  }
  return ($out -join "`n")
}

# ---------------------------------------------------------------------------
# Task 3 (2026-07-16 Blackwell-tier plan): overlay a profile's config_seed onto
# the template config.json text. Pure string->string: only the seed's keys are
# set (existing keys updated, absent keys added); a null/empty seed returns the
# input UNCHANGED (byte-identical - no gratuitous reformat on the common path).
# Step 8 applies this ONLY when creating ~/.local-offload/config.json fresh; an
# existing per-machine config is never touched (it stays the sole authority).
# J2: seed values may carry the __OFFLOAD_HOME__ token - sdcpp bindings are FULL
# paths under the install root, which profiles.json cannot know statically. When
# -OffloadHome is given, the token is substituted (forward slashes) in every
# string value, including strings inside array values. "" = no substitution
# (keeps every pre-J2 caller/test byte-identical).
# ---------------------------------------------------------------------------
function Merge-ConfigSeed {
  param([string]$ConfigText, $Seed, [string]$OffloadHome = '')
  if ($null -eq $Seed) { return $ConfigText }
  $props = @($Seed.PSObject.Properties)
  if ($props.Count -eq 0) { return $ConfigText }
  $homeFwd = ''
  if ($OffloadHome) { $homeFwd = $OffloadHome.Replace('\', '/') }
  # NB: every array return is prefixed with the comma operator - PowerShell's return
  # pipeline UNROLLS a 1-element array to its bare element, which would serialize a
  # 1-flag sdcpp_extra_args as a JSON *string* and make Go's json.Unmarshal reject
  # the whole config (found in adversarial review; both AMD seeds ship exactly one
  # extra arg, so every fresh AMD install would have produced a corrupt config).
  function Expand-SeedValue {
    param($Value, [string]$HomeFwd)
    if ($Value -is [string]) {
      if ($HomeFwd) { return $Value.Replace('__OFFLOAD_HOME__', $HomeFwd) }
      return $Value
    }
    if ($Value -is [System.Array]) {
      $out = @($Value | ForEach-Object { if ($_ -is [string] -and $HomeFwd) { $_.Replace('__OFFLOAD_HOME__', $HomeFwd) } else { $_ } })
      return ,([object[]]$out)
    }
    return $Value
  }
  $cfg = $ConfigText | ConvertFrom-Json
  foreach ($p in $props) {
    $v = Expand-SeedValue -Value $p.Value -HomeFwd $homeFwd
    if ($p.Value -is [System.Array]) { $v = [object[]]@($v) }   # belt: never let an array degrade
    if ($cfg.PSObject.Properties[$p.Name]) { $cfg.($p.Name) = $v }
    else { $cfg | Add-Member -NotePropertyName $p.Name -NotePropertyValue $v }
  }
  return ($cfg | ConvertTo-Json -Depth 8)
}

# ---------------------------------------------------------------------------
# run-graph (Task 12): ensure the manifest-satisfier tooling in the ComfyUI venv.
# offload_run_graph provisions an arbitrary workflow's node manifest by shelling to
# ComfyUI-Manager's cm-cli.py; that Manager clone is the REQUIRED tool and a clone
# failure IS surfaced (throws). comfy-cli is an OPTIONAL convenience — on some boxes
# its wheel deps (pydantic-core) have no prebuilt wheel and no Rust toolchain to build
# from, so its install is BEST-EFFORT: a failure logs WARN and continues, never fails
# the setup step. run-graph does not depend on comfy-cli; a missing satisfier makes
# run-graph DEFER SATISFIER_UNAVAILABLE at call time (a clean defer, never a crash).
# All side effects are injected scriptblocks so the whole step is unit-testable with
# zero network/venv (setup/tests/install-rungraph-deps.test.ps1). Returns a "; "-joined
# log of what it did/skipped.
# ---------------------------------------------------------------------------
function Ensure-RunGraphDeps {
  param(
    [string]$ComfyPy, [string]$ComfyDir,
    [scriptblock]$HasComfyCli = { param($py) & $py -m pip show comfy-cli 2>$null; return ($LASTEXITCODE -eq 0) },
    [scriptblock]$HasManager  = { param($d)  Test-Path (Join-Path $d 'custom_nodes/ComfyUI-Manager') },
    [scriptblock]$Pip         = { param($py) & $py -m pip install comfy-cli 2>&1 | Out-Null; if ($LASTEXITCODE -ne 0) { throw "pip install comfy-cli exit $LASTEXITCODE" } },
    [scriptblock]$Clone       = { param($d)  git clone https://github.com/Comfy-Org/ComfyUI-Manager (Join-Path $d 'custom_nodes/ComfyUI-Manager') 2>&1 | Out-Null; if ($LASTEXITCODE -ne 0) { throw "git clone ComfyUI-Manager exit $LASTEXITCODE" } },
    [scriptblock]$HasGitPython = { param($py) & $py -c 'import git' 2>$null; return ($LASTEXITCODE -eq 0) },
    [scriptblock]$PipGitPython = { param($py) & $py -m pip install GitPython 2>&1 | Out-Null; if ($LASTEXITCODE -ne 0) { throw "pip install GitPython exit $LASTEXITCODE" } },
    [scriptblock]$HasUv = { param($py) Test-Path (Join-Path (Split-Path $py) 'uv.exe') },
    [scriptblock]$PipUv = { param($py) & $py -m pip install uv 2>&1 | Out-Null; if ($LASTEXITCODE -ne 0) { throw "pip install uv exit $LASTEXITCODE" } }
  )
  $log = @()
  # comfy-cli: OPTIONAL. Best-effort install; a failure is a WARN, not a hard error.
  if (& $HasComfyCli $ComfyPy) {
    $log += 'SKIP comfy-cli (present)'
  } else {
    try { & $Pip $ComfyPy; $log += 'DO comfy-cli install' }
    catch { $log += "WARN comfy-cli install failed (optional convenience; run-graph does not need it): $($_.Exception.Message)" }
  }
  # ComfyUI-Manager (cm-cli): REQUIRED for manifest satisfaction. A clone failure throws.
  if (& $HasManager $ComfyDir) {
    $log += 'SKIP ComfyUI-Manager (present)'
  } else {
    & $Clone $ComfyDir
    $log += 'DO clone ComfyUI-Manager'
  }
  # GitPython: REQUIRED by cm-cli.py itself (`import git` — live finding 2026-07-17: a fresh
  # Manager clone fails ModuleNotFoundError without it). Same required class as the clone.
  if (& $HasGitPython $ComfyPy) {
    $log += 'SKIP GitPython (present)'
  } else {
    & $PipGitPython $ComfyPy
    $log += 'DO pip install GitPython'
  }
  # uv: REQUIRED by the run-graph pack satisfier (unified `uv pip compile` across all
  # packs' requirements — the installed cm-cli has no --uv flag, so uv is driven
  # directly; live finding 2026-07-17). Missing uv => run-graph defers SATISFIER_UNAVAILABLE.
  if (& $HasUv $ComfyPy) {
    $log += 'SKIP uv (present)'
  } else {
    & $PipUv $ComfyPy
    $log += 'DO pip install uv'
  }
  return ($log -join '; ')
}

# Test seam: OFFLOAD_INSTALL_DOT_SOURCE=1 -> define the pure helpers above and
# return before ANY main-flow work (no dirs, no transcript, no detection). Used
# by setup/tests/install-cuda-build.test.ps1 + install-config-seed.test.ps1 +
# install-rungraph-deps.test.ps1.
# Same pattern as selftest.ps1's OFFLOAD_SELFTEST_DOT_SOURCE.
if ($env:OFFLOAD_INSTALL_DOT_SOURCE -eq '1') { return }

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
# -RenderOnly is a side-effect-free dry run: it renders ONE yaml and touches no artifact/build/
# manifest paths, so creating the full install tree (llama/llama-swap/models/harness/.stage) would
# just litter $HOME\offload-stack with empty dirs when OFFLOAD_HOME is unset. Under -RenderOnly
# create ONLY the render's output parent; a normal install still builds the whole tree below.
if ($RenderOnly) {
  if ($RenderOut) { $renderOutParent = Split-Path -Parent $RenderOut } else { $renderOutParent = $HOME_DIR }
  if ($renderOutParent) { New-Item -ItemType Directory -Force -Path $renderOutParent | Out-Null }
} else {
  foreach ($d in @($HOME_DIR, $llamaDir, $swapDir, $modelDir, $harnessDir, $stageDir)) {
    New-Item -ItemType Directory -Force -Path $d | Out-Null
  }
}

# R3.3: transcript everything to $OFFLOAD_HOME\install.log from here on.
# -RenderOnly is a side-effect-free dry run — no transcript, no manifest.
$logPath = Join-Path $HOME_DIR 'install.log'
if (-not $RenderOnly) {
  try { Stop-Transcript -ErrorAction SilentlyContinue | Out-Null } catch {}
  Start-Transcript -Path $logPath -Append | Out-Null
}

# R3.5: load the prior manifest (if any) before doing any work.
$manifestPath = Join-Path $HOME_DIR 'installed.json'
$manifestOld = $null
if ((Test-Path $manifestPath) -and (-not $RenderOnly)) {
  try { $manifestOld = Get-Content -Raw $manifestPath | ConvertFrom-Json } catch { $manifestOld = $null }
}
$manifestComponents = @{}   # populated as each component completes/confirms this run

if ($RenderOnly) { Write-Host "==> offload-harness RENDER-ONLY (dry run) | home=$HOME_DIR" -ForegroundColor White }
else {
  Write-Host "==> offload-harness installer | home=$HOME_DIR | with_family=$withFamily" -ForegroundColor White
  Write-Host "    log: $logPath" -ForegroundColor White
}

# ---------------------------------------------------------------------------
# Step 1: backend + hardware profile (H2)
# detect.ps1 emits backend + profile + ram_tier + big_ram. The backend picks the
# llama.cpp binary; the profile picks the serving params (Step 6). Both are
# overridable: OFFLOAD_BACKEND, OFFLOAD_PROFILE, OFFLOAD_RAM_TIER (for testing a
# synthetic box / a render-only dry run without the real hardware).
# ---------------------------------------------------------------------------
$profileId = $null; $ramTier = $null; $bigRam = $false
$cudaDriver = $null; $cudaToolkit = $null   # H4: detect's cuda_driver / cuda_toolkit
if ($RenderOnly -and -not $env:OFFLOAD_BACKEND) {
  throw "-RenderOnly requires OFFLOAD_BACKEND (and OFFLOAD_PROFILE) set so no hardware detection runs"
}
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
  $verdictObj = $jsonLine | ConvertFrom-Json
  $backend   = $verdictObj.backend
  $profileId = $verdictObj.profile
  $ramTier   = $verdictObj.ram_tier
  if ($null -ne $verdictObj.big_ram) { $bigRam = [bool]$verdictObj.big_ram }
  $cudaDriver  = $verdictObj.cuda_driver
  $cudaToolkit = $verdictObj.cuda_toolkit
}
# Overrides win over detect (and cover the OFFLOAD_BACKEND-override path, where
# detect never ran and $profileId/$ramTier are still null).
if ($env:OFFLOAD_PROFILE)  { $profileId = $env:OFFLOAD_PROFILE.Trim() }
if ($env:OFFLOAD_RAM_TIER) { $ramTier   = $env:OFFLOAD_RAM_TIER.Trim().ToLower() }
if ($null -ne $env:OFFLOAD_BIG_RAM) { $bigRam = ($env:OFFLOAD_BIG_RAM -ne '0') }
if ($env:OFFLOAD_CUDA_DRIVER)  { $cudaDriver  = $env:OFFLOAD_CUDA_DRIVER.Trim() }   # H4: synthetic-box testing
if ($env:OFFLOAD_CUDA_TOOLKIT) { $cudaToolkit = $env:OFFLOAD_CUDA_TOOLKIT.Trim() }
if ($backend -notin @('cuda','vulkan','cpu')) { throw "unsupported backend '$backend' (expected cuda|vulkan|cpu)" }
if (-not $ramTier) { $ramTier = 'min' }   # conservative default when unknown (drops the RAM-gated 26B path)
Write-Host "OK    backend = $backend | profile = $(if ($profileId) { $profileId } else { '(none - backend defaults)' }) | ram_tier = $ramTier$(if ($bigRam) { ' | big_ram' } else { '' })" -ForegroundColor Green

# -RenderOnly skips all artifact acquisition (Steps 2-5): it renders the config
# from the templates + profiles.json only. Test-Go126 is defined outside the guard
# so Step 7 (also guarded) can still reference it in a normal run.
function Test-Go126 {
  $g = Get-Command go -ErrorAction SilentlyContinue
  if (-not $g) { return $false }
  $v = (& go version) -replace '.*go(\d+)\.(\d+).*', '$1.$2'
  $parts = $v.Split('.')
  if ($parts.Count -lt 2) { return $false }
  return ([int]$parts[0] -gt 1) -or ([int]$parts[0] -eq 1 -and [int]$parts[1] -ge 26)
}
if (-not $RenderOnly) {
Show-FrontierNote   # J1: surface newer upstream releases (best-effort, never fatal)
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
# SKIP requires: artifact present AND manifest records the currently-pinned tag
# under the SELECTED component key — so a CUDA-build switch (e.g. 12.4 -> 13.3
# after a driver upgrade, or the V100 arriving flipping the profile) forces a
# real re-install on the next run even though old bytes are still on disk.
# H4: for CUDA the build is CHOSEN from (profile, detected CUDA) — flexible,
# never a fixed assumption. A refuse verdict fails LOUD with the guidance.
# ---------------------------------------------------------------------------
$llamaExe = Join-Path $llamaDir 'llama-server.exe'
$cudaBuild = $null
if ($backend -eq 'cuda') {
  $cudaBuild = Select-CudaBuild -ProfileId $profileId -CudaDriver $cudaDriver -CudaToolkit $cudaToolkit
  $cudaBuild.report | ForEach-Object { Write-Host "NOTE  $_" -ForegroundColor Yellow }
  if ($cudaBuild.refuse) { throw "no viable pinned llama.cpp CUDA build for profile '$profileId' - see the NOTE lines above" }
  Write-Host "OK    CUDA build selected: $($cudaBuild.component) (tier=$($cudaBuild.tier))" -ForegroundColor Green
  $llamaComponentKey = $cudaBuild.component
} else {
  $llamaComponentKey = "llama-$backend"
}
Step "llama.cpp ($backend -> $llamaComponentKey) -> $llamaDir" `
  { (Test-Path $llamaExe) -and ((Get-OldVersion $llamaComponentKey) -eq $LLAMA_TAG) } `
  {
    if ($backend -eq 'cuda') {
      foreach ($k in $cudaBuild.keys) { Install-Zip -Key $k -Stage $stageDir -Dest $llamaDir }
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
# Step 5b (J2): media tier - stable-diffusion.cpp (Vulkan) + the image roster.
# Default gate: ON for the AMD/Vulkan profiles (amd-*: the tier this leg was built
# for), OFF elsewhere. OFFLOAD_WITH_MEDIA=1 forces on any box (sd.cpp runs on any
# Vulkan GPU incl. NVIDIA); =0 forces off (metered links - the lead set is ~10GB).
# OFFLOAD_MEDIA_EXTRAS=1 additionally pulls SD1.5 + SDXL base + fp16-fix VAE.
# ---------------------------------------------------------------------------
$withMedia = ($profileId -match '^amd-rdna3')
if ($env:OFFLOAD_WITH_MEDIA -eq '1') { $withMedia = $true }
if ($env:OFFLOAD_WITH_MEDIA -eq '0') { $withMedia = $false }
$sdcppDir = Join-Path $HOME_DIR 'sdcpp'
$sdcppExe = Join-Path $sdcppDir 'sd-cli.exe'
if ($withMedia) {
  Step "sd.cpp (vulkan) -> $sdcppDir" `
    { (Test-Path $sdcppExe) -and ((Get-OldVersion 'sdcpp-vulkan') -eq $PINNED['sdcpp-vulkan'].version) } `
    {
      Install-Zip -Key 'sdcpp-vulkan' -Stage $stageDir -Dest $sdcppDir
      if (-not (Test-Path $sdcppExe)) { throw "sd-cli.exe not found in $sdcppDir after unzip" }
    }
  $manifestComponents['sdcpp-vulkan'] = $PINNED['sdcpp-vulkan'].version
  $mediaKeys = @('model-zimage', 'model-zimage-llm', 'model-zimage-vae')
  if ($env:OFFLOAD_MEDIA_EXTRAS -eq '1') { $mediaKeys += @('model-sd15', 'model-sdxl', 'model-sdxl-vae') }
  foreach ($key in $mediaKeys) {
    $m = $PINNED[$key]
    $dest = Join-Path $modelDir $m.name
    Step "media model: $($m.name)" `
      {
        (Test-Path $dest) -and ((Get-Item $dest).Length -eq $m.size) -and
        ((Get-OldVersion $key) -eq $m.version) -and (Test-CachedSha -Path $dest -ExpectedSha $m.sha)
      } `
      {
        Get-Verified -Url $m.url -Dest $dest -ExpectedSize $m.size -Sha $m.sha
        Test-CachedSha -Path $dest -ExpectedSha $m.sha | Out-Null
      }
    $manifestComponents[$key] = $m.version
  }
} else {
  Write-Host "SKIP  media tier (profile=$(if ($profileId) { $profileId } else { '(none)' }) is not amd-*; set OFFLOAD_WITH_MEDIA=1 to install sd.cpp + the image roster on any Vulkan-capable box)" -ForegroundColor DarkGray
}
}  # end if (-not $RenderOnly) — Steps 2-5 (artifact acquisition)

# ---------------------------------------------------------------------------
# Step 6: render llama-swap.yaml PROFILE-DRIVEN (H2). The template is chosen by
# the profile's backend (cuda|vulkan|cpu|dual-cuda), and the profile's serving
# params (ctx / KV / flash-attn / 26B MoE placement) are token-substituted on top
# of the existing __LLAMA_BIN__/__MODELS__/__NTHREADS__ path substitution. When
# the profile drops the 26B (moe=drop, or cpu_moe with no RAM path), the 26B
# model block AND its swap-group members are removed. Unknown/absent profile ->
# the backend template's own baked defaults (sane fallback).
# R3.6: rendered paths use forward slashes - llama-swap on Windows chokes on backslash
# escapes inside its YAML string scalars; Windows APIs accept forward slashes natively.
# ---------------------------------------------------------------------------
$profilesJson = Join-Path (Join-Path $scriptDir 'templates') 'profiles.json'
$pp = Resolve-ProfileParams -ProfileId $profileId -RamTier $ramTier -BigRam $bigRam -ProfilesJsonPath $profilesJson -Backend $backend
# The template to render: the profile's backend (dual-gpu -> dual-cuda; the
# blackwell-32/48/72 all-resident tiers -> cuda-resident), else the fallback's
# backend (= the detected backend). Both are CUDA-only; guard a stray override.
$tplBackend = $pp.backend
if ($tplBackend -in @('dual-cuda', 'cuda-resident') -and $backend -ne 'cuda') {
  throw "profile '$profileId' wants the $tplBackend template but the resolved backend is '$backend' (need CUDA binaries)"
}
$agentCtxTokens = $pp.agent_ctx   # Deliverable 4 (summary)

if ($RenderOut) { $yamlDest = $RenderOut } else { $yamlDest = Join-Path $HOME_DIR 'llama-swap.yaml' }
# -RenderOnly always renders fresh: drop any stale output so the Step SKIP test can't short-circuit it.
if ($RenderOnly -and (Test-Path $yamlDest)) { Remove-Item $yamlDest -Force }
Step "render llama-swap.yaml (backend=$tplBackend profile=$(if ($profileId) { $profileId } else { '(defaults)' }) ctx=$(if ($pp.known) { $pp.ctx } else { 'template' }) kv=$(if ($pp.known) { $pp.kv_k } else { 'template' }) 26b=$(if ($pp.known) { $pp.moe_mode } else { 'template' }))" `
  {
    (Test-Path $yamlDest) -and
    -not (Select-String -Path $yamlDest -Pattern '__(LLAMA_BIN|MODELS|NTHREADS|CTX|KV_K|KV_V|FLASH_ATTN|MOE_26B)__' -Quiet) -and
    -not (Select-String -Path $yamlDest -SimpleMatch -Pattern $llamaDir -Quiet) -and   # backslash path = stale pre-R3.6 render
    (($tplBackend -ne 'vulkan') -or (Select-String -Path $yamlDest -SimpleMatch -Pattern 'GGML_VK_VISIBLE_DEVICES' -Quiet))   # J1: vulkan render without the device pin = stale pre-0.22.19 render
  } `
  {
    $tpl = Join-Path (Join-Path $scriptDir 'templates') "llama-swap.win-$tplBackend.yaml"
    if (-not (Test-Path $tpl)) { throw "template not found: $tpl" }
    $text = Get-Content -Raw $tpl

    # Profile-driven serving params. $pp always carries concrete values (a known
    # profile's, or the backend fallback's), so substitution is unconditional -
    # the fully-tokenized templates would otherwise leave __CTX__ etc. in a config.
    $text = $text.Replace('__CTX__', $pp.ctx)
    $text = $text.Replace('__KV_K__', $pp.kv_k).Replace('__KV_V__', $pp.kv_v)
    $text = $text.Replace('__FLASH_ATTN__', $pp.flash_attn)
    $text = $text.Replace('__MOE_26B__', $pp.moe_26b)
    if (-not $pp.include_26b) { $text = Remove-26bFromYaml -Text $text }
    # H4 Blackwell runtime env: CUDA_VISIBLE_DEVICES explicit (hybrid-graphics -1
    # trap) + CUDA_MODULE_LOADING=LAZY (llama.cpp #22696). Single-GPU Blackwell
    # profiles only — the dual-cuda template already pins devices per group.
    if ($profileId -match '^blackwell-') {
      $text = Add-GpuEnvToYaml -Text $text -EnvVars @('CUDA_VISIBLE_DEVICES=0','CUDA_MODULE_LOADING=LAZY')
    }

    $llamaDirFwd = $llamaDir.Replace('\', '/')
    $modelDirFwd = $modelDir.Replace('\', '/')
    # Replace token+backslash together so the template's own literal '\' path separator
    # (baked in right after each token, e.g. "__LLAMA_BIN__\llama-server.exe") also becomes
    # forward-slash - a plain token substitution would leave that literal backslash behind.
    $text = $text.Replace('__LLAMA_BIN__\', "$llamaDirFwd/").Replace('__MODELS__\', "$modelDirFwd/")
    $text = $text.Replace('__LLAMA_BIN__', $llamaDirFwd).Replace('__MODELS__', $modelDirFwd)
    if ($tplBackend -eq 'cpu') {
      $cores = (Get-CimInstance Win32_Processor | Measure-Object -Property NumberOfCores -Sum).Sum
      if (-not $cores -or $cores -lt 1) { $cores = [Environment]::ProcessorCount }
      $text = $text.Replace('__NTHREADS__', "$cores")
    }
    # UTF8 without BOM (llama-swap's YAML parser rejects a BOM). PS 5.1's -Encoding
    # UTF8 writes a BOM, so use an explicit no-BOM encoder for both hosts.
    $noBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($yamlDest, $text, $noBom)
  }

# -RenderOnly stops here: config rendered, no artifacts/build/manifest. Emit a
# compact JSON verdict describing the resolved profile so a test can assert on it.
if ($RenderOnly) {
  $rv = @{ render_only = $true; backend = $backend; render_backend = $tplBackend;
    profile = $profileId; ram_tier = $ramTier; big_ram = $bigRam;
    agent_ctx_tokens = $agentCtxTokens; include_26b = [bool]$pp.include_26b;
    moe_mode = "$($pp.moe_mode)"; yaml = $yamlDest } | ConvertTo-Json -Compress
  $rv
  exit 0
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
    # Task 3: seed the FRESH config from the profile's config_seed (profile-keyed
    # media defaults, e.g. 720p video on the big-VRAM tiers). Only this create
    # path seeds - an existing config.json is never touched (the Step SKIPs).
    $cfgText = Get-Content -Raw (Join-Path (Join-Path $scriptDir 'templates') 'config.json')
    $seed = $null
    if ($profileId -and (Test-Path $profilesJson)) {
      $pdoc = Get-Content -Raw $profilesJson | ConvertFrom-Json
      if ($pdoc.profiles.PSObject.Properties[$profileId]) { $seed = $pdoc.profiles.$profileId.config_seed }
    }
    if ($seed) {
      $cfgText = Merge-ConfigSeed -ConfigText $cfgText -Seed $seed -OffloadHome $HOME_DIR
      Write-Host "      config_seed ($profileId): $(@($seed.PSObject.Properties.Name) -join ', ')" -ForegroundColor DarkGray
    }
    $noBomCfg = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($cfgDest, $cfgText, $noBomCfg)
  }

# ---------------------------------------------------------------------------
# Step 8b: run-graph satisfier tooling (best-effort). offload_run_graph provisions a
# workflow's node manifest via ComfyUI-Manager's cm-cli; ensure it (REQUIRED) and
# comfy-cli (OPTIONAL convenience) are present in the ComfyUI venv. Only runs when a
# ComfyUI install is discoverable (COMFY_DIR override, else config default C:/ComfyUI)
# — a box without ComfyUI is unaffected (run-graph then simply DEFERs
# SATISFIER_UNAVAILABLE at call time, never crashes). A comfy-cli wheel-build failure
# is a WARN and continues; a ComfyUI-Manager clone failure fails LOUD.
# ---------------------------------------------------------------------------
if ($env:COMFY_DIR) { $comfyDir = $env:COMFY_DIR } else { $comfyDir = 'C:/ComfyUI' }
if ($env:COMFY_PY)  { $comfyPy  = $env:COMFY_PY }  else { $comfyPy  = Join-Path $comfyDir '.venv/Scripts/python.exe' }
if (Test-Path $comfyDir) {
  Write-Host "DO    run-graph deps (ComfyUI at $comfyDir)" -ForegroundColor Cyan
  $rgLog = Ensure-RunGraphDeps -ComfyPy $comfyPy -ComfyDir $comfyDir
  Write-Host "OK    run-graph deps: $rgLog" -ForegroundColor Green
} else {
  Write-Host "SKIP  run-graph deps (no ComfyUI at $comfyDir; offload_run_graph will DEFER SATISFIER_UNAVAILABLE, not crash)" -ForegroundColor DarkGray
}

# ---------------------------------------------------------------------------
# R3.5: write the version manifest now that every component version is known.
# ---------------------------------------------------------------------------
$manifest = [ordered]@{
  llama_cpp_tag  = $LLAMA_TAG
  llama_swap_tag = $SWAP_TAG
  backend        = $backend
  cuda_build     = $(if ($cudaBuild) { $cudaBuild.component } else { $null })   # H4: which CUDA family was installed
  cuda_driver    = $cudaDriver
  cuda_toolkit   = $cudaToolkit
  profile        = $profileId
  ram_tier       = $ramTier
  big_ram        = $bigRam
  agent_ctx_tokens = $agentCtxTokens
  render_backend = $tplBackend
  install_date   = (Get-Date -Format 'yyyy-MM-ddTHH:mm:ssK')
  components     = $manifestComponents
  models         = @($modelKeys | ForEach-Object { @{ name = $PINNED[$_].name; sha256 = $PINNED[$_].sha } })
}
$manifest | ConvertTo-Json -Depth 6 | Set-Content -Path $manifestPath -Encoding UTF8

# ---------------------------------------------------------------------------
# Step 9: start command + JSON verdict
# ---------------------------------------------------------------------------
$agentExe = Join-Path $harnessDir 'local-agent.exe'
Write-Host ""
Write-Host "Start the stack with:" -ForegroundColor White
Write-Host "  & '$swapExe' --config '$yamlDest' --listen 127.0.0.1:11436" -ForegroundColor White
Write-Host ""
# Deliverable 4: the agent's compaction budget must match the served window. The
# profile's agent_ctx_tokens is the -ctx-tokens value; the served --ctx-size in
# the rendered yaml matches it. openwebui-stack.sh reads LOCAL_AGENT_* env, so no
# launcher signature changes here — document the value for the operator instead.
Write-Host "Run the agent against this profile's served window ($agentCtxTokens tokens):" -ForegroundColor White
Write-Host "  & '$agentExe' `"<objective>`" -base http://127.0.0.1:11436 -model $($pp.resident_tier) -ctx-tokens $agentCtxTokens" -ForegroundColor White
Write-Host ""
$verdict = @{ installed = $true; backend = $backend; render_backend = $tplBackend; profile = $profileId;
  ram_tier = $ramTier; big_ram = $bigRam; agent_ctx_tokens = $agentCtxTokens; home = $HOME_DIR;
  next = 'run selftest.ps1' } | ConvertTo-Json -Compress
try { Stop-Transcript | Out-Null } catch {}
$verdict
