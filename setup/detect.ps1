# setup/detect.ps1 — hardware/OS detection for the offload-harness installer.
# Prints human-readable findings, then a FINAL machine-readable JSON line.
# Exit 0 = a viable backend exists; exit 1 = hard blocker (explained on stderr).
#
# H1 (arch-class matrix): in addition to the base verdict this now emits
#   gpu_arch  — blackwell|ampere|ada|volta|rdna3|gcn|other|none  (name-regex match)
#   gpu_count — number of discrete NVIDIA+AMD GPUs (Intel iGPUs excluded)
#   profile   — one of the arch-class ids the installer (H2) keys a template off
#   ram_tier  — high|mid|low|min  (drives the 26B --cpu-moe decision)
# The full 12-config matrix lives in
#   docs/superpowers/plans/2026-07-11-agent-capabilities-and-hardware-matrix.md
#
# Run modes:
#   detect.ps1            -> normal detection, JSON on the last line, exit 0/1
#   detect.ps1 -SelfTest  -> run the classifier self-checks, print PASS/FAIL,
#                            exit 0 all-pass / 1 any-fail. No hardware detection.
[CmdletBinding()]
param([switch]$SelfTest)

# ---------------------------------------------------------------------------
# Classification helpers (pure functions — no hardware access, so they are
# unit-testable via -SelfTest and by setup/detect.tests.ps1).
# ---------------------------------------------------------------------------

# Map a raw GPU name to an arch class. Case-insensitive regex rules; first hit wins.
# Rules (documented per the matrix's config list):
#   blackwell : RTX 50-series (5060 / 5060 Ti / 5090 / any "RTX 50xx" / "RTX 50").
#   ada       : RTX 40-series (4060 / 4090 / any "RTX 40xx").
#   ampere    : RTX 30-series (3050 / 3070 / 3080 / 3090 / any "RTX 30xx").
#   volta     : Tesla V100 / bare "V100".
#   rdna3     : AMD RDNA3 — Radeon 780M/760M iGPU and 7xxx discrete (RX 7900, etc.).
#   gcn       : older AMD — Vega (e.g. "Vega 7" iGPU) / GCN-era parts.
#   other     : a GPU we recognise as NVIDIA/AMD but not a class above.
#   none      : empty / no GPU name.
function Get-GpuArch {
  param([string]$Name)
  if ([string]::IsNullOrWhiteSpace($Name)) { return 'none' }
  # NVIDIA RTX generations — match the 4-digit model so "RTX 3070" -> ampere.
  if ($Name -imatch 'RTX\s*50\d{2}' -or $Name -imatch 'RTX\s*50\b') { return 'blackwell' }
  # RTX PRO Blackwell workstation cards ("NVIDIA RTX PRO 5000 Blackwell"): "RTX PRO"
  # breaks the RTX\s*50xx match above, so match the explicit Blackwell suffix, plus
  # the "RTX PRO NNNN" branding defensively (that branding IS the Blackwell pro
  # generation — pre-Blackwell pro cards were "RTX A6000" / "RTX 6000 Ada Generation",
  # which do not match). CAVEAT: a future non-Blackwell "RTX PRO" generation needs a
  # new rule ABOVE this one.
  if ($Name -imatch '\bBlackwell\b') { return 'blackwell' }
  if ($Name -imatch 'RTX\s+PRO\s+\d{4}') { return 'blackwell' }
  if ($Name -imatch 'RTX\s*40\d{2}' -or $Name -imatch 'RTX\s*40\b') { return 'ada' }
  if ($Name -imatch 'RTX\s*30\d{2}' -or $Name -imatch 'RTX\s*30\b') { return 'ampere' }
  # Tesla / data-center Volta.
  if ($Name -imatch '\bV100\b') { return 'volta' }
  # AMD RDNA3: 780M/760M iGPU (Phoenix) and 7000-series discrete.
  if ($Name -imatch '\b7\d{2}M\b' -or $Name -imatch 'RX\s*7\d{3}' -or $Name -imatch 'RDNA\s*3') { return 'rdna3' }
  # AMD GCN / Vega (older iGPU + discrete). "Vega 7" is a Ryzen APU iGPU.
  if ($Name -imatch '\bVega\b' -or $Name -imatch '\bGCN\b') { return 'gcn' }
  # Recognised vendor but unclassified generation.
  if ($Name -imatch 'NVIDIA|GeForce|Quadro|Tesla|AMD|Radeon') { return 'other' }
  return 'other'
}

