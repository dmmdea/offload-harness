### Fixed - the display layer's guard can read the card the driver reports for the monitor (`display_device: "auto"`), and the blackwell-3x16 seed ships it

- **The defect.** `layers[].display_device` named a card: the seed said index `1`, a live box may pin a UUID. The `display_floor` guard
  cross-checks that declaration against the driver's own reading of which card drives the monitor (`display_active` / `display_attached`)
  and refuses on a contradiction, but it could only refuse. The 60 % display cap (`RowsWithCap`, the allocator) and the floor the
  allocator holds an opened display card to already follow that same reading and carry no index, so the one place that could not follow
  was the display layer's own guard: with the monitors moved to another card the layer refused forever on the seeded declaration
  ("the driver reports the monitor on index 0"), and the display watcher in `fleet-serve`, which asks the same guard about a loaded
  twin, unloaded any twin it found on every check.
- **The fix, config.** `config.DisplayDeviceAuto` (`"auto"`) and `config.IsAutoDisplayDevice` (case and surrounding whitespace do not
  matter). No new key and no new wire field: `layers[].display_device` already travels in `layers` rows and may now read `auto`. The
  sentinel cannot be taken for the other forms: an index is all digits and a UUID pin starts `GPU-`. `ValidateLayers` is **not** given a
  shape check on `display_device` on purpose: a load error strips every layer from the config `Load` hands back, and the runtime resolves
  any prefix of a dashed GPU UUID (`gpuprobe.IndexOf`), so a stricter rule would turn a working UUID-pinned box into one with no composite
  tier. A typo such as `atuo` is therefore not a load error; it fails closed where it is used, the display layer refusing by name.
  The load-time footprint rule, which can only match a plain CUDA index, does not run for `auto` (admission enforces the same rule on the
  card it derives, as it does for a UUID pin).
- **The fix, guard.** With `auto`, `resolveDisplay` takes the card from `Live.ScreenCards` and returns it as both the key the free-VRAM
  reader is asked and the CUDA index, so every number the guard reads is the derived card's; the admission guard, the node's published
  layer rows and `ResidentVerdict` (the watcher) all read it through the one function. It refuses, saying why, when the reader is absent,
  when the driver flags no card (no NVIDIA card has a monitor attached: a headless box, or a screen on an iGPU; a screen asleep on a
  driver that does not report `display_attached`; a transient failure that left `display_attached` unknown), and when it reports the
  monitor on more than one card: it does not choose, because the tolerance a declared card gets for a
  second monitor is trust in the operator's word and there is none here. Reasons print `display device auto (index N)`.
  **Seat-pin coherence, sentinel path only:** a seat that declares `display_footprint_gib` is meant to sit on the display card, so under
  `auto` it is refused when its pin does not include the derived card, because a floor judged on the monitor's card while the twin loads on
  another would guard nothing. This is a policy choice and it refuses a harmless-looking admission on purpose: its job is to make a stale
  twin pin loud. It keys on the declared footprint, so a seat that declares none is judged free - 0 as a seat off a declared card always
  was. The seat's own `device` stays a literal pin (an index or a UUID): the sentinel is only meaningful in `display_device`, because
  `DeviceList()` feeds lease fencing and placement, where an unresolvable pin reads as every card.
- **The literal and UUID paths are unchanged, byte for byte.** One label helper names the device in every message, including the two
  no-reader messages that printed the raw config string; for a declared card it returns the text each site already printed. A characterization
  test pins the old sentences word for word, a literal layer next to an `auto` layer in one config, a typo, and a seat pinned off a declared
  card.
- **The fix, seed.** `setup/templates/profiles.json`: the `blackwell-3x16` display layer ships `"display_device": "auto"`, and the `_fields`
  text for `layers[].display_device` and `layers[].seats[].device` says so; `docs/tiers/blackwell-3x16.md` is regenerated (`go run
  ./cmd/gentiers .`), and now reads "display device auto". The seeded twin seat pin stays `1` (and the template's
  `CUDA_VISIBLE_DEVICES=1`): on the arrangement the table describes, a monitor on card 1, behaviour is the same; on a box whose monitor is
  elsewhere the twins now refuse with their pin and the monitor's card named, a visible re-pin step, instead of the guard naming the wrong
  card.
