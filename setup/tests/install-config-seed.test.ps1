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
# 2026-07-16 quality-first policy (spec: 2026-07-16-quality-first-generation-design.md),
# amended by the 2026-08-16 tier-doctrine pass: the 16GB tiers KEEP the proven
# HiDream-O1 bf16 + Wan Q8_0 + umt5 + 720p x 81 bindings (24.5GB krea2 does not fit
# one 16GB card — a 32GB-tier seat change is never an argument to change a 16GB seed);
# the 32GB-class tiers (blackwell-32/48/72) inherit the measured frontier seats —
# krea2 image + ltx25 video — asserted in the next block.
$profiles = (Get-Content -Raw (Join-Path (Join-Path $setupDir 'templates') 'profiles.json') | ConvertFrom-Json).profiles
foreach ($tier in @('blackwell-16','ampere-16','volta-16')) {
  $s = $profiles.$tier.config_seed
  Assert ($s.imagegen_family -eq 'hidream-o1')                              "$tier seeds imagegen_family=hidream-o1"
  Assert ($s.imagegen_ckpt -eq 'hidream_o1_image_bf16.safetensors')         "$tier seeds the bf16 Base checkpoint"
  Assert ($s.imagegen_timeout_sec -ge 3600)                                 "$tier seeds a quality-length image timeout"
  Assert ($s.videogen_unet_high -eq 'Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf')  "$tier seeds the Q8_0 high-noise expert"
  Assert ($s.videogen_unet_low -eq 'Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf')    "$tier seeds the Q8_0 low-noise expert"
  Assert ($s.videogen_text_encoder -eq 'umt5_xxl_fp16.safetensors')         "$tier seeds the fp16 text encoder"
  Assert ($s.videogen_width -eq 1280 -and $s.videogen_height -eq 720)       "$tier seeds 720p video"
  Assert ($s.videogen_frames -eq 81)                                        "$tier seeds the 81-frame native ceiling"
  Assert ($s.agent_model -eq 'gemma-4-26b-agent')                           "$tier seats the validated thinking-on 26B agent (model KEY, not alias)"
}

