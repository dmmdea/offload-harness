# 01 — Hosts, install, paths, tools

Check `hostname` first. Facts below were measured on the **workstation** on 2026-09-01 and on the
**editing rig** on 2026-09-10 unless tagged otherwise. **GIMP runs on exactly two of the four
machines** — the workstation and the editing rig; the laptop and the edge node have no GIMP at all
(both re-probed 2026-09-10). The two GIMP hosts are not interchangeable: gimp-mcp exists only on
the workstation, the brand fonts only on the rig.

## workstation — GIMP 3.2.4 [measured]

| Item | Value |
|---|---|
| Install root | `C:\Program Files\GIMP 3` |
| Version | `gimp-console-3.2.exe --version` → "GNU Image Manipulation Program version 3.2.4" |
| GUI binary | `bin\gimp-3.2.exe` (aliases `gimp-3.exe`, `gimp.exe`, identical size) |
| Console binary | `bin\gimp-console-3.2.exe` (aliases `gimp-console-3.exe`, `gimp-console.exe`) — behaves as `gimp --no-interface` [doc: Arch man page] |
| Script-Fu interpreter | `bin\gimp-script-fu-interpreter-3.0.exe` — **cannot run standalone**: `--help` → "is a GIMP plug-in and must be run by GIMP to be used" |
| Other bins | `gegl.exe` (`--list-all` → 258 op names), `gimptool-3.2.exe`, `gimp-debug-tool-3.2.exe`, `gdbus.exe`, `python.exe` / `pythonw.exe` |
| Embedded Python | `bin\python.exe` = **CPython 3.14.4** "[MINGW Clang UCRT 22.1.3 64 bit (AMD64) (GCC)]"; `gi.__version__` = **3.56.2**; site-packages `lib\python3.14\site-packages` (gi, cairo, numpy-free; also meson/jinja2/markdown — build leftovers) |
| Batch Python host | batch code runs inside plug-in `lib\gimp\3.0\plug-ins\python-eval\python-eval.py` (`sys.argv[0]`), pid ≠ gimp-console pid |
| Interpreter maps | `lib\gimp\3.0\interpreters\pygimp.interp` → `python=python.exe`, `:Python:E::py::python3:`; `pygimp_win.interp` uses `pythonw.exe`; `gimp-script-fu-interpreter.interp` maps `.scm` → the interpreter exe |
| Plug-in env | `lib\gimp\3.0\environ\default.env`: `PATH=${gimp_installation_dir}\bin`, `PYTHONDONTWRITEBYTECODE=1`, `__COMPAT_LAYER=HIGHDPIAWARE` |
| System plug-ins | `lib\gimp\3.0\plug-ins\<name>\` — 140+ incl. every `file-*` loader/exporter, `python-console`, `python-eval`, `script-fu`, `script-fu-server`, `procedure-browser`, `plugin-browser`, `screenshot`, `metadata-*` |
| System scripts | `share\gimp\3.0\scripts\*.scm` (drop-shadow, round-corners, addborder, perspective-shadow, …) |
| System gimprc | `etc\gimp\3.0\gimprc` (defaults: `plug-in-path "${gimp_dir}/plug-ins:${gimp_plug_in_dir}/plug-ins"`, `interpreter-path` likewise) |
| Data dir (`Gimp.data_directory()`) | `C:\Program Files\GIMP 3\share\gimp\3.0` |
| Sysconf dir | `C:\Program Files\GIMP 3\etc\gimp\3.0` |

### Per-user profile (`Gimp.directory()`) [measured]

`%APPDATA%\GIMP\3.2` = `%APPDATA%\GIMP\3.2`. GIMP names it after
**major.minor** and creates a fresh one per minor upgrade (3.0 → 3.2 moved it) [doc: gimp-mcp
README + measured folder name]. Contents that matter:

| Path | Role |
|---|---|
| `gimprc` | user overrides (currently: `config-version "3.2.4"`, monitor res 140 dpi, fill/stroke options) |
| `pluginrc` | plug-in registration cache (312 KB); the gimp-mcp plug-in is registered here as `plug-in-mcp-server/-check/-restart` |
| `plug-ins\gimp-mcp-plugin\gimp-mcp-plugin.py` | the only user plug-in installed (copy of `D:\Dev\tools\gimp-mcp\gimp-mcp-plugin.py`) |
| `scripts\` | user Script-Fu scripts (empty) |
| `fonts\` | per-user font dir (empty) — Windows fonts are picked up from the system via fontconfig |
| `tmp\` | GIMP's own temp; batch runs also create `%LOCALAPPDATA%\Temp\gimp-3.2-XXXXXX` (`Gimp.temp_directory()`) |
| `sessionrc`, `devicerc`, `toolrc`, `templaterc`, `tags.xml` | GUI state |

### Logs [measured]
- There is **no GIMP log file on Windows.** Messages go to the console's stderr
  (`GIMP-Warning: Welcome to GIMP 3.2.4!`, `GIMP-Error: …`, plug-in `Warning`s, `INFO:` lines
  about ignored gradient/palette args when run with `-d`). Capture with `2>&1` into your own file.
- `%LOCALAPPDATA%\GIMP\3.2\CrashLog\` (empty) receives crash dumps; `%LOCALAPPDATA%\GIMP\3.2\fontconfig\cache\` is the font cache (2.3 MB, rebuilt on first font load).
- `Gimp.message()` from a batch script prints `python-eval.py-Warning: <text>` on stderr;
  `Gimp.message_set_handler(Gimp.MessageHandlerType.CONSOLE)` is accepted (returns True).

### Fonts [measured]
453 fonts loaded (`Gimp.fonts_get_list("")`). Names are **family + style**: "Impact Regular",
"Arial Bold", "Segoe UI Black", "Bahnschrift SemiBold Condensed", "Sans-serif Bold" (the
built-in alias). `Font.get_by_name("Impact")` / `("Arial")` → **None**; `("Impact Regular")`
/ `("Arial Regular")` → Font. **Brand fonts Montserrat / Anton / League Gothic are NOT
installed on the workstation** — they live on the editing rig, confirmed there through GIMP itself on
2026-09-10 (see that host's section below). Render brand titles on the rig, or install the fonts
machine-wide / per-user (`%APPDATA%\GIMP\3.2\fonts`) before rendering them here.

### Verifier tools on the workstation [measured]
| Tool | Where | Use |
|---|---|---|
| `ffprobe` | on PATH (Gyan ffmpeg 8.1.2 winget build); harness also has `D:\Dev\tools\ffmpeg-9.0.1\bin` | codec, WxH, pix_fmt for png/jpg/webp/tif/gif/bmp/avif/heic/jxl/jp2/tga/exr/qoi/psd/ico |
| Python 3.14 + Pillow 12.3.0 | `C:\Program Files\Python314\python.exe` (system, NOT GIMP's) | size, mode (RGB/RGBA/P), `info['dpi']`, `n_frames`, `getextrema()`; cannot open heic/jxl/psb/exr/xcf/ora |
| `pdfinfo` | poppler (winget) on PATH | PDF page size/count |
| `magick` | **not on PATH** | — |
| `gegl.exe --list-all` | GIMP bin | op inventory without starting GIMP |

### Related installs
- gimp-mcp source: `D:\Dev\tools\gimp-mcp` (git remote github.com/maorcc/gimp-mcp, HEAD 09bfb2d
  "Address CodeRabbit nitpicks"); deps via `uv` (`pyproject`: mcp, fastmcp; python ≥3.11).
- On-demand MCP launcher: `%USERPROFILE%\.claude\mcp-ondemand\mcp-ondemand.ps1` + `stash.json`
  (gimp-mcp stashed as user-scope stdio: `uv run --directory D:/Dev/tools/gimp-mcp gimp_mcp_server.py`).
- local-offload harness `flatten_design` route is bound to `gimp_console_path=C:/Program Files/GIMP 3/bin/gimp-console-3.2.exe` [measured via offload_status]; mem0 evidence: fresh-install host-tool discovery configures the GIMP console path and `edit_python` only best-effort and never modifies an existing config.

## editing-rig — the editing rig, GIMP 3.2.4 [measured 2026-09-10]

**The second GIMP host, and the one with the brand fonts.** Same installer, same layout as the
workstation, so everything in 02–08 transfers; the differences below are the ones that change what you
can do there.

| Item | Value |
|---|---|
| GIMP | **3.2.4**, `C:\Program Files\GIMP 3\bin\gimp-console-3.2.exe` (+ `gimp-console-3.exe`) |
| Embedded Python / PyGObject | **3.14.4 / 3.56.2** — identical to the workstation |
| PDB | **1030 procedures** (workstation: 1033). The delta is exactly the three gimp-mcp procs; `plug-in-mcp-server` is absent here. 60 `file-*-export`, no `file-apng-export` (same as the workstation) |
| Fonts | **449** loaded (workstation: 453) |
| GEGL | 258 ops, and `Gegl.list_operations()` returns **0 until `Gegl.init(None)`** (same trap as the workstation, 08 #39) |
| Profile | `%APPDATA%\GIMP\3.2`, **created by my first run on 2026-09-10** — GIMP had never been launched on this box |
| Temp | `D:\Temp\gimp-3.2-XXXXXX` — **not** `%LOCALAPPDATA%\Temp` like the workstation (this box redirects TEMP to D:) |
| gimp-mcp | **not installed** — no plug-in in the profile, no `D:\Dev\tools\gimp-mcp` source. MCP mode (02 §C) is workstation-only |
| ffprobe | `D:\WinGet\Portable\Gyan.FFmpeg_…\ffmpeg-9.0-full_build\bin\ffprobe.exe` (9.0, portable — **not** the same path as the workstation) |
| Python (verifier) | `C:\Program Files\Python311\python.exe`. No `magick`, no `uv` |
| Skills | `~/.claude/skills/{gimp,davinci-resolve,ffmpeg}` deployed 2026-09-10 (11 + 14 + 35 files, byte-verified remotely); `claude.exe` via WinGet |
| Also on the box | DaVinci Resolve (this is the editing rig — see the `davinci-resolve` skill) |

### The two facts that actually change your script [measured]
1. **Brand fonts live here, not on the workstation.** `Anton Regular`, `Impact Regular`,
   `League Gothic Regular` / `Condensed` / `SemiCondensed`, and Montserrat in nine weights
   (`Thin, ExtraLight, Light, Regular, Medium, SemiBold, Bold, ExtraBold, Black`).
   Family-only lookup still fails exactly as on the workstation: `Font.get_by_name("Anton")` → **None**,
   `("Anton Regular")` → Font. Same for Montserrat, League Gothic and Impact. **Render brand
   titles here; the workstation falls back to Sans-serif.**
2. **First run on a fresh profile costs ~55 s, not ~8 s.** Measured: 55.4 s for the very first
   `gimp-console` invocation (it builds `%APPDATA%\GIMP\3.2` and the fontconfig cache), then
   **3.7 s warm** — the same steady state as the workstation. The first *export* inside that cold run
   also paid a one-time cost (PNG 7.8 s); the JPEG and WebP right after it took 0.45 s and
   0.38 s, matching the workstation. Budget the first call on any fresh box accordingly, and do not
   read the cold number as this machine being slow.

End-to-end proof on this box (console mode, 2026-09-10): 1280×720 canvas → linear gradient →
`Anton Regular` 110 px title with outline + `gegl:dropshadow` → 300 dpi → exported PNG
(`png,1280,720,rgba`, 569 578 B), JPEG (`mjpeg,…,yuvj444p`, 31 639 B), WebP
(`webp,…,yuv420p`, 10 656 B) and XCF, all verified with the local ffprobe; zero GIMP processes
left behind.

### Other hosts [measured 2026-09-10]
| Host | Result |
|---|---|
| the laptop (Windows) | **no GIMP** — no `C:\Program Files\GIMP*`, no console binary. Has Resolve, ffprobe 8.1.1, `uv`, and only `impact.ttf` of the brand fonts. Skills deployed here too (gimp/davinci-resolve/ffmpeg) so an agent on this box learns GIMP is absent instead of hunting for it |
| the edge node (Linux, ssh as the edge user) | **no GIMP**: nothing on PATH, no flatpak/snap/dpkg GIMP [measured 2026-09-01] |
So GIMP runs on exactly two boxes: the workstation (with gimp-mcp, without brand fonts) and the
editing rig (brand fonts, no gimp-mcp). Re-probe any host with:
`ssh <user>@<host> 'powershell -NoProfile -Command "Test-Path \"C:\Program Files\GIMP 3\bin\gimp-console-3.2.exe\""'`
(the remote login shell is PowerShell, so `$`-expansion happens once before your inner shell sees
it — for anything longer, send `powershell -EncodedCommand <base64-UTF16>`).
