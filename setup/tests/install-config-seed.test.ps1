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
  # ampere-16 was RE-AUDITED 2026-09-16 (ADR 0047, register A-07): Qwen3.8-27B UD-IQ3_S + its embedded
  # MTP head beat the 4B 24 of 24 blind judgements (9.32 vs 5.39). That entry RENDERS and its weights
  # download. The BINDING is held on the 4B anyway, and the reason is measured, not cautious: live on the
  # deployed seat the 27B returned 2/8 and then 0/8 on contracts/digest-8.json, every failure "wall
  # timeout after 300s". A dispatched contract carries timeout_sec = 300 stamped by the DELEGATOR, and no
  # node-side config extends it, so a 6.3 tok/s seat cannot finish a default contract. Callers that pass
  # timeout_sec (cap 900) can name the seat today; it becomes the default when the wall is derived from
  # the seat rate (register D-03). The other 16 GB tiers keep the validated 26B agent.
  if ($tier -eq 'ampere-16') {
    Assert ($s.agent_model -eq 'qwen3.5-4b-agent')                        "$tier binds the seat that can finish a DEFAULT contract (the 27B entry renders, but see ADR 0047)"
    Assert ($s.agent_profile -eq 'research')                              "$tier seeds the research profile"
  } else {
    Assert ($s.agent_model -eq 'gemma-4-26b-agent')                       "$tier seats the validated thinking-on 26B agent (model KEY, not alias)"
  }
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
# MIMO-9B-AGENT ADOPTED (operator-approved, MEASURED 2026-09-24): mimo-9b-agent joins
# qwen3.5-9b-agent as this tier's agent_model at ctx 65536 - a coin-flip quality tie
# with a smaller VRAM footprint (see profiles.json notes for the full bake). Both
# include flags stay true; qwen3.5-9b-agent keeps rendering as the un-aliased rollback.
Assert ($sB8.agent_model -eq 'mimo-9b-agent')                              'blackwell-8 seats mimo-9b-agent (2026-09-24 bake, coin-flip tie w/ smaller footprint)'
Assert ([bool]$profiles.'blackwell-8'.include_mimo_9b)                     'blackwell-8 sets include_mimo_9b (yaml entry + download gate)'
Assert ([bool]$profiles.'blackwell-8'.include_qwen35_9b)                   'blackwell-8 KEEPS include_qwen35_9b true (qwen3.5-9b-agent stays the rollback seat)'
Assert ($profiles.'blackwell-8'.agent_ctx_tokens -eq 65536)                'blackwell-8 agent_ctx_tokens raised to 65536 (mimo-9b-agent literal --ctx-size)'
Assert ($sB8.agent_profile -eq 'research')                                 'blackwell-8 keeps agent_profile=research (the measured 0-to-72 lever)'
Assert ($null -eq $profiles.'blackwell-8'.include_qwen35_4b)               'blackwell-8 include_qwen35_4b stays absent (mimo/9B hold the shared agent-seat alias)'
# ampere-8: seat ADOPTED (operator-approved 2026-08-25; on-reference leg-2 bake 2026-08-24:
# 9B think-off 100% x2 + 5/5 x2 at 3 steps vs E4B 0% x2; fit 6111 MiB @32K on the 3070).
# Twin field-parity with blackwell-8 RESTORED on the agent seat. Same both-halves rule.
$sA8 = $profiles.'ampere-8'.config_seed
Assert ($sA8.agent_model -eq 'mimo-9b-agent')                              'ampere-8 seats mimo-9b-agent (2026-09-24 bake, coin-flip tie w/ smaller footprint)'
Assert ([bool]$profiles.'ampere-8'.include_mimo_9b)                        'ampere-8 sets include_mimo_9b (yaml entry + download gate)'
Assert ([bool]$profiles.'ampere-8'.include_qwen35_9b)                      'ampere-8 KEEPS include_qwen35_9b true (qwen3.5-9b-agent stays the rollback seat)'
Assert ($profiles.'ampere-8'.agent_ctx_tokens -eq 65536)                   'ampere-8 agent_ctx_tokens raised to 65536 (mimo-9b-agent literal --ctx-size)'
Assert ($profiles.'ampere-8'.ctx_size -eq 32768)                           'ampere-8 serves 32K (measured on-reference: E4B 3187 MiB, 9B 6111 MiB @32K)'
# amd-gcn: mimo-9b-agent ADOPTED as this tier's bound agent seat (operator-approved,
# MEASURED 2026-09-24 on <node-f>): equal quality to qwen3.5-4b-agent within the test's
# resolution at roughly HALF the wall (333 s vs 614 s). qwen3.5-4b-agent stays
# include_qwen35_4b true and renders as the un-aliased ROLLBACK seat - the SAME
# both-halves-move-together rule as blackwell-8/ampere-8, one weight class down.
$sAmdGcn = $profiles.'amd-gcn'.config_seed
Assert ($sAmdGcn.agent_model -eq 'mimo-9b-agent')                          'amd-gcn seats mimo-9b-agent (2026-09-24 bake, ~half the wall of qwen3.5-4b-agent)'
Assert ([bool]$profiles.'amd-gcn'.include_mimo_9b)                         'amd-gcn sets include_mimo_9b (yaml entry + download gate)'
Assert ([bool]$profiles.'amd-gcn'.include_qwen35_4b)                       'amd-gcn KEEPS include_qwen35_4b true (qwen3.5-4b-agent stays the rollback seat)'
Assert ($profiles.'amd-gcn'.agent_ctx_tokens -eq 32768)                    'amd-gcn agent_ctx_tokens stays 32768 (mimo-9b-agent serves the TIER window on Vulkan, not the CUDA literal 65536)'
Assert ($profiles.'amd-gcn'.agent_ctx_tokens -eq $profiles.'amd-gcn'.ctx_size) 'amd-gcn agent_ctx_tokens matches ctx_size (the mimo entry renders __CTX__ on linux-vulkan/win-vulkan)'
# The 4B and 9B seats are mutually exclusive EVERYWHERE (shared agent-seat alias;
# the Go renderer refuses both) - pin the whole table so a future tier cannot ship it.
# mimo-9b-agent shares the SAME alias as the 4B and the 9B, but pairing it with EITHER
# is no longer mutually exclusive (2026-09-24, the amd-gcn onboarding, superseding the
# 4B/mimo refusal blackwell-8/ampere-8 shipped with this seat's own introduction): the
# renderer moves the shared alias to mimo and strips it from whichever smaller entry is
# also present, so mimo may render beside the 4B, the 9B, or neither.
foreach ($t in @($profiles.PSObject.Properties.Name)) {
  Assert (-not (($profiles.$t.include_qwen35_4b -eq $true) -and ($profiles.$t.include_qwen35_9b -eq $true))) "tier ${t}: include_qwen35_4b and include_qwen35_9b are mutually exclusive"
}
$s32 = $profiles.'blackwell-32'.config_seed
Assert ($s32.videogen_width -eq 1280 -and $s32.videogen_height -eq 704)     'blackwell-32 seeds REDUCED-RES 1280x704 video (doctrine-conformant recipe)'
# 0.132.4 (wiring-debt W0.1): no tier may get its agent seat from the silent resident_tier fallback
# (TestEveryAgentSeatIsChosenNotDerived) - that fallback is how amd-gcn shipped a seat measured to FAIL.
# blackwell-32 now NAMES the seat it used to derive, so its behaviour is unchanged.
Assert ($s32.agent_model -eq 'gemma4-26b-a4b')                           'blackwell-32 names its agent seat explicitly (the value it used to derive)'
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
$merged = Merge-ConfigSeed -ConfigText $tpl -Seed $amdSeed -OffloadHome 'C:\Users\<user>\offload-stack'
$mo = $merged | ConvertFrom-Json
Assert ($mo.sdcpp_bin -eq 'C:/Users/<user>/offload-stack/sdcpp/sd-cli.exe')     'token expands in string values (forward slashes)'
Assert ($mo.sdcpp_model -eq 'C:/Users/<user>/offload-stack/models/z_image_turbo-Q8_0.gguf') 'token expands in the model path'
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
  # The 8GB twins are NO LONGER field-identical. Operator QUALITY-FIRST ruling 2026-08-24
  # (supersedes the 2026-08-20 z-image split): blackwell-8's DEFAULT image seat is
  # HiDream-O1-Image-Dev (hidream-o1-dev, mxfp8 via ComfyUI, RAM offload) — validated on-box
  # 2026-08-25 (2048^2, ~118s, editorial quality, ~7.8GB peak); Z-Image Turbo is DEMOTED to
  # the operator-selectable SPEED opt-in (sdcpp_* keys retained). ampere-8 KEEPS HiDream-O1
  # (base bf16) pending its own bake. The 2026-08-20 'resident in 8GB/speed' choice violated
  # the 2026-08-02 canonical media-quality-only rule.
  $wantFamily = if ($tier8 -eq 'blackwell-8') { 'hidream-o1-dev' } else { 'hidream-o1' }
  Assert ($cond.imagegen_family -eq $wantFamily)                            "$tier8 conditional seed binds $wantFamily (operator image-seat decision)"
  Assert ($cond.imagegen_vae -eq 'builtin')                                 "$tier8 conditional seed uses the builtin VAE (O1 is pixel-space)"
  if ($tier8 -eq 'blackwell-8') {
    # The 2026-07-23 'no video on 8GB' decision was REVERSED BY THE OPERATOR for
    # blackwell-8 on 2026-08-23 (editor-box role) and the seats were MEASURED on the
    # reference box (520s @832x480x81 I2V, peak 7751MiB — results doc section 0-R).
    Assert ($cond.videogen_family -eq 'wan22')                              'blackwell-8 conditional seed binds wan22 video (operator reversal 2026-08-23, measured)'
    # A-120 (2026-10-01; operator ruling 2026-09-24): the roster that ran on the reference box
    # supersedes the 2026-08-23 first cut named above (Q8_0 pair through DisTorch2, Q5_1 edit unet).
    # Video is the fp8_scaled pair on the NATIVE loader and the loader moves WITH the pair: native
    # refuses a .gguf expert by name, so rolling back to Q8_0 is three keys, never two. The edit unet
    # is fp8mixed (untimed on the 8GB card; every timed edit figure there is Q5_1); rolling it back is
    # one key. The tier notes carry the numbers; the Go gates are internal/tierseed/overlay_load_test.go.
    Assert ($cond.videogen_unet_high -eq 'wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors') 'blackwell-8 seeds the fp8_scaled high-noise expert (2026-09-24 study: 2,029 s native vs 2,939 s Q8_0 through DisTorch2 at 832x480x81)'
    Assert ($cond.videogen_unet_low -eq 'wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors')   'blackwell-8 seeds the fp8_scaled low-noise expert'
    Assert ($cond.videogen_wan_loader -eq 'native')                         'blackwell-8 seeds the native Wan loader (it refuses a .gguf expert, so the pair and the loader move together)'
    Assert ($cond.gen_edit_unet -eq 'qwen_image_edit_2511_fp8mixed.safetensors') 'blackwell-8 seeds the fp8mixed 2511 edit unet (operator-approved 2026-09-24)'
    $ef = $cond.gen_edit_families.'qwen-image-2.1'
    Assert ($null -ne $ef)                                                  'blackwell-8 seeds the qwen-image-2.1 edit family'
    Assert ($ef.license -eq 'Qwen Research License' -and $ef.commercial_use -is [bool] -and $ef.commercial_use -eq $false) 'qwen-image-2.1 edit family records its license pair as fields only (a real boolean, no warning text, operator order 2026-10-01)'
    Assert ($ef.gen_edit_family -eq 'qwen-image-2.1' -and $ef.gen_edit_steps -eq 40 -and $ef.gen_edit_cfg -eq 1) 'qwen-image-2.1 edit family binds the official 40 / 1.0 recipe together (a half-pair defers every edit)'
    # PowerShell's -eq coerces its right operand to the LEFT operand's type: ("40" -eq 40) and ("False" -eq $false)
    # are both True, so pin the scalar TYPES with -is or a merge that stringifies a nested family passes unnoticed.
    $mEdit = (Merge-ConfigSeed -ConfigText $tpl -Seed $cond -OffloadHome 'D:\oh') | ConvertFrom-Json
    $mf = $mEdit.gen_edit_families.'qwen-image-2.1'
    Assert ($mf -is [pscustomobject]) 'the nested edit family survives Merge-ConfigSeed as an object, not a flattened string'
    Assert ($mf.commercial_use -is [bool] -and $mf.commercial_use -eq $false -and ($mf.gen_edit_steps -is [int] -or $mf.gen_edit_steps -is [long]) -and $mf.gen_edit_steps -eq 40) 'the nested edit family keeps its scalar types through Merge-ConfigSeed (bool stays bool, 40 stays a number)'
    Assert ($cond.gen_edit_preset -eq 'lightning8')                         'blackwell-8 edit preset lightning8 (measured)'
    Assert ($cond.inpaint_ckpt -like 'RealVisXL*')                          'blackwell-8 seeds the RealVisXL inpaint ckpt'
    $musicKeys = @($cond.PSObject.Properties.Name | Where-Object { $_ -like 'musicgen_*' })
    Assert ($musicKeys.Count -eq 0)                                         'blackwell-8 conditional seed still has NO music keys'
  } else {
    $mediaKeys = @($cond.PSObject.Properties.Name | Where-Object { $_ -like 'videogen_*' -or $_ -like 'musicgen_*' })
    Assert ($mediaKeys.Count -eq 0)                                         "$tier8 conditional seed has NO video/music keys (2026-07-23 decision stands on ampere-8 pending its own bake)"
  }
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