Write-Host "== profiles.json: 32GB-class frontier seats (tier-doctrine pass 2026-08-16) =="
foreach ($tier32 in @('blackwell-32','blackwell-48','blackwell-72')) {
  $s = $profiles.$tier32.config_seed
  Assert ($s.imagegen_family -eq 'krea2')                                   "$tier32 seeds imagegen_family=krea2"
  Assert ($s.imagegen_ckpt -eq 'krea2_turbo_bf16.safetensors')              "$tier32 seeds the Krea 2 Turbo bf16 checkpoint"
  Assert ($s.imagegen_steps -eq 8 -and $s.imagegen_cfg -eq 1)               "$tier32 seeds the 8-step turbo recipe"
  Assert ($s.videogen_family -eq 'ltx25')                                   "$tier32 seeds videogen_family=ltx25"
  Assert ($s.videogen_transformer -eq 'ltx-2.5-22b-distilled-transformer-comfy-int8-convrot.safetensors') "$tier32 seeds the LTX-2.5 int8 transformer"
  Assert ($s.videogen_frames -eq 121 -and $s.videogen_fps -eq 24)           "$tier32 seeds 121 frames @ 24fps"
  Assert ($null -eq $s.imagegen_pool_vvram_gb -and $null -eq $s.videogen_pool_vvram_gb) "$tier32 seeds NO pool keys (single card - pooling is a 2x16 mechanism)"
  Assert ($s.videogen_unet_high -eq 'Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf')  "$tier32 keeps the Wan fallback-family keys"
}
# 48/72: FULL resolution (39.11GB loaded fits single-card) + the qwen3.8 agent seat;
# 32: REDUCED resolution (the 32GB card cannot hold the bf16-upcast transformer at
# full res) and NO qwen3.8 (cannot be all-resident beside the 26B in 32GB).
foreach ($tierBig in @('blackwell-48','blackwell-72')) {
  $s = $profiles.$tierBig.config_seed
  Assert ($s.videogen_width -eq 1920 -and $s.videogen_height -eq 1088)      "$tierBig seeds FULL-RES 1920x1088 video"
  Assert ($s.agent_model -eq 'qwen3.8-27b')                                 "$tierBig seats the qwen3.8-27b agent/coder"
  Assert ([bool]$profiles.$tierBig.include_qwen38)                          "$tierBig sets include_qwen38 (yaml entry + download gate)"
}
# ampere-6: the measured 6GB agent seat (bake-off 2026-08-17). Both halves must move
# together — a seeded agent_model whose weights never download, or a download whose
# seat nothing binds, is the split-brain the gate mechanism exists to prevent.
$sA6 = $profiles.'ampere-6'.config_seed
Assert ($sA6.agent_model -eq 'qwen3.5-4b-agent')                           'ampere-6 seats the qwen3.5-4b agent (measured bake-off winner)'
Assert ([bool]$profiles.'ampere-6'.include_qwen35_4b)                      'ampere-6 sets include_qwen35_4b (yaml entry + download gate)'
# The seat is only half the measurement: the SAME model scores 0% on the default
# `general` profile and 72% narrowed, so shipping the model without the profile
# ships the configuration that was measured to fail.
Assert ($sA6.agent_profile -eq 'research')                                 'ampere-6 seeds agent_profile=research (general scored 0% on this tier)'
Assert ($null -eq $profiles.'ampere-8'.include_qwen35_4b)                  'ampere-8 include_qwen35_4b stays absent (not measured there)'
# blackwell-8: the measured 8GB agent seat (on-box quality bake 2026-08-22: 9B
# thinking-off 100% extraction x2 + 5/5 x2 vs the E4B fallback's 0% x2; 6344 MiB
# @16K / 6696 @32K on the RTX 5060 reference box). Same both-halves rule as ampere-6.
$sB8 = $profiles.'blackwell-8'.config_seed
Assert ($sB8.agent_model -eq 'qwen3.5-9b-agent')                           'blackwell-8 seats the qwen3.5-9b agent (measured on-box winner)'
Assert ([bool]$profiles.'blackwell-8'.include_qwen35_9b)                   'blackwell-8 sets include_qwen35_9b (yaml entry + download gate)'
Assert ($sB8.agent_profile -eq 'research')                                 'blackwell-8 keeps agent_profile=research (the measured 0-to-72 lever)'
Assert ($null -eq $profiles.'blackwell-8'.include_qwen35_4b)               'blackwell-8 include_qwen35_4b stays absent (9B holds the shared agent-seat alias)'
Assert ($null -eq $profiles.'ampere-8'.include_qwen35_9b)                  'ampere-8 include_qwen35_9b stays absent (own on-box bake pending - twin-parity break recorded)'
# The 4B and 9B seats are mutually exclusive EVERYWHERE (shared agent-seat alias;
# the Go renderer refuses both) - pin the whole table so a future tier cannot ship it.
foreach ($t in @($profiles.PSObject.Properties.Name)) {
  Assert (-not (($profiles.$t.include_qwen35_4b -eq $true) -and ($profiles.$t.include_qwen35_9b -eq $true))) "tier ${t}: include_qwen35_4b and include_qwen35_9b are mutually exclusive"
}
$s32 = $profiles.'blackwell-32'.config_seed
Assert ($s32.videogen_width -eq 1280 -and $s32.videogen_height -eq 704)     'blackwell-32 seeds REDUCED-RES 1280x704 video (doctrine-conformant recipe)'
Assert ($null -eq $s32.agent_model)                                         'blackwell-32 agent stays derived (no explicit agent_model)'
Assert ($null -eq $profiles.'blackwell-32'.include_qwen38)                  'blackwell-32 include_qwen38 stays absent'
Assert ($profiles.'blackwell-32'.media_seats[0].name -eq 'qwen3-vl-8b')     'blackwell-32 vision seat promoted to qwen3-vl-8b (parity with the 16GB tiers)'
# 8GB tiers: the BASE seed stays media-free (low-RAM boxes have no offload path);
# the O1 image seat now lives in the RAM-CONDITIONAL layer asserted in the J4 block below.
Assert ($null -eq $profiles.'ampere-8'.config_seed.imagegen_family)         'ampere-8 BASE seed does NOT bind the o1 family (media is RAM-conditional)'
Assert ($null -eq $profiles.'blackwell-8'.config_seed.imagegen_family)      'blackwell-8 BASE seed does NOT bind the o1 family (media is RAM-conditional)'

