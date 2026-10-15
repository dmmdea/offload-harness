### Added - `local-offload gate selftest`: the verb that writes the self-test receipt a model judge needs

- **What it does.** `gate selftest --pairs PAIRS.json --scorer ID [--out RECEIPT.json]` reads a JSON array of control pairs
  (`{name, good, degraded}`, the compaction eval's `ControlPair`), ranks every pair with the named scorer through
  `compeval.SelfTest`, and prints the receipt the gate's rule R7 asks of a model judge: `scorer_id` (the `--scorer` id, which
  is the judge identity the baseline freezes), `pairs_hash` (the SHA-256 of the pairs file's bytes), `passed`, and
  `build_sha256` (the SHA-256 of the running executable, the hash `verdict` prints as `gate_build_sha256`). The printed JSON
  is the `receipt` object of a gate input as it stands. The pairs file is read as strictly as the gate's other inputs: an
  unknown field, trailing data or an empty list is refused, and a leading byte-order mark is tolerated but still hashed.
  `--out` also writes the receipt to a file and replaces one that is there, so a failing run does not leave an older passing
  receipt behind. `internal/eval` still does not import `internal/compeval`: the verb is in the root package, and the
  gate only reads the receipt, as before.
- **Exit codes.** 0: the scorer ranked every pair. 1: it mis-ranked one (a tie counts); the receipt is still printed, with
  `passed` false, the mis-ranking goes to stderr, and the gate refuses that receipt. 2: the run never reached a finding, so
  no receipt is printed: no `--pairs` or `--scorer`, an unreadable, malformed or empty pairs file, an unknown scorer, a
  scorer that returned an error (a failed receipt would read as a statement about a scorer that was never asked), a build
  that cannot be hashed (the gate would refuse a receipt without a 64-character hash), or a `--out` that could not be
  written. So exit 1 of the gate verbs now means a finding: a verdict that is not ADOPT under `--require-adopt`, or a
  scorer that failed its self-test.
- **Scorers.** One, `compaction-outcome`: the outcome scorer of `compaction-eval ab` (accept, then entity recall over a
  summary of the transcript), through the configured seat. It moved, unchanged, out of the `ab` case into
  `compactionOutcomeScorer`, and both verbs call it. `selftest` is the one gate subcommand that reads `--config` (the others
  still ignore it). The scorer never returns an error: a seat that is down scores every side 0, which is a tie and fails
  the self-test with exit 1. The receipt binds the scorer id, the pairs and the harness build; it does not name the model
  that answered, which the runs' stamps carry. The scorer factory is a variable (`gateScorerFor`), so the tests put fakes
  where the seat would be.
- **Tests.** Red first: 16 new tests failed before the verb existed. They cover a pass (the four receipt fields, no stderr,
  no file written), the hash of the file's bytes (indented, compact and BOM-prefixed forms of the same pairs hash
  differently), a failure by an inverted scorer, a tied scorer and a second pair that mis-ranks (receipt printed, exit 1,
  the pair named), 17 refusals each with its own reason, a scorer error (exit 2, no receipt, cleanup still run), the cleanup
  count, `--out` (same bytes as stdout, a stale passing receipt replaced), five unusable build hashes, the real executable's
  hash, the config path reaching the factory from before and after the subcommand, the real factory's one known id, the
  usage text, and the round trip: a passing receipt (read from the `--out` file) is accepted by `gate verdict` for a model
  judge with the same identity and a model-judged input is refused without one, a failing receipt is refused with exit 2, and
  a receipt for another scorer is refused. The scorer's own numbers are pinned (deferred 0, entity-free 0.2, recall, the cap
  at 8). Twelve mutants of the verb were run, each confirmed applied and each killed: the failure exit changed to 2,
  `passed` forced true, the hash taken of the decoded pairs, a scorer error treated as a finding, the build check removed,
  the cleanup skipped, an empty list allowed, `--out` not written, lenient JSON decoding, a wrong scorer id and a wrong
  build hash in the receipt, and the receipt not printed. No test starts a process or needs a seat; the real scorer was
  not run against a seat.
