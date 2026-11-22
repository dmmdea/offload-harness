### Fixed - the agent seat's cold-load gate judges a stated commit against the commit limit and re-checks for up to a minute

- **What was wrong.** The contract doors' host-RAM gate on a cold load of the agent seat refused most contracts on a busy
  desktop box, for two reasons. The seat stated one figure (`agent_seat_host_ram_gib`), taken for both the touched set and
  the commit, with no commit stated, so the seat's commit was held to physical RAM less the headroom although it is a
  measured figure that belongs against the OS commit limit, as a stated `--ram-commit` is. And the gate was one look, with no
  wait and no relief: right after another model unloads, the host's memory takes up to about a minute to come back, and a
  contract arriving in that window was refused for a load that would fit seconds later.
- **What it does now.** A node can state the seat's commit with the new key `agent_seat_host_commit_gib`, read only together
  with `agent_seat_host_ram_gib`; the commit is then judged against the OS commit limit less the headroom, and the refusal
  says `the seat's N GiB and M GiB of commit`. Both contract doors (`runAgentTask` and the MCP `agent_run` door) now settle
  (`Manager.HostRAMSettle`): a refusal is re-read every 2 seconds for at most 60 seconds, never past the admission budget, and
  asks the WSL page-cache relief on each refusal when `gpu_hostram_wsl_cache_drop` is on. It stops early when the reading
  admits, when the host cannot be read or the need can never fit, or when the shortfall has not shrunk by 0.25 GiB in 10
  seconds. The wait is counted as admission time, not as cold-load time, and a refusal after a wait says how long it waited.
  `doctor`'s `seat ram:` row names the commit key, says it is ignored when set without the ram figure, and says where a
  stated commit is judged: only where the OS enforces a commit limit (always on Windows; on Linux with
  `vm.overcommit_memory=2`), as with `--ram-commit`. On a Linux node in mode 0 or 1, stating it leaves the touched set as
  the only line.
- **Not changed.** With `agent_seat_host_commit_gib` unset, the rule the seat is judged by is as before (the one figure,
  its commit held to physical RAM) and so is the refusal sentence, which gains only the `(after N s waiting ...)` clause
  when the door waited. A lane that declared host RAM still refuses at once, and one granted while the door waited
  refuses the load too. The seat's warm-back at a lease release still takes one look
  and does not wait (the lease it follows is still held); it now also asks the relief once on a refusal. The admission rule
  itself, the relief's rate limit and the lease wait loops are untouched.
- **Check.** Real runs of the built binary on the reference box, the seat absent and about 67 GiB committed. With the
  commit key unset the door waited 10 s, saw no progress and deferred: `host RAM: the host cannot take the seat's 56.0 GiB
  (seat): ... committed 67.5 of 127.7 GiB physical ... (after 10s waiting for the host to give memory back)`,
  `admission_wait_sec` 10.0. With a deliberately high stated commit (45 GiB touched, 80 GiB of commit) it deferred against
  the commit limit instead: `the seat's 45.0 GiB and 80.0 GiB of commit (seat): ... needs 80.0 GiB of commit, committed 67.2
  of 146.7 GiB commit limit, 8.0 GiB headroom (after 10s waiting ...)`. `doctor` printed the two-figure `seat ram:` row for
  that config and the `no commit is stated` row for the live one. Then two loads ADMITTED on a stated commit (45 GiB
  touched, 73 GiB of commit), the seat absent and 54 to 56 GiB committed, each one pinned-local contract through the door
  inside a GPU lease: `deferred` false, the seat cold-loaded and answered (281 and 399 s, the longer one with a cold
  compile cache). Measured over those loads: the host's commit peaked +53.4 and +55.3 GiB over the base and available RAM
  fell by at most 32.2 GiB, which is what a node sets the two keys from.
- **Where.** `internal/config/config.go`, `config.example.json`, `internal/hostneed/hostneed.go` (`SeatNeed`, `Need.HostLoad`,
  `Need.SeatRefusal`), `internal/gpulease/relief.go` (`HostRAMSettle`), `internal/pipeline/seathostram.go`
  (`SeatColdLoadAdmission`), `internal/pipeline/agenttask.go`, `internal/mcpserver/mcpserver_agentrun.go`, `gpu_drain.go`,
  `doctor_seathostram.go`, `docs/systems/gpu-lease.md`, `docs/OPERATOR-GUIDE.md`.
