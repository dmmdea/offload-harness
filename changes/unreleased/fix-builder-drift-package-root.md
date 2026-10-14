### Fixed - builder drift guard: a package root given as the templates directory is refused, with the path to pass

- **The mistake.** `check --templates-dir <pkg>`, with `<pkg>` the package root (the folder that holds `templates/`)
  instead of `<pkg>/templates`, matched no template file and printed one `template X is not in
  comfyui-workflow-templates-json` failure per guarded template, each ending `installed unknown`. Those lines blame the
  package, not the argument, and the runbook command is easy to mistype that way. A reviewer hit it against the
  published package.
- **What happens now.** `--templates-dir` and `COMFY_TEMPLATES_DIR`, for `check`, `explain` and `record`: a directory
  that has no top-level `.json` file but holds a `templates/` folder exits 2 (the code every other usage error uses)
  with `... is the package root ...; pass <dir>/templates`, before any template or manifest is read or written. It is
  refused, not followed into `templates/`, because the manifest records which directory was read. A correct templates
  directory, a missing or empty directory, a directory that holds template files next to a folder called `templates`,
  `--comfy-dir` and `COMFYUI_DIR` behave as before. The runbook in `render/README.md` and the builder drift section of
  `docs/systems/media-generation.md` say which folder to pass.
- **Tests.** Red first: with a package root, `check` exited 1 with one such failure per guarded view. Six mutants each turn a
  test red: the detection removed, the no-top-level-`.json` clause dropped, the folder test dropped (a file called
  `templates` counted), the flag unguarded, the environment variable unguarded, and the message without the path.
