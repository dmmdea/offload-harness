### Fixed - a kept or warm ComfyUI frees its own models and caches after 300 s idle, from inside the engine

- **The defect.** The rule is that no idle model stays loaded: every engine unloads after 5 minutes. llama-swap does it with its own
  `ttl`; ComfyUI has no idle timer (its parser takes no idle or timeout argument), so a ComfyUI that nothing frees holds what it
  keeps. The exposure is narrower than "a kept instance lives to the end of its lease" suggests, in two ways. Every runner POSTs
  `/free` at the end of its job, kept or not (`render/gpu-lock.mjs`), the Go side frees after every run, and the holder of the lease
  stops a kept instance at release. And the harness launches every ComfyUI with `--disable-smart-memory`, with which ComfyUI itself
  calls `unload_all_models()` when a prompt ends, on success and on a handled error (`execution.py` after the node loop; read in
  the ComfyUI checkout of the reference box, its version in `docs/systems/media-generation.md`), so a kept single-prompt instance (`--cache-none`) is already bare between jobs and the pack finds nothing to free
  there (it does not fire and logs nothing). What survives is (a) a warm (`--batch`) session's executor cache, which keeps the
  loader outputs (the weights the unload moved to host RAM) until a free resets the executor, so a runner killed before its cleanup
  (a tree-kill skips it) or a session that stalls holds host RAM until the lease ends, and (b) a hand-started instance run without
  `--disable-smart-memory`, which keeps its models in VRAM after a prompt. Both are read from ComfyUI's code and a fake queue, not
  yet seen in a live ComfyUI.
- **The fix, in the engine.** A kept (`--keep-comfy`) or warm launch now carries a harness-owned pack,
  `render/comfyui-ext/idle-nodes/offload_idle`, standard library only. It exports an empty `NODE_CLASS_MAPPINGS`, because ComfyUI
  reports a folder with neither that nor `comfy_entrypoint` as a custom node that failed to import. One daemon thread polls every
  5 s. A poll is busy when a prompt is queued or running (`get_tasks_remaining`) or `task_counter` moved (a prompt that began and
  ended between two polls); both are read under the queue's mutex. After 300 s of polls that were not busy, if the engine holds
  something (a loaded model, or the executor cache is on, which is the warm case), it sets `unload_models` and `free_memory`, the two
  flags `POST /free` sets, in ONE hold of the mutex after re-checking that nothing is queued and no prompt ran, once per busy period.
  ComfyUI's own worker thread drains them (unload all models, reset the executor, collect garbage); the pack never touches a model. A
  prompt that lands in the microsecond after the check runs first and the flags drain after it: one avoidable reload, never a
  corrupted run. The pack never raises: a failure is logged once as `[offload_idle] NOT ACTIVE` and the next poll retries, and an
  engine it cannot ask what it holds is freed anyway (a `/free` is always safe). It serves `GET /offload/idle` (`limit_sec`,
  `idle_sec`, `armed`, `freed`, `last_free_unix`, `error`), registered at import, which ComfyUI serves because custom nodes import
  before `add_routes`. This is the llama-swap `ttl` analogue: the clock belongs to the engine, the harness runs no timer or watcher
  (plan I5) and acts on no other process.