# The mimo-9b-agent seat: same gate mechanism, same deliberate non-family asymmetry.
$mimo = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeMimo9B $true -WithFamily $true)
Assert ($mimo -contains 'model-mimo-9b')                                    'include_mimo_9b pulls model-mimo-9b'
Assert ($mimo.Count -eq 1)                                                  'include_mimo_9b pulls ONLY its own weights'
$leanMimo = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeMimo9B $true -WithFamily $false)
Assert ($leanMimo -contains 'model-mimo-9b')                                'mimo-9b survives a LEAN install (does NOT ride the family gate)'
# Both 8GB-class seats gated together pull both weights - the shape blackwell-8/ampere-8 ship.
$both9B = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeQwen359B $true -IncludeMimo9B $true -WithFamily $true)
Assert (($both9B -contains 'model-qwen35-9b') -and ($both9B -contains 'model-mimo-9b') -and ($both9B.Count -eq 2)) 'include_qwen35_9b + include_mimo_9b together pull both weights, only those'

# Every key a gate can emit must exist in $PINNED, or the install dies mid-download.
foreach ($k in @('model-qwen35-4b', 'model-qwen35-9b', 'model-mimo-9b', 'model-qwen38', 'model-qwen38-mmproj')) {
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
  if ($profiles.$t.include_mimo_9b -eq $true) {
    Assert ([bool]$PINNED['model-mimo-9b'])                                 "tier $t sets include_mimo_9b and the pin exists"
  }
}

