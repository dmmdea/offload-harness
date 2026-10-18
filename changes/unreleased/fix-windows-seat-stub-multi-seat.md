### Fixed - the rendered Windows seat stubs serve every Windows seat of the tier, so a box with two Windows seats no longer needs a hand-made stub (register B-57)

`seat-cmd.ps1` and `seat-cmdstop.ps1` (`setup/templates/vllm-seat/windows-wsl/`) are one pair of files for every Windows seat on a box: each seat's llama-swap entry
runs the same file from the same seat directory and passes its own seat id. The render baked ONE seat's values into them, so the stub could not serve a second seat.

- **The defect.** `seat-cmd.ps1` waited for `__SEAT_ID__` (the model id the seat serves) in `/v1/models` on `__PORT__`, both filled in for the seat the render was run for.
  A second seat's `cmd` passes its own seat id to the same file, so its stub waited for the first seat's id for its whole load budget, then exited 4. A box with two Windows
  seats on one port therefore ran a hand-made copy of the stub, keyed by a map from the seat argument to the served id and exiting 5 on an unknown seat. That copy has none of
  the template's later fixes: the crash cleanup, the stop marker and the stop-result read.
- **The change.** Both stubs carry a table, one row per Windows seat: seat id (the argument the entry passes) to the id served, the port and the load budget (the seat's
  health-check timeout). `Spec.ArtifactsWith(templatesDir, runtime, seats)` renders it from the rendered seat plus the other Windows seats given; `Spec.Artifacts` is unchanged
  and renders a table of one. `install vllm-seat` passes the tier's `extra_vllm_seats` that start through WSL. The rows are sorted by seat id and do not depend on which seat the
  render was run for, so rendering the seats one after another writes byte-identical stubs and the last render drops nobody. The file's other behaviour is untouched: crash
  cleanup, stop marker, bounds, exit codes 2, 3 and 4.
- **A seat the table does not hold is refused.** The stub writes `unknown seat '<arg>': this stub serves <ids> - nothing started` to `seat-cmd.log` (a fixed name, because the
  argument is not trusted into a file name) and exits 5, before it starts the task or asks a port. `seat-cmdstop.ps1` does the same before it writes the stop marker or starts the
  stop task, so a misnamed entry cannot leave a marker that makes a later crash look like an unload. The render refuses, before reading anything from disk, another seat that
  starts through systemd, an id that is not a seat argument, a port out of range, an id given twice with different values, and any other seats for a seat that has no stub.
- **Deploying it.** Re-render and deploy BOTH stubs (they agree on the table and on the marker name). The seat argument must be the seat id the entry passes, as the rendered
  entries do; a hand-made entry that passes a short name now exits 5 instead of waiting for an id it was never given. `install vllm-seat` still writes the env file and the
  registration script of the lane seat only: an extra Windows seat's own are not rendered by it, and the note it prints for such a seat now says so instead of asking for a systemd unit,
  wrapper scripts and polkit rule that a Windows seat does not have (an extra seat that starts through systemd keeps its note).
- **Limit, stated.** The table holds the seats the tier declares. A tier cannot declare a seat on other cards than its lane seat (`extra_vllm_seats` are alternatives of the lane
  seat), so a seat of that kind on a box still needs its row added by hand until the tier can declare it. Not verified: the stubs under llama-swap as SYSTEM on a real Windows box.
  The PowerShell-driven tests run under whichever PowerShell the test host has and skip without one; the Linux gate runners used had none, so those tests skipped there and only
  the Go-only tests ran. The rendered stubs were instead run by hand, under Windows PowerShell 5.1 and PowerShell 7, with the same drivers.
- **Tests (red first).** `internal/vllmseat/seat_stub_seat_table_test.go` renders two seats on two ports with different load budgets and runs the RENDERED stubs under a PowerShell
  driver with a simulated clock: each seat argument probes its own port, waits for its own id (another seat's id on its port is not it) and keeps its own load budget; an unknown
  seat exits 5 with nothing started, nothing asked and no file named after it; the stop stub watches its own seat's port and writes only its own marker. A Go-only test pins the table
  rows in both files, byte-identical output whichever seat the render is for, the sort order, and the refusals. `install_vllmseat_wsl_siblings_test.go` runs the command over a tier with a
  second Windows seat and a systemd seat: the stubs hold the first two and not the third, and a one-seat tier's stubs hold one row. The existing crash-cleanup driver now renders the seat it
  runs the stub for (`pp3`) instead of passing a seat argument the stub was not rendered for. On `origin/main` the new Go-only tests fail (the root-package ones as they stand; the vllmseat ones against a shim that gives the new
  entry point main's behaviour). By hand, the stubs rendered from `origin/main` for one seat: a second seat probed the first seat's port and either came up on the first seat's id or waited
  out its load budget and exited 4, an unknown seat started a task and exited 0, and the stop stub wrote a marker for it; the new stubs do what the tests above assert. The 12 scenarios
  of the existing crash-cleanup driver (crash, unload marker, task ended early, a live launcher, every stop-task bound and failure, the three marker failures) give call logs identical to
  `origin/main`'s stubs, under both PowerShells and with the template's line endings either LF or CRLF.