- **The fix, launcher** (`render/comfy-lifecycle.mjs`).
  - `resolveIdleBound`: `COMFY_IDLE_FREE_SEC` unset is 300, `0` or `off` is none (one `COMFY-IDLE-OFF` line), a whole number of seconds
    from 5 up is that, anything else is 300 with a `COMFY-IDLE-WARN` naming the value. It reaches the pack as `OFFLOAD_IDLE_FREE_SEC`
    and is fixed when an instance is launched: a runner that reuses a running instance cannot change it.
  - `launchFlags({ idle })` adds the idle yaml (`render/comfyui-ext/extra_model_paths_idle.yaml`, apart from `nodes/` so an ordinary kept
    launch never imports the fail-closed Wan pack) to `--extra-model-paths-config` once, before the operator's args, and puts the pack
    INTO the last `--whitelist-custom-nodes` value list. That flag is `nargs='+'` with no append action, so a second flag would replace
    the multicard packs, and `--whitelist-custom-nodes=a b` reads `b` as a stray argument, so that spelling is rewritten to the space
    form; argparse abbreviations are read as the flags, down to the shortest ComfyUI's own parser accepts (measured on the reference box's checkout: `--wh`,
    `--disable-al`; `--w` and `--disable-a` are ambiguous there and stay unread); with `--disable-all-custom-nodes` and no whitelist
    one is added; with neither, every custom node loads and nothing is. Only when the yaml and the pack file exist: ComfyUI exits at
    boot on an `--extra-model-paths-config` file that does not, so a missing file is one `COMFY-IDLE-WARN` and a launch without it. With
    `idle` null (the default single render, which its runner kills) the argv is byte for byte what it was.
  - `ensureComfy` asks for the bound only for `keep` or `warm`, passes `OFFLOAD_IDLE_FREE_SEC` in a COPY of the child env made after
    the multi-GPU identity check (the parent env and the pinned-memory decision are untouched), and after the instance answers asks it
    once (`GET /offload/idle`): no answer, an `error`, or a `limit_sec` other than the one asked for (the pack never read
    `OFFLOAD_IDLE_FREE_SEC`) is a `COMFY-IDLE-WARN` and the render goes on; an answer that agrees is one `COMFY-IDLE` line.
  - A running instance reused under `keep` or `warm` whose argv lacks the idle yaml (hand-started, or kept by an older build) is
    reused, never restarted or refused, with one `COMFY-IDLE-WARN` per runner start, from at most one `GET /system_stats` per runner
    start (a keyed instance's ownership proof and the warning share the answer; the unkeyed default instance, which asked for none
    before, asks once, up to 8 s on a slow instance); an argv ComfyUI does not report is unknown, not missing. A
    hand-started instance opts in by adding the yaml to its own launch (`docs/OPERATOR-GUIDE.md`); the harness injects nothing into an
    instance it did not start (ADR 0009).
