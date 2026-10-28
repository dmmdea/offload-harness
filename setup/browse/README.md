# Browse-lane sidecar

`runner.py` is the Python sidecar behind the opt-in `offload_browse` lane. It drives the operator's
already-running Chromium (Chrome, Edge, Brave or Chromium) toward a natural-language goal using
browser-use's `jev-ultrafast` agent loop and the `browser-harness` CDP client. The sidecar owns the
browser; the harness owns every model call. Both model calls (typed decisions and field text) are
answered by the harness over a JSON-lines stdio protocol, so the sidecar holds no provider key and makes
no network calls of its own.

Safety built into the sidecar: a deny-list removes publish/send/delete/pay-style controls before the model
sees them and rechecks them, including the live element, at execution; a host allowlist ends the run when
the page leaves it; captured traffic has cookies, authorization and token-like headers redacted.

## Dependencies

- `browser-harness>=0.1.13` (a floor: the newest release at install time)
- `jev-ultrafast` from the head of its git default branch (no commit is named)

There is no lockfile: install runs `uv sync`, which resolves the newest versions and verifies each download
against the registry's own hashes.

## Install

```powershell
pwsh setup/browse/install.ps1              # into ~/.local-offload/browse
pwsh setup/browse/install.ps1 -OffloadHome <OFFLOAD_HOME>
```

The script copies the sidecar, syncs the environment, runs the unit tests with the venv python and
prints one JSON line with `browse_python` and `browse_script` (the values for the `browse_*` config keys).

## Tests

```
cd setup/browse
python -m unittest -v test_runner
```

The tests cover the pure helpers and, through fake `Browser` classes, the observe and act wrappers. They need
neither the dependency packages, a browser nor a network. The animation-script and mutation-counter tests run
their scripts under `node` and are skipped when `node` is not on the PATH.

## Protocol

See [../../docs/systems/browse-lane.md](../../docs/systems/browse-lane.md).

## Telemetry

Disabled: the installer runs `browser-harness telemetry disable`, and the sidecar sets `BH_TELEMETRY=0`,
`BROWSER_HARNESS_TELEMETRY=0` and `ANONYMIZED_TELEMETRY=0` before importing either package.
