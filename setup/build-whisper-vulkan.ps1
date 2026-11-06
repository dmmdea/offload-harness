# build-whisper-vulkan.ps1 - whisper-server with the Vulkan backend, built on the box that runs it.
#
# whisper.cpp publishes no Windows Vulkan binary (its whisper-bin-x64.zip is a CPU build, and the only other Windows
# zips are CUDA), so a Vulkan-only Windows box has one way to a GPU speech seat: a source build. A binary from another
# box is not an option either: GGML_NATIVE compiles for the CPU it builds on (SETUP-AGENT.md, whisper-server).
#
# The compiler comes from a portable MSYS2 under <home>\toolchain: no administrator rights, no installer, nothing
# outside the install root, removable by deleting that folder. It is the build the llama.cpp docs give for Vulkan on
# Windows. The newest whisper.cpp release at or above setup/floors.json is taken, never a pinned one. The build runs at
# below-normal priority on half the logical cores, because the box may be doing something else.
#
#   powershell -NoProfile -File setup\build-whisper-vulkan.ps1 -HomeDir D:\offload-stack
#
# Output: <home>\build\whisper.cpp\build\bin\whisper-server.exe, the path the tier's stt seat names, its runtime DLLs
# beside it, and SOURCE.txt with the tag. A re-run with that tag already built does nothing.
param(
  [Parameter(Mandatory = $true)][string]$HomeDir,
  [int]$Jobs = 0
)
$ErrorActionPreference = 'Stop'
# Started through Start-Process from PowerShell 7, Windows PowerShell inherits PowerShell 7's module paths and cannot
# load the Utility module from them, so Get-FileHash does not exist and every download fails on its hash check.
if ($PSVersionTable.PSEdition -eq 'Desktop') {
  $modulePaths = @($env:PSModulePath -split ';')
  $keptPaths = @($modulePaths | Where-Object { $_ -notlike '*\PowerShell\Modules*' -and $_ -notlike '*\PowerShell\7\Modules*' -and $_ -notlike '*\microsoft.powershell_7*' })
  if ($keptPaths.Count -ne $modulePaths.Count) {
    $env:PSModulePath = $keptPaths -join ';'
    Import-Module (Join-Path $PSHOME 'Modules\Microsoft.PowerShell.Utility\Microsoft.PowerShell.Utility.psd1') -Force
  }
}
try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }

function Log { param([string]$Text) Write-Host "[whisper-vulkan] $Text" }

# Stderr from a native tool is its log; only the exit code decides.
function Invoke-Native {
  param([string]$Exe, [string[]]$ToolArgs)
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    & $Exe @ToolArgs 2>&1 | ForEach-Object { Write-Host "      $_" }
    return $LASTEXITCODE
  } finally { $ErrorActionPreference = $prev }
}

function Get-GitHubJson {
  param([string]$Uri)
  return Invoke-RestMethod -Uri $Uri -Headers @{ 'User-Agent' = 'offload-harness-installer'; 'Accept' = 'application/vnd.github+json' } -TimeoutSec 60
}

$toolRoot = Join-Path $HomeDir 'toolchain'
$msys = Join-Path $toolRoot 'msys64'
$ucrtBin = Join-Path $msys 'ucrt64\bin'
$bash = Join-Path $msys 'usr\bin\bash.exe'
$srcDir = Join-Path $HomeDir 'build\whisper.cpp'
$binDir = Join-Path $srcDir 'build\bin'
$sourceFile = Join-Path $binDir 'SOURCE.txt'

# --- which release -------------------------------------------------------------------------------------------------
$floors = Get-Content -Raw (Join-Path $PSScriptRoot 'floors.json') | ConvertFrom-Json
$floor = [version]$floors.'whisper.cpp'.harness
$tag = $null
# Assigned first: Windows PowerShell hands a JSON array down the pipeline as ONE object, so @(call) would loop once.
$releases = Get-GitHubJson 'https://api.github.com/repos/ggml-org/whisper.cpp/releases?per_page=30'
foreach ($r in $releases) {
  if ($r.draft -or $r.prerelease) { continue }
  if ("$($r.tag_name)" -match '^v(\d+\.\d+\.\d+)$' -and [version]$Matches[1] -ge $floor) { $tag = "$($r.tag_name)"; break }
}
if (-not $tag) { throw "no whisper.cpp release at or above the floor $floor" }
if ((Test-Path -LiteralPath $sourceFile) -and (Test-Path -LiteralPath (Join-Path $binDir 'whisper-server.exe')) -and
    ((Get-Content -Raw -LiteralPath $sourceFile) -match "^ggml-org/whisper\.cpp $([regex]::Escape($tag)) ")) {
  Log "SKIP $tag is already built in $binDir"
  exit 0
}
Log "building whisper.cpp $tag"