# --- J2: the amd-rdna3 sdcpp seed + __OFFLOAD_HOME__ token substitution ---------------
Write-Host ""
Write-Host "== J2: sdcpp seed + __OFFLOAD_HOME__ token =="
$amdSeed = $profiles.'amd-rdna3'.config_seed
Assert ($amdSeed.imagegen_engine -eq 'sdcpp')                               'amd-rdna3 seeds the sdcpp engine'
Assert ($amdSeed.sdcpp_model_kind -eq 'diffusion')                          'amd-rdna3 seeds model_kind diffusion (Z-Image DiT)'
# The tier declares the DIRECTIVE `vae_mode: cpu`; Merge-ConfigSeed (parity copy of
# tierseed.Resolve) is what turns it into the flag. Assert BOTH halves: the old
# assertion read sdcpp_extra_args straight off the raw seed, which stopped existing when
# vae_mode was introduced - and because this suite was never wired into CI, it sat red
# instead of reporting that the PowerShell side had never learned the translation.
Assert ($amdSeed.vae_mode -eq 'cpu')                                        'amd-rdna3 declares vae_mode cpu (iGPU VAE stability, sd.cpp #563/#1621)'
Assert ($null -eq $amdSeed.sdcpp_extra_args)                                'amd-rdna3 does NOT hand-write sdcpp_extra_args (vae_mode is the single writer)'
Assert ($amdSeed.imagegen_cfg -eq 1 -and $amdSeed.imagegen_steps -eq 8)     'amd-rdna3 seeds turbo sampling (cfg 1, 8 steps)'
Assert ($profiles.'amd-rdna3-dgpu'.config_seed.imagegen_engine -eq 'sdcpp') 'amd-rdna3-dgpu seeds the sdcpp engine too'
# J3: the UMA tier MUST sample Dedicated+Shared (Dedicated reads ~0 on an iGPU);
# the discrete tier keeps the plain Dedicated tree.
Assert ($amdSeed.fleet_sampler -eq 'pdh-shared')                            'amd-rdna3 seeds fleet_sampler pdh-shared (UMA footprints)'
Assert ($profiles.'amd-rdna3-dgpu'.config_seed.fleet_sampler -eq 'pdh')     'amd-rdna3-dgpu seeds fleet_sampler pdh (discrete)'
# Token substitution: string values AND strings inside array values expand; the
# default (no -OffloadHome) leaves the template text byte-identical (pre-J2 callers).
$tpl = '{"model":"offload-e4b"}'
$merged = Merge-ConfigSeed -ConfigText $tpl -Seed $amdSeed -OffloadHome 'C:\Users\ju\offload-stack'
$mo = $merged | ConvertFrom-Json
Assert ($mo.sdcpp_bin -eq 'C:/Users/ju/offload-stack/sdcpp/sd-cli.exe')     'token expands in string values (forward slashes)'
Assert ($mo.sdcpp_model -eq 'C:/Users/ju/offload-stack/models/z_image_turbo-Q8_0.gguf') 'token expands in the model path'
Assert (-not ($merged -match '__OFFLOAD_HOME__'))                           'no unexpanded token remains when -OffloadHome given'
# Regression pin (review CRITICAL): a 1-element array seed must serialize as a JSON
# ARRAY, never unroll to a bare string (which makes Go reject the whole config).
Assert ($merged -match '"sdcpp_extra_args":\s*\[')                          '1-element array seed serializes as a JSON array (no PS unroll)'
Assert (@($mo.sdcpp_extra_args) -contains '--vae-on-cpu')                   'array seed values survive the merge'
$mergedNoHomeArr = Merge-ConfigSeed -ConfigText $tpl -Seed $amdSeed
Assert ($mergedNoHomeArr -match '"sdcpp_extra_args":\s*\[')                 'array stays an array with no -OffloadHome too'

