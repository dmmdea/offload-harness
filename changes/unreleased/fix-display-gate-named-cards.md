### Fixed - naming the display card is not a way past its rule

- **What it does.** A `gpu reserve` that names its cards (`--devices`, the cards its command pins itself to, the cards a lost
  lease held when the reserve queues again, and the detached holder of any of these) and includes the display card is held to
  the display card's rule, as `--cards` is: it waits (outside the line, while `--wait` lasts) until the card is idle (the
  operator away and the card under 15 % busy), takes it only with `--vram` declared and within `gpu_display_cap_pct`, and the
  grant asks again when its turn comes. A cap of 0, no `--vram` under a cap below 100, or a footprint over the cap's budget is
  refused at once. Named sets without the display card and whole-node leases are unchanged.
- **Why.** `--devices` was "the operator's word" and skipped the rule, and an agent used it to render on the operator's screen
  while he was at the desk (83 % of the card's VRAM). The operator's rule is 60 % only while the display is idle.
- **Check.** `TestGPUReserveDevicesOnTheDisplayCardIsHeldToItsRule`; on the `blackwell-3x16` reference box with the operator at
  the desk, `gpu reserve --devices 1 --vram 4 --wait 0` is refused with the presence reading and takes nothing. ADR 0039
  amendment 2026-11-10, `docs/systems/gpu-lease.md`.