# --- the compiler --------------------------------------------------------------------------------------------------
$needTools = @('gcc.exe', 'cmake.exe', 'ninja.exe', 'glslc.exe')
$haveTools = @($needTools | Where-Object { Test-Path -LiteralPath (Join-Path $ucrtBin $_) }).Count -eq $needTools.Count
if (-not $haveTools) {
  if (-not (Test-Path -LiteralPath $bash)) {
    New-Item -ItemType Directory -Force -Path $toolRoot | Out-Null
    $rel = Get-GitHubJson 'https://api.github.com/repos/msys2/msys2-installer/releases/latest'
    $asset = @($rel.assets | Where-Object { $_.name -eq 'msys2-base-x86_64-latest.sfx.exe' }) | Select-Object -First 1
    if (-not $asset) { throw 'the MSYS2 release has no msys2-base-x86_64-latest.sfx.exe' }
    $sfx = Join-Path $toolRoot $asset.name
    Log "fetching $($asset.name) ($([math]::Round($asset.size / 1MB)) MB)"
    $prevProgress = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'   # Windows PowerShell's progress bar slows a download by an order of magnitude
    try { Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $sfx -UseBasicParsing -TimeoutSec 1800 } finally { $ProgressPreference = $prevProgress }
    if ($asset.PSObject.Properties['digest'] -and "$($asset.digest)" -match '^sha256:([0-9a-f]{64})$') {
      $want = $Matches[1]
      $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $sfx).Hash.ToLower()
      if ($got -ne $want) { throw "$sfx hash $got is not the publisher's $want" }
    }
    $rc = Invoke-Native $sfx @('-y', "-o$toolRoot")
    if ($rc -ne 0 -or -not (Test-Path -LiteralPath $bash)) { throw "MSYS2 did not extract (exit $rc)" }
    Remove-Item -LiteralPath $sfx -Force
  }
  $env:MSYSTEM = 'UCRT64'
  $env:CHERE_INVOKING = '1'
  $env:MSYS2_PATH_TYPE = 'strict'
  # MSYS2 runs its first-time setup, the package keyring included, only when it finds no home directory: a HOME set by
  # a Git install or a parent shell skips it and every signature then fails as "keyring is not writable".
  Remove-Item Env:HOME, Env:MSYS -ErrorAction SilentlyContinue
  Log 'first run and package update (a core update ends the first pass; the second pass finishes it)'
  $null = Invoke-Native $bash @('-lc', 'true')
  # Every time, not only when the keyring is missing: a first run cut short leaves a keyring that exists but holds no
  # keys, and every later pass then fails on "key is unknown". Both commands are safe to repeat.
  $rc = Invoke-Native $bash @('-lc', 'pacman-key --init && pacman-key --populate msys2')
  if ($rc -ne 0) { throw "could not set up the MSYS2 package keyring (exit $rc)" }
  $null = Invoke-Native $bash @('-lc', 'pacman -Syuu --noconfirm --needed')
  $rc = Invoke-Native $bash @('-lc', 'pacman -Syuu --noconfirm --needed')
  if ($rc -ne 0) { throw "pacman update failed (exit $rc)" }
  $pkgs = @('gcc', 'cmake', 'ninja', 'vulkan-headers', 'vulkan-loader', 'shaderc', 'spirv-headers') | ForEach-Object { "mingw-w64-ucrt-x86_64-$_" }
  $rc = Invoke-Native $bash @('-lc', "pacman -S --noconfirm --needed $($pkgs -join ' ')")
  if ($rc -ne 0) { throw "pacman could not install the build tools (exit $rc)" }
  foreach ($t in $needTools) { if (-not (Test-Path -LiteralPath (Join-Path $ucrtBin $t))) { throw "the toolchain has no $t after its packages installed" } }
  # pacman's signature checks start a gpg-agent daemon. It would outlive this install and hold the toolchain folder.
  $null = Invoke-Native $bash @('-lc', 'gpgconf --homedir /etc/pacman.d/gnupg --kill all')
}
$env:PATH = "$ucrtBin;$env:PATH"

