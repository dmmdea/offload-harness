### Changed - one run-parameters struct under `delegate.RunWith`, and one `maintainSeat` with an options struct

- **What changed.** `internal/delegate`: `runWith` took seven positional parameters that `RunWith` and `RunBatched` forwarded
  one by one; it now takes one `runParams` value, and the exported `Run`, `RunWith` and `RunBatched` signatures are unchanged.
  `gpu_drain.go`: `maintainSeat`, `maintainSeatCtx`, `maintainSeatScoped` and `maintainSeatReport` were four pass-through
  wrappers that each added one argument; they are now one `maintainSeat(ctx, cfg, maintainOpts)`, and its two production
  callers (`gpu reserve` and the detached holder's maintain step) and the existing tests that called the old names pass the
  options struct. `docs/systems/gpu-lease.md` names the one function.
- **Why.** Two desloppify findings: argument ladders where every rung only forwards, so a new argument had to be threaded
  through each one. No behaviour changes: same drain, restamp and unload order, same ledger rows.
- **Check.** `go build ./...`, `go vet` and `go test` on `./internal/delegate/` and the root package's drain and lease tests.
