### Added - ADR 0094, a lease names its cards and a card has one holder; dated pointers in ADRs 0018, 0026, 0039 and 0041; the lease guide's Known gaps located and corrected

- **What it records.** One decision record for the rule the card-naming changes share, written from the code at commit `3be61a3c`
  and not from the older records. A lease names its cards or the whole node; two leases conflict when either is whole-node or
  their card lists share a card; the grant creates each card's claim exclusively under the epoch lock, and a whole-node grant
  verifies the other side; the fence is per epoch; nothing frees a card from a holder that may still be using it except an
  explicit release by epoch, which checks no liveness; the writer is a per-host switch behind a green reader audit; every
  consumer asks about its own cards and a doubt reads as every card; a busy card is a place in line. Every behaviour cites
  `path:line` and a symbol. Status Accepted covers only that part. The decisions README keeps the move to Accepted with the
  operator, so the release review may set the status back to Proposed.
- **What it does not decide.** A separate section lists what is not built and names the constraint the Accepted clauses put on
  it: a takeover that stops a job and then frees its claim (three status surfaces name `gpu takeover`, which is not a verb), a
  release that waits for a dead tree and a writer for the `quarantine.<id>` sidecars, an overdue policy beyond the request, a
  media plan that always names its cards, a pool of local workers across cards, and a scripted acceptance of the whole path.
- **Pointers.** ADRs 0018, 0026, 0039 and 0041 each get a dated amendment of one or two sentences pointing at it, in the
  convention of that file; their decisions are unchanged. The ADR index has the row. ADR 0055 is cross-referenced from the new
  record only.
- **The lease guide.** `docs/systems/gpu-lease.md` keeps its one Known gaps section and puts three groups in front of the older
  gaps: what is not built, the windows the host-RAM hold bound's entry lists as open (each now with the file and symbol it lives
  in), and the text that disagrees with the code. Where the guide and the code disagreed, the guide was corrected: it said the
  session registry's writer was not wired in, and the MCP server writes it when it starts (`registerSession`); what stays
  unverified is whether a given Claude Code build hands its MCP server the session id.
- **Disagreements found and left for their owners** (they sit in code comments and accepted ADRs, which this change does not
  edit): `devices.go` still calls the dropped whole-node record "pending acceptance"; `term.go` and ADR 0070 repeat the registry
  sentence; `proctree/stop.go` says the yield request uses the stop primitives, and nothing does; ADR 0026 says the default
  lease window is stamped on nearly every media lease, and the routes pass their own timeout; ADR 0055's out-of-scope note on the
  reclaim rule reads as if progress played no part, and it decides a term's label.
- **Tests.** Documents only, no code. The existing number-and-index test (`TestADRNumbersAreUnique`) was red on the commit that
  added the record alone ("not linked from docs/architecture/decisions/README.md") and is green with the index row; the docs lint
  and the leak gate run in the root package.
