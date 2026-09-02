# 03 — CLI / batch reference (`gimp-console-3.2.exe`, `gimp-3.2.exe`)

Everything measured on GIMP 3.2.4 / Windows 11 / Qube, 2026-09-01, unless tagged.

## Options that matter (from `--help-all`) [measured text]
| Flag | Meaning | Notes |
|---|---|---|
| `-i, --no-interface` | run without UI | implied by `gimp-console-3.2.exe` [doc man page] |
| `-b, --batch=<command>` | batch command; repeatable | `-b -` reads commands from **stdin** (works with python-fu-eval) |
| `--batch-interpreter=<proc>` | procedure that runs the `-b` strings | **mandatory**; values on this build: `python-fu-eval`, `plug-in-script-fu-eval` |
| `--quit` | quit right after opening images + running batch | **mandatory** for scripts, see rule 2 |
| `-d, --no-data` | don't load brushes/gradients/patterns/palettes | use it; ~faster; prints `INFO:` lines |
| `-f, --no-fonts` | don't load fonts | **do not use** when text or `fonts_get_list` is involved (0 fonts) |
| `-s, --no-splash` | no splash | GUI only |
| `-n, --new-instance` | force a new GUI instance | otherwise files/batch go to the running one (batch refused on Win32) |
| `-a, --as-new` | open images as new (untitled) | |
| `-c, --console-messages` | messages to console instead of dialogs | GUI runs; console already does this |
| `-g, --gimprc=<file>`, `--system-gimprc=<file>`, `--session=<name>` | alternate configs | useful for a clean profile in CI-like runs [inferred] |
| `--pdb-compat-mode=off|on|warn` | 2.x procedure-name compatibility | `on`/`warn` may accept old names [doc]; not measured |
| `--stack-trace-mode`, `--debug-handlers`, `--g-fatal-warnings` | debugging | |
| `--dump-gimprc` | print a default gimprc | |
| `--verbose` | more output | |
| GEGL: `--gegl-threads=<n>`, `--gegl-cache-size=<MB>`, `--gegl-swap=<uri>`, `--gegl-disable-opencl`, `--gegl-quality=0..1`, `--gegl-tile-size`, `--gegl-chunk-size` | GEGL engine tuning | not measured |
| `FILE|URI...` positional | open images | in console mode they are loaded but not displayed |

## Canonical invocations [measured]
Run a Python file (the only sane way to pass code with quotes):
```
"C:\Program Files\GIMP 3\bin\gimp-console-3.2.exe" -i -d --batch-interpreter python-fu-eval -b "exec(open(r'C:\work\job.py').read())" --quit
```
Inline one-liner (Git Bash quoting shown; use `;` to separate statements):
```
gimp-console-3.2.exe -idf --batch-interpreter python-fu-eval -b "import sys;print(Gimp.version());sys.stdout.flush()" --quit
```
Commands from stdin (measured: printed `FROM-STDIN` and `3.2.4`, exit 0, 3.4 s):
```
printf 'print("FROM-STDIN")\nprint(Gimp.version())\n' | gimp-console-3.2.exe -idf --batch-interpreter python-fu-eval -b - --quit
```
Script-Fu, everything in ONE `-b` (a second `-b` is a fresh interpreter: `unbound variable`):
```
gimp-console-3.2.exe -i -d --batch-interpreter plug-in-script-fu-eval -b "(begin (load \"C:/work/lib.scm\") (my-proc \"C:/work/out\"))" --quit
```
Timing: cold 8.1 s, warm 3.0–3.5 s for a trivial batch; 30 s for the 77-step probe with 23
exports; a 6400×3600 RGBA-16 PNG export alone took 11.5 s; `transform_rotate` on a 1280×720
layer 1.5 s; `image.scale` 1600→1280 0.3 s.

