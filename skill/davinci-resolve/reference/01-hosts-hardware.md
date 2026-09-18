# 01 — Hosts, hardware, paths, licensing

Check `hostname` first. Facts below were re-verified live 2026-09-01 unless dated otherwise.

## Dell editing rig 7060 the editing rig — THE editing rig (PRIMARY, all MyTools work happens here)

| Item | Value [measured 2026-09-01] |
|---|---|
| CPU | Intel Core i7-9700T, 8C/8T @ 2.0 GHz (the microcode-mod CPU swap happened) |
| RAM | 64 GB (68,505,698,304 bytes) |
| GPU | **NVIDIA GeForce RTX 5060, 8151 MiB VRAM**, driver 616.56 (CUDA 13.4); Gen3 x4 PCH slot (load-path caveat) |
| iGPU | Intel UHD 630 (Resolve uses it as an extra decoder: "Intel QuickSync decodes H264 up to level 62") |
| OS | Windows 11 Pro 10.0.26200 |
| Drives | C: "Optane - OS" 109 GB (36 free) · **D: "T-Force SSD" 954 GB (525 free) = editing root** · E: "Exos HDD" 3.7 TB (1.26 TB free) = archive · F: "Adata - Cache" 104 GB (96 free) = `F:\ResolveCache` + ComfyUI temp + 8 GB fixed pagefile |
| Tailscale | the editing rig (<tailnet-ip>); SSH as `<user>` (Windows PowerShell 5.1, arrives ELEVATED) |
| Console owner | local account **<editor>** (the editor). Resolve must run in HER session. `<user>` is the operator's admin/SSH account |
| Resolve | Studio **21.1.0.14 since 2026-09-10** (silent in-place upgrade from 21.0.4.5 over SSH; scripting + licence under 21.1 NOT yet verified on this box — see `10-sidecar-and-cli.md` §5), `C:\Program Files\Blackmagic Design\DaVinci Resolve\Resolve.exe`; compute API auto → **CUDA** on the RTX 5060; memory config reserved=12249M pinned=8000M (from the 21.0.4 log) |
| License | Activated on the editor's account 2026-08-31 ("1 activation left" on that key). Proof line: `LeManager \| License Key:` in `<editor-profile>\AppData\Roaming\Blackmagic Design\DaVinci Resolve\Support\logs\davinci_resolve.log` |
| Scripting pref | Preferences → System → General → External scripting using = **Local** (set in the editor's profile) |
| Bridge CLI | `D:\Editing\ResolveTools\resolve.cmd` — deploy artifact of `<dev>\video-pipeline\engine\resolve_bridge` (repo lives on the workstation). Rig build since 2026-09-10: `c099f222a` (the 21.1 sidecar bridge; was `f759aaa29`). Interpreter: bundled `python312\` CPython 3.12.10 embeddable — measured on 21.0.4 only; re-probe under 21.1 |
| Editing tree | `D:\Editing\{Assets{Brand,Fonts,Graphics,LUTs,Music,SFX}, Exports{.gallery,CacheClip,Resolve Project Backups,rx8}, Footage{rx8}, Projects{rx8-shorts}, ResolveCache, ResolveDB, ResolveTools}` plus scratch dirs `_pp_smoke`, `_pp_prove`, `_pp_burn`, `_pp_burnsweep`, `_pp_ceiling` (2026-08-31 render/GPU-tune test outputs, ~20 GB; delete only when the operator says so) |
| LUTs | `C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\LUT\MyTools\Cinematic 10.cube` (relative LUT path for `SetLUT`: `MyTools/Cinematic 10.cube`) |
| Fonts installed machine-wide | League Gothic (+Condensed/SemiCondensed), Montserrat, Anton (brand titles/captions) |
| AI stack | local-offload harness (`D:\offload-harness\`, tier blackwell-8), llama-swap `127.0.0.1:11436` (boot task, idle-by-design), ComfyUI `D:\ComfyUI`, Hailo-8L NPU sidecar `127.0.0.1:18813`, ffmpeg on machine PATH, Insta360 Studio 5.9.0 |
| Archive footage | `E:\MyTools Auto Reviews\…` (working copies go to `D:\Editing\Footage\<project>`) |

Hardware-decode/encode facts from Resolve's own log on this GPU [measured 2026-08-31]:
- NVDEC decodes H.264 4:2:0/4:2:2 8/10-bit, HEVC 4:2:0/4:2:2/4:4:4 8/10/12-bit, VP9 8/10-bit, AV1 8/10-bit, all up to 8192×8192.
- NVENC encodes H.264 and HEVC at 4:2:0/4:2:2/4:4:4 up to 10-bit, AV1 4:2:0 up to 10-bit.
  (So the "NVIDIA" codec ids — `H264_NVIDIA`, `H265_NVIDIA`, `AV1YUV420_8_NVIDIA`, `AV1YUV420_10_NVIDIA` — are live here.)

Rules for this box: the operator reviews outputs ON the rig; Google Drive is out of bounds for MyTools
outputs (never copy to `<cloud-drive>\YouTube\…`). Generative assets come from the workstation, but every
live-Resolve operation, render, and vision QA is pinned to the editing rig.

### 8 GB VRAM operating envelope [community + inferred]
- 1080×1920 / 1920×1080 timelines with H.264/HEVC sources are comfortable; 4K timelines with
  noise reduction, Magic Mask, Super Scale or Fusion-heavy titles will hit "GPU memory full".
- Mitigations in order: lower the **timeline** resolution while editing (render can still be
  full-res via `FormatWidth/FormatHeight`), disable temporal/spatial NR nodes, trim Magic Mask
  ranges, use `SetColorOutputCache` / render cache on heavy clips (cache lives on `F:\ResolveCache`),
  set Preferences → System → Memory and GPU → GPU configuration to CUDA + the RTX 5060 explicitly
  (it already auto-selects CUDA).
- Do not run ComfyUI / llama-swap models concurrently with a render on this GPU; harness seats
  are idle-by-design (ttl 300) but a warm model steals VRAM.

## workstation (workstation) — Studio seat #2, my own local Resolve

| Item | Value |
|---|---|
| GPUs | 3×16 GB: RTX 5070 Ti + 2× RTX 5060 Ti (48 GB) — shared with ComfyUI/llama-swap; coordinate before heavy renders |
| RAM | 128 GB |
| Resolve | **Studio 21.1.0.14 since 2026-09-08** (was 21.0.4.5, activated locally 2026-08-27/28; the seat survived the upgrade, log `License Key: Activated successfully. No activations left`); **Fusion 21.1**; disk DB "Local Database" holding `_ref_scratch` (disposable; 2026-09-09 it carries the `_ref_bin` test media + `_ref_tl`); Media Storage volumes: `E:\Davinci Resolve Videos`, C: D: E: F: G: H: P: U: V: W: X: Y: Z: |
| Bridge | same CLI at `D:\Editing\ResolveTools\resolve.cmd` (deployed build 2296080 on 2026-08-28, pin `C:\Program Files\Python314\python.exe`); the 21.1 catalog + sidecar live on the `feat/resolve-http-sidecar` branch until PR #5 merges. On 21.1 **3.14.7, the bundled `ResolvePython.exe` 3.14.4 and uv 3.11.15 all bind** (09 §2) |
| Native MCP | `C:\Program Files\Blackmagic Design\DaVinci Resolve\{DaVinciResolve.mcpb, ResolveMCP.exe, ResolvePython\}` — stdio server for Claude Desktop, 14 tools (09 §3) |
| Installer | `D:\Temp\DaVinci_Resolve_Studio_21.0.4_Windows.zip` (21.1 installer not kept) |
| Crash dumps | `%APPDATA%\Blackmagic Design\DaVinci Resolve\Support\logs\Resolve.exe.<pid>.dmp` — a 775 MB one from the scripted `ArchiveProject` crash of 2026-09-09 (delete when the operator says so) |
| Use | offline authoring (edit-specs, ASS captions, Fusion `.setting` templates, generative assets), measurement, API dumps. Not the editor's machine — mutating test projects here is fine as long as they are named `_ref_*`/`_pp_*` and deleted |

The live dump `live-dump-2026-09-01.json` was taken here; it is the same Resolve build as
the Dell, so settings keys, render formats and codec ids transfer. NVIDIA codec entries also
appear on the Dell because both are NVIDIA boxes.

## laptop 15P the laptop — DORMANT for Resolve
RTX 3070 8 GB, 64 GB. Resolve 21.0.4.5 installed but NO seat since 2026-08-28. The samuelgursky
`davinci-resolve-mcp` v2.103.1 lives at `%USERPROFILE%\resolve-claude\` (deps pinned
`mcp[cli]>=1.29,<2`). Only its vendored docs matter now (copied into the pipeline repo at
`docs/reference/kernels/`).

## Two Studio keys, one rule
Key A moved laptop→workstation; Key B is the editing rig's own. Per-Windows-account activation, 2
activations per key. To move a seat: DEACTIVATE on the source machine first (DaVinci Resolve →
Deactivate), then activate on the target. Never let a box sit prompting for a key — read the
`LeManager | License Key:` log line before theorising.

## Where Resolve keeps things (Windows) [doc + measured]
- Scripting API: `C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\Developer\Scripting\` (21.1: `README.md`, `CHANGELOG.md`, **`DaVinciResolveScript.pyi`**, `Modules\DaVinciResolveScript.py`, `Examples\`; 21.0.4 had `README.txt`/`CHANGELOG.txt`)
- Built-in interpreter (21.1): `C:\Program Files\Blackmagic Design\DaVinci Resolve\ResolvePython\ResolvePython.exe` (3.14.4; `import DaVinciResolveScript` works with no env vars)
- Library: `C:\Program Files\Blackmagic Design\DaVinci Resolve\fusionscript.dll`; `fuscript.exe` alongside
- Per-user support: `%APPDATA%\Blackmagic Design\DaVinci Resolve\Support\` → `logs\davinci_resolve.log`, `Resolve Project Library\` (per-user disk DB), `Fusion\` (user scripts/templates), `.LUT`, `Fairlight`
- Machine LUTs: `C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\LUT\<vendor>\…`
- Scripts menu folders: all users `%PROGRAMDATA%\Blackmagic Design\DaVinci Resolve\Fusion\Scripts\{Utility,Comp,Tool,Edit,Color,Deliver}`; per user `%APPDATA%\Blackmagic Design\DaVinci Resolve\Support\Fusion\Scripts\…`
- Disk database default: `C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\Resolve Disk Database\Resolve Projects` (or per-user `…\Support\Resolve Project Library`); on the rig `D:\Editing\ResolveDB` exists (a relocated project library) and project backups go to `D:\Editing\Exports\Resolve Project Backups`
- Gallery stills default (project setting `colorGalleryStillsLocation`): `E:\Davinci Resolve Videos\.gallery` on the workstation; `D:\Editing\Exports\.gallery` on the rig
- Cache clips (`perfCacheClipsLocation`): rig `F:\ResolveCache`; empty string in the workstation dump means "default"