# The translation itself, end to end: vae_mode cpu MUST reach the shipped config as the
# flag, and vae_mode MUST NOT survive as a key (it is not a harness config field).
Assert (@($mo.sdcpp_extra_args) -contains '--vae-on-cpu')                   'vae_mode cpu translates to --vae-on-cpu in the merged config'
Assert ($null -eq $mo.vae_mode)                                             'vae_mode does NOT leak into the shipped config (seed-only directive)'
# __EXE__ is OS-dependent, not install-root-dependent: it must expand even with no
# -OffloadHome, or a fresh install writes a binary path that does not exist.
$mergedNoHomeObj = $mergedNoHomeArr | ConvertFrom-Json
Assert (-not ($mergedNoHomeArr -match '__EXE__'))                           'no unexpanded __EXE__ remains without -OffloadHome either'
Assert ($mergedNoHomeObj.sdcpp_bin -match 'sd-cli\.exe$')                   'sdcpp_bin ends in sd-cli.exe (token expanded)'
$mergedNoHome = Merge-ConfigSeed -ConfigText $tpl -Seed $amdSeed
Assert ($mergedNoHome -match '__OFFLOAD_HOME__')                            'without -OffloadHome the token is left as-is (pre-J2 behavior preserved)'
$arrTok = [pscustomobject]@{ sdcpp_extra_args = @('__OFFLOAD_HOME__/x', '--flag') }
$arrOut = (Merge-ConfigSeed -ConfigText $tpl -Seed $arrTok -OffloadHome 'D:\oh') | ConvertFrom-Json
Assert (@($arrOut.sdcpp_extra_args)[0] -eq 'D:/oh/x')                       'token expands inside array elements'

# --- J4: the RAM-conditional 8GB media seed layer -------------------------------------
Write-Host ""
Write-Host "== J4: config_seed_ram_mid_high (8GB tiers) =="
foreach ($tier8 in @('ampere-8', 'blackwell-8')) {
  $cond = $profiles.$tier8.config_seed_ram_mid_high
  Assert ($null -ne $cond)                                                  "$tier8 carries the RAM-conditional seed"
  # The 8GB twins are NO LONGER field-identical. The operator split them on hardware
  # grounds 2026-08-20: blackwell-8 runs Z-Image Turbo via sdcpp (an FP8-class model on
  # sm_120), while ampere-8 KEEPS HiDream-O1, which is that tier's one verified datum
  # (~5.9 min/render on the reference box). Asserting one family for both encoded the
  # old parity claim and turned a deliberate decision into a red build.
  $wantFamily = if ($tier8 -eq 'blackwell-8') { 'z-image-turbo' } else { 'hidream-o1' }
  Assert ($cond.imagegen_family -eq $wantFamily)                            "$tier8 conditional seed binds $wantFamily (operator image-seat decision)"
  Assert ($cond.imagegen_vae -eq 'builtin')                                 "$tier8 conditional seed uses the builtin VAE (O1 is pixel-space)"
  $mediaKeys = @($cond.PSObject.Properties.Name | Where-Object { $_ -like 'videogen_*' -or $_ -like 'musicgen_*' })
  Assert ($mediaKeys.Count -eq 0)                                           "$tier8 conditional seed has NO video/music keys AT ALL (8GB decision 2026-07-23)"
  # The INTENT here is "low-RAM boxes get no MEDIA binding", and that still holds. What
  # changed is that a base config_seed now exists at all: the 2026-08-19 hygiene pass (H4)
  # seeds agent_profile there, because shipping the agent seat UNSEEDED is the
  # configuration the house already measured as broken (0 -> 72 percent on the small tier).
  # So the check is narrowed to the property that actually matters rather than deleted --
  # asserting absence of the whole key would forbid a change that was deliberate.
  $baseSeed = $profiles.$tier8.config_seed
  $baseMedia = if ($null -eq $baseSeed) { @() } else {
    @($baseSeed.PSObject.Properties.Name | Where-Object { $_ -like 'imagegen_*' -or $_ -like 'videogen_*' -or $_ -like 'musicgen_*' })
  }
  Assert ($baseMedia.Count -eq 0)                                           "$tier8 BASE seed binds NO media (low-RAM boxes get no media path)"
}
# The conditional layer merges ON TOP of the template like any seed.
$condMerged = (Merge-ConfigSeed -ConfigText $tpl -Seed $profiles.'ampere-8'.config_seed_ram_mid_high -OffloadHome 'D:\oh') | ConvertFrom-Json
Assert ($condMerged.imagegen_family -eq 'hidream-o1')                       'conditional seed merges cleanly'