# Decide whether an NVIDIA card fell through to the VRAM-banded ampere default
# without being recognised by any arch regex. When arch=other AND vendor=nvidia,
# Get-Profile assigns an ampere-16/-8/-6 profile purely by VRAM band with no
# other signal, so the operator should be warned the card was unidentified.
# Returns the warning string, or $null when no warning is warranted. Pure fn.
function Get-UnrecognizedNvidiaWarning {
  param(
    [string]$Vendor,      # nvidia|amd|none
    [string]$GpuArch,     # blackwell|ampere|ada|volta|rdna3|gcn|other|none
    [string]$GpuName,
    [string]$ProfileId
  )
  if ($Vendor -eq 'nvidia' -and $GpuArch -eq 'other') {
    return "unrecognized NVIDIA GPU '$GpuName' (arch=other) - using profile '$ProfileId' by VRAM band; verify the serving template fits before relying on it."
  }
  return $null
}

# Parse the driver's max CUDA out of the nvidia-smi banner. Two header formats
# exist in the wild: classic drivers print "CUDA Version: 13.3"; newer drivers
# (e.g. 610.62) print "KMD Version: 610.62   CUDA UMD Version: 13.3" — the old
# regex missed the UMD form and silently reported null on real Blackwell boxes
# (found live on the workstation, 2026-07-15). Pure function — self-tested.
function Get-CudaFromSmiHeader {
  param([string]$SmiText)
  if ([string]::IsNullOrWhiteSpace($SmiText)) { return $null }
  $m = [regex]::Match($SmiText, 'CUDA(?:\s+UMD)?\s+Version:\s*([0-9]+\.[0-9]+)')
  if ($m.Success) { return $m.Groups[1].Value }
  return $null
}

# J1: classify an AMD Adrenalin (Radeon Software) version string against the known
# deep-context Vulkan crash class (llama.cpp #17432 - present in 25.11.1 and older).
# Returns a warning string for affected/unreadable versions, $null when the driver is
# newer than the known-bad class. Pure function - self-tested. The caller reads the
# actual version from the registry (Get-AdrenalinVersion below).
function Get-AdrenalinWarning {
  param([string]$Version)
  $knownBad = [version]'25.11.1'
  if ([string]::IsNullOrWhiteSpace($Version)) {
    return "Adrenalin version UNREADABLE - keep the AMD driver CURRENT: 25.11.1 and older have a known deep-context Vulkan crash (llama.cpp #17432); selftest.ps1 includes a canary for it."
  }
  $v = $null
  # Adrenalin versions are 'YY.M.P' (e.g. 25.11.1, 26.3.2). Take the leading dotted-number run.
  $m = [regex]::Match($Version.Trim(), '^(\d+(?:\.\d+){1,3})')
  if ($m.Success) { try { $v = [version]$m.Groups[1].Value } catch { $v = $null } }
  if ($null -eq $v) {
    return "Adrenalin version '$Version' did not parse - keep the AMD driver CURRENT: 25.11.1 and older have a known deep-context Vulkan crash (llama.cpp #17432)."
  }
  if ($v -le $knownBad) {
    return "Adrenalin $Version has the KNOWN deep-context Vulkan crash class (llama.cpp #17432, affects <= 25.11.1) - STOP and ask the human to update the driver before relying on this box. Never update a GPU driver yourself."
  }
  return $null
}

# Map RAM size to a tier. >=120 high (128GB config), >=56 mid (64GB configs
# unlock 26B via --cpu-moe), >=28 low (32GB), else min (<32GB warns already).
function Get-RamTier {
  param([int]$RamGb)
  if ($RamGb -ge 120) { return 'high' }
  if ($RamGb -ge 56)  { return 'mid' }
  if ($RamGb -ge 28)  { return 'low' }
  return 'min'
}

