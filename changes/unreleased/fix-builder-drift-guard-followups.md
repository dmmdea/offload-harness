### Fixed - builder drift guard: the wiring locator's unlinked-input throw is tested, a proxied value is no longer sent to the wrong locator, and the strict runbook states its two expected failures

- **A wiring check over an unlinked input.** The `distinct` locator throws when either input of the node has no link
  (`... node N has an unlinked input in [a, b]`). No test covered it: removing the throw survived the whole suite. A
  test now unlinks each input in turn, and drops one entry, and expects the throw naming the builder, the key and the
  inputs; two mutants (the throw removed, `||` turned into `&&`) turn it red.
- **The proxied-widget message named the wrong locator.** A legacy proxied widget whose instance carries a value was
  refused with "read it with I", but `I` indexes the instance's `widgets_values` by the order of the subgraph's
  widget-typed inputs, not by `proxyWidgets`, so it reads the value that runs only where the two positions agree.
  The message now states the position in `proxyWidgets` and says "read it with I" only when that position is also the
  widget's place among the widget-typed inputs; otherwise it says that no locator reads that position and what `I`
  would read instead. `I` itself is unchanged: making it follow `proxyWidgets` would change what the real templates
  read, and no package is available here to prove the committed manifest still holds. Three mutants of the new branch
  turn its tests red.
- **The runbook's strict command.** `check ... --require-templates --strict` exits 1 on the committed manifest because
  of the two `unreviewed` entries (`wf-qwen-image-edit [full] ksampler.cfg`, a probable builder defect, and
  `wf-qwen-image-21 [edit] encoder.resolution`). Both pages (`docs/systems/media-generation.md`, `render/README.md`) now
  say so, with the count and the two names, and a test keeps the pages in step with the manifest. Neither entry was
  resolved: the CFG one stays open, and the resolution one rests on a builder header that contradicts the template, which
  is a decision for a person. The builder recipes are unchanged.
