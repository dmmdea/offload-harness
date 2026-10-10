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

# The Qwen3.6-35B-A3B RAM-spill agent seat: same gate mechanism, same non-family asymmetry (on a
# 32 GB-class ampere-6 box it IS the agent seat, so a lean install must not strand the binding).
$q36 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -IncludeQwen3635B $true -RamTier 'low' -WithFamily $true)
Assert ($q36 -contains 'model-qwen36-35b')                                  'include_qwen36_35b pulls model-qwen36-35b'
Assert (($q36 -contains 'model-qwen35-4b') -and ($q36.Count -eq 2))         'the spill seat is pulled BESIDE the 4B rollback, nothing else'
$leanQ36 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeQwen3635B $true -RamTier 'mid' -WithFamily $false)
Assert ($leanQ36 -contains 'model-qwen36-35b')                              'the spill seat survives a LEAN install (does NOT ride the family gate)'
# The RAM gate lives INSIDE Get-GatedModelKeys, not only in its caller: the flag with a min (or unknown, or
# omitted) RAM tier downloads nothing, so the 12.3 GiB file cannot outlive the render gate that drops the entry.
foreach ($r in @('min', 'none', '')) {
  $belowFloor = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -IncludeQwen3635B $true -RamTier $r -WithFamily $true)
  Assert (-not ($belowFloor -contains 'model-qwen36-35b'))                   "the spill seat flag on a '$r' RAM tier downloads no 12 GB for it"
  Assert (($belowFloor -contains 'model-qwen35-4b') -and $belowFloor.Count -eq 1) "...and the 4B rollback weights still arrive on a '$r' box"
}
$noQ36 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -WithFamily $true)
Assert (-not ($noQ36 -contains 'model-qwen36-35b'))                         'a tier/box without the spill seat downloads no 12 GB for it'

