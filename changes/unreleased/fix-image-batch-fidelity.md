### Fixed - `generate-image --batch` keeps the flags you typed, resumes at the size the family renders, and spills while the lane is busy

- **Flags were dropped.** `--negative`, `--width`, `--height`, `--steps` and `--seed` were read on the batch verb and then ignored, so a batch
  rendered without its negative. They are now the defaults of every row that does not set its own, on the warm session, the tickets, the spill
  workers and the one-at-a-time path. A row's own value wins. With `--seed S`, a row without a seed gets `S + i` (its 0-based position in the
  file), so the seeds stay the same on a `--resume`.
- **`--resume` re-rendered every picture.** A job asked 1600x900, the family rendered 1600x896, and the resume check wanted 900. The size is now
  snapped once, before anything is placed, to the grid the family renders at (16 px for `qwen-image`, 32 px for `qwen-image-2.1`), so the
  asked size, the rendered size and the resume check agree and a finished picture is skipped.
- **`--spill` needed a free lane.** It now also starts while this machine's lane is busy: the matching nodes work the shared queue at once, and
  this machine's warm session joins when the lane frees (it asks the same lane probe the ticket scan uses, every 10 s). With no matching node
  it behaves as before.
- **Inherited lease.** Inside `gpu reserve`, a call that asked for `--spill` or typed `--route auto|remote` now prints one line on stderr saying
  fleet ticketing and spill are off there and to run the call outside the lease to use them. The helper is `mediaremote.InheritedLeaseNotice`.
- **A slow node health read.** A node whose cards are busy can answer its health read slowly; one timeout was taken as "no node serves image
  generation" and the batch slept up to five minutes. That read is now asked once more after 2.5 s before the node is counted as absent.

### Added - `--placement single|multicard` on `generate-image`, `offload_generate_image` and the fleet image payload, and `--dry-run` on the batch

- **Per-call placement.** `generate-image --placement` (also on `--batch`), the `placement` parameter of `offload_generate_image`, and the
  fleet image-gen payload lay a card placement over the family's own `imagegen_placement` for that call. The node builds the request with it
  the way it reads its config. It is not part of the recipe digest, so it never changes which nodes match.
- **`--dry-run`.** With `--batch`, prints the resolved rows (defaults applied, sizes snapped, seeds and outs fixed, and with `--resume` which
  rows would be skipped) and exits. It renders nothing, asks no lane and contacts no node.
