---
status: Proposed
date: "2026-10-09"
---

# The embeddinggemma2 entry may run from a second llama.cpp build

## Context

0.175.0 added the `embeddinggemma2` entry (EmbeddingGemma-2, a second memory-stack embedder) to ten of the eleven serving
templates (the Rockchip board's has none). Its GGUF architecture, `gemma-embedding2`, first builds in llama.cpp b11452
(upstream PR 30054); b11490 is the build it was proven on. The render has had one llama.cpp build for every entry since
the Linux renderer existed: `--llama-bin` feeds the one `__LLAMA_BIN__` token, and on Linux the one `ld` loader macro.

The `ampere-6` reference box runs b10964, which the RAM-spill agent seat of ADR 0080 needs and which it was measured on. It must
keep that build for its other seats, and b10964 cannot load the new entry. With one build per render the only way to serve both
was a hand-edited config, and a hand-edited config is the state ADR 0043 exists to end: `audit-yaml` reads it HAND-EDITED
(a stamped file whose body moved) or UNSTAMPED (a file nobody stamped), and the next `install.sh` run re-renders unconditionally
and overwrites the edit. Nothing in the repository stated the b11452 floor to the renderer either: it lived in template
comments and in one PowerShell assertion, so a render on a b10964 directory produced an entry that cannot start.

ADR 0054 already took the shape of the answer for a CPU family: a second build directory (`--llama-bin-cpu`,
`Params.AltCPULlamaBin`), a loader macro the Go renderer inserts on Linux (`ldcpu`), and a basis field that rides the
provenance stamp.

## Decision

1. **`install render --llama-bin-eg2 <dir>` points the `embeddinggemma2` entry, and only that entry, at a second build.**
   `Params.EG2LlamaBin` mirrors in `ParamsBasis` as `eg2_llama_bin`, omitted when empty, at the same position in both structs.
   Every other entry keeps `--llama-bin`. The name says what it moves: the 300M `embeddinggemma` entry and the reranker, which
   run on the main build, do not follow it.
2. **The mechanism is a block-scoped rewrite inside `servingtmpl.Render`, before substitution, not a new template token.** It
   shares one scanner with the text-only projector strip (`eg2BlockLines`), requires exactly one `__LLAMA_BIN__` in the entry,
   and on a template that defines the loader macro `ld:` inserts `ldembed` right after it and swaps the entry's `"${ld}"` list
   item for it (the item, not the line, because the Vulkan template keeps `"${vk}"`). The path keys on the template carrying
   `ld:`, not on the target OS. A template edit would have moved `template_sha256` for ten templates and turned every stamped
   node STALE one release after 0.175.0 did exactly that; the rewrite leaves unset renders byte-identical and every stamp where
   it was. A changed template shape fails the render naming the entry.
3. **The value is cleaned once, in `deriveRender`, and the stamp records the cleaned value.** Backslashes become forward
   slashes, the trailing slash goes, a value equal to `--llama-bin` is treated as unset (nothing recorded, the render is the
   unset render), and a double quote, line break or `$` is refused (`Params` refuses them too, for any caller). The flag on a tier
   that does not carry `include_embeddinggemma2` is refused naming the tier and the flag; `Render` with the entry off and the
   field set is an error.
4. **The replay carries it.** `replayRequest` gains `EG2LlamaBin` and the sibling `AltLlamaBinCPU`, which it had dropped (a
   node rendered with a CPU family would have replayed without one and read STALE for a path it chose itself; dormant, because
   no tier declares `alt_backends`). A node rendered with the flag audits MATCH; an entry's path edited by hand still reads
   HAND-EDITED, because `audit-yaml` has no per-entry view. Both are inputs the tier must still permit, so a stamp that records
   one the tier has since withdrawn reads STALE with the renderer's refusal as the detail and no key list; the "no longer in
   the tier table" words stay for a tier that really left it (`provenanceOf` tells the two apart).
5. **The b11452 floor is a write-time check, from the directory's name.** `eg2MinLlamaBuild = 11452`; `install render` reads a
   `b<digits>` token (four to six digits) from the build directory's own last path element, or from the nearest element above
   it when that one is the generic `bin` or `build` (`llamacpp-b10964/bin`, `llama-b11490/build/bin`), and refuses a render
   whose entry build is known and below it. It never runs `llama-server`: `--version` initialises every CUDA card, and
   `install render` runs on live nodes. **The check is therefore advisory:** it protects a build whose directory path states
   its build, and for any other (`<home>/llama`, `/srv/offload/build/llamacpp/build/bin`) it cannot tell what the directory
   holds. A name that states no build is a `note:`, on stdout when `--out` is set (the Windows installer captures that stream
   and relays its `note:` lines, except the one about its own pinned build) and on stderr otherwise (stdout is then the
   stamped config). The check reads the entry's own build when the flag is
   set and the main build otherwise, and only for a render that includes the entry. It is never part of the derivation or the
   replay: an audit runs on another machine with another machine's recorded paths.
6. **Installers.** `install.sh --llama-bin-eg2 DIR` passes it through. `install.ps1` gains the opt-in
   `OFFLOAD_EG2_LLAMA_BIN` and downloads nothing: it installs one pinned tag (b11490) for every node it installs, which is
   already past the floor, and a test holds the pin there.

## Consequences

- A node that keeps an older main build serves the entry from a rendered, stamped config instead of a hand edit; the hand splice
  is retired by re-rendering, as ADR 0054 retired the CPU one.
- **Behaviour change with the flag unset:** a render whose main build the directory path states as older than b11452, on a tier
  that carries the entry, is refused where it used to be written. That entry could not start on such a build. A directory whose
  name states no build gets a note, so an install into a directory called `llama` is not stopped by a check that cannot tell.
- `spec_sha256` differs between same-tier nodes with and without the flag, as it already does with `llama_bin`; a fleet check
  compares serving-config state, not the hash.
- A binary older than 0.177.0 auditing a stamp that carries `eg2_llama_bin` drops the key it does not know and reads
  HAND-EDITED. Only a node rendered with the flag is affected, and the cure is to upgrade the binary before re-rendering with it.
- The residents run two llama.cpp builds side by side. > **Unverified:** that a build-consistency check across loaded seats
  reports drift for this shape; read from its source, not run.
- > **Unverified:** whether llama-swap fails the whole residents set when one member cannot start. The floor check stands on its
  own either way, because the entry cannot start on a build below the floor.
- Windows resolves DLLs beside the executable and the entry has no loader macro there, so the second directory must be a
  complete extraction; the check on `llama-server.exe` proves only that the executable is there.

## Alternatives considered

- **A `__EG2_LLAMA_BIN__` template token.** It would have touched ten templates, moved `template_sha256` for every stamped
  node, and still needed Go-side work on the three Linux templates to swap `${ld}`. It also departs from the choice 0.175.0
  made for the projector (a rewrite in the renderer, the templates keeping the authority's entry verbatim).
- **A `profiles.json` key.** The path is a per-box fact, like the vLLM venv; the tier already permits the flag through
  `include_embeddinggemma2`, and a new key would have moved `profiles_entry_sha256` for every tier.
- **Reading the build by running `llama-server --version`.** Rejected for the CUDA initialisation above, and because its output
  format is not pinned anywhere in the repository.
- **Warning instead of refusing below the floor.** Refusing matches the repository's write-time gates and the entry's actual
  failure; the name-based reader keeps the refusal narrow to builds the name states.
- **Leaving it to a hand edit.** The state this ADR retires.

## Related code

- [`internal/servingtmpl/servingtmpl.go`](../../../internal/servingtmpl/servingtmpl.go) — `Params.EG2LlamaBin`, `retargetEG2Bin`,
  `eg2BlockLines`
- [`internal/servingtmpl/provenance.go`](../../../internal/servingtmpl/provenance.go) — `ParamsBasis.EG2LlamaBin`
- [`install_render.go`](../../../install_render.go) — `--llama-bin-eg2`, `cleanEG2Bin`, `eg2FloorCheck`, `llamaBuildOf`,
  `replayRequest`
- [`setup/install.sh`](../../../setup/install.sh), [`setup/install.ps1`](../../../setup/install.ps1)

## Related docs

- [Setup and installer](../../systems/setup-installer.md) — the flag, the floor check, the adoption recipe
- [ADR 0054](0054-dual-route-node-renders-a-cpu-seat-family-beside-its-gpu-seats.md) — the second-build pattern this extends
- [ADR 0080](0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md) — the agent seat that keeps the main build old
