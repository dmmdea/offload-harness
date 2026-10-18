### Fixed - the vLLM seat launcher no longer loops on a pinned KV pool too small for SEAT_MAX_LEN (new SEAT_KV_NEED_GIB) and mounts the cache-server share at the store address on the pinned network, not a random one

Two production defects of `setup/templates/vllm-seat/seat_fg.sh` (registers B-54 and B-55), found on the reference workstation. The launcher is shell rendered
verbatim into the seat, so both fixes are one file; the tests run that file start to finish against stand-ins. No renderer and no installer changes.

- **B-54, the defect.** With `SEAT_KV_HEADROOM_GIB` set the launcher pins the KV pool (`--kv-cache-memory-bytes`) at the smaller free memory of the seat cards, less
  `SEAT_NONKV_GIB`, less the headroom, floored at `SEAT_KV_FLOOR_GIB` and capped at `SEAT_KV_CAP_GIB`, and it always passed `--max-model-len` = `SEAT_MAX_LEN`. A card
  holding ~2.2 GB of desktop graphics memory gave 1.95 GiB, floored to 2.0 GiB, and vLLM 0.28 refused every start: `2.66 GiB KV cache is needed, which is larger than the
  available KV cache memory (2.0 GiB)` for 163,840 tokens (it estimated 98,784 would fit). llama-swap restarts the seat on every request, so the seat looped: about 98
  failed starts in two days.
- **B-54, the fix.** A new optional knob `SEAT_KV_NEED_GIB`, the `N GiB KV cache is needed` figure vLLM prints in that refusal. With it set and the pinned pool below it,
  the launcher passes `--max-model-len -1` instead of `SEAT_MAX_LEN` and prints one `seat_fg:` line naming the pool, the need, `SEAT_MAX_LEN` and that the seat serves less than
  `SEAT_MAX_LEN`. Pool at or above the knob: nothing changes. Knob unset and the pool floored: `SEAT_MAX_LEN` is kept, because an auto-fit could exceed the length the operator
  set, and a WARNING names `SEAT_KV_NEED_GIB` as the fix (no behaviour change without the knob). A non-number is a warning and is ignored. The pin's banner line is unchanged
  and still printed first. The knob acts only while the pin is on.
- **B-54, why `-1` does what is claimed (read in vLLM 0.28.0, not assumed).** `vllm/config/model.py` accepts `-1` (`max_model_len: int = Field(default=None, ge=-1)`), keeps it as
  `original_max_model_len` and resolves the working value to the model's own derived maximum, so an auto-fit never exceeds the model. With `kv_cache_memory_bytes` set,
  `vllm/v1/worker/gpu_worker.py` skips memory profiling and returns exactly that value as the available KV memory (through `reserve_mm_ipc_gpu_memory`, which returns it unchanged
  for a model without multimodal config); `vllm/v1/engine/core.py` hands it to `get_kv_cache_configs`, which, when `original_max_model_len == -1`, calls
  `_auto_fit_max_model_len` with that value (`vllm/v1/core/kv_cache_utils.py`), a binary search for the largest length that fits it, and then runs the usual admission check at the
  fitted length. `vllm/v1/engine/core_client.py` copies the fitted length back to the front end. So the auto-fit is computed against the PINNED pool, not against
  `--gpu-memory-utilization`.
- **B-55, the defect.** The share is mounted from `//<name>.local/<share>` with `SEAT_L2_MOUNT_SRCADDR=auto` pinning the cifs source address, and the name goes through the system
  resolver, which returns ONE address. The store host has two mDNS responders for its name: one pinned to its wired NIC (the right answer) and another agent that answers on every
  interface with that interface's own address (a Wi-Fi address on the same /24, container bridge addresses, a VPN address). The mount landed on the Wi-Fi path (writes ~24 MB/s, so the
  launcher degraded L2 under `SEAT_L2_MIN_MBPS`), on an unreachable bridge address, or on the VPN address, which refuses the pinned source. Every start for three days ran without L2.
