### Added - `update --component comfy-packs` moves installed ComfyUI node packs to the newest head, behind an import gate, with rollback

- **What it does.** The update verb gains a component that moves each installed, allowlisted ComfyUI node pack to the head of
  its repository's default branch (the allowlisted publishers release and tag nothing, so the head is the one channel they
  share). It resolves `git ls-remote` for the pack's origin, fetches that exact commit and checks `FETCH_HEAD`, takes
  `pip freeze --all` as a snapshot, fast-forwards the pack, runs one unified resolve over all installed packs under the
  host-torch constraints, and gates the result on `pip check`, the host constraints staying put, and
  `python main.py --quick-test-for-ci` exiting 0 with no `(IMPORT FAILED)` line for a moved pack. A pass writes the
  schema-2 record (`previous` = the commit held before) and the dependency marker. A failure resets the pack to its previous
  commit, restores the pip snapshot with `--no-deps`, requires the freeze and `pip check` to equal the baseline, appends the
  commit to `rejected[]` and exits 1. `--rollback comfy-packs` moves each pack back to its record's `previous` the same way,
  gate included. The work is `render/comfy-packs-update.mjs`; `update_cmd.go` starts it with the node's `node_path`,
  `comfy_dir` and render directory and relays its `--json` line and exit code.
- **What it leaves alone.** A tree with modified tracked files, or whose HEAD is not an ancestor of the remote head (local
  commits), is reported `kept` and never reset. A ComfyUI answering on its port makes the run `refused`. A remote that cannot
  be read, a fetch error, a gate that timed out or found the card busy, and a resolve that lost the network are inconclusive:
  everything is put back, nothing is marked rejected, exit 0. A render request still never moves an installed pack.
- **How to run it.** Under the GPU lease wrapper, because the gate initializes torch:
  `local-offload gpu reserve --wait <window> --drain --unload-seat --for <window> --reason "node packs" -- local-offload update --component comfy-packs --json`.
  `--dry-run` and `--resolve` list each pack, the head its repository names and what would happen, and change nothing.
- **Docs.** `docs/systems/update.md` ("Node packs"), ADR 0099 section 6 (one amendment line) and the node-pack row of the
  frontier-updates spec.
- **Rollback state on disk.** Before the first move the run writes `custom_nodes/.packs-update-journal.json` (the `pip freeze --all` and `pip check` baselines and each pack's from/to); the next start undoes a killed run from it first, SIGINT, SIGTERM and SIGBREAK run the same undo, and a pass or a finished undo deletes it. Every reset is `git reset --keep` after a fresh `git status`: a pack edited meanwhile stays where it is, is reported `kept` as `DIRTY_AT_UNDO` and the run exits 2. A quick test that exits non-zero without naming a moved pack in an `(IMPORT FAILED)` line is inconclusive (nothing rejected, exit 0, `QUICK_TEST_NO_PACK_NAMED`).
