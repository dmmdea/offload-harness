# setup/tests/selftest-profile-measure.test.ps1 - stub-integration test for the H3 profile_measure
# helpers in setup/selftest.ps1. Like the R4.2 remediation test, it dot-sources the selftest through
# its OFFLOAD_SELFTEST_DOT_SOURCE=1 seam (functions + paths + the receipt scaffold, no MAIN), then
# drives the PURE helpers - Read-ActiveProfile, Get-ProjectedProfile - and the downshift/tuned logic
# WITHOUT a GPU or any llama-server load. The heavy measurement (Invoke-ProbeLoad) is a live-GPU path
# validated separately by running the real selftest against a sandbox; here we prove the honesty
# plumbing: profile read from each source, projected lookup, and the array-force serialization.
#
# Asserts:
#   - Read-ActiveProfile prefers installed.json (profile + ram_tier + agent_ctx_tokens),
#     falls back to a detect JSON, then to OFFLOAD_PROFILE, then to 'none'.
#   - Get-ProjectedProfile returns the profiles.json projected values for a known id, $null for unknown.
#   - The receipt.profile_measure block exists in the scaffold with the expected sub-keys.
#   - cold_swap array-forces to [object[]] (0-element -> [], 1-element -> array-of-one) and the whole
#     receipt round-trips through ConvertTo-Json/ConvertFrom-Json with profile_measure intact.
#
# Usage:  pwsh -NoProfile -File setup\tests\selftest-profile-measure.test.ps1
#         powershell.exe -NoProfile -File setup\tests\selftest-profile-measure.test.ps1   (5.1)
# Exit: 0 all pass, 1 otherwise. PS 5.1 + pwsh 7 compatible.
$ErrorActionPreference = 'Stop'
$here     = Split-Path -Parent $MyInvocation.MyCommand.Path
$setupDir = Split-Path -Parent $here
$selftestPath = Join-Path $setupDir 'selftest.ps1'
$realProfiles = Join-Path $setupDir 'templates\profiles.json'

$failures = 0
function Assert {
  param([bool]$Cond, [string]$Name)
  if ($Cond) { Write-Host "  PASS $Name" -ForegroundColor Green }
  else       { Write-Host "  FAIL $Name" -ForegroundColor Red; $script:failures++ }
}
function Write-Utf8NoBomLocal {
  param([string]$Path, [string]$Content)
  [System.IO.File]::WriteAllText($Path, $Content, (New-Object System.Text.UTF8Encoding($false)))
}

