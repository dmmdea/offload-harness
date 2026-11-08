### Added - llama.cpp KV slot retention: a seat's context survives the idle unload (ADR 0056 Layer 2 reopened, ADR 0100)

- **What it does.** Five parts:
  - **Render.** On a node whose llama.cpp build is at or above the new floor `"kvslot": 11506` in
    `setup/floors.json`, `install render` gives every one-slot chat and agent seat
    `--slot-save-path <home>/kvslots` and creates that directory. The `--parallel 8` twins no longer carry the
    token, and a new render audit refuses the flag beside a stated slot count above one. An unknown or older
    build renders exactly what it rendered before.
  - **Run start.** The agent loop keys the byte-exact prompt its first call sends: the seat's config pin, the
    seat, the tool specs, and every message, recall and setup replay included. When a slot file for that key
    is on the node, the loop restores it. A key is saved on its second sighting and restored from the third.
  - **Tool waits.** Beside a tool call whose cap is 240 s or more, the loop saves the session under a
    run-scoped key. If the seat unloaded during the tool, it warms the seat and restores. That file is
    deleted when the run ends.
  - **Verdicts.** Every restore is judged on the next completion: `hit` only when the cache served at least
    80 % of what the restore could hold, and a missed start restore deletes its file. The run's
    `kvslot_restore`, `kvslot_admit`, `kvslot_restored_tokens`, `kvslot_restore_ms`, `kvslot_saves`,
    `kvslot_saved_bytes` and `kvslot_toolwait_*` travel on the wire result and the ledger row.
  - **Shared core.** The loop and the bearer-gated `/fleet/kvslot/*` lane share one core, `internal/kvslot`.
    It refuses a seat whose running command does not state one slot, skips a save with under 6 GiB free on
    the slot volume, touches a restored file so the LRU sweep keeps it, and sweeps only `k1-<sha256>.bin`
    files. The node reads the slot directory from the running seat's own command line. `agent_kvslot_retention:
    false` turns the loop's retention off.
- **Why.** Layer 2 was blocked because a restore bought zero prefill on b9934 (upstream #25913). The fix,
  PR #26004, is tag b11506. On b11977 with a hybrid 9B on CUDA, every arm a new process, a restore turned a
  3,279-token prefill into 4 tokens, and a follow-on turn into 26. The idle unload, a media job and a GPU
  lease all drop a seat's context mid-session today; this is the memory wall's "move out and restore
  instead of rebuild by compute" on hardware that has no coherent CPU-GPU link.
- **Check.** On `<node-b>`, a scratch fleet node built from this branch served a Qwen3.5-9B seat with the
  flag. The same contract was delegated three times, with the seat unloaded before the third
  (`/running` empty):
  - Run 1 was `first-sighting` (5,584 tokens prefilled, 1,886 ms).
  - Run 2 was `repeat-saved` (one 241,593,408-byte file).
  - Run 3 was `kvslot_restore: hit` (1,768 tokens restored in 304 ms; 3,845 prefilled, 1,282 ms).
  - The answer was identical in all three.

  The engine re-measure is recorded in the ADR 0056 amendment. The tool-wait save was not driven end to end:
  no tool in the contract ran 240 s. Its save, restore and judge are the same core calls this run exercised.
