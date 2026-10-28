### Added - `local-offload update`: every engine moves to its publisher's newest complete build, with generations, a kept previous build and automatic rollback

- **What it does.** A new verb resolves llama.cpp, llama-swap, whisper.cpp, stable-diffusion.cpp and HyperFrames from
  their publishers at run time (GitHub releases, npm), picks the newest build whose every required asset for this node
  is uploaded with a digest (a half-uploaded release steps back one tag), verifies the publisher's own sha256 (and the
  checksums file and `gh attestation verify` when they exist), stages it, runs an offline self-check (the program
  starts; for llama, every `--flag` the rendered llama-swap yaml passes to it must exist in the staged
  `llama-server --help`), and promotes it as a write-once generation. The component's live path keeps its name and
  becomes a junction (Windows) or symlink (Linux) to `<live>-versions/<id>`, so no rendered yaml, task or probe changes.
  Exactly the current and the previous generation are kept; a failure after the flip re-points to the previous one and
  records the id in `rejected` so it is never retried.
- **A free no-op.** The listing is fetched with `If-None-Match` from `installed.json`; a `304`, the same id, a rejected
  id or an older one ends the run with exit 0 and a timestamp, so running it every five minutes downloads, hashes and
  extracts nothing.
- **llama-swap** is swapped through `node-swap` (hash verify, drain of the running seats through llama-swap's own
  unload, stop, swap, restart, verify `/running` and `-version`, rollback); `--rollback <component>` returns to the
  previous generation. The layout is decided once (`internal/update/layout.go`): where `<home>/llama-swap` is the
  installer's junction or symlink into `llama-swap-versions` the build is staged as a write-once generation and the link
  is flipped (rollback flips it back); a hand layout (one file in an ordinary directory) keeps the file swap with the
  previous build as `<file>.bak-<id>`. Both share the drain, stop, restart and verification.
- **Floors, not pins.** `setup/floors.json` (embedded in the binary) holds the minimum build a seat needs; the verb never
  selects a candidate under a floor and never downgrades. No tag, build number or digest literal exists in the code.
- **`installed.json` schema 2** adds `{current, previous, rejected[], etag, checked_at, floor}` per component, written
  atomically; a missing manifest is bootstrapped by probing `--version`. The installer's own flat record
  (`{version, digest, size, source, installed_at, dest?, staged?}`, keyed by build family such as `llama-cuda13`) is read
  as the current generation, `dest` is carried through, and the live path's own `<id>.meta.json` settles a disagreement.
  A generation the installer left `staged` behind a running engine is promoted without a download when its meta file's
  digests equal the publisher's for the newest build (a newer build ignores it); the verb's meta files now carry the
  installer's `tag`, `digests` and `sizes` fields too, so the installer reads and reuses them.
- **Config.** `update` (`channel`, `paths`, `restart`, `llama_swap_yaml`, `gate`) is a declared `config.json` key: no
  more `unknown config key "update"` warning.
- **`--repoint <yaml> --component llama`** backs up a hand-kept llama-swap yaml, replaces every build-named directory
  with the live path and (Linux) points `LD_LIBRARY_PATH` at the live `bin`, without re-rendering.
- **Side-port smoke gate for llama.** Before a staged llama.cpp build is promoted the verb takes the node's GPU lease
  (the `gpu reserve --wait --drain --unload-seat` path, `update.gate.lease_wait`, default 30m), starts the staged
  `llama-server` on a loopback side port with one of the node's own seat command lines, measures load time, prefill and
  decode tok/s and VRAM, then does the same with the previous generation and rejects on no load, an empty or non-200
  answer, a rate more than 20 percent lower, or VRAM more than 10 percent higher. A lease timeout is "inconclusive,
  retry later", never a rejection. The numbers go in the generation's meta file. After the flip llama-swap reloads its
  seats through its own API and the first real seat load is watched (`update.gate.post_flip_timeout`); a failure rolls
  back with "post-flip load failed". `--no-gate` and `update.gate.enabled: false` opt out.
- **Hardening.** The restart mechanism is probed before any drain (a missing task or unit skips the component,
  nothing stopped; on Windows the one task that runs the live llama-swap is found when the default name is absent);
  the verb reads its config only from `--config`, `$LOCAL_OFFLOAD_CONFIG` or the home config, never the current
  directory's; pruning deletes only meta-file-backed generations inside the live path's own `-versions` folder and
  never the live target; the Windows junction flip makes the new link first and restores the old one by rename; the
  attestation result is reported and ratchets (a once-attested publisher that stops attesting is rejected); the npm
  version must be a semver; a `--repoint` backup keeps the yaml's file mode and the gate's job file carries no seat
  environment; a lock older than three hours is taken over; timeouts, out-of-memory answers and busy seats are
  inconclusive (rolled back, retried), never a rejection.
- **Not in this change.** The installers and the fleet registry rows.
- **Known gap.** After a rejected llama build the gate's `gpu reserve` wrapper still warms the old seat back and the next
  flip unloads it again; a "no warm-back on reject" option belongs to `gpu reserve` (follow-up).
- **Check.** Real run on the ampere-8 reference box: `update --resolve llama` and `--dry-run` against the live publisher,
  and on a scratch installer layout a llama-swap update (junction flipped, previous kept, record rewritten) and a
  rollback (see `docs/systems/update.md`).
