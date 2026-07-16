# setup/tests/install-cuda-build.test.ps1 - H4 unit tests for install.ps1's CUDA build
# selection (Select-CudaBuild) and the Blackwell runtime-env yaml injection
# (Add-GpuEnvToYaml). Uses the OFFLOAD_INSTALL_DOT_SOURCE=1 seam: install.ps1 defines
# its pure helpers and returns before ANY main-flow work (no dirs, no transcript,
# no detection, no downloads).
#
# Usage (both shells):
#   pwsh       -File setup/tests/install-cuda-build.test.ps1
#   powershell -ExecutionPolicy Bypass -File setup\tests\install-cuda-build.test.ps1
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
Assert ([bool](Get-Command Select-CudaBuild -ErrorAction SilentlyContinue)) 'dot-source seam defines Select-CudaBuild'
Assert ([bool](Get-Command Add-GpuEnvToYaml -ErrorAction SilentlyContinue)) 'dot-source seam defines Add-GpuEnvToYaml'

Write-Host "== Select-CudaBuild: Blackwell on a CUDA-13 driver -> pinned 13.3 build (serves) =="
$r = Select-CudaBuild -ProfileId 'blackwell-16' -CudaDriver '13.3' -CudaToolkit $null
Assert (-not $r.refuse) 'blackwell-16/13.3 not refused'
Assert ($r.component -eq 'llama-cuda13') 'blackwell-16/13.3 component=llama-cuda13'
Assert (($r.keys -contains 'llama-cuda13') -and ($r.keys -contains 'llama-cudart13')) 'blackwell-16/13.3 installs build + cudart13'
Assert ($r.tier -eq 'serves') 'blackwell-16/13.3 tier=serves'
Assert (($r.report -join ' ') -match 'cuBLAS')  'blackwell-16/13.3 report explains the perf tier'
Assert (($r.report -join ' ') -notmatch 'available on this box now') 'blackwell-16/13.3 no peak-toolkit note without a toolkit'

Write-Host "== Select-CudaBuild: 12.8 toolkit alongside a 13.x driver -> peak source-build noted =="
$r = Select-CudaBuild -ProfileId 'blackwell-16' -CudaDriver '13.3' -CudaToolkit '12.8'
Assert (-not $r.refuse) 'blackwell-16/13.3+tk12.8 not refused'
Assert ($r.component -eq 'llama-cuda13') 'blackwell-16/13.3+tk12.8 still serves on 13.3 prebuilt'
Assert (($r.report -join ' ') -match 'available on this box now') 'blackwell-16/13.3+tk12.8 peak source-build noted'

Write-Host "== Select-CudaBuild: blackwell-8 keys the same family =="
$r = Select-CudaBuild -ProfileId 'blackwell-8' -CudaDriver '13.3' -CudaToolkit $null
Assert ((-not $r.refuse) -and $r.component -eq 'llama-cuda13') 'blackwell-8/13.3 -> llama-cuda13'
# The big-VRAM tiers (configs 13-15) share the ^blackwell- selection unchanged.
$r = Select-CudaBuild -ProfileId 'blackwell-72' -CudaDriver '13.3' -CudaToolkit $null
Assert ((-not $r.refuse) -and $r.component -eq 'llama-cuda13') 'blackwell-72/13.3 -> llama-cuda13'

Write-Host "== Select-CudaBuild: Blackwell on a 12.8/12.9 driver -> refuse with driver-or-source guidance =="
$r = Select-CudaBuild -ProfileId 'blackwell-16' -CudaDriver '12.8' -CudaToolkit '12.8'
Assert ($r.refuse) 'blackwell-16/12.8 refused'
Assert ($r.keys.Count -eq 0) 'blackwell-16/12.8 installs nothing'
Assert (($r.report -join ' ') -match 'upgrade the NVIDIA driver' -or ($r.report -join ' ') -match 'Upgrade the NVIDIA driver') 'blackwell-16/12.8 guidance names the driver upgrade'
Assert (($r.report -join ' ') -match 'source-build') 'blackwell-16/12.8 guidance names the source-build peak path'

