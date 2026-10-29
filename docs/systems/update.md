# Update verb

## Purpose

`local-offload update` (`update_cmd.go`, engine in `internal/update/`, floors in `internal/floors/` and
`setup/floors.json`) moves every harness component to the newest complete build its publisher has released,
without a version number anywhere in the repository. It resolves the newest build from the publisher, verifies the
publisher's own digest, stages it, promotes it as a write-once generation behind the component's existing live path,
keeps the previous generation, rolls back on failure, and records everything in `installed.json` (schema 2). Run
with nothing new upstream it is one conditional HTTP request and a no-op: updating every five minutes does not
re-hash, re-download or re-extract anything.

Design: `docs/specs/2026-10-28-frontier-updates-design.md` (5.1, 5.2, 5.3, 5.5 and the Rulings). The single-file
swap (llama-swap) is the proven `node-swap` engine ([node-swap.md](node-swap.md)); the verb adds the generation
layout for directory components on top.

## Questions this doc answers

- How do I update llama.cpp, llama-swap, whisper.cpp, stable-diffusion.cpp or HyperFrames on a node?
- How do I move the installed ComfyUI node packs to the newest head of their repository, and what undoes a bad one?
- What does the live path become, and why do my yamls and tasks not change?
- What does a no-op cost, and what makes a candidate "complete"?
- What happens when a build fails its checks, or fails after the flip?
- How do I roll back, and how do I repoint a hand-kept yaml that names a build directory?
- Why does a running MCP server still show the old harness after an update?

## Usage

```
local-offload update [--component llama|llama-swap|whisper|sdcpp|hyperframes|comfy-packs|all] [--channel nightly|release]
                     [--dry-run] [--json] [--resolve <component>] [--rollback <component>]
                     [--repoint <yaml> --component llama [--from <dir>]...] [--live <path>] [--home <dir>] [--no-gate]
```