# The memory stack's second embedder (EmbeddingGemma-2 + its projector): both files or neither, no RAM
# gate (VRAM-resident), and - like the other agent/stack weights - it survives a LEAN install.
$eg2 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -IncludeEmbeddingGemma2 $true -WithFamily $true)
Assert (($eg2 -contains 'model-eg2') -and ($eg2 -contains 'model-eg2-mmproj')) 'include_embeddinggemma2 pulls the model AND its multimodal projector'
Assert (($eg2 -contains 'model-qwen35-4b') -and ($eg2.Count -eq 3))          'the stack member is pulled beside the tier''s other gated weights, nothing else'
$leanEg2 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 $true -WithFamily $false)
Assert (($leanEg2 -contains 'model-eg2') -and ($leanEg2 -contains 'model-eg2-mmproj') -and ($leanEg2.Count -eq 2)) 'the stack member survives a LEAN install (does NOT ride the family gate)'
$noEg2 = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -WithFamily $true)
Assert (-not ($noEg2 -contains 'model-eg2') -and -not ($noEg2 -contains 'model-eg2-mmproj')) 'a tier without the stack member downloads neither file'
foreach ($r in @('min', 'low', 'mid', 'high', '')) {
  $anyRam = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 $true -RamTier $r -WithFamily $true)
  Assert (($anyRam -contains 'model-eg2') -and ($anyRam -contains 'model-eg2-mmproj')) "the stack member has no RAM gate (ram_tier '$r' still pulls it)"
}
# A text-only replica (embeddinggemma2_projector false): the render drops --mmproj and the replica never
# embeds media, so the model alone is fetched - the 0.55 GB projector is for the tier that carries it.
$eg2Text = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $true -IncludeEmbeddingGemma2 $true -IncludeEmbeddingGemma2Projector $false -WithFamily $true)
Assert (($eg2Text -contains 'model-eg2') -and -not ($eg2Text -contains 'model-eg2-mmproj')) 'a text-only replica pulls the embedder model and NOT the projector'
Assert (($eg2Text -contains 'model-qwen35-4b') -and ($eg2Text.Count -eq 2))     'the text-only replica still pulls the tier''s other gated weights, nothing else'
$leanText = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 $true -IncludeEmbeddingGemma2Projector $false -WithFamily $false)
Assert (($leanText.Count -eq 1) -and ($leanText[0] -eq 'model-eg2'))              'a text-only replica survives a LEAN install as the model alone'
foreach ($r in @('min', 'low', 'mid', 'high', '')) {
  $textRam = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 $true -IncludeEmbeddingGemma2Projector $false -RamTier $r -WithFamily $true)
  Assert (($textRam -contains 'model-eg2') -and -not ($textRam -contains 'model-eg2-mmproj')) "the text-only replica has no RAM gate either (ram_tier '$r')"
}
$explicitProj = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 $true -IncludeEmbeddingGemma2Projector $true -WithFamily $true)
Assert (($explicitProj -contains 'model-eg2') -and ($explicitProj -contains 'model-eg2-mmproj') -and ($explicitProj.Count -eq 2)) 'an explicit projector true pulls both files, as the default does'
$projNoEntry = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 $false -IncludeEmbeddingGemma2Projector $true -WithFamily $true)
Assert ($projNoEntry.Count -eq 0)                                                'the projector flag alone downloads nothing: it means something only with the entry'
foreach ($k in @('model-eg2', 'model-eg2-mmproj')) {
  $pin = $PINNED[$k]
  Assert ([bool]$pin)                                                          "PINNED defines $k"
  Assert ($pin.sha -match '^[0-9a-f]{64}$')                                    "$k pins a full sha256"
  Assert ($pin.version -eq $pin.sha.Substring(0, 8))                           "$k version is the first 8 hex of its sha"
  Assert ($pin.url -match '^https://huggingface\.co/ggml-org/embeddinggemma-2-GGUF/resolve/main/') "$k is fetched from the ggml-org EmbeddingGemma-2 repo"
  Assert ($pin.url.EndsWith($pin.name))                                        "$k local name is the file the template cmd names"
}
Assert ([int64]$PINNED['model-eg2'].size -eq 309855456)                        'model-eg2 size is the measured 309,855,456 bytes'
Assert ($PINNED['model-eg2'].sha.StartsWith('2188ac1d'))                       'model-eg2 sha256 begins 2188ac1d (the memory-stack session''s prefix)'
Assert ([int64]$PINNED['model-eg2-mmproj'].size -eq 554821024)                 'model-eg2-mmproj size is the measured 554,821,024 bytes'
Assert ($PINNED['model-eg2-mmproj'].sha -eq 'c4a8a52691ecef40618438928bdf9e68379b854e24166f292592353db0aab64f') 'model-eg2-mmproj sha256 is the measured one'

# --- The pinned llama.cpp build (Lane B, 2026-10-09): the floor, the naming, and one tag everywhere --------------
Write-Host ""
Write-Host "== The pinned llama.cpp build: floor, asset naming, one tag =="
Assert ($LLAMA_TAG -match '^b\d+$')                                            'LLAMA_TAG has the bNNNN shape'
Assert ([int]$LLAMA_TAG.Substring(1) -ge 11452)                                'LLAMA_TAG is at or above b11452 (the gemma-embedding2 architecture)'
foreach ($k in @('llama-vulkan', 'llama-cuda', 'llama-cudart', 'llama-cuda13', 'llama-cudart13', 'llama-cpu')) {
  $p = $PINNED[$k]
  Assert ($p.url -like "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/*") "$k downloads from the $LLAMA_TAG release"
  Assert ($p.version -eq $LLAMA_TAG)                                           "$k manifest version is the tag (a bump re-downloads it)"
  Assert ($p.sha -match '^[0-9a-f]{64}$')                                      "$k pins a full sha256"
  Assert ([int64]$p.size -gt 1000000)                                          "$k pins a size"
}
Assert ($PINNED['llama-cuda13'].url.EndsWith("llama-$LLAMA_TAG-bin-win-cuda-13.4-x64.zip"))   'llama-cuda13 names the cuda-13.4 asset this tag ships (cuda-13.3 is gone)'
Assert ($PINNED['llama-cudart13'].url.EndsWith('cudart-llama-bin-win-cuda-13.4-x64.zip'))     'llama-cudart13 names the cuda-13.4 cudart asset'
Assert ($PINNED['llama-cuda'].url.EndsWith("llama-$LLAMA_TAG-bin-win-cuda-12.4-x64.zip"))      'llama-cuda keeps the 12.4 asset name'
Assert ($PINNED['llama-vulkan'].url.EndsWith("llama-$LLAMA_TAG-bin-win-vulkan-x64.zip"))       'llama-vulkan names the vulkan asset'
Assert ($PINNED['llama-cpu'].url.EndsWith("llama-$LLAMA_TAG-bin-win-cpu-x64.zip"))             'llama-cpu names the cpu asset'
$sixShas = @('llama-vulkan', 'llama-cuda', 'llama-cudart', 'llama-cuda13', 'llama-cudart13', 'llama-cpu') | ForEach-Object { $PINNED[$_].sha }
Assert (($sixShas | Select-Object -Unique).Count -eq 6)                       'the six llama.cpp assets pin six different hashes'

