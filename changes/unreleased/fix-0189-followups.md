### Fixed - the host-RAM rule holds a commit nobody stated to an enforced commit limit below physical RAM and refuses figures that are not numbers; the staged-file, route and decision-record checks pin what they say

- **A commit nobody stated, and a strict Linux limit.** A lease that declares one figure (the media admission, the seat warm-back, a bare `gpu reserve --ram N`)
  states no commit, so its commit is the touched figure standing in for one and was judged against physical RAM less the headroom on every reading. On a reading
  whose commit limit is enforced and sits below physical RAM (Linux with `vm.overcommit_memory=2` and little swap: the limit is about half the RAM plus the swap)
  that line lies further out than the one the kernel enforces, so such a lease was admitted past it while the verdict of the same reading was OVER. The bound of a
  commit nobody stated is now the nearer of the two, `min(physical RAM, commit limit)` less the headroom, when the limit is enforced. Windows (the limit is physical
  RAM plus the page file, so above it) and a limit the OS does not enforce keep the physical line. A commit above that bound is impossible and says which line.
- **The surfaces follow the line.** The refusal and the impossible sentence name the commit limit when it is the line that refused. `unstated_commit_room_gib`
  in the `host_memory` block is the room under the same line. The loud OVER lead (`gpu status`, the brief line of `offload_status`) said less than the note under it:
  when the verdict rests on the commit charge above physical RAM alone, it now adds that a lease that states its commit is judged on the commit limit instead (or
  not at all where the OS enforces none), which the rule admits; the block carries the same words as `stated_commit_note`. A reading at or near an enforced limit,
  or OVER on the available memory, adds nothing.
- **Figures that are not numbers.** `NaN` compares false against everything and Go's `max(NaN, 0)` is `NaN`, so a declaration of it fell through every test of the
  rule and was admitted (and `-Inf` was read as "declares nothing"). The rule now refuses a declared figure that is not finite as impossible, makes a lease wait
  when a figure it compares against (the reading, the headroom, what running leases have yet to load) is not finite, reads such a reading as `unknown`, and counts a
  `NaN` load as declaring host RAM so that it reaches the rule. The hidden `gpu hold` verb checks `--ram`, `--ram-commit`, `--ram-wait` and `--ram-wait-commit`
  itself, as `gpu reserve` does; a `-Inf` commit used to be held.
- **Wording.** The `--ram-commit` help and the operator guide said a stated commit is judged on Linux only with `vm.overcommit_memory=2`; a mode that cannot be read
  counts as strict, so it is judged there too, and both now say so. The lease guide read "fail at once the page file has reached its maximum" and reads "fail once".
  The scan that refuses retired wordings gained these.
- **The refusal for an unreadable lease record says what it rests on.** When the heartbeat of the holder cannot be told (a rename is installing it, or it can be
  neither read nor examined) no sign of life was seen: the holder is only not shown to be gone. The refusal claimed a heartbeat or a write within the heartbeat TTL
  and said the lease is held until the heartbeat goes stale. It now says the heartbeat cannot be told and that the hold ends when it can, when a staged one ages out,
  when the holder lets go, or at `gpu release`. A sign of life within the TTL keeps its sentence.
- **Pins on the staged-file handling, no behaviour change.**
  - A staged heartbeat with no live name beside an unreadable whole-node record counts as a heartbeat that cannot be told only while the staged file is younger than
    `stagingFresh`: just under the bound the record is held, just over it and at ten minutes it is debris and the node is free. The card-record twin does the same
    for `gpu release` with no epoch and for the listing of records that cannot be read. A staged heartbeat beside a live one that reads is not unknown.
  - Every rename of a lease file goes through `renameReplacing`; a direct `os.Rename` anywhere else in the package is named (the drift test read the
    `renameReplacing` call sites only, so a new writer with a direct rename went unseen).
  - The Node launch-marker writer stages under the name `stagingPath` gives a marker, checked from the Go side against its source and from the Node side against the
    names the readers look for, so a marker mid-rename cannot read as no marker and a dead wrapper's lease be reclaimed with its instance running.
- **The fleet node's route check covers a handler wrapped around the ingress guard.** The tables of the credential tests are complete by construction only if no
  handler answers for a path without registering it. The source test now also holds what `Handler` returns (the one return, the `netguard.IngressGuard` call
  around the mux) and what the node's `http.Server` serves (`s.Handler()` exactly, built by `httpServer`); a second `http.Server`, one with no handler, a handler
  around either, and `http.ListenAndServe` and its siblings are named.
- **A decision record.** The record for the overflow of a media call said the graph door is open to any peer. It carries the dated amendment, pointing at the
  decision for the fleet bearer, that every record calling a lane open carries, and the lint that requires the amendment now matches that wording too.
- **How it was proved.** Red first, with the failure lines in the commit messages: the admission rule at a limit below physical RAM (refusal, boundary,
  pending, impossible, and the Windows and unenforced controls), the room key and the OVER lead (`internal/gpucards`, `internal/mcpserver`), the non-finite figures
  (rule, verdict, hidden holder), the help and guide wording, the refusal sentence, the route check rows on the fixtures and on the node's real sources, and the
  amendment lint. The pins that pass on the code before this change (the staged-file bound, the card record, the staged heartbeat beside a live one, the direct
  rename, the marker writer, the stated-commit flag in the deferred-reserve split and in the card allocator) were each shown to survive a one-line edit that removes
  what they pin and to go red once they exist.
- **Not verified here.**
  - No lease was taken and no seat was loaded: every test runs against a stand-in host reading.
  - The strict Linux path has no live host: both Linux nodes read are in mode 0, so the new bound is exercised through the rule's inputs and the reader seam only.
  - The Windows-only staged-file behaviour (the rename gate on a real clock) ran on the local Windows host through targeted tests, not on a loaded runner.