# Choose the arch-class profile id from (vendor, arch, vram, gpu_count, ram).
# Returns @{ profile=<id>; big_ram=<bool> }. big_ram is only meaningful for the
# dual-gpu profile (config #4 = the 128GB Optane variant) — detect cannot see the
# Optane drive, so we approximate config-4 by RAM>=120 on a dual-GPU rig.
function Get-Profile {
  param(
    [string]$Vendor,   # nvidia|amd|none
    [string]$Arch,     # blackwell|ampere|ada|volta|rdna3|gcn|other|none
    [double]$VramGb,
    [int]$GpuCount,
    [int]$RamGb
  )
  $bigRam = $false

  # Multi-GPU with at least one NVIDIA -> the 5060 Ti + V100 dual-resident rig
  # (configs 3-4). Two models resident, no swap. Checked first: a heterogeneous
  # pair outranks any single-card band.
  if ($GpuCount -ge 2 -and $Vendor -eq 'nvidia') {
    if ($RamGb -ge 120) { $bigRam = $true }   # config #4: +128GB (+Optane, unseen)
    return @{ profile = 'dual-gpu'; big_ram = $bigRam }
  }

  if ($Vendor -eq 'nvidia') {
    switch ($Arch) {
      'blackwell' {
        # Bands above 16GB (configs 13-15, spec 2026-07-16-blackwell-profile-tiers-design.md):
        # 72 covers the RTX PRO 5000 72GB AND the PRO 6000 96GB until H3 measures more.
        if ($VramGb -ge 64) { return @{ profile = 'blackwell-72'; big_ram = $false } }
        if ($VramGb -ge 40) { return @{ profile = 'blackwell-48'; big_ram = $false } }
        if ($VramGb -ge 24) { return @{ profile = 'blackwell-32'; big_ram = $false } }
        if ($VramGb -ge 12) { return @{ profile = 'blackwell-16'; big_ram = $false } }
        return @{ profile = 'blackwell-8'; big_ram = $false }
      }
      'volta' {
        return @{ profile = 'volta-16'; big_ram = $false }
      }
      default {
        # ampere / ada / other NVIDIA share the ampere-* bands (Ada ~= Ampere here).
        if ($VramGb -ge 12) { return @{ profile = 'ampere-16'; big_ram = $false } }  # defensive: 3090-class
        if ($VramGb -ge 7)  { return @{ profile = 'ampere-8';  big_ram = $false } }  # 8GB band (3070/5060 fallback)
        return @{ profile = 'ampere-6'; big_ram = $false }                            # 6GB band (3050)
      }
    }
  }

  if ($Vendor -eq 'amd') {
    if ($Arch -eq 'rdna3') {
      # J1 AMD VRAM banding: an iGPU (780M-class) reports a SMALL dedicated carve-out
      # (typically 0.5-4GB - real capacity is UMA/GTT shared memory), while a discrete
      # RDNA3 card (RX 7900-class) reports its real >=12GB dedicated VRAM. Before this
      # band a 24GB RX 7900 XTX silently got the iGPU floor profile (audit finding).
      if ($VramGb -ge 12) { return @{ profile = 'amd-rdna3-dgpu'; big_ram = $false } }
      return @{ profile = 'amd-rdna3'; big_ram = $false }
    }
    return @{ profile = 'amd-gcn'; big_ram = $false }   # gcn / vega / other AMD -> weakest Vulkan path
  }

  return @{ profile = 'cpu'; big_ram = $false }   # no usable GPU
}

