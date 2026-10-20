### Changed - ADR 0086 is Accepted

- **What it does.** ADR 0086 (an overdue lease's holder is asked to yield, never taken from) moves from Proposed to Accepted, in its frontmatter and in the decisions index, with an acceptance section dated 2026-10-14. No code changes: the `gpu_on_overdue` key shipped in 0.186.0 and its behaviour is unchanged.
- **Why.** The operator accepted `request` on 2026-10-14 (ask the holder, never kill), and the record still read Proposed.