# The RAM gate Step 5 applies BEFORE it hands the flag to Get-GatedModelKeys: low, mid and high
# (28 GB and up), never min or an unknown tier. Twin of tierseed.RAMLowUp in Go.
Write-Host ""
Write-Host "== Test-RamLowUp: the RAM floor of the spill seat =="
Assert ([bool](Get-Command Test-RamLowUp -ErrorAction SilentlyContinue)) 'dot-source seam defines Test-RamLowUp'
foreach ($r in @('low', 'mid', 'high')) { Assert ((Test-RamLowUp -RamTier $r) -eq $true)  "ram_tier $r is on the spill seat's side of the floor" }
foreach ($r in @('min', '', 'none', 'huge')) { Assert ((Test-RamLowUp -RamTier $r) -eq $false) "ram_tier '$r' is below the floor (no seat, no 12 GB download)" }

# The pin: size and sha read from the Hugging Face resolve redirect (X-Linked-Size / X-Linked-ETag).
$pin36 = $PINNED['model-qwen36-35b']
Assert ([bool]$pin36)                                                       'PINNED defines model-qwen36-35b'
Assert ($pin36.name -eq 'Qwen3.6-35B-A3B/Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf') 'the pinned name carries the subdirectory the template cmd names'
Assert ($pin36.sha -match '^[0-9a-f]{64}$')                                 'the pinned sha is a full sha256'
Assert ($pin36.version -eq $pin36.sha.Substring(0, 8))                      'the pinned version is the first 8 hex of the sha, like every other model'
Assert ([int64]$pin36.size -gt 13000000000 -and [int64]$pin36.size -lt 13500000000) 'the pinned size is the 12.30 GiB GGUF'

# Every key a gate can emit must exist in $PINNED, or the install dies mid-download.
foreach ($k in @('model-qwen35-4b', 'model-qwen35-9b', 'model-mimo-9b', 'model-qwen36-35b', 'model-eg2', 'model-eg2-mmproj', 'model-qwen38', 'model-qwen38-mmproj')) {
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
  if ($profiles.$t.include_embeddinggemma2 -eq $true) {
    Assert ([bool]$PINNED['model-eg2'] -and [bool]$PINNED['model-eg2-mmproj']) "tier $t sets include_embeddinggemma2 and both pins exist"
  }
  # embeddinggemma2_projector: a JSON boolean when present (Step 5 throws on anything else), and
  # meaningful only on a tier that carries the entry.
  if ($profiles.$t.PSObject.Properties['embeddinggemma2_projector']) {
    Assert ($profiles.$t.embeddinggemma2_projector -is [bool])                 "tier $t embeddinggemma2_projector is a JSON boolean"
    Assert ($profiles.$t.include_embeddinggemma2 -eq $true)                    "tier $t sets embeddinggemma2_projector only with include_embeddinggemma2"
  }
  if ($profiles.$t.include_qwen36_35b -eq $true) {
    Assert ([bool]$PINNED['model-qwen36-35b'])                              "tier $t sets include_qwen36_35b and the pin exists"
    # The tier must declare the measured spill the write gate checks the entry's --n-cpu-moe against.
    Assert ([int]$profiles.$t.n_cpu_moe_max -ge 40)                         "tier $t sets include_qwen36_35b and declares n_cpu_moe_max >= 40"
    # ...and bind the seat only through the low-and-up overlay, never the base seed.
    Assert ($profiles.$t.config_seed.agent_model -ne 'qwen3.6-35b-a3b-agent') "tier $t does not bind the spill seat in its BASE seed (a min box cannot hold it)"
    Assert ($profiles.$t.config_seed_ram_low_up.agent_model -eq 'qwen3.6-35b-a3b-agent') "tier $t binds the spill seat in config_seed_ram_low_up"
  }
}

