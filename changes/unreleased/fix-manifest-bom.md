### Fixed - `installed.json` is written without a BOM, and the Go readers tolerate one

- **What it does.** Both manifest writes in `setup/install.ps1` (the per-component record and the final
  version manifest) go through a BOM-less UTF-8 writer, the way the config write beside them already
  did. `ReadInstalledInfo` (the reader behind `local-offload report` and the fleet node's health)
  strips a leading BOM before decoding, so a manifest written by an earlier installer under Windows
  PowerShell 5.1 still reads. The installer's own two reads of the manifest (the component record it
  carries forward, the previous manifest on a re-run) decode UTF-8 explicitly: Windows PowerShell 5.1
  reads a BOM-less file as the ANSI code page, so a `data_home` with a non-ASCII character would have
  round-tripped as mojibake on every re-run.
- **Why.** `Set-Content -Encoding UTF8` writes a BOM under Windows PowerShell 5.1 and none under
  PowerShell 7. The reference boxes install under PowerShell 7; the first box installed under 5.1 (an
  RTX 5080 desktop, 2026-11-04) got a manifest the Go side rejected with
  `invalid character 'U+FEFF' looking for beginning of value`, so `local-offload report` printed
  `hardware tier: UNKNOWN` on a box the installer had just classified `blackwell-16`, and the fleet
  node's health carried no profile.
- **Check.** On that box, the same manifest with the BOM removed: `report` prints
  `hardware tier: blackwell-16`.
