# Checked-in capability reports

Real output from real machines, produced by `local-offload report` and committed verbatim except
for identity scrubbing: hostnames, usernames, and machine-specific paths are replaced with
placeholders (`<node-a>`, `C:\Users\<user>`, `<repo-root>`) per the repo privacy policy
([STYLE.md](../../STYLE.md)). They are **examples to compare against**, never a claim about your
hardware — hardware, drivers and bindings differ per box.

| file | machine | tier | notes |
|---|---|---|---|
| [blackwell-16-node-b.md](blackwell-16-node-b.md) | `<node-b>` workstation (RTX 5060 Ti 16 GB, Win 11) — **HISTORICAL, 2026-07-27** | `blackwell-16` | full ComfyUI stack; 11/11 media routes CONFIGURED. ⚠️ this box gained a second card (RTX 5070 Ti 16 GB) on **2026-08-02** and a THIRD (RTX 5060 Ti 16 GB) on **2026-08-31**, so it is now **3× Blackwell 16 GB = 48.9 GB**; `blackwell-2x16` is only what its 2-card seats run as. `blackwell-3x16` covers that rig and is SHIPPED — the tier id since 0.113.32, and a COMPOSITE tier (ADR 0052) declaring `single` / `pair` / `display` layers since 0.123.2. This report predates that and is kept as a point-in-time record — do NOT read it as the box's current hardware. |
| [ampere-8-node-a.md](ampere-8-node-a.md) | `<node-a>` laptop (RTX 3070 8 GB, Win 11) | `ampere-8` | after binding `voicegen_script`; render tree co-located with the binary |
| [ampere-6-node-c.md](ampere-6-node-c.md) | `<node-c>` mini-PC (RTX 3050 6 GB, Ubuntu) — **HISTORICAL, 2026-07-27** | `ampere-6` | Linux node; `sdcpp` image engine, ComfyUI bound. ⚠️ this box's RTX 3050 was replaced by an **NVIDIA A2 16 GB** on **2026-09-04**, so it now runs **`ampere-16`**. This report predates that — do NOT read it as the box's current hardware. |
| [blackwell-16-node-h.md](blackwell-16-node-h.md) | `<node-h>` desktop (RTX 5080 16 GB beside a Ryzen iGPU, Win 11, 128 GB RAM), 2026-11-04 | `blackwell-16` | the first box installed end to end by `install.ps1` (0.207.1, Windows PowerShell 5.1) into the default home; the card drives the owner's displays and is shared with the owner's own Ollama and ComfyUI, so every seat is lazy (`ttl 300`). Text, vision and STT seats live; `ocr`/`stt_hq` unset by tier design; `voice` BOUND-BUT-MISSING (no TTS venv) and `comfyui` BOUND-BUT-MISSING (the owner's ComfyUI is the portable package, whose tree sits under `<comfy_dir>/ComfyUI` with `python_embeded` instead of a `.venv`). |

The first three show `hardware tier: UNKNOWN`: none of them was installed by `install.ps1` into the
default `$OFFLOAD_HOME`, which is the only place the manifest is looked for — a box installed the
normal way names its tier, as `<node-h>` does. (Its first report also read `UNKNOWN`: Windows
PowerShell 5.1 had written the manifest with a BOM the Go reader rejected; fixed with that report.)

To add yours: run `local-offload report --out <tier>-<node-label>.md` and drop it here — scrub your
hostname/username first (use a generic node label, not the machine name). The filename prefix is how
[the generator](../../../cmd/gentiers) attaches it to the right tier page.