- **What this does not do.** It does not make the layer follow the monitor on a live box. `config.json` is the operator's and the seed never
  re-applies to it; the layer reads the driver's card only after the operator sets `display_device` to `auto` there **and** re-pins the
  twins (the seat `device` in `layers`, and the pin on each twin in the llama-swap yaml, which the renderer takes from the template, not
  from the layer). Until then a display layer declared with a literal card on a box whose monitor is elsewhere still refuses, with the
  same sentence as before. Benchmark drivers outside this repository that pin the display card by index are not changed here.
- **Behaviour changes to know about.** (1) `auto` is stricter than a declared card where the driver is silent: a literal card is trusted
  when the driver cannot say which card has the monitor and tolerated when a second monitor sits on another card, so a fresh blackwell-3x16
  install on a headless box, one whose screen is on an iGPU, one whose screen sleeps on a driver without `display_attached`, or one with
  monitors on two cards now refuses the display layer at admission until `display_device` names a card. The strictness is not only at
  admission: the display watcher in `fleet-serve` asks the same guard of a loaded twin on every check, dormant layer or not, so under `auto`
  a reading that names no single card unloads a loaded twin on that check, however much VRAM is free and with no drain (a request in flight
  on the twin ends with llama-swap's 502), where a declared card trusts its declaration through the same reading and keeps it. That is the
  watcher's existing fail-closed rule (a missing device sample unloads a twin too) applied to a card the config no longer names. (2) The display layer's whole spec is hashed into the render stamp, so a blackwell-3x16 llama-swap config rendered and
  stamped before this change reports `STALE` under `audit-yaml --against-render` until it is re-rendered (nothing served is wrong; the
  stamp is behind). (3) On a box whose only card drives the monitor, or whose screen card reports no UUID, the card table flags no display
  card while `auto` still follows the screen reading (the card table and the screen reading derive the display card with different
  exclusions); a characterization test pins it. (4) The seed edit changes the `layers` value `audit-config` compares as a whole: a live
  config whose `layers` were merged from the earlier table (`display_device` `1`) now reports `layers` as `DIFFERENT`, and `audit-config`
  exits non-zero while any seed-owned key differs, until its display layer matches the seed's (a box whose twins are re-pinned off index 1
  differs from the seed by those pins as well).
- **Docs.** `docs/systems/composite-tier.md`: the `auto` paragraph under the `display_floor` guard (what it reads, the three refusals, the
  seat-pin rule, the card-table edge) and the watcher's unload under `auto` (in "The guards keep holding after the load") and the seed step (the layer
  arrives with `auto`, the twins are pinned to index 1 and must be re-pinned, the `STALE` stamp note, the `audit-config` drift).
- **Tests, red first.** A behaviour-free seam commit (`IsAutoDisplayDevice` returning false), then the tests, which failed on their
  assertions for the stated reason before any fix: `IsAutoDisplayDevice("auto") = false, want true`; every guard case refused with `display
  device auto cannot be resolved to a CUDA index (not in the probe, or ambiguous)` where it should have admitted or named the monitor's card;
  the watcher unloaded a twin whose monitor card held its floor, and for a low one gave the unresolvable-card refusal instead of naming
  the card; the shipped table named `display_device
  "1"`; the docs lacked the paragraph. Cases that pass before and after by design are the characterization tests above, no shape check at load
  (a dashed full UUID, a short UUID prefix and a typo all validate and a UUID or `auto` config round-trips through `Load` with its layers),
  the delegator's carried verdict standing in where it holds no reader, and the floor read by the allocator not depending on the device
  string. New: `internal/config/layers_auto_test.go`, `internal/placement/guards_auto_test.go`, `resident_auto_test.go`, `auto_docs_test.go`,
  `internal/displaywatch/displaywatch_auto_test.go`, `internal/tierseed/tierseed_displayauto_test.go`.
- **Mutation proofs** (`scripts/mutation-check.sh`, one changed line each, every one RED, file restored byte for byte): `IsAutoDisplayDevice`
  without `TrimSpace`, case-sensitive, a different constant; `resolveAutoDisplay` returning card `1` for both the key and the index, or the
  index of the derived card with card `1` as the key (placement tests and the watcher tests both), the multi-card threshold moved from more
  than one to more than two, the empty-reading and no-reader checks removed; the seat-pin rule deleted, applied to declared cards too, or
  keyed on a footprint of zero or more; the label helper returning the raw string, matching only the lower-case spelling, or the raw string
  in either no-reader message; the auto branch in `resolveDisplay` disabled; the seat label losing its model; the multi-monitor wording; the
  shipped table's `display_device` put back to `"1"`; and each sentence the docs test requires removed.