## What the batch interpreters give you [measured]
- `python-fu-eval`: `exec()` of your string in `python-eval.py`'s globals with `Gimp` (and
  `gi`) already imported (`from gi.repository import Gimp` is pre-done — `Gimp.version()`
  works without an import). Import `Gegl`, `Gio`, `GLib`, `GObject` yourself
  (`gi.require_version('Gegl','0.4')`). Exceptions print a traceback and
  "batch command experienced a calling error … Stopping at failing batch command [N]"; exit 64.
  **Globals do NOT persist across `-b` strings** [measured 2026-09-01: `-b "x=41"` then
  `-b "print(x)"` → `NameError: name 'x' is not defined`; each `-b` is a separate
  `python-fu-eval` call] — put everything in one file and one `-b`.
- `plug-in-script-fu-eval`: TinyScheme; errors → "batch command experienced an execution
  error: Error: …", exit 70. Each `-b` is a separate evaluation [measured] (doc: no long-running state).
- Success prints "batch command executed successfully" once per `-b` on stderr.

## Exit codes [measured]
| Code | Meaning |
|---|---|
| 0 | every `-b` ran (does NOT mean your image work succeeded — check returns + files) |
| 64 | python-fu-eval raised (traceback printed, "Stopping at failing batch command [N]") — measured exit 64, same code as the next row, so read stderr to tell them apart |
| 64 | `-b` given without `--batch-interpreter` → "No batch interpreter specified. Available interpreters are: plug-in-script-fu-eval (Script-fu (scheme)), python-fu-eval (Python 3)"; batch NOT run |
| 69 | `--batch-interpreter no-such-proc` → "is not a valid batch interpreter. Batch mode disabled." |
| 70 | Script-Fu evaluation error |
| 1 | `gimp-3.2.exe -b` with a GUI already running (Win32 refusal) |
| 124 | your `timeout` fired: process alive because `--quit` was missing or `Gimp.quit()`/`(gimp-quit 0)` was used (both hang; `Gimp.quit` is also deprecated) |

## stderr you can ignore vs must read [measured]
Ignore: `GIMP-Warning: Welcome to GIMP 3.2.4!`, `INFO: argument 'gradient' (value "…") of
procedure 'script-fu-…' ignored because resources of type GimpGradient are not loaded` (effect
of `-d`), `Non-native shell detected…` (Git Bash), `DeprecationWarning: Gimp.Drawable.brightness_contrast is deprecated`.
Read: `GIMP-Error: Execution error for procedure '…': …` (e.g. `the filter "gegl:no-such-op" is
not installed`, `Unknown file type`, `Could not open '…' for writing: No such file or directory`),
`Warning: value "60.000000" … is invalid or out of range for property 'quality'` (arg dropped, export
continued), `GIMP-Error: Plug-in crashed: "script-fu.exe"` (after `(gimp-quit 0)`).
Filter noise with `2>&1 | grep -v "^INFO"` when reading, but keep the full log on disk.

## Quoting from each shell [measured]
- **Git Bash → gimp-console**: double-quote the `-b` string, single quotes inside Python; raw
  string `r'C:\…'` for Windows paths or forward slashes (both work). `taskkill /F` is mangled
  to `F:/` — use PowerShell for process work.
- **PowerShell**: `Start-Process -FilePath 'C:\Program Files\GIMP 3\bin\gimp-3.2.exe' -ArgumentList '-s','--batch-interpreter','python-fu-eval','-b','"exec(open(r''C:\x.py'').read())"'` (doubled single quotes inside a single-quoted PS string).
- **Script-Fu strings** need `\"` escapes inside the `-b` double quotes; Windows paths with
  forward slashes worked (`(load "C:/…/probe_sf.scm")`).
- Long code never goes inline: write the file, `exec(open(...).read())`, delete the file after.

## Wrapping for robustness (pattern) [measured pieces, assembled — inferred]
```bash
timeout 600 "/c/Program Files/GIMP 3/bin/gimp-console-3.2.exe" -i -d --batch-interpreter python-fu-eval \
  -b "exec(open(r'$JOB').read())" --quit > "$LOG" 2>&1; rc=$?
grep -q "GIMP-Error" "$LOG" && echo "gimp errors, read $LOG"
[ $rc -eq 0 ] || echo "exit $rc"
# then kill stragglers (gdbus.exe/python.exe) and verify the output file with ffprobe/Pillow
```
Have the job script itself write a JSON result (what it did, returns, sizes) so the caller
never parses stderr for success.