- **B-55, the fix.** For a cifs mount whose source host is a `.local` name and whose source address is pinned (`auto` or explicit), the launcher collects ALL the mDNS A answers for
  the name with a raw query to `224.0.0.251:5353` (QU bit set, sent three times in about a second, out of the pinned source address) and mounts `//<address>/<share>` at the FIRST
  answer inside the pinned source address's IPv4 network (the prefix is read from `ip -4 -o addr`), with the same options. One `seat_fg:` line prints every answer seen and the one
  chosen, and the L2 status file reads `ok <stamp> mbps=<n> addr=<address>` (the field is appended only when an address was chosen, so other seats' lines are byte-identical). If the
  helper fails, times out, finds no answer, or no answer is on that network, or there is no python, the mount is by name exactly as before and the line says why. Names that are not
  `.local`, address literals, nfs mounts and an unset pin never reach the lookup. The degrade messages keep their text (a Go test pins them) and name the address when one was chosen.
- **B-55, the bound.** The helper is a here-doc inside `seat_fg.sh` (no file to install), run with `$VENV/bin/python` or else `python3` under `timeout -s KILL 3`; it listens for 1 s on
  its own and ends. A start can therefore never wait on it for more than 3 s.
- **Limit, stated.** "The first answer inside the pinned network" cannot separate two addresses of the store that sit on the SAME network (a wired and a Wi-Fi address on one /24).
  The line printed shows every answer, and an IPv4 literal in `SEAT_L2_MOUNT_SRC` (mounted as given) is the operator's override. The query uses the legacy-unicast form (source port
  other than 5353), which the mDNS RFC makes every responder answer by unicast; a responder that ignores it would be a "no answer" and falls back to the name.
- **Test seams.** `SEAT_FG_TEST_MDNS_CMD` (a command standing in for the lookup) and `SEAT_FG_TEST_MDNS_DEST` (`host:port` for the helper's query) exist for the test and are documented
  in the script where they are read; nothing in a rendered seat sets them.
- **Tests (red first).** `setup/templates/vllm-seat/seat_fg.kv-and-store.tests.sh` runs the real launcher from a scratch copy against stand-ins for every command that touches the box
  (the MP server's unit is never stopped or started, nothing mounts, no GPU is asked, nothing binds a port) and reads the argument list `vllm serve` was exec'd with, the mount command
  line, the printed lines and the status file: 16 KV cases and 20 store cases, including the embedded helper driven alone against a loopback responder that answers the way real ones
  do (unicast, query id and question repeated, compression pointers, other names and types mixed in, junk first, only the question it must ask answered). On `origin/main`'s launcher 11
  KV cases and 16 store cases fail and nine are guards that pass on both sides (pool above or at the knob, knob unset and not floored, no pin, `SEAT_MAX_LEN` default; non-`.local` name,
  address literal, nfs, no pin). Two Go wrappers (Linux) run it and name every case, and a portable text pin covers the hard bound, the scoping and the single `mount` command. Fifteen
  mutants of the launcher (the comparison turned round, `-1` never set, the warning never printed, number validation loosened two ways, the first answer taken whatever its network,
  the last on-network answer taken, no timeout around the lookup, no `.local` guard, no source-pin guard, the status line without the address, the mount ignoring the chosen source, the
  helper accepting any owner name, no de-duplication, no QU bit) are each caught.
- **Docs.** `docs/OPERATOR-GUIDE.md` and `docs/systems/cache-server.md` describe both behaviours; the template seat env lists the pin family as a commented-out block. No Go renderer
  emits any `SEAT_KV_*` line, and `SEAT_KV_NEED_GIB` stays that way on purpose: like the headroom, floor and cap it is a per-box measured figure (it depends on the card, the model and
  `SEAT_MAX_LEN`), so a rendered default would be a guess.
