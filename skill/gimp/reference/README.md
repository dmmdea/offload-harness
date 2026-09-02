# GIMP 3.2 autonomous-driving reference (skill documentation)

Built 2026-09-01 from: live probes on the Qube against GIMP **3.2.4** (`C:\Program Files\GIMP 3`,
embedded CPython 3.14.4, PyGObject 3.56.2) — a full PDB dump (1033 procedures), 77+40-step
editing/export probes, GUI and headless MCP lifecycle runs, Script-Fu batch runs; the gimp-mcp
source (`D:\Dev\tools\gimp-mcp`, upstream maorcc/gimp-mcp @ 09bfb2d) and its README/protocol
docs; the on-demand MCP launcher (`~/.claude/mcp-ondemand/mcp-ondemand.ps1` + `stash.json`);
official docs (developer.gimp.org API 3.0, docs.gimp.org 3.2 "Starting GIMP", Script-Fu Tools,
GIMP 3.0/3.2 release notes, the 2.10→3.0 porting guide "Removed Functions"), and community
notes (hnbdr migration gist). Every claim is tagged: **[measured]** = observed live on the
Qube on 2026-09-01, **[doc]** = official GIMP text, **[community]** = third-party report,
**[inferred]** = my reasoning, verify before relying.

Purpose: stop re-researching, stop guessing procedure names, translate designer instructions
into GIMP operations, run headless over a slow link, and never trust a return code.

## Reading order (load only what the task needs — this folder is NOT auto-loaded)

| File | Load when |
|---|---|
| `01-hosts-install.md` | any session: which box has GIMP, binaries, embedded Python, profile/config/log/temp dirs, installed fonts, verifier tools |
| `02-connection-modes.md` | choosing console vs GUI vs batch vs MCP; one-GIMP-per-machine; MCP start / tool list / stop; process-tree teardown |
| `03-cli-batch.md` | writing a `gimp-console-3.2.exe` command line: flags, interpreters, `--quit`, stdin batch, exit codes, quoting from Git Bash / PowerShell |
| `04-api-reference.md` | writing Python against `gi.repository.Gimp` / `Gegl`: object model, PDB-name→method rule, enums, PDB calls, DrawableFilter/GEGL, export procedure args |
| `05-editing-playbook.md` | turning "make it 1280×720 with a title, outline, shadow, no background" into operations; social sizes; verify checklist |
| `06-export-formats.md` | which extensions export here (23 measured), per-format args and defaults, what each file really contains, how to verify |
| `07-scripting-scriptfu-python-plugins.md` | Script-Fu 3 batch and scripts, Python plug-in registration model, interpreters, pluginrc, the gimp-mcp plug-in as a persistent-plug-in example |
| `08-failure-modes.md` | anything returned None/False/NULL, hung, exited 64/69/70, or wrote the wrong file; the full trap catalog with fixes |
| `live-dump-qube-2026-09-01.json` | exact signatures of 902 PDB procedures (all `file-*`, `gimp-image/layer/drawable/item/text-layer/selection/context/edit/*`), all 1033 names by type, Python `dir()` of every key class, 28 enums, 258 GEGL op names + property tables for ~120 ops, 453 font names (sample), the 23-format export matrix with verification, every probe step with timing, the 79 MCP tool names |

## The ten rules (memorize; the rest of the folder is detail)

1. **There is no default batch interpreter.** Every batch call needs
   `--batch-interpreter python-fu-eval` (or `plug-in-script-fu-eval`); without it GIMP prints
   "No batch interpreter specified", never runs your `-b`, and exits 64 (or hangs without
   `--quit`). **[measured 3.2.4]**
2. **`--quit` is the only clean exit.** `Gimp.quit()` inside python-fu-eval errors
   ("returned no return values") and the process hangs; `(gimp-quit 0)` likewise crashed
   script-fu.exe and hung. Always pass `--quit` and wrap the call in a timeout. **[measured]**
3. **Use `gimp-console-3.2.exe -i -d`** for unattended work (cold start ≈ 8 s, warm ≈ 3.4 s,
   77-step edit + 23 exports in 30 s). Do NOT add `-f`: it loads zero fonts, so text layers
   and `Font.get_by_name` silently fail. **[measured]**
4. **Kill the whole process tree, not just gimp-console.** Plug-ins are separate processes
   (`python.exe`, `script-fu.exe`, `gdbus.exe`); killing the GIMP host leaves them alive —
   the MCP plug-in kept port 9877 open after its GIMP died. Filter `Win32_Process` by
   `ExecutablePath -like 'C:\Program Files\GIMP 3\*'` and stop them all. **[measured]**
5. **PDB calls are `lookup_procedure` → `create_config` → `set_property` → `run` → `index(0)`.**
   `Gimp.get_pdb().run_procedure` does not exist on 3.2.4 (AttributeError). Status
   `PDBStatusType.SUCCESS == 3`; a failed run returns 0/EXECUTION_ERROR with
   `get_last_error()`, it does not raise. **[measured]**
6. **Failures are quiet.** `Gimp.file_save` returns False (unknown extension, missing dir),
   `image.crop(99999,…)` returns False, `Font.get_by_name("Impact")` returns None (the exact
   listed name is "Impact Regular"), `file_load` of a missing file returns None, an
   out-of-range export arg is dropped and the export still succeeds with the default. Check
   every return, then verify the FILE with ffprobe/Pillow. **[measured]**
7. **Add alpha BEFORE resizing a layer or canvas.** A layer without alpha grows into the
   background colour (white) — measured pixel (1,1,1,1) after `resize_to_image_size` on a
   no-alpha layer. Remove-background = `add_alpha()` → select → `edit_clear()`. **[measured]**
8. **GEGL filters are `Gimp.DrawableFilter.new(drawable, "gegl:op", name)` → `get_config()`
   → `set_property` → `update()` → `merge_filter()` (destructive) or `append_filter()`
   (non-destructive, layers only).** Bad op name → `TypeError: constructor returned NULL`.
   Property names and defaults for ~120 ops are in the live dump. **[measured + doc]**
9. **Export = `Gimp.file_save(RunMode.NONINTERACTIVE, image, Gio.File, None)`** by extension
   (23 extensions verified). Alpha/precision/mode are auto-adapted per format (JPEG flattens,
   GIF indexes, U16→JPEG down-converts) — but TIFF writes every layer as a page, ICO/DDS/SVG
   use the layer bounding box (1358×798 from a 1280×720 canvas with a shadow overflow), and
   PNG keeps full alpha. Flatten or `merge_visible_layers` first when you want one page. **[measured]**
10. **Batch to a running GUI is impossible on Windows** ("Batch commands cannot be run in
    existing instance in Win32", exit 1) and a second `gimp-3.2.exe` just hands its files to
    the first. Console instances run fine beside a GUI but see none of its images. Drive a
    live GUI only through the MCP socket. **[measured]**