# ---------------------------------------------------------------------------
# Self-test mode: feed synthetic tuples to the classifier and assert profiles.
# ---------------------------------------------------------------------------
if ($SelfTest) {
  $fail = 0
  function Assert-Arch {
    param([string]$Name, [string]$Expected)
    $got = Get-GpuArch -Name $Name
    if ($got -eq $Expected) { Write-Host "PASS arch  '$Name' -> $got" }
    else { Write-Host "FAIL arch  '$Name' -> $got (expected $Expected)"; $script:fail++ }
  }
  function Assert-Profile {
    param([string]$Label, [string]$Vendor, [string]$Arch, [double]$Vram, [int]$Count, [int]$Ram, [string]$Expected, [bool]$ExpectBigRam = $false)
    $r = Get-Profile -Vendor $Vendor -Arch $Arch -VramGb $Vram -GpuCount $Count -RamGb $Ram
    $ok = ($r.profile -eq $Expected) -and ($r.big_ram -eq $ExpectBigRam)
    if ($ok) { Write-Host "PASS prof  $Label -> $($r.profile) (big_ram=$($r.big_ram))" }
    else { Write-Host "FAIL prof  $Label -> $($r.profile) big_ram=$($r.big_ram) (expected $Expected big_ram=$ExpectBigRam)"; $script:fail++ }
  }
  function Assert-RamTier {
    param([int]$Ram, [string]$Expected)
    $got = Get-RamTier -RamGb $Ram
    if ($got -eq $Expected) { Write-Host "PASS ram   ${Ram}GB -> $got" }
    else { Write-Host "FAIL ram   ${Ram}GB -> $got (expected $Expected)"; $script:fail++ }
  }
  function Assert-UnrecognizedWarning {
    param([string]$Label, [string]$Vendor, [string]$Arch, [string]$Name, [string]$ProfileId, [bool]$ExpectWarn)
    $w = Get-UnrecognizedNvidiaWarning -Vendor $Vendor -GpuArch $Arch -GpuName $Name -ProfileId $ProfileId
    $got = [bool]$w
    if ($got -eq $ExpectWarn) { Write-Host "PASS warn  $Label -> warn=$got" }
    else { Write-Host "FAIL warn  $Label -> warn=$got (expected warn=$ExpectWarn)"; $script:fail++ }
  }

  function Assert-CudaHeader {
    param([string]$Label, [string]$SmiText, [string]$Expected)
    $got = Get-CudaFromSmiHeader -SmiText $SmiText
    if ("$got" -eq "$Expected") { Write-Host "PASS cuda  $Label -> $(if ($got) { $got } else { '(null)' })" }
    else { Write-Host "FAIL cuda  $Label -> $got (expected $Expected)"; $script:fail++ }
  }

  Write-Host '== nvidia-smi CUDA header parsing (both driver formats) =='
  Assert-CudaHeader 'classic header'      '| NVIDIA-SMI 570.86.10    Driver Version: 570.86.10    CUDA Version: 12.8     |' '12.8'
  Assert-CudaHeader 'UMD header (610.62)' '| NVIDIA-SMI 610.62                 KMD Version: 610.62        CUDA UMD Version: 13.3     |' '13.3'
  Assert-CudaHeader 'no CUDA in text'     'NVIDIA-SMI has failed' ''
  Assert-CudaHeader 'empty text'          '' ''

  Write-Host '== arch name matching =='
  Assert-Arch 'NVIDIA GeForce RTX 5060 Ti'        'blackwell'
  Assert-Arch 'NVIDIA GeForce RTX 5090'           'blackwell'
  Assert-Arch 'NVIDIA GeForce RTX 5060'           'blackwell'
  Assert-Arch 'NVIDIA GeForce RTX 4090'           'ada'
  Assert-Arch 'NVIDIA GeForce RTX 3070 Laptop GPU' 'ampere'
  Assert-Arch 'NVIDIA GeForce RTX 3050'           'ampere'
  Assert-Arch 'Tesla V100-PCIE-16GB'              'volta'
  # RTX PRO Blackwell workstation cards: "RTX PRO" breaks the RTX\s*50xx match,
  # so these need their own rules (72-96GB workstation cards classify here).
  Assert-Arch 'NVIDIA RTX PRO 4500 Blackwell'     'blackwell'
  Assert-Arch 'NVIDIA RTX PRO 5000 Blackwell'     'blackwell'
  Assert-Arch 'NVIDIA RTX PRO 6000 Blackwell Workstation Edition' 'blackwell'
  # Pre-Blackwell pro cards must NOT trip the defensive RTX-PRO rule.
  Assert-Arch 'NVIDIA RTX 6000 Ada Generation'    'other'
  Assert-Arch 'AMD Radeon 780M Graphics'          'rdna3'
  Assert-Arch 'AMD Radeon RX 7900 XTX'            'rdna3'
  Assert-Arch 'AMD Radeon Vega 7 Graphics'        'gcn'
  Assert-Arch ''                                  'none'
  Assert-Arch 'Intel(R) UHD Graphics'             'other'

  Write-Host '== profile selection (matrix configs) =='
  Assert-Profile '5060 Ti 16GB (cfg1)'  'nvidia' 'blackwell' 16 1 64  'blackwell-16'
  Assert-Profile 'V100 16GB (cfg2)'     'nvidia' 'volta'     16 1 64  'volta-16'
  Assert-Profile 'dual 5060Ti+V100 (cfg3)' 'nvidia' 'blackwell' 16 2 64  'dual-gpu'
  Assert-Profile 'dual +128GB (cfg4)'   'nvidia' 'blackwell' 16 2 128 'dual-gpu' $true
  Assert-Profile '3070 8GB (cfg5)'      'nvidia' 'ampere'    8  1 16  'ampere-8'
  Assert-Profile '3070+64GB (cfg6)'     'nvidia' 'ampere'    8  1 64  'ampere-8'
  Assert-Profile '780M+64GB (cfg7)'     'amd'    'rdna3'     0.5 1 64 'amd-rdna3'
  # J1 AMD VRAM banding: iGPU carve-out (small dedicated) stays on the UMA floor;
  # a discrete RDNA3 card (>=12GB dedicated) gets the dgpu band, not the iGPU floor.
  Assert-Profile '780M 4GB carve-out'   'amd'    'rdna3'     4   1 64 'amd-rdna3'
  Assert-Profile 'RX 7900 XTX 24GB'     'amd'    'rdna3'     24  1 64 'amd-rdna3-dgpu'
  Assert-Profile 'RX 7700 XT 12GB'      'amd'    'rdna3'     12  1 32 'amd-rdna3-dgpu'
  Assert-Profile '5060 8GB (cfg8)'      'nvidia' 'blackwell' 8  1 32  'blackwell-8'
  Assert-Profile '3050 6GB (cfg10)'     'nvidia' 'ampere'    6  1 16  'ampere-6'
  Assert-Profile '3090 24GB (defensive)' 'nvidia' 'ampere'   24 1 64  'ampere-16'
  # Blackwell bands above 16GB (configs 13-15; spec 2026-07-16-blackwell-profile-tiers-design.md)
  Assert-Profile '5090 32GB (cfg13)'    'nvidia' 'blackwell' 32 1 64  'blackwell-32'
  Assert-Profile 'PRO 4500 32GB (cfg13)' 'nvidia' 'blackwell' 32 1 128 'blackwell-32'
  Assert-Profile 'PRO 5000 48GB (cfg14)' 'nvidia' 'blackwell' 48 1 64  'blackwell-48'
  Assert-Profile 'PRO 5000 72GB (cfg15)' 'nvidia' 'blackwell' 72 1 128 'blackwell-72'
  Assert-Profile 'PRO 6000 96GB (cfg15)' 'nvidia' 'blackwell' 96 1 128 'blackwell-72'
  Assert-Profile 'Vega7+32GB (cfg12)'   'amd'    'gcn'       0.5 1 32 'amd-gcn'
  Assert-Profile 'no GPU (cpu)'         'none'   'none'      0  0 16  'cpu'

  Write-Host '== ram tiers =='
  Assert-RamTier 128 'high'
  Assert-RamTier 64  'mid'
  Assert-RamTier 56  'mid'
  Assert-RamTier 32  'low'
  Assert-RamTier 16  'min'

  Write-Host '== Adrenalin version classification (J1) =='
  function Assert-Adrenalin {
    param([string]$Label, [string]$Version, [bool]$ExpectWarn)
    $w = Get-AdrenalinWarning -Version $Version
    $got = [bool]$w
    if ($got -eq $ExpectWarn) { Write-Host "PASS adren $Label -> warn=$got" }
    else { Write-Host "FAIL adren $Label -> warn=$got (expected warn=$ExpectWarn)"; $script:fail++ }
  }
  Assert-Adrenalin 'known-bad 25.11.1 warns'   '25.11.1' $true
  Assert-Adrenalin 'older 25.5.1 warns'        '25.5.1'  $true
  Assert-Adrenalin 'newer 26.3.2 clean'        '26.3.2'  $false
  Assert-Adrenalin 'newer 25.12.1 clean'       '25.12.1' $false
  Assert-Adrenalin 'unreadable (empty) warns'  ''        $true
  Assert-Adrenalin 'garbage string warns'      'WHQL'    $true

  Write-Host '== unrecognized-NVIDIA fallback warning =='
  # A GPU that CIM reports as NVIDIA but whose name matches no arch regex ->
  # Get-GpuArch returns 'other', Get-Profile bands it into ampere-* by VRAM, and
  # the operator MUST be warned the card was unidentified.
  Assert-UnrecognizedWarning 'unknown NVIDIA (arch=other) warns' 'nvidia' 'other' 'NVIDIA GeForce RTX 6070' 'ampere-16' $true
  # A recognized NVIDIA card (arch=ampere) must NOT get this warning.
  Assert-UnrecognizedWarning 'recognized ampere does NOT warn' 'nvidia' 'ampere' 'NVIDIA GeForce RTX 3070 Laptop GPU' 'ampere-8' $false
  # AMD arch=other must NOT trip the NVIDIA-only warning (AMD warns via its own path).
  Assert-UnrecognizedWarning 'AMD arch=other does NOT warn' 'amd' 'other' 'AMD Radeon Pro Wxxxx' 'amd-gcn' $false

  if ($fail -eq 0) { Write-Host 'ALL PASS'; exit 0 }
  Write-Host "FAILURES: $fail"; exit 1
}

