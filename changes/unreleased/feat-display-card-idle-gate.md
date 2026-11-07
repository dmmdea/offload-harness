### Changed - the display card takes jobs only while it is idle, and is picked last

- **What it does.** The card allocator (`gpu reserve --cards`, the media admission path, the grant-time check) opens the
  display card only while it is idle: its utilization under 15 % and the operator away by the presence probe
  (`operator_presence` unset reads `auto` for the allocator; the llama-swap twin guards keep their old reading). The 60 %
  cap (`gpu_display_cap_pct`) then applies as before. Every non-display card now sorts ahead of the display card, so
  `--cards 2` on the `blackwell-3x16` reference box takes the two 5060 Ti cards. A new verb, `gpu display-check --vram GiB [--json]`, asks the
  same rule for a launcher outside a lease (exit 0 admits, 3 refuses), and the vLLM seat stub `seat-cmd.ps1` asks it for a seat
  whose row declares `DisplayVramGiB` and starts nothing on a refusal. The display card's `display cap:` status line ends with
  the gate's reading (`idle_gate`, `idle_why` in JSON). A whole-node media call whose worker drives no ComfyUI (sd.cpp, the
  Chatterbox voice worker) used to compute on `cuda:0`, the display card under CUDA's fastest-first order; its children now see
  only the non-display cards unless the display card would take an undeclared job now.
- **Why.** The operator's rule of 2026-11-07, with the monitors back on the 5070 Ti: "send 2 card jobs to the 5060s first and 3
  card jobs follow the rule of only using up to 60% of the 5070ti card WHEN IDLE so i can always keep it interactive." It
  supersedes the 2026-10-11 "60 % at all times, no presence term" rule and the order that put an empty display card first.
- **Check.** On `<node-b>` (`blackwell-3x16`) with the operator at the desk: `gpu display-check --vram 8.5` exited 3 ("card 1 is 20 % busy"),
  `gpu cards` printed `idle gate: CLOSED to jobs`, and `gpu reserve --cards 2 --vram 1` held the two 5060 Ti cards. ADR 0039
  and ADR 0075 amendments 2026-11-07, `docs/systems/gpu-lease.md`.
