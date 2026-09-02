# 07 — Scripting: python-fu-eval, Python plug-ins, Script-Fu 3

## Three ways to run code in GIMP 3 [measured/doc]
| Way | Lifetime | Registers in PDB/menus | Use |
|---|---|---|---|
| `--batch-interpreter python-fu-eval -b "…"` | one console run | no | all unattended jobs (this library's default) |
| Python plug-in in `%APPDATA%\GIMP\3.2\plug-ins\<name>\<name>.py` | registered at startup, run on demand | yes | reusable tools, persistent servers (gimp-mcp), GUI menu items |
| Script-Fu `.scm` in `%APPDATA%\GIMP\3.2\scripts\` or via `plug-in-script-fu-eval` | per snippet (fresh interpreter) | yes for scripts | legacy/simple scripts; all stock "Filters ▸ Light and Shadow ▸ Drop Shadow (legacy)" style effects |

## python-fu-eval details [measured]
- Code executes in `lib\gimp\3.0\plug-ins\python-eval\python-eval.py` (a plug-in process,
  `bin\python.exe` 3.14.4) via `exec(code, globals())` — `Gimp` is pre-imported; import
  `Gegl/Gio/GLib/GObject` yourself. Stdout is forwarded to the console; flush it
  (`sys.stdout.flush()`) before long operations or the end, or lines may not appear before the
  host exits.
- Uncaught exception → traceback + "batch command experienced a calling error" + exit 2; the
  image work done so far is lost (no display, no autosave).
- Put your job in a file and `exec(open(path).read())`; have it write a JSON result file.
- `Gimp.quit()` is deprecated and hangs the run; end with `--quit`.
- There is no `pdb.` proxy and no `gimpfu`; see 04 for the PDB call pattern.

## Python plug-in registration model (GIMP 3) [doc: docs.gimp.org tutorial + measured on gimp-mcp-plugin.py]
Layout rule: folder name == file name (`plug-ins\my-tool\my-tool.py`); no shebang/exec bit needed
on Windows (the `.py` → `python.exe` mapping comes from `interpreters\pygimp.interp`); on
Linux/macOS `chmod +x` + `#!/usr/bin/env python3`. GIMP caches registrations in `pluginrc`
(re-queried when the file mtime changes).
```python
#!/usr/bin/env python3
import sys, gi
gi.require_version('Gimp', '3.0'); from gi.repository import Gimp, GLib, GObject
# gi.require_version('GimpUi', '3.0'); from gi.repository import GimpUi   # only for dialogs

class MyPlugIn(Gimp.PlugIn):
    def do_set_i18n(self, name): return False            # no translations (stops "catalog directory does not exist" logs)
    def do_query_procedures(self): return ["my-tool-run"]
    def do_create_procedure(self, name):
        # ImageProcedure: run(procedure, run_mode, image, drawables, config, run_data) — for Filters-menu tools
        proc = Gimp.ImageProcedure.new(self, name, Gimp.PDBProcType.PLUGIN, self.run, None)
        proc.set_image_types("*"); proc.set_sensitivity_mask(Gimp.ProcedureSensitivityMask.DRAWABLE)
        proc.set_menu_label("My tool"); proc.add_menu_path('<Image>/Filters/Danmar/')
        proc.set_documentation("blurb", "help", name); proc.set_attribution("author", "copyright", "2026")
        proc.add_double_argument("radius", "Radius", "Blur radius", 0.0, 100.0, 5.0, GObject.ParamFlags.READWRITE)
        return proc
    def run(self, procedure, run_mode, image, drawables, config, run_data):
        radius = config.get_property("radius")
        # … work …
        return procedure.new_return_values(Gimp.PDBStatusType.SUCCESS, GLib.Error())

Gimp.main(MyPlugIn.__gtype__, sys.argv)
```
- Plain `Gimp.Procedure.new(self, name, Gimp.PDBProcType.PLUGIN, self.run, None)` (as gimp-mcp
  does) gives `run(procedure, config, run_data)`; add `run-mode` yourself with
  `procedure.add_enum_argument("run-mode", "Run mode", "…", Gimp.RunMode, Gimp.RunMode.INTERACTIVE, GObject.ParamFlags.READWRITE)`.
- Argument helpers on `Gimp.Procedure`: `add_boolean/int/double/string/enum/choice/color/file/font/brush/pattern/gradient/palette/image/layer/drawable/channel/path/unit/*_array_argument`, `*_aux_argument`, `*_return_value` (full list in the dump `python_api_dir.Procedure`).
- Other procedure classes: `Gimp.BatchProcedure` (a new `--batch-interpreter`; `set_interpreter_name`), `Gimp.ExportProcedure`/`LoadProcedure`/`VectorLoadProcedure`/`ThumbnailProcedure` (file formats), `Gimp.ImageProcedure`.
- Calling from batch: `lookup_procedure('my-tool-run')` → config → `run` (04). Non-interactive
  run of a menu plug-in needs `run-mode NONINTERACTIVE` set on the config.
- Debug: plug-in `print()` goes to the GIMP host's console (stderr/stdout of gimp-console); in a
  GUI session use `Gimp.message()` or redirect `sys.stdout` to a file [community gist];
  `--verbose`/`--console-messages` on the host; `GIMP_PLUGIN_DEBUG=<name>,run` env var [doc, not measured].
- The 2.10→3.0 porting table (removed → replacement) is summarized in 04; the authoritative page
  is developer.gimp.org "Removed Functions" (fetched 2026-09-01, 200+ rows).

## gimp-mcp-plugin.py as the model persistent plug-in [measured by reading + running]
- Registers three `Gimp.PDBProcType.PLUGIN` procedures under `<Image>/Tools/MCP`; `plug-in-mcp-server`'s
  `run()` starts a socket thread (`127.0.0.1:9877`, `SO_REUSEADDR`, `listen(1)`, 1 s accept timeout)
  and then blocks in `GLib.MainLoop().run()` on the main thread — "required for GIMP API calls (all
  Gimp.* calls go over the wire protocol which needs GLib to dispatch)". Client commands are
  marshalled to the main thread; `exec` runs in a persistent namespace.