# ---------------------------------------------------------------------------
# Normal detection.
# ---------------------------------------------------------------------------
$ErrorActionPreference = 'Stop'
$warnings = @()

$os = if ($env:OS -eq 'Windows_NT') { 'windows' } else { 'other' }
if ($os -ne 'windows') { Write-Error 'This detector targets Windows. Use the Linux/WSL skill path instead.'; exit 1 }

# CUDA version discovery (NVIDIA only), so the installer can be FLEXIBLE about the build:
#  - driver_cuda   = the driver's max CUDA (nvidia-smi header "CUDA Version: X.Y")
#  - driver_version= the NVIDIA driver build
#  - toolkit_cuda  = the installed CUDA Toolkit (nvcc), or null if no toolkit is present
# Why it matters on Blackwell (sm_120): a CUDA-13 build SERVES (falls back from the MMQ
# kernel to cuBLAS, ~5.6x slower prefill on Q4 — functional, not peak); a CUDA-12.8 build
# hits MMQ for full speed; the old CUDA-12.4 prebuilt has no sm_120 and won't run. The
# installer keys the build choice off these, not a fixed assumption. All null when the
# tools are absent (e.g. no driver yet) — that is a valid, honest state.
function Get-CudaInfo {
  $info = [ordered]@{ driver_cuda = $null; driver_version = $null; toolkit_cuda = $null }
  try {
    $smi = (& nvidia-smi 2>$null) -join "`n"
    if ($smi) {
      $info.driver_cuda = Get-CudaFromSmiHeader -SmiText $smi
      $dv = (& nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>$null | Select-Object -First 1)
      if ($dv) { $info.driver_version = ([string]$dv).Trim() }
    }
  } catch { }
  try {
    $nv = (& nvcc --version 2>$null) -join "`n"
    if ($nv) {
      $m = [regex]::Match($nv, 'release\s*([0-9]+\.[0-9]+)')
      if ($m.Success) { $info.toolkit_cuda = $m.Groups[1].Value }
    }
  } catch { }
  return $info
}

