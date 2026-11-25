---
status: Accepted
date: "2026-11-25"
---

# 0106 — A seat's window is the one its engine serves

## Context

The harness holds two numbers for how large a seat's context window is, and it used to trust the one an operator wrote by hand
(`agent_ctx_tokens`, and `ctx_tokens` on a layer seat) everywhere it could not ask the engine. Two incidents on 2026-11-25 came from that.

**A stale key advertised to the fleet.** A node's serving config said `--ctx-size 49152 --parallel 1` while its key still said 131072
(an edit to the serving config that left the key behind). `/fleet/health` published the key, the delegator sized contracts to it,
and the node was dealt work its engine rejected. Nothing compared the key with the serving config: `doctor` had no row for it, and
the only check was a build-time test of the seeded tier values against the templates.

**A cold run that ran on the key.** On the reference box the seat's key said 163840 and its engine served 113600 (vLLM sizes
itself at load). A run that found the seat cold spent its admission budget waiting for the load, so its window probe had no time
left, and the run budgeted compaction against 163840. The server rejected the first request that outgrew 113600. Nothing was kept
from one run to the next, so every cold run made the same mistake.

## Decision

1. **Three sources, trusted in this order.** What the seat's engine last reported (a window probe, a loaded-only read, an
   `offload_status` read), remembered per seat in `seat-rates.json`; else what the serving config's own flags state; else the key.
   `servedwindow.Known(cfg, seat, configured)` is the one resolver. It reads two small files (both memoised on mtime and size) and never
   calls the engine.
2. **The remembered window is valid against one serving-config entry.** It is stored with a fingerprint of the entry that answers to
   the seat (`servingtmpl.SeatEntryFingerprint`: the command, environment, proxy, health endpoint and concurrency limit, with
   macros expanded). An edit to the entry changes the fingerprint and the memory is no longer used. A memory made with no readable
   serving config and read with none are equal.
3. **Confirmed or unconfirmed.** A remembered window is confirmed when no cold load of the seat ended after it was read. Confirmed,
   it is the answer in both directions (`served`): it beats a key that is too high and a key that is too low. Unconfirmed (the seat
   has loaded since, and a vLLM seat can size itself differently at load), it can only lower: the smaller of it and the serving
   config's statement, or the key when the file states none (`served-unconfirmed`). There is no clock-based expiry; the two facts that change the answer are
   the entry and the loads.
4. **The serving config states a window for a llama.cpp entry only.** The last `-c` / `--ctx-size` (the flag beats
   `LLAMA_ARG_CTX_SIZE`) divided by the stated `--parallel` / `-np` slots, because llama.cpp splits the context among its slots, or
   undivided under `--kv-unified`. No stated slot count, a size of 0, a macro that does not resolve, or a proxy entry (a vLLM seat's
   window lives in its own wrapper, not in the file) states nothing, and the answer falls to the next source (`serving_config`).
5. **Nothing known is today's behaviour.** With no memory and no stated window the key is the answer (`config`), byte for byte. A
   key of 0 stays "not advertised": the node never turns an unset key into an advertisement, though a run may still use a window it
   knows.
6. **Where it applies.**
   - `/fleet/health` advertises `Known(...)` as `agent_ctx_tokens` (only when the key is above 0), with additive `agent_ctx_source`
     (when it is not the key) and `agent_ctx_configured` (when it differs). A composite node's published layer rows get the same
     treatment per seat. A change of the advertised value or its source is logged once.
   - Both run doors pass `Known(...)` as the window to fall back on when the probe cannot answer, and the note names the source.
     Every successful probe is remembered at the probe, even if the run later defers.
   - A run whose warm-up confirmed the seat ready but whose admission budget is spent gets one more bounded read (3 s) for the probe.
     A run whose probe failed installs a one-shot refresh in the loop: after the first answered call, one loaded-only read of the
     window, which replaces the run's window, is remembered and is appended to `ctx_window_note`. A run whose probe answered installs
     nothing.
   - The fleet node reads the window once, loaded-only and off the health path, when its agent seat goes from not ready to ready
     (a load nobody on the box started). It never polls: a read through llama-swap's `/upstream` counts as activity and restarts the
     seat's idle timer.
   - `offload_status` reports the known window beside the cold-seat note, and remembers the window of a loaded seat it read.
   - `doctor` prints a `window:` row comparing the key with what the seat serves (informational; the exit code never changes).

## Consequences

- A node advertises what its engine serves. A key left behind by an edit no longer sends the node work its engine rejects, and a key
  that understates the seat no longer keeps it out of the fleet's placement; `doctor` names either.
- The first cold run of a seat nobody has measured still starts on the key (or the serving config's statement) and corrects itself
  after its first answered call. Every run after it starts on the window the engine reported.
- A vLLM seat re-sized by an edit to its own environment file and restarted by someone else stays "confirmed" until the next run, the
  next not-ready-to-ready edge the node sees, or an `offload_status` read. The fingerprint cannot see that file.
- A run's compaction can change after its first step when the refresh lowers or raises the window, so a run's compaction depends on
  when its window was learned. The alternative was a server rejection.
- The key stays a valid key and becomes the last resort; installers and tier seeds are unchanged (they are the first-boot value).
- The node's own placement over its layers (`internal/placement/decide.go`) still reads the configured seat windows; only the rows it
  publishes are corrected.
- Related: [ADR 0015](0015-compaction-defaults-on-served-window.md) (compaction budgets against the served window) and
  [ADR 0043](0043-serving-config-provenance.md) (the serving config as a read source).
