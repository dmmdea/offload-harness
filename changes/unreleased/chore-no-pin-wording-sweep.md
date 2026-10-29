### Changed - the last "pinned" wording and version ranges are gone, and a guard keeps them out

- **What changed.** The HyperFrames install, the browse sidecar, sd.cpp and llama.cpp were still described as pinned in
  Go comments, docs and two client-visible strings, though ADR 0099 made every one of them "the newest complete build,
  floored". The `offload_browse` tool description and its tools-profile sentence now say "a sidecar" instead of "a
  pinned sidecar", and the compose binding label in route and doctor output reads `hyperframes_dir (project-local CLI)`
  instead of `hyperframes_dir (pinned CLI)`. `integrations/opencode/package.json` takes `latest` for its two ranges, the
  reproduction recipe in the hardware-scope spec downloads the newest llama.cpp release, and the chatterbox setup text in
  `render/README.md` installs torch without a version. The llama-swap README no longer names the surface version the
  CLI was verified against; it points at the vendored schema snapshot that carries it.
- **The guard.** `no_version_pins_test.go` now also reads every tracked `package.json` (a dependency must be `latest`,
  `*`, empty or a non-registry spec; an exact version, a `~` or `^` range and an x-range fail) and scans tracked live
  text (`*.md`, `*.mjs`, `*.js`, non-test `*.go`, `*.ps1`, `*.sh`, `*.py`) for an install command that names a version:
  `pip install` / `uv pip install` / `uv add` with `==`, `npm install` and `npx` with `@<digit>`, `git clone --branch v<digit>`
  and `gh release download b<digits>`. History paths are skipped. A hit names `file:line` and says "ADR 0099: floors
  never pins".
- **Check.** `go test -run NoVersionPins .` fails on the old `package.json` ranges and the old recipe and passes on the
  tree.