# J1: read the AMD Adrenalin (Radeon Software) marketing version from the display-class
# registry key matching the GPU (the same key family the VRAM read below uses). WMI's
# DriverVersion is the internal build (e.g. 32.0.13031.x), NOT the Adrenalin version the
# crash-class advisories are keyed on - AMD writes that as RadeonSoftwareVersion. Returns
# the version string or $null (unreadable is a valid, honest state the classifier handles).
function Get-AdrenalinVersion {
  param([string]$GpuName)
  try {
    $keys = Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\0*' -ErrorAction SilentlyContinue |
      Where-Object { $_.PSObject.Properties['RadeonSoftwareVersion'] -and $_.RadeonSoftwareVersion }
    if (-not $keys) { return $null }
    $match = $keys | Where-Object { $GpuName -and $_.DriverDesc -eq $GpuName } | Select-Object -First 1
    if (-not $match) { $match = $keys | Select-Object -First 1 }
    return [string]$match.RadeonSoftwareVersion
  } catch { return $null }
}

$gpus = Get-CimInstance Win32_VideoController | Where-Object { $_.Name }
$gpuNames = ($gpus | ForEach-Object Name) -join '; '
$nvidia = $gpus | Where-Object { $_.Name -match 'NVIDIA' } | Select-Object -First 1
$amd    = $gpus | Where-Object { $_.Name -match 'AMD|Radeon' } | Select-Object -First 1
# Detection contract: NVIDIA present = CIM name match OR nvidia-smi resolves.
$nvidiaSmi = Get-Command nvidia-smi -ErrorAction SilentlyContinue

