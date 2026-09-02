# 08 — Failure modes, traps, and the fix for each

Ordered by how likely they are to bite an autonomous run. Tags: [measured] on the workstation 2026-09-01 · [doc] · [community] · [inferred].

| # | Symptom | Cause | Fix |
|---|---|---|---|
| 1 | "No batch interpreter specified. Available interpreters are: plug-in-script-fu-eval, python-fu-eval", exit 64, nothing ran (and a `script-fu.exe` crash without `--quit`) [measured] | no `--batch-interpreter`; 3.2 has no default | always `--batch-interpreter python-fu-eval` |
| 2 | Process never exits; `timeout` kills it; "GIMP is now running as a background process. You can quit anytime with Ctrl-C" [measured] | `--quit` missing; or you called `Gimp.quit()` / `(gimp-quit 0)` (both error/crash then hang) | `--quit` on every batch run; never quit from code; wrap with `timeout` |
| 3 | Port 9877 refused / stale plug-in answers then "forcibly closed"; next MCP start never listens [measured] | killing gimp-console/gimp-3.2 leaves plug-in `python.exe` (and `gdbus.exe`) alive holding the port | stop every process whose `ExecutablePath` is under `C:\Program Files\GIMP 3\` before starting again (02 §4) |
| 4 | `AttributeError: 'PDB' object has no attribute 'run_procedure'` [measured] | removed API | `lookup_procedure` → `create_config` → `set_property` → `run` → `index(0)` |
| 5 | `TypeError: constructor returned NULL` from `Gimp.Display.new` [measured] | console mode has no display | skip displays headless; MCP `new_canvas` needs GUI mode (image is still created) |
| 6 | `TypeError: constructor returned NULL` from `Gimp.DrawableFilter.new` + `GIMP-Error: the filter "gegl:xyz" is not installed` [measured] | wrong op name | check `gegl.exe --list-all` / dump `gegl.ops`; e.g. no `gegl:desaturate` (use `gegl:saturation` scale 0) |
| 7 | Text layer creation fails / `Font.get_by_name` None / `fonts_get_list` empty [measured] | `-f/--no-fonts` on the command line | drop `-f`; `-d` is fine |
| 8 | `Font.get_by_name("Impact")` → None though Impact is installed [measured] | names are family + style: "Impact Regular", "Arial Bold" | pick from `fonts_get_list("(?i)impact")`; fall back to `Gimp.context_get_font()` and log it |
| 9 | White (or BG-colour) border after canvas resize / `resize_to_image_size` [measured pixel 1,1,1,1] | layer had no alpha; new area filled with context background | `layer.add_alpha()` before `img.resize`/`layer.resize*`; or `Gimp.context_set_background(Gegl.Color.new("rgba(0,0,0,0)"))` [inferred] |
| 10 | Second text line missing / text cut off [measured image] | `tl.resize(w,h)` made a fixed box; oversized font (48 pt @300 dpi = 200 px) | don't fix the box, or make it big enough; use `Gimp.Unit.pixel()` |
| 11 | Export "succeeded" but wrong quality/args [measured: `quality=60` → default 0.9, status SUCCESS, stderr `Warning: value "60.000000" … out of range`] | typed/ranged GParam rejected the value and kept the default | JPEG quality is 0–1; AVIF/HEIC quality is int 0–100; read stderr for `out of range`; verify bytes |
| 12 | `Gimp.file_save` → False, no file [measured] | unknown extension (`Unknown file type`) or missing directory (`Could not open … for writing`) | use a known extension (06); `os.makedirs` first |
| 13 | TIFF has N pages / Pillow `n_frames` > 1 [measured 5 pages from 4 layers] | TIFF exporter writes each layer as a page | `merge_visible_layers(CLIP_TO_IMAGE)` (or flatten) before export |
| 14 | ICO/DDS/SVG raster is 1358×798 instead of 1280×720 [measured] | those exporters use the union of layer bounds (drop shadow overflow) | `layer.resize_to_image_size()` on every layer or `img.resize_to_layers()`→crop, or merge/flatten first |
| 15 | JPEG/BMP/WebP lost transparency [measured] | exporter can't store alpha (JPEG/BMP) or image fully opaque (WebP) | use PNG/WebP/AVIF/TIFF for alpha; confirm `getextrema()[3][0] < 255` |
| 16 | `img.crop(...)`/setters return False silently [measured] | out-of-range args; PDB EXECUTION_ERROR without exception | check bools; on PDB `run` compare `index(0)` to `PDBStatusType.SUCCESS` and read `pdb.get_last_error()` |
| 17 | `Gimp.file_load` → None + `GIMP-Error: Calling error … 'file-png-load' … value '<not transformable to string>' for argument 'file'` [measured] | file does not exist | check `os.path.exists` before; treat None as failure |
| 18 | `'_ResultTuple' object has no attribute 'get_rgba'` [measured] | multi-return procedure (`pick_color`, `get_offsets`, `bounds`, `get_resolution`) returns a tuple | index `[1]`/named field first |
| 19 | Script-Fu `Error: eval: unbound variable: my-proc`, exit 70 [measured] | each `-b` is a separate interpreter run | single `-b "(begin (load …) (call …))"` |
| 20 | Script-Fu `atom->string: bad base` [measured] | treated a string return as number (`gimp-version` returns string) | check return types; every PDB result is a list → `(car …)` |
| 21 | "Batch commands cannot be run in existing instance in Win32", exit 1 [measured] | `gimp-3.2.exe -b` while a GUI runs | use `gimp-console` (independent instance), `-n`, or the MCP socket into the running GUI |
| 22 | Console job sees `len(Gimp.get_images()) == 0` although GIMP GUI has images open [measured] | separate instances | MCP into the GUI, or pass files to the console job |
| 23 | MCP `get_state_snapshot` via raw socket → `error: 'args'` [measured raw] | raw message shape differs from the stdio server's | go through `gimp_mcp_server.py` (stdio) — `get_state_snapshot`, `get_image_bitmap`, `check_server`, `get_image_metadata`, `call_api` all verified there, headless included |
| 24 | MCP tools "No images are currently open in GIMP" [measured] | plug-in host has no image | `open_image`/`new_canvas` (GUI) first, or `call_api exec` to `Gimp.file_load` |
| 45 | MCP `open_image` headless → "constructor returned NULL" [measured] | it calls `Gimp.Display.new` after loading | the image IS open (metadata/snapshot work next); treat the error as a warning headless, or load via `call_api exec` |
| 46 | `.apng` export → False, "Unknown file type" [measured] | 3.2.4 Windows has `file-apng-load` only, no exporter | animated GIF/WebP from GIMP, or ffmpeg `-f apng` from a PNG sequence |
| 25 | New Claude session lacks `mcp__gimp-mcp__*` tools | server is stashed (on-demand) | `mcp-ondemand.ps1 on gimp-mcp`, then a NEW session; `off` afterwards |
| 26 | `DeprecationWarning: Gimp.Drawable.brightness_contrast/desaturate is deprecated` [measured] | 3.2 deprecates 12 colour ops | still works; prefer `gegl:brightness-contrast`, `gegl:saturation` etc. via DrawableFilter |
| 27 | Stray `INFO: argument 'gradient' … ignored because resources of type GimpGradient are not loaded` [measured] | `-d` skipped gradients; stock Script-Fu shadows need them | harmless unless you call `script-fu-drop-shadow` etc.; then drop `-d` or use `gegl:dropshadow` |
| 28 | `Non-native shell detected, GIMP may behave unexpectedly on Unix shells in Windows` [measured] | launched from Git Bash | cosmetic; use PowerShell `Start-Process` for GUI launches |
| 29 | `taskkill /F` → "Invalid argument/option - 'F:/'" [measured] | Git Bash path mangling | `taskkill //F //IM …` or PowerShell `Stop-Process` |
| 30 | Heredoc/inline quoting breaks (`unexpected EOF while looking for matching '`) [measured twice this session] | long Python with quotes passed through bash | write the file with a file tool, then `exec(open(...).read())` |
| 31 | Windows path in `-b` mangled | backslashes inside the Python string | raw strings `r'C:\…'` or forward slashes (both worked) |
| 32 | `gimp-drawable-merge-new-filter` / `gimp-image-thumbnail` / `gimp-image-get-active-layer` "missing" in PDB [measured] | libgimp-only helpers or removed procs | use Python methods (`merge_filter`, `get_thumbnail_data`, `get_selected_layers`) |
| 33 | Exported PNG > platform cap (YouTube 2 MB) | compression 9 but 16-bit or huge canvas | `convert_precision(U8_NON_LINEAR)`, right size, or JPEG q 0.85–0.9 |
| 34 | `set_text` after a merged filter on a text layer does nothing; stderr `Gimp-Text-CRITICAL: gimp_text_layer_set: assertion 'gimp_item_is_text_layer' failed`; `is_text_layer()` still True, `set_text` returns True, export byte-identical [measured] | `merge_filter` converted the layer in the core; the client-side flag lies | finish text edits before baking effects, or bake on `layer.copy()`, or use `append_filter` |
| 41 | `foreground_extract(MATTING, trimap)` returned True but pixels unchanged [measured] | the result is written to the image selection, not the drawable | `Gimp.Selection.invert(img); layer.edit_clear()` (layer must have alpha) |
| 42 | Python `-b` #2 gets `NameError` for a name defined in `-b` #1 [measured] | each `-b` is a separate `python-fu-eval` call; globals do not persist | one `-b`, one file |
| 43 | Script-Fu server client hangs reading the reply [measured] | reply header is 4 bytes (`G`, err, 2-byte length), not 6 | parse `G` + 1 + 2-byte BE length; see 02 §D |
| 44 | `Calling Plug-In PDB procedures with arguments as an ordered list is deprecated` [measured] | positional args to a plug-in from Script-Fu | use `#:name value` keyword args |
| 35 | 6400×3600 16-bit PNG export took 11.5 s; huge TIFFs 12 MB+ [measured] | size × depth × pages | budget timeouts (≥ 600 s for batches); export what the target needs |
| 36 | Second GIMP GUI "did nothing" [measured] | single-instance hand-off | expected; look at the first window's title |
| 37 | GUI window left open / plug-in processes after a session | forgot teardown | 02 §4 kill-tree snippet; check `Get-NetTCPConnection -LocalPort 9877` |
| 38 | PDB dump shows `GimpUnit` defaults as `<Gimp.Unit object …>` | live objects, not serialisable | use `Gimp.Unit.pixel()/point()` in code |
| 39 | `Gegl.list_operations()` returns [] with `g_hash_table` assertion warnings [measured in the dump run] | GEGL not initialised in that plug-in process before use | `Gegl.init(None)` first (then 258 ops) — or use `gegl.exe --list-all` |
| 40 | Brand fonts render as Sans-serif [measured: Montserrat/Anton/League Gothic absent] | not installed on the workstation | install machine-wide or into `%APPDATA%\GIMP\3.2\fonts`; assert `Font.get_by_name` is not None before rendering |

## Diagnostic ladder
1. Exit code + stderr (`GIMP-Error`, `Warning: … out of range`, `Stopping at failing batch command`).
2. Your job's JSON result: which step returned False/None.
3. The FILE: `ffprobe`, Pillow size/mode/dpi/frames/extrema, then look at it.
4. Process tree (`Win32_Process` under `C:\Program Files\GIMP 3\`) and port 9877 — clean before retrying.
5. Reproduce the single failing call in a 3-line batch (`3.4 s` warm) before rewriting the job.
