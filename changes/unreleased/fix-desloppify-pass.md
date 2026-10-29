### Fixed - the ledger's `tok_per_s` falls back to the observed rate as 0.131.1 said, a fleet door fails closed on an empty token, and one positional splitter replaces four

- **`tok_per_s` on agent rows.** 0.131.1 said the column prefers the calibrated seat rate and falls back to the run's
  observed decode rate when no calibrated sample exists; the fallback sat inside the `SeatTokS > 0` branch, where it could
  never fire, so rows with no calibrated sample stayed empty. The two cases are now an `if` / `else if`.
- **`bearerOK` with no configured token.** `Authorization: Bearer ` (nothing after the scheme) hashes equal to an empty
  token, so a door that called `bearerOK` directly on a node with no `fleet_auth_token` would have accepted it; every such
  door ran an admissibility check first, so no real request reached it. The primitive now returns false on an empty token
  and no longer depends on caller ordering. The tokenless-loopback doors short-circuit before it and are unchanged.
- **One splitter.** `splitArgs`, `splitTwoArgs`, `splitThreeArgs` and `splitFourArgs` were four copies of one loop that
  differed only in slot count; they are now wrappers over `splitPositionals(args, valueFlags, n)`, which the existing
  splitter tests cover. Two unused Python imports and one unused variable in the render self-test are gone.
- **Check.** `go build ./... && go vet` clean; `go test -run 'split|Split|Positional|Overflow' .`, `go test
  ./internal/fleetnode/` and the pipeline package pass; the three Python files byte-compile. Found by a desloppify scan
  and its blind review (strict score 22 before the review, 75.6 after; the remaining findings are API-shape refactors
  listed in the pull request).