Write-Host "== Select-CudaBuild: Blackwell on 12.4-only / undetected CUDA -> refuse =="
$r = Select-CudaBuild -ProfileId 'blackwell-16' -CudaDriver '12.4' -CudaToolkit $null
Assert ($r.refuse) 'blackwell-16/12.4 refused'
Assert (($r.report -join ' ') -match 'NO sm_120') 'blackwell-16/12.4 explains the 12.4 prebuilt gap'
$r = Select-CudaBuild -ProfileId 'blackwell-16' -CudaDriver $null -CudaToolkit $null
Assert ($r.refuse) 'blackwell-16/undetected refused'
Assert (($r.report -join ' ') -match 'undetected') 'blackwell-16/undetected says the CUDA was undetected'

Write-Host "== Select-CudaBuild: dual-gpu -> refuse with the multi-arch source-build guidance =="
$r = Select-CudaBuild -ProfileId 'dual-gpu' -CudaDriver '13.3' -CudaToolkit '12.9'
Assert ($r.refuse) 'dual-gpu refused (no prebuilt covers sm_70+sm_120)'
Assert (($r.report -join ' ') -match '70;120') 'dual-gpu guidance carries -DCMAKE_CUDA_ARCHITECTURES="70;120"'
Assert (($r.report -join ' ') -match '12\.8/12\.9') 'dual-gpu guidance pins the 12.8/12.9 toolkit requirement'

Write-Host "== Select-CudaBuild: non-Blackwell profiles keep the 12.4 status quo =="
foreach ($p in @('ampere-8', 'ampere-16', 'ampere-6', 'volta-16', $null)) {
  $r = Select-CudaBuild -ProfileId $p -CudaDriver '12.4' -CudaToolkit $null
  $label = if ($p) { $p } else { '(none)' }
  Assert ((-not $r.refuse) -and $r.component -eq 'llama-cuda') "$label -> llama-cuda (12.4 prebuilt)"
}
# Even on a CUDA-13 driver a non-Blackwell card stays on the verified 12.4 build.
$r = Select-CudaBuild -ProfileId 'ampere-8' -CudaDriver '13.3' -CudaToolkit $null
Assert ((-not $r.refuse) -and $r.component -eq 'llama-cuda') 'ampere-8 on a 13.x driver stays on llama-cuda'

Write-Host "== Add-GpuEnvToYaml: inserts env on models without one, extends the 26B's list =="
$yaml = @'
healthCheckTimeout: 300

macros:
  common: >-
    --ctx-size 32768 --port ${PORT}

models:
  offload-e4b:
    aliases: [gemma4-e4b]
    cmd: >-
      C:/x/llama/llama-server.exe -m C:/x/models/e4b.gguf
      -ngl 99 ${common}
    ttl: 300
  gemma4-26b-a4b:
    env: [GGML_CUDA_DISABLE_GRAPHS=1]
    cmd: >-
      C:/x/llama/llama-server.exe -m C:/x/models/26b.gguf
      -ngl 99 ${common}
    ttl: 300

groups:
  offload-family:
    swap: true
    members: [offload-e4b, gemma4-26b-a4b]
'@
$vars = @('CUDA_VISIBLE_DEVICES=0', 'CUDA_MODULE_LOADING=LAZY')
$outText = Add-GpuEnvToYaml -Text $yaml -EnvVars $vars
$outLines = $outText -split "`r?`n"
$e4bIdx = [array]::IndexOf($outLines, ($outLines | Where-Object { $_ -match '^\s{2}offload-e4b:' } | Select-Object -First 1))
Assert ($outLines[$e4bIdx + 1] -match '^\s{4}env: \[CUDA_VISIBLE_DEVICES=0, CUDA_MODULE_LOADING=LAZY\]$') 'e4b gains an env line right after its key'
Assert ($outText -match '(?m)^\s{4}env: \[GGML_CUDA_DISABLE_GRAPHS=1, CUDA_VISIBLE_DEVICES=0, CUDA_MODULE_LOADING=LAZY\]$') '26B env list extended, GGML flag kept first'
Assert (@($outLines | Where-Object { $_ -match 'CUDA_VISIBLE_DEVICES=0' }).Count -eq 2) 'exactly one injection per model block'
Assert ($outText -notmatch '(?m)^groups:[\s\S]*CUDA_VISIBLE_DEVICES') 'groups: section untouched'
Assert ($outText -match [regex]::Escape('macros:')) 'macros section untouched'

Write-Host "== Add-GpuEnvToYaml: idempotent (second pass adds nothing) =="
$twice = Add-GpuEnvToYaml -Text $outText -EnvVars $vars
Assert ($twice -eq $outText) 'second application is a no-op'

Write-Host ""
if ($failures -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $failures" -ForegroundColor Red; exit 1
