### Fixed - a configured GPU UUID that no live card has is now a named defect in doctor, `gpu status` and the offload_status brief

- **What was wrong.** After a card was replaced, its UUID stayed in `gpu_comfy_order` (and in every other setting that names a card by UUID,
  and in the `CUDA_VISIBLE_DEVICES` of the llama-swap seats), and each reader degraded on its own, quietly: the ComfyUI order was dropped
  with one `note:` line, card-scoped media leases fell back to whole-node holds, and a seat whose only pin was the missing UUID saw no CUDA
  device and loaded on the CPU. `doctor` passed, and no field told an agent to act.
- **What changed.** One check holds every UUID-valued setting (`gpu_comfy_order`, `primary_gpu_uuid`, a layer's `display_device`, `devices`
  and seat `device`, and the serving config's seat pins) against the card table (`gpuprobe.CheckRefs`, `Config.CardRefs`). `doctor` prints a
  `card refs:  FAIL` row per stale UUID (the setting, the seats, what the setting does meanwhile, the live cards the stale settings do not name, the fix)
  and exits non-zero; `gpu status` and `gpu cards` print the same as a `DEFECT:` block; `gpu status --json`, `gpu cards --json` and the
  `offload_status` brief's `gpu_cards` carry `card_refs` with `state` `none` | `ok` | `stale` | `unknown`. A card table that cannot be read is
  `unknown` and flags nothing as stale. A box whose config names no card by UUID reads no GPU and prints nothing new.
- **Not changed.** No verb rewrites a card UUID in the config: the fix is a hand edit of the setting the row names (a new `gpu_comfy_order`
  is measured with a `CUDA_VISIBLE_DEVICES=<uuid>` probe first). Seats are read only when `serving_config_path` is set, and a pin a macro
  writes is judged by `audit-yaml` (`pin-unresolved`), not here.
- **Check.** `doctor` and `gpu status` run on a temp config (`--config`) naming a made-up UUID in `gpu_comfy_order` and in three seats of a temp
  serving yaml, against the real card table; docs: `docs/systems/gpu-lease.md`, `docs/OPERATOR-GUIDE.md`, `docs/systems/mcp-server.md`.