| flag | what it does |
|---|---|
| `--resolve <c>` | prints the candidate (id, every required asset, its digest) and exits; changes nothing |
| `--dry-run` | prints the plan (installed, resolved, action) without downloading; the manifest is not written |
| `--rollback <c>` | returns one component to its previous generation (an operator decision: nothing is marked rejected) |
| `--repoint <yaml>` | rewrites a hand-kept llama-swap yaml to the live llama path, see below |
| `--from <dir>` | with `--repoint`: a llama build directory whose name carries no build number (`/opt/llama-vulkan`), rewritten like a build-named one; repeatable |
| `--channel` | `nightly` (default: the newest complete `b<N>`) or `release` (the build `nightly-tag.txt` names); `update.channel` in `config.json` sets it per node |
| `--no-gate` | skips the side-port smoke gate (see Gate); prints that it did. The offline self-check and the post-flip watch still run |
| `--component` | one component, or `all` (the default). `all` updates what the node runs: a component with no record in `installed.json` and nothing at its live path is reported `not-installed` (exit 0, nothing written) and left alone. Naming it (`--component sdcpp`) is the only way the verb installs a component a node does not have. `comfy-packs` is never part of `all`; see Node packs |
| `--live` | the live path of the one selected component, for a box whose layout is not `<home>/<component>`. The verb records it, so later runs need neither the flag nor a config entry |
| `--home` | the install root `<home>`. Default: `$OFFLOAD_HOME`, else `update.home` in `config.json`, else the nearest folder that holds the installer's `installed.json` among the running binary's folder and the three above it (the Linux installer's `<home>/bin`, the Windows installer's `<home>/harness`, a clone built inside the root; this is how the fleet registry's ssh call, with no environment, finds it), else `~/offload-stack` when it holds an `installed.json`. No root found is exit 2 with nothing changed: the verb never starts a fresh install in a guessed folder. It prints the root it chose and where that came from |

Exit codes: **0** no-op, updated, skipped (no prebuilt for this node), inconclusive (a lease or a busy seat: try
again) or busy (another run holds `update.lock`); **1** a candidate was rejected or rolled back (loud, with the reason);
**2** an error (a download that did not verify, an unreadable manifest, a swap that could not start).

## Layout

The component's live path keeps its name, so no rendered yaml, scheduled task or probe changes. The live path is, in
order: `--live` (one component), `update.paths.<component>` in `config.json`, the `live` path `installed.json` recorded
the last time the verb worked on the component, else `<home>/<component>`. Every run that finds the live build records
its path, so a node laid out by hand (llama-swap in `C:/llama-swap`) is told once and found from then on. A record
written before `live` existed still finds a single-file component through its current generation's path when that is
a file.

```
<home>/installed.json                       node-local truth, replaced atomically (tmp + rename)
<home>/update.lock                          one update at a time (busy: exit 0 with a message)
<live>                                      a junction (Windows, mklink /J, no privilege) or symlink (Linux) to
<live>-versions/<id>/                       write-once after promotion; <id> is the upstream's own id (b<N>, v<N>, ...)
<live>-versions/.staging/<id>.<rand>/       download + extract + self-check; deleted on every outcome
<live>-versions/<id>.meta.json              source, assets and digests, attestation, size, checks, gate numbers
```

A live path that is a real directory on the first run is migrated: renamed to `<live>-versions/<id>` (the id from
`installed.json`, else from the live build's own version probe, else `unknown-YYYYMMDD`) and replaced by the junction. Models running from it
are unloaded first through llama-swap (with drain), because Windows will not rename a directory a process holds.
Exactly two generations are kept, current and previous; everything else, and every rejected candidate, is deleted at
once.

A build placed by hand is adopted the same way. Point `update.paths.<component>` (or `--live` once) at the directory
the seat already runs from, and the next `update --component <c>` migrates that directory and puts the newest build
behind a junction in its place; the yaml keeps its path, so nothing is re-rendered. The tier profiles render the stt
seat at `<home>/build/whisper.cpp/build/bin`, and on a Windows node that is where whisper-server was copied by hand:
with `update.paths.whisper` set to that directory the verb owns it (measured 2026-10-28 on `<node-g>`: the copy
hashed identical to upstream b5454, moved to `bin-versions/unknown-20261028`, and the junction now points at
`bin-versions/b5454`; a fresh transcription read back correctly). A Linux GPU node keeps its source-built whisper,
because the publisher ships no Linux GPU build to adopt.

**llama-swap has two layouts, decided once** in `internal/update/layout.go` (`layoutOf`); everything around the swap
(preflight, staging, self-check, drain, daemon stop, restart, verification, rollback) is the same code for both:

| layout | when | what a swap does | previous generation |
|---|---|---|---|
| generations | `<dir>` (the folder of the live file, normally `<home>/llama-swap`) is a junction or symlink into a `*-versions` folder, the installer's layout; or the live path is exactly `<home>/llama-swap/llama-swap[.exe]` in a plain folder (migrated into `<home>/llama-swap-versions/<id>` at the flip, with the daemon stopped) | the build is staged as `<dir>-versions/<id>/llama-swap[.exe]` with `<id>.meta.json` beside it, the daemon is drained and stopped, the link is flipped, the daemon restarted, `/running` and `-version` verified; a failure flips back and restarts | the old generation directory stays; `--rollback llama-swap` flips back and restarts |
| file | any other path, for example a hand layout (`update.paths.llama-swap` or `--live` names `<somewhere>/llama-swap.exe`) | the file is swapped in place: hash verify, idle wait, stop, backup by rename to `<file>.bak-<id>`, restart, verify | the `<file>.bak-<id>` file |

The daemon is drained through llama-swap's own unload (never killed while a seat is busy), stopped through the box's
own mechanism, and restarted by it: the scheduled task `llama-swap-native` on Windows, `systemctl restart
llama-swap.service` elsewhere. `update.restart.llama-swap` (`task`, `command` or `none`) overrides it. The mechanism is
probed before anything is drained or stopped (the task exists, the unit answers `systemctl cat`, the command's program
resolves); on Windows, when `llama-swap-native` does not exist but exactly one scheduled task runs the live
`llama-swap.exe`, that task is used and named. A mechanism that is not there skips the component with "set
update.restart.llama-swap" (exit 0, nothing touched): a daemon is never stopped that cannot be started again. The swap engine is
`node-swap`'s (`nodeswap.Plan.Generation` replaces its rename-and-copy steps with the link flip; the process is found by
the real path of the generation's executable, which is what the OS reports for a process started through a link).
`update --dry-run` prints which layout it found.

## Algorithm

1. **Lock** `<home>/update.lock`. Busy: exit 0, say so. A lock whose process is gone, or older than three hours (a
   pid reused after a crash or a reboot), is taken over.
2. **Resolve** from the publisher (GitHub releases, `per_page=10`, paged; npm `latest`) with `If-None-Match` from
   `installed.json`. A `304`, a candidate equal to the installed id, a candidate in `rejected[]` or older than what is
   installed: write `checked_at`, exit 0. That is the five-minute no-op. A candidate is **complete** only when every
   required asset for this node is present, uploaded, with a non-empty `sha256:` digest; otherwise the verb steps back
   one tag. The required-asset sets are a table in `internal/update/resolve.go` with patterns (`<TAG>`, `<N>`,
   `<SHA7>`) and no version number. The **build family** (`cuda13`, `cuda12`, `vulkan`, `cpu`) is chosen per
   component: `update.variant.<component>` in `config.json`, else the family of the build at the live path (a
   `ggml-cuda` or `ggml-vulkan` library beside its program), else the node's backend. A node can run one component
   in another family than the rest: the maxwell-3 tier serves llama.cpp on CUDA and sd.cpp on Vulkan, the only family
   measured to render on that card, and resolving its sd.cpp from the node's CUDA backend would fetch a build with no
   code for the card. The CUDA family is the driver's CUDA major (13.x takes the cuda-13.4 assets, anything else
   cuda-12; a card below compute 7.5 is always 12), else the backend `installed.json` records.
3. **Floor** (`setup/floors.json`, embedded in the binary): a candidate under a floor is never selected, and the
   verb never downgrades. A floor only ever rises; a ceiling or an exact pin is not representable (ADR 0099).
4. **Download** to `.staging`, check size and the publisher's sha256 (the file is deleted on a mismatch), cross-check
   the publisher's checksums file when there is one, run `gh attestation verify` when `gh` exists (fail closed when it
   is present and fails; "none published" only for the publisher's own "no attestations found" or a 404), extract
   (zip-slip safe, a lone wrapper directory is lifted). The attestation result is a line of the report. It only ever
   ratchets: when the generation now serving was `verified`, a candidate whose publisher no longer attests ("none
   published") is rejected, and one whose check could not run (`gh` missing or signed out) is inconclusive.
5. **Offline self-check**: the program starts (`--version` exits 0). For llama, every `--flag` the rendered
   llama-swap yaml passes to this binary must appear in the staged `llama-server --help`; an unknown flag rejects the
   candidate before any seat moves.
6. **Gate** (`Gate` interface, `internal/update/gate.go`; llama only, see Gate below): the side-port smoke under the
   GPU lease, measured back to back against the previous generation. A lease that cannot be had in time is
   "inconclusive", never a rejection.
7. **Promote**: rename staging to `versions/<id>`, flip the junction or symlink (Linux: `current.tmp`, then rename
   over), rotate `previous`, prune the generations that are neither current nor previous. Pruning deletes only what
   this verb or the installer made (a directory with a sibling `<id>.meta.json`), only inside the live path's own
   `<live>-versions` folder, and never the directory the live link points at, whatever the record says; a hand-placed
   build without a meta file stays. The flip never leaves the live path missing: on Windows the new junction is made
   as `<live>.new` first, the old one renamed aside and back on a failure. llama and whisper: llama-swap is
   asked to unload (with drain, through its own client, keep-set members left alone and named) the models that run from
   the live path, so the next load uses the new build, and for llama the first real load is watched (Gate below).
   llama-swap: drained, then swapped and restarted through
   `node-swap`.
8. **Rollback** on any failure after the flip: re-point to `previous`, record `rejected{id, reason, ts}` in
   `installed.json`, exit 1. A rejected id is never retried; a newer id is a fresh candidate.
9. **Write** `installed.json` and print the generation table.

`installed.json` schema 2 keeps every top-level key the installer wrote (profile, backend, accelerators, ...) and adds
`schema: 2` and, per component, `{current, previous, rejected[], etag, checked_at, floor, dest?}` where a generation is
`{id, path, digest, size, source, installed_at}` (plus `asset` and, for single files, `file_sha256`). The verb writes
that shape and nothing else.

**The installer's records** (`setup/install.ps1`) are flat, `components.<key> = {version, digest, size, source,
installed_at, dest?, staged?}`, keyed by build family (`llama-cuda13`, `llama-vulkan`, `sdcpp-vulkan`) or by
component (`llama-swap`). The verb reads them as follows:

- the flat record is the component's **current generation** (`version` is the id) when the verb has not written an
  entry of its own; a `llama-swap` record is rewritten in the verb's shape on the next save, a build-family record is
  left as the installer wrote it; `dest` is carried through. A bare version string (the old schema 1) is not trusted:
  that entry is bootstrapped by probing the live build's `--version`.
- the **live path's own meta file** settles a disagreement: when the live link points at `<versions>/<g>` and
  `<g>.meta.json` names a tag other than the record's, the record follows the link (the old current becomes the
  previous generation while its directory is still there).
- a **staged** generation (`staged: "<id>"`: the installer found a process running from the live directory, put the
  build in `<live>-versions/<id>` and did not flip) is **promoted without a download** when `<id>.meta.json` names the
  newest build's tag and its `digests` equal the publisher's digests for it (the archive is gone after extraction, so
  the generation is trusted through the meta file the installer wrote from the publisher's digests); the offline
  self-check, the gate, the flip and the post-flip checks all still run. A newer published build ignores it (the
  staged directory is pruned as neither current nor previous). `staged` is cleared when the verb promotes.
- the meta files the verb writes carry the installer's `tag`, `digests` and `sizes` fields too, so the installer reads
  the live generation's tag and reuses a generation whose digests match without a download.

A missing entry is bootstrapped by probing the live build's `--version`.

## Gate

For `llama` the gate is `SmokeGate` (`internal/update/smokegate.go`); every other component passes it with
"no smoke gate for <component>". It runs after the offline self-check and before anything moves:

1. It takes the node's GPU lease the way `gpu reserve --wait --drain --unload-seat` does (the verb re-runs itself as
   the wrapped command of that wrapper, so the queue, drain, unload, heartbeat, warm-back and release are the
   binary's own). A lease or drain that does not complete within `update.gate.lease_wait` is **inconclusive**: nothing
   changes, exit 0, the next run tries again. It is never recorded as a rejection. `update` takes its own lease for the
   gate, so it is run bare. Run inside `gpu reserve`, it works only when that reserve was given `--drain --unload-seat`:
   the gate then measures under the caller's lease (it logs "running under the caller's GPU lease"). Under a lease that
   did not drain and unload, the gate answers inconclusive at once instead of queueing behind its own parent for
   `lease_wait`, and says what to change.
2. It picks the node's seat: among the chat seats of the rendered llama-swap yaml that run from the live path
   (embedding and reranking seats cannot chat), a resident one (`ttl` 0 or below), else the one with the smallest
   model file. The seat's own command line is used, macros expanded, with `--host 127.0.0.1 --port <side port>`.
   With no usable yaml or seat the gate passes ("no seat to smoke"); the offline self-check still ran.
3. It starts the **staged** `llama-server` on the side port, waits for `/health` (`update.gate.load_timeout`), sends
   one `/v1/chat/completions` ("Reply with the single word: ready", `max_tokens` 32) and three timed requests of a
   fixed long prompt (`cache_prompt` false, 64 tokens), takes the median `timings.prompt_per_second` and
   `timings.predicted_per_second` and reads VRAM from `nvidia-smi --query-gpu=memory.used` (steady state, less the
   baseline read before the server started), then stops the server. It does the same with the **previous**
   generation's `llama-server`. The server runs on the card like the seat it copies; it is always stopped.
4. Verdict: **reject** when the candidate does not load or does not answer `/health` in time, answers with a status
   other than 200 or an empty text, decodes or prefills more than 20 percent slower than the previous generation, or
   uses more than 10 percent more VRAM (and more than 256 MiB more, so desktop noise is not a regression). The reason
   names the number. Otherwise **pass**. A previous generation that cannot be measured, or an empty `PreviousDir`,
   leaves load and output as the whole test. A rate the build does not report is not compared.

The numbers are recorded in `<live>-versions/<id>.meta.json` as `gate_numbers`: `cand_load_s`, `cand_prefill_tps`,
`cand_decode_tps`, `cand_vram_mib`, `prev_load_s`, `prev_prefill_tps`, `prev_decode_tps`, `prev_vram_mib`, and printed
under the generation table.

**Post-flip watch.** After the flip llama-swap is asked through its own API to unload (with drain) the seats that run
from the live path, never a kill. The verb then sends one short completion for the same seat through llama-swap
(which loads it on the new build), checks that llama-swap lists the seat running from the live path and that the
seat's `/health` through llama-swap is not failing. The watch runs after the gate released its GPU lease, so both
requests go through the fence (`modelaffinity.AwaitModelRoute` and `AwaitUpstream`): while a render holds the cards
the load waits for the fence to lift, never loads onto a fenced card, and a fence still up at the timeout is
inconclusive. The gate's own side-port requests are not llama-swap's and need no fence (`internal/update/sideport.go`). A failure within `update.gate.post_flip_timeout` rolls back with
the reason "post-flip load failed". Nothing to watch (no endpoint, no chat seat, llama-swap down) is a note.

A failure that says nothing about the build is **inconclusive**, not a rejection: a timeout, an out-of-memory answer
(another process holds the card), a seat that stayed busy through the drain. In the gate nothing is promoted; after the
flip the live path goes back to the previous build. Either way the id is not added to `rejected[]` and the next run
tries it again. A crash, a wrong or empty answer, an unknown flag or a measured regression is a rejection. The gate's
job file never carries the seat's environment (a yaml's `env` may hold a token): the lease-held child reads the seat
from the yaml again.

## Config keys

All optional; the `update` object of `config.json` is declared in `internal/config` (`UpdateConfig`), so it is a known
key, and read by the verb through `internal/update.FileConfig`. These keys name commands the verb runs and folders it
renames and prunes, so the verb reads its config only from `--config`, `$LOCAL_OFFLOAD_CONFIG` or
`~/.local-offload/config.json`, never a `config.json` in the directory it runs from, and prints the file it used.

| key | default | meaning |
|---|---|---|
| `update.channel` | `nightly` | `nightly` (the newest complete build) or `release` (llama.cpp only: the build the publisher's release-tag file names); `--channel` overrides |
| `update.paths.<component>` | `<home>/<component>`; llama-swap `<home>/llama-swap/llama-swap[.exe]` | a component's live path (`llama`, `llama-swap`, `whisper`, `sdcpp`, `hyperframes`) |
| `update.restart.llama-swap` | the box's own mechanism | `{"task": "<scheduled task>"}`, `{"command": "<shell command>"}` or `{"none": true}` (no daemon stop, drain or restart; the swap is proven by hash and `-version` alone) |
| `update.llama_swap_yaml` | `<home>/llama-swap.yaml` | the rendered llama-swap config the llama flag check and the gate read |
| `update.home` | none | the install root (an absolute path) when neither `--home` nor `$OFFLOAD_HOME` names one, for a node whose binary does not sit under its root |
| `update.variant.<component>` | the live build's family, else the node's backend | the build family a component resolves in: `cuda13`, `cuda12`, `vulkan` or `cpu` |
| `update.gate.*` | see below | the smoke gate and the post-flip watch |

`config.json`, object `update.gate`:

| key | default | meaning |
|---|---|---|
| `enabled` | `true` | `false` turns the smoke gate and the post-flip watch off (`--no-gate` skips only the gate, for one run) |
| `lease_wait` | `30m` | how long to queue for the GPU lease and drain the seats; past it the answer is inconclusive |
| `side_port` | `11499` | loopback port of the gate's `llama-server`; the next free port is used when busy; 80, 443, 3000, 5000, 8000 and 6443 are refused |
| `load_timeout` | `5m` | how long one `llama-server` may take to answer `/health` |
| `post_flip_timeout` | `5m` | how long the first real seat load after the flip is watched |

Thresholds (`MaxSlowdown` 0.20, `MaxVRAMGrowth` 0.10) are constants in `smokegate.go`, Ruling R6 of the design.

## Repoint

`update --repoint <yaml> --component llama` is for a hand-kept yaml that names a build directory (`llama.cpp-b<N>`,
`llamacpp-b<N>`, `llama-b<N>`), or a directory named with `--from` (a build kept under a name with no build number,
such as `/opt/llama-vulkan`; matched only as whole path components, so `/opt/llama-vulkan-b9934` is left alone). It
backs the file up to `<yaml>.bak-<yyyyMMdd-HHmmss>`, replaces every occurrence with the live path and drops a `bin`
segment right after it when the live build has no `bin` folder (the publisher's archives are flat; a source tree
keeps its programs in `build/bin`, so `llamacpp-b11669/bin/llama-server` becomes `<live>/llama-server`). On Linux it
sets any other `LD_LIBRARY_PATH=` in a repointed block to the live path's `bin` directory (the live path itself when
it has none); a line it already rewrote keeps the rest of its list (`:${LD_LIBRARY_PATH:-}`). It prints a diff
summary and never re-renders. Comment lines are left alone. It refuses (exit 2, nothing written) while the live path
is not there: install or copy the build first. Run it on a
copy when llama-swap runs with `-watch-config` (it reloads on every write), and swap the copy in at a quiet moment.

## Node packs

`--component comfy-packs` is the one component that does not use the generation layout and is not part of `all`. The
work is `render/comfy-packs-update.mjs`, which the verb starts with the `node_path` and `comfy_dir` of the node's config
and resolves beside `run_graph_script` (the render directory the binary already uses for run-graph). The verb takes no
install root and no `update.lock`, and relays the script's report and exit code: with `--json` it prints the script's one
JSON line (`{component, mode, dry_run, action, detail, exit, packs[], notes[]}`, one `packs[]` row per pack with
`installed`, `resolved` and `action`) and not the array the other components print. `--resolve comfy-packs` and
`--dry-run` are the same read-only run: they list each installed pack, the head its repository names and what would happen.
`--rollback comfy-packs` moves each pack back to the `previous` commit of its record, gate included. Run it under the lease
wrapper, because the gate starts ComfyUI's own import pass, which initializes torch (the verb does not take the lease
itself):

```
local-offload gpu reserve --wait <window> --drain --unload-seat --for <window> --reason "node packs" -- \
    local-offload update --component comfy-packs --json
```

A lease that is not granted never starts the command: nothing changed, exit 0 by the wrapper, run again later.

The channel is the head of each pack's default branch (`git ls-remote <origin> HEAD`), because the allowlisted publishers
tag and release nothing; the git commit id is the content digest and `render/node-packs.json` is the trust boundary. A
render request never runs this: it clones an absent pack and uses an installed one as it is. Per installed pack whose
`origin` is a row of that file (one trailing `/` and one trailing `.git` are ignored on both sides of the comparison;
nothing else is, so case, scheme and host must match exactly):

1. The head is the checkout's HEAD, or is listed in the pack's `rejected[]`: nothing to do, `checked_at` is written.
2. A tree with modified tracked files, or whose HEAD is not an ancestor of the remote head (local commits, a branch of its
   own), is reported `kept` and never touched, never reset. Untracked files do not count (Python writes `__pycache__`
   into packs). If a ComfyUI answers on its port the run is `refused` (exit 0, nothing changed): the pack set cannot change
   under a live instance.
3. The exact commit is fetched and `FETCH_HEAD` must be it; what is gated is what is promoted.
4. `pip freeze --all` and `pip check` are taken as the baseline and, with each moved pack's `{name, dir, from, to}`, written
   atomically to `custom_nodes/.packs-update-journal.json` before the first `git merge --ff-only` moves a pack (the journal is
   updated as packs move).
5. One unified resolve over every installed allowlisted pack, under the host-torch constraints read from the live venv
   (the satisfier's own `resolveDeps`, so conflicts between packs surface).
6. Gate: `pip check` no worse than the baseline, the host constraints (torch, torchvision, torchaudio, numpy) unmoved, and
   `python main.py --quick-test-for-ci` in the ComfyUI directory exiting 0 with no `(IMPORT FAILED)` line for a moved pack in
   its "Import times for custom nodes" block.
7. Pass: `custom_nodes/<name>.installed.json` (schema 2) gets the new commit and `previous` = the commit held before, and
   the venv's `.offload-deps-satisfied` marker is written, and the journal is deleted. Fail: `git reset --keep` to the previous
   commit (see below), the pip snapshot
   put back with `--no-deps` (only the packages that differ), freeze and `pip check` required to equal the baseline, the
   failed commit appended to `rejected[]` of the pack that failed (all moved packs when nothing names one) together with a short
   sha256 of the venv's `pip freeze` (a rejection is ignored once the freeze has changed, so a fixed venv is not locked out
   of the commit), exit 1, and the journal is deleted. A
   pack moved in the same run whose own import was fine is put back too but not marked rejected, and is tried again alone
   next run. A restore that does not reach the baseline is exit 2 and says which packages differ.

A remote that cannot be read (including a private mirror without credentials), a fetch that failed, a quick test that timed
out or found the card busy, and a resolve that lost the network are **inconclusive**: whatever moved is moved back, nothing
is marked rejected, exit 0, and the next run tries again. A quick test that exits non-zero while its output names no moved pack in an `(IMPORT FAILED)` line (code
`QUICK_TEST_NO_PACK_NAMED`) is inconclusive too: it says nothing about the pack. A rollback is an operator decision and marks nothing rejected,
so a later update moves the pack forward again to the repository's head. `--dry-run` runs `ls-remote` and the
tree checks but fetches nothing, so a head it has not seen reads `update-available` and the ancestor test is made at fetch.

**A run that was killed.** The journal is the rollback state on disk. A start that finds one undoes it first: each listed pack
that still sits at the commit the run moved it to goes back to `from`, the pip baseline is restored and `pip freeze` is
required to equal it, the report says what was recovered (`notes[]`), and then the run plans as usual. A pack that sits at
neither commit (someone moved it since) is left alone. `--dry-run` only reports the journal. SIGINT, SIGTERM and SIGBREAK
(where the OS delivers them) run the same undo and end the run with exit 2; a hard kill leaves the file for the next start.
The script detects and logs; it never refuses to run because of it.

**Edits made while a run is under way.** Every reset (the undo, the journal recovery and `--rollback`) first re-reads
`git status --porcelain --untracked-files=no` and uses `git reset --keep`, never the hard form. A pack with tracked
modifications at that moment is not touched: it is reported `kept` with code `DIRTY_AT_UNDO` naming the pack, stays at the
commit it holds, is not marked rejected, the rest is put back and the venv restored, and the run ends exit 2 (the journal
then lists only that pack, so the next start looks at it again).

## ADR 0085 notice

A running MCP server keeps the harness image it started with. This verb updates engines and the llama-swap daemon, not
the harness executable; an MCP server started before an update keeps serving from its old image until it is restarted.

## Interfaces and entry points

| piece | where |
|---|---|
| verb | `update_cmd.go`, wired in `main.go` |
| engine | `internal/update/` (`resolve.go`, `fetch.go`, `selfcheck.go`, `layout.go`, `generations.go`, `single.go`, `gate.go`, `smokegate.go`, `repoint.go`, `manifest.go`) |
| layout decision, link flip, staged generation | `internal/update/layout.go` |
| floors | `setup/floors.json`, parsed by `internal/floors` |
| single-file swap | `internal/nodeswap` (`Preflight` lets the verb refuse before it downloads) |
| GitHub token | `GH_TOKEN`, `GITHUB_TOKEN`, else `gh auth token` (account via `OFFLOAD_UPDATE_GH_USER`); never stored |
| node packs | `internal/update/comfypacks.go` (starts the script, relays its exit code), `render/comfy-packs-update.mjs` (the update, gate and rollback), `update_cmd.go` (`runUpdateComfyPacks`) |