# --- the source ----------------------------------------------------------------------------------------------------
$git = (Get-Command git -ErrorAction Stop).Source
if (Test-Path -LiteralPath (Join-Path $srcDir '.git')) {
  $rc = Invoke-Native $git @('-C', $srcDir, 'fetch', '--depth', '1', 'origin', 'tag', $tag)
  if ($rc -ne 0) { throw "git fetch $tag failed (exit $rc)" }
  $rc = Invoke-Native $git @('-C', $srcDir, 'checkout', '--force', $tag)
  if ($rc -ne 0) { throw "git checkout $tag failed (exit $rc)" }
} else {
  if (Test-Path -LiteralPath $srcDir) {
    # A prebuilt laid down here earlier: keep it until the new build replaces it.
    $aside = "$srcDir.prev-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Move-Item -LiteralPath $srcDir -Destination $aside
    Log "moved the old $srcDir to $aside"
  }
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $srcDir) | Out-Null
  $rc = Invoke-Native $git @('clone', '--depth', '1', '--branch', $tag, 'https://github.com/ggml-org/whisper.cpp.git', $srcDir)
  if ($rc -ne 0) { throw "git clone $tag failed (exit $rc)" }
}

# --- the build -----------------------------------------------------------------------------------------------------
if ($Jobs -lt 1) { $Jobs = [math]::Max(1, [math]::Floor([Environment]::ProcessorCount / 2)) }
$self = [System.Diagnostics.Process]::GetCurrentProcess()
$self.PriorityClass = [System.Diagnostics.ProcessPriorityClass]::BelowNormal   # children inherit it
$buildDir = Join-Path $srcDir 'build'
$cmake = Join-Path $ucrtBin 'cmake.exe'
$rc = Invoke-Native $cmake @('-S', $srcDir, '-B', $buildDir, '-G', 'Ninja', '-DCMAKE_BUILD_TYPE=Release', '-DGGML_VULKAN=ON',
  '-DWHISPER_BUILD_TESTS=OFF', '-DWHISPER_BUILD_EXAMPLES=ON', '-DWHISPER_BUILD_SERVER=ON', '-DBUILD_SHARED_LIBS=OFF')
if ($rc -ne 0) { throw "cmake configure failed (exit $rc)" }
$rc = Invoke-Native $cmake @('--build', $buildDir, '--target', 'whisper-server', '-j', "$Jobs")
if ($rc -ne 0) { throw "the build failed (exit $rc)" }
$exe = Join-Path $binDir 'whisper-server.exe'
if (-not (Test-Path -LiteralPath $exe)) { throw "the build reported success but produced no $exe" }

# --- the runtime DLLs it was linked against, beside it, so the seat needs no toolchain on PATH ---------------------
$ldd = Join-Path $msys 'usr\bin\ldd.exe'
$lddOut = @(& $ldd $exe 2>$null | ForEach-Object { "$_" })
foreach ($line in $lddOut) {
  if ($line -match '=>\s+/ucrt64/bin/([^\s]+\.dll)\s') {
    Copy-Item -LiteralPath (Join-Path $ucrtBin $Matches[1]) -Destination $binDir -Force
    Log "runtime: $($Matches[1])"
  }
}
if (-not (@($lddOut) -match 'vulkan')) { throw "$exe does not link a Vulkan loader, so it would run on the CPU" }

Set-Content -LiteralPath $sourceFile -Encoding ASCII -Value "ggml-org/whisper.cpp $tag source build GGML_VULKAN=ON ($(Get-Date -Format 'yyyy-MM-dd'))"
$env:PATH = $env:PATH.Replace("$ucrtBin;", '')
Log "OK $exe ($tag)"
