# A vLLM seat behind llama-swap on Linux (systemd + polkit)

> **NOT persistent, as of 2026-09-08.** This pattern originally shipped the seat as a boot-enabled unit with
> `ttl: 0`, a `persistent` group and a startup preload — four independent ways to pin a model to a GPU forever.
> The reference A2 then held 10,338 of 15,356 MiB with the engine idle. The seat now loads on first use and
> **unloads after 300 s idle like every other seat**; the unit has no `[Install]` section so it cannot be
> enabled at boot. A long cold load is paid once by one request; a pinned card is paid continuously by
> everything else. See ADR 0035's amendment.

Reference files for the pattern decided in [ADR 0035](../../../../docs/architecture/decisions/0035-persistent-vllm-seat-behind-llama-swap.md):
the engine is a **persistent system unit** (up at boot, `Restart=on-failure`), and llama-swap — still the one endpoint the
fleet node talks to — gets a **thin entry** whose `cmd` starts the unit and stays attached and whose `cmdStop` stops it.
Because llama-swap runs as an unprivileged user under `NoNewPrivileges=yes` (no `sudo` possible), the start/stop goes
through a **polkit rule scoped to that one unit**.

Why not point the node's `endpoint` at vLLM directly: the node's STT, image-gen and vision seats live behind llama-swap,
and the agent task's admission wait, roster probe, window probe and chat re-pack all read `cfg.Endpoint`. Why not let
llama-swap spawn vLLM as a plain `cmd`: the engine would inherit llama-swap's sandbox (`ProtectHome`, `ProtectSystem=strict`,
private `/tmp`) and a different JIT/compile-cache environment from the one the arms were measured in, and every llama-swap
restart would be a 2–4 min engine reload with no boot persistence.

What the pattern buys, measured on the reference box (NVIDIA A2 16 GB, `ampere-16`, harness 0.113.19, 2026-09-06):

| property | how it holds |
|---|---|
| `served_models` / `agent_seat_resident` | roster-derived from llama-swap's `/v1/models`, exactly as for a llama.cpp seat (the entry lists the id + aliases) |
| `gpu reserve --drain --unload-seat` | drain reads vLLM's `/metrics` through llama-swap; unload → `cmdStop` → `systemctl stop` → card free in 6 s |
| `gpu release --warm-seat` | `/upstream/<seat>/health` → `cmd` → `systemctl start` → seat `ready` in 35 s warm (125–250 s cold) |
| reclaim accounting | the seat is "ours" (llama-swap-resident), so the idle baseline is only sampled with the card empty |
| coexistence | `--gpu-memory-utilization` sized so the small seats fit beside it (0.65 on a 15,356 MiB card = ~9.6 GB used, ~5.8 GB free); heavier opt-in seats are documented as not co-resident |

## Files

| file | role |
|---|---|
| `vllm-seat.service` | the persistent unit (edit user, paths, unit name) |
| `vllm-seat-run.sh` | `ExecStart`: environment + the measured launch line; change the line here, `systemctl restart` the unit |
| `vllm-seat-cmd.sh` | llama-swap `cmd`: `systemctl reset-failed` + `start`, then block while the unit is active |
| `vllm-seat-cmdstop.sh` | llama-swap `cmdStop`: `systemctl stop` (a real stop, so the unload frees the card) |
| `50-llama-swap-vllm-seat.rules` | polkit: the llama-swap user may start/stop/restart/reset-failed THAT unit only |
| `llama-swap-entry.yaml` | the entry, its persistent group and the startup preload; raise `healthCheckTimeout` for the cold load |

## Traps found on the reference deployment

- **The proxy target must be an address the engine actually binds.** On the box itself the MagicDNS hostname resolved to
  IPv6 addresses only while vLLM bound the Tailscale IPv4 — llama-swap's health check would have failed forever. Use the
  literal Tailscale IPv4 (stable, not a LAN lease) in `proxy:`.
- **vLLM 404s any model name it does not serve** — set `useModelName` to the served id so every alias reaches it, or list
  every alias in `--served-model-name`.
- **`healthCheckTimeout` is global** (default 120; the reference box had 60): the vLLM cold load is 125–250 s, so raise it
  (480) — llama.cpp seats still fail fast when broken, they just get a longer ceiling.
- **A thinking model spends `max_tokens` on reasoning first** — a smoke test with `max_tokens: 8` returns `content: null`
  and the text in `reasoning_content`; that is the `--reasoning-parser` working, not a broken seat.
- **The harness's llama-server seat-pin probe (`/upstream/<seat>/props`) 404s on vLLM** — logged and non-fatal; the
  `seat_config_*` row fields are absent for vLLM seats.
- **llama-swap's own restart runs `cmdStop`** (it stops every loaded model on shutdown) — a config edit costs one engine
  reload; the preload hook re-attaches on start.
- **Text-only trims the footprint**: `--limit-mm-per-prompt '{"image":0,"video":0}'` on a multimodal checkpoint dropped the
  weights from 5.11 to 4.48 GiB and removed the encoder-cache reservation; measure the pool from the `GPU KV cache size`
  banner at the utilization you intend to ship, never from arithmetic alone.
