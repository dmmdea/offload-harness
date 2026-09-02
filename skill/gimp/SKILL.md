---
name: gimp
description: Use when the user wants to edit, resize, crop, composite, retouch, add text/titles, remove a background, add a drop shadow or outline, batch-convert or export images with GIMP 3.2 — or any task that means driving GIMP programmatically: gimp-console batch mode, Python (GObject-introspection Gimp/Gegl), Script-Fu, the gimp-mcp server, GEGL filters, YouTube thumbnails / social-size exports, PNG/JPEG/WebP/AVIF/TIFF/PDF export. Triggers on "GIMP", "gimp-console", "python-fu", "script-fu", "make a thumbnail", "remove the background", "export as webp", "batch resize these images", "add a title to this image".
---

# GIMP 3.2 (autonomous driving)

This file only routes. Every fact, command line, procedure name and trap lives in
`reference/` (not auto-loaded — open the file the task needs). Facts there are tagged
[measured]/[doc]/[community]/[inferred]; anything untagged is a bug in the doc.

**Start here:** `reference/README.md` — the index plus the ten rules (memorize those; they
cover 90 % of what goes wrong: no default batch interpreter, `--quit` is mandatory, the
plug-in process outlives gimp-console, alpha before resize, exact font names, verify by file).

| Task | Open |
|---|---|
| Which box has GIMP, paths, embedded Python, profile/log/temp dirs, verifier tools | `reference/01-hosts-install.md` |
| Console vs GUI vs batch vs MCP, one-GIMP-per-machine, MCP start / tool list / stop, teardown | `reference/02-connection-modes.md` |
| Exact `gimp-console-3.2.exe` command lines, flags, exit codes, quoting, stdin batch | `reference/03-cli-batch.md` |
| Writing Python against `gi.repository.Gimp` (classes, methods, enums, PDB calls, GEGL filters) | `reference/04-api-reference.md` |
| Designer words → operations (resize, crop, canvas, text, outline, shadow, remove BG, composite, thumbnail sizes) | `reference/05-editing-playbook.md` |
| Export formats that actually work here, per-format args, how to verify the file | `reference/06-export-formats.md` |
| Script-Fu 3, Python plug-in registration, the gimp-mcp plug-in as a model | `reference/07-scripting-scriptfu-python-plugins.md` |
| Something returned None/False/NULL, hung, or wrote the wrong file | `reference/08-failure-modes.md` |
| Exact PDB signatures on this build (902 procs), enums, GEGL op properties, fonts, export matrix | `reference/live-dump-qube-2026-09-01.json` (grep it; do not load whole) |

Working rules that are not GIMP facts: console mode only unless a human wants to watch;
kill the whole GIMP process tree when done (never leave a GIMP window or plug-in process);
temp scripts go in the session scratchpad and are deleted; outputs are verified with a
second tool (ffprobe / Pillow / pdfinfo), never by return code alone.