# gpu_count = discrete NVIDIA + AMD/Radeon adapters (Intel iGPUs are NOT usable
# offload targets and are excluded from the count). @() forces array semantics
# so .Count is reliable even for a single match.
$gpuCount = @($gpus | Where-Object { $_.Name -match 'NVIDIA|AMD|Radeon' }).Count

$vendor = 'none'; $backend = 'cpu'; $gpuName = ''
if ($nvidia) { $vendor = 'nvidia'; $backend = 'cuda';   $gpuName = $nvidia.Name }
elseif ($nvidiaSmi) {
  # nvidia-smi present but CIM naming missed the adapter — still NVIDIA/cuda per contract.
  $vendor = 'nvidia'; $backend = 'cuda'
  try { $gpuName = (& nvidia-smi --query-gpu=name --format=csv,noheader 2>$null | Select-Object -First 1).Trim() }
  catch { $gpuName = '' }
  if (-not $gpuName) { $gpuName = '' }
  # nvidia-smi found a card CIM missed -> ensure the count reflects at least one.
  if ($gpuCount -lt 1) { $gpuCount = 1 }
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

# Arch class + profile (pure functions above).
$gpuArch = Get-GpuArch -Name $gpuName
$ramTier = Get-RamTier -RamGb $ramGB
# NB: use $profileId, not $profile — $profile is a PowerShell automatic variable.
$sel       = Get-Profile -Vendor $vendor -Arch $gpuArch -VramGb $vramGB -GpuCount $gpuCount -RamGb $ramGB
$profileId = $sel.profile
$bigRam    = [bool]$sel.big_ram

# CUDA versions (NVIDIA only) + a Blackwell build hint so the installer/agent picks the
# right llama.cpp build flexibly rather than assuming one CUDA version.
$cuda = [ordered]@{ driver_cuda = $null; driver_version = $null; toolkit_cuda = $null }
if ($vendor -eq 'nvidia') { $cuda = Get-CudaInfo }
if ($gpuArch -eq 'blackwell') {
  $eff = if ($cuda.toolkit_cuda) { $cuda.toolkit_cuda } elseif ($cuda.driver_cuda) { $cuda.driver_cuda } else { $null }
  if ($eff -match '^12\.4') {
    $warnings += "Blackwell (sm_120) with CUDA ${eff}: the 12.4 prebuilt has NO sm_120 and will not run - install a CUDA-13 (serves) or CUDA-12.8 (peak) build. See SETUP-AGENT.md Blackwell note."
  } elseif ($eff -match '^13\.') {
    $warnings += "Blackwell (sm_120) with CUDA ${eff}: SERVES on a CUDA-13 build (cuBLAS fallback, ~5.6x slower prefill on Q4). For peak throughput build/install against CUDA 12.8 (MMQ). Functional now; not peak."
  } elseif ($eff -match '^12\.(8|9)' -or $eff -match '^12\.1[0-9]') {
    $warnings += "Blackwell (sm_120) with CUDA ${eff}: peak path - build with -DCMAKE_CUDA_ARCHITECTURES=120 (MMQ)."
  } else {
    $warnings += "Blackwell (sm_120): CUDA version undetected ($eff). Detect the installed CUDA before choosing the build (12.8=peak, 13.x=serves-slower, 12.4=won't run)."
  }
}

# An NVIDIA card that matched no arch regex (arch=other) fell through to the
# VRAM-banded ampere default. Make that silence audible (WARNING only — the
# fallback profile stands and the exit code is unchanged).
$unrecognizedNvidiaWarning = Get-UnrecognizedNvidiaWarning -Vendor $vendor -GpuArch $gpuArch -GpuName $gpuName -ProfileId $profileId
if ($unrecognizedNvidiaWarning) { $warnings += $unrecognizedNvidiaWarning }

if ($ramGB -lt 32) { $warnings += "RAM ${ramGB}GB < 32GB: install only the E4B workhorse tier (set OFFLOAD_WITH_FAMILY=0)" }
if ($diskGB -lt 25) { Write-Error "Only ${diskGB}GB free on ${targetDrive}: (install target drive); need >=25GB for models + binaries."; exit 1 }
$amdAdrenalin = $null
if ($vendor -eq 'amd') {
  $warnings += 'AMD path uses the llama.cpp VULKAN backend (native Windows). ROCm/HIP is NOT supported on RDNA3 iGPUs (gfx1103) and WSL2 cannot accelerate AMD iGPUs - do not attempt either.'
  # J1: the driver-age warning is now CHECKED, not generic - read the Adrenalin version
  # and classify it against the known deep-context Vulkan crash class (<= 25.11.1).
  $amdAdrenalin = Get-AdrenalinVersion -GpuName $gpuName
  $adrenWarn = Get-AdrenalinWarning -Version $amdAdrenalin
  if ($adrenWarn) { $warnings += $adrenWarn }
  else { Write-Host "Adrenalin: $amdAdrenalin (newer than the known crash class <= 25.11.1 - OK; selftest's deep-context canary still runs)" }
}

Write-Host "OS: windows | GPUs: $gpuNames"
Write-Host "Vendor: $vendor | Arch: $gpuArch | GPUs(discrete): $gpuCount | Backend: $backend | Dedicated VRAM: ${vramGB}GB | RAM: ${ramGB}GB (tier=$ramTier) | Free disk: ${diskGB}GB on ${targetDrive}: (install target)"
Write-Host "Profile: $profileId$(if ($bigRam) { ' (big_ram)' } else { '' })"
$warnings | ForEach-Object { Write-Host "WARN: $_" }

@{ os=$os; gpu_vendor=$vendor; gpu_name=$gpuName; gpu_arch=$gpuArch; gpu_count=$gpuCount;
   vram_dedicated_gb=$vramGB; ram_gb=$ramGB; ram_tier=$ramTier; profile=$profileId; big_ram=$bigRam;
   cuda_driver=$cuda.driver_cuda; cuda_driver_version=$cuda.driver_version; cuda_toolkit=$cuda.toolkit_cuda;
   amd_adrenalin=$amdAdrenalin;
   disk_free_gb=$diskGB; backend=$backend; warnings=$warnings } | ConvertTo-Json -Compress