# The three tiers that carry the embeddinggemma2 entry, and what each downloads: the memory authority's
# card (ampere-6) carries the projector, the two replicas are text-only (their recorded footprints cannot
# hold the projector: eg2CardBudget in embeddinggemma2_stack_test.go). Each states its projector flag
# explicitly, and each seeds the memory-stack keep-set whether or not it carries the projector.
Write-Host ""
Write-Host "== embeddinggemma2 per tier: projector flag, download set, keep-set =="
foreach ($c in @(@('ampere-6', $true), @('ampere-8', $false), @('blackwell-3x16', $false))) {
  $t = $c[0]; $wantProj = [bool]$c[1]
  Assert ($profiles.$t.include_embeddinggemma2 -eq $true)                      "tier $t carries the embeddinggemma2 entry"
  Assert (($profiles.$t.embeddinggemma2_projector -is [bool]) -and ($profiles.$t.embeddinggemma2_projector -eq $wantProj)) "tier $t states embeddinggemma2_projector = $wantProj"
  $keys = @(Get-GatedModelKeys -IncludeQwen38 $false -IncludeQwen354B $false -IncludeEmbeddingGemma2 ([bool]$profiles.$t.include_embeddinggemma2) -IncludeEmbeddingGemma2Projector ([bool]$profiles.$t.embeddinggemma2_projector) -WithFamily $false)
  Assert (($keys -contains 'model-eg2') -and (($keys -contains 'model-eg2-mmproj') -eq $wantProj)) "tier $t downloads the projector iff it carries it (projector = $wantProj)"
  $ms = @(((Merge-ConfigSeed -ConfigText $tplText -Seed $profiles.$t.config_seed -OffloadHome 'D:\oh') | ConvertFrom-Json).memory_stack)
  Assert (($ms -join ',') -eq 'embeddinggemma,bge-reranker-v2-m3,embeddinggemma2') "tier $t seeds the memory-stack keep-set with embeddinggemma2 (projector = $wantProj)"
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
# The embeddinggemma2 projector flag, wired in the main flow (below the seam, so pinned by source): read from
# the profile with the same strict-boolean check as its siblings, absent meaning true, and handed to
# Get-GatedModelKeys, or a text-only tier would still download the projector (or a projector tier lose it).
Assert ($installText -match '\$includeEmbeddingGemma2Projector = \$true')                                                 'Step 5 defaults the embeddinggemma2 projector to true (absent means true)'
Assert ($installText -match 'embeddinggemma2_projector must be a JSON boolean')                                          'Step 5 refuses a non-boolean embeddinggemma2_projector, like its siblings'
Assert ($installText -match '-IncludeEmbeddingGemma2 \$includeEmbeddingGemma2 -IncludeEmbeddingGemma2Projector \$includeEmbeddingGemma2Projector -WithFamily \$withFamily') 'Step 5 hands the resolved projector flag to Get-GatedModelKeys'
# OFFLOAD_EG2_LLAMA_BIN (install render --llama-bin-eg2), wired in the main flow (below the seam, so pinned by
# source): documented in the header, checked for llama-server.exe, normalised to forward slashes, handed to the
# renderer from ONE place and only when set, and part of the Step 6 SKIP probe so an upgrade re-renders instead of
# keeping a yaml that predates the override.
Assert ($installText -match '(?m)^#\s+OFFLOAD_EG2_LLAMA_BIN \(opt-in')                                  'install.ps1 documents OFFLOAD_EG2_LLAMA_BIN in its header env list'
Assert ($installText.Contains('Test-Path -LiteralPath (Join-Path $eg2Dir ''llama-server.exe'')'))        'OFFLOAD_EG2_LLAMA_BIN is checked for llama-server.exe before anything renders'
Assert ($installText.Contains('$eg2Dir.Replace(''\'', ''/'')'))                                          'OFFLOAD_EG2_LLAMA_BIN is normalised to forward slashes (llama-swap on Windows mis-parses backslashes)'
Assert ($installText.Contains('if ($eg2Bin) { $renderArgs += @(''--llama-bin-eg2'', $eg2Bin) }'))          'install.ps1 appends --llama-bin-eg2 to the render args only when the override is set'
Assert (([regex]::Matches($installText, '--llama-bin-eg2'', \$eg2Bin')).Count -eq 1)                    'the renderer is handed --llama-bin-eg2 from exactly one place'
Assert ($installText.Contains('((-not $eg2Bin) -or (Select-String -Path $yamlDest -SimpleMatch -Pattern "$eg2Bin/llama-server.exe" -Quiet))')) 'the Step 6 SKIP probe re-renders a yaml that predates the override, matched on the executable path'
Assert (-not $installText.Contains('-Pattern $eg2Bin -Quiet'))                                           'the SKIP probe does not match the bare directory (a substring of a longer sibling directory would skip a needed re-render)'
Assert ($installText.Contains('$_ -match ''^note:'' -and ($eg2Bin -or $_ -notmatch ''so the floor was not checked'')')) 'install.ps1 relays the renderer''s note: lines (the floor note), except the one about its own pinned build'

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

# ---------------------------------------------------------------------------
# Data home (register C-92): C: holds Windows and program installs, never data. Step 8
# used to write no `home` (install.sh always did), so a fresh Windows install kept its
# cache, ledger, media and delegation log under the user profile. Get-DataHome is the
# pure rule; the Step 8 call site only runs `install volumes --json` and feeds it.
# ---------------------------------------------------------------------------
Write-Host "== Get-DataHome: the data drive comes from the install-volume rule =="
Assert ([bool](Get-Command Get-DataHome -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-DataHome'
$volOk = '{"data_target":true,"volumes":[{"root":"C:\\","is_os":true},{"root":"D:\\"}],"choice":{"volume":{"root":"D:\\","fs":"NTFS"},"because":"most free space of the non-OS volumes (900.0 GiB free of 2000.0 GiB)"}}'
$dh = Get-DataHome -VolumesJson $volOk -OsDrive 'C:'
Assert ($dh.Home -ceq 'D:/local-offload')                                  'home is a fixed directory under the chosen volume, forward slashes'
Assert ($dh.Because -match 'most free space')                             'the rule''s reason is carried through so it can be recorded'
Assert ([string]::IsNullOrEmpty($dh.Error))                                'a qualifying volume is not an error'
$dhSlash = Get-DataHome -VolumesJson '{"data_target":true,"choice":{"volume":{"root":"E:/"},"because":"x"}}' -OsDrive 'C:'
Assert ($dhSlash.Home -ceq 'E:/local-offload')                             'a root spelled with a trailing slash does not double it'

Write-Host "== Get-DataHome: an explicit OFFLOAD_DATA_HOME wins, but never onto the OS drive silently =="
$ov = Get-DataHome -Override 'E:\stack\data\' -OsDrive 'C:' -VolumesJson $volOk
Assert ($ov.Home -ceq 'E:/stack/data' -and $ov.Because -match 'OFFLOAD_DATA_HOME') 'the override is used, normalised, and the record says who chose it'
$ovOs = Get-DataHome -Override 'C:\data\offload' -OsDrive 'C:' -VolumesJson $volOk
Assert ([string]::IsNullOrEmpty($ovOs.Home) -and $ovOs.Error -match 'OS drive')    'an override on the OS drive is refused'
Assert ($ovOs.Error -match 'OFFLOAD_ALLOW_OS_DATA')                               'the refusal names the explicit way to keep data there'
$ovOsOk = Get-DataHome -Override 'c:\data\offload' -OsDrive 'C:' -VolumesJson $volOk -AllowOS $true
Assert ($ovOsOk.Home -ceq 'c:/data/offload' -and [string]::IsNullOrEmpty($ovOsOk.Error)) 'OFFLOAD_ALLOW_OS_DATA makes it a deliberate, accepted choice'
$ovUnc = Get-DataHome -Override '\\srv\share\offload' -OsDrive 'C:' -VolumesJson $volOk
Assert ([string]::IsNullOrEmpty($ovUnc.Error))                             'a UNC override is not on the OS drive'

Write-Host "== Get-DataHome: nowhere acceptable is a loud error, never a silent C: =="
$none = Get-DataHome -VolumesJson '{"volumes":[{"root":"C:\\","is_os":true}],"error":"no eligible install volume: only the OS volume C:\\ qualifies"}' -OsDrive 'C:'
Assert ([string]::IsNullOrEmpty($none.Home) -and $none.Error -match 'no eligible data volume')  'no qualifying volume yields an error and no home'
Assert ($none.Error -match 'only the OS volume')                           'the loader''s own reason is shown'
Assert ($none.Error -match 'OFFLOAD_DATA_HOME' -and $none.Error -match 'OFFLOAD_ALLOW_OS_DATA') 'both ways forward are named'
$bad = Get-DataHome -VolumesJson 'not json at all' -OsDrive 'C:'
Assert ([string]::IsNullOrEmpty($bad.Home) -and $bad.Error -match 'could not read')              'unreadable volume output is an error, not a guess'
$empty = Get-DataHome -VolumesJson '' -OsDrive 'C:'
Assert ([string]::IsNullOrEmpty($empty.Home) -and $empty.Error -match 'could not read')          'an empty probe (binary failed) is an error, not a guess'
$osChoice = Get-DataHome -VolumesJson '{"data_target":true,"choice":{"volume":{"root":"C:\\","is_os":true},"because":"OS volume, selected only because it was explicitly allowed and no other volume qualified"}}' -OsDrive 'C:' -AllowOS $true
Assert ($osChoice.Home -ceq 'C:/local-offload' -and $osChoice.Because -match 'explicitly allowed')  'with the OS volume explicitly allowed, the rule''s own choice stands and says so'

Write-Host "== Get-DataHome: the choice must come from the data-volume rule, not the plain install rule =="
# `install volumes` without --data names the volume with the most free space, and a Google
# Drive virtual drive (FAT32, not removable, a cloud quota's free space) usually has the most.
# The data-volume rule skips it and says so with data_target; a choice without that marker
# is the plain rule's answer and is refused rather than written as the node's home.
$cloudPick = Get-DataHome -VolumesJson '{"volumes":[{"root":"E:/","fs":"FAT32","label":"Google Drive"}],"choice":{"volume":{"root":"E:/","fs":"FAT32","label":"Google Drive"},"because":"most free space of the non-OS volumes (900.0 GiB free of 2000.0 GiB)"}}' -OsDrive 'C:'
Assert ([string]::IsNullOrEmpty($cloudPick.Home) -and $cloudPick.Error -match 'install volumes --data') 'a plain-rule choice (a Google Drive FAT32 volume with the most free space) is refused, never written as home'
Assert ($cloudPick.Error -match 'OFFLOAD_DATA_HOME')                       'the refusal names the explicit way forward'
$dataPick = Get-DataHome -VolumesJson '{"data_target":true,"volumes":[{"root":"E:/"},{"root":"D:/"}],"choice":{"volume":{"root":"D:/","fs":"NTFS"},"because":"most free space of the non-OS volumes (100.0 GiB free of 1000.0 GiB); passed over for data: E:/ is a cloud-synced virtual drive (label Google Drive)"}}' -OsDrive 'C:'
Assert ($dataPick.Home -ceq 'D:/local-offload' -and [string]::IsNullOrEmpty($dataPick.Error)) 'a data-rule choice is accepted'
Assert ($dataPick.Because -match 'passed over for data')                   'and its recorded reason says what the rule passed over'
$onlyCloud = Get-DataHome -VolumesJson '{"data_target":true,"volumes":[{"root":"E:/"}],"error":"no eligible install volume: not usable for data: E:/ is a cloud-synced virtual drive (label Google Drive)"}' -OsDrive 'C:'
Assert ([string]::IsNullOrEmpty($onlyCloud.Home) -and $onlyCloud.Error -match 'no eligible data volume' -and $onlyCloud.Error -match 'Google Drive') 'with only a cloud drive left the install fails loud and names it'
$ovCloud = Get-DataHome -Override 'E:\stack\data' -OsDrive 'C:' -VolumesJson '{"volumes":[]}'
Assert ($ovCloud.Home -ceq 'E:/stack/data' -and [string]::IsNullOrEmpty($ovCloud.Error)) 'an explicit OFFLOAD_DATA_HOME needs no probe at all: the operator chose the directory'

Write-Host "== Get-DataVolumeArgs: the probe always asks the data-volume rule =="
Assert ([bool](Get-Command Get-DataVolumeArgs -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-DataVolumeArgs'
$va = Get-DataVolumeArgs
Assert ((($va -join ' ')) -ceq 'install volumes --json --data')            'without the allow flag: install volumes --json --data'
$vaOs = Get-DataVolumeArgs -AllowOS $true
Assert ($vaOs -contains '--data' -and $vaOs[-1] -ceq '--allow-os-volume')  'OFFLOAD_ALLOW_OS_DATA adds the explicit allow and keeps --data'

Write-Host '== a fresh config carries home, and the template pins no data path =='
$withHome = Merge-ConfigSeed -ConfigText $tplText -Seed ([pscustomobject]@{ home = $dh.Home })
Assert ((($withHome | ConvertFrom-Json).home) -ceq 'D:/local-offload')    'Merge-ConfigSeed writes the home key'
Assert (@(($tplText | ConvertFrom-Json).PSObject.Properties | Where-Object { $_.Value -is [string] -and $_.Value -match '\.local-offload' }).Count -eq 0) `
  'the template spells no ~/.local-offload data path: a literal is an explicit value that `home` cannot rebase'
$hadHome = ($tplText | ConvertFrom-Json).PSObject.Properties['home']
Assert ($null -eq $hadHome)                                                'the template itself carries no home: it is chosen per machine, never shipped'

# Step 8 itself needs a full install to run, so its wiring is read from the source: the same
# way the Go side lints the call sites it cannot reach (a fresh config must get its home from
# the rule, fail loud when there is none, and leave the record).
Write-Host '== Step 8 wires the data home =='
$installText = Get-Content -Raw (Join-Path $setupDir 'install.ps1')
Assert ($installText -match [regex]::Escape('$dh = Get-DataHome -VolumesJson $volJson -Override $env:OFFLOAD_DATA_HOME')) 'Step 8 resolves the data home through Get-DataHome'
Assert ($installText -match [regex]::Escape('if ($dh.Error) { throw "data home: $($dh.Error)" }'))                      'Step 8 fails loud when no data home is acceptable'
Assert ($installText -match [regex]::Escape('-Seed ([pscustomobject]@{ home = $dh.Home })'))                           'Step 8 merges the chosen home into the fresh config'
Assert ($installText -match [regex]::Escape('$volArgs = Get-DataVolumeArgs -AllowOS $allowOsData')) 'Step 8 builds the probe through Get-DataVolumeArgs, so --data cannot be dropped at the call site'
Assert ($installText -match [regex]::Escape("if (`$AllowOS) { `$a += '--allow-os-volume' }")) 'OFFLOAD_ALLOW_OS_DATA (and only it) reaches the volume rule as its explicit allow'
Assert ($installText -match [regex]::Escape("`$allowOsData = (`$env:OFFLOAD_ALLOW_OS_DATA -eq '1')")) 'the allow is the explicit env value, never a default'
Assert ($installText -match [regex]::Escape("`$manifest['data_home'] = `$script:dataHomeChoice.Home"))                   'the choice is recorded in installed.json'

if ($failures -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $failures" -ForegroundColor Red; exit 1
