# setup/tests/install-config-seed.test.ps1 - Task 3 (2026-07-16 Blackwell-tier plan):
# unit tests for the profile-keyed config seeding. Merge-ConfigSeed is the pure
# overlay (profile config_seed values onto the template config.json text); Step 8
# applies it ONLY when creating ~/.local-offload/config.json fresh - an existing
# per-machine config is never touched, so there are no de-confliction rules.
# Uses the OFFLOAD_INSTALL_DOT_SOURCE=1 seam (no main-flow work).
#
# Usage (both shells):
#   pwsh       -File setup/tests/install-config-seed.test.ps1
#   powershell -ExecutionPolicy Bypass -File setup\tests\install-config-seed.test.ps1
# Exit: 0 all assertions pass, 1 otherwise.
$ErrorActionPreference = 'Stop'
$here     = Split-Path -Parent $MyInvocation.MyCommand.Path
$setupDir = Split-Path -Parent $here

$failures = 0
function Assert {
  param([bool]$Cond, [string]$Name)
  if ($Cond) { Write-Host "  PASS $Name" -ForegroundColor Green }
  else       { Write-Host "  FAIL $Name" -ForegroundColor Red; $script:failures++ }
}

$prevSeam = $env:OFFLOAD_INSTALL_DOT_SOURCE
try {
  $env:OFFLOAD_INSTALL_DOT_SOURCE = '1'
  . (Join-Path $setupDir 'install.ps1')
} finally {
  if ($null -ne $prevSeam) { $env:OFFLOAD_INSTALL_DOT_SOURCE = $prevSeam }
  else { Remove-Item Env:OFFLOAD_INSTALL_DOT_SOURCE -ErrorAction SilentlyContinue }
}
Assert ([bool](Get-Command Merge-ConfigSeed -ErrorAction SilentlyContinue)) 'dot-source seam defines Merge-ConfigSeed'

$tplPath = Join-Path (Join-Path $setupDir 'templates') 'config.json'
$tplText = Get-Content -Raw $tplPath

Write-Host "== Merge-ConfigSeed: overlay applied, everything else untouched =="
$seed = [pscustomobject]@{ videogen_width = 1280; videogen_height = 720; videogen_frames = 49 }
$merged = Merge-ConfigSeed -ConfigText $tplText -Seed $seed
$obj = $merged | ConvertFrom-Json
Assert ($obj.videogen_width -eq 1280)  'videogen_width seeded to 1280'
Assert ($obj.videogen_height -eq 720)  'videogen_height seeded to 720'
Assert ($obj.videogen_frames -eq 49)   'videogen_frames seeded to 49'
$tplObj = $tplText | ConvertFrom-Json
Assert ($obj.endpoint -eq $tplObj.endpoint) 'unrelated key (endpoint) untouched'
Assert ($obj.imagegen_ckpt -eq $tplObj.imagegen_ckpt) 'imagegen_ckpt untouched (roster stays per-machine)'
Assert ($obj.model -eq $tplObj.model) 'model untouched'
$tplKeys = @($tplObj.PSObject.Properties.Name)
$outKeys = @($obj.PSObject.Properties.Name)
Assert (@($tplKeys | Where-Object { $outKeys -notcontains $_ }).Count -eq 0) 'no template keys lost in the merge'

Write-Host "== Merge-ConfigSeed: null/empty seed is identity =="
Assert ((Merge-ConfigSeed -ConfigText $tplText -Seed $null) -eq $tplText) 'null seed returns the input text unchanged'
Assert ((Merge-ConfigSeed -ConfigText $tplText -Seed ([pscustomobject]@{})) -eq $tplText) 'empty seed returns the input text unchanged'

Write-Host "== Merge-ConfigSeed: a seed key absent from the template is added =="
$merged2 = Merge-ConfigSeed -ConfigText $tplText -Seed ([pscustomobject]@{ videogen_upscale_width = 1920 })
Assert (($merged2 | ConvertFrom-Json).videogen_upscale_width -eq 1920) 'absent key added with its value'

Write-Host "== profiles.json: quality-first config_seed on every >=16GB CUDA tier =="
# 2026-07-16 quality-first policy (spec: 2026-07-16-quality-first-generation-design.md):
# every >=16GB CUDA tier seeds the PROVEN highest-quality bindings on a fresh install —
# HiDream-O1 bf16 Base via its family graph + Wan Q8_0 experts + umt5 fp16 + 720p x 81.
$profiles = (Get-Content -Raw (Join-Path (Join-Path $setupDir 'templates') 'profiles.json') | ConvertFrom-Json).profiles
foreach ($tier in @('blackwell-72','blackwell-48','blackwell-32','blackwell-16','ampere-16','volta-16')) {
  $s = $profiles.$tier.config_seed
  Assert ($s.imagegen_family -eq 'hidream-o1')                              "$tier seeds imagegen_family=hidream-o1"
  Assert ($s.imagegen_ckpt -eq 'hidream_o1_image_bf16.safetensors')         "$tier seeds the bf16 Base checkpoint"
  Assert ($s.imagegen_timeout_sec -ge 3600)                                 "$tier seeds a quality-length image timeout"
  Assert ($s.videogen_unet_high -eq 'Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf')  "$tier seeds the Q8_0 high-noise expert"
  Assert ($s.videogen_unet_low -eq 'Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf')    "$tier seeds the Q8_0 low-noise expert"
  Assert ($s.videogen_text_encoder -eq 'umt5_xxl_fp16.safetensors')         "$tier seeds the fp16 text encoder"
  Assert ($s.videogen_width -eq 1280 -and $s.videogen_height -eq 720)       "$tier seeds 720p video"
  Assert ($s.videogen_frames -eq 81)                                        "$tier seeds the 81-frame native ceiling"
}
# 8GB tiers: SDXL-class image stays until O1-on-8GB is verified on a real box (policy table).
Assert ($null -eq $profiles.'ampere-8'.config_seed.imagegen_family)         'ampere-8 does NOT seed the o1 family (unverified on 8GB)'
Assert ($null -eq $profiles.'blackwell-8'.config_seed.imagegen_family)      'blackwell-8 does NOT seed the o1 family (unverified on 8GB)'

Write-Host ""
if ($failures -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $failures" -ForegroundColor Red; exit 1