- Works headless when invoked from `gimp-console` batch (procedure run via
  `lookup_procedure('plug-in-mcp-server')` + config with `run-mode NONINTERACTIVE`).
- Its known 3.2 fixes (README): `layer.copy()` no args; `gimp-blend` gone → `gegl:linear-gradient`;
  `Gimp.text_fontname` gone; `fonts_get_list` returns Font objects; `image.select_none()` gone
  (→ `Gimp.Selection.none`); `get_pixel` returns `Gegl.Color`. Runs its own test suite `run_tests.py` (56 tests) [community].

## Script-Fu 3 [measured + doc]
Batch (one `-b`, everything inside `(begin …)`; a second `-b` is a fresh interpreter → `unbound variable`):
```
gimp-console-3.2.exe -i -d --batch-interpreter plug-in-script-fu-eval -b "(begin (load \"C:/w/lib.scm\") (my-proc \"C:/w/out\"))" --quit
```
Working 3.2.4 Scheme (from the probe, exported a verified 640×360 RGBA PNG at 300 dpi):
```scheme
(define (probe-sf outdir)
  (let* ((img (car (gimp-image-new 640 360 RGB)))
         (lay (car (gimp-layer-new img "bg" 640 360 RGBA-IMAGE 100 LAYER-MODE-NORMAL))))
    (gimp-image-insert-layer img lay 0 0)
    (gimp-context-set-foreground '(255 128 0))                 ; colours are 0–255 lists in Script-Fu
    (gimp-drawable-edit-fill lay FILL-FOREGROUND)
    (let* ((font (car (gimp-font-get-by-name "Sans-serif Bold")))
           (tl (car (gimp-text-layer-new img "SCRIPT-FU 3" font 72 UNIT-PIXEL))))
      (gimp-image-insert-layer img tl 0 0)
      (gimp-text-layer-set-color tl '(255 255 255))
      (gimp-layer-set-offsets tl 40 120))
    (gimp-message (string-append "layers: " (number->string (vector-length (car (gimp-image-get-layers img))))))
    (gimp-file-save RUN-NONINTERACTIVE img (string-append outdir "/sf_out.png") "")   ; file as STRING, options as ""
    (gimp-image-delete img)
    (gimp-message "SF-DONE")))                                 ; prints 'script-fu.exe-Warning: SF-DONE'
```
- Every PDB call returns a **list**; take `(car …)`. Object arrays come back as vectors
  (`vector-length`). `(gimp-version)` returns a string, so `(number->string (car (gimp-version)))`
  errors (`atom->string: bad base`).
- Constants: `RGB`, `RGBA-IMAGE`, `LAYER-MODE-NORMAL`, `FILL-FOREGROUND`, `UNIT-PIXEL`,
  `RUN-NONINTERACTIVE`, `CHANNEL-OP-REPLACE` … (enum nick upper-cased with dashes).
- Registration for installed scripts: `script-fu-register-procedure` (general) and
  `script-fu-register-filter` (image filters, gets `GimpProcedureDialog` UI for free);
  `script-fu-register` is deprecated [doc: 3.0 release notes]. Stock examples in
  `share\gimp\3.0\scripts\` (`drop-shadow.scm`, `round-corners.scm`, `addborder.scm`).
- `gimp-script-fu-interpreter-3.0.exe` is for **independent** `.scm` plug-ins placed in a
  plug-ins folder with a shebang `#!/usr/bin/env gimp-script-fu-interpreter-3.0`; it "must be run
  by GIMP" and does nothing from a shell [measured + doc Script-Fu Tools].
- Server [measured]: `(plug-in-script-fu-server #:run-mode 1 #:ip "127.0.0.1" #:port 10008 #:logfile "C:/w/sf.log")`
  (named `#:` args — positional works with a deprecation warning). Protocol: send
  `G` + 2-byte BE length + script; receive `G` + 1-byte error (0/1) + 2-byte BE length + text.
  Definitions persist across requests and reconnects; PDB calls (`gimp-image-new` …) work
  headless. Details and byte samples in 02 §D. Text console `--batch=-` is Unix-only [doc].
- `-b` snippets, by contrast, share nothing (fresh interpreter each) [measured].
- Errors: `batch command experienced an execution error: Error: …`, exit 70; `(gimp-quit 0)` in
  batch crashed `script-fu.exe` and hung the host — use `--quit`.

## Stock PDB scripts/plug-ins still useful from batch [dump: present]
`script-fu-drop-shadow`, `script-fu-perspective-shadow`, `script-fu-round-corners`, `script-fu-addborder`
(Scheme, need gradients unless `-d` filtered them — the `INFO: … ignored` lines), `plug-in-screenshot`,
`plug-in-plug-in-details`, `plug-in-unsharp-mask`, `plug-in-gauss`, `plug-in-autocrop-layer`. Prefer the GEGL
DrawableFilter for shadows/blur (no gradient/palette dependencies, measured fast).