# --- The 26B download follows the resolved include_26b (<node-e> parity audit, 2026-09-23) --
# Step 5 added 'model-26b' on the family gate alone, so blackwell-8 (include_26b false,
# moe_26b drop) downloaded 14.25 GB the rendered yaml never serves.
Write-Host ""
Write-Host "== Get-FamilyModelKeys: the 26B download honours the tier =="
Assert ([bool](Get-Command Get-FamilyModelKeys -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-FamilyModelKeys'
$fam26 = @(Get-FamilyModelKeys -WithFamily $true -Include26B $true)
Assert (($fam26 -contains 'model-e2b') -and ($fam26 -contains 'model-26b') -and $fam26.Count -eq 2) 'family + include_26b -> E2B and 26B'
$famNo26 = @(Get-FamilyModelKeys -WithFamily $true -Include26B $false)
Assert (($famNo26 -contains 'model-e2b') -and -not ($famNo26 -contains 'model-26b')) 'include_26b false -> E2B only, never the 26B'
Assert (@(Get-FamilyModelKeys -WithFamily $false -Include26B $true).Count -eq 0) 'a lean install downloads neither (the family gate still rules)'

# End to end through the pure resolver, on the tier the audit found downloading it.
$profilesPath = Join-Path (Join-Path $setupDir 'templates') 'profiles.json'
foreach ($rt in @('min', 'low', 'mid', 'high')) {
  $ppB8 = Resolve-ProfileParams -ProfileId 'blackwell-8' -RamTier $rt -BigRam $false -ProfilesJsonPath $profilesPath -Backend 'cuda'
  Assert (-not $ppB8.include_26b) "blackwell-8 ram_tier=$rt resolves include_26b false"
  Assert (-not (@(Get-FamilyModelKeys -WithFamily $true -Include26B ([bool]$ppB8.include_26b)) -contains 'model-26b')) "blackwell-8 ram_tier=$rt downloads no 26B"
}
# Every tier: the 26B is downloaded exactly when the resolved profile keeps it.
foreach ($t in @($profiles.PSObject.Properties.Name)) {
  $ppT = Resolve-ProfileParams -ProfileId $t -RamTier 'high' -BigRam $true -ProfilesJsonPath $profilesPath -Backend 'cuda'
  $dl = @(Get-FamilyModelKeys -WithFamily $true -Include26B ([bool]$ppT.include_26b)) -contains 'model-26b'
  Assert ($dl -eq [bool]$ppT.include_26b) "tier $t downloads the 26B iff include_26b ($([bool]$ppT.include_26b))"
}
# The main flow (below the seam, so not executable here) must take its family download
# set from Get-FamilyModelKeys fed the resolved $pp.include_26b, resolved before Step 5.
$installText = Get-Content -Raw (Join-Path $setupDir 'install.ps1')
Assert ($installText -match '\$modelKeys \+= @\(Get-FamilyModelKeys -WithFamily \$withFamily -Include26B \(\[bool\]\$pp\.include_26b\)\)') 'Step 5 builds the family set from Get-FamilyModelKeys + $pp.include_26b'
Assert (([regex]::Matches($installText, "'model-26b'")).Count -eq 3) "'model-26b' appears only in PINNED, Get-FamilyModelKeys and its comment (no hard-coded download)"
$ppAt = $installText.IndexOf('$pp = Resolve-ProfileParams')
$step5At = $installText.IndexOf('$modelKeys = @(')
Assert (($ppAt -gt 0) -and ($ppAt -lt $step5At)) '$pp is resolved before the Step 5 download set'

# --- Task 6: accelerator seed (ADR 0024) ----------------------------------------------
Write-Host ""
Write-Host "== accelerator seed: merged after the tier seed, __HAILO_HOME__ expanded =="
Assert ([bool](Get-Command Get-AcceleratorSeed -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-AcceleratorSeed'
$pdoc = Get-Content -Raw (Join-Path (Join-Path $setupDir 'templates') 'profiles.json') | ConvertFrom-Json
$accSeed = Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @('hailo-8l') -HailoHome 'D:\x\hailo'
Assert ($null -ne $accSeed) 'seed returned for hailo-8l'
$m2 = Merge-ConfigSeed -ConfigText $tplText -Seed $accSeed -OffloadHome 'C:\stack'
$o2 = $m2 | ConvertFrom-Json
Assert (@($o2.accelerators) -contains 'hailo-8l') 'config.accelerators lists hailo-8l'
Assert ($o2.hailo_sidecar_cmd -eq 'D:/x/hailo/hailo-http.cmd') 'hailo_sidecar_cmd expanded __HAILO_HOME__'
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

# --- Media-seat bindings: the missing tierseed.Resolve layer (field: <node-e>) ---
Write-Host ""
Write-Host "== Get-MediaSeatBindings: seats bind vision_model/stt_model on the fresh path =="
Assert ([bool](Get-Command Get-MediaSeatBindings -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-MediaSeatBindings'
$b8 = Get-MediaSeatBindings -ProfileRow $profiles.'blackwell-8'
Assert ($null -ne $b8)                                                      'blackwell-8 produces seat bindings'
Assert ($b8.vision_model -eq 'qwen3.5-9b-vl')                               'blackwell-8 binds vision_model=qwen3.5-9b-vl (measured seat 2026-08-23; the seat name, not the file)'
Assert ($b8.stt_model -eq 'whisper-stt')                                    'blackwell-8 binds stt_model=whisper-stt'
Assert ($b8.ocr_model -eq 'paddleocr-vl')                                   'blackwell-8 binds ocr_model=paddleocr-vl (the ocr seat kind, 0.88.0)'
$b8m = (Merge-ConfigSeed -ConfigText $tplText -Seed $b8) | ConvertFrom-Json
Assert ($b8m.vision_model -eq 'qwen3.5-9b-vl' -and $b8m.stt_model -eq 'whisper-stt' -and $b8m.ocr_model -eq 'paddleocr-vl') 'bindings survive the merge into the shipped config'
# Closure: EVERY tier that declares media_seats must bind EVERY kind's key — a seat the
# config never routes to is exactly the split-brain mediaseat.Bindings exists to prevent.
foreach ($t in @($profiles.PSObject.Properties.Name)) {
  $row = $profiles.$t
  if ($row.PSObject.Properties['media_seats'] -and @($row.media_seats).Count -gt 0) {
    $bt = Get-MediaSeatBindings -ProfileRow $row
    $kinds = @($row.media_seats | ForEach-Object { $_.kind })
    if ($kinds -contains 'vision') { Assert ($null -ne $bt.vision_model -and $bt.vision_model -ne '') "$t vision seat binds vision_model" }
    if ($kinds -contains 'stt')    { Assert ($null -ne $bt.stt_model -and $bt.stt_model -ne '')       "$t stt seat binds stt_model" }
    if ($kinds -contains 'ocr')    { Assert ($null -ne $bt.ocr_model -and $bt.ocr_model -ne '')       "$t ocr seat binds ocr_model" }
  }
}
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow $null))                'null profile row -> no bindings'
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ config_seed = @{} }))) 'row without media_seats -> no bindings'
$unknownKind = [pscustomobject]@{ media_seats = @([pscustomobject]@{ kind = 'aroma'; name = 'x' }) }
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow $unknownKind))         'unknown seat kind binds nothing (mirror of mediaseat.configKey)'
# A-131: a registered extra (extra = true) binds nothing, whatever its kind and wherever it
# sits in the list - mirror of mediaseat.Seat.BindingKey. Without the skip the LAST vision
# seat wins, and an extra listed after the tier's own seat silently becomes vision_model.
$primaryV = [pscustomobject]@{ kind = 'vision'; name = 'primary-vl' }
$extraV   = [pscustomobject]@{ kind = 'vision'; name = 'extra-vl'; extra = $true }
$extraV2  = [pscustomobject]@{ kind = 'vision'; name = 'extra-vl-2'; extra = $true }
$bAfter   = Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ media_seats = @($primaryV, $extraV, $extraV2) })
Assert ($bAfter.vision_model -eq 'primary-vl')                              'extras listed after the primary do not take vision_model'
$bBefore  = Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ media_seats = @($extraV, $primaryV) })
Assert ($bBefore.vision_model -eq 'primary-vl')                             'an extra listed before the primary does not take vision_model'
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ media_seats = @($extraV) }))) 'a row of only extras binds nothing (no empty object)'
$extraStt = [pscustomobject]@{ kind = 'stt'; name = 'extra-stt'; extra = $true }
$extraOcr = [pscustomobject]@{ kind = 'ocr'; name = 'extra-ocr'; extra = $true }
Assert ($null -eq (Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ media_seats = @($extraStt, $extraOcr) }))) 'stt and ocr extras bind nothing either'
$extraFalse = [pscustomobject]@{ kind = 'vision'; name = 'flag-off-vl'; extra = $false }
Assert ((Get-MediaSeatBindings -ProfileRow ([pscustomobject]@{ media_seats = @($extraFalse) })).vision_model -eq 'flag-off-vl') 'extra = false still binds (only an explicit true skips)'
# The shipped table: blackwell-8 seeds two extras, and neither is a binding.
$b8extras = @($profiles.'blackwell-8'.media_seats | Where-Object { $_.PSObject.Properties['extra'] -and $_.extra })
Assert ($b8extras.Count -eq 2)                                              'blackwell-8 ships its two vision extras'
Assert (@($b8extras | ForEach-Object { $_.name } | Where-Object { $b8.vision_model -eq $_ -or $b8.ocr_model -eq $_ -or $b8.stt_model -eq $_ }).Count -eq 0) 'no blackwell-8 extra is named by a seat binding'

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

