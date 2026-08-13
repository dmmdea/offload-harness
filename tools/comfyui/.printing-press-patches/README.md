# Local patches to the generated tree

Deviations that live in this vendored copy of `comfyui-pp-cli` rather than upstream in the
Printing Press. See [`docs/systems/printed-clis.md`](../../../docs/systems/printed-clis.md) for the
adoption rules.

**Each entry here is a debt, not a feature.** A patch survives only until the next reprint
regenerates the tree, so every one of these must also be fixed upstream — otherwise the reprint
silently reverts it. Record the upstream status in the entry.

| Patch | Fixes | Upstream status |
|---|---|---|
| `0001-cross-platform-host-path-handling.patch` | `internal/comfy/media` was Windows-only | **Not yet upstreamed** — see below |

---

## 0001 — cross-platform host-path handling

**Applied:** 2026-08-13, during the initial vendoring (PR #101).

### What was wrong

`StagedName` and `ValidateComfyFilename` both handle *host* paths — foreign input that may have been
written on a different operating system than the one reading it. Both delegated to Go's `filepath`
package, which honors **only the running OS's separator**:

- `filepath.Base(` `` `D:\refs\portrait.png` `` `)` returns the whole string on Linux, so the staged
  name became `D_refs_portrait-<sha>.png` instead of `portrait-<sha>.png`.
- `filepath.ToSlash` is a no-op on Linux, so `\\server\share\x.png` kept its backslashes and the UNC
  and `..` traversal checks never fired.

### Why it mattered beyond a red test

Two real defects, not cosmetic ones:

1. **`StagedName`'s core guarantee broke across a mixed fleet.** Its own doc comment calls the
   content hash "load-bearing": identical bytes must stage under one name, or an archived run's
   provenance splits. On Linux, the same file referenced by a Windows-style path produced a
   *different* name than on Windows — the exact collision the design set out to prevent.
2. **`ValidateComfyFilename` failed OPEN on every non-Windows node.** Its whole job is to reject host
   paths (absolute, drive-qualified, UNC, `..` escapes) before they reach `LoadImage`. On Linux it
   silently stopped rejecting UNC and backslash-separated traversal.

This was invisible until now because the CLI was generated, verified, and scored on Windows only.
Vendoring it into a repo whose CI runs `ubuntu-latest` is what surfaced it.

### The fix

Handle both `/` and `\` explicitly, independent of `runtime.GOOS`:

- new `hostBase()` replaces `filepath.Base(filepath.FromSlash(...))` in `StagedName`, splitting on
  either separator and stripping a `D:`-style drive qualifier;
- `ValidateComfyFilename` normalises separators with `strings.ReplaceAll(trimmed, "\\", "/")`
  instead of `filepath.ToSlash`.

Behavior on Windows is unchanged — `filepath` already did this there. Only the non-Windows path
changes, from wrong to correct.

Also in this patch: `media_test.go` had a fixture path containing a real local username
(`/home/<user>/refs/portrait.png`), replaced with `/home/user/...`. This repository is public and its
[docs/STYLE.md](../../../docs/STYLE.md) privacy rule forbids filesystem paths that leak an identity —
in code as well as docs.

### Verification

All previously failing subtests pass on real Linux (cross-compiled test binary executed under WSL2,
`uname -s` = `Linux`): `TestStagedName/windows_path`, `TestValidateComfyFilename/unc_path`,
`TestValidateComfyFilename/unix_absolute`, and the identical-content-one-name assertion. Windows
remains green.

### Upstream

**Still owed.** The durable fix belongs in the CLI Printing Press generator/templates that emitted
these two functions, so the next `/printing-press-reprint comfyui` carries it instead of reverting
it. Until that lands, re-apply this patch after any reprint and re-run the Linux check.
