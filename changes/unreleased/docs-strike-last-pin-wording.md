### Changed - the last version-pin wording is gone from the CLI help, the OpenSpec rules, a Proposed ADR and the notices

- **The gap.** After ADR 0099 some text still described pins the harness no longer has. `install forecast` help said
  "the pinned timestral package" (it installs the head of the default branch and checks the weights against the
  publisher's digest). The OpenSpec project rules told a change that third-party software enters with a "pinned version
  and sha256", the opposite of `docs/systems/security.md`. Proposed ADR 0089 (Strata) asked its spike for a pinned
  release tag. Two `servingtmpl` comments, the doctor's model-bindings legend and an `install_render.go` comment said
  "pinned build" or "pinned download" for the installed build and the recorded download size.
- **What it says now.** The newest release, checked against the publisher's digest at install time, never a pinned
  version; the installed build; the size the installer recorded. The vendored HyperFrames kit and the ContextBench port
  name the commit they were taken from as their source commit (provenance, not a pin; the kit test still checks the
  same hashes).
- **Also.** A llama-swap swap on a node that stops its scheduled task first no longer logs "no running process found"
  after ending the task: it says no process was left after ending the task.
