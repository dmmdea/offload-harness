### Added - CONTRIBUTING says a seam test must not inject the seam it certifies

A test that certifies a wiring (a function variable, an interface, a config key or a door that hands work from one part of the harness to
another) proves nothing if it assigns a stand-in to that very seam and asserts on the value it just put there: it passes with the
production wiring deleted. A mutation of that kind has survived a green seam test in this repository before, and the red-first practice of
this series (a behaviour-free stub first, then the failing test) makes the rule more relevant, not less.

- **What changed.** `CONTRIBUTING.md` gains a section, "Tests that certify a seam", between "Proving a test can fail" and "Keeping tests out
  of the OS temp dir". It states the rule and three working habits: fake the far side of the seam and never the seam; assert on something the
  production path produces; show that the test goes red with `scripts/mutation-check.sh` and quote the `RED` verdict in the pull request.
  It names `TestRunNoEligibleRemoteNamesTheRealCause` in `internal/delegate/run_test.go` as the worked example. It ends with one line for a
  reviewer: for each test that claims to cover a wiring, ask which production line it goes red without.
- **What did not change.** No code, no test and no lint. There is no mechanical gate for this rule: a source lint for "a test assigns a
  seam variable and asserts only on the value it injected" would have to decide what counts as a seam, which only the reader can, so the rule
  lives where contributors and reviewers read it.
- **Checked.** `go test -run 'TestDocsLint|Changelog|Fragment' .` and `go run ./cmd/changelog check` pass; the section was read back
  against the example it cites.
