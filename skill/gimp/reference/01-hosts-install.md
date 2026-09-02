# 01 — Hosts, install, paths, tools

Check `hostname` first. All facts below were measured on the **workstation** on 2026-09-01 unless tagged
otherwise. GIMP presence on the other machines (the editing rig, the laptop, the edge node) is **unverified** — do not assume; check `Test-Path 'C:\Program Files\GIMP 3\bin'`.

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
installed on the workstation** (they live on the editing rig per the Resolve reference) — install them
machine-wide or per-user (`%APPDATA%\GIMP\3.2\fonts`) before rendering brand titles here.

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

### Other hosts [probed 2026-09-01 22:40]
| Host | Result |
|---|---|
| the edge node (Linux, ssh as the edge user) | **no GIMP**: no `gimp`/`gimp-3.0`/`gimp-console` on PATH, no flatpak/snap/dpkg GIMP [measured] |
| the laptop (Windows) | **offline** — Tailscale "last seen 7h ago", SSH timed out; unverified |
| the editing rig (Windows, editing rig) | **offline** — Tailscale "last seen 5h ago", SSH timed out; unverified. Brand fonts (League Gothic, Montserrat, Anton) live there |
So today the workstation is the only GIMP host. If GIMP is installed on another Windows box the layout
above transfers 1:1 (same installer); the profile is under that user's `%APPDATA%\GIMP\3.2`.
Re-probe with: `ssh <user>@<host> 'powershell -NoProfile -Command "Test-Path \"C:\Program Files\GIMP 3\bin\gimp-console-3.2.exe\""'`.