- **What this does not do.** It bounds models and executor caches, not the process: the Python process and its CUDA context live until
  the lease holder stops the instance at release, and an instance whose holder died lives until an operator stops it. A self-exit
  is deliberately not built: from inside ComfyUI a runner in a long step that is not ComfyUI's (ffmpeg, a QA pass, an LLM call) looks
  the same as an abandoned instance, and a stopped instance breaks that runner's next prompt where a `/free` only costs a reload; a
  pack-side check of the lease epoch is unsafe too (reuse re-stamps the marker while the child's env keeps the old epoch). So the
  register rows that say a kept instance lives to the end of its lease (A-RAM-3, E-4, D-2d.4-5) stay `partly`: models and caches
  bounded, process lease-bounded. Back-to-back batches are not touched: a running prompt, even a 550 s one, is never idle, the gaps
  inside a batch are seconds, and a driver that pauses more than 5 minutes between items pays one reload, which is the rule.
- **Docs.** `docs/systems/media-generation.md` (the new bullet with the live check, the log and "Who stops it" paragraphs, the source
  map), `docs/systems/gpu-lease.md` (the sentence that said nothing here is a timer now says the harness runs none and the engine
  carries its own), ADR 0009 (a dated "Extended" note), `docs/ROADMAP.md` (INV-2), `docs/OPERATOR-GUIDE.md` (the hand-started opt-in),
  `render/README.md`, and the package comment of `internal/comfyinst` (comment only).
- **Tests, red first.** `render/comfyui-ext/test_offload_idle.py` (standard library only, a fake queue with the signatures of ComfyUI's `PromptQueue` and a
  real `RLock`, a hand-set clock, no sleep: 30 tests) and `render/comfy-idle.test.mjs` (43 tests: the argv, the whitelist splice, the
  guard, the env copy, the reuse warning, the probe, the real `fetchIdleProbe` against a loopback server). They were committed
  failing on their assertions against an inert seam before the fix (24 failures in the Python file; 30 in the JavaScript file plus the
  test that runs the Python file, every one an `ERR_ASSERTION`). A review round then added five tests and changed two, again red
  first: the shortest abbreviation `--disable-al` failed on its argv comparison (no whitelist for the pack), a pack that reports another
  bound than the one asked for failed on `0 !== 1` warnings (it printed a plain `COMFY-IDLE` line), and the doubled `/system_stats`
  request of a keyed reuse failed on `2 !== 1`. The already-carried-yaml test, which failed only in a checkout whose path has a space
  (measured red from a copy under a folder named `sp ace`, 1 of 38), now builds its case from a path without whitespace, or says it
  cannot. `render/comfyui-ext.test.mjs` runs the Python file under any Python 3 (the ComfyUI interpreter when it exists, else `python`,
  else `python3`, each probed with `-I -c`), so CI runs it through `python3`. The three existing tests that reach the post-launch
  probe now inject it, so none can ask whatever listens on the default port.
- **Mutation proof.** 90 single-line mutants, run one at a time through `scripts/mutation-check.sh` (a syntax check first, so a mutant
  that does not build is `BAD-MUTANT` and never a kill; the unmutated tree is the control; the file is restored byte for byte), every one
  `RED`: 38 in the pack and 52 in the launcher. Pack: the limit comparison (`<` to `<=`, one second early, one second late); either
  flag dropped; the once-per-busy-period latch; `task_counter` ignored; a running prompt ignored; the recheck under the mutex removed,
  and its counter half removed; the fire, and the snapshot, made outside the mutex; the 5 s floor; the 300 s default; `0` not
  disabling; the thread not a daemon; a bad value not 300; the cache gate and the loaded-models gate dropped; an engine that cannot be
  asked held instead of freed; a tick that raises; an error logged on every tick; the route path; the poll clamp and the loop's wait;
  a failed route registration made fatal; the missing-queue line; the counter baseline; `NODE_CLASS_MAPPINGS` not `{}`; `torch`
  imported at module scope; `cache_on` and `models_loaded` inverted; the queue attribute; the status fields; the announce line.
  Launcher: idle on by default; the existence guard (the yaml, then the pack file); idle requested always, never, for keep only, for warm
  only, or still added under `COMFY_IDLE_FREE_SEC=0`; the yaml added twice, not looked for in the operator's args, or placed first; the
  FIRST whitelist chosen instead of the last; the `=` spelling not rewritten; the pack listed twice or placed after later flags; a
  whitelist always added; the abbreviations of the two flags unread; the child env built from `process.env`, written into the parent's,
  or set when nothing was launched; "applied" decided without looking at the argv; the reuse check run always or never, refusing, or
  warning on an unknown argv; the yaml's path spellings; the probe never or always asked, a probe that throws, an `error` answer not said,
  a 404 or an array taken as an answer, the probe path; the 5 s floor, `off`, and the default of `resolveIdleBound`; the pack name,
  the yaml name and the re-export in `wan-multicard.mjs`. The 24 mutants whose text carries quotes, backslashes or an empty
  replacement were run again through generated scripts after a first pass in which Windows command-line quoting had altered them.
  Review round (11 more, all `RED`, each killed by the test named for it): the `--disable-all-custom-nodes` floor 12 to 13 and to 11,
  the `--whitelist-custom-nodes` floor 4 to 3, the reported-bound mismatch branch off, its comparison inverted, its number check
  dropped, the asked bound left out of its warning, the `COMFY-IDLE` line printing another number, the shared `/system_stats` answer
  removed, and either of its two readers given the raw request function; the "yaml already carried" mutant was run again against the
  rewritten test.
- **Not measured.** The tests use a fake queue; no ComfyUI was started and no card was used, so the whole mechanism is read from
  the reference box's ComfyUI code (`main.py` `prompt_worker` drains the two flags, `execution.py` `PromptQueue` holds the mutex, the counter and
  `get_tasks_remaining`, `server.py` `post_free` sets the same two flags) and not seen running. The live check is written out in
  `docs/systems/media-generation.md`: it first confirms that a harness-launched kept instance is already bare in VRAM when a prompt
  ends (so VRAM cannot show the bound there), then measures the warm case in the ComfyUI process's host RAM at t+0, t+240 s and
  t+330 s, and the smart-memory case (a hand-started instance) in per-card memory. It also settles whether `/free` returns VRAM under
  `--gpu-only` (the multicard Wan placement), where ComfyUI's own unload frees nothing and a weight leaves the cards when its patcher
  loses its last reference. A warm launch now carries the pack, so the S3 batch acceptance is to be re-run. Other nodes' ComfyUI
  versions were not read; the gate (a loaded model, or a cache that is on) reads the engine at run time and does not depend on it.
