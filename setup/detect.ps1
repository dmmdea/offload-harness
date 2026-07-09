# setup/detect.ps1 — hardware/OS detection for the offload-harness installer.
# Prints human-readable findings, then a FINAL machine-readable JSON line.
# Exit 0 = a viable backend exists; exit 1 = hard blocker (explained on stderr).
$ErrorActionPreference = 'Stop'
$warnings = @()

$os = if ($env:OS -eq 'Windows_NT') { 'windows' } else { 'other' }
if ($os -ne 'windows') { Write-Error 'This detector targets Windows. Use the Linux/WSL skill path instead.'; exit 1 }

$gpus = Get-CimInstance Win32_VideoController | Where-Object { $_.Name }
$gpuNames = ($gpus | ForEach-Object Name) -join '; '
$nvidia = $gpus | Where-Object { $_.Name -match 'NVIDIA' } | Select-Object -First 1
$amd    = $gpus | Where-Object { $_.Name -match 'AMD|Radeon' } | Select-Object -First 1
# Detection contract: NVIDIA present = CIM name match OR nvidia-smi resolves.
$nvidiaSmi = Get-Command nvidia-smi -ErrorAction SilentlyContinue

$vendor = 'none'; $backend = 'cpu'; $gpuName = ''
if ($nvidia) { $vendor = 'nvidia'; $backend = 'cuda';   $gpuName = $nvidia.Name }
elseif ($nvidiaSmi) {
  # nvidia-smi present but CIM naming missed the adapter — still NVIDIA/cuda per contract.
  $vendor = 'nvidia'; $backend = 'cuda'
  try { $gpuName = (& nvidia-smi --query-gpu=name --format=csv,noheader 2>$null | Select-Object -First 1).Trim() }
  catch { $gpuName = '' }
  if (-not $gpuName) { $gpuName = '' }
}
elseif ($amd) { $vendor = 'amd';   $backend = 'vulkan'; $gpuName = $amd.Name }

# Dedicated VRAM (iGPUs report small carve-out; that is EXPECTED — Vulkan uses shared memory)
# Primary source: registry qwMemorySize (64-bit, matched by DriverDesc = GPU name);
# fallback: Win32_VideoController.AdapterRAM (uint32 — saturates at ~4GB on big cards).
$vramGB = 0
if ($vendor -ne 'none') {
  $qw = $null
  try {
    $qw = (Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\0*' -ErrorAction SilentlyContinue |
      Where-Object { $_.DriverDesc -eq $gpuName -and $_.'HardwareInformation.qwMemorySize' } |
      Select-Object -First 1).'HardwareInformation.qwMemorySize'
  } catch { $qw = $null }
  if ($qw) { $vramGB = [math]::Round($qw / 1GB, 1) }
  else {
    $adapterRAM = ($gpus | Where-Object { $_.Name -eq $gpuName }).AdapterRAM
    if ($adapterRAM) { $vramGB = [math]::Round($adapterRAM / 1GB, 1) }
  }
}
$ramGB  = [math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB)
# Free disk is measured on the INSTALL TARGET drive: models/binaries go under
# $env:OFFLOAD_HOME (default $HOME\offload-stack), not necessarily where this script lives.
$installRoot = if ($env:OFFLOAD_HOME) { $env:OFFLOAD_HOME } else { $HOME }
$targetDrive = (Split-Path -Qualifier $installRoot).TrimEnd(':')
$diskGB = [math]::Round((Get-PSDrive -Name $targetDrive).Free / 1GB)

if ($ramGB -lt 32) { $warnings += "RAM ${ramGB}GB < 32GB: install only the E4B workhorse tier (set OFFLOAD_WITH_FAMILY=0)" }
if ($diskGB -lt 25) { Write-Error "Only ${diskGB}GB free on ${targetDrive}: (install target drive); need >=25GB for models + binaries."; exit 1 }
if ($vendor -eq 'amd') {
  $warnings += 'AMD path uses the llama.cpp VULKAN backend (native Windows). ROCm/HIP is NOT supported on RDNA3 iGPUs (gfx1103) and WSL2 cannot accelerate AMD iGPUs - do not attempt either.'
  $warnings += 'Keep the AMD Adrenalin driver CURRENT: an older driver (25.11.1) has a known deep-context Vulkan crash (llama.cpp #17432). selftest.ps1 includes a canary for it.'
}

Write-Host "OS: windows | GPUs: $gpuNames"
Write-Host "Vendor: $vendor | Backend: $backend | Dedicated VRAM: ${vramGB}GB | RAM: ${ramGB}GB | Free disk: ${diskGB}GB on ${targetDrive}: (install target)"
$warnings | ForEach-Object { Write-Host "WARN: $_" }

@{ os=$os; gpu_vendor=$vendor; gpu_name=$gpuName; vram_dedicated_gb=$vramGB; ram_gb=$ramGB;
   disk_free_gb=$diskGB; backend=$backend; warnings=$warnings } | ConvertTo-Json -Compress