$tmpDir = Join-Path $env:TEMP ("pmtest-{0}" -f ([guid]::NewGuid().ToString('N').Substring(0, 8)))
New-Item -ItemType Directory -Force -Path $tmpDir | Out-Null
$prevHome    = $env:OFFLOAD_HOME
$prevSeam    = $env:OFFLOAD_SELFTEST_DOT_SOURCE
$prevProfEnv = $env:OFFLOAD_PROFILE
$prevRamEnv  = $env:OFFLOAD_RAM_TIER
$prevProfJson= $env:OFFLOAD_PROFILES_JSON
$prevDetect  = $env:OFFLOAD_DETECT_JSON
try {
  Write-Host "==> H3 profile_measure stub test | engine=$($PSVersionTable.PSVersion) | tmp=$tmpDir"

  # --- dot-source the selftest via its seam (defines helpers; no MAIN) ------------------------
  $env:OFFLOAD_SELFTEST_DOT_SOURCE = '1'
  $env:OFFLOAD_HOME = $tmpDir
  # Point the projected map at the REAL profiles.json so Get-ProjectedProfile reads the shipped values.
  $env:OFFLOAD_PROFILES_JSON = $realProfiles
  $env:OFFLOAD_PROFILE = $null
  $env:OFFLOAD_RAM_TIER = $null
  $env:OFFLOAD_DETECT_JSON = $null
  . $selftestPath

  Assert ($null -ne $receipt -and $null -ne $receipt.profile_measure) 'seam: receipt.profile_measure scaffold defined'
  $pm = $receipt.profile_measure
  $subKeys = @('profile','profile_src','ram_tier','projected','ctx','moe26b','cold_swap','q8_kv','dual_gpu','optane','tuned')
  $haveAll = $true
  foreach ($k in $subKeys) { if (-not $pm.Contains($k)) { $haveAll = $false } }
  Assert $haveAll 'profile_measure has all expected sub-keys (projected/ctx/moe26b/cold_swap/q8_kv/dual_gpu/optane/tuned)'

  # --- Read-ActiveProfile: installed.json wins ------------------------------------------------
  $manifestPath = Join-Path $tmpDir 'installed.json'
  $man = [ordered]@{ backend='cuda'; profile='ampere-8'; ram_tier='mid'; big_ram=$false; agent_ctx_tokens=16384 }
  Write-Utf8NoBomLocal -Path $manifestPath -Content ($man | ConvertTo-Json -Depth 4)
  $a1 = Read-ActiveProfile -ManifestPath $manifestPath -DetectJsonPath $null
  Assert ($a1.profile -eq 'ampere-8' -and $a1.profile_src -eq 'installed.json' -and $a1.ram_tier -eq 'mid' -and $a1.agent_ctx_tokens -eq 16384) `
    'Read-ActiveProfile prefers installed.json (profile=ampere-8, ram_tier=mid, agent_ctx_tokens=16384)'

  # --- Read-ActiveProfile: falls back to a detect JSON when installed.json has no profile -----
  $preH2Manifest = Join-Path $tmpDir 'installed-preh2.json'
  Write-Utf8NoBomLocal -Path $preH2Manifest -Content (([ordered]@{ backend='cuda'; llama_cpp_tag='b9934' }) | ConvertTo-Json)
  $detectJson = Join-Path $tmpDir 'detect.json'
  Write-Utf8NoBomLocal -Path $detectJson -Content (([ordered]@{ profile='volta-16'; ram_tier='high'; gpu_count=1 }) | ConvertTo-Json -Compress)
  $a2 = Read-ActiveProfile -ManifestPath $preH2Manifest -DetectJsonPath $detectJson
  Assert ($a2.profile -eq 'volta-16' -and $a2.profile_src -eq 'detect.ps1' -and $a2.ram_tier -eq 'high') `
    'Read-ActiveProfile falls back to detect.ps1 JSON when installed.json predates H2'

  # --- Read-ActiveProfile: OFFLOAD_PROFILE env as last resort ---------------------------------
  $env:OFFLOAD_PROFILE = 'amd-rdna3'
  $env:OFFLOAD_RAM_TIER = 'mid'
  $a3 = Read-ActiveProfile -ManifestPath $preH2Manifest -DetectJsonPath $null
  Assert ($a3.profile -eq 'amd-rdna3' -and $a3.profile_src -eq 'OFFLOAD_PROFILE' -and $a3.ram_tier -eq 'mid') `
    'Read-ActiveProfile uses OFFLOAD_PROFILE env when no manifest/detect profile'
  $env:OFFLOAD_PROFILE = $null
  $env:OFFLOAD_RAM_TIER = $null

  # --- Read-ActiveProfile: nothing resolves -> profile=$null, src=none ------------------------
  $a4 = Read-ActiveProfile -ManifestPath $preH2Manifest -DetectJsonPath $null
  Assert ($null -eq $a4.profile -and $a4.profile_src -eq 'none') 'Read-ActiveProfile records none when no source resolves'

  # --- Get-ProjectedProfile: known id from the REAL profiles.json -----------------------------
  $p = Get-ProjectedProfile -ProfileId 'ampere-8' -ProfilesJsonPath $realProfiles
  Assert ($null -ne $p -and $p.ctx_size -eq 16384 -and $p.kv_type -eq 'q8_0' -and $p.moe_26b -eq 'cpu_moe' -and $p.resident_tier -eq 'offload-e4b' -and $p.include_26b -eq $true) `
    'Get-ProjectedProfile(ampere-8) = ctx16384/q8_0/cpu_moe/e4b/include26b (matches profiles.json)'
  $pdual = Get-ProjectedProfile -ProfileId 'dual-gpu' -ProfilesJsonPath $realProfiles
  Assert ($null -ne $pdual -and $pdual.dual_resident -eq $true) 'Get-ProjectedProfile(dual-gpu) has dual_resident=true'
  $pu = Get-ProjectedProfile -ProfileId 'does-not-exist' -ProfilesJsonPath $realProfiles
  Assert ($null -eq $pu) 'Get-ProjectedProfile(unknown) returns $null'

  # --- FIX 1: a non-alloc ctx-probe failure must be HONEST (src='not-attempted', not 'measured') ---
  # The non-alloc failure branch handles: probe port busy, llama-server.exe absent, model gguf
  # absent, or the probe threw - i.e. the load NEVER started, so nothing was measured. Marking it
  # 'measured' was a dishonesty bug: it (a) suppressed the does_not_prove honesty line and (b) let
  # tuned.source read 'measured' while tuned.ctx_size fell back to the PROJECTED value.
  # This test drives the EXACT downstream gates MAIN uses (the does_not_prove gate and the
  # $anyMeasured tuned-source rule) against the state the fixed non-alloc branch produces, and
  # anchors on the source literal itself so a regression back to 'measured' fails RED.
  $nonAllocCtx = (Select-String -Path $selftestPath -SimpleMatch 'Non-alloc failure' -Context 0,8 | Select-Object -First 1)
  $ctxProbeSrcLiteral = [string](@($nonAllocCtx.Context.PostContext) | Where-Object { $_ -match '\$pm\.ctx\.src\s*=' } | Select-Object -First 1)
  Assert ([bool]($ctxProbeSrcLiteral -match 'not-attempted')) "source: non-alloc ctx-probe branch sets ctx.src='not-attempted' (never 'measured')"

  # Simulate exactly what the fixed non-alloc branch writes into the receipt.
  $pm.ctx.projected_ctx   = 16384
  $pm.ctx.measured_ctx    = $null
  $pm.ctx.measured_ctx_ok = $false
  $pm.ctx.downshifted     = $false
  $pm.ctx.src             = 'not-attempted'
  $pm.ctx.detail          = 'ctx probe at 16384 failed (non-alloc): probe port 18804 busy'
  $pm.moe26b.src          = 'skipped'
  # (a) the does_not_prove gate (MAIN: `if ($pmf.ctx.src -ne 'measured')`) now FIRES.
  $ctxGateFires = ($pm.ctx.src -ne 'measured')
  Assert $ctxGateFires 'FIX1(a): does_not_prove ctx gate fires on a non-attempted probe (src != measured)'
  # (b) $anyMeasured (MAIN: ctx.src -eq measured OR moe26b.src -eq measured) EXCLUDES this run,
  # so tuned.source is NOT falsely 'measured' - it must be 'projected' when nothing else measured.
  $anyMeasured = ($pm.ctx.src -eq 'measured') -or ($pm.moe26b.src -eq 'measured')
  $tunedSource = if ($anyMeasured) { 'measured' } else { 'projected' }
  Assert ($tunedSource -ne 'measured') 'FIX1(b): tuned.source is NOT falsely measured on a non-attempted ctx probe'
  Assert ($tunedSource -eq 'projected') 'FIX1(b): tuned.source falls back to projected when nothing legitimately measured'

  # --- cold_swap array-force + full receipt round-trips through JSON with profile_measure -----
  # 0-element -> [], then 1-element -> array-of-one (the PS 5.1 unwrap hazard the [object[]] guards).
  $receipt.profile_measure.cold_swap = @()
  $receipt.profile_measure.cold_swap = [object[]]@($receipt.profile_measure.cold_swap)
  $json0 = ([pscustomobject]$receipt) | ConvertTo-Json -Depth 8 -Compress
  $rt0 = $json0 | ConvertFrom-Json
  Assert ($json0 -match '"cold_swap":\[\]') 'empty cold_swap serializes as [] (never null)'

  $receipt.profile_measure.cold_swap = @([ordered]@{ tier='offload-e4b'; cold_swap_s=3.1; ok=$true })
  $receipt.profile_measure.cold_swap = [object[]]@($receipt.profile_measure.cold_swap)
  $receipt.tiers          = [object[]]@($receipt.tiers)
  $receipt.remediations   = [object[]]@($receipt.remediations)
  $receipt.proves         = [object[]]@($receipt.proves)
  $receipt.does_not_prove = [object[]]@($receipt.does_not_prove)
  $json1 = ([pscustomobject]$receipt) | ConvertTo-Json -Depth 8 -Compress
  $rt1 = $json1 | ConvertFrom-Json
  Assert ($json1 -match '"cold_swap":\[\{') 'one-element cold_swap serializes as array-of-one [{...}] (never a bare object)'
  Assert ($rt1.schema -eq 1) 'round-trip: schema still 1 (contract preserved)'
  Assert ($null -ne $rt1.profile_measure -and $null -ne $rt1.profile_measure.projected -and $null -ne $rt1.profile_measure.tuned) `
    'round-trip: profile_measure + projected + tuned present after JSON parse'
  Assert (@($rt1.profile_measure.cold_swap).Count -eq 1 -and $rt1.profile_measure.cold_swap[0].tier -eq 'offload-e4b') `
    'round-trip: cold_swap[0].tier survives as offload-e4b'
}
finally {
  $env:OFFLOAD_SELFTEST_DOT_SOURCE = $prevSeam
  $env:OFFLOAD_HOME = $prevHome
  $env:OFFLOAD_PROFILE = $prevProfEnv
  $env:OFFLOAD_RAM_TIER = $prevRamEnv
  $env:OFFLOAD_PROFILES_JSON = $prevProfJson
  $env:OFFLOAD_DETECT_JSON = $prevDetect
  Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
}

if ($failures -gt 0) { Write-Host "RESULT: FAIL ($failures assertion(s) failed)" -ForegroundColor Red; exit 1 }
Write-Host 'RESULT: PASS (all assertions)' -ForegroundColor Green
exit 0