Write-Host ""

# --- Gate -> download set (closes the untested half of the split-brain) ---------------
# Deleting the qwen3.5-4b download line used to leave the ENTIRE suite green: the seat
# rendered into the yaml, config_seed named it, and the weights never arrived.
Write-Host ""
Write-Host "== Get-GatedModelKeys: the download half of the gate =="
Assert ([bool](Get-Command Get-GatedModelKeys -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-GatedModelKeys'

$none = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -WithFamily $true)
Assert ($none.Count -eq 0)                                                  'no gates -> no extra downloads'

$q354 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -WithFamily $true)
Assert ($q354 -contains 'model-qwen35-4b')                                  'include_qwen35_4b pulls model-qwen35-4b'
Assert ($q354.Count -eq 1)                                                  'include_qwen35_4b pulls ONLY its own weights'

# The deliberate asymmetry, pinned in BOTH directions so a future "consistency" edit fails.
$leanQ354 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -WithFamily $false)
Assert ($leanQ354 -contains 'model-qwen35-4b')                              'qwen3.5-4b survives a LEAN install (does NOT ride the family gate)'
$leanQ38 = @(Get-GatedModelKeys -IncludeQwen38 $true -IncludeQwen354B $false -WithFamily $false)
Assert ($leanQ38.Count -eq 0)                                               'qwen3.8-27b IS dropped by a lean install (rides the family gate)'
$fullQ38 = @(Get-GatedModelKeys -IncludeQwen38 $true -IncludeQwen354B $false -WithFamily $true)
Assert ($fullQ38 -contains 'model-qwen38' -and $fullQ38 -contains 'model-qwen38-mmproj') 'qwen3.8-27b pulls weights + mmproj on a full install'

# The 9B agent seat: same gate mechanism, same deliberate non-family asymmetry (it
# replaces a fallback planner MEASURED at 0% extraction, so a lean install must not
# silently drop it).
$q359 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeQwen359B $true -WithFamily $true)
Assert ($q359 -contains 'model-qwen35-9b')                                  'include_qwen35_9b pulls model-qwen35-9b'
Assert ($q359.Count -eq 1)                                                  'include_qwen35_9b pulls ONLY its own weights'
$leanQ359 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeQwen359B $true -WithFamily $false)
Assert ($leanQ359 -contains 'model-qwen35-9b')                              'qwen3.5-9b survives a LEAN install (does NOT ride the family gate)'