# --- Composition lane (ADR 0059): node gate + the bind/un-bind seed rule -------------
Write-Host ""
Write-Host "== Get-NodeMajor / Get-HyperframesSeed: the compose lane seed (pure half) =="
Assert ([bool](Get-Command Get-HyperframesSeed -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-HyperframesSeed'
Assert ((Get-NodeMajor -VersionText 'v26.7.0') -eq 26)   'node v26.7.0 -> 26'
Assert ((Get-NodeMajor -VersionText 'v20.19.1') -eq 20)  'node v20 -> 20 (below the HyperFrames floor)'
Assert ((Get-NodeMajor -VersionText '') -eq 0)           'no node -> 0'
Assert ((Get-NodeMajor -VersionText 'garbage') -eq 0)    'unparsable -> 0'
Assert ($HYPERFRAMES_MIN_NODE -eq 22)                     'HyperFrames engines floor is node >= 22'
$hfOk = Get-HyperframesSeed -Installed $true -HyperframesDir 'D:\stack\hyperframes' -BrowserPath 'D:\stack\hyperframes\chrome\chs.exe'
Assert ($hfOk.compose_script -eq 'render/compose-hyperframes.mjs')    'installed -> compose_script bound to the relative runner'
Assert ($hfOk.hyperframes_dir -eq 'D:/stack/hyperframes')             'installed -> hyperframes_dir with forward slashes'
Assert ($hfOk.hyperframes_browser_path -eq 'D:/stack/hyperframes/chrome/chs.exe') 'installed -> the pinned browser path'
$hfSkip = Get-HyperframesSeed -Installed $false -HyperframesDir '' -BrowserPath ''
Assert ($hfSkip.compose_script -eq '' -and $null -eq $hfSkip.PSObject.Properties['hyperframes_dir']) 'skipped -> compose_script un-bound, nothing else written'
$hfNoBrowser = Get-HyperframesSeed -Installed $true -HyperframesDir 'D:\x' -BrowserPath ''
Assert ($hfNoBrowser.compose_script -eq '') 'installed without a browser -> un-bound (never a BOUND-BUT-MISSING route)'
$tierSeeded = Merge-ConfigSeed -ConfigText $tplText -Seed ([pscustomobject]@{ compose_script = 'render/compose-hyperframes.mjs' })
Assert (((Merge-ConfigSeed -ConfigText $tierSeeded -Seed $hfSkip) | ConvertFrom-Json).compose_script -eq '') 'the un-bind overrides the tier seed'
Assert (((Merge-ConfigSeed -ConfigText $tierSeeded -Seed $hfOk) | ConvertFrom-Json).hyperframes_dir -eq 'D:/stack/hyperframes') 'the bind merges over the tier seed'
foreach ($t in @('blackwell-3x16', 'blackwell-8', 'ampere-8', 'amd-gcn')) {
  Assert ($profiles.$t.config_seed.compose_script -eq 'render/compose-hyperframes.mjs') "$t seeds compose_script (render tree ships)"
}
Assert ($null -eq $profiles.cpu.config_seed -or $null -eq $profiles.cpu.config_seed.PSObject.Properties['compose_script']) 'cpu tier seeds no compose lane'

# --- A-132: seed placeholders expand INSIDE objects (a named image family is an object) ---
# Expand-SeedValue used to substitute __OFFLOAD_HOME__ / __EXE__ in strings and string arrays only,
# so a path token inside a family block shipped as the literal token: a config that loads fine and
# fails at render. tierseed.expand (Go) is the authoritative rule; this is its parity copy, and both
# suites load the SAME fixture (internal/tierseed/testdata/nested-expand-parity.json).
Write-Host ""
Write-Host "== A-132: nested placeholder expansion (parity with tierseed.expand) =="
function Get-SeedCanon {
  param($V)
  if ($null -eq $V) { return 'null' }
  if ($V -is [string]) { return ($V | ConvertTo-Json -Compress) }
  if ($V -is [bool]) { if ($V) { return 'true' } else { return 'false' } }
  if ($V -is [System.Array]) { return '[' + ((@($V) | ForEach-Object { Get-SeedCanon $_ }) -join ',') + ']' }
  if ($V -is [pscustomobject]) {
    $parts = @($V.PSObject.Properties.Name | Sort-Object { $_ } | ForEach-Object { ('"' + $_ + '":') + (Get-SeedCanon $V.$_) })
    return '{' + ($parts -join ',') + '}'
  }
  return [string]::Format([cultureinfo]::InvariantCulture, '{0}', $V)
}
$repoRoot = Split-Path -Parent $setupDir
$fx = Get-Content -Raw (Join-Path (Join-Path (Join-Path (Join-Path $repoRoot 'internal') 'tierseed') 'testdata') 'nested-expand-parity.json') | ConvertFrom-Json
$fxText = Merge-ConfigSeed -ConfigText '{"model":"x"}' -Seed $fx.seed -OffloadHome $fx.home
$fxObj = $fxText | ConvertFrom-Json
foreach ($name in @($fx.expected.PSObject.Properties.Name)) {
  Assert ((Get-SeedCanon $fxObj.$name) -ceq (Get-SeedCanon $fx.expected.$name)) "parity fixture: '$name' expands exactly as tierseed.expand does"
}
Assert (-not ($fxText -match '__[A-Z0-9_]+__'))                              'parity fixture: no placeholder survives anywhere, objects included'
Assert ($fxText -match '"one_element":\s*\[')                                'parity fixture: a 1-element array inside an object stays a JSON array'
Assert ($fxText -match '"empty_array":\s*\[\s*\]')                           'parity fixture: an empty array inside an object stays a JSON array'
Assert ($fxText -match '"empty_object":\s*\{\s*\}')                          'parity fixture: an empty object inside an object stays a JSON object'
$fam = $fxObj.imagegen_families.'fam-a'
Assert ($fam.commercial_use -is [bool] -and $fam.off -is [bool] -and $fam.off -eq $false) 'parity fixture: booleans inside an object stay booleans'
Assert (($fam.imagegen_steps -is [int] -or $fam.imagegen_steps -is [long]) -and $fam.imagegen_steps -eq 8) 'parity fixture: integers inside an object stay numbers'
Assert ($fam.deeper.again.n -eq 40 -and $fam.deeper.again.p -ceq 'D:/oh/dd')  'parity fixture: an object nested two deep expands'
# Arrays recurse into EVERY element (Go: expand's []any case). The shapes PowerShell is liable to flatten or
# stringify are asserted on the JSON text itself, not only on the parsed canon: a 1-element array holding an
# object or an array must come out as that, not as its bare content.
Assert ($fxText -match '"objects":\s*\[\s*\{\s*"p":\s*"D:/oh/o1"')                              'parity fixture: an object inside an array expands, an array of objects stays an array'
Assert ($fxText -match '"one_object":\s*\[\s*\{\s*"p":\s*"D:/oh/solo"\s*\}\s*\]')               'parity fixture: a 1-element array holding an object stays an array of one object'
Assert ($fxText -match '"one_nested":\s*\[\s*\[\s*"D:/oh/z"\s*\]\s*\]')                         'parity fixture: a 1-element array holding an array stays nested (not flattened)'
Assert ($fxText -match '"matrix":\s*\[\s*\[\s*"D:/oh/a",\s*"b"\s*\],\s*\[\s*"\.exe"\s*\],\s*\[\s*\],\s*\[\s*\[\s*"D:/oh/deep"\s*\]\s*\]\s*\]') 'parity fixture: an array of arrays expands element by element, the empty inner array kept'
Assert (($fxObj.imagegen_families.'fam-a'.objects[1].t -is [bool]) -and $fxObj.imagegen_families.'fam-a'.mixed.Count -eq 6) 'parity fixture: scalars inside arrayed objects keep their types, a mixed array keeps its length'
# Without -OffloadHome the home token stays (pre-J2 behaviour), __EXE__ still expands - inside objects too.
$fxNoHome = Merge-ConfigSeed -ConfigText '{"model":"x"}' -Seed $fx.seed
Assert (($fxNoHome -match '__OFFLOAD_HOME__') -and -not ($fxNoHome -match '__EXE__')) 'nested: without -OffloadHome the home token is left, __EXE__ still expands'

# The shipped consumer: blackwell-8 seeds Z-Image Turbo as a named sdcpp family with its OWN model paths.
$b8cond = $profiles.'blackwell-8'.config_seed_ram_mid_high
$b8text = Merge-ConfigSeed -ConfigText $tplText -Seed $b8cond -OffloadHome 'D:/oh'
$zf = ($b8text | ConvertFrom-Json).imagegen_families.'z-image-turbo'
Assert ($null -ne $zf)                                                       'blackwell-8 seeds the z-image-turbo family'
Assert ($zf.imagegen_engine -ceq 'sdcpp' -and $zf.sdcpp_model_kind -ceq 'diffusion') 'z-image-turbo family binds the sdcpp engine, diffusion model kind'
Assert ($zf.sdcpp_model -ceq 'D:/oh/models/z_image_turbo-Q8_0.gguf')         'z-image-turbo family carries its own diffusion model path under the install home'
Assert ($zf.sdcpp_vae -ceq 'D:/oh/models/zimage_ae.safetensors')             'z-image-turbo family carries its own VAE path'
Assert ($zf.sdcpp_llm -ceq 'D:/oh/models/Qwen3-4B-Instruct-2507-Q4_K_M.gguf') 'z-image-turbo family carries its own text-encoder path'
Assert ($zf.license -ceq 'Apache-2.0' -and $zf.commercial_use -is [bool] -and $zf.commercial_use -eq $true) 'z-image-turbo family records its license pair (Apache-2.0, commercial)'
Assert ((@($zf.sdcpp_extra_args) -join ' ') -ceq '--vae-tiling --offload-to-cpu --diffusion-fa --max-vram 6.5 --stream-layers') 'z-image-turbo family carries the measured 6.5 GB graph-cut arm'
Assert (($zf.imagegen_steps -is [int] -or $zf.imagegen_steps -is [long]) -and $zf.imagegen_steps -eq 8 -and $zf.imagegen_cfg -eq 1) 'z-image-turbo family keeps its turbo recipe (8 steps, cfg 1)'
Assert (($b8text | ConvertFrom-Json).imagegen_family -ceq 'hidream-o1-dev') 'the default image family is unchanged (z-image-turbo is a per-request opt-in)'
# The overlay gate (Go: TestEveryShippedOverlayLoadsAndValidates), on the installer's side: no seed
# layer of any tier may leave a placeholder in the config an install writes.
foreach ($tid in @($profiles.PSObject.Properties.Name)) {
  foreach ($layerName in @('config_seed', 'config_seed_ram_mid_high')) {
    $layer = $profiles.$tid.$layerName
    if ($null -eq $layer) { continue }
    $layerText = Merge-ConfigSeed -ConfigText '{"model":"x"}' -Seed $layer -OffloadHome 'D:/oh'
    $left = [regex]::Matches($layerText, '__[A-Z0-9]+(?:_[A-Z0-9]+)*__') | ForEach-Object { $_.Value } | Sort-Object -Unique
    Assert (@($left).Count -eq 0) "overlay gate: $tid $layerName leaves no placeholder in the installed config ($(@($left) -join ','))"
  }
}

if ($failures -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $failures" -ForegroundColor Red; exit 1
