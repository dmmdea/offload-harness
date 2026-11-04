### Fixed - an AMD iGPU beside a discrete NVIDIA card no longer makes the box `dual-gpu`

- **What it does.** `setup/detect.ps1` counts offload targets, not adapters: an AMD iGPU (APU graphics,
  `Test-AmdIgpuName`) next to a discrete card is left out of `gpu_count` and of the per-adapter arch
  list, and counts as the one GPU only when it is alone (an APU box). Intel iGPUs never counted and
  still do not. The rule is `Get-OffloadGpuNames`, self-tested with the hybrid shapes (RTX 5080 +
  Ryzen iGPU, laptop 3070 + 780M, 780M alone, 2x and 3x Blackwell with an Intel iGPU, NVIDIA + a
  discrete RX 7900).
- **Why.** Every AM5 Ryzen desktop exposes `AMD Radeon(TM) Graphics` beside its NVIDIA card. On an
  RTX 5080 + Ryzen 9 9950X3D box the detector reported `gpu_count: 2` and the profile `dual-gpu`, the
  heterogeneous sm_70 + sm_120 tier the CUDA installer refuses, instead of `blackwell-16`. The Go
  twin (`hwdetect`) takes nvidia-smi's row count and was right; the PowerShell path counted every
  AMD/Radeon adapter.
- **Check.** `powershell -File setup\detect.ps1` on that box, after the fix: `profile: blackwell-16`,
  `gpu_count: 1`, `gpu_archs: ["blackwell"]`; `setup\detect.ps1 -SelfTest` passes the new asserts.