# Every key a gate can emit must exist in $PINNED, or the install dies mid-download.
foreach ($k in @('model-qwen35-4b', 'model-qwen35-9b', 'model-qwen38', 'model-qwen38-mmproj')) {
  Assert ([bool]$PINNED[$k])                                                "PINNED defines $k (gate cannot name a key with no pin)"
}
# Closure the other way: a tier that sets the flag must have its pin present.
foreach ($t in @($profiles.PSObject.Properties.Name)) {
  if ($profiles.$t.include_qwen35_4b -eq $true) {
    Assert ([bool]$PINNED['model-qwen35-4b'])                               "tier $t sets include_qwen35_4b and the pin exists"
  }
  if ($profiles.$t.include_qwen35_9b -eq $true) {
    Assert ([bool]$PINNED['model-qwen35-9b'])                               "tier $t sets include_qwen35_9b and the pin exists"
  }
}

# --- Task 6: accelerator seed (ADR 0024) ----------------------------------------------
Write-Host ""
Write-Host "== accelerator seed: merged after the tier seed, __HAILO_HOME__ expanded =="
Assert ([bool](Get-Command Get-AcceleratorSeed -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-AcceleratorSeed'
$pdoc = Get-Content -Raw (Join-Path (Join-Path $setupDir 'templates') 'profiles.json') | ConvertFrom-Json
$accSeed = Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @('hailo-8l') -HailoHome 'D:\Dev\Hailo'
Assert ($null -ne $accSeed) 'seed returned for hailo-8l'
$m2 = Merge-ConfigSeed -ConfigText $tplText -Seed $accSeed -OffloadHome 'C:\stack'
$o2 = $m2 | ConvertFrom-Json
Assert (@($o2.accelerators) -contains 'hailo-8l') 'config.accelerators lists hailo-8l'
Assert ($o2.hailo_sidecar_cmd -eq 'D:/Dev/Hailo/hailo-http.cmd') 'hailo_sidecar_cmd expanded __HAILO_HOME__'
Assert ($o2.hailo_endpoint -eq 'http://127.0.0.1:18813') 'hailo_endpoint seeded'
$none = Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @() -HailoHome 'D:\x'
Assert ($null -eq $none) 'no accelerators -> no seed (config byte-identical to today)'
$threw = $false
try { Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @('tpu') -HailoHome 'D:\x' | Out-Null } catch { $threw = $true }
Assert $threw 'undeclared accelerator id throws (authoring error, never silent)'
# The config's accelerators list must survive as a JSON ARRAY (1 element - the PS unroll
# would hand Go a bare string and the whole config is rejected), same pin as sdcpp_extra_args.
Assert ($m2 -match '"accelerators":\s*\[') 'config accelerators serializes as a JSON array (no PS unroll)'
# Manifest idiom pin: installed.json writes `accelerators = @($accelerators)` into an
# [ordered] hashtable serialized via ConvertTo-Json - a 1-element list must stay an ARRAY.
$mjson = [ordered]@{ big_ram = $false; accelerators = @(@('hailo-8l')) } | ConvertTo-Json -Depth 6
Assert ($mjson -match '"accelerators":\s*\[') 'manifest accelerators serializes as a JSON array (1 element, no unroll)'

# --- Media-seat bindings: the missing tierseed.Resolve layer (field: OptiPlex 7060) ---
Write-Host ""
Write-Host "== Get-MediaSeatBindings: seats bind vision_model/stt_model on the fresh path =="
Assert ([bool](Get-Command Get-MediaSeatBindings -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-MediaSeatBindings'
$b8 = Get-MediaSeatBindings -ProfileRow $profiles.'blackwell-8'
Assert ($null -ne $b8)                                                      'blackwell-8 produces seat bindings'
Assert ($b8.vision_model -eq 'qwen3vl-4b')                                  'blackwell-8 binds vision_model=qwen3vl-4b (the seat name, not the file)'
Assert ($b8.stt_model -eq 'whisper-stt')                                    'blackwell-8 binds stt_model=whisper-stt'
$b8m = (Merge-ConfigSeed -ConfigText $tplText -Seed $b8) | ConvertFrom-Json
Assert ($b8m.vision_model -eq 'qwen3vl-4b' -and $b8m.stt_model -eq 'whisper-stt') 'bindings survive the merge into the shipped config'
# Closure: EVERY tier that declares media_seats must bind BOTH keys — a seat the
# config never routes to is exactly the split-brain mediaseat.Bindings exists to prevent.
foreach ($t in @($profiles.PSObject.Properties.Name)) {
  $row = $profiles.$t
  if ($row.PSObject.Properties['media_seats'] -and @($row.media_seats).Count -gt 0) {
    $bt = Get-MediaSeatBindings -ProfileRow $row
    $kinds = @($row.media_seats | ForEach-Object { $_.kind })
    if ($kinds -contains 'vision') { Assert ($null -ne $bt.vision_model -and $bt.vision_model -ne '') "$t vision seat binds vision_model" }
    if ($kinds -contains 'stt')    { Assert ($null -ne $bt.stt_model -and $bt.stt_model -ne '')       "$t stt seat binds stt_model" }
  }
}
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow $null))                'null profile row -> no bindings'
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ config_seed = @{} }))) 'row without media_seats -> no bindings'
$unknownKind = [pscustomobject]@{ media_seats = @([pscustomobject]@{ kind = 'aroma'; name = 'x' }) }
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow $unknownKind))         'unknown seat kind binds nothing (mirror of mediaseat.configKey)'

