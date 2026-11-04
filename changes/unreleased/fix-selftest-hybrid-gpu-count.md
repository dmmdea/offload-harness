### Fixed - the selftest's dual-GPU gate counts offload GPUs by the detect.ps1 rule

- **What it does.** `setup/selftest.ps1` derives the `gpu_count` of its dual-GPU gate the way
  `setup/detect.ps1` does since 0.207.1: NVIDIA and AMD adapters, minus an AMD iGPU beside a discrete
  card; the iGPU counts only when it is the box's only GPU.
- **Why.** The selftest counted every NVIDIA/AMD/Radeon adapter, so a Ryzen desktop's
  `AMD Radeon(TM) Graphics` next to an RTX 5080 reported `gpu_count=2` in the receipt (2026-11-04).
  The verdict `not-applicable-on-this-host` was right only because the profile is not dual-resident;
  the count was wrong.
- **Check.** The rule, run live on the three-card reference workstation (three discrete Blackwell
  cards, Intel iGPU): `count=3`.