# --- Host-tool seed: gimp_console_path / edit_python discovery rule -------------------
Write-Host ""
Write-Host "== Get-HostToolSeed: discovery rule (pure half) =="
Assert ([bool](Get-Command Get-HostToolSeed -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-HostToolSeed'
$hs = Get-HostToolSeed -GimpConsole 'C:\Program Files\GIMP 3\bin\gimp-console.exe' -PythonExe 'C:\Py\python.exe' -PythonHasPil $true -ComfyVenvPresent $false
Assert ($hs.gimp_console_path -eq 'C:/Program Files/GIMP 3/bin/gimp-console.exe') 'gimp path seeded with forward slashes'
Assert ($hs.edit_python -eq 'C:/Py/python.exe')                             'edit_python seeded when PIL present and no comfy venv'
$hsComfy = Get-HostToolSeed -GimpConsole '' -PythonExe 'C:\Py\python.exe' -PythonHasPil $true -ComfyVenvPresent $true
Assert ($null -eq $hsComfy)                                                 'comfy venv present -> edit_python NOT seeded (runtime derives it)'
$hsNoPil = Get-HostToolSeed -GimpConsole '' -PythonExe 'C:\Py\python.exe' -PythonHasPil $false -ComfyVenvPresent $false
Assert ($null -eq $hsNoPil)                                                 'python without Pillow -> edit_python NOT seeded (no lying CONFIGURED route)'
$hsGimpOnly = Get-HostToolSeed -GimpConsole 'C:\Program Files\GIMP 3\bin\gimp-console.exe' -PythonExe '' -PythonHasPil $false -ComfyVenvPresent $false
Assert ($hsGimpOnly.gimp_console_path -like '*gimp-console.exe' -and $null -eq $hsGimpOnly.PSObject.Properties['edit_python'].Value) 'gimp alone seeds only gimp_console_path'
Assert ($null -eq (Get-HostToolSeed -GimpConsole '' -PythonExe '' -PythonHasPil $false -ComfyVenvPresent $false)) 'nothing found -> no seed (config byte-identical)'
$hsMerged = (Merge-ConfigSeed -ConfigText $tplText -Seed $hs) | ConvertFrom-Json
Assert ($hsMerged.gimp_console_path -eq 'C:/Program Files/GIMP 3/bin/gimp-console.exe') 'host-tool seed merges into the shipped config'

if ($failures -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $failures" -ForegroundColor Red; exit 1
